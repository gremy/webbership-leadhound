#!/bin/sh
# leadhound-backup: dump the leadhound database in pg_dump's custom format and keep the
# newest LEADHOUND_BACKUP_KEEP dumps, within LEADHOUND_BACKUP_MAX_BYTES together. Installed as
# /opt/leadhound/bin/leadhound-backup and run as postgres by leadhound-backup.service.
#
# Restore: pg_restore --clean --if-exists -d leadhound <file> (as postgres).
set -eu

dir=${LEADHOUND_BACKUP_DIR:-/var/backups/leadhound}
db=${LEADHOUND_BACKUP_DB:-leadhound}
keep=${LEADHOUND_BACKUP_KEEP:-14}
max=${LEADHOUND_BACKUP_MAX_BYTES:-4294967296}

case $keep in
'' | *[!0-9]*) echo "leadhound-backup: LEADHOUND_BACKUP_KEEP must be a whole number" >&2; exit 2 ;;
esac
if [ "$keep" -lt 1 ]; then
	echo "leadhound-backup: LEADHOUND_BACKUP_KEEP must be at least 1" >&2
	exit 2
fi
# At most 18 digits, so the sums below stay inside the shell's 64-bit arithmetic.
case $max in
'' | *[!0-9]* | ???????????????????*) echo "leadhound-backup: LEADHOUND_BACKUP_MAX_BYTES must be a whole number of at most 18 digits" >&2; exit 2 ;;
esac
if [ "$max" -lt 1 ]; then
	echo "leadhound-backup: LEADHOUND_BACKUP_MAX_BYTES must be at least 1" >&2
	exit 2
fi

# size prints a file's size in bytes: GNU stat, else BSD stat.
size() {
	stat -c %s -- "$1" 2>/dev/null || stat -f %z -- "$1"
}

umask 077
stamp=$(date -u +%Y%m%dT%H%M%SZ)
out="$dir/leadhound-$stamp.dump"
tmp="$dir/.leadhound-$stamp.dump.part"
trap 'rm -f "$tmp"' EXIT

# Written under a temporary name, so a failed dump never counts as one of the kept, and the
# script stops here before it deletes anything.
pg_dump --format=custom --file="$tmp" "$db"
mv "$tmp" "$out"
echo "leadhound-backup: wrote $out"

# The names sort by time. Walking from the newest, keep each dump while at most $keep are kept
# and their sizes together stay within $max; the first that does not fit and every older one
# go. The newest is always kept, even when it alone is over $max. Nothing is deleted before the
# cut, so a failed stat stops the script with every dump still there.
n=0
total=0
newest=0
cut=
while IFS= read -r f; do
	[ -f "$f" ] || continue
	if [ -z "$cut" ]; then
		s=$(size "$f")
		n=$((n + 1))
		total=$((total + s))
		[ "$n" -eq 1 ] && newest=$s
		if [ "$n" -eq 1 ] || { [ "$n" -le "$keep" ] && [ "$total" -le "$max" ]; }; then
			continue
		fi
		cut=1
	fi
	rm -f -- "$f"
	echo "leadhound-backup: removed $f"
done <<LIST
$(printf '%s\n' "$dir"/leadhound-*.dump | sort -r)
LIST
if [ "$newest" -gt "$max" ]; then
	echo "leadhound-backup: the newest dump alone is over LEADHOUND_BACKUP_MAX_BYTES ($newest > $max bytes)" >&2
fi
