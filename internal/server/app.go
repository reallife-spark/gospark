// Package server assembles the goSpark control plane and node agent from the
// lower-level packages, and is the only place that knows about all of them.
package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/compute"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/gateway"
	"github.com/reallife-spark/goSpark/internal/identity"
	"github.com/reallife-spark/goSpark/internal/kvrouter"
	"github.com/reallife-spark/goSpark/internal/kvtransfer"
	"github.com/reallife-spark/goSpark/internal/router"
	"github.com/reallife-spark/goSpark/internal/telemetry"
	"github.com/reallife-spark/goSpark/internal/transport"
)

// Role selects what a process runs.
type Role string

const (
	RoleControl Role = "control"
	RoleAgent   Role = "agent"
)

// Options configures an App.
type Options struct {
	Config   config.Config
	Identity *identity.Identity
	Role     Role

	// MasterPublic is the operational public key whose signed tasks this agent
	// accepts. Required for RoleAgent.
	MasterPublic ed25519.PublicKey
	// ExecutorPublic is the node identity key whose results the control plane
	// accepts. Required for RoleControl when results are expected.
	ExecutorPublic map[string]ed25519.PublicKey // node_id -> public key

	// Registry is the Master-signed node registry. On RoleControl it is the
	// authoritative set of authorizations the control plane signs and serves;
	// on RoleAgent it is an optional already-signed registry to adopt directly
	// (an agent can also adopt one from the bus).
	Registry *apiv1.NodeRegistry

	// TransportListen overrides the config transport listen address (use ":0"
	// in tests).
	TransportListen string

	// Bus, when set, is used instead of dialling from config. It lets two Apps
	// share one bus in-process, which is what an end-to-end test needs.
	Bus bus.Bus
}

// App is a running control plane or agent.
type App struct {
	cfg       config.Config
	id        *identity.Identity
	role      Role
	log       *slog.Logger
	bus       bus.Bus
	router    *router.Router
	cache     *kvtransfer.DeltaCache
	counters  *telemetry.Counters
	gw        *gateway.Gateway
	transport *transport.Server

	// compute, when non-nil, is the Python/CUDA compute-plane worker that
	// executes the operations named in config.Compute.Operations. It is nil
	// (the default) when the seam is disabled, leaving the pure sandbox.
	compute compute.Executor

	// kv, when non-nil, is the compute-plane RadixAttention engine that knows
	// which node holds which cached KV prefix. It is advisory routing input: a
	// hit lets the dispatcher prefer a node whose prefill is already paid for,
	// and a miss or an unreachable engine degrades to telemetry placement.
	kv kvrouter.Router

	verifier  *authz.Verifier
	masterPub ed25519.PublicKey
	// executorPubs maps node_id -> key whose results the control plane accepts.
	// It is derived from the registry and refreshed on every distribute/revoke,
	// so execMu guards it against the bus goroutine reading it concurrently.
	executorPubs map[string]ed25519.PublicKey
	execMu       sync.RWMutex

	registry *Registry // node health reports (control plane)

	// ready is closed once the control plane has installed its health registry
	// and armed its subscriptions. Callers that need to read placement inputs
	// (which are populated by bus goroutines) wait on it to establish a happens-
	// before and avoid racing runControl's setup.
	ready chan struct{}

	// gpu holds the last GPU telemetry per node, reported by the sparknet-gpu
	// sidecar on the health subject. It is separate from `registry` so a
	// telemetry-only report cannot clobber an agent's full health record.
	gpu   map[string]apiv1.GpuTelemetry
	gpuMu sync.RWMutex

	// nodeRegistry is the authorized-nodes view: authored and served by the
	// control plane, adopted and enforced by the agent.
	nodeRegistry *authz.Registry
	registryDoc  *apiv1.NodeRegistry
	regMu        sync.RWMutex

	transportAddr string
	adminAddr     string
	addrMu        sync.Mutex
	boundAddr     string
	injectedBus   bus.Bus
	ownsBus       bool

	// draining is set when the process stops accepting new work (operator drain
	// or shutdown) so in-flight work can finish. It gates new task dispatches and
	// new gateway requests, is reported by /readyz, and shows in /metrics.
	draining atomic.Bool
}

// New builds an App from options.
func New(opts Options) (*App, error) {
	if opts.Identity == nil {
		return nil, errors.New("server: identity is required")
	}
	if opts.Role == "" {
		opts.Role = RoleAgent
	}
	logger := telemetry.Logger(opts.Config.Log.Level, opts.Config.Log.Format)
	a := &App{
		cfg:  opts.Config,
		id:   opts.Identity,
		role: opts.Role,
		log:  logger,
		router: router.New(router.Config{
			EdgeTokenCeiling:  opts.Config.Router.EdgeTokenCeiling,
			ClusterTokenFloor: opts.Config.Router.ClusterTokenFloor,
			MaxNodes:          opts.Config.Router.MaxNodes,
		}),
		cache:         kvtransfer.NewDeltaCache(),
		counters:      &telemetry.Counters{},
		masterPub:     opts.MasterPublic,
		executorPubs:  opts.ExecutorPublic,
		registryDoc:   opts.Registry,
		transportAddr: opts.Config.Transport.Listen,
		adminAddr:     opts.Config.Admin.Listen,
		injectedBus:   opts.Bus,
		ownsBus:       opts.Bus == nil,
		ready:         make(chan struct{}),
	}
	if opts.TransportListen != "" {
		a.transportAddr = opts.TransportListen
	}
	a.gw = gateway.New(opts.Config.Gateway.Upstream, a.router)
	a.gw.LocalCaps = opts.Config.Node.Capabilities
	a.gw.Model = opts.Config.Gateway.Model
	a.gw.ReadyPath = opts.Config.Gateway.ReadyPath
	a.gw.ProbeTimeout = opts.Config.Gateway.ProbeTimeout
	a.gw.Draining = a.Draining

	if opts.Config.Compute.Endpoint != "" {
		ex, err := compute.NewHTTPExecutor(compute.Options{
			Endpoint:   opts.Config.Compute.Endpoint,
			Secret:     opts.Config.Compute.Secret,
			Operations: opts.Config.Compute.Operations,
			Timeout:    opts.Config.Compute.Timeout,
		})
		if err != nil {
			return nil, err
		}
		a.compute = ex
	}

	if opts.Config.KV.Endpoint != "" {
		kv, err := kvrouter.NewHTTPRouter(kvrouter.Options{
			Endpoint: opts.Config.KV.Endpoint,
			Secret:   opts.Config.KV.Secret,
			Timeout:  opts.Config.KV.Timeout,
		})
		if err != nil {
			return nil, err
		}
		a.kv = kv
	}

	if opts.Role == RoleAgent && len(opts.MasterPublic) == 0 {
		return nil, errors.New("server: agent requires MasterPublic")
	}
	return a, nil
}

// Run starts the app and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	var err error
	a.bus, err = a.dialBus()
	if err != nil {
		return err
	}
	if a.ownsBus {
		defer a.bus.Close()
	}

	a.verifier = &authz.Verifier{
		Nonces: authz.NewMemoryNonceStore(nil, time.Hour),
	}

	// The data plane (KV block intake) runs for both roles.
	ready := make(chan net.Addr, 1)
	a.transport = &transport.Server{
		Addr:      a.transportAddr,
		ChunkSize: a.cfg.Transport.ChunkSize,
		Ready:     ready,
		Handler: func(_ context.Context, h apiv1.BlockHeader, b *kvtransfer.Block) (string, error) {
			digest := a.cache.Put(h.BlockID, b)
			a.counters.BlocksReceived.Add(1)
			a.counters.BytesStreamed.Add(uint64(len(b.Packed)))
			a.log.Debug("block received", "block_id", h.BlockID, "origin", h.OriginNodeID, "digest", digest)
			return digest, nil
		},
	}
	go func() {
		if err := a.transport.ListenAndServe(ctx); err != nil {
			a.log.Error("transport server stopped", "err", err)
		}
	}()
	go func() {
		select {
		case addr := <-ready:
			a.addrMu.Lock()
			a.boundAddr = addr.String()
			a.addrMu.Unlock()
			a.log.Info("data plane listening",
				"addr", addr.String(),
				"cert_fingerprint", a.transport.Fingerprint())
		case <-ctx.Done():
		}
	}()

	// Gateway.
	gwSrv := &http.Server{Addr: a.cfg.Gateway.Listen, Handler: a.gw.Handler()}
	go func() {
		a.log.Info("gateway listening", "addr", a.cfg.Gateway.Listen)
		if err := gwSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("gateway stopped", "err", err)
		}
	}()

	// Admin surface (control plane only). It grants full registry authority, so
	// it refuses to bind anything but loopback.
	if a.role == RoleControl && a.adminAddr != "" {
		if err := requireLoopback(a.adminAddr); err != nil {
			return err
		}
		adminSrv := &http.Server{Addr: a.adminAddr, Handler: a.adminHandler()}
		go func() {
			a.log.Info("admin surface listening", "addr", a.adminAddr, "scope", "loopback-only")
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.Error("admin surface stopped", "err", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = adminSrv.Shutdown(shutdownCtx)
		}()
	}

	// Prometheus surface. Disabled unless an operator sets metrics.listen; when
	// set it serves /metrics and /healthz for a scrape.
	if addr := a.cfg.Metrics.Listen; addr != "" {
		mSrv := &http.Server{Addr: addr, Handler: a.metricsHandler()}
		go func() {
			a.log.Info("metrics surface listening", "addr", addr)
			if err := mSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.Error("metrics surface stopped", "err", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = mSrv.Shutdown(shutdownCtx)
		}()
	}

	switch a.role {
	case RoleControl:
		if err := a.runControl(ctx); err != nil {
			return err
		}
	case RoleAgent:
		if err := a.runAgent(ctx); err != nil {
			return err
		}
	}

	<-ctx.Done()
	a.BeginDrain()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), drainShutdownTimeout)
	defer cancel()
	_ = gwSrv.Shutdown(shutdownCtx)
	return nil
}

func (a *App) dialBus() (bus.Bus, error) {
	if a.injectedBus != nil {
		a.log.Info("bus: injected")
		return a.injectedBus, nil
	}
	if a.cfg.Bus.URL == "" {
		a.log.Info("bus: in-memory (no URL configured)")
		return bus.NewMemory(), nil
	}
	n, err := bus.Connect(bus.NATSOptions{
		URL:        a.cfg.Bus.URL,
		Name:       "gospark-" + string(a.role) + "-" + a.id.NodeID,
		Stream:     a.cfg.Bus.Stream,
		Subjects:   []string{a.cfg.Bus.SubjectRoot + ".>"},
		Queue:      a.cfg.Bus.Queue,
		MaxDeliver: a.cfg.Bus.MaxDeliver,
	})
	if err != nil {
		return nil, err
	}
	a.log.Info("bus: connected", "url", a.cfg.Bus.URL)
	return n, nil
}

// LocalAddr reports the bound transport address (the real port when the config
// used ":0", otherwise the configured address). Empty until the listener is up.
func (a *App) LocalAddr() string {
	a.addrMu.Lock()
	defer a.addrMu.Unlock()
	if a.boundAddr != "" {
		return a.boundAddr
	}
	return a.transportAddr
}
