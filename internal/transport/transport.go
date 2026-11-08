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
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
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

	// ClientFingerprint, if set, requires mutual TLS: the connecting peer must
	// present a client certificate whose hex SHA-256 matches this value.
	ClientFingerprint string
	// AllowUnauthenticated permits an unpinned non-loopback listener. It is for
	// an explicit single-process rehearsal; a deployment must pin.
	AllowUnauthenticated bool
	// MaxInflightBytes bounds the memory reserved by concurrent streams. A
	// stream reserves maxBlockBytes before reading; zero uses 256 MiB.
	MaxInflightBytes int64

	inflightBytes int64
	inflightMu    sync.Mutex

	// tlsOnce caches the listener certificate so its fingerprint is available to
	// a peer before the stream arrives.
	tlsOnce        sync.Once
	tlsConf        *tls.Config
	tlsFingerprint string
	tlsErr         error
}

// Fingerprint returns the hex SHA-256 of the server's data-plane certificate,
// the value a peer must pin. It is safe to call before listening: the
// certificate is generated on first use.
func (s *Server) Fingerprint() string {
	s.tlsOnce.Do(func() {
		s.tlsConf, s.tlsFingerprint, s.tlsErr = selfSignedTLS()
	})
	return s.tlsFingerprint
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
	if s.MaxInflightBytes <= 0 {
		s.MaxInflightBytes = 256 << 20
	}
	s.tlsOnce.Do(func() {
		s.tlsConf, s.tlsFingerprint, s.tlsErr = selfSignedTLS()
	})
	if s.tlsErr != nil {
		return s.tlsErr
	}
	if s.ClientFingerprint == "" && !s.AllowUnauthenticated && !loopbackHost(s.Addr) {
		return fmt.Errorf("transport: non-loopback listener %q has no client certificate pin", s.Addr)
	}
	if s.ClientFingerprint != "" {
		s.tlsConf.ClientAuth = tls.RequireAnyClientCert
		want := strings.ToLower(strings.TrimSpace(s.ClientFingerprint))
		s.tlsConf.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("transport: client presented no certificate")
			}
			if got := CertFingerprint(rawCerts[0]); got != want {
				return fmt.Errorf("transport: client certificate fingerprint mismatch: got %s want %s", got, want)
			}
			return nil
		}
	}
	ln, err := quic.ListenAddr(s.Addr, s.tlsConf, &quic.Config{
		MaxIdleTimeout:     30 * time.Second,
		KeepAlivePeriod:    10 * time.Second,
		MaxIncomingStreams: 64,
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

func (s *Server) reserveStream() bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflightBytes+maxBlockBytes > s.MaxInflightBytes {
		return false
	}
	s.inflightBytes += maxBlockBytes
	return true
}

func (s *Server) releaseStream() {
	s.inflightMu.Lock()
	s.inflightBytes -= maxBlockBytes
	if s.inflightBytes < 0 {
		s.inflightBytes = 0
	}
	s.inflightMu.Unlock()
}

func (s *Server) handleStream(ctx context.Context, stream *quic.Stream) error {
	if !s.reserveStream() {
		return errors.New("transport: in-flight block budget exhausted")
	}
	defer s.releaseStream()
	_ = stream.SetReadDeadline(time.Now().Add(30 * time.Second))
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
	if header.BlockID == "" || len(header.BlockID) > 256 {
		return errors.New("invalid block id")
	}
	if header.NumElements != uint32(block.NumElements) ||
		header.NumBlocks != uint32((block.NumElements+kvtransfer.BlockSize-1)/kvtransfer.BlockSize) ||
		header.GlobalScale != block.GlobalScale ||
		header.DType != kvtransfer.DType {
		return errors.New("block header does not match decoded block")
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

const maxBlockBytes = 64 << 20

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
		if len(out)+len(chunk) > maxBlockBytes {
			return nil, fmt.Errorf("block exceeds %d bytes", maxBlockBytes)
		}
		out = append(out, chunk...)
	}
}

// DialOptions controls how SendBlock authenticates the peer.
type DialOptions struct {
	// AllowUnauthenticated permits an empty pin. It exists only for the
	// in-process SendBlock scaffold; callers outside that test must pin.
	AllowUnauthenticated bool

	// PinnedFingerprint, if non-empty, is the hex SHA-256 of the peer's leaf
	// certificate (see Server.Fingerprint and CertFingerprint). When set, the
	// dial fails unless the presented certificate matches exactly.
	PinnedFingerprint string

	// ClientCertificate, if non-nil, is presented for mutual TLS. The server
	// pins its fingerprint via Server.ClientFingerprint.
	ClientCertificate *tls.Certificate
}

func loopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientTLS builds the dialer's TLS config. With a pin, verification is done by
// comparing the raw peer certificate's fingerprint — no CA chain is trusted,
// which is the correct model for a self-formed overlay. Without a pin the dial
// is unauthenticated and is only acceptable in a single-process scaffold.
func clientTLS(opts DialOptions) (*tls.Config, error) {
	conf := &tls.Config{
		NextProtos: []string{ALPN},
		MinVersion: tls.VersionTLS13,
		// The peer certificate is not CA-issued; when pinned we verify it
		// ourselves in VerifyPeerCertificate, so the default chain check is off.
		InsecureSkipVerify: true,
	}
	if opts.ClientCertificate != nil {
		conf.Certificates = []tls.Certificate{*opts.ClientCertificate}
	}
	rawPin := opts.PinnedFingerprint
	pin := strings.TrimSpace(rawPin)
	if rawPin != "" && pin == "" {
		return nil, errors.New("transport: certificate pin is empty")
	}
	if pin == "" {
		if !opts.AllowUnauthenticated {
			return nil, errors.New("transport: a certificate pin is required")
		}
		return conf, nil
	}
	conf.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("transport: peer presented no certificate")
		}
		got := CertFingerprint(rawCerts[0])
		if !strings.EqualFold(got, pin) {
			return fmt.Errorf("transport: certificate fingerprint mismatch: got %s want %s", got, strings.ToLower(pin))
		}
		return nil
	}
	return conf, nil
}

// SendBlock streams a block to addr and returns the server's ack. It performs
// no peer authentication and exists only for the in-process transport test.
// Callers outside that test must use SendBlockWithOptions with a pin.
//
// Deprecated: use SendBlockWithOptions with DialOptions.PinnedFingerprint.
func SendBlock(ctx context.Context, addr string, blockID, originNodeID string, b *kvtransfer.Block, chunkSize int) (string, error) {
	return SendBlockWithOptions(ctx, addr, blockID, originNodeID, b, chunkSize, DialOptions{AllowUnauthenticated: true})
}

// SendBlockWithOptions streams a block to addr and returns the server's ack.
func SendBlockWithOptions(ctx context.Context, addr string, blockID, originNodeID string, b *kvtransfer.Block, chunkSize int, opts DialOptions) (string, error) {
	if chunkSize <= 0 {
		chunkSize = 256 * 1024
	}
	if blockID == "" || len(blockID) > 256 {
		return "", errors.New("transport: block id is empty or too long")
	}
	tlsConf, err := clientTLS(opts)
	if err != nil {
		return "", err
	}
	conn, err := quic.DialAddr(ctx, addr, tlsConf, &quic.Config{MaxIdleTimeout: 30 * time.Second})
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
