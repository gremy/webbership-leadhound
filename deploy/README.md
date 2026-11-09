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
Each fixed timer adds a random delay of up to 10 minutes.
The ATS collect, the LinkedIn collect and three leads jobs run on a rolling schedule instead: their timers tick every 15 minutes with no delay, and a due-check starts each job one interval after its last start (see [Run now and the rolling schedule](#run-now-and-the-rolling-schedule)).
Every timer but the leads send and sync is persistent, so a run missed while the server was down happens at the next boot.

| Timer | When | Runs |
| --- | --- | --- |
| `leadhound-ats.timer` | every 24 hours from its last start, first at 01:00 | `collect ats`, then `score --max-duration 2h` |
| `leadhound-backup.timer` | 03:30 | `pg_dump -Fc` into `/var/backups/leadhound/`, newest 14 kept within 4 GiB together |
| `leadhound-collect.timer` | every 12 hours from its last start, first at 06:30, then 18:30 | `collect linkedin`, then `score --max-duration 45m` |
| `leadhound-prebrief.timer` | 07:30 | `canary linkedin` (once a day; a second start the same day does nothing) |
| `leadhound-brief.timer` | 07:50 | `brief --send`, retried twice, 10 minutes apart, if the send fails |
| `leadhound-trim.timer` | 15:00 | `trim --max-duration 45m`, under `timeout 50m`: see [The trim](#the-trim) |
| `leadhound-leads-verify-balance.timer` | every 30 minutes, up to 2 minutes late | `leads verify --balance-only`: see [The credits on the Status page](#the-credits-on-the-status-page) |

The five leads timers are installed on every deploy but enabled only when `/etc/leadhound/env` allows them, and each job also has its own switch on the console's Controls page: see [Leads on the server](#leads-on-the-server).
The contacts, probe and batch services also have a Run now watcher each, armed with their timer.

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
Each unit's `TimeoutStartSec` is longer than its due-check's minute, lock waits and timeouts together: 8h16min for the ATS unit (1 minute + 4h05 + 1h30 + 2h22 = 7h58) and 5h31min for the LinkedIn one (1 minute + 30 minutes + 2h05 + 1h30 + 1h02 = 5h08).

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

Check the watchers with `systemctl list-units 'leadhound-*.path'`: these four, the mailbox check's `leadhound-mailbox-check.path` (see [Mailboxes](#mailboxes)), `leadhound-leads-research.path` (see [Contact research](#contact-research)), Draft it's `leadhound-leads-draft.path` (see [Draft it](#draft-it)), the Status page's `leadhound-leads-verify-balance.path` and Check email now's `leadhound-leads-verify-now.path` should all be `active` and `waiting`, and Send now's `leadhound-leads-send-now.path` too wherever the send timer is enabled (see [Send now](#send-now)).
The leads jobs' Run now watchers go with their timers: `leadhound-leads-contacts.path` and `leadhound-leads-probe.path` wherever the finding timers are enabled, and `leadhound-leads-batch.path` wherever the batch timer is.
Check a run with `journalctl -u leadhound-preview.service` (or `leadhound-fetch`, `leadhound-brief`, `leadhound-ats`).
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

### Run now and the rolling schedule

Five jobs run on a rolling schedule: each keeps its rhythm, and its next automatic run is planned one interval after its last start, whoever started it.
Press Contacts at 15:02 and the next automatic run is 15:00 tomorrow instead of 10:00; nothing is skipped and nothing runs twice.

| Job | Service | Rhythm | First run with no history | On demand |
| --- | --- | --- | --- | --- |
| Contacts | `leadhound-leads-contacts.service` | every 24 hours | 10:00 | Run now on Controls → Contacts (`run/leads-contacts`) |
| Site probe | `leadhound-leads-probe.service` | every 24 hours | 13:00 | Run now on Controls → Site probe (`run/leads-probe`) |
| Morning batch | `leadhound-leads-batch.service` | every 24 hours | 06:15 | Run now on Controls → Morning batch (`run/leads-batch`) |
| ATS collect | `leadhound-ats.service` | every 24 hours | 01:00 | Run the nightly ATS collect now (`run/ats`) |
| LinkedIn collect | `leadhound-collect.service` | every 12 hours | 06:30, then 18:30 | Fetch new jobs, then show (`run/fetch`), which pulls the collect in |

Every other timer keeps its fixed time: the brief and the prebrief belong to the morning email, the hourly send to the sending window, the sync and the Reoon balance run too often for a plan to mean anything, and the backup and the trim are maintenance.

**How a run is planned.**
The five timers only tick: `OnCalendar=*-*-* *:00/15:00 Europe/Bucharest`, with `RandomizedDelaySec=0` and `Persistent=true`.
There is no random delay, because a tick late by more than half of it would move the rhythm.
At each tick the service's first line, the due-check, decides whether the job starts:

```ini
ExecCondition=/usr/bin/timeout 1m /opt/leadhound/bin/leadhound timer-guard --record <job>
ExecStopPost=-/usr/bin/timeout 1m /opt/leadhound/bin/leadhound timer-guard --record --ended <job>
```

The plan is the last start plus the interval, rounded to the nearest 15-minute tick, so a start a minute or two late never drifts the job.
A run by hand counts at its start, so a press moves the rhythm.
A timed run counts at its planned time, so a run that waited behind another job, or caught up after a reboot, keeps the job on its usual time.
A daily job adds one calendar day, so it keeps its local time across the clock changes.
With no history at all, the first run is at the job's old time.
A job idle for more than two intervals (a timer switched back on after weeks, a long outage) resumes at its usual time, not the moment it is deployed.
A tick that is missed, deferred or caught up at boot is due at once on the next tick, so a run is never lost.

**The due-check** (`leadhound timer-guard`) lets a start through in two cases, and passes over the tick in every other.
A press starts the job at once: a request file in `run/` newer than 20 minutes that no run has served yet.
A due job starts when its plan has come, unless the job sharing its lock is running: Contacts and Site probe share the sites' fetch lock, so each waits while the other runs, a press included.
That wait is never a skip: the next tick checks again, and the job starts once the lock is free.
When in doubt, it waits: a database it cannot read, a crash or its own one-minute timeout passes over the tick, and the next tick tries again.
So a broken check shows as an overdue run on the console, never as a job run every 15 minutes.
The one exception is a fresh press, which runs even when the database cannot be read, so the job itself reports the database.
Every press the check lets through or finds already served is marked in a hidden file beside its request (`run/.<request>.served`), so a press that has had its run never runs again while the database is out of reach; a marker it cannot read or write means wait.
`systemctl start` of a guarded service by hand is passed over too unless the job is due: use its button, or write its request file as the console does.

**The rows.**
Each start the check lets through opens one run row, `run-now` (note `asked at <time>`) or `scheduled` (note `planned <time>`), with the job as its source.
The row stays open while the job runs, and the unit's `ExecStopPost=` closes it with systemd's own result (`· ended success`, `· ended timeout`, …), also after a failure or a kill.
A row left open by a reboot is closed `· ended unrecorded` by the next check.
These rows are the plan's anchor, and the console reads a run's state from them.
An `unrecorded` row with no step row of its job inside it (for ATS and collect, no collect row of their own) is a start that never ran: it neither anchors the plan nor serves a press, so its slot or press is still due at the next tick.
They stay off the dashboard's latest runs; each step still records its own row as before.

**The journal.**
systemd logs about three info lines for every tick it passes over, so `systemctl list-timers` LAST and PASSED show the last tick, not the job's last run.
The console shows the plan instead: Controls (each job's row), the dashboard's Run now card (ATS and fetch) and Status (each unit).
Each says when the next run is planned and why, for example "Next run planned at 15:00 tomorrow, 24 hours after the run by hand at 15:02."
While the job waits behind the other fetch-lock job, it says "Planned at 13:00; waiting while the contacts job runs."
A timer that is not enabled shows no plan.

**When a run is overdue** (more than 30 minutes past its plan, with no start and nothing holding its lock), Controls and Status say so and Status warns.
Read the check's own lines with `journalctl -u <service> -g timer-guard`, or everything at warning and above with `journalctl -u <service> -p warning`.
Then ask the check on the server: `make remote HOST=… ARGS='timer-guard <job>'` prints the verdict, the anchor and the plan, and writes nothing, so asking never moves the plan.
The job names are `leads-contacts`, `leads-probe`, `leads-batch`, `ats` and `collect`.
The batch unit holds the Anthropic key, which systemd loads before the check runs, so on a server without the key the batch unit fails at every tick until you place it (see [The Anthropic key](#the-anthropic-key)).

## Layout on the server

| Path | Owner, mode | What |
| --- | --- | --- |
| `/opt/leadhound/bin/leadhound` | root, 0755 | the binary |
| `/opt/leadhound/bin/leadhound-backup` | root, 0755 | `deploy/leadhound-backup.sh` |
| `/opt/leadhound/data/` | leadhound, 0750 | `LEADHOUND_DATA_DIR`: `leadhound.yaml`, `ats-boards.yaml`, `leads.yaml`, `agencies.yaml` `leads-templates/` and `campaigns/` (root:leadhound 0640, from `make deploy-data`; see [Campaigns on the server](#campaigns-on-the-server)), the outbox of a brief that is not sent, `preview.json` (the latest Run now results) |
| `/opt/leadhound/data/run/` | leadhound, 0700 | the console's Run now requests, its mailbox check requests (`run/mailbox-check`) its contact research requests (`run/leads-research`) its Draft it requests (`run/leads-draft`) the Status page's Reoon refresh (`run/leads-verify-balance`), Send now and the manual send (`run/leads-send-now`), Check email now (`run/leads-verify-now`) and the leads jobs' Run now on Controls (`run/leads-contacts`, `run/leads-probe`, `run/leads-batch`), one file per action, read by the `leadhound-*.path` watchers |
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

- `make deploy HOST=… [ARCH=…] CONFIRM=1` ships a new release: it runs `make gate` (check, vet with the integration and scale tags, the deploy tests; the integration and scale suites are `make gate-full`, run by choice), builds a static Linux binary, copies it with `deploy/install.sh`, the backup script and the units, runs `migrate` with the new binary as the owner role, then puts the new binary and units in place, reloads systemd and enables the timers and the Run now watchers.
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
  The config is checked twice: once on your machine with this checkout's source, then on the server with the binary already there, which runs `score --check-config` and `leads check-config` on the staged files as the leadhound user.
  The second check catches a `leads.yaml` key the live binary doesn't know yet (on 29 Oct, `cafe_own_vendor_hosts`), which would otherwise make every leads command on the server refuse the file, sending included.
  When either server check fails or refuses, it prints the output, removes the stage and installs nothing; deploy the newer binary first (`make deploy`), then run `deploy-data` again.
  Without a local `campaigns/`, the server's campaigns are left as they are.
  For a first `leads.yaml`, start from `examples/leads.yaml` (a demo with reserved names) and set your own sender, addresses and templates.
  It copies nothing else: the title screen is part of `leadhound.yaml`, and the eval golden set, the privacy denylist and the board seed lists are never needed on the server.
  After copying `campaigns/` it prints the `leads campaigns register` line to run next, as [Campaigns on the server](#campaigns-on-the-server) says; it does not register for you.
- `make remote HOST=… ARGS='track list'` runs one command on the server as the `leadhound` user with the units' environment.
  `track list`, `rules list`, `rules test`, `boards list`, `leads suppress list|check`, `leads batch show` and `leads mailboxes list` run freely; every other command writes or reaches the network (`collect`, `canary`, `brief`, `score`, `migrate`, `recheck`, `probe`, `track <n> …`, `rules add`, `boards import` …) and needs `CONFIRM=1`.
  `ARGS` is split into words by your own shell, as on a command line, and each word reaches `leadhound` whole: the server's shell never reinterprets it.
  So a pattern with spaces or backslashes goes in single quotes inside the double-quoted `ARGS`: `make remote HOST=… ARGS="rules test --field title --pattern 'plain words\b'"`.
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

Five timers run the leads; all times are Europe/Bucharest.
The probe, contacts and batch timers tick every 15 minutes and their due-check starts each job every 24 hours from its last start (see [Run now and the rolling schedule](#run-now-and-the-rolling-schedule)); the send and sync timers keep their fixed times, with up to 10 minutes of random delay.
Every leads unit has the same sandbox as the other timed units, `MemoryMax=600M` with `GOMEMLIMIT=450MiB`, `Nice=10`, idle I/O and `CPUWeight=50`, and each command runs under a `timeout` that `TimeoutStartSec` covers.
Business hours, spacing, daily caps, the warm-up ramp and the pause are the commands' own (`leads.yaml` and the console), never the units'.

| Timer | When | Runs | Enabled when | Console switch |
| --- | --- | --- | --- | --- |
| `leadhound-leads-probe.timer` | every 24 hours from its last start, first at 13:00 | `leads sites probe --max 10000`, under `timeout 3h`, after waiting up to 195 minutes for the fetch lock; `TimeoutStartSec=391min` | `LEADHOUND_LEADS_TIMERS=on` | Site probe (`probe`) |
| `leadhound-leads-contacts.timer` | every 24 hours from its last start, first at 10:00 | `leads contacts --max 300`, under `timeout 2h`, then `leads enrich --max 300`, under `timeout 60m`, then `leads rate --max 10000` for `drupal-eol` and `leads rate --campaign cafes-loyalty --max 10000`, each under `timeout 180m`, then `leads build`, under `timeout 20m`; `TimeoutStartSec=10h1min` | `LEADHOUND_LEADS_TIMERS=on` | Contacts (`contacts`): the contacts, enrich and rate steps; the build always runs |
| `leadhound-leads-batch.timer` | every 24 hours from its last start, first at 06:15 | `leads build`, under `timeout 20m`, then `leads verify --max 60`, under `timeout 50m`, then `leads batch new`, under `timeout 20m`, for the `drupal-eol` campaign; `TimeoutStartSec=2h1min`; it holds the [Anthropic key](#the-anthropic-key) because Claude drafts at batch time, and the [Reoon key](#the-reoon-key) for the email check | `LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live` | Morning batch (`batch`): `leads batch new`; Email check (`verify`): `leads verify`; the build always runs |
| `leadhound-leads-send.timer` | hourly 10:00 to 19:00, Monday to Friday, not persistent | `leads send --max 25`, under `timeout 225m` | `LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live` | Sending (the send pause) |
| `leadhound-leads-sync.timer` | every 15 minutes, not persistent | `leads sync`, under `timeout 10m` | `LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live` | Inbox sync (`sync`) |

- The probe and the contact search are the finding timers.
  They start unattended reads of public sites: up to 10,000 homepages a day, and up to 300 organisations searched for a published contact a day, each site's robots file read and honoured first.
  They share a fetch lock in the data directory, `/opt/leadhound/data/.leads-fetch.lock`.
  The contact search and the enrichment do not wait for it: a step that finds it held exits 75, which the contacts unit counts as done.
  The due-check already holds each back while the other runs, so neither starts into a held lock.
  The probe still waits for it, up to 11700 seconds (195 minutes), because the contacts unit can hold it for 190 minutes; a wait that runs out exits 75 and fails the probe unit, so a lost probe shows in `systemctl`.
- After the contact search, the contacts unit enriches organisations, rates leads for relevance, then runs `leads build`, which turns every organisation with a finding into a lead and reads no web page, so the console's Leads pages fill while only finding is on; the batch rebuilds the list again before drafting.
  Its steps, from `leadhound-leads-contacts.service`:

  ```ini
  ExecStart=-/usr/bin/flock --nonblock --conflict-exit-code 75 /opt/leadhound/data/.leads-fetch.lock /usr/bin/timeout --kill-after=5m 2h /opt/leadhound/bin/leadhound leads contacts --max 300
  ExecStart=-/usr/bin/flock --nonblock --conflict-exit-code 75 /opt/leadhound/data/.leads-fetch.lock /usr/bin/timeout --kill-after=5m 60m /opt/leadhound/bin/leadhound leads enrich --max 300
  ExecStart=-/usr/bin/timeout --kill-after=5m 180m /opt/leadhound/bin/leadhound leads rate --max 10000
  ExecStart=-/usr/bin/timeout --kill-after=5m 180m /opt/leadhound/bin/leadhound leads rate --campaign cafes-loyalty --max 10000
  ExecStart=/usr/bin/timeout --kill-after=5m 20m /opt/leadhound/bin/leadhound leads build
  SuccessExitStatus=75
  TimeoutStartSec=10h1min
  ```

  Every step but the build is `ExecStart=-`, so a failed search, enrichment or rating never stops the build.
  The rating and the build read no web page, so they run outside the fetch lock.
  `leads rate` rates one campaign's leads, `drupal-eol` unless `--campaign` names another, so the café campaign has its own rate line, the second.
  `TimeoutStartSec=10h1min` is longer than the due-check's minute and the five timeouts and their kill graces together (9h46).
- Before the batch drafts, `leads verify` checks with Reoon the email addresses that batch would draft (see [The email check](#the-email-check)); it is written `ExecStart=-`, so a Reoon outage or a missing key makes a smaller batch, never no batch.
- The batch drafts the next batch for you to read and approve in the console; while a batch waits, `leads batch new` makes none.
  While your saved settings no longer fit `leads.yaml` (so they are not applied), `leads batch new` makes none and exits 2, so the batch unit shows as failed in `systemctl`; fix or reset them in Leads → Settings.
  The send sends only approved messages, each in its recipient's business hours (the send hours cover the working day in the UK, Ireland and central Europe), and the sync reads every mailbox that is not retired for replies, opt-outs and bounces.
  When one mailbox cannot be read, the others still sync and `leads sync` exits 1, so the sync unit shows as failed in `systemctl` and the journal names the mailbox.
  `leads send --max 25` is the most sent in one run over all mailboxes together; with several mailboxes, raise it in `leadhound-leads-send.service`.
  Send and sync take no file lock: their database locks keep them apart.
  The send and sync timers are not persistent, so a run missed while the server was down is never caught up outside its hours.
- No leads unit reads `/etc/leadhound/migrate.env`; they hold only the app role.

Every deploy installs the units and decides the timers from `/etc/leadhound/env`, the last line for each name counting, as for the units:

- With `LEADHOUND_LEADS_TIMERS=on`, the probe and contacts timers and their Run now watchers are enabled; otherwise they are disabled and stopped.
- With that and `LEADHOUND_OUTREACH=live`, the batch, send and sync timers, the batch's Run now watcher and Send now's are enabled; otherwise they are disabled and stopped, so the server never drafts a shadow batch.
  The mailboxes live in the database, not in this file: a run with no ready mailbox sends nothing and says so.
- The deploy prints which leads timers are on, and for those that are off, which lines are missing.

Whether an enabled timer does anything is the console's: see [Switches in the console](#switches-in-the-console).
Draft it has no timer and no switch: see [Draft it](#draft-it).
To turn finding on, add `LEADHOUND_LEADS_TIMERS=on` to `/etc/leadhound/env` and run `sudo systemctl enable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer leadhound-leads-contacts.path leadhound-leads-probe.path` (or deploy).
To turn sending on, first add a mailbox, assign it to its campaign and send its test, as [Mailboxes](#mailboxes) says; then, with `LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live` in the file, deploy (or run `sudo systemctl enable --now leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer leadhound-leads-batch.path leadhound-leads-send-now.path` and `sudo systemctl restart leadhound-console.service`, so the console reads the new settings), and switch Sending on on the console's Controls page.
Without `LEADHOUND_LEADS_TIMERS=on` the next deploy disables the sending timers again, even with `LEADHOUND_OUTREACH=live` set.
A unit reads the env file only when it starts.

To stop everything, remove `LEADHOUND_LEADS_TIMERS=on` (or set it to anything but `on`) and run `sudo systemctl disable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer leadhound-leads-contacts.path leadhound-leads-probe.path leadhound-leads-batch.path leadhound-leads-send-now.path`; the next deploy leaves them off.
A run already in progress finishes; `sudo systemctl stop leadhound-leads-<name>.service` ends it at once.

### Switches in the console

The console's Controls page (in the top menu, after the dashboard) shows every on/off that decides what the leads work runs, and switches each job without touching the server.
At its top one sentence says why nothing is going out, or when the next email does: shadow mode, sending switched off, the send timer not enabled, nothing approved waiting or due, a mailbox that cannot send or is not assigned to its campaign, today's warm-up limit, or the sending window, the first that applies.
The same sentence, with a link to Controls, sits above the Outreach tab's list and on the dashboard's Leads card.

- **Sending** is the send pause, the master switch above every mailbox: switched off, the send timer keeps running and sends nothing until you switch it on, and nothing on the server changes.
  Switching it on Controls brings you back to Controls; the same switch is on the Leads page's Outreach tab, and `leadhound leads send pause [--reason R]` and `leads send resume` work it from the command line.
- **The jobs**, each with its own switch: Morning batch (`batch`), Inbox sync (`sync`), Site probe (`probe`), Contacts (`contacts`: the contacts, enrich and rate steps), Research (`research`), and Email check (`verify`: `leads verify`, and the check every live batch makes; see [The email check](#the-email-check)).
  Each row shows what the job does, its schedule, since when and why it is on or off, whether its timer is enabled and its next run (read from `systemctl show`; when that cannot be read, the page says so and shows what the env file makes the last deploy do), and its last run.
- **A job switched off** runs nothing: it prints "<Job> is switched off in the console (since <time>): nothing ran.", records its run with the result "switched off", and exits 0, so its timer never shows a failure for it.
  `leads build` has no switch, so the contacts and batch units still build the list.
  The console's New batch button is you acting by hand and still works while Morning batch is off.
  Inbox sync switched off stops the sync timer's runs only: `leads send` reads every mailbox's replies itself before it sends, so replies and opt-outs are never missed.
- **Research switched back on** starts what a star queued while it was off: it writes the research request file, the only thing that starts `leadhound-leads-research.service`.
  While Research is off, a star still queues its search, and its line ends "Research is switched off on the Controls page, so it will run once research is on."; only Look again is refused, with "Research is switched off on the Controls page."
- **Run a job now**: the Contacts, Site probe and Morning batch rows each have a Run now block, so you can start the job without ssh.
  Tick the box to confirm, because the run takes a while and spends credits, then press Run now.
  The console writes the request file (`run/leads-contacts`, `run/leads-probe` or `run/leads-batch`), and the job's watcher starts its timed service unchanged.
  It refuses, and says why, when the server's systemd cannot be read, while the job runs (until its unit has recorded the end) or a press already waits, while the job is switched off, while its watcher is not armed or has stopped, for Contacts while the site probe runs and for Site probe while the contacts job runs (they share the sites' fetch lock), for Morning batch while a batch waits for you to approve it, and while the box is not ticked.
  The block then follows the run in a live line: waiting to start, running and which step, ended, did not finish, did not start, or stuck.
  It refreshes itself every 30 seconds while something is pending.
  "Ended" means the unit ended; each step's own result is in the row's Last run.
  A press moves the next automatic run to one interval after it (see [Run now and the rolling schedule](#run-now-and-the-rolling-schedule)).
  Switching a job back on never starts it: only a press does.
  A press that stays "asked and not started" means its watcher stopped: reset it as for Run now, for example `sudo systemctl reset-failed leadhound-leads-contacts.path leadhound-leads-contacts.service`, then `sudo systemctl start leadhound-leads-contacts.path`.
- From the command line: `leadhound leads switches list` prints Sending and every job (on or off, since when, the reason), and `leadhound leads switches set JOB on|off [--reason R]` switches one.
  `make remote HOST=… ARGS='leads switches list'` needs no `CONFIRM=1`; `set` does.

Three switches stay on the server, on purpose, and Controls shows them read-only with the line that changes each:

| Line in `/etc/leadhound/env` | What it decides |
| --- | --- |
| `LEADHOUND_OUTREACH` | `live` sends for real; anything else is shadow, and the server never drafts a shadow batch |
| `LEADHOUND_LIVE` | outbound fetches and the paid Claude and Jev calls |
| `LEADHOUND_LEADS_TIMERS` | `on` lets the deploy enable the leads timers (batch, send and sync only with live outreach too) |

The console can never change them: a stolen console session must not be able to send for real from a shadow server or spend money.
Each takes effect with the next `make deploy`.

**`LEADHOUND_LEADS_SEND` is retired.**
The first deploy with the console switches carries it over once, before it enables any timer: when the line was anything but `on` (no line counts as off), it pauses sending as the system, with the reason "Sending was off on the server (LEADHOUND_LEADS_SEND) before the console switches", so that deploy never starts sending by itself; then it writes the marker `/etc/leadhound/send-switch-converted` (root, 0640).
With the marker there, later deploys never read the line again, so removing it is safe.
The marker lives in `/etc/leadhound`, not the data directory, so the app user cannot delete it and a data restore cannot lose it; either would make a later deploy convert again and pause sending.
A failed carry-over fails the deploy, writes no marker and leaves the leads timers as they were; the next deploy tries again.
Resume sending on Controls when you mean it to send.

### The sending window

The sending window (start, end and days, in each recipient's local time) is set on the Controls page, in its Sending window card, and saved in the console's settings over `leads.yaml`'s `schedule.business_hours`.
Every reader takes the window in force, the saved one, else the file's: the send (each message is checked against it at the moment it goes), the size of each morning batch, the time a first email falls due when you approve a batch, follow-up due times and the sentence at the top of Controls.
The card says where the window comes from ("Changed here on …", or the leads settings file); "Use the file's window" puts the file's back.
The form checks what `leads.yaml` checks: times like 08:30, the end after the start, at least one day.
A saved window wins over the file, so a `make deploy-data` with another window in `leads.yaml` leaves the saved one in force, as for every other setting.
From the command line, `leads settings show` prints it with its source and `leads settings reset --section window` puts the file's back; on the server both run through `make remote … CONFIRM=1`.
The hourly send runs from 10:00 to 19:00 Bucharest time, Monday to Friday, and the card says so: a time the window allows outside those runs (an evening, a Saturday) is reached only by Send now.
The sentence at the top never promises such a time: "go out in the next sending window" names the first hourly run inside a recipient's window, and a window no run ever falls in says "the hourly send never runs inside the sending window, so only Send now reaches them".
The batch sizing still counts every day the window names.

### Send now

Send now sends the approved emails that are due now, skipping the sending window (its hours and days) for that one run only.
Everything else still applies: the sending switch, shadow mode, the reply sync first, each mailbox's warm-up limit and the domain's daily limit, the spacing between emails, paused and unassigned mailboxes, suppressions, the run's `--max 25`, and every due time, so a follow-up never goes before its day.
There is no standing bypass.
The button sits beside the sentence on Controls and on the Leads page's Outreach tab when the sentence says the emails go in the next sending window or at the next run, with a line saying what it skips and what it keeps.
The console refuses a press before it asks for anything when sending is off, in shadow mode, when the send timer is not enabled, when nothing is due now, when today's limit is used ("Today's warm-up limit is used (n of cap); Send now only skips the sending window."), and while a send runs or a press already waits.
"A send runs" is read from the send lock itself, so a run that was killed without finishing its record blocks nothing.
While a press waits or a send runs, the button gives way to that line.
Otherwise it asks the way Run now does: it writes the request time to `run/leads-send-now`, and `leadhound-leads-send-now.path` starts `leadhound-leads-send-now.service`, with the hourly send's mailbox key, limits and sandbox.
The unit's command is fixed, `leads send --from-request`, and `--from-request` implies `--now` and `--max 25`.
It reads the request file: a time or an empty file, as this button writes it, is this Send now, the same run as `leads send --now --max 25`; a JSON line is a manual send (see [Manual Send now](#manual-send-now)).
The deploy arms that watcher with the send timer, on the same two lines (`LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live`), and disarms it with them: where the hourly send does not run, Send now does not either.
Both send units take the same database send lock, so they never run at once: whichever starts second says "Another send is running; this one did nothing."
The run is recorded as the hourly send's is, with "send now" in its note, so Controls shows its last run as "Finished (Send now)" and the line under the button says what that run sent (an hourly run never counts as the answer to a press); its journal starts "Send now: this run skips the sending window only".
From the command line: `make remote HOST=… CONFIRM=1 ARGS='leads send --now'`.
If a press stays "asked and not started", reset its watcher as for Run now: `sudo systemctl reset-failed leadhound-leads-send-now.path leadhound-leads-send-now.service`, then `sudo systemctl start leadhound-leads-send-now.path`.

### Manual Send now

The manual Send now sends leads you pick, now, and lets you go past named warnings with one confirm.
It rides the same watcher, unit and send lock as Send now; the console still never chooses a command line and holds no key.

**End to end.**

1. **Pick.** "Send now" on a lead's page, the Leads list's tick boxes and "Send now (N)", or "Approve and send now" on a batch's page.
   At most 25 leads go in one send; more is refused with "Split it".
2. **Preview** (`POST /leads/send-now/preview`).
   The console builds a manual batch of the chosen leads through the normal batch path, with the campaign's template drafts (a manual batch never waits for Claude drafts).
   The confirm page (`/leads/send-now/{id}`) lists each lead as going, waiting, or stopped with its reason, and every warning as a sentence with its numbers.
   A manual batch never blocks the morning batch, and one left unconfirmed is discarded after 30 minutes (on the next preview).
3. **Waiting on the email check.**
   With the Email check on, an address not checked yet waits, left out of the send until you press Check email now, and an unsure answer goes. On chosen leads the others can go without it; Approve and send now holds its button until the batch is checked, since an approved address would go unchecked on the hourly send.
   "Check email now" (`POST /leads/send-now/{id}/check-email`) writes `run/leads-verify-now`; `leadhound-leads-verify-now.path` starts `leadhound-leads-verify-now.service`, which runs `leads verify --from-request --max 25` on that batch's addresses only, with the Reoon key, and the page updates itself while it waits.
4. **Confirm** (`POST /leads/send-now/{id}/confirm`).
   You tick "I have read the warnings" and press send.
   The backend reads the plan again: the batch hash must be the one shown, every warning now must be one the page showed, and a new hard stop sends you back to the confirm page with the new plan.
   It then approves the batch with its first emails due now, leaves out the leads with a hard stop, and writes a `send_request` row in the same transaction: the batch, the warning codes, the hash shown, the operator, and the warnings as shown.
   A lead left out by a hard stop goes back to new (lead_event reason "left out of manual batch N"), so the next batch or Send now can take it again; only the operator's own skip on a daily batch marks a lead skipped.
5. **The request file.**
   Only once that row is committed does the console write `run/leads-send-now`, as one JSON line holding the batch and the time, `{"batch_id": N, "at": "…"}`.
   The file is untrusted, because the console and the send service run as the same user, so it names the batch only; the codes and the operator come from the row.
6. **The run.**
   `leadhound-leads-send-now.path` starts `leadhound-leads-send-now.service`: `leads send --from-request`.
   After the pause check, it takes the batch's newest unused `send_request` row no older than 15 minutes and checks it again, then claims it, once only, in the same transaction: the batch exists, is manual, approved, not discarded and of this server's kind (live or shadow), its hash matches what was confirmed and its messages still hash to it, and every code is in the closed set.
   A refused request sends nothing, fails the unit so it shows, and stays unused; a request already used or over 15 minutes old sends nothing.
   Either way the batch page then offers **Send now again** (`/leads/send-now/{id}/again`) while none of the batch's messages has gone: it shows the warnings again, asks for the tick again and writes a fresh `send_request`, without approving the batch again.
   An approved manual batch nobody sends again goes out with the hourly send, under the normal limits and with no overrides.
   A paused run never uses up the request: once sending is resumed it still works within its 15 minutes.
   The run sends that batch alone, with only the confirmed warnings lifted.
   Leads that share a recipient domain go one per run, so the rest go in the next runs, and the confirm page says so.

**The request file formats** that `leads send --from-request` accepts in `run/leads-send-now`:

| Content | Means |
| --- | --- |
| empty | Send now from Controls (`leads send --now --max 25`) |
| an RFC3339 time | Send now from Controls, as the button writes it |
| a JSON line `{"batch_id": N, "at": "…"}` with N above 0 | the manual send of batch N, under its `send_request` row |

Text starting with `{` that is not exactly that JSON, or names no batch, is refused, and so is a missing file or one over 4 KiB.
Any other text is read as a Controls Send now, because the old service never read the file.

**Warnings you may go past** (the closed set; the database refuses any other code):

| Code | What it lifts |
| --- | --- |
| `window` | your sending window is closed now |
| `business_hours` | a recipient is outside business hours in their own time, or will be before the send is through |
| `ramp` | a mailbox's warm-up cap for today; it may then go up to the hard ceiling of 40 |
| `domain_daily` | the sending domain's daily limit (`caps.domain_per_day`, 50 by default); it may then go up to the hard ceiling of 100 |
| `spacing_jitter` | the usual random spacing; emails go 60 seconds apart instead, never closer |
| `schedule` | a first email would wait for its next slot; it goes now instead |

**Hard stops no confirm lifts**: sending switched off on Controls, shadow mode on a live batch (or a live batch on a shadow server), a suppressed lead, a lead no longer new, a blocked email check, a reserved test domain, a mailbox that is not ready (paused, retired, not checked today) or not assigned to the campaign, 40 sends a day per mailbox (`HardDailyCeiling`), 100 a day per sending domain (`HardDomainDailyCeiling`), a first email already sent or already in another batch, a first step that is not an email, more than 25 leads, a batch whose messages changed after drafting, and one recipient domain per run.
On "Approve and send now" a stopped email lead blocks the whole batch, so a daily batch is never sent with a lead quietly left out: use Approve to send it on its schedule instead.
The send checks suppressions, the email check, reserved domains and a first email already gone once more as it sends each message.

**Where overrides are recorded.**
The run note gets `override: <codes>` (for example `override: business_hours,schedule,window`), beside `send now`.
Every message a manual send with any override sends (or writes to the shadow outbox) gets a `send_override` row: the run, the batch, the lead, the message, the codes, the operator, and in `detail` the numbers each warning carried at the run's start together with the warnings exactly as the confirm page showed them.
The row is written after the email is sent, so a database error at that moment stops the run but leaves that one email sent without its `send_override` row; the run note still says `override: <codes>`.
The `send_request` row keeps the confirm itself, with `used_at` set when the run claims it.

**Shadow and dev.**
On a server in shadow mode the preview is refused with the shadow stop, and a batch drafted in the other mode cannot be sent.
On `make console-local` (dev) a manual batch is allowed in shadow; nothing watches the request file there, so the confirm says to run `leadhound leads send --from-request`, which writes the emails to the shadow outbox.
Selftest leads cannot be picked in the console; `--from-request` sends a batch of selftest leads alone, and refuses one that mixes them with other leads.

If a manual send stays "asked and not started", reset the watcher as above; the request still works if the run starts within 15 minutes of the confirm, and after that, use Send now again on the batch page.
For "Check email now": `sudo systemctl reset-failed leadhound-leads-verify-now.path leadhound-leads-verify-now.service`, then `sudo systemctl start leadhound-leads-verify-now.path`.

### Draft it

The Draft it button on a lead's page drafts that one lead now, without waiting for the morning batch.
The console never holds the Anthropic key and never calls Claude: it queues a `draft_request` row and writes the request time to `/opt/leadhound/data/run/leads-draft`.
`leadhound-leads-draft.path` sees the write (`PathChanged=`, as the [Run now](#run-now) watchers do) and starts `leadhound-leads-draft.service`, which runs `leads drafts make --from-request` under `timeout 15m`, with the key as its credential.
Nothing in this job sends.

- **No timer.**
  Every deploy enables the watcher, whatever the leads lines in `/etc/leadhound/env` say, and the job runs only on a press.
- **What it makes**: a batch of its own with `origin = 'draft_it'`, manual and live, and its lead moves from new to batched.
  It never blocks `leads batch new`, and the sweep that discards stale Send now batches leaves it alone.
  Discard on the lead's page returns the lead to new.
- **A run takes at most 5 requests**, oldest first, and asks for another run when it took 5.
  A lead has at most one open request; a press on one still queued wakes the service again, then refuses with the time it was asked.
- **What it refuses**, on the lead's page with a plain reason: a lead that is not new, already batched, suppressed or in a conversation elsewhere, and one with no contact on that channel or no mailbox for its campaign.
- **No key or no `LEADHOUND_LIVE=1`.**
  The unit fails at start while `/etc/leadhound/anthropic.key` is missing, and the request stays queued until the next press once the key is there.
  With the key and without `LEADHOUND_LIVE=1`, or past a cap, the drafts come from the templates and the reason is shown.
- **Status, not Controls.**
  The Status page has a `leads-draft` row read from systemd, because a timeout or a failed start reaches no run row.
  Controls has no row for it: a Controls row is a timer job's off switch, and this job runs only on a press.
- **Locally** there is no watcher: run `go run ./cmd/leadhound leads drafts make --from-request` by hand after the press, with `--decider fake` to call no Claude.

Check the watcher with `systemctl status leadhound-leads-draft.path` and a run with `journalctl -u leadhound-leads-draft.service`.
If it hits its start limit, reset it as the Run now watchers are reset:

```sh
sudo systemctl reset-failed leadhound-leads-draft.path leadhound-leads-draft.service
sudo systemctl start leadhound-leads-draft.path
```

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

The contacts unit runs `leads rate --max 10000` for the `drupal-eol` campaign and then `leads rate --campaign cafes-loyalty --max 10000` for the cafés, each under `timeout 180m`, after the enrichment and outside the fetch lock.
Each run rates only its own campaign's leads, and both count against the one leads cap, Drupal first.
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
It says how many calls it will make before the first, and is recorded only with 30 or more labelled leads; the latest recorded eval decides.
A Jev eval grades at most 120 labelled leads, every call other than `no` first and then the newest `no` calls, and prints how many it left out.
That cap changes what the gate measures: past 120 calls it grades the leads kept, not all your calls, so skips count for less than they do in your list.
It cannot starve the gate, because 120 is well above the 30 the gate needs.

Each lead type has its own question, its own 30 calls and its own gate.
The command above measures Drupal leads (sites and agencies), because `--type drupal` is the default.
Once you have 30 calls on café leads, measure them the same way: `make remote HOST=… CONFIRM=1 ARGS='leads relevance eval --type cafe --decider jev'`, with `LEADHOUND_LIVE=1` in the env file as for Drupal.
A passing eval of one type never trusts the other.

The first contacts run after the release that gave cafés their own question re-rates every open café lead once, in its second rate line (`--campaign cafes-loyalty`): about 2,300 Jev calls, counted against the leads cap.
That costs cents of input: the eval's estimate is `CostPerMillionInput = 0.042` US dollars per million input tokens (`internal/eval/eval.go`), so even 2,000 input tokens a call comes to about 19 cents.
Drupal ratings are untouched: their question, facts and hash are exactly as before, so no Drupal lead is asked again.
Nothing is sent by it.

### Settings in the console

The console's Settings page (`/leads/settings`) changes some leads settings over `leads.yaml`, in the database: who to contact, the kinds and their order, small sites, where batches draw from, the Drupal end-of-life dates, relevance rating and its daily calls, and the fallback order.
The commands that use these settings read them from the database on every run, with the file underneath, so a change reaches the next run of each unit without a deploy or a restart.
The sending pace and ramp, the sender, the country channels and languages, and the templates show read-only: the send path reads them from the file, so change them in `leads.yaml` and run `make deploy-data`.
The sending window is the exception: it is saved the same way but changed on the Controls page (see [The sending window](#the-sending-window)), and the pace lines say where it comes from; "Reset everything to the file" resets it too.
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

#### A places campaign by hand

A campaign whose `source` is `overture` (the café example) reads places from a file you export locally with DuckDB, as the main README's "Café places" says.
The timers build and batch `drupal-eol` only, so a places campaign runs by hand, in the order below.
`HOST` is your ssh host, and `<data dir>` is the server's `LEADHOUND_DATA_DIR` from the table above, typed out: `make remote` passes each word as written, so a variable there is not expanded.

Copy the export (`places-ro.json` below; the name is yours) to the server and put it where the leadhound user can read it, outside the `campaigns/` and `leads-templates/` trees that `make deploy-data` replaces:

```sh
scp places-ro.json HOST:/tmp/
ssh HOST 'sudo install -D -o root -g leadhound -m 0640 /tmp/places-ro.json <data dir>/places/places-ro.json && rm /tmp/places-ro.json'
```

Then import it, naming the Overture release it came from; a release already imported stores nothing twice:

```sh
make remote HOST=HOST CONFIRM=1 ARGS='leads places import overture --campaign cafes-loyalty --release <release> <data dir>/places/places-ro.json'
```

A place's own emails and phones become routes through the contacts crawl's filters, so a café with no website can be reached too.
Then run the loop, one step at a time, checking the counts each prints:

```sh
make remote HOST=HOST CONFIRM=1 ARGS='leads sites probe'
make remote HOST=HOST CONFIRM=1 ARGS='leads contacts'
make remote HOST=HOST CONFIRM=1 ARGS='leads build --campaign cafes-loyalty'
make remote HOST=HOST CONFIRM=1 ARGS='leads batch new --campaign cafes-loyalty'
```

`leads sites probe` and `leads contacts` take no campaign: they work on every site and organisation, the places' homepages among them, and the timers run them too, so a step a timer is running exits 75 and you try again later.
`build` and `batch new` refuse a campaign that is not registered, so register it first.
Before the batch, `leads.yaml` must list your own product's hosts as `probe.cafe_own_vendor_hosts`, or a café already on your product becomes a lead.

### Turning the leads on in production

Production has never built a lead, so take these steps in order:

1. Place the Anthropic key first, as [The Anthropic key](#the-anthropic-key) says: from the release with campaigns on, the batch, contact research and Draft it units fail to start without it.
2. Deploy: `make deploy` migrates the core set up to 00009 and the leads set up to 00028 (see [Leads migrations 00026 to 00028](#leads-migrations-00026-to-00028)), with the new binary, before it puts that binary in place; a failed migration leaves the old release running.
3. `make deploy-data` (it copies `campaigns/` too), then `leads campaigns register`, as [Campaigns on the server](#campaigns-on-the-server) says.
4. Suppress the addresses and domains of people you already know (`leads suppress add address|domain VALUE`), before any finding.
5. Add `LEADHOUND_LEADS_TIMERS=on` to `/etc/leadhound/env` to turn finding on.
6. Check the first contacts run: it searches for contacts, enriches, rates and builds, and the console's List fills.
   To start it before 10:00, press Run now on Controls → Contacts; a plain `systemctl start` is passed over by the due-check until the job is due.
7. Rating is on by default and runs inside that unit once `LEADHOUND_LIVE=1` and `TYPESAFE_API_KEY` are in the env file; turn it off in Settings if you do not want it.
   Then label 30 leads under To label and run the eval, as [Relevance](#relevance) says.
   Each type needs its own: 30 Drupal calls for `leads relevance eval --decider jev`, and 30 café calls for `leads relevance eval --type cafe --decider jev`.
8. Before any campaign with a call step is batched: write your privacy notice, set `sender.privacy_url` in `leads.yaml` and run `make deploy-data` (the deadline is 7 Nov), and screen the Great Britain numbers against TPS and CTPS before the first GB call, as [Calls, screening and phone research](#calls-screening-and-phone-research) says.
9. Add a mailbox, assign it to its campaign, send its test and turn sending on, as [Mailboxes](#mailboxes) and the steps above say.

## Mailboxes

A mailbox is one email account the leads are sent from: an address, its login and password, its sending (SMTP) and reading (IMAP) servers, and its DKIM selector.
You can have as many as you like, each with its own warm-up, and you add, check, pause, resume and retire them on the console's Mailboxes page (in the top menu).
The same verbs run from the command line: `leadhound leads mailboxes list | add | check | pause | resume | retire`; editing a mailbox is in the console only.
On the server, add mailboxes through the console: `make remote` hands the command a pipe, not a terminal, so `leads mailboxes add` could not ask for the password without showing it; keep the command-line add for local use.
Each campaign sends only from the mailboxes assigned to it (see [Mailboxes per campaign](#mailboxes-per-campaign)).
A new lead goes to the ready assigned mailbox with the most room left, and its follow-ups always go from the same mailbox, in the same thread.
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
   It also opens `https://<the address's host>/` (redirects followed, at most 5, within 10 seconds) and keeps the address it lands on, which the mailbox and Campaigns pages show; a website that does not answer is a note, "The domain's website didn't answer.", never a failed check.
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

- **New leads** go only to a mailbox that is assigned to the batch's campaign, active, whose latest check passed, and whose records passed today.
  The 06:15 batch checks today's records itself before it drafts.
  The console's New batch never reaches the network, so before 06:15 it says "Today's domain check has not run for … yet": press Check now on the mailbox's page, or wait for the morning batch.
  With no mailbox at all, a batch is refused with "Add a mailbox first".
- **Each send** also needs that mailbox's reply sync to have just succeeded; the send run syncs first.
- A follow-up whose mailbox is paused waits; a retired mailbox's unsent follow-ups are cancelled, since a thread must come from the same address.

### Mailboxes per campaign

A live batch drafts only onto the mailboxes assigned to its campaign, and a message is sent only while its mailbox is still assigned to its batch's campaign.
Assign them on the console's Campaigns tab (Leads → Campaigns): each campaign lists every mailbox that is not retired with a tick box, its domain, its state and where its domain's website lands.
From the command line: `leadhound leads mailboxes assign ID CAMPAIGN` and `leadhound leads mailboxes unassign ID CAMPAIGN`, with the mailbox's number and the campaign's short name; `leads mailboxes list` ends each line with its campaigns, or "no campaign".
A mailbox's page lists its campaigns too, and says when it has none, so it sends nothing.

- **A campaign with no mailbox assigned** drafts no live batch: `leads batch new` is refused with "No mailbox is assigned to <campaign>: assign one on its Campaign page.", and the Campaigns tab says so.
  Shadow batches are unchanged.
- **An unassigned mailbox still counts toward its domain's daily limit**: it gets no new leads, but its sends today still use the domain's room.
- **A message whose mailbox is not assigned to its campaign** (approved before the assignment, or unassigned since) is held, not moved: the send reports it as "mailbox not assigned to the campaign" before it uses any room, checks again just before each send, and sends it once the mailbox is assigned again.
  A message never moves to another mailbox, so unassigning a mailbox holds its follow-ups.
- **Where each domain lands**: the Campaigns tab shows "<host> → <address>" for each mailbox, the host being the part of the mailbox's address after the `@`, from its latest check, and warns "<host> sends visitors to <address>, not to this campaign's site." when the address it lands on is not on the site of the campaign's `product.site_url`.
  A mailbox checked before this existed shows "where its domain goes is not checked yet": press Check now on its page.
- **The first deploy seeds the assignments**, so nothing stops: each mailbox that is not retired and whose live messages (selftests left out) all belong to one campaign is assigned to that campaign, as the system.
  A mailbox with no live message, or with messages of two campaigns, is left unassigned: assign it yourself.
  `leads selftest` needs no assignment.

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
Sending, on the Controls page (and the pause switch on the Leads page), stays the master switch above every mailbox.

### Sending a test, then turning sending on

1. Switch Morning batch off on the console's Controls page, so no batch is drafted before the test has passed, and assign the mailbox to its campaign on the Campaigns tab.
   Then add `LEADHOUND_OUTREACH=live` to `/etc/leadhound/env` and deploy, or run `sudo systemctl restart leadhound-console.service`.
   With `LEADHOUND_LEADS_TIMERS=on` in the file, the deploy enables the batch, send and sync timers; the send has nothing approved to send.
   The console reads the env file only when it starts, and until it sees live mode it treats sending as not set up and never drafts a batch; `make remote` reads the file afresh on every run, so the next steps need no restart.
2. Find the mailbox's number, then send one test email from it to one of your own addresses, `sender.operator_addresses` in `leads.yaml`:

   ```sh
   make remote HOST=<ssh host> ARGS='leads mailboxes list'
   make remote HOST=<ssh host> CONFIRM=1 ARGS='leads selftest --to <your address> --mailbox <its number>'
   ```

   `--mailbox` takes the number or the address, and is needed when more than one mailbox is not retired.
   The test runs the send's own order first: it stops while Sending is switched off, and it needs the key, a passed check, today's records and a reply sync of that mailbox, or it says "No selftest: mailbox N is not ready to send." and sends nothing.
   It ignores business hours and the daily limit, though its one email counts toward that day's sends, and it does not start the mailbox's warm-up ramp: only a real lead's send does.
   If the server refuses the login, the mailbox pauses itself as in any send, the test email is cancelled, and the command exits 1.
3. Reply to the test email from your own address; the reply lands in the mailbox's inbox, where it stays.
   The sync timer reads it within 15 minutes; to read it now, by hand:

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
   `leads sync` honours the Inbox sync switch, so switch it on first if it is off.
4. Switch Morning batch back on, and Sending if it is off, on the Controls page, as [Leads on the server](#leads-on-the-server) says.
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

LeadHound calls Claude to draft emails and manual texts, to find a contact for a starred lead and to find a business phone, so the jobs that do that need an Anthropic API key.
The key is a file on the server, `/etc/leadhound/anthropic.key` (`root:root 0600`), holding the key alone on one line.

- **Place it by hand.**
  Make the key in the Claude Console, in a workspace of its own.
  Then ssh to the server, open the file in your own editor as root (for example `sudo -e /etc/leadhound/anthropic.key`), paste the key, save, and run `sudo chmod 0600 /etc/leadhound/anthropic.key`.
  Never type the key into a command line, where it lands in your shell history, and never paste it into a chat.
- **Who reads it.**
  Only the units that call Claude get it, as a systemd credential (`LoadCredential=anthropic-key:/etc/leadhound/anthropic.key`): `leadhound-leads-batch.service`, because drafting runs at batch time, `leadhound-leads-research.service` (see [Contact research](#contact-research)) and `leadhound-leads-draft.service` (see [Draft it](#draft-it)).
  No other unit gets it, and never the console; `make remote` hands it on only when you add `REMOTE_CLAUDE=1` (see "Running a Claude command by hand" below).
  This is why it is not a line in `/etc/leadhound/env`: the env file reaches every unit, the console included, and a credential reaches only the units that need it.
- **No `ANTHROPIC_API_KEY` line is read anywhere.**
  LeadHound reads the key only from `$CREDENTIALS_DIRECTORY/anthropic-key` on the server or from the file `LEADHOUND_ANTHROPIC_KEY_FILE` names locally, and never from the environment.
- **The console refuses to start** when it can see the key: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or `LEADHOUND_ANTHROPIC_KEY_FILE` set to anything, or an `anthropic-key` credential handed to it.
  It exits 2, naming what it found, and systemd does not restart it.
- **Every deploy** makes the file `root:root 0600` again if it is there, and never creates, reads or prints it.
  Until you place it, the deploy says `install: no Anthropic key at /etc/leadhound/anthropic.key`, and `leadhound-leads-batch.service`, `leadhound-leads-research.service` and `leadhound-leads-draft.service` cannot start, because systemd fails a unit whose `LoadCredential=` file is missing.
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
`leads drafts sample`, `leads drafts make`, `leads drafts eval --decider claude`, `leads phones research` and `leads research run` call Claude, and `make remote` hands the Anthropic key to none of them unless you add `REMOTE_CLAUDE=1`, for example:

```sh
make remote HOST=<ssh host> CONFIRM=1 REMOTE_CLAUDE=1 ARGS='leads phones research --campaign drupal-eol --max 3'
```

`REMOTE_CLAUDE=1` adds `-p LoadCredential=anthropic-key:/etc/leadhound/anthropic.key` to the run, the same credential the batch, research and Draft it units get.
It refuses, before anything runs, without `CONFIRM=1` (it spends money), for the console (`serve`, `console …`), and for a command that gets the mailbox key (no command holds both keys).

Each prints what it spent; a run stops at the caps below.

### Claude spend caps

Every Claude call is priced before it is made, at its worst case, and refused when that would pass a cap; after the call its real cost is recorded, per lead and per purpose.

- `LEADHOUND_CLAUDE_DAILY_USD_CAP` in `/etc/leadhound/env` (in US dollars) is the ceiling for one day across every purpose.
- Each purpose (drafting, research, and the drafts eval when its decider is Claude) has its own daily cap under that ceiling, and a cap per lead checked against what that lead has already cost under the purpose; the drafting per-lead cap leaves room for the one retry a refused draft gets.
- The ceiling's default and the current per-purpose and per-lead caps, and the drafting writer's output ceiling they are sized against, are in `deploy/env.example`; only the ceiling is set in the env file, the per-purpose and per-lead caps are built in and the same for every campaign, and a campaign spec cannot set its own caps yet.
- A refused call costs nothing and is logged; the dashboard and the Campaigns tab show today's spend against the ceiling.
- A batch that reaches a cap drafts the remaining leads from templates and says how many and why.

## The Reoon key

The email check (`leads verify`) asks Reoon about each address, so it needs a Reoon API key.
The key is a file on the server, `/etc/leadhound/reoon.key` (`root:root 0600`), holding the key alone on one line, handled exactly as [the Anthropic key](#the-anthropic-key) is.

- **Place it by hand**: make the key in your Reoon account (Dashboard, API), then on the server `sudo -e /etc/leadhound/reoon.key`, paste, save, `sudo chmod 0600 /etc/leadhound/reoon.key`.
  Never type it into a command line or paste it into a chat.
- **Who reads it**: only `leadhound-leads-batch.service`, `leadhound-leads-verify-balance.service` (the credits for the Status page, below) and `leadhound-leads-verify-now.service` (Check email now, see [Manual Send now](#manual-send-now)), as the systemd credential `LoadCredential=reoon.key:/etc/leadhound/reoon.key`; no other unit, never the console, and it is never a line in `/etc/leadhound/env`.
  LeadHound reads it from `$CREDENTIALS_DIRECTORY/reoon.key` on the server, or locally from the file `LEADHOUND_REOON_KEY_FILE` names (made `0600`; a file others can read is refused).
- **Without the key the batch still runs.**
  The unit also has `SetCredential=reoon.key:`, an empty default systemd uses when the file cannot be read, so the unit starts; `leads verify` reads the empty credential as no key, prints that nothing was checked because the key is not at `/etc/leadhound/reoon.key`, records its run, and Controls says the last run checked nothing for that reason.
  While the Email check is on, a live batch then drafts no email lead whose address was never checked, so switch the Email check off on Controls until the key is in.
  That default relies on systemd treating a `LoadCredential=` that cannot be read as not fatal when the same name has a `SetCredential=`; check it once on the server before you rely on it (place no key, press Run now on Controls → Morning batch, and read `journalctl -u leadhound-leads-batch.service` for the "no Reoon key" line).
- **Every deploy** makes the file `root:root 0600` again if it is there, and never creates, reads or prints it; until you place it, the deploy says `install: no Reoon key at /etc/leadhound/reoon.key`.
- **Every check needs `LEADHOUND_LIVE=1`** as well as the key; without it `leads verify` prints one line saying no Reoon call was made and exits 0.
- **To rotate**, make a new key in Reoon, replace the file's contents, and the next run uses it; then delete the old key in Reoon.
- **Credits.**
  Reoon gives 20 checks a day free; daily plans start at 500 a day, and one-off credits never expire (re-check the prices on Reoon's site before buying).
  `LEADHOUND_REOON_DAILY_CAP` in `/etc/leadhound/env` (default 200) is LeadHound's own ceiling for one day, counted from the day's stored checks.

### The email check

While the Email check job is on, every address must pass a Reoon check before it can enter a live batch, so a dead address never costs a mailbox its reputation.

- **`leads verify [--max N]`** reads the credits first and stops with "No Reoon credits left today." when none of either kind is left.
  Then it takes the email addresses the next live batch of each active campaign would draft, in the batch's own order (starred first, then the outreach order), skips every address with a fresh answer, and checks the rest two at a time, at most `--max`, the day's cap and the credits left.
  Each answer is stored (`email_check`, append-only); the run is recorded with how many were checked, may be sent and are blocked, and the credits left.
  A check that fails (Reoon down, an error answer) is counted, stores nothing, and the address is tried again on the next run.
- **What each answer does**, in one place (`internal/verify`, whose comment records Reoon's own meanings):
  safe and role addresses (info@, contact@) are sent; invalid, disabled, disposable, inbox full and spam trap are blocked; catch-all is sent unless Leads → Settings → Email check says to leave catch-all addresses out (`verify.catch_all: block` in `leads.yaml` does the same); an unknown answer is sent.
  An answer older than 90 days counts as no answer, so the address is checked again.
- **Your own addresses** (added by you, or marked as yours) stand on your word and no mail server check, so the live send asks for a passing answer at the moment of sending, on every road: the hourly send, `leads send --now` and Send now, whichever batch the message came from, and with the Email check on or off.
  Without one the message stays approved and unsent, takes none of the day's room, and the run prints why and what to do and notes `email check`; it goes on the run after a pass, and a failed answer holds it the same way until you retire the address.
  `leads verify` asks for these addresses first, because their leads have left the batch queue and Check email now covers only an unapproved batch.
  With the Email check off nothing can check them, so they wait until you switch it on, and the Send now confirm page stops them with that reason.
  Other addresses behave as described above; shadow runs and selftests are not asked.
- **The batch's gate.**
  A live lead whose email goes to its own address needs a sendable fresh answer for it.
  A blocked address makes the lead fall to its next route, as a missing email does when the list is built: another email address of the same domain, judged the same way, or a contact form; the lead is then drafted there and its page shows that route.
  With no other route it is skipped, with the line "skipped <domain> (<kind>, email): email check: <answer>", the answer in plain words (catch-all, inbox full, role address, invalid, …).
  An address with no answer yet waits for a later batch, and the batch says "N leads wait for their email check."
  A call-first lead's emails go to the address given on the call, so its route is not gated.
- **Switched off**, batches draft exactly as before the check, and Controls says "Email check is off: addresses are not checked before sending."
- Controls' Email check row shows the switch, the last run, today's checks (checked, may be sent, blocked) and the credits Reoon had left at the last run; each email address on a lead's page shows its latest answer and when.

### The credits on the Status page

The console holds no key, so the Status page reads Reoon's credits, and Claude's last refusal, from a database row (`service_status`) the jobs that hold the keys write.

- **`leads verify --balance-only`** reads the credits and stores them, or Reoon's error in one line; it checks no address, spends no credit and writes no run row.
  A Reoon error is a reading the Status page shows, so the command still exits 0; only a crash or an unreachable database fails it.
  Without `LEADHOUND_LIVE=1` or without the key it calls nobody, records nothing and says so.
  It does not read the Email check switch: the credits stay current while the check is off.
- **`leadhound-leads-verify-balance.service`** runs it, with the Reoon key alone (the same `LoadCredential=`/`SetCredential=` pair as the batch unit) and the batch's sandbox.
  `leadhound-leads-verify-balance.timer` starts it every 30 minutes, and `leadhound-leads-verify-balance.path` when the Status page's Refresh button writes `run/leads-verify-balance`; both are enabled on every deploy, whatever the leads switches say.
  The morning batch's check run stores the credits too (both its balance reads), so the page is fresh after each batch.
- **Claude** writes its row when a call is refused for money: a daily cap (of a purpose, or across all Claude work) refused it, or Anthropic answered that the account's spend limit or credit is used up.
  A lead's own cap and a cap set to 0 are not written: the first is one lead's, the second a switch.
- If Refresh stays "asked and not started", reset the watcher: `sudo systemctl reset-failed leadhound-leads-verify-balance.path leadhound-leads-verify-balance.service`, then `sudo systemctl start leadhound-leads-verify-balance.path`.

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
- **LinkedIn.**
  A star on a lead with no channel also looks for its LinkedIn page, and so does Look again on a form lead or a lead with no channel; `leads research request LEAD --linkedin` asks the same by hand, and without the flag it stays email and phone.
  A country written to by form only, where calls are not allowed, queues LinkedIn alone.
  An attempt that did not look for LinkedIn does not count against the 90 days when the new request does, once.
  What counts as found is in [LinkedIn and your own contacts](#linkedin-and-your-own-contacts).
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

## Leads migrations 00026 to 00028

`make deploy` applies them with the new binary, as the owner role, before the binary is put in place.

| Migration | Adds |
| --- | --- |
| `00026_service_status.sql` | `service_status` (the last Reoon and Claude reading or error, one row per provider) and `claude_topup` (the top-ups the Status page's credit estimate counts from) |
| `00027_send_override.sql` | `outreach_batch.manual`, `send_request` (each manual confirm, claimed once) and `send_override` (each message sent past a warning); both check their codes against the closed set |
| `00028_crm.sql` | `lead_crm` (stage, lost reason, note, next action and its date, deal value and currency, tenant link, snooze), `lead_stage_event` (every stage change), `mailbox_event.snippet` (at most 300 characters) and `call_outcome.call_back_on` |

A database that applied an earlier draft of 00027 during development needs it rolled back and applied again; production has not run any 00027 yet.

## Leads migration 00029

`make deploy` applies it with the new binary, as the owner role, before the binary is put in place.
It adds outreach by hand:

| Adds | For |
| --- | --- |
| `lead_channel` row `linkedin`, `contact_route.kind` `linkedin`, `contact_route.person_id` | LinkedIn as a channel, and the person a route belongs to |
| `org_person`, `contact_mark` | people at an organisation, and the append-only claim and retire marks on a route, fact or person |
| the `operator@1` marker on `contact_route`, `org_fact` and `org_person` | a row you typed in, which came from no page and no place |
| `message_edit` | each saved edit of a draft, the text before and after |
| `org_sector`, `proof`, `proof_retire` | a lead's sector and your proof library |
| `lead_proposal`, `lead_proposal_status` | proposals and their statuses |
| `draft_request`, `outreach_batch.origin` | Draft it, and what made each batch; existing manual batches become `send_now` |
| `draft_generation.channel`, `contact_research.want` may name `linkedin` | manual drafts kept out of the email gate, and research that looks for LinkedIn |

It also copies every stored LinkedIn company page fact into a `linkedin` route, with that fact's own source, and a value already there is left alone.
Every new table is append-only for the app role, apart from a `draft_request`'s status, reason, batch and times.

**The app role can now re-hash an unapproved batch.**
An email edit has to change the text the batch's hash covers, so the app role gains `UPDATE` on `outreach_message (subject, body_text)` and `outreach_batch (content_sha256)`.
The grants name columns only, so the limit to an unapproved batch is the `WHERE approved_at IS NULL` in the statement itself, and an edit after approval is refused there.
You still approve the hash you are shown, and an edit after you read it makes that approval fail.
The send-time recheck of each message against its `approved_sha256` stays the backstop.
A form or LinkedIn edit touches none of these: it writes only `message_edit`, so an approved batch is never called changed.

## Leads migration 00030

`make deploy` applies it with the new binary, as the owner role, before the binary is put in place.
It adds the List's triage:

| Adds | For |
| --- | --- |
| `lead_place_type` | a café lead's place type (`cafe` or `roastery`, or `unclear` from Jev or Instagram), from its name, Jev's website read, Instagram's read (since migration 00032) or you, with the matched word or question and, for Jev, the input hash |
| `roastery_outreach_switch` | Roasteries in café outreach on Controls; no row means off |
| `lead_event.skip_label_id` | the "no" call a skip from the List wrote, so Undo clears only that call |

Both new tables are append-only for the app role.
With the switch off, a roastery or online coffee store is held: kept out of café batches, Send now, the call queue and Draft it, and a pending message to one is cancelled when the send reaches it.
A held lead that was batched with nothing sent goes back to new, so it is kept for later.

Before the first production café batch, in this order:

1. `make remote HOST=… ARGS='leads place-types report --campaign cafes-loyalty'` counts the leads whose names carry a roastery word and prints a sample; it writes nothing, so it needs no `CONFIRM=1`.
2. `make remote HOST=… CONFIRM=1 ARGS='leads place-types apply --campaign cafes-loyalty'` stores those name tags.
3. The first Jev pass: `make remote HOST=… CONFIRM=1 ARGS='leads rate --campaign cafes-loyalty'`, which after the relevance pass also asks each open café lead the place question once, about 2,300 Jev calls, cents of input (see [Relevance](#relevance)).
   It needs `LEADHOUND_LIVE=1` and the Jev key, as every rating does; the nightly contacts unit asks only leads that are new or changed after that.

`leads transfer` files carry no place types, so run `leads place-types apply` after an import; the café build tags each new lead by its name on its own.
The build also skips a lead that is the same place as another (same normalised name, address and town) as the system, keeping one; its Undo is refused, because the next build would skip it again.
`MarkContacted` stays open for a held lead, because it records something you already did yourself; research on a starred roastery stays allowed for the same reason.

## Leads migration 00031

`make deploy` applies it with the new binary, as the owner role, before the binary is put in place.
It adds Instagram signals on café leads and Instagram as a manual channel (README "Instagram signals"):

| Adds | For |
| --- | --- |
| `ig_profile`, `ig_follow` | each imported capture of a profile (a handle no longer there is stored as `missing`), and each coffee account kept from a following list (and, since migration 00033, from a vendor's followers list, by its `direction`), whose name is kept only when it is matched or on the review list |
| `ig_match` | what each handle matches: by handle or website (strong), by name (weak), your own call, or nothing; tied leads are kept as its candidates |
| `ig_judgment` | Jev's and the rules' answers on each profile, with Jev's own choice, its confidence and the input hash |
| `ig_signal_hint` | weak hints; today only "followed by a loyalty vendor's own account" |
| `ig_review_status` | the review list's statuses (new, dismissed, matched, wanted) |
| `lead_channel` row `instagram`, `contact_route.kind` `instagram`, and `instagram` in the channel checks of `draft_generation`, `draft_request` and `outreach_message` | Instagram as a manual channel |
| the `instagram@1` marker on `contact_route` | a route the import or your match wrote, which came from no page and no place; allowed on an `instagram` route only |

It also copies every stored Instagram profile link (an `org_fact` social row) into an `instagram` route, with that fact's own source, in one canonical form: the profile's `www` address on Instagram, ending in `/<handle>/`.
Instagram's own pages (posts, reels, explore, stories) are no profile and are skipped, and a value already there is left alone.
All six new tables are append-only for the app role (`leadhound migrate` and `deploy/sql/grants.sql` revoke `UPDATE` again after every migrate); only `leads forget`, as the owner, deletes from them.
Instagram routes are a business's own handle, never a person's, so `contact_route_person_check` is unchanged, and contact research never looks for Instagram.
Its Down refuses while any Instagram message, generation or draft request exists; otherwise it moves Instagram leads to channel `none`, deletes every `instagram` route and drops the six tables.

The capture files are made on the operator's machine (README "Instagram signals"), and an import needs the file on the same side as the database.
On the server: copy the file to a path the `leadhound` user can read, as for `boards import`, then `make remote HOST=… CONFIRM=1 ARGS='leads ig import <path> --max-calls N'`.
The judgment hashes live in each database, so a production import asks Jev again for every profile with text, within `--max-calls` and the leads daily cap; it needs `LEADHOUND_LIVE=1` and the Jev key, as every rating does.
`leads ig report|lookups|review` are free on `make remote`: they write nothing, so `make remote HOST=… ARGS='leads ig report'` (or `lookups`, `review`) needs no `CONFIRM=1`.
`lookups --place-type unclear` lists the café leads Jev's place question left unclear, with the handles each already holds.

## Leads migration 00032

`make deploy` applies it with the new binary, as the owner role, before the binary is put in place.
It makes Instagram a fourth source of a café lead's place type (README "Instagram signals"):

| Changes | For |
| --- | --- |
| `lead_place_type_source_check` | allows `instagram` beside `name`, `jev` and `operator` |
| `lead_place_type_unclear_check` | allows `unclear` from `instagram` as well as `jev`: the row that says Instagram is silent now |

No table or grant is added, and the input-hash check is unchanged: an Instagram row carries no hash.
The ranking is your call, then Instagram's read, then Jev's website read, then the name.
Each ALTER waits at most 5 seconds for its lock, because every queue read uses this table, so a busy moment fails the migrate instead of stalling the queue; deploy again once it is quiet.
Its Down refuses while any `instagram` row exists, because erasing Instagram's place types is a decision, not a side effect of a rollback; with none, it restores both old checks.

The deploy itself writes no place type, so a lead already matched on Instagram keeps its old type until the first sync.
After the deploy, on your yes as for the deploy itself, because it changes which leads are held:

1. `make remote HOST=… CONFIRM=1 ARGS='leads ig place-types --dry-run'` prints what it would write (roastery, café and silent counts, then the leads Instagram would release and hold).
   It makes the same writes and rolls them back, so nothing stays, but `place-types` is not on `make remote`'s free list, so even the dry run needs `CONFIRM=1`.
2. `make remote HOST=… CONFIRM=1 ARGS='leads ig place-types'` writes them.

It makes no Jev call and reads no capture file, so it needs neither `LEADHOUND_LIVE=1` nor the Jev key, and a second run writes nothing.
From then on every `leads ig import`, `leads ig match` and `unmatch`, and the console's It's this lead keep it current.

## Leads migration 00033

`make deploy` applies it with the new binary, as the owner role, before the binary is put in place.
It adds loyalty intent on café leads (README "Loyalty intent"):

| Adds | For |
| --- | --- |
| `ig_follow.direction` | `following` (the list owner follows the account; every row before 00033) or `followers` (the account follows the list owner); the capture key takes the direction |
| `ig_vendor_touch` | one dated contact between a vendor's account and another account: a comment on the vendor's post, a tag or @-name in it, or the account's own post tagging the vendor; a comment's text (at most 300 characters) is kept only for an account matched strongly at its import, a name only for a matched or reviewed one |
| `lead_intent` | each café lead's derived class under `intent@1`, with its overlap, score, reason, signal date and, for `just_chose`, the date it is held until; a row only when the class changes |
| `lead_intent_call` | your own call on a lead's class (`leads intent set` and `clear`), which wins over the derived one |

All three new tables are append-only for the app role (`leadhound migrate` and `deploy/sql/grants.sql` revoke `UPDATE` again after every migrate); only `leads forget`, as the owner, deletes from them.
The ALTERs on `ig_follow` wait at most 5 seconds for their lock, because every import reads it; deploy again once it is quiet.
Its Down refuses while any `lead_intent_call`, `ig_vendor_touch` or `followers` row of `ig_follow` exists, because your calls and the vendor reads would be lost; with none, it drops the three tables, restores the old capture key and drops the column.

The deploy itself writes no class, so every café lead reads `cold` until the first sync.
After the deploy, on your yes as for the deploy itself, because it changes which leads are held and the café batch order:

1. `make remote HOST=… CONFIRM=1 ARGS='leads intent sync'` writes each café lead's class and prints the count per class and how many changed (`--dry-run` says the same and writes nothing; it needs `CONFIRM=1` too).
2. `make remote HOST=… ARGS='leads intent report'` lists the leads per class; it writes nothing, so it needs no `CONFIRM=1`.

The sync makes no Jev call and reads no capture file, so it needs neither `LEADHOUND_LIVE=1` nor the Jev key, and a second run writes nothing.
From then on every `leads ig import`, `leads ig match` and `unmatch`, the console's It's this lead, `leads intent set|clear` and each café batch build keep it current.
Vendor reads come in as `ig-capture@2` capture files, imported as in "Leads migration 00031"; a new vendor in `signals.yaml` changes Jev's `ig_vendor` options, so the next import asks Jev again about every profile with text, within `--max-calls`.

## Leads migration 00034

`make deploy` applies it with the new binary, as the owner role, before the binary is put in place.
It adds the business kind behind the Leads list's one-line description (README "Business kinds"):

| Adds | For |
| --- | --- |
| `lead_business_kind` | Jev's pick of what an open lead's business is, from the closed list of `business-kind@<n>`, with its confidence, the question and the input hash; the latest row of a lead decides, and a lead with no row has no kind |

The table is append-only for the app role (`leadhound migrate` and `deploy/sql/grants.sql` revoke `UPDATE` again after every migrate); only `leads forget`, as the owner, deletes from it.
Its Down drops the table with no refusal, because the rows are derived and the next rate run asks again.

The deploy itself asks nothing, so every lead's line reads without a kind (a café falls back to its place category) until its first rating.
The first contacts run after the deploy asks the business-kind question once of every open lead in both campaigns, about 3,450 Jev calls, which is about $0.27 at the earlier observed rate of $0.11 per 1,400 calls; each later run asks only leads whose facts changed.
The pass runs inside `leads rate`, after relevance and the café place-type pass and only when both ended clean, within the same `--max` and the leads daily cap, so relevance always spends first.
To run it now rather than wait for the nightly contacts unit, on your yes:

1. `make remote HOST=… CONFIRM=1 ARGS='leads rate --campaign cafes-loyalty'`
2. `make remote HOST=… CONFIRM=1 ARGS='leads rate'` (the Drupal campaign, the default)

Each needs `LEADHOUND_LIVE=1` and the Jev key, as every rating does, and prints a `business kind:` line (named, unclear, unchanged, still due, calls, failed calls and the stop reason) after the place-type line.

Before the first production run, check the Settings value `relevance.leads_daily_calls` (`make remote HOST=… ARGS='leads settings show'`, or Settings in the console).
The Drupal line now spends more of the shared daily cap before the café line runs: at the 10,000 default this does not matter, but a low value set in the console would leave café relevance short of calls.

### When card words ship

A `card:` block in a campaign file (README "Campaigns") changes that spec's hash, and a binary older than this release refuses the `card:` key outright, because the spec decoder rejects unknown keys.
Until the new version is registered, `leads batch new` and the place import refuse that campaign with "changed since it was registered", and the console keeps showing the registered spec, so the words do not show either.
Ship card words in this order:

1. The binary: `make deploy HOST=… CONFIRM=1`, so the server reads the `card:` key.
2. The data: `make deploy-data HOST=… CONFIRM=1`, after committing and pushing the private files.
3. The register: `make remote HOST=… CONFIRM=1 ARGS='leads campaigns register'`, because until then that campaign's batches are refused.
4. A check that the plan passes: there is no dry-run batch, so run the register line once more and see that it prints `<slug>: unchanged`, which means the file on the server matches the registered version the batch checks.

Skipping step 3 stops the morning café batch, so card words ship with the register step or wait until after a batch day.
Every later edit of the words re-arms this.

## LinkedIn and your own contacts

- **LeadHound's leads side never fetches LinkedIn's own site.**
  Every leads fetch client (`siteClients`: the probe, contact search, enrichment, research, phone research and the agency import) refuses LinkedIn's host and its subdomains, on the first request and on every redirect, whatever the live setting says.
  The job-search side is unchanged: `collect linkedin`, `canary linkedin` and the brief's recheck read LinkedIn on purpose, through their own clients.
- **Where a link comes from.**
  A LinkedIn link counts only as an `href` on a page fetched from the organisation's own domain.
  Enrichment keeps a company page; contact research may keep a company or school page, or a person's profile with a role title printed beside the link.
  A name is stored only if it is printed beside the link, and only in `org_person`.
- **What is stored**: a `contact_route` of kind `linkedin`, whose value is one canonical form with no `www`, a lower-case slug and no query or sub-page, so a link is never stored twice for one organisation.
  A person's profile also points at its `org_person` row.
  A lead may use either; among your own rows a person's profile outranks a company page, and among research's rows the company page comes first, as the safer default.
- **Your own contacts** carry `derived_by = 'operator@1'` and no page or place, plus a `claim` mark in the same transaction.
  Marks are append-only and the latest per row decides: `claim` is yours and carries your note, `retire` is gone.
  A value exists once per organisation, so adding one already there writes a claim on that row, and changing a value is a new or claimed row plus a retire on the old one, in one transaction.
- **Your email skips two checks.**
  The role-address filter does not apply, and the address counts as having a working mail server, because the console's unit cannot look up DNS (`IPAddressDeny=any`); `mx_ok` is stored false.
  So the email check is its only gate: see [The email check](#the-email-check).
  The email check before any send stays the gate; see [The email check](#the-email-check).
- **Retiring a contact** cancels every unsent message or call aimed at it, with the reason "contact retired", so a sequence never stalls on a value that is gone.
  A batched lead with nothing live left returns to new, and the send path skips a retired address.
  A claim does not revive what was cancelled: Draft it makes a new draft.
  Research, the crawl and enrichment never bring a retired value back; only your claim does.
- **`leads transfer`** skips retired routes, and marks do not cross, so a claimed route crosses unclaimed.
  A lead on a route with no page crosses with no route and channel `none`.
- **Erasure** covers all of this; see [Erasure](#erasure).

## Instagram channel

- **LeadHound never reads Instagram.**
  No leads command fetches it: profiles reach the database only through `leads ig import` of a capture file the operator's own browser wrote.
  The leads fetch clients do not refuse Instagram's host the way they refuse LinkedIn; nothing in the leads loop asks for it.
- **Where a route comes from**: a social link on the organisation's own site or map listing (copied by enrichment and by migration 00031), a strong Instagram match by handle or website, your own `leads ig match`, or a contact you add.
  The value is always that canonical profile address, lower case, so one handle is never stored twice for an organisation.
  An import writes a route only when the organisation holds no live Instagram route, so a route you added or claimed is never replaced, and a weak match never writes one.
- **One direct message.**
  An Instagram lead gets one Claude-drafted DM, step 0, with no follow-up and no connection step: copy it, send it by hand as a direct message on Instagram, then Mark sent, which moves the lead from batched to contacted.
  The DM holds no link and no web address (Instagram flags links in a first message), no greeting and no sign-off, and it is at most `sender.instagram_dm_max` characters (`leads.yaml`; 1 to 1000, 500 when left out); a longer draft is refused like an over-long LinkedIn note.
  A kind's `<lang>/instagram.txt` template is optional; with none, the lead has only Claude's draft.
- **Café campaigns only.**
  The manual order is form, LinkedIn, then Instagram, and Instagram is picked only for a café campaign, so a Drupal lead whose organisation has an Instagram route stays on its form, LinkedIn or email.
  A lead held as a roastery is refused on Instagram as on every other road.
- **Meta's messaging API cannot open a cold DM**, so the channel is manual by design: LeadHound never sends one.
- **Erasure** covers Instagram; see [Erasure](#erasure).

## Reply snippets

Reply sync used to keep no text of a reply.
Since 00028 it keeps one thing for the Replies page: a snippet on a `reply` event matched to a lead, at most 300 characters.
It is the reply's opening plain text: the first text part is decoded, HTML is stripped, and quoted history (attribution lines, quote headers, lines starting with `>`), the signature, phone and webmail footers and our own lines (the identity and opt-out lines) are taken out, then whitespace collapses and a longer text is cut on a word.
Any other event, and a reply matched to no lead, keeps no snippet, and nothing else of any body is stored.
`leads forget org` empties the snippets of the organisation's lead and deletes its CRM rows, as [Erasure](#erasure) says; a backup written before then keeps them until it ages out.

## Erasure

`leads forget org ID --reason TEXT` erases one organisation's personal data when someone asks, in one transaction: its phone facts, phone research and screenings, its call notes, its kept page bodies, its description and social facts, and the phones and emails of its map listing.
It also suppresses the organisation's domain and its numbers (as hashes, with the source `forget: <reason>`), so it is never collected, enriched, researched or called again, moves its leads to suppressed and cancels their pending messages.
It also erases its lead's CRM: every reply snippet of its mailbox events is emptied (the event rows stay), and its `lead_crm` row (note, next action, deal, tenant link) and its stage changes (`lead_stage_event`) are deleted; the counts line names them.
It also erases outreach by hand, in this order: the organisation's contact marks; any lead's use of a person's route (the lead goes to channel `none`); those routes and its `org_person` rows, with the pages people were read from and the email checks of their addresses; the before and after text of its draft edits, the subject and body of its edited emails, and any form or LinkedIn target that was a person's profile, or an email target that was a person's address.
Then its proposal titles become `(erased)`, its proposal notes are blanked, and its sector and Draft it request rows are deleted.
The counts line names each.
It also erases Instagram (migration 00031), in this order: any lead's use of the organisation's Instagram routes (the lead goes to channel `none`, because `lead.route_id` has no cascade); the target of its Instagram messages (the rows stay); its `instagram` routes.
Then, for every handle matched to one of its leads and every handle of its routes (a vendor's own account and a handle that names only other organisations' leads excepted), it deletes the `ig_judgment`, `ig_signal_hint`, `ig_match`, `ig_review_status`, `ig_profile` and `ig_follow` rows, the accounts read from that handle's following list included, and any other match naming one of its leads; another capture's lead hint naming one of its leads is cleared.
Since migration 00033 it also deletes every `ig_vendor_touch` row of those handles (as the touching account or as the vendor's own), and its leads' `lead_intent` and `lead_intent_call` rows.
Since migration 00034 it also deletes its leads' `lead_business_kind` rows.
The domain suppression keeps the organisation out of every later import's matching, so importing a capture file again attaches nothing to it and writes no route.
**Capture files are out of the database's reach**: a file under `data/instagram/` on the operator's machine, or one copied to the server for an import, still holds the profile as it was read; delete it by hand, or keep it out of any later import.
Last it re-hashes each batch those changes reach, because a batch's hash covers every row: `content_sha256` is recomputed, and on an approved batch `approved_hash` moves with it, since the two are one value at approval and the Send now checks refuse a difference.
The hash a page showed before the erasure is refused, so reload the page; each other lead's approved message stays bound by its own `approved_sha256`, which the erasure does not touch and the send compares byte for byte.
A batch that already failed its hash before the erasure is left as it is and stays refused.
It does not yet empty the bodies of its unedited email messages, or the values of routes that belong to no person, Instagram routes excepted.
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

### The console API

The console can also answer a small JSON API, for a local tool that works through the jobs from your own machine.
It is off until `/etc/leadhound/env` holds a `LEADHOUND_API_TOKEN_SHA256` value; while it is off, every `/api/` path answers 404.

| Route | What it does |
| --- | --- |
| `GET /api/jobs/next?limit=N&days=D` | Open jobs from recent briefs still at `new` or `shortlisted`, with earlier jobs at the same company, under the active skip rules (`limit` defaults to 20, at most 50, and counts visible jobs only; `days` defaults to 14, at most 60). The answer also holds `hidden`, what the rules left out, and `more_visible`, the visible jobs past the limit. |
| `GET /api/jobs/{id}` | One job's page: description, criteria, unclear points and history. |
| `GET /api/tracked?status=S` | The tracked jobs at one status (default `applied`). |
| `POST /api/jobs/{id}/track` | Marks a job, with the body `{"action":"applied\|skip\|maybe","reason":"..."}`. |

Every route needs `Authorization: Bearer <token>` and answers JSON, errors included.
A wrong or missing token answers 401, never a redirect to the sign-in page.

**What the queue hides.**
The next route applies the same skip rules as the ranker, before the limit, so a full page of real jobs still comes back; a flag rule, a disabled rule and a stored pattern that does not compile hide nothing.
`hidden` holds every job a rule left out, with the rule's reason, and `briefed` says which of the two kinds it is.
With `briefed` true it came from a brief of those days; with `briefed` false it was first seen in the last 30 days and is in no brief of those days.
A job only other gates fail is not in that second kind: those gates keep it from every brief whatever the rules say.
That second kind also lists a job whose current observation has a stored rule-gate failure, under that rule's reason, because the ranker goes on skipping it: after the nightly trim blanks the description a rule read, and after a rule is disabled, until a newer observation replaces the stored failure.
A job from a brief is read by the live rules alone, so disabling a rule puts it straight back in the queue.
On the local tool, `leadhound api next` adds one stderr line when anything was hidden, and `leadhound api next --hidden` lists it all.
A rule on `description` is what makes this read slow, because the server reads the text of every job in that second kind, so `api next` waits up to 25 seconds where the other commands wait 10.

**To bring a hidden job back**, run `leadhound api mark ID maybe`: a job whose latest status event is yours is never hidden.
One that reached no brief then lists after the briefed jobs, with an empty brief day, however old it gets.

**To add a rule**, try it first, which writes nothing: `leadhound rules test --field title --pattern 'plain words' [--days 30]` prints what it would hide from the queue and what reached no brief of the last 14 days.
It can undercount: it cannot see a description the trim has already blanked, and it leaves out a job another gate fails.
It counts this pattern alone, so another rule's stored failure is not in its lists.
Then `leadhound rules add --kind skip --field title --pattern 'plain words' --reason 'why'`.
Skip rules shrink the morning brief as well.
`rules disable` does not fully undo a rule: its failure is stored on each new observation and the ranker keeps skipping until a newer observation replaces it, and an ATS posting that fails is never asked for its detail.
Until then a job that reached no brief stays in `hidden`, and a job from a brief shows in the queue while the brief still skips it.
So start narrow, and prefer `--kind flag` when unsure.

**To turn it on**, do this before you deploy, so the deploy's own console restart picks up the hash and you are not signed out a second time:

1. On your machine, run `make api-token URL=https://console.example`, with the console's public URL.
   It writes the URL and a new token, mode 0600, to `data/api.env` in the repository's ignored `data` directory, and prints only the token's hash, so the token itself never appears in your terminal.
2. Add the printed `LEADHOUND_API_TOKEN_SHA256=<hex>` line to `/etc/leadhound/env`.
3. Deploy (`make deploy … CONFIRM=1`), or, with no deploy due, restart the console: `sudo systemctl restart leadhound-console.service`.

A value that is not 64 hex characters stops the console at start with a line naming the variable, so the deploy's wait for the console fails and shows its log.

**To rotate the token**, run `make api-token URL=https://console.example FORCE=1`, replace the hash line on the server, and restart the console.
Without `FORCE=1` the existing `data/api.env` is never overwritten.
The local tool answers 401 from the moment the file changes until the console restarts, and the old token stops working at that restart.
The restart signs you out of the console, as every restart does.

**To turn it off**, remove the hash line from `/etc/leadhound/env` (an empty value does the same) and restart the console.

**What the token opens.**
It opens the `/api/` routes above and nothing else: not the console's pages, and not its other actions, so adding a skip rule stays a signed-in action.
A signed-in session does not open `/api/`, so the password and the token stay separate.
A token in a query string is never accepted, so nothing needs to put it in a URL, where a log or a browser history would keep it.
`POST /api/jobs/{id}/track` writes, with the same effect as marking the job in the console, so keep `data/api.env` as you would a password.

**Access logs.**
This repository holds no Caddyfile, and the server's `log` settings are not in it.
Caddy's documentation says `Authorization` is logged as `REDACTED` in access logs by default, unless the `log_credentials` global server option is on.
So check the server's Caddyfile for `log_credentials` and for a `log` block that logs that header differently, before you rely on this.

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
