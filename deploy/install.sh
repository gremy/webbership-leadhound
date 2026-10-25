#!/bin/sh
# install.sh STAGE: installs a release that `make deploy` copied into the directory STAGE.
# Run on the server as root (make deploy runs it with sudo). Order: the env files' owner and
# mode, the Postgres limits on the leadhound roles, the new binary beside the old one, migrate
# with the new binary as the owner role, then the swap, the backup script, the units, the timers and the console. A failed migrate
# leaves the old binary and the old units in place. STAGE is removed once the units are in.
set -eu

stage=${1:?usage: install.sh STAGE}
bin=/opt/leadhound/bin
data=/opt/leadhound/data

fail() {
	echo "install: $*" >&2
	exit 1
}

# The one-time bootstrap (deploy/README.md) comes first.
id leadhound >/dev/null 2>&1 || fail "no leadhound user: run the bootstrap in deploy/README.md first"
id postgres >/dev/null 2>&1 || fail "no postgres user: install Postgres first (deploy/README.md)"
for f in /etc/leadhound/env /etc/leadhound/migrate.env; do
	[ -f "$f" ] || fail "$f is missing: write it from deploy/$(basename "$f").example"
done
# Both hold secrets; systemd reads them as root before dropping to leadhound.
chown root:root /etc/leadhound/env /etc/leadhound/migrate.env
chmod 0600 /etc/leadhound/env /etc/leadhound/migrate.env

install -d -o root -g root -m 0755 /opt/leadhound "$bin"
install -d -o leadhound -g leadhound -m 0750 "$data"
install -d -o postgres -g postgres -m 0700 /var/backups/leadhound

# The per-role limits (sql/limits.sql) are re-applied on every deploy. root opens the file and
# psql reads it on stdin, because the stage is 0700 and the postgres user cannot read inside it.
runuser -u postgres -- psql -X -v ON_ERROR_STOP=1 -d postgres <"$stage/limits.sql" ||
	fail "the Postgres limits for the leadhound roles did not apply; nothing else was changed"

# The new binary migrates before it replaces the old one, so a failed migrate leaves the
# release that was running.
install -o root -g root -m 0755 "$stage/leadhound" "$bin/.leadhound.new"
if ! systemd-run --quiet --wait --pipe --collect --uid=leadhound --gid=leadhound \
	-p EnvironmentFile=/etc/leadhound/migrate.env \
	"$bin/.leadhound.new" migrate; then
	rm -f -- "$bin/.leadhound.new"
	fail "migrate failed: the old binary and units are still in place; nothing else was changed (the release stays in $stage)"
fi
# A rename, so a run in progress keeps its own binary.
mv -f "$bin/.leadhound.new" "$bin/leadhound"
install -o root -g root -m 0755 "$stage/leadhound-backup.sh" "$bin/leadhound-backup"
for f in "$stage"/leadhound-*.service "$stage"/leadhound-*.timer; do
	install -o root -g root -m 0644 "$f" /etc/systemd/system/
done

systemctl daemon-reload
systemctl enable --now leadhound-ats.timer leadhound-collect.timer leadhound-prebrief.timer \
	leadhound-brief.timer leadhound-backup.timer
systemctl list-timers 'leadhound-*' --no-pager
rm -rf -- "$stage"

# The console starts only once its sign-in is configured (deploy/README.md, Console), so a
# deploy before that still succeeds. It is restarted on every deploy to run the new binary,
# which signs the operator out. With Type=exec systemd calls it active the moment it starts,
# even when it then exits on bad config, so install waits for /healthz to answer.
if grep -q '^LEADHOUND_CONSOLE_PASSWORD_HASH=.' /etc/leadhound/env; then
	systemctl enable leadhound-console.service
	systemctl restart leadhound-console.service
	addr=$(sed -n 's/^LEADHOUND_CONSOLE_ADDR=//p' /etc/leadhound/env | tail -n 1 | tr -d "\"'")
	addr=${addr:-localhost:8090}
	tries=0
	until curl -fsS -o /dev/null "http://$addr/healthz" 2>/dev/null; do
		tries=$((tries + 1))
		if [ "$tries" -ge 10 ]; then
			journalctl -u leadhound-console.service -n 20 --no-pager
			fail "the console did not answer on $addr within 10 seconds (its log is above); the timers are in place"
		fi
		sleep 1
	done
	echo "install: the console answers on $addr"
else
	echo "install: console not started: /etc/leadhound/env has no LEADHOUND_CONSOLE_PASSWORD_HASH yet (deploy/README.md, Console)"
fi

echo "install: done"
