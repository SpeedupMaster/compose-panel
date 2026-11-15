#!/bin/sh
# Smoke test against a running panel: first-run setup, login, then every
# read-only GET endpoint. Usage: smoke.sh <base-url> <setup-token>
# Needs curl. Prints one line per request; exits 1 if any returns 5xx.
set -u
BASE=${1:-http://127.0.0.1:7800}
TOKEN=${2:-}
JAR=$(mktemp)
FAIL=0

req() { # method path [json]
  if [ -n "${3:-}" ]; then
    code=$(curl -s -o /tmp/smoke.body -w '%{http_code}' -b "$JAR" -c "$JAR" -X "$1" -H 'X-Panel: 1' -H 'Content-Type: application/json' -d "$3" "$BASE$2")
  else
    code=$(curl -s -o /tmp/smoke.body -w '%{http_code}' -b "$JAR" -c "$JAR" -X "$1" -H 'X-Panel: 1' "$BASE$2")
  fi
  printf '%s %-6s %-40s %s\n' "$code" "$1" "$2" "$(head -c 160 /tmp/smoke.body | tr '\n' ' ')"
  case $code in 5*|000) FAIL=1 ;; esac
}

req GET /api/setup/status
if [ -n "$TOKEN" ]; then
  req POST /api/setup "{\"token\":\"$TOKEN\",\"username\":\"admin\",\"email\":\"admin@example.com\",\"password\":\"smoke-test-pass-123\"}"
fi
req POST /api/auth/login '{"username":"admin","password":"smoke-test-pass-123"}'
req GET /api/auth/me

for p in /api/overview /api/projects /api/templates /api/containers /api/containers/stats \
  /api/docker/df /api/docker/images /api/docker/networks /api/docker/volumes /api/registries \
  /api/databases /api/db-engines /api/backups /api/backup-policies /api/backup-sources /api/backup-key \
  /api/storage /api/sites /api/certificates /api/proxy/config /api/jobs /api/schedules \
  /api/notify /api/users /api/tokens /api/audit /api/audit/verify /api/me/sessions \
  /api/settings /api/system/info /api/import/scan; do
  req GET "$p"
done

req POST /api/compose/validate '{"compose":"services:\n  web:\n    image: nginx:alpine\n"}'
req POST /api/storage '{"name":"local-test","kind":"local","config":{"path":"/tmp/smoke-backups"}}'
req POST /api/projects '{"name":"smoke-web","source":"inline","compose":"services:\n  web:\n    image: nginx:alpine\n","deploy":false}'
req GET /api/projects

rm -f "$JAR"
exit $FAIL
