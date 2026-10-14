# The goSpark data plane: NVFP4 KV blocks over HTTP/3

## Why QUIC, not TCP

The design warns about cross-WAN, lossy links (a WireGuard overlay across
unreliable paths). On a single TCP stream, **one lost packet stalls every block
queued behind it** — head-of-line blocking is exactly the pathology that makes a
KV-block transfer feel like a hang. QUIC multiplexes independent streams over
UDP, so a lost block delays only itself.

## Why NVFP4

KV-cache transfer moves the most bytes of anything in the system. NVFP4 (4-bit
E2M1 with a per-16-element block scale and a tensor-global scale) is the Wire
format that makes the transfer affordable on a Blackwell-class host. goSpark's
`internal/kvtransfer` is the **reference** codec: pure Go, CPU, no CUDA. The
production path hands packed blocks to the GPU runtime, which is the compute
plane.

The codec is `Codec`-shaped (`Encode`/`Decode`) so a hardware implementation can
replace `ReferenceCodec` without touching callers.

## Framing

A stream is `header-frame`, then `chunk-frame*`, then a zero-length
`terminator`, then the peer's `ack-frame`:

```
┌──────────────┬────────────┬────────────┬─────┬────────────┬───────────┐
│ header frame │ chunk 0    │ chunk 1    │ ... │ terminator │ ack frame │
│ (BlockHeader │ (≤64 MiB)  │            │     │ (len 0)    │ (digest)  │
│  JSON)       │            │            │     │            │           │
└──────────────┴────────────┴────────────┴─────┴────────────┴───────────┘
```

Every frame is a 4-byte big-endian length followed by that many bytes. A frame
larger than 64 MiB is refused (`readFrame`), so a hostile peer cannot make the
receiver allocate unbounded memory.

The `BlockHeader.Digest` is asserted against the recomputed digest of the
decoded block before the handler runs: a truncated or tampered payload is
refused before it reaches the delta cache.

## Delta sync

`kvtransfer.DeltaCache` stores blocks by id and returns a stable digest. Two
blocks with the same digest carry the same bytes, so a peer can advertise the
digests it has and receive only the rest (`apiv1.SemanticDelta`). Delta sync is
sound precisely because the digest is over the *canonical* payload, not over the
in-memory struct.

## TLS and peer pinning

The overlay is self-formed, so there is no CA to chain to. Authentication is by
**certificate pinning**: the listener's leaf certificate is hashed
(`SHA-256` of the DER, `transport.CertFingerprint`) and the dialer refuses any
peer whose presented certificate does not match that hash
(`DialOptions.PinnedFingerprint`).

`Server.Fingerprint()` returns the listener's pin — it is logged at startup and
is the value an operator copies to the peer's `sparknetctl send-block -pin` or
to the peer's config. Verification happens in `VerifyPeerCertificate` over the
raw certificate, so no CA chain is trusted; `InsecureSkipVerify` is set only
because the default chain check is not the mechanism here.

An **empty** pin dials unauthenticated (`SendBlock`). That path exists for
single-process tests and is never appropriate for a real transfer: without a
pin, TLS still encrypts but authenticates nothing, so a wrong peer is
indistinguishable from the right one. `send-block` therefore accepts `-pin` and
callers are expected to set it.

When the scaffold's throwaway certificate is replaced by one bound to the
node's overlay identity, the pin mechanism is unchanged — only the source of
the certificate changes.

## Non-goals

- No TCP fallback: if QUIC is unavailable the data plane is unavailable.
- No congestion-control tuning here; the design's BBR/MTU notes target the
  WireGuard layer (`internal/mesh`), not this package.
