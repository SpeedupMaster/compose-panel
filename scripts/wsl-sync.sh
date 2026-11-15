#!/bin/sh
# Dev helper (WSL): copy sources from the Windows checkout into /opt/src and
# rebuild + restart the stack. Usage: sh wsl-sync.sh [windows-checkout-path]
set -eu
SRC=${1:-/mnt/f/代码/compose-panel}
DST=/opt/src/compose-panel
mkdir -p "$DST"
for d in cmd internal deploy scripts web/src web/public; do
  [ -e "$SRC/$d" ] || continue
  rm -rf "$DST/$d"; mkdir -p "$(dirname "$DST/$d")"; cp -r "$SRC/$d" "$DST/$d"
done
for f in go.mod go.sum Dockerfile .dockerignore web/package.json web/package-lock.json web/index.html web/vite.config.ts web/tsconfig.json web/embed.go; do
  [ -e "$SRC/$f" ] && cp "$SRC/$f" "$DST/$f"
done
cd "$DST/deploy"
GOPROXY=${GOPROXY:-https://goproxy.cn,direct} NPM_REGISTRY=${NPM_REGISTRY:-https://registry.npmmirror.com} \
  docker compose up -d --build --force-recreate > /tmp/up.log 2>&1 || { tail -40 /tmp/up.log; exit 1; }
sleep 5
docker ps --format '{{.Names}}\t{{.Status}}' | grep panel
