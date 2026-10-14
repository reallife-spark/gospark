// Package testutil holds helpers shared by integration tests. It is not
// imported by production code.
package testutil

import (
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// HasNATSServer reports whether the nats-server binary is on PATH.
func HasNATSServer() bool {
	_, err := exec.LookPath("nats-server")
	return err == nil
}

// NATSServer starts a throwaway nats-server with JetStream enabled and returns
// its client URL. It skips the test if the binary is absent and stops the
// server when the test ends.
func NATSServer(t *testing.T) string {
	t.Helper()
	if !HasNATSServer() {
		t.Skip("nats-server not on PATH; skipping NATS integration test")
	}
	port := freePort(t)
	store := t.TempDir()
	cmd := exec.Command("nats-server",
		"-a", "127.0.0.1",
		"-p", fmt.Sprint(port),
		"-js",
		"-sd", store,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nats-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		nc, err := nats.Connect(url, nats.Timeout(500*time.Millisecond))
		if err == nil {
			nc.Close()
			return url
		}
		if time.Now().After(deadline) {
			t.Fatalf("nats-server never became ready at %s", url)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
