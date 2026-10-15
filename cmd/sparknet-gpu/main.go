// Command sparknet-gpu is the GPU-telemetry sidecar: the one goSpark binary
// that links NVML and DCGM. It collects a node's GPU snapshot and either prints
// it (once) or publishes it to the control plane on the health subject (watch).
//
// It is deliberately separate from sparknet-agent: the agent is a pure-Go,
// statically linkable binary with no GPU dependency, and this sidecar is the
// only cgo/GPU binary. A host with no NVIDIA driver, or a CGO_ENABLED=0 build,
// still produces an honest "unavailable" answer rather than crashing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/gputel"

	// Register the cgo GPU backends. These are the only imports that pull a GPU
	// library into the binary.
	_ "github.com/reallife-spark/goSpark/internal/gputel/dcgm"
	_ "github.com/reallife-spark/goSpark/internal/gputel/nvml"
	"github.com/reallife-spark/goSpark/internal/gputel/sim"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-gpu:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("sparknet-gpu", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (YAML)")
	watch := fs.Bool("watch", false, "publish telemetry on a cadence instead of printing once")
	once := fs.Bool("once", false, "collect once and print (default when -watch is not set)")
	interval := fs.Duration("interval", 30*time.Second, "poll interval for -watch")
	nodeID := fs.String("node-id", "", "node id to report as (defaults to config node.id)")
	overlay := fs.String("overlay", "", "overlay address to report (defaults to config node.overlay_ipv4)")
	busURL := fs.String("bus-url", "", "bus URL override (defaults to config bus.url; empty => in-memory)")
	subjectRoot := fs.String("subject-root", "", "bus subject root override (defaults to config bus.subject_root)")
	text := fs.Bool("text", false, "human-readable output instead of JSON (once mode)")
	simFixture := fs.String("sim-fixture", "", "read a synthetic snapshot from this JSON fixture file (opt-in; also SPARKNET_GPU_FIXTURE)")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *busURL != "" {
		cfg.Bus.URL = *busURL
	}
	if *subjectRoot != "" {
		cfg.Bus.SubjectRoot = *subjectRoot
	}
	id := *nodeID
	if id == "" {
		id = cfg.Node.ID
	}
	ov := *overlay
	if ov == "" {
		ov = cfg.Node.OverlayIPv4
	}

	if *simFixture != "" {
		// Opt-in synthetic backend for hosts with no NVIDIA hardware; telemetry
		// only, never authority. A no-op registration-wise if the environment
		// already set SPARKNET_GPU_FIXTURE.
		sim.SetFixture(*simFixture)
	}
	collector := gputel.Default()

	if !*watch || *once {
		return printOnce(collector, id, ov, *text)
	}
	if id == "" {
		return fmt.Errorf("-watch requires -node-id (or config node.id)")
	}
	return watchAndPublish(cfg, collector, id, ov, *interval)
}

// printOnce collects and prints one snapshot, without touching the bus.
func printOnce(c gputel.Collector, nodeID, overlay string, text bool) error {
	snap := c.Snapshot(context.Background())
	tel := snap.Wire()
	if text {
		fmt.Printf("node %s  gpu telemetry: %s (available=%t)\n", nodeID, tel.Source, tel.Available)
		if tel.Reason != "" {
			fmt.Printf("  reason: %s\n", tel.Reason)
		}
		if tel.DriverVersion != "" {
			fmt.Printf("  driver: %s\n", tel.DriverVersion)
		}
		for _, g := range tel.GPUs {
			fmt.Printf("  gpu %d %s temp=%dC mem=%d/%d util=%d%% power=%dmW health=%s\n",
				g.Index, g.Name, g.TempC, g.MemUsedBytes, g.MemTotalBytes, g.UtilPct, g.PowerMilliwatts, g.Health)
		}
		return nil
	}
	out := struct {
		NodeID       string             `json:"node_id,omitempty"`
		Overlay      string             `json:"overlay_address,omitempty"`
		GPUTelemetry apiv1.GpuTelemetry `json:"gpu_telemetry"`
	}{NodeID: nodeID, Overlay: overlay, GPUTelemetry: tel}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// watchAndPublish collects on a cadence and publishes a NodeHealth carrying the
// GPU snapshot on the health subject. Health is telemetry: the control plane
// records it and never treats it as authority.
func watchAndPublish(cfg config.Config, c gputel.Collector, nodeID, overlay string, interval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var b bus.Bus
	if cfg.Bus.URL == "" {
		b = bus.NewMemory()
	} else {
		n, err := bus.Connect(bus.NATSOptions{
			URL:      cfg.Bus.URL,
			Name:     "gospark-gpu-" + nodeID,
			Stream:   cfg.Bus.Stream,
			Subjects: []string{cfg.Bus.SubjectRoot + ".>"},
		})
		if err != nil {
			return err
		}
		b = n
	}
	defer b.Close()

	subject := bus.Subject(cfg.Bus.SubjectRoot, bus.KindHealth, "")
	fmt.Fprintf(os.Stderr, "sparknet-gpu: watching via %s, publishing to %s every %s\n", cfg.Bus.URL, subject, interval)

	publish := func() {
		tel := c.Snapshot(ctx).Wire()
		h := apiv1.NodeHealth{NodeID: nodeID, OverlayAddress: overlay, ReportedAtUnix: time.Now().Unix(), GPUTelemetry: &tel}
		data, err := json.Marshal(apiv1.RegisterNode{Health: &h})
		if err != nil {
			fmt.Fprintln(os.Stderr, "sparknet-gpu: marshal:", err)
			return
		}
		if err := b.Publish(ctx, subject, data); err != nil {
			fmt.Fprintln(os.Stderr, "sparknet-gpu: publish:", err)
		}
	}
	publish()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			publish()
		}
	}
}
