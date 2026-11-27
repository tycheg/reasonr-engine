GO ?= go
FUZZTIME ?= 20s
# Two Go modules developed together (go.work): the core (the root) and the
# contract of plugins (port/, tags port/vX.Y.Z). The tests read the example
# models from the compiler module the core requires; the taxi example
# requires the auth module by version: make modules puts it into the module
# cache. They read the models outside this module, and go test does not
# recheck such files when it reuses a cached result: -count=1.
COMPILER = $$($(GO) list -m -f '{{.Dir}}' github.com/tycheg/reasonr-compiler)

.PHONY: all check modules fmt-check vet lint test test-race bench fuzz scale update build install images

all: check

check: fmt-check vet lint test

modules:
	$(GO) build -o bin/reasonr github.com/tycheg/reasonr-compiler/cmd/reasonr
	cd "$(COMPILER)/examples/taxi" && $(CURDIR)/bin/reasonr mod download

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...
	cd port && $(GO) vet ./...

lint:
	staticcheck ./...
	cd port && staticcheck ./...

test: modules
	$(GO) test -count=1 ./...
	cd port && $(GO) test ./...

test-race: modules
	$(GO) test -race -count=1 ./...
	cd port && $(GO) test -race ./...

# Benchmarks of the test runs and the memory engine (the targets are in RFC §29).
bench: modules
	$(GO) test ./internal/testrun ./internal/adapters/memory -run='^$$' -bench=. -benchmem

# A short run of the fuzz test of the memory engine; the nightly run uses FUZZTIME=1h.
fuzz: modules
	$(GO) test ./internal/adapters/memory -run='^$$' -fuzz=FuzzEngine -fuzztime=$(FUZZTIME)

# Measurements at scale (docs/plans/0015): loading the program and the engine
# tests on a synthetic model, SCALE_MODEL — the directory make scale of
# reasonr-compiler writes (.scale/model-1m there).
SCALE ?= 1m
SCALE_MODEL ?= ../reasonr-compiler/.scale/model-$(SCALE)
scale:
	mkdir -p .scale
	$(GO) run ./internal/scale --model $(SCALE_MODEL) --budget -o .scale/engine-$(SCALE).json

# Update the golden files of the packages whose tests import golden (the
# others have no -update flag).
GOLDEN = $(GO) list -f '{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}'
update: modules
	$(GO) test $$($(GOLDEN) ./... | grep /internal/golden | cut -d" " -f1) -update

build:
	$(GO) build -o bin/reasonr-build ./cmd/reasonr-build

install:
	$(GO) install ./cmd/reasonr-build

# The reasonr-builder image: reasonr-build with the Go toolchain.
images:
	docker build -f cmd/reasonr-build/Dockerfile -t reasonr-builder:0.1.0 .
