// Package mesh manages the WireGuard overlay the control plane runs over.
//
// This scaffold keeps the mesh surface honest and small: it reports peer state
// by parsing `wg show`, and reroutes by rewriting peer endpoints. It does not
// link against wgctrl/netlink in this pass; the interface exists so a real
// driver can be dropped in without touching the control plane. Callers see
// ErrNoWireGuard when the tool is absent, never a silent success.
package mesh

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ErrNoWireGuard is returned when the wg tool is unavailable on this host.
var ErrNoWireGuard = errors.New("mesh: wg tool not available on this host")

// Peer is one WireGuard peer as reported by the kernel/tooling.
type Peer struct {
	PublicKey     string
	Endpoint      string
	AllowedIPs    []string
	LatestHandske int64 // unix seconds, 0 if never
	RxBytes       int64
	TxBytes       int64
}

// Interface is the mesh surface the control plane depends on.
type Interface interface {
	// PeerState returns the peers currently configured on iface.
	PeerState(iface string) ([]Peer, error)
	// Reroute sets the endpoint of the peer with the given public key.
	Reroute(iface, publicKey, endpoint string) error
	// Available reports whether the underlying tool exists.
	Available() bool
}

// WG is the tool-backed implementation.
type WG struct {
	// Bin is the wg binary path (default "wg").
	Bin string
}

// NewWG builds a WG interface using the given binary name.
func NewWG(bin string) *WG {
	if bin == "" {
		bin = "wg"
	}
	return &WG{Bin: bin}
}

// Available reports whether the wg binary is on PATH.
func (w *WG) Available() bool {
	_, err := exec.LookPath(w.Bin)
	return err == nil
}

// PeerState parses `wg show <iface> dump`.
func (w *WG) PeerState(iface string) ([]Peer, error) {
	if !w.Available() {
		return nil, ErrNoWireGuard
	}
	cmd := exec.Command(w.Bin, "show", iface, "dump")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mesh: wg show %s: %w", iface, err)
	}
	return parseDump(string(out)), nil
}

// Reroute updates a peer endpoint with `wg set`.
func (w *WG) Reroute(iface, publicKey, endpoint string) error {
	if !w.Available() {
		return ErrNoWireGuard
	}
	if err := exec.Command(w.Bin, "set", iface, "peer", publicKey, "endpoint", endpoint).Run(); err != nil {
		return fmt.Errorf("mesh: reroute %s: %w", publicKey, err)
	}
	return nil
}

// parseDump parses the tab-separated `wg show ... dump` format. The first line
// describes the interface; subsequent lines are peers.
func parseDump(s string) []Peer {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var peers []Peer
	line := 0
	for sc.Scan() {
		line++
		if line == 1 { // interface line
			continue
		}
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 8 {
			continue
		}
		p := Peer{
			PublicKey:     fields[0],
			Endpoint:      fields[2],
			AllowedIPs:    splitCSV(fields[3]),
			LatestHandske: atoi64(fields[4]),
		}
		peers = append(peers, p)
	}
	return peers
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func atoi64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
