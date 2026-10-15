package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/config"
)

func TestDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bus.SubjectRoot != "sparknet" {
		t.Errorf("subject_root = %q", cfg.Bus.SubjectRoot)
	}
	if cfg.Router.EdgeTokenCeiling != 2048 {
		t.Errorf("edge ceiling = %d", cfg.Router.EdgeTokenCeiling)
	}
	if cfg.Transport.ChunkSize != 256*1024 {
		t.Errorf("chunk size = %d", cfg.Transport.ChunkSize)
	}
	if cfg.Admin.Listen != "" {
		t.Errorf("admin listen = %q, want empty (disabled by default)", cfg.Admin.Listen)
	}
}

func TestValidateRejectsBadFloor(t *testing.T) {
	cfg := config.Default()
	cfg.Router.ClusterTokenFloor = cfg.Router.EdgeTokenCeiling - 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when cluster floor < edge ceiling")
	}
}

func TestValidateRejectsBadLogLevel(t *testing.T) {
	cfg := config.Default()
	cfg.Log.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown log level")
	}
}

func TestValidateRejectsZeroChunk(t *testing.T) {
	cfg := config.Default()
	cfg.Transport.ChunkSize = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for zero chunk size")
	}
}

func TestLoadYAMLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	body := "router:\n  edge_token_ceiling: 512\n  cluster_token_floor: 4096\n  max_nodes: 2\nlog:\n  level: debug\n  format: json\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Router.EdgeTokenCeiling != 512 || cfg.Router.MaxNodes != 2 {
		t.Fatalf("yaml not applied: %+v", cfg.Router)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Fatalf("log yaml not applied: %+v", cfg.Log)
	}
	// Defaults survive for keys the file omitted.
	if cfg.Bus.SubjectRoot != "sparknet" {
		t.Fatalf("default lost: %q", cfg.Bus.SubjectRoot)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("GOSPARK_NODE_ID", "node-env")
	t.Setenv("GOSPARK_BUS_URL", "nats://example:4222")
	t.Setenv("GOSPARK_BUS_QUEUE", "control")
	t.Setenv("GOSPARK_REGISTRY_FILE", "/etc/sparknet/registry.json")
	t.Setenv("GOSPARK_ADMIN_LISTEN", "127.0.0.1:8899")
	t.Setenv("GOSPARK_LOG_LEVEL", "warn")
	t.Setenv("GOSPARK_EDGE_TOKEN_CEILING", "777")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.ID != "node-env" {
		t.Errorf("node id = %q", cfg.Node.ID)
	}
	if cfg.Bus.URL != "nats://example:4222" {
		t.Errorf("bus url = %q", cfg.Bus.URL)
	}
	if cfg.Bus.Queue != "control" {
		t.Errorf("bus queue = %q", cfg.Bus.Queue)
	}
	if cfg.Registry.File != "/etc/sparknet/registry.json" {
		t.Errorf("registry file = %q", cfg.Registry.File)
	}
	if cfg.Admin.Listen != "127.0.0.1:8899" {
		t.Errorf("admin listen = %q", cfg.Admin.Listen)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log level = %q", cfg.Log.Level)
	}
	if cfg.Router.EdgeTokenCeiling != 777 {
		t.Errorf("edge ceiling = %d", cfg.Router.EdgeTokenCeiling)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEnvOverridesGatewayAndCompute(t *testing.T) {
	t.Setenv("GOSPARK_GATEWAY_MODEL", "Qwen/Qwen2.5-32B-Instruct")
	t.Setenv("GOSPARK_GATEWAY_READY_PATH", "/health")
	t.Setenv("GOSPARK_GATEWAY_PROBE_TIMEOUT", "5s")
	t.Setenv("GOSPARK_COMPUTE_ENDPOINT", "http://127.0.0.1:9090")
	t.Setenv("GOSPARK_COMPUTE_SECRET", "s")
	t.Setenv("GOSPARK_COMPUTE_OPERATIONS", "infer, embed")
	t.Setenv("GOSPARK_COMPUTE_TIMEOUT", "30s")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Model != "Qwen/Qwen2.5-32B-Instruct" {
		t.Errorf("gateway model = %q", cfg.Gateway.Model)
	}
	if cfg.Gateway.ReadyPath != "/health" {
		t.Errorf("gateway ready_path = %q", cfg.Gateway.ReadyPath)
	}
	if cfg.Gateway.ProbeTimeout != 5*time.Second {
		t.Errorf("gateway probe_timeout = %v", cfg.Gateway.ProbeTimeout)
	}
	if cfg.Compute.Endpoint != "http://127.0.0.1:9090" || cfg.Compute.Secret != "s" {
		t.Errorf("compute = %+v", cfg.Compute)
	}
	if len(cfg.Compute.Operations) != 2 || cfg.Compute.Operations[0] != "infer" || cfg.Compute.Operations[1] != "embed" {
		t.Errorf("compute operations = %v", cfg.Compute.Operations)
	}
	if cfg.Compute.Timeout != 30*time.Second {
		t.Errorf("compute timeout = %v", cfg.Compute.Timeout)
	}
}

func TestValidateRejectsComputeShadowsSandbox(t *testing.T) {
	cfg := config.Default()
	cfg.Compute = config.ComputeConfig{Endpoint: "http://127.0.0.1:9090", Operations: []string{"digest"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("compute operation shadowing the sandbox should be rejected")
	}
}

func TestValidateRejectsEndpointWithoutOperations(t *testing.T) {
	cfg := config.Default()
	cfg.Compute = config.ComputeConfig{Endpoint: "http://127.0.0.1:9090"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("compute endpoint without operations should be rejected")
	}
}

func TestValidateRejectsOperationsWithoutEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Compute = config.ComputeConfig{Operations: []string{"infer"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("compute operations without an endpoint should be rejected")
	}
}

func TestValidateRejectsNegativeProbeTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.Gateway.ProbeTimeout = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative gateway.probe_timeout should be rejected")
	}
}

func TestEnvOverridesBackup(t *testing.T) {
	t.Setenv("GOSPARK_BACKUP_SOURCE", "/mnt/knowledge")
	t.Setenv("GOSPARK_BACKUP_TARGET", "/mnt/data/backups")
	t.Setenv("GOSPARK_BACKUP_KEY_FILE", "/etc/sparknet/node.key")
	t.Setenv("GOSPARK_BACKUP_INTERVAL", "6h")
	t.Setenv("GOSPARK_BACKUP_SKIP_DIRS", ".git, node_modules")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Source != "/mnt/knowledge" || cfg.Backup.Target != "/mnt/data/backups" {
		t.Errorf("backup = %+v", cfg.Backup)
	}
	if cfg.Backup.KeyFile != "/etc/sparknet/node.key" {
		t.Errorf("backup key_file = %q", cfg.Backup.KeyFile)
	}
	if cfg.Backup.Interval != 6*time.Hour {
		t.Errorf("backup interval = %v", cfg.Backup.Interval)
	}
	if len(cfg.Backup.SkipDirs) != 2 || cfg.Backup.SkipDirs[0] != ".git" {
		t.Errorf("backup skip_dirs = %v", cfg.Backup.SkipDirs)
	}
}

func TestValidateRejectsNegativeBackupInterval(t *testing.T) {
	cfg := config.Default()
	cfg.Backup.Interval = -time.Hour
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative backup.interval should be rejected")
	}
}
