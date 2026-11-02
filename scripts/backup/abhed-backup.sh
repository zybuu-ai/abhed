#!/bin/sh
# Back up a self-hosted Abhed: a logical dump of its Postgres database and a
# tarball of the server's state folders. POSIX sh, so it runs on a host or in
# a Postgres container whose client tools match the server's version.
#
#   PGPASSFILE=/etc/abhed/backup.pgpass \
#   ABHED_BACKUP_DSN=postgres://abhed_backup@db/abhed \
#   ABHED_BACKUP_STATE="/home/abhed/.abhed /etc/abhed" \
#   ABHED_BACKUP_DIR=/srv/backups  scripts/backup/abhed-backup.sh
#
# ABHED_BACKUP_DSN   the database to dump, with no password in it: the
#                    password comes from PGPASSFILE (or ~/.pgpass) or
#                    PGPASSWORD, so it never appears on a command line. A DSN
#                    that carries one is refused. The role must read every
#                    tenant's rows past FORCE ROW LEVEL SECURITY: a superuser,
#                    or a role with BYPASSRLS and pg_read_all_data. Unset: no
#                    dump.
# ABHED_BACKUP_STATE folders and files to tar (space separated). Stop the
#                    server first, or use a filesystem snapshot. Unset: none.
# ABHED_BACKUP_DIR   where backups go (default ./abhed-backups), made 0700.
# ABHED_BACKUP_KEEP_DAYS  remove this script's backups older than this many
#                    days (a whole number; 14 by default).
#
# Each file is written under a temporary name and renamed when complete; a
# run that fails removes what it left. The dump is listed with pg_restore
# before it counts. The state tarball holds secrets and keys: keep the
# backup folder on encrypted storage. Restore with abhed-restore.sh.
set -eu
umask 077

DEST="${ABHED_BACKUP_DIR:-./abhed-backups}"
KEEP="${ABHED_BACKUP_KEEP_DAYS:-14}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

case "$KEEP" in
  '' | *[!0-9]*) echo "abhed-backup: ABHED_BACKUP_KEEP_DAYS must be a whole number of days, not '$KEEP'" >&2; exit 2 ;;
esac

# A password in a connection string would be on the clients' command lines,
# readable by any local user from the process list.
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

if [ -z "${ABHED_BACKUP_DSN:-}" ] && [ -z "${ABHED_BACKUP_STATE:-}" ]; then
  echo "abhed-backup: set ABHED_BACKUP_DSN, ABHED_BACKUP_STATE or both" >&2
  exit 2
fi
if [ -n "${ABHED_BACKUP_DSN:-}" ] && has_password "$ABHED_BACKUP_DSN"; then
  echo "abhed-backup: ABHED_BACKUP_DSN carries a password; put it in PGPASSFILE or PGPASSWORD instead" >&2
  exit 2
fi

mkdir -p "$DEST"
chmod 700 "$DEST"
parts=""
cleanup() {
  for f in $parts; do rm -f "$f"; done
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

if [ -n "${ABHED_BACKUP_DSN:-}" ]; then
  out="$DEST/abhed-db-$STAMP.dump"
  parts="$parts $out.part"
  pg_dump --format=custom --no-password --dbname="$ABHED_BACKUP_DSN" --file="$out.part"
  # A dump pg_restore cannot list is not a backup.
  pg_restore --list "$out.part" > /dev/null
  mv "$out.part" "$out"
  echo "database  $out"
fi

if [ -n "${ABHED_BACKUP_STATE:-}" ]; then
  out="$DEST/abhed-state-$STAMP.tar.gz"
  parts="$parts $out.part"
  # Word splitting is the list's separator.
  # shellcheck disable=SC2086
  tar -czf "$out.part" $ABHED_BACKUP_STATE
  tar -tzf "$out.part" > /dev/null
  mv "$out.part" "$out"
  echo "state     $out"
fi

find "$DEST" -maxdepth 1 -type f \( -name 'abhed-db-*.dump' -o -name 'abhed-state-*.tar.gz' \) \
  -mtime +"$KEEP" -print -exec rm -f {} \; | sed 's/^/removed   /'
