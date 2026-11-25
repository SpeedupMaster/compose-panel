#!/bin/sh
# Wipe a TEST host back to a fresh panel: stops every compose project whose
# directory lives under ${PANEL_ROOT:-/opt/compose-panel} (apps and database
# instances), removes their volumes, the panel stack and its data.
# Never run this on a server with real data.
set -eu
ROOT=${PANEL_ROOT:-/opt/compose-panel}
if [ -z "$ROOT" ] || [ "$ROOT" = "/" ]; then
  echo "ERROR: refusing unsafe PANEL_ROOT='$ROOT'" >&2
  exit 1
fi
docker ps -a --format '{{.Label "com.docker.compose.project"}} {{.Label "com.docker.compose.project.working_dir"}}' | sort -u |
while read -r p dir; do
  case $dir in "$ROOT"/*) docker compose -p "$p" down -v --remove-orphans ;; esac
done
cd "$(dirname "$0")/../deploy"
docker compose down -v --remove-orphans
rm -rf "$ROOT"
docker network rm panel-db 2>/dev/null || true
docker compose up -d --build
i=0
until curl -fsS http://127.0.0.1:7800/api/setup/status >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "ERROR: panel did not become ready at http://127.0.0.1:7800/api/setup/status" >&2
    docker compose logs --tail=50 panel >&2 || true
    exit 1
  fi
  sleep 1
done
docker ps --format '{{.Names}}\t{{.Status}}'
