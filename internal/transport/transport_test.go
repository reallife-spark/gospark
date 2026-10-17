package transport_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/kvtransfer"
	"github.com/reallife-spark/goSpark/internal/transport"
)

// startServer launches a data-plane server on a random port and returns it, the
// bound address and a stop function.
func startServer(t *testing.T) (*transport.Server, string, func()) {
	t.Helper()
	received := make(chan apiv1.BlockHeader, 4)
	ready := make(chan net.Addr, 1)
	srv := &transport.Server{
		Addr:      "127.0.0.1:0",
		ChunkSize: 8, // force multiple chunks
		Ready:     ready,
		Handler: func(_ context.Context, h apiv1.BlockHeader, b *kvtransfer.Block) (string, error) {
			received <- h
			return b.Digest(), nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.ListenAndServe(ctx) }()
	select {
	case addr := <-ready:
		return srv, addr.String(), cancel
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server never became ready")
		return nil, "", nil
	}
}

func sampleBlock(t *testing.T) *kvtransfer.Block {
	t.Helper()
	b, err := (kvtransfer.ReferenceCodec{}).Encode([]float32{1, -2, 3, -4, 5.5, 6, -0.5, 0.25, 2, 1.5, 0, -6})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSendBlockRoundTrip(t *testing.T) {
	srv, addr, stop := startServer(t)
	defer stop()
	block := sampleBlock(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := transport.SendBlock(ctx, addr, "blk-1", "node-a", block, 8)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if ack != block.Digest() {
		t.Fatalf("ack = %q, want %q", ack, block.Digest())
	}
	if srv.Fingerprint() == "" {
		t.Fatal("server must expose a certificate fingerprint")
	}
}

func TestSendBlockWithPinnedFingerprint(t *testing.T) {
	srv, addr, stop := startServer(t)
	defer stop()
	block := sampleBlock(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := transport.SendBlockWithOptions(ctx, addr, "blk-2", "node-a", block, 8, transport.DialOptions{
		PinnedFingerprint: srv.Fingerprint(),
	})
	if err != nil {
		t.Fatalf("pinned send should succeed: %v", err)
	}
	if ack != block.Digest() {
		t.Fatalf("ack = %q, want %q", ack, block.Digest())
	}
}

func TestSendBlockRejectsWrongPin(t *testing.T) {
	_, addr, stop := startServer(t)
	defer stop()
	block := sampleBlock(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A valid-length but wrong fingerprint must be refused at the TLS layer.
	wrong := "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := transport.SendBlockWithOptions(ctx, addr, "blk-3", "node-a", block, 8, transport.DialOptions{
		PinnedFingerprint: wrong,
	}); err == nil {
		t.Fatal("expected dial to fail on fingerprint mismatch")
	}
}

func TestSendBlockToDeadPeerTimesOut(t *testing.T) {
	block, _ := (kvtransfer.ReferenceCodec{}).Encode([]float32{1, 2, 3})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := transport.SendBlock(ctx, "127.0.0.1:1", "b", "n", block, 0); err == nil {
		t.Fatal("expected an error dialing a dead peer")
	}
}

func TestCertFingerprintStable(t *testing.T) {
	a := []byte("hello")
	first, second := transport.CertFingerprint(a), transport.CertFingerprint(a)
	if first != second {
		t.Fatal("fingerprint must be stable")
	}
	if transport.CertFingerprint(a) == transport.CertFingerprint([]byte("world")) {
		t.Fatal("different certs must differ")
	}
	if len(transport.CertFingerprint(a)) != 64 {
		t.Fatalf("sha256 hex should be 64 chars")
	}
}
