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

func TestSendBlockRoundTrip(t *testing.T) {
	codec := kvtransfer.ReferenceCodec{}
	block, err := codec.Encode([]float32{1, -2, 3, -4, 5.5, 6, -0.5, 0.25, 2, 1.5, 0, -6})
	if err != nil {
		t.Fatal(err)
	}

	received := make(chan apiv1.BlockHeader, 1)
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
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx) }()

	var addr net.Addr
	select {
	case addr = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sendCancel()
	ack, err := transport.SendBlock(sendCtx, addr.String(), "blk-1", "node-a", block, 8)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if ack != block.Digest() {
		t.Fatalf("ack = %q, want %q", ack, block.Digest())
	}

	select {
	case h := <-received:
		if h.BlockID != "blk-1" || h.OriginNodeID != "node-a" {
			t.Fatalf("header = %+v", h)
		}
		if h.Digest != block.Digest() {
			t.Fatalf("header digest = %q, want %q", h.Digest, block.Digest())
		}
		if h.NumElements != uint32(block.NumElements) {
			t.Fatalf("num elements = %d, want %d", h.NumElements, block.NumElements)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
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
