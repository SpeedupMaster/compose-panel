#!/bin/sh
# Phase-2 end-to-end test. Run right after scripts/e2e.sh on the same stack
# (it reuses the e2e-web project, the whoami.localhost site and the e2e-pg
# instance). Run on the Docker host as root:  sh scripts/e2e2.sh [base-url]
# Needs curl, python3, gzip. Pulls rclone/rclone and redis:7-alpine.
set -eu
BASE=${1:-http://127.0.0.1:7800}
JAR=$(mktemp)
TMP=$(mktemp -d)
PASS=e2e-test-pass-123
step() { printf '\n== %s\n' "$*"; }
die() { echo "FAIL: $*" >&2; exit 1; }
warn() { echo "WARN: $*" >&2; }

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
j() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
# py EXPR ARGS... : print a JSON document built by a python expression (argv in a)
py() { e=$1; shift; python3 -c "import json,sys; a=sys.argv[1:]; print(json.dumps($e))" "$@"; }

wait_job() {
  i=0
  while :; do
    st=$(call GET "/api/jobs/$1" | j 'd.get("status") or d.get("job",{}).get("status")')
    case $st in
      success) echo "job $1 success"; return 0 ;;
      failed|cancelled) curl -s -b "$JAR" -H 'X-Panel: 1' "$BASE/api/jobs/$1/log" | tail -n 30; die "job $1 $st" ;;
    esac
    i=$((i + 1)); [ $i -lt 240 ] || die "job $1 timed out ($st)"
    sleep 2
  done
}
job_log() { curl -s -b "$JAR" -H 'X-Panel: 1' "$BASE/api/jobs/$1/log"; }
site() { curl -sk -o "$TMP/site.out" -w '%{http_code}' --resolve whoami.localhost:443:127.0.0.1 "https://whoami.localhost$1"; }

call POST /api/auth/login "{\"username\":\"admin\",\"password\":\"$PASS\"}" >/dev/null
PID=$(call GET /api/projects | j '[p["id"] for p in d if p["name"]=="e2e-web"][0]')
DBID=$(call GET /api/databases | j '[x["instance"]["id"] for x in d if x["instance"]["name"]=="e2e-pg"][0]')
PGC=$(docker ps -q -f label=com.docker.compose.project=e2e-pg | head -n1)
PGPROJ=$(call GET "/api/databases/$DBID" | j 'd["instance"]["projectId"]')
echo "project=$PID db=$DBID pgcontainer=$PGC"

step "metrics history"
i=0
until [ "$(call GET '/api/metrics/host?range=1h' | j 'len(d["ts"])')" -gt 0 ]; do
  i=$((i + 1)); [ $i -lt 50 ] || die "no host metrics after 100s"; sleep 2
done
call GET '/api/metrics/host?range=24h' | j '"host points=%d cpu=%s disk=%s/%s" % (len(d["ts"]), d["cpu"][-1], d["disk"][-1], d["diskSize"][-1])'
call GET "/api/containers/$PGC/metrics?range=1h" | j '"container %s points=%d mem=%s" % (d["name"], len(d["ts"]), d["mem"][-1] if d["mem"] else None)'
call GET "/api/projects/$PID/metrics?range=7d" | j '"project series=%s" % [s["name"] for s in d]'

step "templates"
call GET /api/templates | j '"builtin: " + ", ".join(t["id"] for t in d if not t["custom"])'
call GET /api/templates | j '[t["vars"] for t in d if t["id"]=="postgres"][0]'
TID=$(call POST "/api/projects/$PID/template" '{"name":"e2e-tpl","description":"from e2e"}' | j 'd["id"]')
echo "custom template $TID"
call GET /api/templates | python3 -c "import json,sys; t=[t for t in json.load(sys.stdin) if t['id']=='nginx'][0]; json.dump({'name':'e2e-nginx','source':'inline','compose':t['compose'],'env':t['env'],'files':t['files'],'deploy':False}, open('$TMP/tpl.json','w'))"
NP=$(call POST /api/projects "$(cat "$TMP/tpl.json")" | j 'd["id"]')
call GET "/api/projects/$NP/files" | j '"files from template: %s" % [f["path"] for f in d]' | tee "$TMP/files.txt"
grep -q 'index.html' "$TMP/files.txt" || die "template files not saved"
R=$(call DELETE "/api/projects/$NP?volumes=1&files=1"); wait_job "$(echo "$R" | j 'd["jobId"]')"
call DELETE "/api/templates/${TID#custom-}" >/dev/null
echo "template removed"

step "image update check"
call POST "/api/projects/$PID/images/check" '{}' | j '["%s %s %s %s" % (c["service"], c["image"], c["status"], c["error"]) for c in d]'
call GET /api/image-updates | j '"pending updates: %d" % len(d)'

step "cron task inside a container"
CT=$(call POST /api/cron-tasks "$(py '{"name":"e2e-cron","projectId":int(a[0]),"service":"db","command":"echo hello-cron; id -un","schedule":"0 3 * * *","timeout":60,"enabled":True,"notify":False}' "$PGPROJ")" | j 'd["id"]')
JOB=$(call POST "/api/cron-tasks/$CT/run" '{}' | j 'd["jobId"]'); wait_job "$JOB"
job_log "$JOB" | grep -q hello-cron || die "cron output missing"
call GET "/api/cron-tasks?project=$PGPROJ" | j '"%s last=%s next=%s" % (d[0]["name"], d[0]["lastStatus"], d[0]["next"])'
call DELETE "/api/cron-tasks/$CT" >/dev/null

step "file browser"
call GET "/api/containers/$PGC/fs?path=/etc" | j '"pg /etc: %d entries via %s, has passwd=%s" % (len(d["entries"]), d["via"], any(e["name"]=="passwd" for e in d["entries"]))'
echo "uploaded-by-e2e $(date +%s)" > "$TMP/up.txt"
curl -sf -b "$JAR" -X PUT -H 'X-Panel: 1' --data-binary @"$TMP/up.txt" "$BASE/api/containers/$PGC/fs/upload?path=/tmp&name=e2e.txt" >/dev/null || die "container upload"
curl -sf -b "$JAR" "$BASE/api/containers/$PGC/fs/download?path=/tmp/e2e.txt" -o "$TMP/down.txt" || die "container download"
cmp "$TMP/up.txt" "$TMP/down.txt" || die "downloaded file differs"
echo "container upload/download round-trip ok"
WC=$(docker ps -q -f label=com.docker.compose.project=e2e-web | head -n1)
call GET "/api/containers/$WC/fs?path=/" | j '"scratch image /: %s via %s" % ([e["name"] for e in d["entries"]], d["via"])'
VOL=$(docker inspect "$PGC" --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{end}}{{end}}')
call GET "/api/docker/volumes/$VOL/fs?path=/" | j '"volume %s: %s" % ("'"$VOL"'", [e["name"] for e in d["entries"]][:8])'
curl -sf -b "$JAR" -X PUT -H 'X-Panel: 1' --data-binary @"$TMP/up.txt" "$BASE/api/docker/volumes/$VOL/fs/upload?path=/&name=e2e.txt" >/dev/null || die "volume upload"
curl -sf -b "$JAR" "$BASE/api/docker/volumes/$VOL/fs/download?path=/e2e.txt" -o "$TMP/vdown.txt" && cmp "$TMP/up.txt" "$TMP/vdown.txt" || die "volume round-trip"
docker exec "$PGC" stat -c '%U %n' /var/lib/postgresql/data/e2e.txt
docker exec "$PGC" rm -f /var/lib/postgresql/data/e2e.txt /tmp/e2e.txt

step "SQL console safety"
call POST "/api/databases/$DBID/query" '{"database":"app","sql":"delete from t","readOnly":false}' | j 'd' | tee "$TMP/q.txt"
grep -q needConfirm "$TMP/q.txt" || die "dangerous statement was not held back"
call POST "/api/databases/$DBID/query" '{"database":"app","sql":"select count(*) from t"}' | j 'd["rows"]' | grep -q 500 || die "delete ran without confirm"
call POST "/api/databases/$DBID/query" '{"database":"app","sql":"select g from generate_series(1,5000) g"}' | j '"rows=%d truncated=%s" % (len(d["rows"]), d["truncated"])' | tee "$TMP/q.txt"
grep -q 'rows=1000 truncated=True' "$TMP/q.txt" || die "row limit not applied"
call POST "/api/databases/$DBID/query" '{"database":"app","sql":"select pg_sleep(40)"}' | j '"timeout -> %s (%sms)" % (d.get("error"), d.get("elapsedMs"))'

step "table browser + slow queries"
call GET "/api/databases/$DBID/tables/t?db=app" | j '"columns=%s indexes=%s" % ([c["name"]+":"+c["type"] for c in d["columns"]], [i["name"] for i in d["indexes"]])'
call GET "/api/databases/$DBID/tables/t/rows?db=app&page=3&size=20" | j '"page 3: %d rows, first id=%s" % (len(d["rows"]), d["rows"][0][0])'
call GET "/api/databases/$DBID/slow-queries" | j '"slow queries: %d hint=%s" % (len(d["items"]), d["hint"][:60])'

step "export + import"
curl -sf -b "$JAR" "$BASE/api/databases/$DBID/export?db=app&format=sql&gzip=1" -o "$TMP/app.sql.gz" || die "export"
gzip -dc "$TMP/app.sql.gz" | grep -c 'CREATE TABLE' | xargs echo "export ok, CREATE TABLE count:"
call POST "/api/databases/$DBID/dbs" '{"name":"app_import"}' >/dev/null
R=$(curl -sf -b "$JAR" -X POST -H 'X-Panel: 1' --data-binary @"$TMP/app.sql.gz" "$BASE/api/databases/$DBID/import?db=app_import&name=app.sql.gz") || die "import upload"
wait_job "$(echo "$R" | j 'd["jobId"]')"
call POST "/api/databases/$DBID/query" '{"database":"app_import","sql":"select count(*) from t"}' | j 'd["rows"]' | grep -q 500 || die "imported row count mismatch"
echo "import ok (500 rows)"

step "restore drill (postgres)"
POL=$(call POST /api/backup-policies "$(py '{"name":"e2e-pg-app","source":{"kind":"database","instanceId":int(a[0]),"database":"app"},"schedule":"0 3 * * *","keepDaily":7,"keepWeekly":0,"keepMonthly":0,"keepYearly":0,"storageId":1,"encrypt":True,"enabled":False}' "$DBID")" | j 'd["id"]')
wait_job "$(call POST "/api/backup-policies/$POL/run" '{}' | j 'd["jobId"]')"
DR=$(call POST /api/restore-drills "$(py '{"policyId":int(a[0]),"schedule":"0 5 * * 0","checkSql":"select count(*) from t","minRows":500,"enabled":True}' "$POL")" | j 'd["id"]')
JOB=$(call POST "/api/restore-drills/$DR/run" '{}' | j 'd["jobId"]'); wait_job "$JOB"
call GET /api/restore-drills | j 'd["runs"][0]["status"] + "\n" + d["runs"][0]["report"]'
call GET /api/backups | j '"verified backups: %s" % [b["id"] for b in (d if isinstance(d,list) else d["items"]) if b.get("verifiedAt")]'
[ -z "$(docker ps -aq -f name=drill)" ] || warn "drill containers left behind: $(docker ps -a -f name=drill --format '{{.Names}}')"
DR2=$(call POST /api/restore-drills "$(py '{"policyId":int(a[0]),"schedule":"0 5 * * 0","checkSql":"select count(*) from t","minRows":999999,"enabled":False}' "$POL")" | j 'd["id"]')
JOB=$(call POST "/api/restore-drills/$DR2/run" '{}' | j 'd["jobId"]')
i=0; while [ "$(call GET "/api/jobs/$JOB" | j 'd.get("status") or d.get("job",{}).get("status")')" != failed ]; do i=$((i+1)); [ $i -lt 120 ] || die "drill with impossible threshold did not fail"; sleep 2; done
echo "drill with minRows=999999 failed as expected"
call DELETE "/api/restore-drills/$DR2" >/dev/null

step "redis backup, drill and restore"
R=$(call POST /api/databases '{"name":"e2e-redis","engine":"redis","version":"7","exposure":"internal"}')
RID=$(echo "$R" | j 'd["id"]'); wait_job "$(echo "$R" | j 'd["jobId"]')"
RC=$(docker ps -q -f label=com.docker.compose.project=e2e-redis | head -n1)
rcli() { docker exec "$RC" sh -c 'redis-cli -a "$REDIS_PASSWORD" --no-auth-warning "$@"' sh "$@"; }
i=0; until rcli ping 2>/dev/null | grep -q PONG; do i=$((i+1)); [ $i -lt 30 ] || die "redis not ready"; sleep 2; done
rcli eval "for i=1,200 do redis.call('set','k'..i,i) end return 1" 0 >/dev/null
rcli set marker before >/dev/null
JOB=$(call POST /api/backups/run "$(py '{"source":{"kind":"database","instanceId":int(a[0]),"database":"all"},"storageId":1,"encrypt":True}' "$RID")" | j 'd["jobId"]'); wait_job "$JOB"
RB=$(call GET /api/backups | j '[b for b in (d if isinstance(d,list) else d["items"]) if b["jobId"]=="'"$JOB"'"][0]["id"]')
RPOL=$(call POST /api/backup-policies "$(py '{"name":"e2e-redis","source":{"kind":"database","instanceId":int(a[0]),"database":"all"},"schedule":"0 3 * * *","keepDaily":3,"keepWeekly":0,"keepMonthly":0,"keepYearly":0,"storageId":1,"encrypt":False,"enabled":False}' "$RID")" | j 'd["id"]')
wait_job "$(call POST "/api/backup-policies/$RPOL/run" '{}' | j 'd["jobId"]')"
RDR=$(call POST /api/restore-drills "$(py '{"policyId":int(a[0]),"schedule":"0 5 * * 0","checkSql":"","minRows":200,"enabled":False}' "$RPOL")" | j 'd["id"]')
wait_job "$(call POST "/api/restore-drills/$RDR/run" '{}' | j 'd["jobId"]')"
call GET /api/restore-drills | j 'd["runs"][0]["report"]'
rcli flushall >/dev/null; rcli set marker after >/dev/null
curl -s -b "$JAR" -X POST -H 'X-Panel: 1' -H 'Content-Type: application/json' -d '{"mode":"overwrite","confirm":"wrong"}' "$BASE/api/backups/$RB/restore" | grep -q 'e2e-redis' || die "redis restore accepted a wrong confirmation"
wait_job "$(call POST "/api/backups/$RB/restore" '{"mode":"overwrite","confirm":"e2e-redis"}' | j 'd["jobId"]')"
i=0; until rcli ping 2>/dev/null | grep -q PONG; do i=$((i+1)); [ $i -lt 30 ] || die "redis not back"; sleep 2; done
RC=$(docker ps -q -f label=com.docker.compose.project=e2e-redis | head -n1)
[ "$(rcli get marker)" = before ] || die "redis marker = $(rcli get marker)"
[ "$(rcli dbsize)" = 201 ] || die "redis dbsize = $(rcli dbsize)"
docker restart "$RC" >/dev/null; sleep 3
i=0; until rcli ping 2>/dev/null | grep -q PONG; do i=$((i+1)); [ $i -lt 30 ] || die "redis not back after restart"; sleep 2; done
[ "$(rcli dbsize)" = 201 ] || die "data lost after restart (AOF not rebuilt?): $(rcli dbsize)"
echo "redis restored: 201 keys, survives restart"

step "SFTP and WebDAV storage"
docker rm -f e2e-sftp e2e-dav >/dev/null 2>&1 || true
docker run -d --name e2e-sftp --network panel-proxy rclone/rclone serve sftp /data --addr :2022 --user foo --pass bar >/dev/null
docker run -d --name e2e-dav --network panel-proxy rclone/rclone serve webdav /data --addr :8080 --user foo --pass bar >/dev/null
sleep 3
for spec in "sftp|{\"host\":\"e2e-sftp\",\"port\":2022,\"username\":\"foo\",\"password\":\"bar\",\"path\":\"backups\"}" "webdav|{\"url\":\"http://e2e-dav:8080/backups/\",\"username\":\"foo\",\"password\":\"bar\"}"; do
  kind=${spec%%|*}; conf=${spec#*|}
  r=$(call POST /api/storage/test "{\"name\":\"t\",\"kind\":\"$kind\",\"config\":$conf}")
  echo "$kind test: $r"; echo "$r" | grep -q '"ok":true' || die "$kind connection test"
  SID=$(call POST /api/storage "{\"name\":\"e2e-$kind\",\"kind\":\"$kind\",\"config\":$conf}" | j 'd["id"]')
  JOB=$(call POST /api/backups/run "$(py '{"source":{"kind":"database","instanceId":int(a[0]),"database":"app"},"storageId":int(a[1]),"encrypt":True}' "$DBID" "$SID")" | j 'd["jobId"]'); wait_job "$JOB"
  B=$(call GET /api/backups | j '[b for b in (d if isinstance(d,list) else d["items"]) if b["jobId"]=="'"$JOB"'"][0]["id"]')
  wait_job "$(call POST "/api/backups/$B/verify" '{}' | j 'd["jobId"]')"
  call GET /api/storage | j '[("%s: %s backups, password=%s" % (s["target"]["name"], s["backups"], s["target"]["config"].get("password"))) for s in d if s["target"]["id"]=='"$SID"'][0]'
  wait_job "$(call POST "/api/backups/$B/restore" '{"mode":"new","target":"app_from_'"$kind"'"}' | j 'd["jobId"]')"
  call POST "/api/databases/$DBID/query" "{\"database\":\"app_from_$kind\",\"sql\":\"select count(*) from t\"}" | j 'd["rows"]' | grep -q 500 || die "restore from $kind"
  echo "$kind backup → verify → restore ok"
done
docker exec e2e-sftp find /data -type f | head -n 3

step "site: access log, maintenance, load balancing"
call GET /api/sites | python3 -c "
import json,sys
s=[x['site'] for x in json.load(sys.stdin)['sites'] if 'whoami.localhost' in x['site']['domains']][0]
json.dump(s, open('$TMP/site.json','w'))"
SITE=$(j 'd["id"]' < "$TMP/site.json")
COMPOSE3=$(py '"services:\n  web:\n    image: traefik/whoami:v1.10\n    deploy:\n      replicas: 2\n"')
call PUT "/api/projects/$PID" "{\"compose\":$COMPOSE3}" >/dev/null
wait_job "$(call POST "/api/projects/$PID/deploy" '{}' | j 'd["jobId"]')"
python3 -c "
import json; s=json.load(open('$TMP/site.json')); s.update(accessLog=True, loadBalance=True, healthPath='/health', maintenanceDeploy=True, maintenanceMsg='e2e 维护中'); s['basicPassword']=''
json.dump(s, open('$TMP/site2.json','w'))"
call PUT "/api/sites/$SITE" "$(cat "$TMP/site2.json")" | j 'd'
sleep 3
for n in 1 2 3 4 5 6 7 8 9 10; do site "/e2e/$n?x=1" >/dev/null; grep '^Hostname:' "$TMP/site.out"; done | sort | uniq -c | tee "$TMP/lb.txt"
[ "$(wc -l < "$TMP/lb.txt")" -ge 2 ] || die "requests were not spread over 2 replicas"
site /missing-path >/dev/null
sleep 2
call GET "/api/sites/$SITE/access-logs?hours=1" | j '"access log: total=%s avg=%sms classes=%s top=%s" % (d["total"], d["avgMs"], d["classes"], d["topPaths"][:2])' | tee "$TMP/al.txt"
grep -q "total=0" "$TMP/al.txt" && die "no access log entries"
call GET "/api/sites/$SITE/access-logs?hours=1&path=/e2e/3" | j '"filtered: %d %s" % (d["total"], [e["uri"] for e in d["entries"]])'

call POST "/api/sites/$SITE/maintenance" '{"on":true,"message":"计划维护 10 分钟"}' | j 'd'
sleep 1
code=$(site /); echo "maintenance on -> $code"; [ "$code" = 503 ] || die "expected 503, got $code"
grep -q '计划维护 10 分钟' "$TMP/site.out" || die "maintenance message missing"
call POST "/api/sites/$SITE/maintenance" '{"on":false}' >/dev/null
sleep 1
code=$(site /); echo "maintenance off -> $code"; [ "$code" = 200 ] || die "expected 200 after maintenance, got $code"

JOB=$(call POST "/api/projects/$PID/deploy" '{}' | j 'd["jobId"]')
seen=; i=0
while [ $i -lt 150 ]; do
  [ "$(site /)" = 503 ] && seen=1
  st=$(call GET "/api/jobs/$JOB" | j 'd.get("status") or d.get("job",{}).get("status")')
  case $st in success|failed|cancelled) break ;; esac
  i=$((i + 1)); sleep 0.3
done
wait_job "$JOB"
[ -n "$seen" ] && echo "deploy maintenance page observed" || warn "deploy finished before a 503 was observed"
sleep 1
code=$(site /); echo "after deploy -> $code"; [ "$code" = 200 ] || die "site not restored after deploy ($code)"

step "notification channels"
call POST /api/notify/channels '{"name":"e2e-bark","kind":"bark","config":{"url":"https://api.day.app/EXAMPLEKEY123456"},"enabled":false}' | j 'd'
call GET /api/notify | j '[("%s %s %s" % (c["name"], c["kind"], c["config"].get("url"))) for c in d["channels"]]'
call GET /api/notify | j '"events: " + ", ".join(e["id"] for e in d["events"])'

step "cleanup"
docker rm -f e2e-sftp e2e-dav >/dev/null
call GET /api/audit/verify | j 'd'
rm -rf "$JAR" "$TMP"
printf '\nALL PHASE-2 E2E STEPS PASSED\n'
