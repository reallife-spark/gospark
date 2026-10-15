# goSpark — Go control/service plane for SparkNet.

GO ?= go
BIN := bin
PKGS := ./...

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
	@echo "  make lint         build + vet + fmt-check (the gate)"
	@echo "  make ci           fmt-check + vet + test-race"
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

# The GPU telemetry sidecar is the one cgo binary: it links NVML (go-nvlib) and
# DCGM (go-dcgm) and degrades to "unavailable" on a host with no driver.
.PHONY: build-gpu
build-gpu:
	@mkdir -p $(BIN)
	CGO_ENABLED=1 $(GO) build -o $(BIN)/sparknet-gpu ./cmd/sparknet-gpu

.PHONY: test
test:
	$(GO) test $(PKGS)

.PHONY: test-race
test-race:
	$(GO) test -race $(PKGS)

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

.PHONY: lint
lint: build vet fmt-check
	@echo "lint: ok"

.PHONY: ci
ci: fmt-check vet test-race
	@echo "ci: ok"

# No private key material is ever committed. This mirrors the fabric's
# keys-check: a tracked key-shaped file fails the build.
.PHONY: keys-check
keys-check:
	@files=$$(git ls-files | grep -E '(\.key$$|\.pem$$|_key\.json$$|\.state/keys/|\.state/.*\.key$$)' || true); \
	if [ -n "$$files" ]; then echo "keys-check: refusing, tracked key files:"; echo "$$files"; exit 1; fi
	@echo "keys-check: ok"

.PHONY: clean
clean:
	rm -rf $(BIN)
