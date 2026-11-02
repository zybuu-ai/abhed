#!/usr/bin/env bash
# The restore test: proves that a backup made with abhed-backup.sh, restored
# with abhed-restore.sh or from a physical base backup, keeps the record
# exactly (every chain position, hash and commit_tx), still verifies with
# `abhed record verify` and `abhed record audit`, and lets a reader of the
# record in commit order go on from its cursor without skipping or repeating
# an event. Run from the repository root:
#
#   scripts/backup/restore-test.sh            # podman; ENGINE=docker for docker
#
# It starts throwaway Postgres containers named restoretest-<pid>-…, on ports
# the engine picks on 127.0.0.1, and removes them, with their volumes, when it
# ends. It touches no other container. Needs go and a container engine.
#
#   ENGINE   podman (default) or docker
#   IMAGE    the Postgres image (default docker.io/library/postgres:17-alpine)
#   BURN     transactions spent on the source first, so a fresh cluster's
#            counter is behind it, as after a restore elsewhere (20000)
#   KEEP=1   leave the containers and the work folder for inspection
set -euo pipefail

ENGINE="${ENGINE:-podman}"
IMAGE="${IMAGE:-docker.io/library/postgres:17-alpine}"
BURN="${BURN:-20000}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
P="restoretest-$$"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/abhed-restoretest.XXXXXX")"
made=()

cleanup() {
  if [ "${KEEP:-}" = 1 ]; then
    echo "kept: ${made[*]:-} and $WORK"
    return
  fi
  for c in "${made[@]:-}"; do
    [ -n "$c" ] && "$ENGINE" rm -f -v "$c" > /dev/null 2>&1 || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n== %s\n' "$*"; }

# start NAME [create-only]: a Postgres container of this run.
start() {
  local c="$P-$1"
  "$ENGINE" run -d --name "$c" -e POSTGRES_USER=pgsuper -e POSTGRES_PASSWORD=pgsuper \
    -e POSTGRES_DB=postgres -p 127.0.0.1::5432 "$IMAGE" > /dev/null
  made+=("$c")
  ready "$c"
}

# ready waits for the server proper, not the entrypoint's first start.
ready() {
  local c="$1" i
  for i in $(seq 1 120); do
    if "$ENGINE" exec "$c" pg_isready -q -U pgsuper -h 127.0.0.1 2>/dev/null &&
       "$ENGINE" exec "$c" psql -q -At -U pgsuper -h 127.0.0.1 -d postgres -c 'SELECT 1' > /dev/null 2>&1; then
      sleep 1
      "$ENGINE" exec "$c" psql -q -At -U pgsuper -h 127.0.0.1 -d postgres -c 'SELECT 1' > /dev/null 2>&1 && return 0
    fi
    sleep 1
  done
  echo "$c did not become ready" >&2
  "$ENGINE" logs "$c" >&2
  return 1
}

port() { "$ENGINE" port "$P-$1" 5432/tcp | head -1 | sed 's/.*://'; }
sql() { local c="$P-$1" db="$2"; shift 2; "$ENGINE" exec "$c" psql -q -v ON_ERROR_STOP=1 -U pgsuper -d "$db" "$@"; }

roles() {
  sql "$1" postgres \
    -c "CREATE ROLE abhed_owner LOGIN PASSWORD 'owner' NOSUPERUSER NOBYPASSRLS" \
    -c "CREATE ROLE abhed_runtime LOGIN PASSWORD 'runtime' NOSUPERUSER NOBYPASSRLS" \
    -c "CREATE ROLE abhed_backup LOGIN PASSWORD 'backup' NOSUPERUSER BYPASSRLS" \
    -c "GRANT pg_read_all_data TO abhed_backup"
}

newdb() { sql "$1" postgres -c "CREATE DATABASE $2"; sql "$1" "$2" -c "GRANT ALL ON SCHEMA public TO abhed_owner"; }

# dsn NAME DB USER:PASS. Never falls back to a default port.
dsn() {
  local p
  p="$(port "$1")"
  [ -n "$p" ] || { echo "no port for $P-$1" >&2; exit 1; }
  echo "postgres://$3@127.0.0.1:$p/$2?sslmode=disable"
}

BIN="$WORK/abhed"
say "build abhed"
(cd "$ROOT" && go build -o "$BIN" ./cmd/abhed)
mkdir -p "$WORK/home/.abhed"
chmod 700 "$WORK/home/.abhed"
printf '{"storage":{"driver":"postgres","tenant":"restore"}}\n' > "$WORK/home/.abhed/config.json"
chmod 600 "$WORK/home/.abhed/config.json"

# check NAME DB: migrate, the two record checks, and the fingerprint test.
results=()
check() {
  local name="$1" db="$2" owner runtime
  owner="$(dsn "$name" "$db" abhed_owner:owner)"
  runtime="$(dsn "$name" "$db" abhed_runtime:runtime)"
  HOME="$WORK/home" ABHED_MIGRATE_DATABASE_URL="$owner" ABHED_DATABASE_URL="$runtime" "$BIN" migrate > "$WORK/$name-migrate.log" 2>&1
  HOME="$WORK/home" ABHED_DATABASE_URL="$runtime" "$BIN" record verify > "$WORK/$name-verify.log" 2>&1
  HOME="$WORK/home" ABHED_DATABASE_URL="$runtime" "$BIN" record audit > "$WORK/$name-audit.log" 2>&1
  (cd "$ROOT" && ABHED_TEST_DSN="$owner" ABHED_TEST_RUNTIME_DSN="$runtime" ABHED_RESTORE_PHASE=check \
    ABHED_RESTORE_FILE="$WORK/fingerprint.json" go test ./store/ -run '^TestRestoreCheck$' -count=1 -v) \
    > "$WORK/$name-check.log" 2>&1 || { cat "$WORK/$name-check.log"; return 1; }
  grep -q -- '--- PASS: TestRestoreCheck' "$WORK/$name-check.log"
  echo "  shift after migrate: $(sql "$name" "$db" -At -c 'SELECT shift FROM commit_shift')"
  grep -c '^ok' "$WORK/$name-verify.log" | sed 's/^/  sessions verified: /'
  grep '^ok' "$WORK/$name-audit.log" | sed 's/^/  /'
  results+=("$name: PASS")
}

say "source: seed a record"
start src
# An engine-side abhed too, for the restore script's own migrate and checks.
case "$("$ENGINE" exec "$P-src" uname -m)" in
  aarch64 | arm64) arch=arm64 ;; x86_64 | amd64) arch=amd64 ;; s390x) arch=s390x ;;
  *) echo "unknown container architecture" >&2; exit 1 ;;
esac
LINUXBIN="$WORK/abhed-linux"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$LINUXBIN" ./cmd/abhed)
roles src
seq 1 "$BURN" | sed 's/.*/SELECT pg_current_xact_id();/' | "$ENGINE" exec -i "$P-src" psql -q -At -U pgsuper -d postgres > /dev/null
newdb src abhed
(cd "$ROOT" && ABHED_TEST_DSN="$(dsn src abhed abhed_owner:owner)" ABHED_TEST_RUNTIME_DSN="$(dsn src abhed abhed_runtime:runtime)" \
  ABHED_RESTORE_PHASE=seed ABHED_RESTORE_FILE="$WORK/fingerprint.json" go test ./store/ -run '^TestRestoreSeed$' -count=1)
src_runtime="$(dsn src abhed abhed_runtime:runtime)"
HOME="$WORK/home" ABHED_DATABASE_URL="$src_runtime" "$BIN" record verify > /dev/null

say "back up with abhed-backup.sh (as abhed_backup: BYPASSRLS, pg_read_all_data)"
for f in abhed-backup.sh abhed-restore.sh; do "$ENGINE" cp "$ROOT/scripts/backup/$f" "$P-src:/tmp/$f"; done
# A state folder like a server's, with the modes Abhed insists on.
"$ENGINE" exec "$P-src" sh -c 'mkdir -p /tmp/state/.abhed/secrets.d && echo "{}" > /tmp/state/.abhed/secrets.json &&
  echo key > /tmp/state/.abhed/cli-token.key && chmod 700 /tmp/state/.abhed /tmp/state/.abhed/secrets.d &&
  chmod 600 /tmp/state/.abhed/secrets.json /tmp/state/.abhed/cli-token.key'
# Refusals: a password on the command line, a retention that is not a
# number, and a failed run leaving nothing half-written behind.
if "$ENGINE" exec -e ABHED_BACKUP_DSN="postgres://abhed_backup:backup@127.0.0.1/abhed" -e ABHED_BACKUP_DIR=/tmp/neg \
  "$P-src" sh /tmp/abhed-backup.sh 2> "$WORK/neg1.log"; then echo "a DSN with a password was accepted" >&2; exit 1; fi
grep -q 'carries a password' "$WORK/neg1.log"
if "$ENGINE" exec -e ABHED_BACKUP_STATE=/tmp/state -e ABHED_BACKUP_KEEP_DAYS=7d -e ABHED_BACKUP_DIR=/tmp/neg \
  "$P-src" sh /tmp/abhed-backup.sh 2> "$WORK/neg2.log"; then echo "ABHED_BACKUP_KEEP_DAYS=7d was accepted" >&2; exit 1; fi
grep -q 'whole number' "$WORK/neg2.log"
if "$ENGINE" exec -e PGPASSWORD=backup -e ABHED_BACKUP_DSN="postgres://abhed_backup@127.0.0.1/nosuchdb" -e ABHED_BACKUP_DIR=/tmp/neg \
  "$P-src" sh /tmp/abhed-backup.sh 2> /dev/null; then echo "a dump of a missing database succeeded" >&2; exit 1; fi
[ "$("$ENGINE" exec "$P-src" sh -c 'ls /tmp/neg | wc -l')" = 0 ] || { echo "a failed run left files behind" >&2; exit 1; }
if "$ENGINE" exec -e ABHED_RESTORE_DSN="postgres://pgsuper:pgsuper@127.0.0.1/postgres" -e ABHED_RESTORE_DUMP=/dev/null \
  "$P-src" sh /tmp/abhed-restore.sh 2> "$WORK/neg3.log"; then echo "a restore DSN with a password was accepted" >&2; exit 1; fi
grep -q 'carries a password' "$WORK/neg3.log"
echo "  refused: passwords in DSNs, a non-numeric retention; a failed run left no .part file"
"$ENGINE" exec -e PGPASSWORD=backup -e ABHED_BACKUP_DSN="postgres://abhed_backup@127.0.0.1/abhed" \
  -e ABHED_BACKUP_STATE=/tmp/state -e ABHED_BACKUP_DIR=/tmp/bk "$P-src" sh /tmp/abhed-backup.sh 2> /dev/null
"$ENGINE" cp "$P-src:/tmp/bk" "$WORK/bk"
DUMP="$(cd "$WORK/bk" && ls abhed-db-*.dump)"
STATE="$(cd "$WORK/bk" && ls abhed-state-*.tar.gz)"

say "back up physically (pg_basebackup)"
"$ENGINE" exec "$P-src" pg_basebackup -U pgsuper -h 127.0.0.1 -D - -Ft -X fetch > "$WORK/base.tar"

say "restore 1: logical, into a new database of the same cluster"
sql src postgres -c "CREATE DATABASE abhed_same"
"$ENGINE" exec -e PGPASSWORD=pgsuper -e ABHED_RESTORE_DSN="postgres://pgsuper@127.0.0.1/abhed_same" \
  -e ABHED_RESTORE_DUMP="/tmp/bk/$DUMP" "$P-src" sh /tmp/abhed-restore.sh
# A second restore into the now full database is refused, before any role is touched.
if "$ENGINE" exec -e PGPASSWORD=pgsuper -e ABHED_RESTORE_DSN="postgres://pgsuper@127.0.0.1/abhed_same" \
  -e ABHED_RESTORE_DUMP="/tmp/bk/$DUMP" -e ABHED_RESTORE_CREATE_ROLES="abhed_extra" "$P-src" sh /tmp/abhed-restore.sh 2> /dev/null; then
  echo "a restore over a full database went ahead" >&2; exit 1
fi
[ "$(sql src postgres -At -c "SELECT count(*) FROM pg_roles WHERE rolname = 'abhed_extra'")" = 0 ] ||
  { echo "a refused restore made a role" >&2; exit 1; }
check src abhed_same

say "restore 2: logical, into a fresh cluster whose counter is behind the source's"
start fresh
"$ENGINE" cp "$WORK/bk" "$P-fresh:/tmp/bk"
"$ENGINE" cp "$ROOT/scripts/backup/abhed-restore.sh" "$P-fresh:/tmp/abhed-restore.sh"
sql fresh postgres -c "CREATE DATABASE abhed"
echo "  source counter $(sql src postgres -At -c 'SELECT pg_current_xact_id()'), fresh counter $(sql fresh postgres -At -c 'SELECT pg_current_xact_id()')"
supers="$(sql fresh postgres -At -c 'SELECT count(*) FROM pg_roles WHERE rolsuper')"
"$ENGINE" exec -e PGPASSWORD=pgsuper -e ABHED_RESTORE_DSN="postgres://pgsuper@127.0.0.1/abhed" \
  -e ABHED_RESTORE_DUMP="/tmp/bk/$DUMP" -e ABHED_RESTORE_CREATE_ROLES="abhed_owner abhed_runtime" \
  -e ABHED_RESTORE_STATE="/tmp/bk/$STATE" -e ABHED_RESTORE_STATE_ROOT=/tmp/restored "$P-fresh" sh -c 'mkdir -p /tmp/restored && sh /tmp/abhed-restore.sh'
[ "$(sql fresh postgres -At -c 'SELECT count(*) FROM pg_roles WHERE rolsuper')" = "$supers" ] ||
  { echo "the restore made a superuser" >&2; exit 1; }
[ "$(sql fresh postgres -At -c "SELECT count(*) FROM pg_authid WHERE rolname IN ('abhed_owner', 'abhed_runtime')
  AND rolcanlogin AND NOT rolsuper AND NOT rolbypassrls AND NOT rolcreaterole AND NOT rolcreatedb
  AND rolpassword IS NULL")" = 2 ] || { echo "the Abhed roles were not made as plain roles without a password" >&2; exit 1; }
want="$("$ENGINE" exec "$P-src" sh -c 'cd /tmp/state && find . -exec stat -c "%a %n" {} \; | sort && cat .abhed/secrets.json .abhed/cli-token.key')"
got="$("$ENGINE" exec "$P-fresh" sh -c 'cd /tmp/restored/tmp/state && find . -exec stat -c "%a %n" {} \; | sort && cat .abhed/secrets.json .abhed/cli-token.key')"
[ "$want" = "$got" ] || { echo "the state came back different:"; echo "$want"; echo "$got"; exit 1; } >&2
echo "  state tarball restored with its modes and contents"
# The roles were made without passwords; set them, then let the restore
# script run abhed migrate, record verify and record audit itself.
sql fresh postgres -c "ALTER ROLE abhed_owner PASSWORD 'owner'" -c "ALTER ROLE abhed_runtime PASSWORD 'runtime'"
"$ENGINE" cp "$LINUXBIN" "$P-fresh:/tmp/abhed"
"$ENGINE" exec "$P-fresh" sh -c 'mkdir -p /tmp/home/.abhed && chmod 700 /tmp/home/.abhed &&
  printf "{\"storage\":{\"driver\":\"postgres\",\"tenant\":\"restore\"}}\n" > /tmp/home/.abhed/config.json &&
  chmod 600 /tmp/home/.abhed/config.json && chown -R postgres /tmp/home /tmp/abhed'
"$ENGINE" exec -u postgres -e HOME=/tmp/home -e ABHED_BIN=/tmp/abhed \
  -e ABHED_MIGRATE_DATABASE_URL="postgres://abhed_owner:owner@127.0.0.1/abhed?sslmode=disable" \
  -e ABHED_DATABASE_URL="postgres://abhed_runtime:runtime@127.0.0.1/abhed?sslmode=disable" \
  "$P-fresh" sh /tmp/abhed-restore.sh > "$WORK/fresh-script.log" 2>&1 || { cat "$WORK/fresh-script.log"; exit 1; }
grep -q '^ok .*audit' "$WORK/fresh-script.log"
echo "  abhed-restore.sh ran migrate ($(sql fresh abhed -At -c 'SELECT shift FROM commit_shift') shift), $(grep -c '^ok .*chained event' "$WORK/fresh-script.log") sessions verified, audit ok"
check fresh abhed

say "restore 3: physical, a new cluster started from the base backup"
mkdir -p "$WORK/restored"
tar -xf "$WORK/base.tar" -C "$WORK/restored"
"$ENGINE" create --name "$P-phys" -e PGDATA=/var/lib/postgresql/restored -p 127.0.0.1::5432 "$IMAGE" > /dev/null
made+=("$P-phys")
"$ENGINE" cp "$WORK/restored" "$P-phys:/var/lib/postgresql/restored"
"$ENGINE" start "$P-phys" > /dev/null
ready "$P-phys"
check phys abhed

say "results"
printf '  %s\n' "${results[@]}"
