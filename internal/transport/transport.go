// Package transport is the goSpark data plane: NVFP4 KV blocks streamed over
// HTTP/3's transport (QUIC), kept strictly separate from the control-plane bus.
//
// Why QUIC and not TCP: a single dropped packet on a TCP stream stalls every
// queued block behind it (head-of-line blocking) — the exact pathology the
// design warns about on a lossy WireGuard WAN. QUIC multiplexes independent
// streams over UDP, so one lost block does not delay the others.
package transport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/kvtransfer"
)

// Handler is called once per fully received block. The returned string is the
// ack sent back to the sender (typically the stored digest).
type Handler func(ctx context.Context, header apiv1.BlockHeader, block *kvtransfer.Block) (string, error)

// Server receives KV blocks.
type Server struct {
	Addr      string
	ChunkSize int
	Handler   Handler
	// Ready, if set, receives the actually bound address once the listener is
	// up. It exists because Addr may be ":0"; callers that need the real port
	// (tests, LocalAddr reporting) read it here.
	Ready chan<- net.Addr
}

// BlockHeaderFor builds the wire header for a block.
func BlockHeaderFor(blockID, originNodeID string, b *kvtransfer.Block, now time.Time) apiv1.BlockHeader {
	return apiv1.BlockHeader{
		BlockID:       blockID,
		OriginNodeID:  originNodeID,
		Digest:        b.Digest(),
		NumElements:   uint32(b.NumElements),
		NumBlocks:     uint32((b.NumElements + kvtransfer.BlockSize - 1) / kvtransfer.BlockSize),
		GlobalScale:   b.GlobalScale,
		DType:         kvtransfer.DType,
		CreatedAtUnix: now.Unix(),
	}
}

// ListenAndServe blocks serving QUIC streams until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	tlsConf, err := selfSignedTLS()
	if err != nil {
		return err
	}
	ln, err := quic.ListenAddr(s.Addr, tlsConf, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("transport: listen %s: %w", s.Addr, err)
	}
	defer ln.Close()
	if s.Ready != nil {
		s.Ready <- ln.Addr()
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("transport: accept: %w", err)
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Server) serveConn(ctx context.Context, conn *quic.Conn) {
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go func() {
			defer stream.Close()
			if err := s.handleStream(ctx, stream); err != nil {
				_ = writeFrame(stream, []byte("ERR:"+err.Error()))
			}
		}()
	}
}

func (s *Server) handleStream(ctx context.Context, stream *quic.Stream) error {
	headerBytes, err := readFrame(stream)
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	var header apiv1.BlockHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return fmt.Errorf("parse header: %w", err)
	}
	payload, err := s.readChunks(stream)
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	block, err := kvtransfer.DecodeCanonical(payload)
	if err != nil {
		return fmt.Errorf("decode block: %w", err)
	}
	if got := block.Digest(); got != header.Digest {
		return fmt.Errorf("digest mismatch: header=%s computed=%s", header.Digest, got)
	}
	ack := header.Digest
	if s.Handler != nil {
		ack, err = s.Handler(ctx, header, block)
		if err != nil {
			return err
		}
	}
	return writeFrame(stream, []byte(ack))
}

func (s *Server) readChunks(r io.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := readFrame(r)
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			return out, nil
		}
		out = append(out, chunk...)
	}
}

// SendBlock streams a block to addr and returns the server's ack.
func SendBlock(ctx context.Context, addr string, blockID, originNodeID string, b *kvtransfer.Block, chunkSize int) (string, error) {
	if chunkSize <= 0 {
		chunkSize = 256 * 1024
	}
	conn, err := quic.DialAddr(ctx, addr, &tls.Config{
		// The scaffold pins nothing; a deployed node verifies the peer's
		// overlay certificate. See docs/transport.md.
		InsecureSkipVerify: true,
		NextProtos:         []string{ALPN},
		MinVersion:         tls.VersionTLS13,
	}, &quic.Config{MaxIdleTimeout: 30 * time.Second})
	if err != nil {
		return "", fmt.Errorf("transport: dial %s: %w", addr, err)
	}
	defer conn.CloseWithError(0, "done")

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return "", fmt.Errorf("transport: open stream: %w", err)
	}
	defer stream.Close()

	header := BlockHeaderFor(blockID, originNodeID, b, time.Now().UTC())
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	if err := writeFrame(stream, headerBytes); err != nil {
		return "", err
	}
	payload := b.Canonical()
	for off := 0; off < len(payload); off += chunkSize {
		end := off + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		if err := writeFrame(stream, payload[off:end]); err != nil {
			return "", err
		}
	}
	if err := writeFrame(stream, nil); err != nil {
		return "", err
	}
	ack, err := readFrame(stream)
	if err != nil {
		return "", fmt.Errorf("transport: read ack: %w", err)
	}
	return string(ack), nil
}

func writeFrame(w io.Writer, b []byte) error {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 {
		return nil, nil
	}
	if n > 64<<20 {
		return nil, fmt.Errorf("transport: frame %d exceeds 64 MiB cap", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
