#!/bin/sh
set -eu

# Build the narrowly patched Guacamole 1.6.0 daemon. The source and FreeRDP
# version are pinned so the bridge/identity parameter cannot silently drift.
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
git clone -q https://github.com/apache/guacamole-server.git "$WORK/guacamole-server"
cd "$WORK/guacamole-server"
git checkout -q 1f664e08feae6e7d15d8146b78acab2e6fb470ae
git apply "$ROOT/guacd/server-name.patch"
docker build --build-arg FREERDP_VERSION=3 --build-arg WITH_FREERDP=3.17.2 \
  --build-arg WITH_LIBWEBSOCKETS=NO \
  --build-arg BUILD_JOBS=4 -t "${JUMP_GUACD_IMAGE:-jump-guacd:local}" .
