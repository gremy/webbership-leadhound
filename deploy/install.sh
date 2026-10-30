#!/bin/sh
# install.sh STAGE: installs a release that `make deploy` copied into the directory STAGE.
# Run on the server as root (make deploy runs it with sudo). Order: the env files' owner and
# mode, the Postgres limits on the leadhound roles, the new binary beside the old one, migrate
# with the new binary as the owner role, then the swap, the mailbox key, the Anthropic key's mode, the backup script, the units,
# the timers, the one-time carry-over of LEADHOUND_LEADS_SEND to the console's sending switch,
# the leads timers as /etc/leadhound/env switches them, and the console. A failed migrate leaves the old
# binary and the old units in place. STAGE is removed once the units are in.
set -eu

stage=${1:?usage: install.sh STAGE}
bin=/opt/leadhound/bin
data=/opt/leadhound/data

fail() {
	echo "install: $*" >&2
	exit 1
}

# env_value FILE NAME: the value the units get for NAME from FILE, from the last line that sets
# it, quotes and trailing blanks left out; empty when no line sets it.
env_value() {
	sed -n "s/^[[:space:]]*$2=//p" "$1" | tail -n 1 | tr -d "\"'" | sed 's/[[:space:]]*$//'
}

# leads_timers FILE: the leads timers reach the public web and a mailbox, so each set runs only
# when FILE switches it on (deploy/README.md, Leads on the server); otherwise it is disabled and
# stopped, so a deploy never leaves one running that FILE does not ask for. The finding timers
# need LEADHOUND_LEADS_TIMERS=on. The drafting, sending and reply timers also need live
# outreach, so the server never drafts a shadow batch. The Send now watcher goes with the send
# timer: where the hourly send does not run, Send now does not either (the console refuses it
# with the same sentence). Whether anything is sent is the
# console's sending switch, in the database: LEADHOUND_LEADS_SEND is no longer read here
# (send_switch_convert carried it over once). The mailboxes live in the database too (the
# console's Mailboxes page): a run with no ready mailbox sends nothing and says so.
leads_timers() {
	off=
	[ "$(env_value "$1" LEADHOUND_LEADS_TIMERS)" = on ] || off="$off LEADHOUND_LEADS_TIMERS=on"
	if [ -z "$off" ]; then
		systemctl enable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer
		echo "install: leads finding timers on: probe, contacts"
	else
		systemctl disable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer
		echo "install: leads finding timers off (probe, contacts): $1 has no$off"
	fi
	[ "$(env_value "$1" LEADHOUND_OUTREACH)" = live ] || off="$off LEADHOUND_OUTREACH=live"
	if [ -z "$off" ]; then
		systemctl enable --now leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer leadhound-leads-send-now.path
		echo "install: leads sending timers on: batch, send, sync, and the Send now watcher"
	else
		systemctl disable --now leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer leadhound-leads-send-now.path
		echo "install: leads sending timers off (batch, send, sync, and the Send now watcher): $1 has no$off"
	fi
}

# send_switch_convert ENV MARKER: carries the retired LEADHOUND_LEADS_SEND switch in ENV over
# to the console's sending switch, once. With no MARKER, the installed binary runs, as leadhound
# with ENV, leads switches convert-send-env --was with ENV's value (empty when no line sets
# it), which pauses sending unless that value is on; then MARKER is written (root:root 0640).
# With MARKER there, nothing runs and ENV's line is never read again, so removing it is safe.
# A failed conversion fails the deploy and writes no MARKER, so the next deploy tries again.
# MARKER lives in /etc/leadhound (root:root 0700, the bootstrap in deploy/README.md), not in
# the leadhound-owned data dir: the app user cannot delete it and a data-dir restore cannot
# lose it, either of which would make a later deploy convert again and pause sending.
send_switch_convert() {
	if [ -e "$2" ]; then
		echo "install: LEADHOUND_LEADS_SEND was carried over to the console's sending switch already ($2)"
		return
	fi
	was=$(env_value "$1" LEADHOUND_LEADS_SEND)
	systemd-run --quiet --wait --pipe --collect --uid=leadhound --gid=leadhound \
		-p EnvironmentFile="$1" \
		"$bin/leadhound" leads switches convert-send-env --was "$was" ||
		fail "the old LEADHOUND_LEADS_SEND switch could not be carried over to the console's sending switch; the leads timers were not changed"
	(umask 0137 && printf 'LEADHOUND_LEADS_SEND=%s\n' "$was" >"$2")
	chown root:root "$2"
	chmod 0640 "$2"
	echo "install: LEADHOUND_LEADS_SEND=$was carried over to the console's sending switch; the line is no longer read"
}

# mailbox_key KEY ENV: the mailboxes' key pair. The private key KEY (root:root 0600) is
# generated once, by the binary just moved into place, only when KEY is missing; an existing
# key is never overwritten, because every saved mailbox password is sealed to it, only made
# root:root 0600 again (a restored copy may not be). Then the public key is always derived
# from KEY and written to ENV as LEADHOUND_MAILBOX_PUBLIC_KEY= (replacing a line that
# differs, appended when there is none), so ENV never holds a public key that does not match
# the private one. Only the fingerprint is printed.
mailbox_key() {
	if [ ! -e "$1" ]; then
		out=$("$bin/leadhound" leads mailboxes keygen --out "$1") ||
			fail "the mailbox key could not be generated at $1"
		echo "install: new mailbox key at $1, fingerprint $(printf '%s\n' "$out" | sed -n 's/^Fingerprint: //p')"
	fi
	# A key the operator restored from a backup may come back with another owner or mode,
	# which pubkey refuses; it is root's alone, generated or kept.
	chown root:root "$1"
	chmod 0600 "$1"
	line=$("$bin/leadhound" leads mailboxes pubkey --key "$1") ||
		fail "the public key could not be read from $1 (it must be root:root 0600)"
	case $line in
	LEADHOUND_MAILBOX_PUBLIC_KEY=?*) ;;
	*) fail "leads mailboxes pubkey printed no LEADHOUND_MAILBOX_PUBLIC_KEY= line" ;;
	esac
	if [ "LEADHOUND_MAILBOX_PUBLIC_KEY=$(env_value "$2" LEADHOUND_MAILBOX_PUBLIC_KEY)" = "$line" ] &&
		[ "$(grep -c '^[[:space:]]*LEADHOUND_MAILBOX_PUBLIC_KEY=' "$2")" -eq 1 ]; then
		echo "install: $2 holds the mailbox public key"
		return
	fi
	tmp=$(mktemp "$2.XXXXXX")
	grep -v '^[[:space:]]*LEADHOUND_MAILBOX_PUBLIC_KEY=' "$2" >"$tmp" || true
	printf '%s\n' "$line" >>"$tmp"
	chmod 0600 "$tmp"
	mv -f "$tmp" "$2"
	echo "install: wrote the mailbox public key to $2"
}

# anthropic_key KEY: the Anthropic key (deploy/README.md, The Anthropic key). The operator
# places KEY by hand; install never creates, reads or prints it. When it is there it is made
# root:root 0600 again on every deploy (a copy put back by hand may not be); when it is not,
# the units that call Claude cannot start, and install says so without failing the deploy.
anthropic_key() {
	if [ ! -e "$1" ]; then
		echo "install: no Anthropic key at $1: the units that call Claude (leadhound-leads-batch, leadhound-leads-research) cannot start until you place it (deploy/README.md, The Anthropic key)"
		return
	fi
	chown root:root "$1"
	chmod 0600 "$1"
	echo "install: the Anthropic key at $1 is root:root 0600"
}

# reoon_key KEY: the Reoon key (deploy/README.md, The Reoon key), as anthropic_key does the
# Anthropic key: placed by hand, never created, read or printed here, made root:root 0600 on
# every deploy when it is there. Without it the batch unit still starts (its SetCredential=
# default) and its email check says there is no key.
reoon_key() {
	if [ ! -e "$1" ]; then
		echo "install: no Reoon key at $1: the email check checks nothing until you place it; switch the Email check off in Controls meanwhile (deploy/README.md, The Reoon key)"
		return
	fi
	chown root:root "$1"
	chmod 0600 "$1"
	echo "install: the Reoon key at $1 is root:root 0600"
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
# The console's Run now card writes its requests here, and the leadhound-*.path watchers
# read them; created before the watchers are armed (deploy/README.md, Run now).
install -d -o leadhound -g leadhound -m 0700 "$data/run"
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
# The mailbox key, before the units that read it are installed (mailbox_key above).
mailbox_key /etc/leadhound/mailbox.key /etc/leadhound/env
# The Anthropic key's owner and mode, before the units that read it are installed.
anthropic_key /etc/leadhound/anthropic.key
reoon_key /etc/leadhound/reoon.key
install -o root -g root -m 0755 "$stage/leadhound-backup.sh" "$bin/leadhound-backup"
for f in "$stage"/leadhound-*.service "$stage"/leadhound-*.timer "$stage"/leadhound-*.path; do
	install -o root -g root -m 0644 "$f" /etc/systemd/system/
done

systemctl daemon-reload
systemctl enable --now leadhound-ats.timer leadhound-collect.timer leadhound-prebrief.timer \
	leadhound-brief.timer leadhound-backup.timer leadhound-trim.timer
# The Run now watchers, the mailbox check's and contact research's (Send now's goes with the send timer, in leads_timers): the console asks through
# these, and each starts only its own unit. Arming one never starts its service, and PathChanged= waits for a new write, so
# a request file left from before cannot fire on a deploy.
systemctl enable --now leadhound-preview.path leadhound-fetch.path leadhound-brief.path leadhound-ats.path leadhound-mailbox-check.path leadhound-leads-research.path
# Once, before any sending timer can be enabled: the old env switch becomes the console's.
send_switch_convert /etc/leadhound/env /etc/leadhound/send-switch-converted
leads_timers /etc/leadhound/env
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
