#!/bin/sh
# Builds the static binaries: the agents (embedded in the server) and the server itself.
# Server target platform: GOOS/GOARCH (default linux/amd64). Go is all you need, no C compiler.
set -eu
cd "$(dirname "$0")"
export CGO_ENABLED=0
mkdir -p agent-dist
for arch in amd64 arm64; do
  GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "agent-dist/pulsecheck-agent-linux-$arch" ./cmd/agent
done
GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" go build -trimpath -ldflags="-s -w" -o pulsecheck .
ls -lh pulsecheck agent-dist
