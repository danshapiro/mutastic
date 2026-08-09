#!/usr/bin/env bash
# Cross-compile mutastic.exe for Windows from WSL.
set -euo pipefail
cd "$(dirname "$0")"
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc \
  go build -ldflags "-s -w" -o bin/mutastic.exe .
echo "built bin/mutastic.exe"
