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
roles src
seq 1 "$BURN" | sed 's/.*/SELECT pg_current_xact_id();/' | "$ENGINE" exec -i "$P-src" psql -q -At -U pgsuper -d postgres > /dev/null
newdb src abhed
(cd "$ROOT" && ABHED_TEST_DSN="$(dsn src abhed abhed_owner:owner)" ABHED_TEST_RUNTIME_DSN="$(dsn src abhed abhed_runtime:runtime)" \
  ABHED_RESTORE_PHASE=seed ABHED_RESTORE_FILE="$WORK/fingerprint.json" go test ./store/ -run '^TestRestoreSeed$' -count=1)
src_runtime="$(dsn src abhed abhed_runtime:runtime)"
HOME="$WORK/home" ABHED_DATABASE_URL="$src_runtime" "$BIN" record verify > /dev/null

say "back up with abhed-backup.sh (as abhed_backup: BYPASSRLS, pg_read_all_data)"
"$ENGINE" cp "$ROOT/scripts/backup/abhed-backup.sh" "$P-src:/tmp/abhed-backup.sh"
"$ENGINE" cp "$ROOT/scripts/backup/abhed-restore.sh" "$P-src:/tmp/abhed-restore.sh"
"$ENGINE" exec -e ABHED_BACKUP_DSN="postgres://abhed_backup:backup@127.0.0.1/abhed" -e ABHED_BACKUP_DIR=/tmp/bk \
  "$P-src" sh /tmp/abhed-backup.sh
"$ENGINE" cp "$P-src:/tmp/bk" "$WORK/bk"
DUMP="$(cd "$WORK/bk" && ls abhed-db-*.dump)"
ROLES="$(cd "$WORK/bk" && ls abhed-roles-*.sql)"

say "back up physically (pg_basebackup)"
"$ENGINE" exec "$P-src" pg_basebackup -U pgsuper -h 127.0.0.1 -D - -Ft -X fetch > "$WORK/base.tar"

say "restore 1: logical, into a new database of the same cluster"
sql src postgres -c "CREATE DATABASE abhed_same"
"$ENGINE" exec -e ABHED_RESTORE_DSN="postgres://pgsuper:pgsuper@127.0.0.1/abhed_same" \
  -e ABHED_RESTORE_DUMP="/tmp/bk/$DUMP" "$P-src" sh /tmp/abhed-restore.sh
check src abhed_same

say "restore 2: logical, into a fresh cluster whose counter is behind the source's"
start fresh
"$ENGINE" cp "$WORK/bk" "$P-fresh:/tmp/bk"
"$ENGINE" cp "$ROOT/scripts/backup/abhed-restore.sh" "$P-fresh:/tmp/abhed-restore.sh"
sql fresh postgres -c "CREATE DATABASE abhed"
echo "  source counter $(sql src postgres -At -c 'SELECT pg_current_xact_id()'), fresh counter $(sql fresh postgres -At -c 'SELECT pg_current_xact_id()')"
"$ENGINE" exec -e ABHED_RESTORE_DSN="postgres://pgsuper:pgsuper@127.0.0.1/abhed" \
  -e ABHED_RESTORE_DUMP="/tmp/bk/$DUMP" -e ABHED_RESTORE_ROLES="/tmp/bk/$ROLES" "$P-fresh" sh /tmp/abhed-restore.sh
# The roles file carries no passwords.
sql fresh postgres -c "ALTER ROLE abhed_owner PASSWORD 'owner'" -c "ALTER ROLE abhed_runtime PASSWORD 'runtime'"
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
