# syntax=docker/dockerfile:1
# One image, all binaries, compiled ONCE. The three services (core, runner,
# slackbot) previously each ran their own `go build` — three cold compiles of
# the same code, the most memory-hungry step, three times over. Building every
# binary in a single `go build` shares the dependency compile cache, so it is
# cheaper in both wall-time and peak RAM. Each compose service selects its
# binary via `command:`.
#
# The runtime base carries git + node (the runner needs them for PR checkouts
# and eslint/tsc); core and slackbot share the image — the extra bytes are one
# shared layer. When LLM reviewers return, add the claude CLI here.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Cache mounts persist Go's module + build caches across builds (BuildKit), so a
# repeat build only recompiles the packages that changed — turning a full cold
# compile (10-15 min on a swap-thrashing 1 GB droplet) into ~1-2 min. -ldflags
# "-s -w" strips debug info: smaller binaries and a lighter link step (the
# memory-hungry phase that OOM-kills the compiler on tiny droplets).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/ ./cmd/core ./cmd/runner ./cmd/slackbot ./cmd/casino

FROM node:20-alpine
RUN apk add --no-cache ca-certificates tzdata git && \
    mkdir -p /work && chown node:node /work
COPY --from=build /out/ /usr/local/bin/
ENV WORKDIR=/work
USER node
# No ENTRYPOINT: each compose service sets `command: ["core"|"runner"|"slackbot"]`.
