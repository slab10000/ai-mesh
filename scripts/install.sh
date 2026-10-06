#!/bin/sh
set -eu
mesh_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$mesh_root"
sh scripts/build.sh
mesh_install_dir="${MESH_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$mesh_install_dir"
cp bin/mesh "$mesh_install_dir/mesh"
chmod 700 "$mesh_install_dir/mesh"
for mesh_binary in dist/mesh-*; do
  cp "$mesh_binary" "$mesh_install_dir/"
  chmod 700 "$mesh_install_dir/$(basename "$mesh_binary")"
done
printf 'Installed in %s. Ensure this directory is on your shell PATH.\n' "$mesh_install_dir"
if [ "${1:-}" != '--no-setup' ]; then
  exec "$mesh_install_dir/mesh" setup
fi
