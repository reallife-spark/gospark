// Command sparknet-agent runs a goSpark node agent: it reports health, accepts
// signed tasks from the control plane, executes a closed operation whitelist,
// and returns an attested result. It holds no control-plane authority.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
	"github.com/reallife-spark/goSpark/internal/registryfile"
	"github.com/reallife-spark/goSpark/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("sparknet-agent", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (YAML)")
	keyFile := fs.String("key", "", "node identity key file")
	masterPub := fs.String("master-pub", "", "control-plane public key (base64)")
	registryFile := fs.String("registry", "", "Master-signed node registry file to adopt at startup (overrides config registry.file)")
	transportListen := fs.String("transport-listen", "", "override transport listen address")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *keyFile == "" {
		return fmt.Errorf("-key is required")
	}
	if *masterPub == "" {
		return fmt.Errorf("-master-pub is required")
	}
	id, err := identity.Load(*keyFile)
	if err != nil {
		return err
	}
	pub, err := identity.ParsePublicKeyB64(*masterPub)
	if err != nil {
		return err
	}
	cfg.Node.ID = id.NodeID
	cfg.Node.OwnerID = id.OwnerID

	regDoc, err := loadRegistryDoc(*registryFile, cfg.Registry.File)
	if err != nil {
		return err
	}

	app, err := server.New(server.Options{
		Config:          cfg,
		Identity:        id,
		Role:            server.RoleAgent,
		MasterPublic:    pub,
		Registry:        regDoc,
		TransportListen: *transportListen,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx)
}

// loadRegistryDoc loads a signed registry the agent adopts at startup. The agent
// verifies it against the pinned master key when adopting, so a stale or forged
// file fails loudly rather than silently authorizing nobody.
func loadRegistryDoc(flagPath, cfgPath string) (*apiv1.NodeRegistry, error) {
	path := flagPath
	if path == "" {
		path = cfgPath
	}
	if path == "" {
		return nil, nil
	}
	return registryfile.Load(path)
}
