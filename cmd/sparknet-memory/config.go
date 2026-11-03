package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the memory service configuration. It is intentionally local: the
// service listens on loopback and trusts only the Python gateway token.
type Config struct {
	Listen            string         `yaml:"listen"`
	StateDir          string         `yaml:"state_dir"`
	NodeID            string         `yaml:"node_id"`
	GatewayToken      string         `yaml:"gateway_token"`
	AllowedVaultRoots []string       `yaml:"allowed_vault_roots"`
	RetentionDays     map[string]int `yaml:"retention_days"`
	OpenCodeCommand   []string       `yaml:"opencode_command"`
	MaxRequestBytes   int64          `yaml:"max_request_bytes"`
	QdrantURL         string         `yaml:"qdrant_url"`
	QdrantAPIKey      string         `yaml:"qdrant_api_key"`
	QdrantCollection  string         `yaml:"qdrant_collection"`
	EmbedEndpoint     string         `yaml:"embed_endpoint"`
	EmbedSecret       string         `yaml:"embed_secret"`
	EmbedModel        string         `yaml:"embed_model"`
}

func defaultConfig() Config {
	return Config{
		Listen:          "127.0.0.1:4110",
		MaxRequestBytes: 2 << 20,
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if cfg.Listen == "" {
		cfg.Listen = defaultConfig().Listen
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = defaultConfig().MaxRequestBytes
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("GOSPARK_MEMORY_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_STATE"); v != "" {
		cfg.StateDir = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_NODE_ID"); v != "" {
		cfg.NodeID = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_GATEWAY_TOKEN"); v != "" {
		cfg.GatewayToken = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_VAULT_ROOTS"); v != "" {
		cfg.AllowedVaultRoots = splitList(v)
	}
	if v := os.Getenv("GOSPARK_MEMORY_OPENCODE_CMD"); v != "" {
		cfg.OpenCodeCommand = strings.Fields(v)
	}
	if v := os.Getenv("GOSPARK_MEMORY_QDRANT_URL"); v != "" {
		cfg.QdrantURL = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_QDRANT_API_KEY"); v != "" {
		cfg.QdrantAPIKey = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_QDRANT_COLLECTION"); v != "" {
		cfg.QdrantCollection = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_EMBED_ENDPOINT"); v != "" {
		cfg.EmbedEndpoint = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_EMBED_SECRET"); v != "" {
		cfg.EmbedSecret = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_EMBED_MODEL"); v != "" {
		cfg.EmbedModel = v
	}
	if v := os.Getenv("GOSPARK_MEMORY_MAX_REQUEST_BYTES"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MaxRequestBytes = parsed
		}
	}
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == ',' })
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
