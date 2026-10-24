#!/bin/sh
# leadhound-backup: dump the leadhound database in pg_dump's custom format and keep the
# newest LEADHOUND_BACKUP_KEEP dumps. Installed as /opt/leadhound/bin/leadhound-backup and
# run as postgres by leadhound-backup.service.
#
# Restore: pg_restore --clean --if-exists -d leadhound <file> (as postgres).
set -eu

dir=${LEADHOUND_BACKUP_DIR:-/var/backups/leadhound}
db=${LEADHOUND_BACKUP_DB:-leadhound}
keep=${LEADHOUND_BACKUP_KEEP:-14}

case $keep in
'' | *[!0-9]*) echo "leadhound-backup: LEADHOUND_BACKUP_KEEP must be a whole number" >&2; exit 2 ;;
esac
if [ "$keep" -lt 1 ]; then
	echo "leadhound-backup: LEADHOUND_BACKUP_KEEP must be at least 1" >&2
	exit 2
fi

umask 077
stamp=$(date -u +%Y%m%dT%H%M%SZ)
out="$dir/leadhound-$stamp.dump"
tmp="$dir/.leadhound-$stamp.dump.part"
trap 'rm -f "$tmp"' EXIT

# Written under a temporary name, so a failed dump never counts as one of the kept.
pg_dump --format=custom --file="$tmp" "$db"
mv "$tmp" "$out"
echo "leadhound-backup: wrote $out"

# The names sort by time; delete all but the newest $keep.
printf '%s\n' "$dir"/leadhound-*.dump | sort -r | tail -n +"$((keep + 1))" |
	while IFS= read -r old; do
		[ -f "$old" ] || continue
		rm -f -- "$old"
		echo "leadhound-backup: removed $old"
	done
