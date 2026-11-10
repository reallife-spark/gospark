# goSpark — Go control/service plane for SparkNet.

GO ?= go
BIN := bin
PKGS := ./...
GOLANGCI_LINT ?= golangci-lint

# Evaluation (make eval) — reference adoption of lm-evaluation-harness, driven
# from the pinned clone rather than a tracked dependency (fabric Step 51a). Point
# EVAL_BASE_URL at any OpenAI-compatible /v1/completions endpoint to score a real
# model; leave it empty to self-host the deterministic CPU stub tier.
LMEVAL_DIR ?= /mnt/nvme/github/lm-evaluation-harness
LMEVAL_PY ?= $(LMEVAL_DIR)/.venv/bin/python
EVAL_BASE_URL ?=
EVAL_PORT ?= 18443
EVAL_MODEL ?= sparknet-stub
EVAL_TASKS ?= spark_smoke
EVAL_INCLUDE ?= eval
EVAL_LIMIT ?= 3
EVAL_OUT ?= .state/eval

.PHONY: help
help:
	@echo "goSpark targets:"
	@echo "  make build        build the pure-Go binaries into $(BIN)/ (CGO_ENABLED=0)"
	@echo "  make build-gpu    build the cgo GPU sidecar (sparknet-gpu, links NVML/DCGM)"
	@echo "  make test         run all tests"
	@echo "  make test-race    run tests with the race detector"
	@echo "  make test-nats    run the NATS/JetStream integration tests"
	@echo "  make nats-dev     start a local JetStream server for manual runs"
	@echo "  make cover        coverage summary"
	@echo "  make vet          go vet ./..."
	@echo "  make fmt          gofmt -w ."
	@echo "  make fmt-check    fail if any file needs gofmt"
	@echo "  make lint         build + vet + fmt-check + golangci-lint (the gate)"
	@echo "  make ci           fmt-check + vet + lint + test-race"
	@echo "  make eval         drive lm-eval (pinned clone) against the stub tier"
	@echo "  make keys-check   refuse if a key-shaped file is tracked"
	@echo "  make clean        remove build output"

# The core binaries are deliberately built with CGO_ENABLED=0: they are the
# static, GPU-free control/service plane (ADR 0001). The GPU sidecar is the only
# cgo binary and is built separately by build-gpu.
.PHONY: build
build:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknet-control ./cmd/sparknet-control
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknet-agent ./cmd/sparknet-agent
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknetctl ./cmd/sparknetctl
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknet-ingest ./cmd/sparknet-ingest
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknet-backup ./cmd/sparknet-backup
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknet-mcp ./cmd/sparknet-mcp
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknet-memory ./cmd/sparknet-memory
	CGO_ENABLED=0 $(GO) build -o $(BIN)/sparknet-understand ./cmd/sparknet-understand
	# CPU-only OpenAI-compatible vLLM stub: lets a machine with no GPU bring the
	# whole node graph up with a real round-trip through the agent gateway.
	CGO_ENABLED=0 $(GO) build -o $(BIN)/stub-vllm ./cmd/stub-vllm

# The GPU telemetry sidecar is the one cgo binary: it links NVML (go-nvlib) and
# DCGM (go-dcgm) and degrades to "unavailable" on a host with no driver.
.PHONY: build-gpu
build-gpu:
	@mkdir -p $(BIN)
	CGO_ENABLED=1 $(GO) build -o $(BIN)/sparknet-gpu ./cmd/sparknet-gpu

.PHONY: test
test:
	$(GO) test $(PKGS)

# The race detector needs cgo, so a CGO_ENABLED=0 build (a CI runner with no
# NVIDIA C stack, or any bare clone) falls back to plain tests and says so
# rather than failing on -race itself. Locally CGO is on and the race detector
# still runs.
.PHONY: test-race
test-race:
	@if [ "$(CGO_ENABLED)" = "0" ]; then \
		echo "test-race: race detector SKIPPED — CGO_ENABLED=0, running plain tests"; \
		$(GO) test $(PKGS); \
	else \
		$(GO) test -race $(PKGS); \
	fi

# The NATS integration tests skip themselves when nats-server is absent; this
# target names them explicitly so a missing server is visible rather than silent.
.PHONY: test-nats
test-nats:
	$(GO) test -run 'NATS|OverNATS' -v $(PKGS)

# Throwaway JetStream server for manual end-to-end runs. Point both configs at
# nats://127.0.0.1:4222 and leave bus.url empty to use the in-memory bus instead.
.PHONY: nats-dev
nats-dev:
	@mkdir -p .state/nats
	nats-server -a 127.0.0.1 -p 4222 -js -sd .state/nats

.PHONY: cover
cover:
	$(GO) test -cover $(PKGS)

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "fmt: clean"

.PHONY: golangci-lint
golangci-lint:
	@if command -v $(GOLANGCI_LINT) >/dev/null 2>&1; then \
		echo "golangci-lint: run ./..."; \
		$(GOLANGCI_LINT) run ./...; \
	else \
		echo "golangci-lint: SKIPPED — not installed"; \
		echo "  install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		echo "  config:  $(CURDIR)/.golangci.yml"; \
	fi

.PHONY: lint
lint: build vet fmt-check golangci-lint
	@echo "lint: ok"

.PHONY: ci
ci: fmt-check vet golangci-lint test-race
	@echo "ci: ok"

# No private key material is ever committed. This mirrors the fabric's
# keys-check: a tracked key-shaped file fails the build.
.PHONY: keys-check
keys-check:
	@files=$$(git ls-files | grep -E '(\.key$$|\.pem$$|_key\.json$$|\.state/keys/|\.state/.*\.key$$)' || true); \
	if [ -n "$$files" ]; then echo "keys-check: refusing, tracked key files:"; echo "$$files"; exit 1; fi
	@echo "keys-check: ok"

# Reference evaluation gate (fabric Step 51a). Drives lm-evaluation-harness
# against an OpenAI-compatible endpoint and fails non-zero if the harness does.
# The default tier is the deterministic stub (cmd/stub-vllm): the pinned task
# `eval/spark_smoke.yaml` targets the stub's exact echo, so exact_match = 1.0
# proves request/response/metric wiring end to end; against a real model the
# same task reports a real score. lm-eval lives in the pinned clone's venv, not
# in this module's dependency graph, so a missing venv is an honest SKIP.
.PHONY: eval
eval: ## Reference eval: lm-eval (pinned clone) against the stub tier
	@set -eu; \
	if [ ! -x "$(LMEVAL_PY)" ]; then \
		echo "eval: SKIPPED — no lm-eval venv at $(LMEVAL_PY)"; \
		echo "  set up once:  python3 -m venv $(LMEVAL_DIR)/.venv"; \
		echo "                $(LMEVAL_DIR)/.venv/bin/pip install -e '$(LMEVAL_DIR)[api]'"; \
		exit 0; \
	fi; \
	base_url="$(EVAL_BASE_URL)"; \
	stub_pid=""; \
	if [ -z "$$base_url" ]; then \
		$(MAKE) --no-print-directory build >/dev/null; \
		mkdir -p "$(EVAL_OUT)"; \
		./$(BIN)/stub-vllm -host 127.0.0.1 -port $(EVAL_PORT) -model $(EVAL_MODEL) >"$(EVAL_OUT)/stub.log" 2>&1 & \
		stub_pid=$$!; \
		trap 'kill $$stub_pid 2>/dev/null || true' EXIT; \
		ready=0; \
		for _ in $$(seq 1 50); do \
			if curl -sf "http://127.0.0.1:$(EVAL_PORT)/health" >/dev/null 2>&1; then ready=1; break; fi; \
			sleep 0.1; \
		done; \
		if [ "$$ready" -ne 1 ]; then echo "eval: stub never became healthy"; cat "$(EVAL_OUT)/stub.log"; exit 1; fi; \
		base_url="http://127.0.0.1:$(EVAL_PORT)/v1/completions"; \
	fi; \
	echo "eval: base_url=$$base_url tasks=$(EVAL_TASKS)"; \
	"$(LMEVAL_PY)" -m lm_eval \
		--model local-completions \
		--model_args "base_url=$$base_url,tokenizer_backend=none" \
		--tasks $(EVAL_TASKS) \
		--include_path $(EVAL_INCLUDE) \
		--limit $(EVAL_LIMIT) \
		--output_path "$(EVAL_OUT)" \
		--log_samples; \
	echo "eval: results in $(EVAL_OUT)"

.PHONY: clean
clean:
	rm -rf $(BIN)
