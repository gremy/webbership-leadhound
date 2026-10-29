# Deploying leadhound to a server

leadhound runs on one Linux server with systemd, from timers.
Nothing in this directory names a server: the host is an ssh destination in your own ssh config, passed as `HOST=` on the command line, and every secret lives in files on the server.

## Server

Since 2026-10-26 production runs on a Hetzner CAX31 (arm64, 8 vCPU, 16 GiB), moved from the 2-vCPU CAX11 it shared with other services.
Its Postgres 18 cluster is shared with one other service, whose pgBackRest stanza backs up the whole cluster, so the leadhound database also rides there, encrypted and off the server, besides its own nightly dumps (see [Backups and the storage budget](#backups-and-the-storage-budget)).
Use the console's own DNS name as `HOST=`, so a deploy follows DNS; the server's name and address stay in the private notes.
The units' `MemoryMax` and `CPUQuota` caps were sized for the old shared 2-vCPU box and are left as they are; retuning them is a separate decision.

## What runs when

All times are Europe/Bucharest.
Each timer adds a random delay of up to 10 minutes.
Every timer but the leads send and sync is persistent, so a run missed while the server was down happens at the next boot.

| Timer | When | Runs |
| --- | --- | --- |
| `leadhound-ats.timer` | 01:00 | `collect ats`, then `score --max-duration 2h` |
| `leadhound-backup.timer` | 03:30 | `pg_dump -Fc` into `/var/backups/leadhound/`, newest 14 kept within 4 GiB together |
| `leadhound-collect.timer` | 06:30 and 14:30 | `collect linkedin`, then `score --max-duration 45m` |
| `leadhound-prebrief.timer` | 07:30 | `canary linkedin` (once a day; a second start the same day does nothing) |
| `leadhound-brief.timer` | 07:50 | `brief --send`, retried twice, 10 minutes apart, if the send fails |
| `leadhound-trim.timer` | 15:00 | `trim --max-duration 45m`, under `timeout 50m`: see [The trim](#the-trim) |

The five leads timers are installed on every deploy but run only when `/etc/leadhound/env` switches them on: see [Leads on the server](#leads-on-the-server).

The console, `leadhound-console.service`, has no timer: it runs all the time once it is configured, and restarts five seconds after a crash, but never after bad config (exit 2).
See [Console](#console).
`leadhound-preview.service` and `leadhound-fetch.service` have no timer either: the console's Run now card starts them through their watchers, see [Run now](#run-now).

Every command exits 0 once it has recorded its outcome, a stop such as `throttled`, `budget`, `parser_broke` or `spend_cap` included, because the brief reports it.
A non-zero exit means a crash or bad configuration.
In a unit with several steps every step but the last is written `ExecStart=-…`, so a crash in one never stops the next.
The two `score` steps take a lock in the data directory, so a long night run and the morning one never score side by side.

Every step that reaches LinkedIn (`collect linkedin`, `canary linkedin` and `brief --send`, whose recheck does) holds one shared lock, `/opt/leadhound/data/.linkedin.lock`, through `flock --wait <N> --conflict-exit-code 75`:

| Step | Waits up to | When the wait runs out |
| --- | --- | --- |
| `collect linkedin` | 30 minutes | skipped (`ExecStart=-`), `score` still runs |
| `canary linkedin` | 45 minutes | the unit fails; the brief reports the canary as missing |
| `brief --send` | 30 minutes | exit 75, a failure, so `Restart=` retries it |

The lock timeout exits 75 rather than flock's default 1 so it is never confused with the program's own codes, and in particular never with 2, which `RestartPreventExitStatus=2` keeps for bad config.
The brief is ordered `After=` the collect and the canary, and the canary `After=` the collect, so a persistent catch-up after downtime runs them in that order.
`collect ats` runs under `timeout 4h` and `collect linkedin` under `timeout 2h`, so a hang never stops the `score` after it.
Each `score` stops itself cleanly at its `--max-duration` budget, and runs under a `timeout` that ends a score that hangs (`140m` for the night's 2h, `1h` for the 45 minutes).
The timeout sits 20 and 15 minutes over the budget because a stopped score still finishes its in-flight job, records the run and makes its summary pass over every stored job, which reads every description when a description rule is active.
Each unit's `TimeoutStartSec` is longer than its lock waits and timeouts together: 8h15min for the ATS unit (4h05 + 1h30 + 2h22 = 7h57) and 5h30min for the LinkedIn one (30 minutes + 2h05 + 1h30 + 1h02 = 5h07).

The server is shared with other services, so every unit but the backup has a `MemoryMax`: past it the kernel kills leadhound, never a neighbour.
No unit has `MemoryHigh`, because a unit throttled there does not fail but hangs, every request of the console included; a killed one shows in the journal, and the console restarts five seconds later.
Each one also sets `GOMEMLIMIT` below its ceiling, so Go collects garbage harder before the kill:

| Service | `MemoryMax` | `GOMEMLIMIT` |
| --- | --- | --- |
| `leadhound-console` | 300M | 200MiB |
| `leadhound-brief`, `leadhound-prebrief`, `leadhound-preview`, `leadhound-fetch` | 600M | 450MiB |
| `leadhound-collect` | 1G | 850MiB |
| `leadhound-ats` | 1200M | 900MiB |
| `leadhound-trim` | 300M | 200MiB |
| `leadhound-leads-*` | 600M | 450MiB |

A LinkedIn collect has peaked near 530 MB and is not streamed, and the ATS run's peak has not been measured, so those two keep the highest ceilings.
The two collect units and the five leads units also run at `CPUWeight=50`.

### Run now

The console's dashboard has a Run now card, so you can see results before the 07:50 email without an ssh session.
The console asks and systemd runs: the console stays in its sandbox and never starts a unit itself.
A button writes the request time to a file in `/opt/leadhound/data/run/`, and one `.path` watcher per action sees the write (`PathChanged=`) and starts its one fixed unit:

| Button | Request file | Watcher | Starts | What runs |
| --- | --- | --- | --- | --- |
| Show results now | `run/preview` | `leadhound-preview.path` | `leadhound-preview.service` | `preview` under the LinkedIn lock: the brief's jobs part, rechecked and ranked, written to `preview.json` |
| Fetch new jobs, then show | `run/fetch` | `leadhound-fetch.path` | `leadhound-fetch.service` | `leadhound-collect.service` whole (`Wants=`, `After=`), then `preview --after-fetch` |
| Send today's email now | `run/brief` | `leadhound-brief.path` | `leadhound-brief.service` | `brief --send`, early; the 07:50 timer then finds the day sent |
| Run the nightly ATS collect now | `run/ats` | `leadhound-ats.path` | `leadhound-ats.service` | `collect ats`, then `score` |

Each watcher has the same name as its service, so it can start nothing else, and the console can only write the four request times.
The preview never emails, never writes the day's brief and never shortlists; its recheck refreshes listing status as the brief's would.
The fetch pulls in the timed collect unit itself, so a collect already running is joined, not repeated, and the preview starts once it has finished.
The brief and ATS buttons start the timed units unchanged, so a run from the console is the timed run.
A press while the same service is starting or running would be merged into that run by systemd, so the console refuses it and says when the running one started.
`PathChanged=` fires when the file is closed after a write, not while it exists, so the request files are never deleted, and arming a watcher on a deploy never starts its service.

Check the watchers with `systemctl list-units 'leadhound-*.path'`: each of the six, these four, the mailbox check's `leadhound-mailbox-check.path` (see [Mailboxes](#mailboxes)) and `leadhound-leads-research.path` (see [Contact research](#contact-research)), should be `active` and `waiting`; check a run with a run with `journalctl -u leadhound-preview.service` (or `leadhound-fetch`, `leadhound-brief`, `leadhound-ats`).
The same preview runs by hand with `make remote HOST=… ARGS='preview' CONFIRM=1`, which takes the LinkedIn lock as the unit does.

**A watcher stops when its service hits its start limit.**
`man systemd.path` says the start-limit error of the service is propagated to the path unit, which fails too and no longer watches its path until it is restarted.
Only the brief has a start limit (`StartLimitBurst=3` in 4 hours, its two retries included), and a start from the console counts toward it, so a press after two failed tries can trip it; the same holds for a watcher's own `TriggerLimitBurst=` (200 triggers in 2 seconds by default).
The console then shows the action as stuck: asked and not started.
Reset both units and arm the watcher again, here for the brief:

```sh
sudo systemctl reset-failed leadhound-brief.path leadhound-brief.service
sudo systemctl start leadhound-brief.path
```

## Layout on the server

| Path | Owner, mode | What |
| --- | --- | --- |
| `/opt/leadhound/bin/leadhound` | root, 0755 | the binary |
| `/opt/leadhound/bin/leadhound-backup` | root, 0755 | `deploy/leadhound-backup.sh` |
| `/opt/leadhound/data/` | leadhound, 0750 | `LEADHOUND_DATA_DIR`: `leadhound.yaml`, `ats-boards.yaml`, `leads.yaml`, `agencies.yaml` `leads-templates/` and `campaigns/` (root:leadhound 0640, from `make deploy-data`; see [Campaigns on the server](#campaigns-on-the-server)), the outbox of a brief that is not sent, `preview.json` (the latest Run now results) |
| `/opt/leadhound/data/run/` | leadhound, 0700 | the console's Run now requests, its mailbox check requests (`run/mailbox-check`) and its contact research requests (`run/leads-research`), one file per action, read by the `leadhound-*.path` watchers |
| `/etc/leadhound/env` | root, 0600, enforced | the units' environment (`deploy/env.example`) |
| `/etc/leadhound/mailbox.key` | root, 0600 | the mailboxes' private key, made by the first deploy; see [Mailboxes](#mailboxes) |
| `/etc/leadhound/anthropic.key` | root, 0600 | the Anthropic API key, placed by hand; see [The Anthropic key](#the-anthropic-key) |
| `/etc/leadhound/migrate.env` | root, 0600, enforced | the owner role's URL, for `migrate` only (`deploy/migrate.env.example`) |
| `/etc/systemd/system/leadhound-*` | root, 0644 | the units and timers, the Run now watchers, the console's included |
| `/var/backups/leadhound/` | postgres, 0700 | the nightly dumps |

`deploy/install.sh` sets both env files to `root:root 0600` on every deploy, before anything reads them.

## One-time bootstrap

Run these on the server as root, in order.

1. Create the user and the directories:

   ```sh
   useradd --system --home-dir /opt/leadhound --shell /usr/sbin/nologin leadhound
   install -d -o root -g root -m 0755 /opt/leadhound /opt/leadhound/bin
   install -d -o leadhound -g leadhound -m 0750 /opt/leadhound/data
   install -d -o root -g root -m 0700 /etc/leadhound
   ```

2. Install Postgres if the server has none (for example `apt install postgresql`).
   The backup runs as the `postgres` user over the local socket, so Postgres must be on this server.

3. Create the roles and the database, as the Postgres superuser, from a copy of this repository's `deploy/sql/`:

   ```sh
   sudo -u postgres psql -v ON_ERROR_STOP=1 -d postgres -f deploy/sql/roles.sql
   sudo -u postgres psql -v ON_ERROR_STOP=1 -d postgres -f deploy/sql/limits.sql
   sudo -u postgres psql -v ON_ERROR_STOP=1 -d postgres -c "CREATE DATABASE leadhound OWNER leadhound_owner"
   sudo -u postgres psql -v ON_ERROR_STOP=1 -d leadhound -f deploy/sql/grants.sql
   ```

   `limits.sql` sets the per-role limits described under Everyday; every deploy applies it again.
   Then give both roles a password generated on the server (`openssl rand -hex 32`), set with `ALTER ROLE … PASSWORD` in `psql`, never on a command line that lands in a shell history.

4. Write `/etc/leadhound/env` from `deploy/env.example` and `/etc/leadhound/migrate.env` from `deploy/migrate.env.example`, both `root:root 0600`.
   The env file sets `LEADHOUND_LIVE=1`.

5. From your machine, deploy (this checks, builds, copies, migrates and enables the timers):

   ```sh
   make deploy HOST=<ssh host> ARCH=arm64 CONFIRM=1   # or ARCH=amd64
   make deploy-data HOST=<ssh host> CONFIRM=1
   ```

   The ssh user needs `sudo` on the server.

6. Fill the board registry, which lives in the database, so the server's starts empty.
   Copy your board lists to a temporary directory on the server that the `leadhound` user can read, and import each one, for example:

   ```sh
   make remote HOST=<ssh host> CONFIRM=1 ARGS='boards import /tmp/boards/<list>.csv --origin <origin>'
   ```

   Delete the temporary copies afterwards.
   Boards in `ats-boards.yaml` are collected even when the registry does not hold them.

7. First manual runs, before the timers take over:

   ```sh
   make remote HOST=<ssh host> CONFIRM=1 ARGS='canary linkedin'
   make remote HOST=<ssh host> CONFIRM=1 ARGS='collect linkedin'
   make remote HOST=<ssh host> CONFIRM=1 ARGS='brief'
   ```

   `brief` without `--send` writes the brief to the outbox and sends nothing, but its recheck still reaches LinkedIn, so it needs `CONFIRM=1` too.
   Then check the units on the server: `systemd-analyze verify /etc/systemd/system/leadhound-*` and `systemctl list-timers 'leadhound-*'`.

## Everyday

- `make deploy HOST=… [ARCH=…] CONFIRM=1` ships a new release: it runs `make gate` (check, the integration and scale tests, vet with their tags, the deploy tests), builds a static Linux binary, copies it with `deploy/install.sh`, the backup script and the units, runs `migrate` with the new binary as the owner role, then puts the new binary and units in place, reloads systemd and enables the timers and the Run now watchers.
  When the gate already passed on this commit and the tree is clean (`git status --porcelain` empty, untracked files included), it prints `deploy: the gate passed on <hash>; not running it again` and goes straight to the build; any change, or a new commit, runs the gate again.
  A failed `migrate` stops the deploy with a non-zero exit and leaves the old binary and units running; the release stays in its temporary directory on the server.
  Once the console is configured, the deploy also restarts it on the new binary and waits up to 10 seconds for it to answer; if it does not, the deploy prints its last log lines and exits non-zero, with the timers already in place.
- Every deploy re-applies `deploy/sql/limits.sql` as the Postgres superuser before it migrates; if that fails, the deploy stops with the old release running.
  Postgres runs a query's work in its own service, so the units' CPU and memory settings cannot bound it; these per-role limits are what does:
  - `leadhound_app` statements stop after 3 minutes, so no single query can hold the shared database for long; a command that needs longer sets its own bound for one session.
  - `leadhound_app` transactions left idle are ended after 10 minutes, which is above the longest an AI call holds the usage transaction open (about a minute), so a billed call's usage row is never rolled back.
  - Both roles run without parallel workers, so one leadhound query uses one CPU.
  - Both roles run without JIT, which only adds compile time to the queries leadhound runs.
  - `leadhound_app` sessions may write at most 512 MB of temp files, so a runaway sort or hash cannot fill the disk.
  - `leadhound_app` may hold at most 32 connections: four per pool for every process that can run at once, at worst the console, the ATS run, the LinkedIn collect and the brief with the leads contact search, send and sync at 10:00 (28).
    The 15:00 trim can overlap the afternoon collect, the leads probe, send and sync, and the console (24).
  - `leadhound_owner` has no statement timeout, so a migration's index build may take as long as it needs.
  Locally `make db-local` applies the same file, then lifts the connection limit so parallel test runs can share the role.
- `make deploy-data HOST=… CONFIRM=1` copies `leadhound.yaml`, `ats-boards.yaml`, `leads.yaml`, `agencies.yaml`, the `leads-templates/` directory and, when there is one, the `campaigns/` directory from `LEADHOUND_PRIVATE_DIR` (default `../job-hunt/leadhound`), after `score --check-config`, `leads check-config` and (with a `campaigns/`) `leads campaigns check` pass on them; run it after you edit any of them.
  The first five must exist, or it stops before copying anything; a template or campaign file you deleted locally is deleted on the server too.
  Without a local `campaigns/`, the server's campaigns are left as they are.
  For a first `leads.yaml`, start from `examples/leads.yaml` (a demo with reserved names) and set your own sender, addresses and templates.
  It copies nothing else: the title screen is part of `leadhound.yaml`, and the eval golden set, the privacy denylist and the board seed lists are never needed on the server.
  After copying `campaigns/` it prints the `leads campaigns register` line to run next, as [Campaigns on the server](#campaigns-on-the-server) says; it does not register for you.
- `make remote HOST=… ARGS='track list'` runs one command on the server as the `leadhound` user with the units' environment.
  `track list`, `rules list`, `boards list`, `leads suppress list|check`, `leads batch show` and `leads mailboxes list` run freely; every other command writes or reaches the network (`collect`, `canary`, `brief`, `score`, `migrate`, `recheck`, `probe`, `track <n> …`, `rules add`, `boards import` …) and needs `CONFIRM=1`.
  `ARGS` is split into words by your own shell, as on a command line, and each word reaches `leadhound` whole: the server's shell never reinterprets it.
- `remote` runs without the units' hardening, memory ceiling and timeouts.
  It does take the units' locks, without waiting: `collect linkedin`, `canary`, `brief` and `preview` take the LinkedIn lock and `score` the score lock, and when a timer's run holds one, `remote` exits 75 and runs nothing; try again when it has finished (`systemctl list-units 'leadhound-*'`).
  Even so, prefer to run collects and briefs by hand only while no timer is due.
- `remote` starts each run with `systemd-run -p EnvironmentFile=/etc/leadhound/env`, so it reads the env file afresh every time; a running unit, the console's included, reads it only when it starts.
- On the server, a `systemctl` line that starts, stops, enables, disables or restarts a unit needs root, so this file writes it with `sudo`; `systemctl list-units`, `list-timers` and `journalctl` read only and need no `sudo`.
- `deploy`, `deploy-data` and `remote` run their `sudo` steps over `ssh -t`, so `sudo` can ask for its password at your terminal.
  Run without a terminal (from a script), ssh warns that it allocates none, and the steps then need `sudo` without a password for the ssh user.
- `deploy`, `deploy-data` and `forget` refuse to run, even as `make -n`, without `HOST` and `CONFIRM=1`; `remote` refuses without `HOST`, and without `CONFIRM=1` for anything but the read-only lists.
- The brief's LinkedIn recheck runs before the day's `brief_log` row is written, so a brief that crashes in between rechecks again on its retry; the three-try limit bounds that.

## Leads on the server

Five timers run the leads; all times are Europe/Bucharest, each with up to 10 minutes of random delay.
Every leads unit has the same sandbox as the other timed units, `MemoryMax=600M` with `GOMEMLIMIT=450MiB`, `Nice=10`, idle I/O and `CPUWeight=50`, and each command runs under a `timeout` that `TimeoutStartSec` covers.
Business hours, spacing, daily caps, the warm-up ramp and the pause are the commands' own (`leads.yaml` and the console), never the units'.

| Timer | When | Runs | Switched on by |
| --- | --- | --- | --- |
| `leadhound-leads-probe.timer` | 13:00 | `leads sites probe --max 10000`, under `timeout 3h`, after waiting up to 195 minutes for the fetch lock; `TimeoutStartSec=390min` | `LEADHOUND_LEADS_TIMERS=on` |
| `leadhound-leads-contacts.timer` | 10:00 | `leads contacts --max 300`, under `timeout 2h`, then `leads enrich --max 300`, under `timeout 60m`, then `leads rate --max 10000`, under `timeout 180m`, then `leads build`, under `timeout 20m`; `TimeoutStartSec=7h` | `LEADHOUND_LEADS_TIMERS=on` |
| `leadhound-leads-batch.timer` | 06:15 | `leads build`, then `leads batch new`, each under `timeout 20m`, for the `drupal-eol` campaign; it holds the [Anthropic key](#the-anthropic-key) because Claude drafts at batch time | sending on |
| `leadhound-leads-send.timer` | hourly 10:00 to 19:00, Monday to Friday, not persistent | `leads send --max 25`, under `timeout 225m` | sending on |
| `leadhound-leads-sync.timer` | every 15 minutes, not persistent | `leads sync`, under `timeout 10m` | sending on |

- The probe and the contact search are the finding timers.
  They start unattended reads of public sites: up to 10,000 homepages a day, and up to 300 organisations searched for a published contact a day, each site's robots file read and honoured first.
  They share a fetch lock in the data directory, `/opt/leadhound/data/.leads-fetch.lock`.
  The contact search and the enrichment do not wait for it: a step that finds it held exits 75, which the contacts unit counts as done.
  The probe waits for it, up to 11700 seconds (195 minutes), because the contacts unit can hold it for 190 minutes, until about 13:20, after the probe's timer has fired; a wait that runs out exits 75 and fails the probe unit, so a lost probe shows in `systemctl`.
- After the contact search, the contacts unit enriches organisations, rates leads for relevance, then runs `leads build`, which turns every organisation with a finding into a lead and reads no web page, so the console's Leads pages fill while only finding is on; the batch rebuilds the list again before drafting.
  Its steps, from `leadhound-leads-contacts.service`:

  ```ini
  ExecStart=-/usr/bin/flock --nonblock --conflict-exit-code 75 /opt/leadhound/data/.leads-fetch.lock /usr/bin/timeout --kill-after=5m 2h /opt/leadhound/bin/leadhound leads contacts --max 300
  ExecStart=-/usr/bin/flock --nonblock --conflict-exit-code 75 /opt/leadhound/data/.leads-fetch.lock /usr/bin/timeout --kill-after=5m 60m /opt/leadhound/bin/leadhound leads enrich --max 300
  ExecStart=-/usr/bin/timeout --kill-after=5m 180m /opt/leadhound/bin/leadhound leads rate --max 10000
  ExecStart=/usr/bin/timeout --kill-after=5m 20m /opt/leadhound/bin/leadhound leads build
  SuccessExitStatus=75
  TimeoutStartSec=7h
  ```

  Every step but the build is `ExecStart=-`, so a failed search, enrichment or rating never stops the build.
  The rating and the build read no web page, so they run outside the fetch lock.
  `TimeoutStartSec=7h` is longer than the four timeouts and their kill graces together (6h40).
- The batch drafts the next batch for you to read and approve in the console; while a batch waits, `leads batch new` makes none.
  While your saved settings no longer fit `leads.yaml` (so they are not applied), `leads batch new` makes none and exits 2, so the batch unit shows as failed in `systemctl`; fix or reset them in Leads → Settings.
  The send sends only approved messages, each in its recipient's business hours (the send hours cover the working day in the UK, Ireland and central Europe), and the sync reads every mailbox that is not retired for replies, opt-outs and bounces.
  When one mailbox cannot be read, the others still sync and `leads sync` exits 1, so the sync unit shows as failed in `systemctl` and the journal names the mailbox.
  `leads send --max 25` is the most sent in one run over all mailboxes together; with several mailboxes, raise it in `leadhound-leads-send.service`.
  Send and sync take no file lock: their database locks keep them apart.
  The send and sync timers are not persistent, so a run missed while the server was down is never caught up outside its hours.
- No leads unit reads `/etc/leadhound/migrate.env`; they hold only the app role.

Every deploy installs the units and decides the timers from `/etc/leadhound/env`, the last line for each name counting, as for the units:

- With `LEADHOUND_LEADS_TIMERS=on`, the probe and contacts timers are enabled; otherwise they are disabled and stopped.
- With that, `LEADHOUND_LEADS_SEND=on` and `LEADHOUND_OUTREACH=live`, the batch, send and sync timers are enabled; otherwise they are disabled and stopped, so the server never drafts a shadow batch.
  The mailboxes live in the database, not in this file: a run with no ready mailbox sends nothing and says so.
- The deploy prints which leads timers are on, and for those that are off, which lines are missing.

To turn finding on, add `LEADHOUND_LEADS_TIMERS=on` to `/etc/leadhound/env` and run `sudo systemctl enable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer` (or deploy).
To turn sending on, first add a mailbox and send its test, as [Mailboxes](#mailboxes) says; then, with `LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live` already in the file, add `LEADHOUND_LEADS_SEND=on`, run `sudo systemctl enable --now leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer`, and restart the console so it reads the new settings: `sudo systemctl restart leadhound-console.service`.
Without `LEADHOUND_LEADS_TIMERS=on` the next deploy disables the sending timers again, even with the other two lines set.
A unit reads the env file only when it starts.

To pause sending, use the pause switch on the console's Leads page: the send timer keeps running and sends nothing until you resume, and nothing on the server changes.
To stop everything, remove the two switch lines (or set them to anything but `on`) and run `sudo systemctl disable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer`; the next deploy leaves them off.
A run already in progress finishes; `sudo systemctl stop leadhound-leads-<name>.service` ends it at once.

### Enrichment

After the contact search, the contacts unit runs `leads enrich --max 300`, under `timeout 60m` and the same fetch lock, before `leads rate` and `leads build`.
It takes up to 300 organisations with a lead that were not enriched in the last 30 days, newest lead first.
It reads phone numbers, a description and links to social profiles from the pages already kept for each one: its homepage and the pages a contact route was read from.
When none of those is a contact page, it fetches at most two contact links from the homepage, under the contact search's rules: robots.txt on the link and on the final URL, a 2xx answer on the organisation's own domain, public addresses only, and the same rate and budgets.
A fetched page keeps its body only when it gave a new fact.
A phone is a fact, never a contact route, so drafting and sending never use it.
A LinkedIn link is kept only for a company page, and no fact ever holds an email address.
The run prints one line, `orgs N · phones N · descriptions N · social N · requests N · stop <reason>`, where the counts are new facts and the stop is `none` or `disk_budget`.
An enrichment cut short (no answer, a breaker stop, a cancel) records nothing, so that organisation is tried again on the next run.
It needs `LEADHOUND_LIVE=1`, like the contact search; `leads enrich --dry-run --fixtures DIR` reads fixtures and only organisations on reserved names.
`make remote … ARGS='leads enrich …'` does not take the fetch lock, so run it by hand only while the contacts unit is not running.

### Lead kinds and findings

A lead points at one finding: a row that says what a detector saw that makes an organisation worth contacting, with the source row it was read from and its evidence.
The kinds and the channels are rows in `lead_kind` and `lead_channel`, not checks on `lead`, and they change only by migration: the app role can read them, never write them.
Today's kinds are `unsupported` (Drupal 6 to 9), `d10` (Drupal 10) and `agency_partner`, which batches may draft, and `selftest` and `hand`, which they never draft.
`finding` is append-only, and a source row gives one finding however often a build runs.
Later kinds are designed, not built, and each needs one `INSERT INTO lead_kind` and its own finding source, never a change to `lead`:

- A job posting for a role that could be automated: kind `automation`, detector `jobad@1`, source table `job`, subject `{job_id, roles, systems}`.
- A shop with broken tracking: detector `tracking@1` on a site fetch, subject `{platform, pixels, gap}`.
- A new company: detector `registry@1`, subject `{register, number, incorporated}`.
- A business with no website: the `domain` column of `org` becomes nullable with an `org_ref (source, ref)` unique key, in that plan's own migration; `lead` is not altered.

`org.site_id` has no unique key yet: today's writers never give two organisations the same site, and if one did, the migration or the build would fail loudly rather than mix them up.

### Relevance

The contacts unit runs `leads rate --max 10000`, under `timeout 180m`, after the enrichment and outside the fetch lock.
It asks Jev one question about every open lead whose facts changed since its last rating and stores the answer as a number from 0 to 100, so a day's run after the first is small.

- It needs `LEADHOUND_LIVE=1` and the Jev key, `TYPESAFE_API_KEY`, in `/etc/leadhound/env`.
  Without the live line it requests nothing and exits 0; without the key it refuses.
  Either way it rates nothing, and the build after it still runs, because the step is `ExecStart=-`.
- While rating is off in Leads → Settings it refuses at once, and the build runs next.
- Its calls count against `LEADHOUND_JEV_LEADS_DAILY_CALL_CAP` (10,000 when left out), kept apart from the jobs' `LEADHOUND_JEV_DAILY_CALL_CAP`, so scoring jobs keeps its full cap whatever leads do.
  The cap is a guard against a runaway loop, not a budget; the Settings value may lower it, never raise it, and a higher one is refused with "raise it in .env first".
- One rating runs at a time, through a database lock; a second says so and exits 0.
- A rating stopped by `--max`, the cap, an unavailable provider or five failed calls in a row is recorded as a `leads-rate` run and exits 0.

The number orders the list only once it passes its trust gate: an eval with `--decider jev` on at least 30 of your own calls ("Your call" on a lead's page, listed under To label) that is within one band of you on at least 80% of them, with Spearman's rho at least 0.5.
Until then the list and the batches order by the fallback order's points, and starred leads always come first.
Run the eval by hand once you have labelled the leads: `make remote HOST=… CONFIRM=1 ARGS='leads relevance eval --decider jev'`.
It says how many calls it will make before the first, is not capped, and is recorded only with 30 or more labelled leads; the latest recorded eval decides.

### Settings in the console

The console's Settings page (`/leads/settings`) changes some leads settings over `leads.yaml`, in the database: who to contact, the kinds and their order, small sites, where batches draw from, the Drupal end-of-life dates, relevance rating and its daily calls, and the fallback order.
The commands that use these settings read them from the database on every run, with the file underneath, so a change reaches the next run of each unit without a deploy or a restart.
The sending pace and ramp, the sender, the country channels and languages, and the templates show read-only: the send path reads them from the file, so change them in `leads.yaml` and run `make deploy-data`.
No secret is shown or stored.
`leads settings show` prints each setting in force and where it comes from, and `leads settings reset [--section …]` puts a section, or all of them, back to the file; on the server both run through `make remote … CONFIRM=1`.

### Campaigns on the server

A campaign is a folder in the data directory, `/opt/leadhound/data/campaigns/<slug>/`, holding `campaign.yaml` and its `templates/`; start from `examples/campaigns/` and make it your own in your private directory.
`make deploy-data` runs `leads campaigns check` on your private `campaigns/` and copies it, with the same owner and modes it gives the templates (directories `root:leadhound 0750`, files `root:leadhound 0640`), removing a campaign file you deleted locally.
The local check reads no database, so it refuses a `writer: claude` campaign (the draft gate lives in the server's database); keep `writer: template` in the private copy until that is solved.
Then register it right away, as `deploy-data` reminds you:

```sh
make remote HOST=<ssh host> CONFIRM=1 ARGS='leads campaigns register'
```

A data directory that holds a campaign file with no registered version, or one changed since it was registered, refuses `leads batch new`, so the morning batch unit fails until you register.
Run `register` after every change to a campaign file.
Without a folder, `drupal-eol` drafts from `leads.yaml` and its templates as before, and the batch never asks for a registration.
A data directory whose campaign lists a call-first country (none in the table since 29 Oct 2026) needs that campaign's `call_first/` templates for each kind and language, or `register` and the batch refuse and name the missing files.
`make remote HOST=<ssh host> ARGS='leads campaigns list' CONFIRM=1` shows what is registered.

### Turning the leads on in production

Production has never built a lead, so take these steps in order:

1. Place the Anthropic key first, as [The Anthropic key](#the-anthropic-key) says: from the release with campaigns on, the batch and contact research units fail to start without it.
2. Deploy: `make deploy` migrates the core set up to 00009 and the leads set up to 00021, with the new binary, before it puts that binary in place; a failed migration leaves the old release running.
3. `make deploy-data` (it copies `campaigns/` too), then `leads campaigns register`, as [Campaigns on the server](#campaigns-on-the-server) says.
4. Suppress the addresses and domains of people you already know (`leads suppress add address|domain VALUE`), before any finding.
5. Add `LEADHOUND_LEADS_TIMERS=on` to `/etc/leadhound/env` to turn finding on.
6. Check the first contacts run: it searches for contacts, enriches, rates and builds, and the console's List fills.
   To start it before 10:00, run `sudo systemctl start leadhound-leads-contacts.service`.
7. Rating is on by default and runs inside that unit once `LEADHOUND_LIVE=1` and `TYPESAFE_API_KEY` are in the env file; turn it off in Settings if you do not want it.
   Then label 30 leads under To label and run the eval, as [Relevance](#relevance) says.
8. Before any campaign with a call step is batched: write your privacy notice, set `sender.privacy_url` in `leads.yaml` and run `make deploy-data` (the deadline is 7 Nov), and screen the Great Britain numbers against TPS and CTPS before the first GB call, as [Calls, screening and phone research](#calls-screening-and-phone-research) says.
9. Add a mailbox, send its test and turn sending on, as [Mailboxes](#mailboxes) and the steps above say.

## Mailboxes

A mailbox is one email account the leads are sent from: an address, its login and password, its sending (SMTP) and reading (IMAP) servers, and its DKIM selector.
You can have as many as you like, each with its own warm-up, and you add, check, pause, resume and retire them on the console's Mailboxes page (in the top menu).
The same verbs run from the command line: `leadhound leads mailboxes list | add | check | pause | resume | retire`; editing a mailbox is in the console only.
On the server, add mailboxes through the console: `make remote` hands the command a pipe, not a terminal, so `leads mailboxes add` could not ask for the password without showing it; keep the command-line add for local use.
A new lead goes to the ready mailbox with the most room left, and its follow-ups always go from the same mailbox, in the same thread.
Every batch shows each message's From address before you approve it.

### The key

Each mailbox password is sealed the moment you enter it, with a public key, and only the jobs that send, sync and check a mailbox hold the private key that opens it.
The console never has the private key: it can store a new password but never read one back, so a console that was broken into still cannot give a password away.
The console and the jobs share one database login, so someone who takes over the console can point a mailbox at another server with a password of their own, but can never read a password already saved.
Every such change of login, server or selector is logged with the old and new values; a console edit lands in `journalctl -u leadhound-console.service`.
Someone with a shell as the service user is outside what the key protects against.

- The first deploy makes the key pair, after the new binary is in place: the private key at `/etc/leadhound/mailbox.key` (`root:root 0600`), and the public key as the `LEADHOUND_MAILBOX_PUBLIC_KEY=` line in `/etc/leadhound/env`.
  That deploy prints `install: new mailbox key at /etc/leadhound/mailbox.key, fingerprint …`; later deploys print no fingerprint, because they never make a new key.
- An existing key is never overwritten.
  Every deploy reads the public key from the key file again and rewrites the env line if it differs, so the two always match; leave the line empty in your copy of `deploy/env.example`.
- The key reaches only the send, sync and mailbox check units, as a systemd credential (`LoadCredential=`).
  Every other unit, the console's included, has `/run/credentials` walled off, except the units that hold the [Anthropic key](#the-anthropic-key), which never get the mailbox key either.
  `make remote` hands it on the same way, and only to `leads selftest`, `leads send`, `leads sync`, `leads mailboxes check` and `leads suppress review`.
- `LEADHOUND_MAILBOX_KEY_FILE` is for local runs only; never set it on the server.
  The console refuses to start when it is set to anything (the empty line in `deploy/env.example` is fine).
- **Back the key up by hand.**
  The nightly backup dumps the database, not `/etc`, and a sealed password cannot be opened without the key that sealed it.
  Copy `/etc/leadhound/mailbox.key`, as root, to somewhere safe off the server, such as your password manager, and never into a repository.
  To restore it, put it back at the same path before the next deploy, which makes it `root:root 0600` again.
- If the key is lost, the next deploy makes a new one, and each mailbox pauses itself the first time a job opens its password, with "re-enter the password: it was saved under a different key".
  Open each mailbox, choose Edit its settings, enter its password again, save, and resume it.

### Adding your first mailbox

Before you start: the leads settings are on the server (`make deploy-data`), the console is set up (see [Console](#console)), the check's watcher is armed (`systemctl list-units 'leadhound-mailbox-check.path'` shows it `active` and `waiting`; every deploy arms it), and you have the mailbox's address and password from your mail provider.

1. In the console, open Mailboxes and follow the Add a mailbox link.
   If the page says "the mailbox key is not set up", the deploy has not made the key yet: deploy again and read its `install:` lines.
2. Under Provider, choose your provider's preset in Fill in the servers for, and press Fill in: it fills the sending server, the reading server and the DKIM selector.
   If your provider has no preset, choose Other (it fills nothing) and type them yourself: the sending server as `host:port` with port 465 or 587, the reading server with port 993, and the DKIM selector as the name before `._domainkey` in your domain's DKIM record.
3. Type the address and the password.
   Leave Login blank when you sign in with the address; leave From name blank to use the sender name in `leads.yaml`.
   Some providers need an app password instead of your normal one.
4. Press Add and check it.
   The mailbox page opens with a Check card that says "Checking…" and updates by itself, usually within a minute.
   The check signs in to both servers, looks for the Sent and Junk folders and reads the domain's records; it never sends an email.
   On the server, `leadhound-mailbox-check.path` starts the check job when the console asks; locally nothing watches for that, so run `make mailbox-check-local` (see below).
5. The card shows one tick or cross per step, and under a cross the first thing to fix:

| Line on the card | What a cross means | What to do |
| --- | --- | --- |
| SMTP sign-in | the sending server could not be reached, refused the login, or its certificate did not check out | check the server name and port and the password (or app password) |
| IMAP sign-in | the same, for the reading server | the same |
| Sent and Junk folders | signed in, but the Sent or Junk folder was not found, so replies sorted into Junk would be missed | turn on or create those folders with your provider |
| SPF, DKIM, DMARC | that DNS record is missing for the address's domain | add the record the fix names, with your DNS provider, then press Check now |

The three records are TXT records in your domain's DNS that tell receiving servers your mail is genuine: SPF lists who may send for the domain, DKIM publishes the key your provider signs each message with, and DMARC says what to do with mail that fails the other two.
Your mail provider's own help pages give the exact values for its service.

"Could not run" means the password could not be used, for example after a key change or after the servers changed since it was saved; the fix says to enter the password again.
Press Check now whenever you have fixed something.
A check that has not started five minutes after it was asked for says "The check has not started"; press Check now to ask again.

Locally, the same add is `leadhound leads mailboxes add --address A --preset NAME` (or `--smtp H:P --imap H:P --selector S`), which asks for the password twice at the terminal without showing it, and never takes it from the command line; `leads mailboxes check ID` runs a check at once.
`leads mailboxes list` shows each mailbox's number, state, latest check, today's records and today's sends:

```
1  outreach@sender.example · active · check passed · DNS today pass · sent today 0
2  hello@sender.example · paused: the server refused the login · check failed: The server refused the login. Check the password (some providers need an app password). · DNS today fail or not run · sent today 0
```

The records are checked on the address's own host (the part after the `@`), with that mailbox's own selector.
DMARC is the one exception: when the host has no DMARC record of its own, the one at the registrable domain counts, as receiving servers read it.
The daily domain limit, the number of mailboxes and the domain pauses count by the registrable domain: an address at `out.sender.example` counts toward `sender.example`.
`leads senders check` checks the records of every mailbox that is not retired, one line per host and selector; with no mailbox it says so and exits 0.

### When a mailbox is given leads and sends

- **New leads** go only to a mailbox that is active, whose latest check passed, and whose records passed today.
  The 06:15 batch checks today's records itself before it drafts.
  The console's New batch never reaches the network, so before 06:15 it says "Today's domain check has not run for … yet": press Check now on the mailbox's page, or wait for the morning batch.
  With no mailbox at all, a batch is refused with "Add a mailbox first".
- **Each send** also needs that mailbox's reply sync to have just succeeded; the send run syncs first.
- A follow-up whose mailbox is paused waits; a retired mailbox's unsent follow-ups are cancelled, since a thread must come from the same address.

### Limits

| Limit | Default | At most | Setting in `leads.yaml` |
| --- | --- | --- | --- |
| Sends per mailbox per day | the warm-up ramp: in `examples/leads.yaml` 5 a day from day 0, 12 from day 7, 25 from day 14 (calendar days); with `caps.start_date: first-send` each mailbox counts from its own first real send, so a new one starts at day 0 (the example has a fixed date instead) | 40 | `ramp` and `start_date` under `caps:` |
| Sends per sending domain per day, over all its mailboxes | 50 | 100 | `caps.domain_per_day` |
| Mailboxes on one sending domain | 2 | 5 | `caps.mailboxes_per_domain` |

The domain limit is the real safety line: the large mail providers judge reputation by the sending domain, not the single inbox, and none publishes a per-inbox number for cold email (the sources are in the README's Campaigns and leads section).
A second mailbox on the same domain spreads the sending, but does not double what the domain may send.
Adding one mailbox more than `caps.mailboxes_per_domain` is refused with a sentence naming the setting.

### Pauses and how to resume

A mailbox pauses itself, and the page says why, when:

- 2 hard bounces (an address that does not exist) fall within its last 50 real sends (`health.bounce_pause` in `leads.yaml`);
- its server refuses the login;
- its password cannot be opened, or was saved for other settings ("re-enter the password: …").

A **policy block**, a receiving server refusing a recipient with a code `5.7.x`, refuses that one email and pauses every active mailbox on its domain.
A `5.7.x` that arrives only after the message was handed over counts as uncertain (`leads resolve`) and does not pause anything.
Spam complaints have no automatic feed at this volume: pause a mailbox or a domain yourself, with a reason.

Only you resume.
On a mailbox's page press Resume this mailbox (or Pause this mailbox, with an optional reason); a domain is paused with Pause every mailbox on <domain> and resumed with Resume <domain> on the Mailboxes page, which resumes only the mailboxes that pause stopped (one you paused on its own stays paused).
From the command line: `leadhound leads mailboxes pause ID|--domain D [--reason R]` and `leadhound leads mailboxes resume ID|--domain D`.
A resumed mailbox sends again once its check has passed and its records have passed today.
Retire this mailbox… on its page (tick I understand, retire it, then press Retire <address>), or `leads mailboxes retire ID --confirm`, retires a mailbox for good; to send from another address, add a new mailbox.
The pause switch on the Leads page stays the master switch above every mailbox.

### Sending a test, then turning sending on

1. Add `LEADHOUND_OUTREACH=live` to `/etc/leadhound/env`, without `LEADHOUND_LEADS_SEND=on` yet, so the sending timers stay off, then deploy or run `sudo systemctl restart leadhound-console.service`.
   The console reads the env file only when it starts, and until it sees live mode it treats sending as not set up and never drafts a batch; `make remote` reads the file afresh on every run, so the next steps need no restart.
2. Find the mailbox's number, then send one test email from it to one of your own addresses, `sender.operator_addresses` in `leads.yaml`:

   ```sh
   make remote HOST=<ssh host> ARGS='leads mailboxes list'
   make remote HOST=<ssh host> CONFIRM=1 ARGS='leads selftest --to <your address> --mailbox <its number>'
   ```

   `--mailbox` takes the number or the address, and is needed when more than one mailbox is not retired.
   The test runs the send's own order first: it stops while the pause switch on the Leads page is on, and it needs the key, a passed check, today's records and a reply sync of that mailbox, or it says "No selftest: mailbox N is not ready to send." and sends nothing.
   It ignores business hours and the daily limit, though its one email counts toward that day's sends, and it does not start the mailbox's warm-up ramp: only a real lead's send does.
   If the server refuses the login, the mailbox pauses itself as in any send, the test email is cancelled, and the command exits 1.
3. Reply to the test email from your own address; the reply lands in the mailbox's inbox, where it stays.
   The sync timer is still off, so read it by hand:

   ```sh
   make remote HOST=<ssh host> CONFIRM=1 ARGS='leads sync'
   ```

   It prints one line per mailbox, the findings indented under it, for example:

   ```
   Mailbox 1: synced.
     1 reply to read in the outreach mailbox: your selftest
   ```

   "Mailbox 1: synced, nothing new." means the reply has not arrived yet; "Mailbox 1: not synced: …" names what failed, and the command exits 1.
   Repeat steps 2 and 3 for each mailbox.
4. Turn sending on as [Leads on the server](#leads-on-the-server) says: `LEADHOUND_LEADS_TIMERS=on`, `LEADHOUND_OUTREACH=live` and `LEADHOUND_LEADS_SEND=on` all in the file, the three timers enabled, and a console restart.
   Every batch still waits for you to read and approve it.

### Locally

1. `make mailbox-key` writes a key pair to `data/mailbox.key` (never overwritten) and prints two lines; add them to your `.env` by hand.
2. `make console-local` serves the console without the key file variable, as on the server.
3. Add the mailbox in the console; then, in a second terminal, `make mailbox-check-local` runs the waiting check.

Without `LEADHOUND_OUTREACH=live`, a send writes each message to the outbox, with the mailbox's address as its From, and nothing leaves your machine.

### Moving from the old env mailbox

The four old lines, `LEADHOUND_SMTP_ADDR`, `LEADHOUND_IMAP_ADDR`, `LEADHOUND_OUTREACH_USER` and `LEADHOUND_OUTREACH_PASSWORD`, are no longer read.
`leads check-config` and the Mailboxes page say so while any is set: add the mailbox in the console, then delete the lines.

If your install sent or synced through the old env mailbox, the migration moves those messages and their read mail to a placeholder mailbox, `legacy@legacy.invalid`, retired, which nothing sends through or reads.
When you add the same account as a mailbox, its first sync reads the last 30 days of its mail again, as a new mailbox's first sync always does.
Old replies, bounces and opt-outs found that way are matched to your old messages and applied again:

- a lead still batched or contacted moves to replied, bounced or opted out, as the old sync did;
- a suppression you removed or lifted since then comes back, and any unsent follow-ups of that lead are cancelled;
- the reply, bounce and opt-out counts for that day can include those old ones, since they are recorded when read.

Nothing is sent because of it, and the new mailbox's bounce pause counts only its own sends.
Look over the Leads page after that first sync; a fresh install has no such rows and sees none of this.

### Opt-outs, suppressions and lifting one

The sync reads a message as an opt-out when its subject or the sender's own words ask to hear no more:

- the subject holds an opt-out word, or is just "stop";
- the sender's own text holds an opt-out word, or is just "stop" once spaces and punctuation are trimmed.

The opt-out words are in English, Romanian, German and Dutch: unsubscribe, remove me, opt out (or opt-out, optout), dezabonare, dezabona, dezabonez, dezabonează, dezabonați, abmelden, abbestellen, austragen and afmelden.
Quoted text does not count: lines starting with `>`, the "On … wrote:" line and its Romanian, German, Dutch and French forms, and an HTML reply's quoted part are taken out first.
Our own lines do not count either: the opt-out line from `leads.yaml` and the selftest's instruction sentence are removed wherever they appear, in any case or quote marks, wrapped or cut off at the end.
A forwarded or Outlook-style quote ("Original Message", "From: … Sent: …") is not thrown away: an opt-out word the sender typed inline under it still counts.
When a reply is in several parts (as Gmail sends them), the first text part is decoded before it is read, so an opt-out written in a base64 or quoted-printable part is caught.
A message that is not an opt-out but answers us is a reply, and stops that lead's follow-ups as any reply does.

Because a reply quotes our email, our own text may hold an opt-out word only in the opt-out line.
The draft check refuses a draft whose subject, or whose text outside the opt-out line, holds one ("the text holds an opt-out word outside the opt-out line"); reword the template.

A suppression stops all writing to its address or domain.
One recorded by mistake can be lifted, by its number; nothing in the app ever lifts one on its own.
A lift is a record, never a delete: the suppression stays, with the lift's date, where it came from and your reason beside it.

- In the console: open Leads, go to Suppressions, and under Lift a suppression type the number from the list, the domain shown in the list (for an address, the part after the `@`) to confirm, and a reason.
- From the command line on the server:

  ```sh
  make remote HOST=<ssh host> CONFIRM=1 ARGS='leads suppress lift 2 --domain example.com --reason "reply quoted our opt-out line"'
  ```

The reason is 3 to 300 characters on one line, with no address in it.
A lift is refused, and nothing changes, when the number does not exist, the domain is not that suppression's, the reason is missing, or the suppression was already lifted.
When it works it says what was lifted and whether another active suppression still blocks the same address or domain.
A lift never changes a lead's state: a lead that read as opted out still does, and the command line says how many such leads there are.
A new opt-out from the same address suppresses it again, as a new suppression.
`leads suppress list` shows the active suppressions, then the lifted ones with their reasons; the console shows the same under Suppressions.

To find past opt-outs that were only quoted text, run the review:

```sh
make remote HOST=<ssh host> CONFIRM=1 ARGS='leads suppress review'
```

It reads each opt-out message again from its mailbox and judges it by the rules above, so it gets the mailbox key and needs `CONFIRM=1`; it only reads, and writes nothing.
It lists first each one that now reads as a reply, with the exact lift line to run, then how many still read as opt-outs and which could not be read again (a renumbered folder, a deleted message, a retired mailbox), and ends with "Nothing was lifted: review and lift by hand."
It prints no lift line for a suppression that another opt-out still leans on ("also covers event …: do not lift"), since lifting it would free an address that really asked to hear no more.

`leads transfer` carries only active suppressions: a lifted one is left behind, and a suppression the target had lifted is recorded there again.

## The Anthropic key

LeadHound calls Claude to draft emails, to find a contact for a starred lead and to find a business phone, so the jobs that do that need an Anthropic API key.
The key is a file on the server, `/etc/leadhound/anthropic.key` (`root:root 0600`), holding the key alone on one line.

- **Place it by hand.**
  Make the key in the Claude Console, in a workspace of its own.
  Then ssh to the server, open the file in your own editor as root (for example `sudo -e /etc/leadhound/anthropic.key`), paste the key, save, and run `sudo chmod 0600 /etc/leadhound/anthropic.key`.
  Never type the key into a command line, where it lands in your shell history, and never paste it into a chat.
- **Who reads it.**
  Only the units that call Claude get it, as a systemd credential (`LoadCredential=anthropic-key:/etc/leadhound/anthropic.key`): `leadhound-leads-batch.service`, because drafting runs at batch time, and `leadhound-leads-research.service` (see [Contact research](#contact-research)).
  No other unit gets it, and never the console; `make remote` hands it on only when you add `REMOTE_CLAUDE=1` (see "Running a Claude command by hand" below).
  This is why it is not a line in `/etc/leadhound/env`: the env file reaches every unit, the console included, and a credential reaches only the units that need it.
- **No `ANTHROPIC_API_KEY` line is read anywhere.**
  LeadHound reads the key only from `$CREDENTIALS_DIRECTORY/anthropic-key` on the server or from the file `LEADHOUND_ANTHROPIC_KEY_FILE` names locally, and never from the environment.
- **The console refuses to start** when it can see the key: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or `LEADHOUND_ANTHROPIC_KEY_FILE` set to anything, or an `anthropic-key` credential handed to it.
  It exits 2, naming what it found, and systemd does not restart it.
- **Every deploy** makes the file `root:root 0600` again if it is there, and never creates, reads or prints it.
  Until you place it, the deploy says `install: no Anthropic key at /etc/leadhound/anthropic.key`, and `leadhound-leads-batch.service` and `leadhound-leads-research.service` cannot start, because systemd fails a unit whose `LoadCredential=` file is missing.
  That holds even for a batch that drafts from templates only and never calls Claude, so place the key before you deploy this release with sending on, or the morning batch stops.
  `ImportCredential=` would let the units start without the file, but it needs systemd 254 or later; the units do not use it yet, so check the server's version (`systemctl --version`) before that change.
- **Locally**, put the key in a file outside the repository, made `0600` (`chmod 0600`), and set `LEADHOUND_ANTHROPIC_KEY_FILE=/path/outside/the/repo/anthropic.key` in your local `.env`.
  A file any other user can read is refused.
  Never set it on the server.
- **To rotate**, make a new key in the Claude Console, replace the file's contents in your editor as above, and start the next run; a running job keeps the key it started with.
  Then delete the old key in the Claude Console.
  No deploy is needed.
- **A second ceiling.**
  Set a monthly spend limit on the key's workspace in the Claude Console, so a bug past LeadHound's own caps still stops at a sum you chose.
- **Every Claude command needs `LEADHOUND_LIVE=1`** as well as the key; without it, it prints one line saying no Claude call was made and exits 0.

**Running a Claude command by hand.**
`leads drafts sample`, `leads drafts eval --decider claude`, `leads phones research` and `leads research run` call Claude, and `make remote` hands the Anthropic key to none of them unless you add `REMOTE_CLAUDE=1`, for example:

```sh
make remote HOST=<ssh host> CONFIRM=1 REMOTE_CLAUDE=1 ARGS='leads phones research --campaign drupal-eol --max 3'
```

`REMOTE_CLAUDE=1` adds `-p LoadCredential=anthropic-key:/etc/leadhound/anthropic.key` to the run, the same credential the batch and research units get.
It refuses, before anything runs, without `CONFIRM=1` (it spends money), for the console (`serve`, `console …`), and for a command that gets the mailbox key (no command holds both keys).

Each prints what it spent; a run stops at the caps below.

### Claude spend caps

Every Claude call is priced before it is made, at its worst case, and refused when that would pass a cap; after the call its real cost is recorded, per lead and per purpose.

- `LEADHOUND_CLAUDE_DAILY_USD_CAP` in `/etc/leadhound/env` (in US dollars) is the ceiling for one day across every purpose.
- Each purpose (drafting, research, and the drafts eval when its decider is Claude) has its own daily cap under that ceiling, and a cap per lead checked against what that lead has already cost under the purpose; the drafting per-lead cap leaves room for the one retry a refused draft gets.
- The ceiling's default and the current per-purpose and per-lead caps, and the drafting writer's output ceiling they are sized against, are in `deploy/env.example`; only the ceiling is set in the env file, the per-purpose and per-lead caps are built in and the same for every campaign, and a campaign spec cannot set its own caps yet.
- A refused call costs nothing and is logged; the dashboard and the Campaigns tab show today's spend against the ceiling.
- A batch that reaches a cap drafts the remaining leads from templates and says how many and why.

## Contact research

Starring a lead that has no usable way to reach it sends a researcher to find one.
The star control says so up front: "No email yet: starring it looks for one", or in a country written to by form only "Calls-first country: starring it looks for a business phone"; where there is nothing to look for it says "Not in this campaign's countries" or "Calls aren't allowed there under the current rules".
The console never holds the Anthropic key and never calls Claude: it queues an attempt in the database and writes the request time to `/opt/leadhound/data/run/leads-research`.
`leadhound-leads-research.path` sees the write (`PathChanged=`, as the [Run now](#run-now) watchers do) and starts `leadhound-leads-research.service`, which runs `leads research run --max 20` with the key as its credential.

- **What it looks for** is one decision, shared by the star note, the request a star queues, "Look again" and `leads research request`: the lead's campaign must list its country (when the campaign lists countries); then the country rule in `leads.yaml` gives a published, impersonal role address where the country is emailed, a business phone where it is written to by form only, and nothing where it is off; and a phone is looked for only where the country table lets the campaign call (Germany needs the campaign's written `acknowledge:` reason, and a country the table does not list, such as Switzerland, is never called).
  A lead that is suppressed, deferred or no longer new gets no attempt, and a suppressed domain is never researched.
- **What it keeps.**
  LeadHound fetches every page Claude cites and keeps only an address it finds there itself, classified as a role address the way the contact finder classifies one, with a working mail server; a phone counts only when it is printed on the page.
  The next `leads build` gives the lead its channel.
- **One attempt per organisation every 90 days**, counted from the attempt that ran; an attempt the spend cap stopped before calling counts for nothing.
  The lead page's "Contact research" line says the latest attempt in plain words, and "Look again" asks for another, refused inside the 90 days with the date it can run.
- **Caps.** Research has its own daily cap and a per-lead cap (see [Claude spend caps](#claude-spend-caps)); a run past them leaves the attempt paused, and it can be asked again.
- **A run takes at most 20 attempts**, starred leads first, oldest first; the rest wait for the next request.
  A request while the service runs is merged into that run by systemd.
- **The batch refusal lists each starred lead and why it is out** (ready, no email yet, looking for a contact, found nothing, country rule, suppressed, deferred, ranked out), in the console and from `leads batch new`.
- **Locally** there is no watcher: run `leadhound leads research run --max 3` by hand with the key file set as in [The Anthropic key](#the-anthropic-key) and `LEADHOUND_LIVE=1`.

Check the watcher with `systemctl status leadhound-leads-research.path` and a run with `journalctl -u leadhound-leads-research.service`.
Like `leadhound-leads-batch.service`, the unit fails at start while `/etc/leadhound/anthropic.key` is missing, so place the key before the first star on the server; the request stays queued and runs on the next request once the key is there.
If it hits its start limit, reset it as the Run now watchers are reset:

```sh
sudo systemctl reset-failed leadhound-leads-research.path leadhound-leads-research.service
sudo systemctl start leadhound-leads-research.path
```

## Calls, screening and phone research

Calls are made by hand: the server never dials.
A campaign with a call step drafts the call with the rest of the lead's sequence at batch time, and the call falls due after the step before it is sent.
The console's Today tab lists the calls to make now and the overdue ones; each opens a call card with the number, where it came from, three talking points, the screening status and the six outcome buttons.
Record what happened on the card, or from the command line:

```sh
make remote HOST=<ssh host> CONFIRM=1 ARGS='leads calls list --due'
make remote HOST=<ssh host> CONFIRM=1 ARGS='leads calls outcome <message> no_answer --note "rang twice"'
```

`leads send` never touches a call row, and a reply or opt-out at any step cancels every pending call of that lead.

**When a call is callable.**
One rule decides it in every place that shows a call, in this order: there is a number to dial; it is not suppressed; it was not marked wrong on an earlier call; the country table (`internal/compliance/countries.yaml`) allows calling this organisation; the number's screening is clean and recent enough; and it is inside the calling hours in the callee's time.
A country the table marks off (none since the operator opened Austria, Poland, the Czech Republic and Bulgaria to companies on 29 Oct 2026), or does not list, is never callable, and no campaign setting opens it.
A campaign whose own countries say `call: off` still gets its calls drafted, with a warning beside each that it is your call whether to ring.
A US, Canadian or Australian call needs the lead's state or region, which today only a café place gives; without it the card says "region unknown: not callable".
The table was researched on 27 Oct 2026 and is not legal advice: entries marked `secondary`, `unverified` or `operator_decision` are leads for a lawyer, and `leads campaigns check` warns once the research is more than 180 days old.

**The privacy notice, before calls start.**
Phone numbers and call notes are personal data collected from public pages, so each lead must be able to find your privacy notice.
Write the notice, set `sender.privacy_url` in `leads.yaml` to it, and run `make deploy-data`, before the first call campaign is batched (by 7 Nov); every email's footer then ends "How I handle your details: <url>".
`leads campaigns check` warns for every campaign with a call step while it is missing.

**Screening, before any Great Britain call.**
A GB number is callable only with a clean TPS and a clean CTPS result from the last 28 days; other registers have their own age, and one with no age (Italy's, Spain's, Ireland's, Australia's and Greece's) counts from the latest batch approval.
LeadHound has no register API: you export the numbers, screen the file with the register's own service, and import the result.

```sh
make remote HOST=<ssh host> CONFIRM=1 ARGS='leads phones screen export --register tps --out /tmp/tps-export.txt'
make remote HOST=<ssh host> CONFIRM=1 ARGS='leads phones screen import --register tps /tmp/tps-result.csv'
```

The export lists only the numbers whose screening is missing or too old, a header line and then one E.164 number per line, at mode 0600, and never writes over an existing file (Ireland's export leaves out every number not known to be a landline, since only landlines are called there).
The file is the `leadhound` user's, so copy it off the server as root, screen it, put the result back where that user can read it, import, and delete every copy on both sides: they hold phone numbers.
The import reads the register's result file, counts lines it cannot read by number without guessing, and suppresses every listed number; a later clean result never lifts that suppression.
Repeat with `--register ctps`.
The registers are `tps`, `ctps`, `dncm`, `ndd`, `dncr`, `rpo`, `robinson` and `gr_art11`.

**Do not call.**
"Do not call" on the card, or `leads suppress add phone +NUMBER --reason do_not_call`, suppresses the number for every campaign and ends the lead's sequence; the suppression stores a hash of the number, never the number.
"Wrong number" marks that number bad, so no later batch drafts it and no pending call to it stays callable.

**Phone research.**
`leads phones research --campaign SLUG [--max N]` (10 organisations by default) looks for a business phone for the campaign's leads that have a call step ahead and no phone, or only a switchboard, starred first.
It is the contact research below with phones only: Claude (Haiku) searches, LeadHound fetches each page Claude cites and keeps a number only when it is printed there, on the organisation's own domain; it stores the line kind and a job title, never a person's name, and never Claude's quote of the page.
Each organisation gets one try every 90 days, found or not, under the research caps in [Claude spend caps](#claude-spend-caps).
It has no unit and no timer: run it by hand with the key, as "Running a Claude command by hand" in [The Anthropic key](#the-anthropic-key) shows.

## Erasure

`leads forget org ID --reason TEXT` erases one organisation's personal data when someone asks, in one transaction: its phone facts, phone research and screenings, its call notes, its kept page bodies, its description and social facts, and the phones and emails of its map listing.
It also suppresses the organisation's domain and its numbers (as hashes, with the source `forget: <reason>`), so it is never collected, enriched, researched or called again, moves its leads to suppressed and cancels their pending messages.
The reason is one line of at most 200 characters with no `@` in it; it prints counts only, never a number or a page.
ID is the organisation's id in the database (the `id` column of the `org` table), not the lead's number; the console does not show it yet.

It runs as the owner role, because the app role can delete none of these rows, so it needs `/etc/leadhound/migrate.env` and cannot run through `make remote`; `make forget` runs it that way:

```sh
make forget HOST=<ssh host> ORG=<ID> REASON='asked to be removed' CONFIRM=1
```

It refuses, before anything runs, without `HOST`, `ORG` (digits only), `REASON` or `CONFIRM=1`; the reason reaches `leadhound` whole, single-quoted for the server's shell.
On the server it is `systemd-run … -p EnvironmentFile=/etc/leadhound/env -p EnvironmentFile=/etc/leadhound/migrate.env … leadhound leads forget org <ID> --reason '…'`, as the `leadhound` user, the way `install.sh` runs `migrate`.

Its log line, printed with the counts, names the org id and nothing personal; keep the ids you erase (the ids alone) in your private notes.

**Backups keep erased data for a while.**
The nightly dumps in `/var/backups/leadhound/` are not edited: an erased organisation stays in every dump written before the erasure until that dump ages out, at most the newest 14 dumps within 4 GiB (see [Backups and the storage budget](#backups-and-the-storage-budget)), so about two weeks.
The shared cluster's own backups keep it for as long as their retention says.
After any restore, run `leads forget org ID --reason TEXT` again for every id erased since that dump was written.
It is safe to run again: on an organisation already erased it finds nothing left and says "nothing left to forget".

## Console

`leadhound serve` is a small web console for one operator: the day's health, the latest brief's jobs to mark, the tracked jobs, the runs and the rules.
It listens on loopback only and refuses any other address, so Caddy on the same server is the only way in.

1. On your machine, run `make console-password`.
   It asks for the password twice, without showing it, and writes its bcrypt hash, mode 0600, to the repository's ignored `data` directory.
   Use a long random password from a password manager; one under 12 characters is refused.

2. Add four lines to `/etc/leadhound/env` (`deploy/env.example` has them):

   ```sh
   LEADHOUND_CONSOLE_ADDR=localhost:8090
   LEADHOUND_CONSOLE_EMAIL=you@mail.example
   LEADHOUND_CONSOLE_PASSWORD_HASH=<the hash file make console-password writes, on one line>
   LEADHOUND_CONSOLE_URL=https://console.example
   ```

   `LEADHOUND_CONSOLE_ADDR` can be left out for port 8090 on loopback.
   `LEADHOUND_CONSOLE_URL` goes into the brief's footer as the link to the Jobs page.
   Without the email and the hash the console refuses to start, and the timers run either way.

3. Point a DNS record for the console's name at the server: an `A` record, and an `AAAA` one if the server has IPv6.
   If your DNS provider can proxy traffic, set the record to DNS only, not proxied, so Caddy gets its own certificate and sees each visitor's real address, which the sign-in limit counts by.

4. Add a site block to the server's Caddyfile, with the same `log` settings as its other sites, and reload Caddy:

   ```
   console.example {
   	log {
   		…
   	}
   	reverse_proxy localhost:8090
   }
   ```

   The port is the one in `LEADHOUND_CONSOLE_ADDR`.
   The console takes the visitor's address from the last `X-Forwarded-For` entry only when the request comes from loopback, which is how Caddy forwards it.

5. Deploy (`make deploy … CONFIRM=1`).
   `deploy/install.sh` enables and starts the console only when `/etc/leadhound/env` has a `LEADHOUND_CONSOLE_PASSWORD_HASH` line; until then it prints one line saying so and the deploy carries on.

Sessions live in memory, so every restart signs you out, and every deploy restarts the console: sign in again after a deploy.
Sign-in allows five tries a minute from one address, and stops everyone after 100 failed tries in 15 minutes; a device that has signed in before can still sign in during such a flood (it keeps the five-a-minute limit).

To reset the password, run `make console-password` again, replace the `LEADHOUND_CONSOLE_PASSWORD_HASH` line on the server with the new hash, and restart the console: `sudo systemctl restart leadhound-console.service`.
A new password forgets every device that signed in with the old one.
Its log is `journalctl -u leadhound-console.service`.

## Backups and the storage budget

The nightly dump keeps the newest `LEADHOUND_BACKUP_KEEP` dumps (14) whose sizes together stay within `LEADHOUND_BACKUP_MAX_BYTES` (4 GiB); both are set in `leadhound-backup.service`.
Each night, after the new dump is written, the script walks the dumps from the newest: the first one that would make one too many or take the total over the budget is deleted, with every older one.
The newest dump is always kept, even when it alone is over the budget; the journal then says so (`journalctl -u leadhound-backup.service`).
A failed dump deletes nothing: the old dumps stay until a dump succeeds.

The database has its own budget, `LEADHOUND_DB_MAX_BYTES` in `/etc/leadhound/env`, and the disk a floor, `LEADHOUND_DISK_MIN_FREE_PCT`.
Once the database is over its budget or the disk's free space under the floor, every collector stops storing: its run ends with the stop reason `disk_budget`, and the brief and the console's dashboard say so in one line.
Scoring, the brief and the console carry on with what is stored.

Trimming old posting text frees space inside the tables for new rows to reuse, but never shrinks the files, so it does not bring the database back under its budget.
To recover, do one of these:

- Raise `LEADHOUND_DB_MAX_BYTES` in `/etc/leadhound/env`, when the disk has the room; the next run reads it.
- Or shrink the large tables with `VACUUM FULL`, in a maintenance window with the timers stopped.
  `VACUUM FULL` rewrites a table and its indexes under a lock that blocks every read and write of it, and needs free disk for the new copy while the old one still exists; if the disk is short, lower `LEADHOUND_BACKUP_KEEP` or delete the oldest dumps first.

```sh
systemctl list-timers 'leadhound-*'          # note which timers are active
sudo systemctl stop 'leadhound-*.timer' leadhound-console.service
systemctl list-units 'leadhound-*'           # wait until no leadhound service is running
sudo -u postgres psql -d leadhound -c "SELECT relname, pg_size_pretty(pg_total_relation_size(oid)) FROM pg_class WHERE relkind = 'r' AND relnamespace = 'public'::regnamespace ORDER BY pg_total_relation_size(oid) DESC LIMIT 5"
sudo -u postgres psql -d leadhound -c 'VACUUM (FULL, VERBOSE, ANALYZE) job_observation'
sudo -u postgres psql -d leadhound -c 'VACUUM (FULL, VERBOSE, ANALYZE) gate_result'
sudo systemctl start <the timers noted above> leadhound-console.service
```

Name the tables the size query lists at the top; `job_observation` (the posting text) and `gate_result` are usually the largest.

## The trim

`leadhound-trim.timer` runs `leadhound trim` every day at 15:00, as the app role, through a function the owner role defined (`leadhound_trim_batch`), so no timer holds the owner role.
It blanks the description text of a posting rejected by a gate more than 7 days ago (`--rejected-after 168h`) and of a posting that expired more than 30 days ago (`--expired-after 720h`).
Every row stays, with its verdicts, its status history and its ids; only the text goes, and a trimmed posting is never scored on its empty text.
A protected job is never trimmed: one you set a status on, one applied to, interviewing or with an offer, and one listed in a brief.

It works through the untrimmed postings in batches of 5,000 by id, each its own short transaction, with a pause between batches, and stops itself at its 45-minute budget; the next run carries on with what is left, so a large backlog takes more than one run.
It prints `trim: rejected N · expired M · batches B · <duration>` and records a `run` row.

Why 15:00: it stays clear of the night's ATS collect (01:00, which can run past 06:00), the 03:30 dump and the morning collect and brief.
It runs in the daytime beside the server's other services, so it yields to all of them: `Nice=19`, idle I/O and `CPUWeight=20`, with `MemoryMax=300M` and `GOMEMLIMIT=200MiB`; the work Postgres does for it is bounded by its small batches.

To run it by hand, now, with the unit's limits:

```sh
sudo systemctl start leadhound-trim.service
journalctl -u leadhound-trim.service -f
```

`make remote HOST=… ARGS='trim' CONFIRM=1` also runs it, without the unit's hardening, ceiling and timeout; the flags (`--rejected-after`, `--expired-after`, `--batch`, `--max-duration`) can be given there.

The space the trim frees inside the tables is reused by new rows, but never returned to the disk: the files do not shrink, so the trim does not bring the database back under `LEADHOUND_DB_MAX_BYTES` (see [Backups and the storage budget](#backups-and-the-storage-budget)).

## Restore

Dumps are in `pg_dump`'s custom format:

```sh
sudo -u postgres pg_restore --clean --if-exists -d leadhound /var/backups/leadhound/leadhound-<stamp>.dump
```

After a restore, run `leads forget` again for every organisation erased since that dump was written, as [Erasure](#erasure) says.
