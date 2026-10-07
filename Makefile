# VibeCI development tasks. `make help` lists them; AGENTS.md explains the
# test tiers. Only Go and (for the docker tiers) a Docker engine with the
# compose plugin are needed.

GO      ?= go
DOCKER  ?= docker
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Docker-backed tests bind-mount work dirs: keep them where the engine can
# see them (with Colima or Docker Desktop: under $HOME).
TESTDATA ?= $(CURDIR)/.tmp-test
# Real-model fragment for e2e-live (see e2e/llm.example.jsonc).
E2E_LLM ?= .secrets/e2e-llm.jsonc

E2E = VIBECI_E2E=1 VIBECI_E2E_DIR=$(TESTDATA)/e2e $(GO) test -count=1 -v

.PHONY: help all build lint fmt test race test-docker images e2e e2e-docker e2e-compose e2e-live ci clean

help: ## list targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

all: lint test build ## lint, unit tests, build

build: ## build bin/vibeci
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/vibeci ./cmd/vibeci

lint: ## gofmt check and go vet
	@out="$$(gofmt -l cmd internal e2e)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

fmt: ## gofmt -w
	gofmt -w cmd internal e2e

test: ## unit tests (no docker, no network)
	$(GO) test ./...

race: ## unit tests with the race detector
	$(GO) test -race ./...

test-docker: ## sandbox policy tests against a real Docker engine
	mkdir -p $(TESTDATA)
	VIBECI_DOCKER_HOST="$${DOCKER_HOST:-$$($(DOCKER) context inspect --format '{{.Endpoints.docker.Host}}')}" \
	VIBECI_TEST_DATA=$(TESTDATA) $(GO) test -count=1 -v -run Docker ./internal/sandbox

images: ## build the harness and sandbox images
	$(DOCKER) build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t vibeci:latest .
	$(DOCKER) build -f deploy/sandbox/base.Dockerfile -t vibeci-sandbox:base deploy/sandbox
	$(DOCKER) build -f deploy/sandbox/go.Dockerfile -t vibeci-sandbox:go deploy/sandbox

e2e: ## end to end: fake model, unsafe-local sandbox
	$(E2E) -timeout 20m ./e2e

e2e-docker: ## end to end: fake model, docker sandboxes
	VIBECI_E2E_SANDBOX=docker $(E2E) -timeout 30m ./e2e

e2e-compose: ## end to end: the production docker compose stack, fake model
	VIBECI_E2E_COMPOSE=1 $(E2E) -timeout 30m -run TestCompose ./e2e

e2e-live: ## end to end: real models (E2E_LLM=file), docker sandboxes, incl. fatih/color (network)
	@test -f "$(E2E_LLM)" || { echo "e2e-live needs a real-model fragment: E2E_LLM=$(E2E_LLM) (see e2e/llm.example.jsonc)"; exit 2; }
	VIBECI_E2E_SANDBOX=docker VIBECI_E2E_LLM_CONFIG=$(E2E_LLM) VIBECI_E2E_OSS=1 $(E2E) -timeout 90m ./e2e

ci: lint race e2e test-docker e2e-docker e2e-compose ## everything CI runs (no secrets needed)

clean: ## remove build output and test work dirs
	chmod -R u+w $(TESTDATA)/e2e 2>/dev/null || true
	rm -rf bin $(TESTDATA)/e2e
