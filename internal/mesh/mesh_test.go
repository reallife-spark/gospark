package mesh

import "testing"

func TestParseDump(t *testing.T) {
	// First line is the interface; the rest are peers (tab-separated).
	dump := "PRIVATE\tPUBLIC\t51820\toff\n" +
		"PEERPUBKEY1\t(none)\t1.2.3.4:51820\t10.44.0.2/32\t1700000000\t1024\t2048\t25\n" +
		"PEERPUBKEY2\t(none)\t(none)\t10.44.0.3/32,10.44.1.0/24\t0\t0\t0\toff\n"

	peers := parseDump(dump)
	if len(peers) != 2 {
		t.Fatalf("parsed %d peers, want 2", len(peers))
	}
	if peers[0].PublicKey != "PEERPUBKEY1" || peers[0].Endpoint != "1.2.3.4:51820" {
		t.Fatalf("peer 0 = %+v", peers[0])
	}
	if len(peers[0].AllowedIPs) != 1 || peers[0].AllowedIPs[0] != "10.44.0.2/32" {
		t.Fatalf("peer 0 allowed ips = %v", peers[0].AllowedIPs)
	}
	if peers[0].LatestHandske != 1700000000 {
		t.Fatalf("peer 0 handshake = %d", peers[0].LatestHandske)
	}
	if len(peers[1].AllowedIPs) != 2 {
		t.Fatalf("peer 1 allowed ips = %v", peers[1].AllowedIPs)
	}
}

func TestParseDumpIgnoresShortLines(t *testing.T) {
	if got := parseDump("iface\nshort\tline\n"); len(got) != 0 {
		t.Fatalf("expected no peers, got %d", len(got))
	}
}

func TestWGUnavailable(t *testing.T) {
	w := NewWG("definitely-not-a-real-binary-xyz")
	if w.Available() {
		t.Skip("unexpectedly found the binary; skipping availability assertion")
	}
	if _, err := w.PeerState("wg0"); err != ErrNoWireGuard {
		t.Fatalf("PeerState err = %v, want ErrNoWireGuard", err)
	}
	if err := w.Reroute("wg0", "pub", "1.2.3.4:1"); err != ErrNoWireGuard {
		t.Fatalf("Reroute err = %v, want ErrNoWireGuard", err)
	}
}

func TestNewWGDefaultBin(t *testing.T) {
	if NewWG("").Bin != "wg" {
		t.Fatal("default bin must be wg")
	}
}
