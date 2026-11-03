# ADR 0016 — Delegate runs are local, bounded and recorded

## Status

Accepted, 2026-11-02.

## Context

A coordinator should be able to hand parts of a request to specialist harnesses
without giving a harness the coordinator's authority or an unbounded workspace.
The fabric already distinguishes a signed session grant from an execution
adapter; the delegate broker must not blur that line.

## Decision

- A plan is validated before execution: unique task ids, known harness, known
  dependencies, no cycles.
- Every task has an explicit budget (wall time and actionable steps) and an
  explicit permission block (workspace mode, network, allowed environment).
- Runs and normalized events are durable and scoped to the same vault.
- The broker runs no model and holds no key. It accepts only a plan that the
  Python gateway already scoped to a user and vault.
- A failed task never reruns the original request; consumers are blocked.
- A delegate writes to its workspace. A human approves a report before Python
  provenance writes and signs a vault page.

## Consequences

Delegates are observable and cancellable, and a harness cannot turn execution
into authority. Cross-node full-harness delegation still needs its own contract;
the first implementation is local-first.
