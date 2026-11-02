#!/bin/sh
# Restore what scripts/backup/abhed-backup.sh wrote. POSIX sh.
#
#   ABHED_RESTORE_DSN=postgres://postgres:…@db/abhed \
#   ABHED_RESTORE_DUMP=/srv/backups/abhed-db-20261102T030000Z.dump \
#   scripts/backup/abhed-restore.sh
#
# ABHED_RESTORE_DSN    a superuser's connection to an EMPTY database, made
#                      beforehand (createdb). The owner and runtime roles must
#                      already exist; ABHED_RESTORE_ROLES can make them.
# ABHED_RESTORE_DUMP   the database dump. Unset: no database restore.
# ABHED_RESTORE_ROLES  the roles file from the same backup, applied first;
#                      roles that exist are left as they are. Roles made from
#                      it have no password: set one before the server starts.
# ABHED_RESTORE_STATE  a state tarball, unpacked into ABHED_RESTORE_STATE_ROOT
#                      (default /), keeping the files' modes. Stop the server
#                      first.
# ABHED_BIN, ABHED_MIGRATE_DATABASE_URL, ABHED_DATABASE_URL
#                      when all three are set, run `abhed migrate` as the
#                      owner, then `abhed record verify` and `abhed record
#                      audit` as the runtime role, and fail if either fails.
#
# The whole dump is restored in one transaction and stops at the first
# error. The record's triggers are created after the rows are loaded, as
# pg_restore orders a full dump, so every chained column is kept as it was.
# Then `abhed migrate` must run before any server starts: in a cluster whose
# transaction counter is behind the one the backup came from, it moves the
# commit order on so readers of the record in commit order (the OCSF file,
# SIEM delivery) do not skip new events.
set -eu

if [ -n "${ABHED_RESTORE_DUMP:-}" ]; then
  : "${ABHED_RESTORE_DSN:?set ABHED_RESTORE_DSN to a superuser connection to an empty database}"
  [ -r "$ABHED_RESTORE_DUMP" ] || { echo "abhed-restore: cannot read $ABHED_RESTORE_DUMP" >&2; exit 1; }
  pg_restore --list "$ABHED_RESTORE_DUMP" > /dev/null
  if [ -n "${ABHED_RESTORE_ROLES:-}" ]; then
    # One statement at a time, so a role that exists does not stop the rest.
    psql --no-password --quiet --dbname="$ABHED_RESTORE_DSN" --set ON_ERROR_STOP=0 \
      --file="$ABHED_RESTORE_ROLES" 2>&1 | grep -v 'already exists' || true
  fi
  n="$(psql --no-password -At --dbname="$ABHED_RESTORE_DSN" -c \
    "SELECT count(*) FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace
     WHERE s.nspname = 'public' AND c.relkind IN ('r', 'p')")"
  if [ "$n" != 0 ]; then
    echo "abhed-restore: the target database already has $n tables; restore into an empty one" >&2
    exit 1
  fi
  pg_restore --no-password --exit-on-error --single-transaction \
    --dbname="$ABHED_RESTORE_DSN" "$ABHED_RESTORE_DUMP"
  echo "database  restored from $ABHED_RESTORE_DUMP"
fi

if [ -n "${ABHED_RESTORE_STATE:-}" ]; then
  root="${ABHED_RESTORE_STATE_ROOT:-/}"
  tar -tzf "$ABHED_RESTORE_STATE" > /dev/null
  tar -xpzf "$ABHED_RESTORE_STATE" -C "$root"
  echo "state     restored under $root"
fi

if [ -n "${ABHED_BIN:-}" ] && [ -n "${ABHED_MIGRATE_DATABASE_URL:-}" ] && [ -n "${ABHED_DATABASE_URL:-}" ]; then
  "$ABHED_BIN" migrate
  "$ABHED_BIN" record verify
  "$ABHED_BIN" record audit
elif [ -n "${ABHED_RESTORE_DUMP:-}" ]; then
  echo "next: run \`abhed migrate\` as the owner, then \`abhed record verify\` and \`abhed record audit\`, before starting any server"
fi
