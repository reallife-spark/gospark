---
name: Feature request
about: Propose a change to a package, a binary, the wire contract or a seam
title: ""
labels: ""
assignees: ""
---

## The problem

<!-- What is hard or impossible today. Be concrete about the surface. -->

## The proposal

<!-- What you would change and where. If this touches the on-wire contract
     (proto/sparknet/v1) or the fabric's contract snapshot (spec/sparknet),
     say how the fabric side changes too. -->

## Boundary check

<!-- Does this keep the pure-Go core free of a GPU/model/compute dependency?
     If it adds a seam, name it (internal/gputel, internal/compute, ...). -->

## Resource-first check

<!-- Before building a container, model or service of our own, check NVIDIA's
     catalog first (in the fabric: `make nv-search q="..."`). Paste the verdict,
     or say why this is not a catalog question. -->

## Design

<!-- If the decision is not obvious from the code, this becomes an ADR in
     docs/adr/ — what is the decision and its main consequence? -->

## What would make this done

<!-- The observable criterion a reviewer can check — a command and its output. -->
