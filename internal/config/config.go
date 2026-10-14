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
}

type GatewayConfig struct {
	Listen   string `yaml:"listen"`
	Upstream string `yaml:"upstream"` // e.g. http://127.0.0.1:8000 (vLLM)
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
	if v := os.Getenv("GOSPARK_TRANSPORT_LISTEN"); v != "" {
		cfg.Transport.Listen = v
	}
	if v := os.Getenv("GOSPARK_GATEWAY_LISTEN"); v != "" {
		cfg.Gateway.Listen = v
	}
	if v := os.Getenv("GOSPARK_GATEWAY_UPSTREAM"); v != "" {
		cfg.Gateway.Upstream = v
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
	return nil
}
