# Deploying leadhound to a server

leadhound runs on one Linux server with systemd, from timers.
Nothing in this directory names a server: the host is an ssh destination in your own ssh config, passed as `HOST=` on the command line, and every secret lives in files on the server.

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

Check the watchers with `systemctl list-units 'leadhound-*.path'` (each should be active, waiting) and a run with `journalctl -u leadhound-preview.service` (or `leadhound-fetch`, `leadhound-brief`, `leadhound-ats`).
The same preview runs by hand with `make remote HOST=… ARGS='preview' CONFIRM=1`, which takes the LinkedIn lock as the unit does.

**A watcher stops when its service hits its start limit.**
`man systemd.path` says the start-limit error of the service is propagated to the path unit, which fails too and no longer watches its path until it is restarted.
Only the brief has a start limit (`StartLimitBurst=3` in 4 hours, its two retries included), and a start from the console counts toward it, so a press after two failed tries can trip it; the same holds for a watcher's own `TriggerLimitBurst=` (200 triggers in 2 seconds by default).
The console then shows the action as stuck: asked and not started.
Reset both units and arm the watcher again, here for the brief:

```sh
systemctl reset-failed leadhound-brief.path leadhound-brief.service
systemctl start leadhound-brief.path
```

## Layout on the server

| Path | Owner, mode | What |
| --- | --- | --- |
| `/opt/leadhound/bin/leadhound` | root, 0755 | the binary |
| `/opt/leadhound/bin/leadhound-backup` | root, 0755 | `deploy/leadhound-backup.sh` |
| `/opt/leadhound/data/` | leadhound, 0750 | `LEADHOUND_DATA_DIR`: `leadhound.yaml`, `ats-boards.yaml`, the outbox of a brief that is not sent, `preview.json` (the latest Run now results) |
| `/opt/leadhound/data/run/` | leadhound, 0700 | the console's Run now requests, one file per action, read by the `leadhound-*.path` watchers |
| `/etc/leadhound/env` | root, 0600, enforced | the units' environment (`deploy/env.example`) |
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

- `make deploy HOST=… [ARCH=…] CONFIRM=1` ships a new release: it runs `make check`, builds a static Linux binary, copies it with `deploy/install.sh`, the backup script and the units, runs `migrate` with the new binary as the owner role, then puts the new binary and units in place, reloads systemd and enables the timers and the Run now watchers.
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
- `make deploy-data HOST=… CONFIRM=1` copies `leadhound.yaml` and `ats-boards.yaml` from `LEADHOUND_PRIVATE_DIR` (default `../job-hunt/leadhound`) after checking the profile loads; run it after you edit either file.
  It copies nothing else: the title screen is part of `leadhound.yaml`, and the eval golden set, the privacy denylist and the board seed lists are never needed on the server.
- `make remote HOST=… ARGS='track list'` runs one command on the server as the `leadhound` user with the units' environment.
  `track list`, `rules list` and `boards list` run freely; every other command writes or reaches the network (`collect`, `canary`, `brief`, `score`, `migrate`, `recheck`, `probe`, `track <n> …`, `rules add`, `boards import` …) and needs `CONFIRM=1`.
  `ARGS` is split into words by your own shell, as on a command line, and each word reaches `leadhound` whole: the server's shell never reinterprets it.
- `remote` runs without the units' hardening, memory ceiling and timeouts.
  It does take the units' locks, without waiting: `collect linkedin`, `canary`, `brief` and `preview` take the LinkedIn lock and `score` the score lock, and when a timer's run holds one, `remote` exits 75 and runs nothing; try again when it has finished (`systemctl list-units 'leadhound-*'`).
  Even so, prefer to run collects and briefs by hand only while no timer is due.
- `deploy`, `deploy-data` and `remote` run their `sudo` steps over `ssh -t`, so `sudo` can ask for its password at your terminal.
  Run without a terminal (from a script), ssh warns that it allocates none, and the steps then need `sudo` without a password for the ssh user.
- `deploy` and `deploy-data` refuse to run, even as `make -n`, without `HOST` and `CONFIRM=1`; `remote` refuses without `HOST`, and without `CONFIRM=1` for anything but the read-only lists.
- The brief's LinkedIn recheck runs before the day's `brief_log` row is written, so a brief that crashes in between rechecks again on its retry; the three-try limit bounds that.

## Leads on the server

Five timers run the Drupal leads; all times are Europe/Bucharest, each with up to 10 minutes of random delay.
Every leads unit has the same sandbox as the other timed units, `MemoryMax=600M` with `GOMEMLIMIT=450MiB`, `Nice=10`, idle I/O and `CPUWeight=50`, and each command runs under a `timeout` that `TimeoutStartSec` covers.
Business hours, spacing, daily caps, the warm-up ramp and the pause are the commands' own (`leads.yaml` and the console), never the units'.

| Timer | When | Runs | Switched on by |
| --- | --- | --- | --- |
| `leadhound-leads-probe.timer` | 13:00 | `leads sites probe --max 10000`, under `timeout 3h` | `LEADHOUND_LEADS_TIMERS=on` |
| `leadhound-leads-contacts.timer` | 10:00 | `leads contacts --max 300`, under `timeout 2h` | `LEADHOUND_LEADS_TIMERS=on` |
| `leadhound-leads-batch.timer` | 06:15 | `leads build`, then `leads batch new`, each under `timeout 20m` | sending on |
| `leadhound-leads-send.timer` | hourly 10:00 to 19:00, Monday to Friday, not persistent | `leads send --max 25`, under `timeout 225m` | sending on |
| `leadhound-leads-sync.timer` | every 15 minutes, not persistent | `leads sync`, under `timeout 10m` | sending on |

- The probe and the contact search are the finding timers.
  They start unattended reads of public sites: up to 10,000 homepages a day, and up to 300 organisations searched for a published contact a day, each site's robots file read and honoured first.
  They share a fetch lock in the data directory, `/opt/leadhound/data/.leads-fetch.lock`, without waiting: a run that finds it held exits 75, which the unit counts as done.
- The batch drafts the next batch for you to read and approve in the console; while a batch waits, `leads batch new` makes none.
  The send sends only approved messages, each in its recipient's business hours (the send hours cover the working day in the UK, Ireland and central Europe), and the sync reads the mailbox for replies, opt-outs and bounces.
  Send and sync take no file lock: their database locks keep them apart.
  The send and sync timers are not persistent, so a run missed while the server was down is never caught up outside its hours.
- No leads unit reads `/etc/leadhound/migrate.env`; they hold only the app role.

Every deploy installs the units and decides the timers from `/etc/leadhound/env`, the last line for each name counting, as for the units:

- With `LEADHOUND_LEADS_TIMERS=on`, the probe and contacts timers are enabled; otherwise they are disabled and stopped.
- With that, `LEADHOUND_LEADS_SEND=on`, `LEADHOUND_OUTREACH=live`, and a `LEADHOUND_OUTREACH_USER` and a `LEADHOUND_IMAP_ADDR` that are not empty, the batch, send and sync timers are enabled; otherwise they are disabled and stopped, so the server never drafts a shadow batch and never syncs without a mailbox.
- The deploy prints which leads timers are on, and for those that are off, which lines are missing.

To turn finding on, add `LEADHOUND_LEADS_TIMERS=on` to `/etc/leadhound/env` and run `systemctl enable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer` (or deploy).
To turn sending on, write the mailbox lines (`LEADHOUND_SMTP_ADDR`, `LEADHOUND_IMAP_ADDR`, `LEADHOUND_OUTREACH_USER`, `LEADHOUND_OUTREACH_PASSWORD`, typed into the file on the server) and `LEADHOUND_OUTREACH=live`, then `LEADHOUND_LEADS_SEND=on`, run `systemctl enable --now leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer`, and restart the console so it reads the new settings: `systemctl restart leadhound-console.service`.
A unit reads the env file only when it starts.

To pause sending, use the pause switch on the console's Leads page: the send timer keeps running and sends nothing until you resume, and nothing on the server changes.
To stop everything, remove the two switch lines (or set them to anything but `on`) and run `systemctl disable --now leadhound-leads-probe.timer leadhound-leads-contacts.timer leadhound-leads-batch.timer leadhound-leads-send.timer leadhound-leads-sync.timer`; the next deploy leaves them off.
A run already in progress finishes; `systemctl stop leadhound-leads-<name>.service` ends it at once.

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

To reset the password, run `make console-password` again, replace the `LEADHOUND_CONSOLE_PASSWORD_HASH` line on the server with the new hash, and restart the console: `systemctl restart leadhound-console.service`.
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
