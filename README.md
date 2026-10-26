# webbership-leadhound

- **What**: a single Go binary, `leadhound`, that collects job postings from public sources, filters and scores them, and sends one daily brief by email.
  It runs as one-shot subcommands under systemd timers, with Postgres as its store; the application's database role cannot delete or drop.
- **How to run**: copy `.env.example` to `.env` and fill it in, then use the make targets.
  - `make check`: format check, vet, lint (when installed), unit tests and the tracked-file privacy check.
  - `make test-integration`: database tests against the local `leadhound_test`.
  - `make test-scale`: time and memory budgets on `leadhound_scale`, a local database the size of production: 450,000 jobs (`LEADHOUND_SCALE_JOBS`) and a year of lead finding and outreach (a million sites, 700,000 probed, 5,000 leads, 400 batches).
    It is seeded once, about two minutes for the jobs, and reused; `make deploy` runs it and refuses to ship when a budget fails.
  - `make db-local`: create the roles, their limits and the local dev, test and scale databases.
  - `make migrate`: apply migrations to the dev database as the owner role.
  - `make hooks`: install the pre-commit and commit-msg privacy hooks.
  - `make prepush`: the full gate before a push.
  - `make build-linux`: static Linux binary (`ARCH=arm64`, the default, or `amd64`).
  - `make deploy`, `make deploy-data`, `make remote`: run it on a server under systemd timers; see `deploy/README.md`.

  Subcommands: `collect`, `score`, `brief`, `track`, `rules`, `canary`, `recheck`, `eval`, `boards` (`import`, `list`, `check`, `harvest`), `probe company`, `trim`, `leads`, `serve`, `console hash-password` and `migrate`, run as `go run ./cmd/leadhound <subcommand>`.
  Without `LEADHOUND_LIVE`, no command fetches from a job source.
- **Scoring**: `score --max-duration D` stops cleanly before the next job once `D` has passed, with the stop reason `budget`, which the brief words as a paused day; the next run carries on with the backlog.
  One scorer runs at a time across processes, through a database lock: a second run says so and records nothing.
- **Storage**: every collector stops storing once the database passes `LEADHOUND_DB_MAX_BYTES` (default 8 GiB) or free disk falls under `LEADHOUND_DISK_MIN_FREE_PCT` (default 15).
  Its run ends with the stop reason `disk_budget`, and the brief and the console's dashboard say so in one line; scoring, the brief and the console carry on with what is stored.
  `leadhound trim` blanks the description text of postings rejected by a gate more than 7 days ago and expired more than 30 days ago, in batches of 5,000 within a 45-minute budget (`--rejected-after`, `--expired-after`, `--batch`, `--max-duration`); every row, verdict and id stays, and a job you acted on or that appeared in a brief is never trimmed.
  On a server it runs daily as `leadhound-trim.timer`; how to recover from an over-budget database is in "Backups and the storage budget" and "The trim" in `deploy/README.md`.
- **Console**: `leadhound serve` is a small web console for one operator, on loopback behind a reverse proxy, and every action in it runs the same code as the matching command.
  - The dashboard: the day's health, funnel, cost, boards and runs, built in the background and served from the last snapshot, so a page never waits for the ranking.
  - Jobs: the latest brief's jobs to mark applied, skip or maybe (the same writes as `track`), and a page per job (`/jobs/{id}`) with everything needed to apply and its own mark form; the tracked jobs, capped at 500 rows with the total shown; the runs.
  - Rules: the list, with forms to add a rule and to disable one.
  - Leads: the summary and the sending state with the pause switch; each batch waiting for approval read in full, with its approve form shown only while the messages still match their hash, and discard; replies with suppress, suppressions, manual items to mark done, mark contacted, and a message to check by hand as sent or not sent.
    The queue (`/leads/next`) lists leads in drafting order with pin, push back and skip, and the priority profile (`/leads/priorities`) sets that order, with a preview.
    The console never approves a batch on its own and never drafts a shadow batch.
  It signs in with an email and a bcrypt hash from the environment: `make console-password` writes the hash, and `make console-local` serves it locally in dev mode.
  A device that signed in before can still sign in during a flood of failed attempts, and a new password forgets every device.
  On a server it runs as `leadhound-console.service`; see the Console section of `deploy/README.md`.
- **Contributing**: see `CONTRIBUTING.md`; ported work is credited in `NOTICE`.
- **State**: see `STATE.md` (the head-pointer for where this left off).

## Drupal leads

`leadhound leads` finds websites still running an old Drupal major version, finds a public role address or contact form for each organisation, and drafts a short message per lead from fixed templates for the operator to read and approve.
Nothing is written by a model: every draft is a template filled with facts the probe recorded.

- **Data directory**: the leads commands read `leads.yaml` from `LEADHOUND_DATA_DIR`, with the templates under `leads-templates/` (the `templates_dir` key) and the agency list in `agencies.yaml`.
  `examples/leads.yaml`, `examples/leads-templates/` and `examples/agencies.demo.yaml` are a fictional demo with reserved names only.
- **Finding sites**: `leads sites import majestic|tranco|httparchive FILE` or `leads sites import manual DOMAIN...` adds domains, keeping only the suffixes in `site_lists.tlds`.
  `leads sites probe [--max N] [--domain D]...` reads each due site's homepage and records its Drupal major and, when shown, its exact version; `leads sites eval --labels FILE` compares a run with a hand-checked CSV.
  `leads agencies import [FILE]` reads the agency list, and `leads contacts [--max N] [--org ID]...` looks for contact routes on each organisation's own site; `leads export routes [--limit N]` writes them to a file.
- **Fetching rules**: site fetching needs `LEADHOUND_LIVE=1`, and `--dry-run --fixtures DIR` runs probe, contacts and agencies import on local files instead.
  Only hosts that resolve to public network addresses are fetched, robots.txt is honoured on every path, and a bot check is recorded and skipped, never worked around.
  Only role addresses (the `role_local_parts` list) are stored; an address that looks like a person's is counted and dropped.
  `LEADHOUND_PROBE_CONCURRENCY` and `LEADHOUND_PROBE_RATE` set how many sites run at once and the requests per second across a run.
- **The daily loop**: `leads sites probe` → `leads contacts` → `leads build` → `leads batch new [--size N]` → read `leads batch show N` → `leads batch approve N --hash H [--skip 3,5]` → `leads send [--max N]` → `leads sync`.
  `batch show` prints the approve line with the batch's hash, and approval is refused if the messages changed since they were drafted; the console's Leads page shows and approves the same batches.
  A new batch is refused while one of its kind waits, and a live batch is sized to what the warm-up ramp can send over the next three business days; `leads batch discard N` returns its leads to new.
  The ramp starts on a fixed date or, with `caps.start_date: first-send`, on the first real send.
  Contact-form and LinkedIn items are done by hand and closed with `leads done MESSAGE`; `leads mark-contacted DOMAIN [--note TEXT]` records a lead reached some other way, and `leads resolve MESSAGE sent|not-sent` settles a message whose send is uncertain, after checking the mailbox's Sent folder.
- **Shadow and live**: without `LEADHOUND_OUTREACH=live`, `leads send` writes each complete message to `<data dir>/outreach-outbox/` and nothing leaves the machine.
  With `LEADHOUND_OUTREACH=live`, a message is sent only from a batch the operator approved, from the mailbox it was drafted for, after that mailbox's records passed today's DNS check (`leads senders check`) and its reply sync has just succeeded.
  `leads send pause [--reason TEXT]` and `leads send resume`, or the pause switch in the console, stop and restart sending; the pause is checked before every message.
  `leads sync` reads replies, bounces and opt-outs from every mailbox and suppresses where they say so; it exits 1 when any mailbox could not be read.
  `leads selftest --to ADDRESS [--mailbox ID]` sends one real message from one mailbox to one of `sender.operator_addresses`.
- **Mailboxes**: the sending mailboxes live in the database, not in the environment; add any number on the console's Mailboxes page, or with `leads mailboxes add`, and each is checked (both sign-ins, the Sent and Junk folders, its domain's SPF, DKIM and DMARC) without sending anything.
  Each password is sealed with a public key as it is entered, and only the send, sync and check jobs hold the private key; `make mailbox-key` makes a local one, and the first deploy makes the server's.
  Each mailbox has its own warm-up ramp (at most 40 a day), and each sending domain a daily limit over all its mailboxes (`caps.domain_per_day`, default 50) and a cap on its mailboxes (`caps.mailboxes_per_domain`, default 2), because the large providers judge reputation by the sending domain: see [Google's sender guidelines](https://support.google.com/mail/answer/81126), [their FAQ](https://support.google.com/a/answer/14229414) and [Microsoft's 550 5.7.515](https://support.microsoft.com/en-us/outlook/fix-ndr-error-550-5-7-515-in-outlook-com).
  A mailbox pauses itself on repeated hard bounces or a refused login, and a policy block pauses its whole domain; only the operator resumes.
  The old mailbox lines in the env file are no longer read.
  Adding a first mailbox, the key and its backup, the limits, the pauses and the first test are in "Mailboxes" in `deploy/README.md`.
- **Suppression**: `leads suppress add address|domain VALUE [--reason manual|opt_out|bounce|replied_no]`, `leads suppress list` and `leads suppress check VALUE`; a suppressed address or domain is never drafted or sent to.
  An opt-out is read from the subject and the sender's own words only, never from quoted text or our own lines, and a draft whose text holds an opt-out word outside the opt-out line is refused.
  A suppression recorded by mistake is lifted by the operator only, by number, with a reason (`leads suppress lift ID --domain DOMAIN --reason TEXT`, or Lift a suppression on the console's Leads page); the lift is kept as a record, lead states do not change, and a new opt-out suppresses again.
  `leads suppress review` reads past opt-outs again and lists those that were only quoted text, with the lift line for each; it writes nothing.
  The details are in "Opt-outs, suppressions and lifting one" in `deploy/README.md`.
- **Settings and data**: `leads check-config [--agencies FILE]` checks `leads.yaml`, the templates and the agency list without printing any value; `make deploy-data` runs it, then copies `leads.yaml`, `agencies.yaml` and `leads-templates/` to the server with the jobs settings (`leadhound.yaml`, `ats-boards.yaml`); `examples/leads.yaml` is the starting point for your own.
  `leads transfer export DIR` and `leads transfer import DIR` move lead data from one database to another, re-keyed by natural keys so they never collide with the other side's ids; an import can run again safely, it is refused while a live message may be in flight, and both print counts only.
- **On a server**: five leads timers run the loop (probe and contacts daily, the batch each morning, send hourly in business hours on weekdays, sync every 15 minutes); every deploy installs them, and each stays off until `/etc/leadhound/env` turns it on.
  `LEADHOUND_LEADS_TIMERS=on` turns on finding (probe and contacts).
  Sending (batch, send and sync) needs all three lines, `LEADHOUND_LEADS_TIMERS=on`, `LEADHOUND_LEADS_SEND=on` and `LEADHOUND_OUTREACH=live` (without the first, the next deploy turns them off again), and sends only from a ready mailbox; even then every batch is read and approved by the operator before anything is sent.
  The deploy prints which leads timers are on and, for those off, which lines are missing; the steps are in "Leads on the server" in `deploy/README.md`.
- **Migrations**: the leads tables have their own migration set, versioned in `goose_db_version_leads`; `leadhound migrate` applies the core set, then the leads set.
