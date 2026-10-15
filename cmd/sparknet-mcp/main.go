// Command sparknet-mcp is a stdio Model Context Protocol server that exposes a
// running goSpark control plane's read-only admin surface as MCP tools. Point an
// MCP client (an agent, an IDE) at this binary and it can inspect node health,
// GPU telemetry, the registry and the metrics, but not mutate anything.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/reallife-spark/goSpark/internal/mcp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("sparknet-mcp", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr(), "control-plane admin base URL")
	timeout := fs.Duration("timeout", 0, "per-request timeout (0 uses the default)")
	_ = fs.Parse(args)

	srv := &mcp.Server{AdminBase: *addr, Timeout: *timeout}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

func defaultAddr() string {
	if v := os.Getenv("GOSPARK_ADMIN_ADDR"); v != "" {
		return v
	}
	return "http://127.0.0.1:8899"
}
