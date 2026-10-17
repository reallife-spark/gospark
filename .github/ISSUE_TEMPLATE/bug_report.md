---
name: Bug report
about: Something in goSpark — a binary, a package, the CLI or the gateway — is wrong
title: ""
labels: ""
assignees: ""
---

## What happened

<!-- What you observed, and what you expected instead. -->

## How to reproduce

```
# the smallest sequence of commands, from the repo root
```

## Where

<!-- The binary, package or command at fault, with line numbers if you have them. -->

## What the gates say

<!-- Output of `make ci`, `make test-nats`, or the single failing test. Paste
     the failing lines, not the whole log. -->

## Environment

- Commit:
- Go version (`go version`):
- Does it need a GPU/driver? <!-- the core is CGO_ENABLED=0 and must not -->
- `make keys-check` result, if the report is about secrets
