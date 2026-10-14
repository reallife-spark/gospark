# goSpark — Go control/service plane for SparkNet.

GO ?= go
BIN := bin
PKGS := ./...

.PHONY: help
help:
	@echo "goSpark targets:"
	@echo "  make build        build all binaries into $(BIN)/"
	@echo "  make test         run all tests"
	@echo "  make test-race    run tests with the race detector"
	@echo "  make cover        coverage summary"
	@echo "  make vet          go vet ./..."
	@echo "  make fmt          gofmt -w ."
	@echo "  make fmt-check    fail if any file needs gofmt"
	@echo "  make lint         build + vet + fmt-check (the gate)"
	@echo "  make ci           fmt-check + vet + test-race"
	@echo "  make keys-check   refuse if a key-shaped file is tracked"
	@echo "  make clean        remove build output"

.PHONY: build
build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/sparknet-control ./cmd/sparknet-control
	$(GO) build -o $(BIN)/sparknet-agent ./cmd/sparknet-agent
	$(GO) build -o $(BIN)/sparknetctl ./cmd/sparknetctl

.PHONY: test
test:
	$(GO) test $(PKGS)

.PHONY: test-race
test-race:
	$(GO) test -race $(PKGS)

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
