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

# ── the SDK ─────────────────────────────────────────────────────────────────

# Regenerate sdk/ from api/openapi.yaml. The generated code is committed, so an
# external consumer can `go get` the package without installing the generator;
# this is only run when the spec changes.
#
#	Go comes from oapi-codegen (via go:generate beside the code it produces);
#	Python, TypeScript and Java come from openapi-generator, which is a Java
#	program this script downloads and pins. One script rather than a rule per
#	language, because they are generated together from one document and a
#	language left out is a language describing an API that no longer exists.
#
#	Local note: this machine cannot reach proxy.golang.org, so a first run needs
#	GOPROXY=https://goproxy.cn,direct. CI has normal network and does not.
.PHONY: sdk
sdk:
	./hack/generate-sdks.sh

# sdk-check fails when the committed generated code disagrees with the spec, so
# a spec edited without regenerating is a red build rather than a lie. It is
# what makes "the spec is the source of truth" a property rather than a promise.
#
# It regenerates into the working tree and diffs, so run it on a clean checkout
# — which is exactly what CI does.
.PHONY: sdk-check
sdk-check:
	./hack/generate-sdks.sh
	@if ! git diff --quiet -- sdk/; then \
	  echo "sdk/ is out of date:"; \
	  git diff --stat -- sdk/; \
	  echo "run \`make sdk\` and commit the result"; \
	  exit 1; \
	fi
	@# The SDK must stay importable by an external module, which means it may
	@# not reach into internal/. Nothing else checks this, and breaking it would
	@# make the package useless to the person most likely to want it.
	@if $(GO) list -deps ./sdk/go | grep -q 'sandboxlab/internal/'; then \
	  echo "sdk imports internal/, so no external module could use it:"; \
	  $(GO) list -deps ./sdk/go | grep 'sandboxlab/internal/'; \
	  exit 1; \
	fi

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

# Package the chart into a Helm repository directory. Needs helm; the same
# script CI runs, so a local publish and a published one cannot diverge.
#   make chart-package VERSION=0.1.0-dev PAGES=./pages
#
# The version defaults to what this commit would publish — the same script CI
# runs — rather than a literal here, so a chart packaged by hand cannot be
# stamped with a version no image is published under.
.PHONY: chart-package
chart-package: PAGES ?= ./pages
chart-package:
	@mkdir -p $(PAGES)
	./hack/package-chart.sh $${VERSION:-$$(./hack/chart-version.sh)} $${APP_VERSION:-$$(git rev-parse --short HEAD)} $(PAGES) $${REPO_URL:-https://www.chenshaowen.com/sandboxlab}

# Render this repository's documentation into the static site published
# alongside the chart. Needs no helm and no cluster: it reads the markdown and
# writes HTML, and fails on a link that would be dead on the site.
#   make docs PAGES=./pages
.PHONY: docs
docs: PAGES ?= ./pages
docs:
	@mkdir -p $(PAGES)
	go run ./cmd/gendocs -dest $(PAGES) \
		-repo $${REPO_URL:-https://github.com/shaowenchen/sandboxlab} \
		-branch $${REPO_BRANCH:-main}
