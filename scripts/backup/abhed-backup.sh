#!/bin/sh
# Back up a self-hosted Abhed: a logical dump of its Postgres database and a
# tarball of the server's state folders. POSIX sh, so it runs on a host or in
# a Postgres container whose client tools match the server's version.
#
#   ABHED_BACKUP_DSN=postgres://abhed_backup:…@db/abhed \
#   ABHED_BACKUP_STATE="/home/abhed/.abhed /etc/abhed" \
#   ABHED_BACKUP_DIR=/srv/backups  scripts/backup/abhed-backup.sh
#
# ABHED_BACKUP_DSN   the database to dump. The role must read every tenant's
#                    rows past FORCE ROW LEVEL SECURITY: a superuser, or a
#                    role with BYPASSRLS and pg_read_all_data. Unset: no dump.
# ABHED_BACKUP_STATE folders and files to tar (space separated). Stop the
#                    server first, or use a filesystem snapshot. Unset: none.
# ABHED_BACKUP_DIR   where backups go (default ./abhed-backups), made 0700.
# ABHED_BACKUP_KEEP_DAYS  remove this script's backups older than this (14).
#
# Each file is written under a temporary name and renamed when complete, and
# the dump is listed with pg_restore before it counts. The state tarball
# holds secrets and keys: keep the backup folder on encrypted storage.
# Restore with scripts/backup/abhed-restore.sh.
set -eu
umask 077

DEST="${ABHED_BACKUP_DIR:-./abhed-backups}"
KEEP="${ABHED_BACKUP_KEEP_DAYS:-14}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$DEST"
chmod 700 "$DEST"

if [ -z "${ABHED_BACKUP_DSN:-}" ] && [ -z "${ABHED_BACKUP_STATE:-}" ]; then
  echo "abhed-backup: set ABHED_BACKUP_DSN, ABHED_BACKUP_STATE or both" >&2
  exit 2
fi

if [ -n "${ABHED_BACKUP_DSN:-}" ]; then
  out="$DEST/abhed-db-$STAMP.dump"
  pg_dump --format=custom --no-password --dbname="$ABHED_BACKUP_DSN" --file="$out.part"
  # A dump pg_restore cannot list is not a backup.
  pg_restore --list "$out.part" > /dev/null
  mv "$out.part" "$out"
  # Roles are global, not in the dump; their names and attributes, without
  # passwords, so a restore can recreate them first.
  pg_dumpall --roles-only --no-role-passwords --dbname="$ABHED_BACKUP_DSN" > "$DEST/abhed-roles-$STAMP.sql.part"
  mv "$DEST/abhed-roles-$STAMP.sql.part" "$DEST/abhed-roles-$STAMP.sql"
  echo "database  $out"
fi

if [ -n "${ABHED_BACKUP_STATE:-}" ]; then
  out="$DEST/abhed-state-$STAMP.tar.gz"
  # Word splitting is the list's separator.
  # shellcheck disable=SC2086
  tar -czf "$out.part" $ABHED_BACKUP_STATE
  tar -tzf "$out.part" > /dev/null
  mv "$out.part" "$out"
  echo "state     $out"
fi

find "$DEST" -maxdepth 1 -type f \( -name 'abhed-db-*.dump' -o -name 'abhed-roles-*.sql' \
  -o -name 'abhed-state-*.tar.gz' \) -mtime +"$KEEP" -print -exec rm -f {} \; | sed 's/^/removed   /'
