#!/bin/sh
set -eu
mesh_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$mesh_root"
mkdir -p dist bin
for mesh_os in darwin linux; do
  for mesh_arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS="$mesh_os" GOARCH="$mesh_arch" go build -trimpath -o "dist/mesh-$mesh_os-$mesh_arch" ./cmd/mesh
  done
done
go build -trimpath -o bin/mesh ./cmd/mesh
printf 'Built bin/mesh and four destination executables in dist/.\n'
