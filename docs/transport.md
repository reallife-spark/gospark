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

## TLS in this scaffold

`transport` uses an ephemeral self-signed certificate and the sender dials with
`InsecureSkipVerify`. This is a scaffold: there is no peer authentication yet.
A deployed node must verify the peer's overlay certificate, not skip it. This is
flagged in the code and is the first thing to harden before any real transfer.

## Non-goals

- No TCP fallback: if QUIC is unavailable the data plane is unavailable.
- No congestion-control tuning here; the design's BBR/MTU notes target the
  WireGuard layer (`internal/mesh`), not this package.
