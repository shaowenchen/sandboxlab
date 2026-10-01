# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
# CGO is off so the binary is static and carries no libc dependency of its own.
# Nothing in the module needs cgo — the control plane is HTTP and the Kubernetes
# API — so this is a choice about the artifact rather than a constraint of a
# dependency.
#
# The builder is pinned to $BUILDPLATFORM, and that is not a detail. Left
# unpinned, a multi-arch build pulls an image per target architecture and runs
# this whole stage — the module download and every compilation — under QEMU for
# each one that is not the runner's, which is slow enough to look like a hang.
# Pinned, the stage runs natively and Go cross-compiles, which it does at full
# speed: CGO_ENABLED=0 below means there is no C toolchain to emulate, so the
# target architecture changes only which object files the compiler emits.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG TARGETOS=linux
ARG TARGETARCH

ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src

# Dependencies first, so a source change does not refetch the module graph. This
# layer is keyed on the module files alone and is reused until they move.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/shaowenchen/sandboxlab/internal/buildinfo.Version=${VERSION} \
        -X github.com/shaowenchen/sandboxlab/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/sandbox ./cmd/sandbox \
 && go build -trimpath \
      -ldflags "-s -w \
        -X github.com/shaowenchen/sandboxlab/internal/buildinfo.Version=${VERSION} \
        -X github.com/shaowenchen/sandboxlab/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/sandbox-cli ./cmd/sandbox-cli

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
# Distroless rather than Alpine, and that is the opposite of what an image that
# shells out to git needs — but this one does not. The control plane talks to
# the Kubernetes API and serves HTTP; it runs no subprocess and needs no shell,
# so there is nothing for a base with a package manager to provide. A smaller
# base is also a smaller thing to keep patched.
#
# static-debian12:nonroot is the variant with a user already defined as uid
# 65532, so the pod runs unprivileged without the chart having to arrange it.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/sandbox /usr/local/bin/sandbox
COPY --from=builder /out/sandbox-cli /usr/local/bin/sandbox-cli

# The chart sets the real values; these are what a bare `docker run` of the
# image gets, so it starts somewhere sensible rather than failing on an empty
# configuration.
ENV SANDBOX_LISTEN=:8080 \
    SANDBOX_LOG_LEVEL=info

USER nonroot:nonroot

EXPOSE 8080

# Distroless has no shell, so the binary is the entrypoint directly — there is
# no tini here and none is needed: the process is PID 1 and installs its own
# signal handling, which is what a shell wrapper would otherwise be for.
ENTRYPOINT ["/usr/local/bin/sandbox"]
CMD ["serve"]
