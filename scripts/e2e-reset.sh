#!/bin/sh
# Wipe a TEST host back to a fresh panel: stops every compose project whose
# directory lives under ${PANEL_ROOT:-/opt/compose-panel} (apps and database
# instances), removes their volumes, the panel stack and its data.
# Never run this on a server with real data.
set -u
ROOT=${PANEL_ROOT:-/opt/compose-panel}
docker ps -a --format '{{.Label "com.docker.compose.project"}} {{.Label "com.docker.compose.project.working_dir"}}' | sort -u |
while read -r p dir; do
  case $dir in "$ROOT"/*) docker compose -p "$p" down -v --remove-orphans ;; esac
done
cd "$(dirname "$0")/../deploy"
docker compose down -v --remove-orphans
rm -rf "$ROOT"
docker network rm panel-db 2>/dev/null
docker compose up -d
sleep 5
docker ps --format '{{.Names}}\t{{.Status}}'
