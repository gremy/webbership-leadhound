# Deploying leadhound to a server

leadhound runs on one Linux server with systemd, from timers.
Nothing in this directory names a server: the host is an ssh destination in your own ssh config, passed as `HOST=` on the command line, and every secret lives in files on the server.

## What runs when

All times are Europe/Bucharest.
Each timer adds a random delay of up to 10 minutes and is persistent, so a run missed while the server was down happens at the next boot.

| Timer | When | Runs |
| --- | --- | --- |
| `leadhound-ats.timer` | 01:00 | `collect ats`, then `score` |
| `leadhound-backup.timer` | 03:30 | `pg_dump -Fc` into `/var/backups/leadhound/`, newest 14 kept |
| `leadhound-collect.timer` | 06:30 and 14:30 | `collect linkedin`, then `score` |
| `leadhound-prebrief.timer` | 07:30 | `canary linkedin` (once a day; a second start the same day does nothing) |
| `leadhound-brief.timer` | 07:50 | `brief --send`, retried twice, 10 minutes apart, if the send fails |

The console, `leadhound-console.service`, has no timer: it runs all the time once it is configured, and restarts five seconds after a crash, but never after bad config (exit 2).
See [Console](#console).

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
`collect ats` runs under `timeout 5h` and `collect linkedin` under `timeout 2h`, so a hang never stops the `score` after it; each unit's `TimeoutStartSec` is longer than its lock waits and timeouts together.

The server is shared with other services, so every unit but the backup has `MemoryHigh=900M` and `MemoryMax=1200M`: past the ceiling the kernel stops leadhound, never a neighbour.
The two collect units also run at `CPUWeight=50`.

## Layout on the server

| Path | Owner, mode | What |
| --- | --- | --- |
| `/opt/leadhound/bin/leadhound` | root, 0755 | the binary |
| `/opt/leadhound/bin/leadhound-backup` | root, 0755 | `deploy/leadhound-backup.sh` |
| `/opt/leadhound/data/` | leadhound, 0750 | `LEADHOUND_DATA_DIR`: `leadhound.yaml`, `ats-boards.yaml`, the outbox of a brief that is not sent |
| `/etc/leadhound/env` | root, 0600, enforced | the units' environment (`deploy/env.example`) |
| `/etc/leadhound/migrate.env` | root, 0600, enforced | the owner role's URL, for `migrate` only (`deploy/migrate.env.example`) |
| `/etc/systemd/system/leadhound-*` | root, 0644 | the units and timers, the console's included |
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

- `make deploy HOST=… [ARCH=…] CONFIRM=1` ships a new release: it runs `make check`, builds a static Linux binary, copies it with `deploy/install.sh`, the backup script and the units, runs `migrate` with the new binary as the owner role, then puts the new binary and units in place, reloads systemd and enables the timers.
  A failed `migrate` stops the deploy with a non-zero exit and leaves the old binary and units running; the release stays in its temporary directory on the server.
  Once the console is configured, the deploy also restarts it on the new binary and waits up to 10 seconds for it to answer; if it does not, the deploy prints its last log lines and exits non-zero, with the timers already in place.
- Every deploy re-applies `deploy/sql/limits.sql` as the Postgres superuser before it migrates; if that fails, the deploy stops with the old release running.
  Postgres runs a query's work in its own service, so the units' CPU and memory settings cannot bound it; these per-role limits are what does:
  - `leadhound_app` statements stop after 3 minutes, so no single query can hold the shared database for long; a command that needs longer sets its own bound for one session.
  - `leadhound_app` transactions left idle are ended after 10 minutes, which is above the longest an AI call holds the usage transaction open (about a minute), so a billed call's usage row is never rolled back.
  - Both roles run without parallel workers, so one leadhound query uses one CPU.
  - Both roles run without JIT, which only adds compile time to the queries leadhound runs.
  - `leadhound_app` sessions may write at most 512 MB of temp files, so a runaway sort or hash cannot fill the disk.
  - `leadhound_app` may hold at most 24 connections: four per pool for the console, the three collecting and briefing units and two leads units running at once.
  - `leadhound_owner` has no statement timeout, so a migration's index build may take as long as it needs.
  Locally `make db-local` applies the same file, then lifts the connection limit so parallel test runs can share the role.
- `make deploy-data HOST=… CONFIRM=1` copies `leadhound.yaml` and `ats-boards.yaml` from `LEADHOUND_PRIVATE_DIR` (default `../job-hunt/leadhound`) after checking the profile loads; run it after you edit either file.
  It copies nothing else: the title screen is part of `leadhound.yaml`, and the eval golden set, the privacy denylist and the board seed lists are never needed on the server.
- `make remote HOST=… ARGS='track list'` runs one command on the server as the `leadhound` user with the units' environment.
  `track list`, `rules list` and `boards list` run freely; every other command writes or reaches the network (`collect`, `canary`, `brief`, `score`, `migrate`, `recheck`, `probe`, `track <n> …`, `rules add`, `boards import` …) and needs `CONFIRM=1`.
  `ARGS` is split into words by your own shell, as on a command line, and each word reaches `leadhound` whole: the server's shell never reinterprets it.
- `remote` runs without the units' hardening, memory ceiling and timeouts.
  It does take the units' locks, without waiting: `collect linkedin`, `canary` and `brief` take the LinkedIn lock and `score` the score lock, and when a timer's run holds one, `remote` exits 75 and runs nothing; try again when it has finished (`systemctl list-units 'leadhound-*'`).
  Even so, prefer to run collects and briefs by hand only while no timer is due.
- `deploy`, `deploy-data` and `remote` run their `sudo` steps over `ssh -t`, so `sudo` can ask for its password at your terminal.
  Run without a terminal (from a script), ssh warns that it allocates none, and the steps then need `sudo` without a password for the ssh user.
- `deploy` and `deploy-data` refuse to run, even as `make -n`, without `HOST` and `CONFIRM=1`; `remote` refuses without `HOST`, and without `CONFIRM=1` for anything but the read-only lists.
- The brief's LinkedIn recheck runs before the day's `brief_log` row is written, so a brief that crashes in between rechecks again on its retry; the three-try limit bounds that.

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

## Restore

Dumps are in `pg_dump`'s custom format:

```sh
sudo -u postgres pg_restore --clean --if-exists -d leadhound /var/backups/leadhound/leadhound-<stamp>.dump
```
