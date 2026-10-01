# sandboxlab — build, test and package.
#
# The same targets are what CI runs, so "it works locally" and "it works in the
# build" are one answer rather than two.

MODULE  := github.com/shaowenchen/sandboxlab
BIN     := bin
IMAGE   ?= docker.io/shaowenchen/sandboxlab
TAG     ?= dev
# The version and commit are stamped into the binaries so a running control
# plane can say what it is, which is the first question anyone asks of one.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
  -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
  -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT)

GO ?= go
# -mod=mod so a target works from a clean checkout; CI sets its own GOFLAGS.
GOFLAGS ?= -mod=mod

.PHONY: all
all: fmt vet test build

.PHONY: build
build: build-server build-cli

.PHONY: build-server
build-server:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/sandbox ./cmd/sandbox

.PHONY: build-cli
build-cli:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/sandbox-cli ./cmd/sandbox-cli

.PHONY: run
run: build-server
	SANDBOX_LOG_LEVEL=debug $(BIN)/sandbox serve --listen :8080

.PHONY: test
test:
	$(GO) test ./...

.PHONY: test-race
test-race:
	$(GO) test -race ./...

.PHONY: coverage
coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	$(GO) fmt ./...

# fmt-check fails rather than rewriting, so CI never edits a tree it is meant to
# be judging.
.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l . | grep -v '^vendor/' || true); \
	if [ -n "$$out" ]; then echo "these files are not gofmt'd:"; echo "$$out"; exit 1; fi

.PHONY: check
check: fmt-check vet test

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out

# ── images and charts ───────────────────────────────────────────────────────

.PHONY: docker-build
docker-build:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  -t $(IMAGE):$(TAG) .

.PHONY: docker-push
docker-push:
	docker push $(IMAGE):$(TAG)

.PHONY: helm-lint
helm-lint:
	helm lint charts/sandbox

.PHONY: helm-template
helm-template:
	helm template sandbox charts/sandbox

# helm-check renders the chart and asserts the things the deployment depends on,
# which lint and template do not: a ServiceAccount the Deployment names, the
# namespace it lands in, the API key reaching the pod.
.PHONY: helm-check
helm-check: helm-template
	@hack/helm-check.sh

.PHONY: package-chart
package-chart:
	hack/package-chart.sh
