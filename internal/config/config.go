// Package config loads goSpark's runtime configuration from YAML with
// environment overrides. It is intentionally small: one file, explicit
// defaults, and no hidden global state.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole control-plane configuration.
type Config struct {
	Node      NodeConfig      `yaml:"node"`
	Bus       BusConfig       `yaml:"bus"`
	Transport TransportConfig `yaml:"transport"`
	Gateway   GatewayConfig   `yaml:"gateway"`
	Mesh      MeshConfig      `yaml:"mesh"`
	Router    RouterConfig    `yaml:"router"`
	Registry  RegistryConfig  `yaml:"registry"`
	Admin     AdminConfig     `yaml:"admin"`
	Compute   ComputeConfig   `yaml:"compute"`
	Embed     EmbedConfig     `yaml:"embed"`
	KV        KVConfig        `yaml:"kv"`
	Backup    BackupConfig    `yaml:"backup"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	Log       LogConfig       `yaml:"log"`
}

type NodeConfig struct {
	ID           string   `yaml:"id"`
	Role         string   `yaml:"role"` // master | peer (fabric identity_fields)
	KeyFile      string   `yaml:"key_file"`
	OverlayIPv4  string   `yaml:"overlay_ipv4"`
	Capabilities []string `yaml:"capabilities"`
}

type BusConfig struct {
	URL         string `yaml:"url"`          // nats://... ; empty => in-memory
	SubjectRoot string `yaml:"subject_root"` // default sparknet
	Stream      string `yaml:"stream"`       // JetStream stream name
	// Queue, when set, makes bus subscribers load-balance across instances
	// that share it (HA replicas). Empty means each instance receives its own
	// copy (fan-out), which is what a single control plane plus observers want.
	Queue string `yaml:"queue"`
	// MaxDeliver caps redeliveries of a failing JetStream message before it is
	// dead-lettered. Zero uses the bus default.
	MaxDeliver int `yaml:"max_deliver"`
}

type TransportConfig struct {
	Listen    string        `yaml:"listen"`     // host:port for the HTTP/3 data plane
	ChunkSize int           `yaml:"chunk_size"` // bytes per streamed chunk
	Timeout   time.Duration `yaml:"timeout"`
	// ClientFingerprint is the pinned client-certificate fingerprint for mutual
	// TLS. Required for a non-loopback listener; loopback-only test servers may
	// omit it.
	ClientFingerprint string `yaml:"client_fingerprint"`
	// AllowUnauthenticated explicitly permits an unpinned non-loopback listener.
	// It exists for a single-process rehearsal, never for a deployment.
	AllowUnauthenticated bool `yaml:"allow_unauthenticated"`
}

type GatewayConfig struct {
	Listen   string `yaml:"listen"`
	Upstream string `yaml:"upstream"` // e.g. http://127.0.0.1:8000 (vLLM)
	// Model is the serving model this gateway fronts; reported by /readyz and
	// /healthz and used as /v1/models discovery when no upstream is set.
	Model string `yaml:"model"`
	// ReadyPath is the upstream path probed by /readyz (default /v1/models).
	ReadyPath string `yaml:"ready_path"`
	// ProbeTimeout bounds the /readyz upstream probe.
	ProbeTimeout time.Duration `yaml:"probe_timeout"`
	// MaxInflight bounds concurrent inference requests proxied to the upstream
	// (admission control). <= 0 (the default) disables the limit. This is edge
	// admission, not batching: the engine does continuous batching.
	MaxInflight int `yaml:"max_inflight"`
	// MaxQueued is how many requests may wait for a slot when the limit is hit;
	// with 0 a full gateway refuses immediately. Ignored when MaxInflight <= 0.
	MaxQueued int `yaml:"max_queued"`
	// QueueWait bounds how long a queued request waits for a slot (0 waits until
	// the request context is cancelled). Ignored when MaxInflight <= 0.
	QueueWait time.Duration `yaml:"queue_wait"`
}

type MeshConfig struct {
	Interface string `yaml:"interface"` // wg0
	WGConfig  string `yaml:"wg_config"`
}

type RouterConfig struct {
	// EdgeTokenCeiling: prompts at or below this token estimate stay on the
	// edge. Above it the router escalates.
	EdgeTokenCeiling int `yaml:"edge_token_ceiling"`
	// ClusterTokenFloor: prompts at or above this estimate fan out to a
	// cluster job rather than a single DGX teacher.
	ClusterTokenFloor int `yaml:"cluster_token_floor"`
	// MaxNodes bounds a cluster job.
	MaxNodes int `yaml:"max_nodes"`
}

type LogConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
}

// RegistryConfig points at a Master-signed node registry on disk. The control
// plane treats the file's authorizations as its own and re-signs them; an agent
// loads the signed file and verifies it against the pinned master key.
type RegistryConfig struct {
	File string `yaml:"file"`
}

// AdminConfig exposes the control plane's runtime registry control (revoke,
// re-distribute). It grants full authority over the signed registry — the
// process holds the Master key — so it must stay on the loopback interface.
// Empty disables the surface entirely (the default).
type AdminConfig struct {
	Listen string `yaml:"listen"`
	// Token is required whenever Listen is set. The admin surface can re-sign
	// the registry, so loopback alone is not an authorization boundary: any
	// local process could otherwise revoke or authorize nodes.
	Token string `yaml:"token"`
}

// MetricsConfig exposes the control plane's read-only Prometheus surface. The
// /admin/metrics route always serves it on the admin listener; a non-empty
// Listen additionally starts a standalone server that answers /metrics and
// /healthz, for a Prometheus scrape. It is disabled by default because the
// snapshot carries node ids, authorization state and accelerator telemetry —
// start it only on a trusted network interface.
type MetricsConfig struct {
	Listen string `yaml:"listen"`
}

// ComputeConfig points a node's agent at a Python/CUDA compute-plane worker and
// names the operations that worker is trusted to run. Empty Endpoint (the
// default) disables the seam and leaves the closed operation sandbox intact.
type ComputeConfig struct {
	// Endpoint is the worker base URL, e.g. http://127.0.0.1:9090.
	Endpoint string `yaml:"endpoint"`
	// Secret is sent as X-Sparknet-Auth to the worker. Empty sends no header.
	Secret string `yaml:"secret"`
	// Operations is the explicit set of operation names delegated to the
	// worker. Each must be non-empty and must not shadow a local sandbox
	// operation (arith, digest); Validate enforces both.
	Operations []string      `yaml:"operations"`
	Timeout    time.Duration `yaml:"timeout"`
}

// EmbedConfig points the ingest pipeline at an OpenAI-compatible embedding
// service on the compute plane. Empty Endpoint (the default) leaves ingestion
// crawl-and-chunk only.
type EmbedConfig struct {
	// Endpoint is the embeddings server base URL, e.g. http://127.0.0.1:9090.
	Endpoint string `yaml:"endpoint"`
	// Secret is sent as X-Sparknet-Auth. Empty sends no header.
	Secret string `yaml:"secret"`
	// Model is the embedding model name. Empty lets the server choose.
	Model string `yaml:"model"`
	// Batch caps inputs per request.
	Batch int `yaml:"batch"`
	// Timeout bounds one embeddings request.
	Timeout time.Duration `yaml:"timeout"`
}

// KVConfig points the router at a compute-plane RadixAttention engine that
// knows which node holds which cached KV prefix. Empty Endpoint (the default)
// leaves placement to GPU telemetry alone; the residency hint is advisory and
// degrades to telemetry placement when the engine is unreachable.
type KVConfig struct {
	// Endpoint is the RadixAttention daemon base URL.
	Endpoint string `yaml:"endpoint"`
	// Secret is sent as X-Sparknet-Auth. Empty sends no header.
	Secret string `yaml:"secret"`
	// Timeout bounds one lookup. It sits on the routing path, so keep it short.
	Timeout time.Duration `yaml:"timeout"`
}

// BackupConfig points the backup daemon at a source tree, a target directory
// and the node key used to sign each snapshot's manifest. It is a
// control/service-plane task: content-hash, sign, verify, restore — no rsync.
type BackupConfig struct {
	// Source is the directory tree to snapshot.
	Source string `yaml:"source"`
	// Target is the directory under which timestamped snapshots are created.
	Target string `yaml:"target"`
	// KeyFile is the node key that signs manifests (needs the private half).
	KeyFile string `yaml:"key_file"`
	// Interval is the daemon's snapshot cadence.
	Interval time.Duration `yaml:"interval"`
	// SkipDirs are directory names pruned from the snapshot (e.g. ".git").
	SkipDirs []string `yaml:"skip_dirs"`
}

// Default returns the built-in defaults before any file or env override.
func Default() Config {
	return Config{
		Node: NodeConfig{Capabilities: nil},
		Bus: BusConfig{
			SubjectRoot: "sparknet",
			Stream:      "SPARKNET",
		},
		Transport: TransportConfig{
			Listen:    "127.0.0.1:7443",
			ChunkSize: 256 * 1024,
			Timeout:   10 * time.Second,
		},
		Gateway: GatewayConfig{Listen: "127.0.0.1:8080"},
		Mesh:    MeshConfig{Interface: "wg0"},
		Router: RouterConfig{
			EdgeTokenCeiling:  2048,
			ClusterTokenFloor: 32768,
			MaxNodes:          4,
		},
		Log: LogConfig{Level: "info", Format: "text"},
		Embed: EmbedConfig{
			Batch:   32,
			Timeout: 60 * time.Second,
		},
		KV: KVConfig{
			Timeout: 2 * time.Second,
		},
		Backup: BackupConfig{
			Interval: 24 * time.Hour,
		},
	}
}

// Load reads path (which may be empty), applies env overrides, and validates.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("GOSPARK_NODE_ID"); v != "" {
		cfg.Node.ID = v
	}
	if v := os.Getenv("GOSPARK_ROLE"); v != "" {
		cfg.Node.Role = v
	}
	if v := os.Getenv("GOSPARK_KEY_FILE"); v != "" {
		cfg.Node.KeyFile = v
	}
	if v := os.Getenv("GOSPARK_OVERLAY_IPV4"); v != "" {
		cfg.Node.OverlayIPv4 = v
	}
	if v := os.Getenv("GOSPARK_BUS_URL"); v != "" {
		cfg.Bus.URL = v
	}
	if v := os.Getenv("GOSPARK_BUS_QUEUE"); v != "" {
		cfg.Bus.Queue = v
	}
	if v := os.Getenv("GOSPARK_BUS_MAX_DELIVER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Bus.MaxDeliver = n
		}
	}
	if v := os.Getenv("GOSPARK_REGISTRY_FILE"); v != "" {
		cfg.Registry.File = v
	}
	if v := os.Getenv("GOSPARK_ADMIN_LISTEN"); v != "" {
		cfg.Admin.Listen = v
	}
	if v := os.Getenv("GOSPARK_ADMIN_TOKEN"); v != "" {
		cfg.Admin.Token = v
	}
	if v := os.Getenv("GOSPARK_METRICS_LISTEN"); v != "" {
		cfg.Metrics.Listen = v
	}
	if v := os.Getenv("GOSPARK_COMPUTE_ENDPOINT"); v != "" {
		cfg.Compute.Endpoint = v
	}
	if v := os.Getenv("GOSPARK_COMPUTE_SECRET"); v != "" {
		cfg.Compute.Secret = v
	}
	if v := os.Getenv("GOSPARK_COMPUTE_OPERATIONS"); v != "" {
		cfg.Compute.Operations = splitList(v)
	}
	if v := os.Getenv("GOSPARK_COMPUTE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Compute.Timeout = d
		}
	}
	if v := os.Getenv("GOSPARK_EMBED_ENDPOINT"); v != "" {
		cfg.Embed.Endpoint = v
	}
	if v := os.Getenv("GOSPARK_EMBED_SECRET"); v != "" {
		cfg.Embed.Secret = v
	}
	if v := os.Getenv("GOSPARK_EMBED_MODEL"); v != "" {
		cfg.Embed.Model = v
	}
	if v := os.Getenv("GOSPARK_EMBED_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Embed.Batch = n
		}
	}
	if v := os.Getenv("GOSPARK_EMBED_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Embed.Timeout = d
		}
	}
	if v := os.Getenv("GOSPARK_KV_ENDPOINT"); v != "" {
		cfg.KV.Endpoint = v
	}
	if v := os.Getenv("GOSPARK_KV_SECRET"); v != "" {
		cfg.KV.Secret = v
	}
	if v := os.Getenv("GOSPARK_KV_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.KV.Timeout = d
		}
	}
	if v := os.Getenv("GOSPARK_BACKUP_SOURCE"); v != "" {
		cfg.Backup.Source = v
	}
	if v := os.Getenv("GOSPARK_BACKUP_TARGET"); v != "" {
		cfg.Backup.Target = v
	}
	if v := os.Getenv("GOSPARK_BACKUP_KEY_FILE"); v != "" {
		cfg.Backup.KeyFile = v
	}
	if v := os.Getenv("GOSPARK_BACKUP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Backup.Interval = d
		}
	}
	if v := os.Getenv("GOSPARK_BACKUP_SKIP_DIRS"); v != "" {
		cfg.Backup.SkipDirs = splitList(v)
	}
	if v := os.Getenv("GOSPARK_TRANSPORT_CLIENT_FINGERPRINT"); v != "" {
		cfg.Transport.ClientFingerprint = v
	}
	if v := os.Getenv("GOSPARK_TRANSPORT_ALLOW_UNAUTHENTICATED"); v == "1" {
		cfg.Transport.AllowUnauthenticated = true
	}
	if v := os.Getenv("GOSPARK_TRANSPORT_LISTEN"); v != "" {
		cfg.Transport.Listen = v
	}
	if v := os.Getenv("GOSPARK_GATEWAY_LISTEN"); v != "" {
		cfg.Gateway.Listen = v
	}
	if v := os.Getenv("GOSPARK_GATEWAY_UPSTREAM"); v != "" {
		cfg.Gateway.Upstream = v
	}
	if v := os.Getenv("GOSPARK_GATEWAY_MODEL"); v != "" {
		cfg.Gateway.Model = v
	}
	if v := os.Getenv("GOSPARK_GATEWAY_READY_PATH"); v != "" {
		cfg.Gateway.ReadyPath = v
	}
	if v := os.Getenv("GOSPARK_GATEWAY_PROBE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Gateway.ProbeTimeout = d
		}
	}
	if v := os.Getenv("GOSPARK_GATEWAY_MAX_INFLIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Gateway.MaxInflight = n
		}
	}
	if v := os.Getenv("GOSPARK_GATEWAY_MAX_QUEUED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Gateway.MaxQueued = n
		}
	}
	if v := os.Getenv("GOSPARK_GATEWAY_QUEUE_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Gateway.QueueWait = d
		}
	}
	if v := os.Getenv("GOSPARK_MESH_INTERFACE"); v != "" {
		cfg.Mesh.Interface = v
	}
	if v := os.Getenv("GOSPARK_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("GOSPARK_LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
	}
	if v := os.Getenv("GOSPARK_EDGE_TOKEN_CEILING"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Router.EdgeTokenCeiling = n
		}
	}
	if v := os.Getenv("GOSPARK_CLUSTER_TOKEN_FLOOR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Router.ClusterTokenFloor = n
		}
	}
}

// Validate rejects configurations the control plane cannot honour.
func (c Config) Validate() error {
	if c.Router.EdgeTokenCeiling <= 0 {
		return fmt.Errorf("config: router.edge_token_ceiling must be > 0")
	}
	if c.Router.ClusterTokenFloor < c.Router.EdgeTokenCeiling {
		return fmt.Errorf("config: router.cluster_token_floor (%d) must be >= edge_token_ceiling (%d)",
			c.Router.ClusterTokenFloor, c.Router.EdgeTokenCeiling)
	}
	if c.Transport.ChunkSize <= 0 {
		return fmt.Errorf("config: transport.chunk_size must be > 0")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log.level %q is not one of debug|info|warn|error", c.Log.Level)
	}
	if err := validateCompute(c.Compute); err != nil {
		return err
	}
	if c.Gateway.ProbeTimeout < 0 {
		return fmt.Errorf("config: gateway.probe_timeout must be >= 0")
	}
	if c.Gateway.MaxInflight < 0 {
		return fmt.Errorf("config: gateway.max_inflight must be >= 0")
	}
	if c.Gateway.MaxQueued < 0 {
		return fmt.Errorf("config: gateway.max_queued must be >= 0")
	}
	if c.Gateway.QueueWait < 0 {
		return fmt.Errorf("config: gateway.queue_wait must be >= 0")
	}
	if err := validateEmbed(c.Embed); err != nil {
		return err
	}
	if c.KV.Timeout < 0 {
		return fmt.Errorf("config: kv.timeout must be >= 0")
	}
	if c.Backup.Interval < 0 {
		return fmt.Errorf("config: backup.interval must be >= 0")
	}
	return nil
}

// localSandboxOperations mirrors internal/server's closed operation whitelist.
// It is duplicated here — not imported — so config stays free of the server
// package, and a delegated operation can never shadow a sandbox operation.
var localSandboxOperations = map[string]struct{}{
	"arith":  {},
	"digest": {},
}

func validateCompute(c ComputeConfig) error {
	if c.Endpoint == "" {
		if len(c.Operations) > 0 {
			return fmt.Errorf("config: compute.operations set but compute.endpoint is empty")
		}
		return nil
	}
	if len(c.Operations) == 0 {
		return fmt.Errorf("config: compute.endpoint set but compute.operations is empty")
	}
	seen := map[string]struct{}{}
	for _, op := range c.Operations {
		op = strings.TrimSpace(op)
		if op == "" {
			return fmt.Errorf("config: compute.operations contains an empty name")
		}
		if _, ok := localSandboxOperations[op]; ok {
			return fmt.Errorf("config: compute.operation %q shadows a local sandbox operation", op)
		}
		if _, dup := seen[op]; dup {
			return fmt.Errorf("config: compute.operation %q is listed twice", op)
		}
		seen[op] = struct{}{}
	}
	if c.Timeout < 0 {
		return fmt.Errorf("config: compute.timeout must be >= 0")
	}
	return nil
}

func validateEmbed(c EmbedConfig) error {
	if c.Batch < 0 {
		return fmt.Errorf("config: embed.batch must be >= 0")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("config: embed.timeout must be >= 0")
	}
	return nil
}

// splitList splits a comma- or space-separated list, dropping empties.
func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
