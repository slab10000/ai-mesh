#!/bin/sh
set -eu
mesh_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$mesh_root"
sh scripts/build.sh
mesh_install_dir="${MESH_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$mesh_install_dir"
mesh_stage=$(mktemp "$mesh_install_dir/.mesh-install.XXXXXX")
trap 'rm -f "$mesh_stage"' EXIT HUP INT TERM
cp bin/mesh "$mesh_stage"
chmod 700 "$mesh_stage"
mv "$mesh_stage" "$mesh_install_dir/mesh"
for mesh_binary in dist/mesh-*; do
  mesh_stage=$(mktemp "$mesh_install_dir/.mesh-install.XXXXXX")
  cp "$mesh_binary" "$mesh_stage"
  chmod 700 "$mesh_stage"
  mv "$mesh_stage" "$mesh_install_dir/$(basename "$mesh_binary")"
done
printf 'Installed in %s. Ensure this directory is on your shell PATH.\n' "$mesh_install_dir"
if [ "${1:-}" != '--no-setup' ]; then
  exec "$mesh_install_dir/mesh" setup
fi
