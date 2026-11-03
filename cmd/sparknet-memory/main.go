// Command sparknet-memory is the loopback memory service. It stores typed,
// per-vault records and delegate runs for the node. It holds no signing key and
// accepts only the local gateway token; authority is verified by the Python
// gateway that fronts it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/reallife-spark/goSpark/internal/embed"
	"github.com/reallife-spark/goSpark/internal/memory"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-memory:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("sparknet-memory", flag.ExitOnError)
	configPath := flags.String("config", "", "YAML configuration file")
	listen := flags.String("listen", "", "loopback listen address (default 127.0.0.1:4110)")
	state := flags.String("state", "", "memory state directory")
	token := flags.String("gateway-token", "", "local gateway token (prefer GOSPARK_MEMORY_GATEWAY_TOKEN)")
	nodeID := flags.String("node-id", "", "node id used to namespace vault ids")
	var roots stringList
	flags.Var(&roots, "vault-root", "allowed vault root (repeatable)")
	_ = flags.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *state != "" {
		cfg.StateDir = *state
	}
	if *token != "" {
		cfg.GatewayToken = *token
	}
	if *nodeID != "" {
		cfg.NodeID = *nodeID
	}
	if len(roots) > 0 {
		cfg.AllowedVaultRoots = append([]string(nil), roots...)
	}
	if cfg.StateDir == "" {
		cfg.StateDir = stateDirFromEnv()
	}
	if cfg.StateDir == "" {
		return errors.New("state directory is required (--state or GOSPARK_MEMORY_STATE)")
	}
	if cfg.GatewayToken == "" {
		return errors.New("gateway token is required")
	}
	if len(cfg.AllowedVaultRoots) == 0 {
		return errors.New("at least one --vault-root or GOSPARK_MEMORY_VAULT_ROOTS is required")
	}
	if cfg.NodeID == "" {
		cfg.NodeID, _ = os.Hostname()
	}

	manager, err := memory.NewManager(memory.Config{
		Root:              cfg.StateDir,
		NodeID:            cfg.NodeID,
		AllowedVaultRoots: cfg.AllowedVaultRoots,
	})
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var semantic *memory.QdrantIndex
	if cfg.QdrantURL != "" && cfg.EmbedEndpoint != "" {
		embedder, err := embed.NewHTTPEmbedder(embed.Options{
			Endpoint: cfg.EmbedEndpoint,
			Secret:   cfg.EmbedSecret,
			Model:    cfg.EmbedModel,
		})
		if err != nil {
			logger.Warn("semantic index disabled", "error", err)
		} else {
			semantic = memory.NewQdrantIndex(memory.QdrantConfig{
				URL:        cfg.QdrantURL,
				APIKey:     cfg.QdrantAPIKey,
				Collection: cfg.QdrantCollection,
			}, embedder)
		}
	}
	svc := newService(cfg, manager, logger)
	svc.semantic = semantic

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           svc.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // delegate event streams are long-lived
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	logger.Info("sparknet-memory listening", "addr", cfg.Listen, "state", cfg.StateDir, "roots", len(cfg.AllowedVaultRoots), "node", cfg.NodeID)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("empty vault root")
	}
	*s = append(*s, value)
	return nil
}
