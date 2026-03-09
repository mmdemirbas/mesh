#!/usr/bin/env bash
set -e

# Change to the project directory
cd "$(dirname "$0")"

mkdir -p bin
rm -f bin/*

echo "🔨 Building for macOS M1 (Darwin/ARM64)..."
GOOS=darwin GOARCH=arm64 go build -o bin/mesh-darwin-arm64 ./cmd/mesh/

echo "🔨 Building for Linux/WSL (Linux/AMD64)..."
GOOS=linux GOARCH=amd64 go build -o bin/mesh-linux-amd64 ./cmd/mesh/

echo "🔨 Building for Windows (Windows/AMD64)..."
GOOS=windows GOARCH=amd64 go build -o bin/mesh-windows-amd64.exe ./cmd/mesh/

echo "✅ Build complete! Binaries are located in the bin/ directory:"
ls -lh bin/
