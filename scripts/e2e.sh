#!/bin/sh
# End-to-end test against a live stack started from deploy/docker-compose.yml
# on a FRESH data dir. Run on the Docker host as root:
#   sh scripts/e2e.sh [base-url]
# Walks: setup → deploy a project → bind a *.localhost site through Caddy →
# create PostgreSQL → write rows → encrypted backup → verify → restore into a
# new database → compare → redeploy → rollback.
# Needs curl and python3. Exits non-zero on the first failure.
set -eu
BASE=${1:-http://127.0.0.1:7800}
JAR=$(mktemp)
PASS=e2e-test-pass-123
step() { printf '\n== %s\n' "$*"; }
die() { echo "FAIL: $*" >&2; exit 1; }

# call METHOD PATH [JSON] -> body on stdout; fails on HTTP >= 400
call() {
  if [ -n "${3:-}" ]; then
    out=$(curl -s -w '\n%{http_code}' -b "$JAR" -c "$JAR" -X "$1" -H 'X-Panel: 1' -H 'Content-Type: application/json' -d "$3" "$BASE$2")
  else
    out=$(curl -s -w '\n%{http_code}' -b "$JAR" -c "$JAR" -X "$1" -H 'X-Panel: 1' "$BASE$2")
  fi
  code=$(printf '%s' "$out" | tail -n1)
  body=$(printf '%s' "$out" | sed '$d')
  [ "$code" -lt 400 ] || die "$1 $2 -> $code $body"
  printf '%s' "$body"
}
# j EXPR <<< json : evaluate a python expression on the parsed value `d`
j() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

# wait_job ID : poll until the job ends; print its log on failure
wait_job() {
  i=0
  while :; do
    st=$(call GET "/api/jobs/$1" | j 'd.get("status") or d.get("job",{}).get("status")')
    case $st in
      success) echo "job $1 success"; return 0 ;;
      failed|cancelled) curl -s -b "$JAR" -H 'X-Panel: 1' "$BASE/api/jobs/$1/log" | tail -n 30; die "job $1 $st" ;;
    esac
    i=$((i + 1)); [ $i -lt 180 ] || die "job $1 timed out ($st)"
    sleep 2
  done
}

step "setup + login"
if [ "$(call GET /api/setup/status | j 'd["needed"]')" = "True" ]; then
  TOKEN=$(docker logs panel 2>&1 | sed -n 's/.*初始化令牌：\([A-Za-z0-9_-]*\).*/\1/p' | tail -n1)
  [ -n "$TOKEN" ] || die "no setup token in panel logs"
  call POST /api/setup "{\"token\":\"$TOKEN\",\"username\":\"admin\",\"email\":\"admin@example.com\",\"password\":\"$PASS\"}" >/dev/null
fi
call POST /api/auth/login "{\"username\":\"admin\",\"password\":\"$PASS\"}" | j 'd["user"]["username"]'

step "deploy project"
COMPOSE='services:\n  web:\n    image: traefik/whoami:v1.10\n'
PID=$(call POST /api/projects "{\"name\":\"e2e-web\",\"source\":\"inline\",\"compose\":\"$COMPOSE\",\"deploy\":true}" | j 'd["id"]')
echo "project $PID"
JOB=$(call GET "/api/projects/$PID/deployments" | j '(d if isinstance(d,list) else d["items"])[0]["jobId"]')
wait_job "$JOB"
call GET "/api/projects/$PID" | j '"status=%s running=%s" % (d.get("status") or d["project"]["status"], d.get("running", d.get("project",{}).get("running")))'

step "site whoami.localhost via Caddy"
call POST /api/sites "{\"domains\":[\"whoami.localhost\"],\"kind\":\"proxy\",\"projectId\":$PID,\"service\":\"web\",\"port\":80,\"compress\":true,\"enabled\":true}" >/dev/null
ok=
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -sk --resolve whoami.localhost:443:127.0.0.1 https://whoami.localhost/ | grep -q '^Hostname:'; then ok=1; break; fi
  sleep 2
done
[ -n "$ok" ] || die "https://whoami.localhost not served by Caddy"
curl -sk --resolve whoami.localhost:443:127.0.0.1 https://whoami.localhost/ | head -n 3

step "create PostgreSQL"
R=$(call POST /api/databases '{"name":"e2e-pg","engine":"postgres","version":"16","exposure":"internal","initDb":"app","initUser":"app"}')
DBID=$(echo "$R" | j 'd["id"]'); wait_job "$(echo "$R" | j 'd["jobId"]')"
for i in 1 2 3 4 5 6 7 8 9 10; do
  if call POST "/api/databases/$DBID/query" '{"database":"app","sql":"select 1"}' >/dev/null 2>&1; then break; fi; sleep 3
done
call POST "/api/databases/$DBID/query" '{"database":"app","sql":"create table t(id int primary key, v text); insert into t select g, md5(g::text) from generate_series(1,500) g;"}' >/dev/null
call POST "/api/databases/$DBID/query" '{"database":"app","sql":"select count(*) as n from t"}' | j 'd'

step "backup (zstd + age) and verify"
JOB=$(call POST /api/backups/run "{\"source\":{\"kind\":\"database\",\"instanceId\":$DBID,\"database\":\"app\"},\"storageId\":1,\"encrypt\":true}" | j 'd["jobId"]')
wait_job "$JOB"
BID=$(call GET /api/backups | j '[b for b in (d if isinstance(d,list) else d["items"]) if b["jobId"]=="'"$JOB"'"][0]["id"]')
call GET /api/backups | j '[("id=%s size=%s enc=%s status=%s" % (b["id"],b["size"],b["encrypted"],b["status"])) for b in (d if isinstance(d,list) else d["items"]) if b["id"]=='"$BID"'][0]'
wait_job "$(call POST "/api/backups/$BID/verify" '{}' | j 'd["jobId"]')"

step "restore into new database"
wait_job "$(call POST "/api/backups/$BID/restore" '{"mode":"new","target":"app_restored"}' | j 'd["jobId"]')"
N=$(call POST "/api/databases/$DBID/query" '{"database":"app_restored","sql":"select count(*) as n from t"}' | j 'json.dumps(d)')
echo "$N"; echo "$N" | grep -q '500' || die "restored row count mismatch"

step "redeploy with change, then rollback"
COMPOSE2='services:\n  web:\n    image: traefik/whoami:v1.10\n    environment:\n      WHOAMI_NAME: v2\n'
call PUT "/api/projects/$PID" "{\"compose\":\"$COMPOSE2\"}" >/dev/null
R=$(call POST "/api/projects/$PID/deploy" '{}'); wait_job "$(echo "$R" | j 'd["jobId"]')"
FIRST=$(call GET "/api/projects/$PID/deployments" | j '(d if isinstance(d,list) else d["items"])[-1]["id"]')
wait_job "$(call POST "/api/deployments/$FIRST/rollback" '{}' | j 'd["jobId"]')"

step "containers, logs, audit"
CID=$(call GET /api/containers | j '[c for c in (d if isinstance(d,list) else d["items"]) if c.get("project")=="e2e-web"][0]["id"]')
call GET "/api/containers/$CID" >/dev/null && echo "container $CID ok"
call GET /api/audit/verify | j 'd'
call GET /api/overview | j '"containers=%s" % d["containers"]'

rm -f "$JAR"
printf '\nALL E2E STEPS PASSED\n'
