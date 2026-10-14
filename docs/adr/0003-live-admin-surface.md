# ADR 0003 — A loopback-only live admin surface for registry control

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** goSpark could mint (`registry sign`) and revoke
  (`registry revoke`) a registry *on disk*, but a running control plane served
  whatever it was started with. Changing the authorizations of a live control
  plane meant editing the file and restarting. The fabric names three Master
  rights — `sign-authorization`, `revoke-node`, `distribute-registry` — and none
  had a running-process trigger.

## Decision

Add an HTTP admin surface to the control plane, bound **loopback-only** and
**disabled by default** (`admin.listen` / `GOSPARK_ADMIN_LISTEN`).

- **Three routes, one write path.** `GET /admin/registry` snapshots the served
  registry; `POST /admin/registry/authorize` grants or updates one node;
  `POST /admin/registry/revoke` flips one node's revocation flag;
  `PUT /admin/registry` replaces the whole authorization set. Every mutating
  route funnels through `App.mutateRegistry`, which re-signs each entry with a
  fresh stamp, re-derives the accepted executor keys and re-publishes — the same
  path `ReSignRegistry` (the config-driven distribute) now shares, so the two
  cannot drift apart.
- **An operator CLI, not a bespoke client.** `sparknetctl admin
  show|authorize|revoke` speaks to the surface, mirroring the on-disk
  `registry` commands.
- **Full authority implies a loopback binding.** The process holds the Master
  private key, so the surface can grant, revoke and distribute at will. The
  server refuses to bind any non-loopback address (`requireLoopback`) rather
  than rely on an operator not to expose it.

## Why not a remote admin API

Remote administration needs its own authentication — a signed admin request
verified against an `admin-supervise` capability, plus replay protection — which
is a fabric-level design that is still marked `UNSPECIFIED`. Shipping an
unauthenticated remote surface "for now" would be exactly the kind of
authority-without-key-possession the fabric forbids. Keeping it loopback-only
means the only way to reach it is to already be on the master host.

## Consequences

1. **Live registry control without a restart.** Grant, revoke and replace take
   effect immediately: the digest advances, the change re-publishes to peers,
   and `executorPubs` is refreshed under its own lock (`execMu`) so a live
   mutation does not race the result-subscription goroutine.
2. **A revoked node's key lingers in `executorPubs`.** Refresh only adds or
   updates — dropping explicit `-executors` keys supplied outside the registry
   would be wrong. It is not exploitable: a result is signature-checked first,
   then refused by `authorizeNode` when the registry no longer authorizes the
   node, so revocation takes effect regardless.
3. **A mismatched identity projection is tolerated, not rejected.** A grant
   carries the node's identity projection; if the operator supplies a
   `requested_capabilities` that differs from the node's own, the entry's
   fingerprint no longer equals the node's. Authorization matches by `node_id`,
   so this is functional, but the projection should be copied faithfully — the
   CLI defaults `-req-caps` to `-caps` only as a convenience.

## Not decided here

- A signed-request admin path for remote operation (needs the fabric to specify
  the admin capability and replay rules).
- Whether the admin surface should also expose node *health* (the control
  plane's `Registry` of `NodeHealth` reports) rather than only authorizations.
