// Command sparknet-control runs the goSpark control plane: it holds the
// operational signing key, verifies nodes' attested results, and dispatches
// signed tasks. It is the master role in the fabric's sign/verify split.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
	"github.com/reallife-spark/goSpark/internal/registryfile"
	"github.com/reallife-spark/goSpark/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-control:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("sparknet-control", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (YAML)")
	masterKey := fs.String("master-key", "", "control-plane identity key file (holds the signing key)")
	executors := fs.String("executors", "", "comma list node_id=public_key_b64 of trusted executors")
	registryFile := fs.String("registry", "", "node authorizations file to sign and serve (overrides config registry.file)")
	transportListen := fs.String("transport-listen", "", "override transport listen address")
	dispatch := fs.String("dispatch", "", "dispatch a task to this node id, then keep serving")
	op := fs.String("op", "arith", "operation for -dispatch")
	params := fs.String("params", `{"expression":"2+2"}`, "params JSON for -dispatch")
	ttl := fs.Duration("ttl", 60*time.Second, "task time-to-live for -dispatch")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *masterKey == "" {
		return fmt.Errorf("-master-key is required")
	}
	id, err := identity.Load(*masterKey)
	if err != nil {
		return err
	}
	if id.Private == nil {
		return fmt.Errorf("control plane key %s has no private half; it cannot sign", *masterKey)
	}
	cfg.Node.ID = id.NodeID

	pubs, err := parseExecutors(*executors)
	if err != nil {
		return err
	}

	regDoc, err := loadRegistryDoc(*registryFile, cfg.Registry.File)
	if err != nil {
		return err
	}

	app, err := server.New(server.Options{
		Config:          cfg,
		Identity:        id,
		Role:            server.RoleControl,
		ExecutorPublic:  pubs,
		Registry:        regDoc,
		TransportListen: *transportListen,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *dispatch != "" {
		go func() {
			// Give the bus a moment to connect before the first publish.
			time.Sleep(500 * time.Millisecond)
			env, err := signTask(id, *op, *params, *ttl)
			if err != nil {
				fmt.Fprintln(os.Stderr, "sparknet-control: build task:", err)
				return
			}
			if err := app.DispatchTask(context.Background(), *dispatch, env); err != nil {
				fmt.Fprintln(os.Stderr, "sparknet-control: dispatch:", err)
			}
		}()
	}
	return app.Run(ctx)
}

func signTask(id *identity.Identity, op, params string, ttl time.Duration) (apiv1.TaskEnvelope, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return apiv1.TaskEnvelope{}, err
	}
	now := time.Now()
	env := apiv1.TaskEnvelope{
		Fingerprint:        id.Fingerprint(),
		Capabilities:       []string{"cluster-serve"},
		TaskID:             "task-" + nonce[:12],
		Operation:          op,
		ParamsJSON:         params,
		ContentInSignature: true,
		Nonce:              nonce,
		IssuedAtUnix:       now.Unix(),
		ExpiresAtUnix:      now.Add(ttl).Unix(),
	}
	msg, err := apiv1.CanonicalBytes(&env)
	if err != nil {
		return env, err
	}
	sig, err := id.Sign(msg)
	if err != nil {
		return env, err
	}
	env.SignatureB64 = identity.EncodeSignature(sig)
	return env, nil
}

// loadRegistryDoc reads the authorizations the control plane will sign and
// serve. If the file is a full registry, its issue time and expiry are carried
// over so a peer handed the same file sees an identical registry; otherwise
// only the authorization entries are taken and a fresh issue time is stamped.
func loadRegistryDoc(flagPath, cfgPath string) (*apiv1.NodeRegistry, error) {
	path := flagPath
	if path == "" {
		path = cfgPath
	}
	if path == "" {
		return nil, nil
	}
	if reg, err := registryfile.Load(path); err == nil {
		return reg, nil
	}
	entries, err := registryfile.LoadAuthorizations(path)
	if err != nil {
		return nil, err
	}
	return &apiv1.NodeRegistry{Authorizations: entries}, nil
}

func parseExecutors(s string) (map[string]ed25519.PublicKey, error) {
	out := make(map[string]ed25519.PublicKey)
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.Index(pair, "=")
		if i <= 0 {
			return nil, fmt.Errorf("executors: %q is not node_id=public_key_b64", pair)
		}
		node, keyB64 := pair[:i], pair[i+1:]
		pub, err := identity.ParsePublicKeyB64(keyB64)
		if err != nil {
			return nil, fmt.Errorf("executors: %s: %w", node, err)
		}
		out[node] = pub
	}
	return out, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
