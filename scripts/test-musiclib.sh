#!/usr/bin/env bash
# Deterministic host lifecycle tests; Docker is never contacted.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
export FIXTURE="$work/installation with spaces" CASE_DIR="$work/case"
mkdir -- "$FIXTURE" "$CASE_DIR" "$work/bin"
cp -- "$root/musiclib" "$FIXTURE/musiclib"
export PATH="$work/bin:$PATH"
export DOCKER_CONTEXT='' DOCKER_HOST=unix:///fake/docker.sock

cat > "$work/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
[[ $PWD -ef $FIXTURE && $1 == --host && $2 == unix:///fake/docker.sock ]]
shift 2
cd -- "$CASE_DIR"
printf '%s\n' "$1 $2" >> calls
printf -v a '%064d' 1; printf -v p '%064d' 2; printf -v o '%064d' 3
printf -v ai 'sha256:%064d' 4; printf -v pi 'sha256:%064d' 5
printf -v ah '%064d' 6; printf -v ph '%064d' 7
case "$1 $2" in
  'info --format') printf 'Fake Docker\r\n' ;;
  'compose --project-directory')
    [[ $3 -ef $FIXTURE ]]; shift 3
    case "$1 $2" in
      'config --images') [[ $3 == postgres ]]; printf 'fixture/postgres\r\n' ;;
      'config --format') [[ $3 == json && $4 == app && $5 == postgres ]]; printf '{ "fixture":true }\n' ;;
      'config --hash') [[ $3 == '*' ]]; printf 'app %s\r\npostgres %s\r\n' "$ah" "$ph" ;;
      'up --no-start')
        [[ $# == 8 && $3 == --no-deps && $4 == --no-recreate && $5 == --no-build && $6 == --pull && $7 == never ]]
        printf 'create %s\n' "$8" >> trace
        printf 'created|' > "$8"
        ;;
      'run -d')
        [[ $3 == -T && $4 == --interactive=false && $5 == --no-deps && $6 == --pull && $7 == never && $8 == --name && $9 == fixture-musiclib-offline && ${10} == app ]]
        printf 'offline %s\n' "${11}" >> trace
        shift 10
        printf '%s\n' "$@" > offline-args
        [[ ! -f cli-run-fails ]] || exit 1
        printf '%s\r\n' "$o"
        ;;
      *) exit 99 ;;
    esac
    ;;
  'run --rm')
    [[ $* == *'--network none --read-only --cap-drop ALL --tmpfs /var/lib/postgresql/data'* ]]
    IFS= read -r model
    [[ $model == '{ "fixture":true }' ]]
    data=fixture_data
    [[ ! -f volume-drift ]] || data=fixture_other_data
    printf 'project|fixture|fixture_default|bridge-hash\r\nimage|app|fixture/app\r\nimage|postgres|fixture/postgres\r\n'
    printf 'volume|db|fixture_db|volume-hash\nvolume|data|%s|volume-hash\nvolume|backup|fixture_backup|volume-hash\n' "$data"
    printf 'app|volume|%s|/data|true\napp|volume|fixture_backup|/backup|true\napp|bind|%s/import|/import|false\n' "$data" "$FIXTURE"
    printf 'postgres|volume|fixture_db|/var/lib/postgresql/data|true\n'
    ;;
  'image inspect')
    case "${*: -1}" in
      fixture/app) if [[ -f retagged ]]; then printf 'sha256:%064d\n' 8; else printf '%s\n' "$ai"; fi ;;
      fixture/postgres) printf '%s\n' "$pi" ;;
      *) exit 99 ;;
    esac
    ;;
  'volume ls') printf '%s\n' fixture_data fixture_backup fixture_db ;;
  'volume inspect')
    v=${*: -1}
    hash=volume-hash options=0 project=fixture
    [[ ! -f legacy-volumes ]] || hash=''
    [[ ! -f wrong-volume-hash ]] || hash=mismatch
    [[ ! -f volume-options ]] || options=1
    [[ ! -f wrong-volume-owner ]] || project=other
    printf 'local|local|%s|%s|%s|%s\n' "$options" "$project" "${v#fixture_}" "$hash"
    ;;
  'network ls') printf '%s\n' fixture_default ;;
  'network inspect')
    hash=bridge-hash options='{"com.docker.network.enable_ipv4":"true","com.docker.network.enable_ipv6":"false"}' project=fixture
    if [[ -f legacy-network ]]; then
      hash='' options='{"com.docker.network.enable_ipv4":"true"}'
    fi
    [[ ! -f wrong-network-hash ]] || hash=mismatch
    [[ ! -f network-options ]] || options='{"com.docker.network.bridge.enable_icc":"false"}'
    [[ ! -f wrong-network-owner ]] || project=other
    printf 'bridge|local|false|false|false|false|false||default|0|1|""|0|%s|%s|default|%s\n' "$options" "$project" "$hash"
    ;;
  'container ls')
    case "${*: -1}" in
      *=app) if [[ -f app ]]; then printf '%s\r\n' "$a"; fi ;;
      *=postgres) if [[ -f postgres ]]; then printf '%s\r\n' "$p"; fi ;;
      *) exit 99 ;;
    esac
    ;;
  'container inspect')
    case "$5" in
      "$a") service=app; hash=$ah; image=$ai ;;
      "$p") service=postgres; hash=$ph; image=$pi ;;
      "$o") service=offline ;;
      *) exit 99 ;;
    esac
    case "$4" in
      *working_dir*)
        if [[ -f "foreign-$service" ]]; then printf '%s\r\n' "$CASE_DIR"; else printf '%s\r\n' "$FIXTURE"; fi
        ;;
      *config-hash*)
        [[ ! -f config-drift ]] || hash=$ph
        printf 'False|%s|%s|fixture_default|\r\n' "$hash" "$image"
        ;;
      *.Mounts*)
        if [[ $service == app ]]; then
          printf 'volume|fixture_backup|/backup|true\nvolume|fixture_data|/data|true\nbind|%s/import|/import|false\n' "$FIXTURE"
        else
          printf 'volume|fixture_db|/var/lib/postgresql/data|true\n'
        fi
        ;;
      *ExitCode*)
        [[ $service == offline ]]
        if [[ -f offline-state ]]; then
          printf '%s\n' "$(< offline-state)"
        else
          printf 'exited|%s|false||2026-01-01T00:00:00Z|2026-01-01T00:00:01Z\n' "$(< status)"
        fi
        ;;
      *.State.Status*) printf '%s\n' "$(< "$service")" ;;
      *) exit 99 ;;
    esac
    ;;
  'container stop')
    [[ $3 == "$a" ]]; printf 'stop app\n' >> trace
    [[ ! -f stop-fails ]] || exit 1
    printf 'exited|' > app
    ;;
  'container wait')
    [[ $3 == "$o" ]]; printf 'wait offline\n' >> trace
    while [[ -f block-wait ]]; do sleep 0.02; done
    [[ ! -f cli-wait-fails ]] || exit 1
    printf '%s\n' "$(< status)"
    ;;
  'container logs')
    [[ $3 == "$o" ]]
    printf 'offline diagnostics\n' >&2
    [[ ! -f report ]] || printf '%s\n' "$(< report)"
    [[ ! -f stderr-report ]] || printf '%s\n' "$(< stderr-report)" >&2
    [[ ! -f cli-logs-fails ]] || exit 1
    ;;
  'container rm') [[ $# == 3 && $3 == "$o" ]]; printf 'remove offline\n' >> trace ;;
  'container start')
    case "$3" in
      "$a")
        printf 'start app\n' >> trace
        if [[ -f startup-fails ]]; then printf 'running|unhealthy' > app; else printf 'running|healthy' > app; fi
        ;;
      "$p") printf 'start postgres\n' >> trace; printf 'running|healthy' > postgres ;;
      *) exit 99 ;;
    esac
    ;;
  *) exit 99 ;;
esac
FAKE
chmod +x "$work/bin/docker"

lock="$FIXTURE/.musiclib-command.lock"
fail() { printf 'FAIL: %s\n' "$*" >&2; [[ ! -f $CASE_DIR/output ]] || tail -n 30 "$CASE_DIR/output" >&2; exit 1; }
reset() {
  rm -rf -- "$CASE_DIR" "$lock"
  mkdir -- "$CASE_DIR"
  printf 'running|healthy' > "$CASE_DIR/app"
  printf 'running|healthy' > "$CASE_DIR/postgres"
  printf '0\n' > "$CASE_DIR/status"
  : > "$CASE_DIR/trace"
}
run() {
  local expected=$1 rc=0
  shift
  "$FIXTURE/musiclib" "$@" > "$CASE_DIR/output" 2>&1 || rc=$?
  [[ $rc == "$expected" ]] || fail "$*: got $rc, expected $expected"
}
trace() { [[ $(< "$CASE_DIR/trace") == "$*" ]] || fail "unexpected lifecycle: $(< "$CASE_DIR/trace")"; }
clean() { [[ ! -e $lock ]] || fail 'known outcome retained lock'; }
retained() { [[ -f $lock/owner ]] || fail 'uncertain outcome released lock'; }
said() { grep -Fq -- "$1" "$CASE_DIR/output" || fail "expected diagnostic: $1"; }

reset
run 0 --help
for name in '' . .. ../elsewhere 'a/b' 'a\b' .musiclib-backup-leftover.tmp; do run 2 backup "$name"; done
run 2 restore
run 2 doctor --bad
run 2 rebuild --store-id AAAAAAAA-0000-0000-0000-000000000000
[[ ! -e $CASE_DIR/calls ]] || fail 'invalid arguments contacted Docker'
clean
for drift in foreign-app foreign-postgres config-drift volume-drift retagged \
  wrong-volume-hash wrong-network-hash volume-options network-options wrong-volume-owner wrong-network-owner; do
  reset
  # Test actual metadata and ownership guards with no hash to rely on.
  case "$drift" in
    volume-options|wrong-volume-owner) touch "$CASE_DIR/legacy-volumes" ;;
    network-options|wrong-network-owner) touch "$CASE_DIR/legacy-network" ;;
    foreign-*) printf 'exited|' > "$CASE_DIR/app" ;;
  esac
  touch "$CASE_DIR/$drift"
  run 3 backup archive
  trace ''; clean
  case "$drift" in
    foreign-*) said 'different or unidentifiable installation' ;;
    volume-drift) said 'mount at /data differs' ;;
    *volume-hash|volume-options|*volume-owner) said 'does not match the configured project volume' ;;
    *network-hash|network-options|*network-owner) said 'network does not match the configured project bridge' ;;
    *) said 'configuration, image or network differs' ;;
  esac
done
reset
DOCKER_HOST=ssh://fake run 3 backup archive
said 'requires a local Docker'; trace ''; clean

reset
run 0 backup 'quoted archive'
trace $'stop app\noffline backup\nwait offline\nremove offline\nstart app'; clean
[[ $(< "$CASE_DIR/offline-args") == $'backup\n--to\n/backup/quoted archive' ]] || fail 'backup argument was not preserved'
said 'MusicLib is ready'
for legacy in legacy-volumes legacy-network both; do
  reset
  [[ $legacy == legacy-network ]] || touch "$CASE_DIR/legacy-volumes"
  [[ $legacy == legacy-volumes ]] || touch "$CASE_DIR/legacy-network"
  run 0 backup archive
  trace $'stop app\noffline backup\nwait offline\nremove offline\nstart app'; clean
done
reset
printf 'exited|' > "$CASE_DIR/app"
run 0 backup
trace $'offline backup\nwait offline\nremove offline'; clean
grep -Eq '^/backup/[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{6}Z$' "$CASE_DIR/offline-args" || fail 'no UTC default name'
said 'left stopped'
reset
printf 'running|unhealthy' > "$CASE_DIR/postgres"
run 3 backup archive
trace ''; clean
reset
touch "$CASE_DIR/stop-fails"
run 3 backup archive
trace 'stop app'; retained

for operation in backup rebuild; do
  reset
  printf '1\n' > "$CASE_DIR/status"
  case "$operation" in
    backup) run 1 backup archive ;;
    rebuild) run 1 rebuild --store-id 11111111-0000-0000-0000-000000000000 ;;
  esac
  trace "stop app"$'\n'"offline $operation"$'\nwait offline\nremove offline'; clean; said 'offline diagnostics'
done
summary='Doctor found damaged or inconsistent data. Nothing was changed.'
reset
printf '1\n' > "$CASE_DIR/status"
printf 'error doctor_output_missing fixture: Output is missing.\n%s\r\n' "$summary" > "$CASE_DIR/report"
run 1 doctor --deep
trace $'stop app\noffline doctor\nwait offline\nremove offline\nstart app'; clean
said 'doctor_output_missing'; said "$summary"; said 'offline diagnostics'

# A signal to the one-off alone can be handled by Go as exit 1. Docker
# wait/inspect/logs still succeed, unlike a signal to this wrapper (below).
for report in cancelled substring not-final stderr transport-failure; do
  reset
  printf '1\n' > "$CASE_DIR/status"
  case "$report" in
    cancelled) printf 'doctor_failed: context canceled\n' > "$CASE_DIR/stderr-report" ;;
    substring) printf 'warning fixture: %s\n%s extra\n' "$summary" "$summary" > "$CASE_DIR/report" ;;
    not-final) printf '%s\nmore stdout\n' "$summary" > "$CASE_DIR/report" ;;
    stderr) printf '%s\n' "$summary" > "$CASE_DIR/stderr-report" ;;
    transport-failure) printf '%s\n' "$summary" > "$CASE_DIR/report"; touch "$CASE_DIR/cli-logs-fails" ;;
  esac
  run 1 doctor --deep
  trace $'stop app\noffline doctor\nwait offline'; retained
  said 'cannot confirm a completed doctor report'; said 'offline diagnostics'
  if [[ $report == cancelled ]]; then said 'doctor_failed: context canceled'; else said "$summary"; fi
done
reset
run 3 restore archive
trace ''; clean; said 'refuses a running target app'
for result in 0 1 2 unhealthy; do
  reset
  rm -- "$CASE_DIR/app" "$CASE_DIR/postgres"
  status=$result
  if [[ $result == unhealthy ]]; then status=0; touch "$CASE_DIR/startup-fails"; fi
  printf '%s\n' "$status" > "$CASE_DIR/status"
  expected=$status
  [[ $result != unhealthy ]] || expected=3
  run "$expected" restore 'quoted archive'
  steps=$'create postgres\nstart postgres\noffline restore\nwait offline\nremove offline'
  if [[ $status == 0 ]]; then steps+=$'\ncreate app\nstart app'; said 'catalog and originals are restored'; fi
  [[ $result != unhealthy ]] || said 'MusicLib is not ready'
  [[ $(< "$CASE_DIR/offline-args") == $'restore\n--from\n/backup/quoted archive' ]] || fail 'restore argument was not preserved'
  trace "$steps"; clean
done

for fault in cli-run-fails cli-wait-fails offline-state; do
  reset
  printf '1\n' > "$CASE_DIR/status"
  printf 'exited|1|false|start error|0001-01-01|0001-01-01\n' > "$CASE_DIR/$fault"
  run 3 doctor
  steps=$'stop app\noffline doctor'
  [[ $fault == cli-run-fails ]] || steps+=$'\nwait offline'
  trace "$steps"; retained
done
reset
printf 'exited|137|true||2026-01-01T00:00:00Z|2026-01-01T00:00:01Z\n' > "$CASE_DIR/offline-state"
run 137 doctor
trace $'stop app\noffline doctor\nwait offline'; retained

reset
touch "$CASE_DIR/block-wait"
"$FIXTURE/musiclib" doctor > "$CASE_DIR/first" 2>&1 &
pid=$!
for ((i=0; i<250; i++)); do
  if grep -Fqx 'wait offline' "$CASE_DIR/trace"; then break; fi
  sleep 0.02
done
if ! grep -Fqx 'wait offline' "$CASE_DIR/trace"; then
  rm -- "$CASE_DIR/block-wait"
  wait "$pid" || true
  tail -n 20 "$CASE_DIR/first" >&2
  fail 'first command did not reach the wait barrier'
fi
run 3 backup competing
said 'cannot acquire'; retained
kill -TERM "$pid"
status=0; wait "$pid" || status=$?
[[ $status == 143 ]] || fail "signal returned $status (expected 143)"
trace $'stop app\noffline doctor\nwait offline'; retained
run 3 backup stale
said 'cannot acquire'; retained
trace $'stop app\noffline doctor\nwait offline'
printf '%s\n' 'musiclib fake-Docker lifecycle checks passed'
