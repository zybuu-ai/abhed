#!/bin/sh
# Restore what abhed-backup.sh wrote. POSIX sh.
#
#   PGPASSFILE=/root/restore.pgpass \
#   ABHED_RESTORE_DSN=postgres://postgres@db/abhed \
#   ABHED_RESTORE_DUMP=/srv/backups/abhed-db-20261102T030000Z.dump \
#   ABHED_RESTORE_CREATE_ROLES="abhed_owner abhed_runtime" \
#   scripts/backup/abhed-restore.sh
#
# ABHED_RESTORE_DSN    a superuser's connection to an EMPTY database, made
#                      beforehand (createdb), with no password in it: the
#                      password comes from PGPASSFILE (or ~/.pgpass) or
#                      PGPASSWORD. A DSN that carries one is refused.
# ABHED_RESTORE_DUMP   the database dump. Unset: no database restore.
# ABHED_RESTORE_CREATE_ROLES  the Abhed roles the dump's objects belong to
#                      (owner and runtime), made after the empty-database
#                      check and only if missing: LOGIN, no password,
#                      NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE. A role
#                      that exists is not changed. Set the new roles'
#                      passwords before a server starts. Unset: the roles
#                      must exist already.
# ABHED_RESTORE_STATE  a state tarball, unpacked into ABHED_RESTORE_STATE_ROOT
#                      (default /), keeping the files' modes. Stop the server
#                      first.
# ABHED_BIN, ABHED_MIGRATE_DATABASE_URL, ABHED_DATABASE_URL
#                      when all three are set, run `abhed migrate` as the
#                      owner, then `abhed record verify` and `abhed record
#                      audit` as the runtime role, and fail if either fails.
#                      abhed reads them from its environment, not its command
#                      line.
#
# The whole dump is restored in one transaction that stops at the first
# error. A full dump creates the record's triggers after its rows are
# loaded, so every chained column is kept as it was. Then `abhed migrate`
# must run before any server starts: in a cluster whose transaction counter
# is behind the one the backup came from, it moves the commit order on so
# readers of the record in commit order (the OCSF file, SIEM delivery) do not
# skip new events; a runtime server started before it refuses.
set -eu

has_password() {
  case "$1" in
    *[Pp][Aa][Ss][Ss][Ww][Oo][Rr][Dd]=*) return 0 ;;
    postgres://*@* | postgresql://*@*)
      userinfo="${1#*://}"
      userinfo="${userinfo%%@*}"
      case "$userinfo" in *:*) return 0 ;; esac ;;
  esac
  return 1
}

if [ -n "${ABHED_RESTORE_DUMP:-}" ]; then
  : "${ABHED_RESTORE_DSN:?set ABHED_RESTORE_DSN to a superuser connection to an empty database}"
  if has_password "$ABHED_RESTORE_DSN"; then
    echo "abhed-restore: ABHED_RESTORE_DSN carries a password; put it in PGPASSFILE or PGPASSWORD instead" >&2
    exit 2
  fi
  [ -r "$ABHED_RESTORE_DUMP" ] || { echo "abhed-restore: cannot read $ABHED_RESTORE_DUMP" >&2; exit 1; }
  pg_restore --list "$ABHED_RESTORE_DUMP" > /dev/null
  psql_() { psql --no-password --quiet -At --set ON_ERROR_STOP=1 --dbname="$ABHED_RESTORE_DSN" "$@"; }

  # Before anything cluster-wide is touched.
  n="$(psql_ -c "SELECT count(*) FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace
     WHERE s.nspname = 'public' AND c.relkind IN ('r', 'p')")"
  if [ "$n" != 0 ]; then
    echo "abhed-restore: the target database already has $n tables; restore into an empty one" >&2
    exit 1
  fi

  for role in ${ABHED_RESTORE_CREATE_ROLES:-}; do
    case "$role" in
      *[!a-z0-9_]* | [0-9]*) echo "abhed-restore: '$role' is not a role name this script makes" >&2; exit 2 ;;
    esac
    if [ "$(psql_ -c "SELECT count(*) FROM pg_roles WHERE rolname = '$role'")" = 0 ]; then
      psql_ -c "CREATE ROLE $role LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"
      echo "role      $role made, with no password"
    fi
  done

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
