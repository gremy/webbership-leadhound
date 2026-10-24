# webbership-leadhound

- **What**: a single Go binary, `leadhound`, that collects job postings from public sources, filters and scores them, and sends one daily brief by email.
  It runs as one-shot subcommands under systemd timers, with Postgres as its store; the application's database role cannot delete or drop.
- **How to run**: copy `.env.example` to `.env` and fill it in, then use the make targets.
  - `make check`: format check, vet, lint (when installed), unit tests and the tracked-file privacy check.
  - `make test-integration`: database tests against the local `leadhound_test`.
  - `make db-local`: create the roles and the local dev and test databases.
  - `make migrate`: apply migrations to the dev database as the owner role.
  - `make hooks`: install the pre-commit and commit-msg privacy hooks.
  - `make prepush`: the full gate before a push.
  - `make build-linux`: static Linux binary (`ARCH=arm64`, the default, or `amd64`).
  - `make deploy`, `make deploy-data`, `make remote`: run it on a server under systemd timers; see `deploy/README.md`.

  Subcommands (planned for slice 1): `collect`, `score`, `routes`, `brief`, `track`, `rules`, `canary`, `recheck`, `eval`, `boards` (`verify`, `add`), `probe company` and `migrate`, run as `go run ./cmd/leadhound <subcommand>`.
  Without `LEADHOUND_LIVE`, no command fetches from a job source.
- **Console**: `leadhound serve` is a small web console for one operator, on loopback behind a reverse proxy: the day's health, the latest brief's jobs to mark applied, skip or maybe (the same writes as `track`), the tracked jobs, the runs and the rules.
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
  `batch show` prints the approve line with the batch's hash, and approval is refused if the messages changed since they were drafted.
  Contact-form and LinkedIn items are done by hand and closed with `leads done MESSAGE`; `leads mark-contacted DOMAIN [--note TEXT]` records a lead reached some other way.
- **Shadow and live**: without `LEADHOUND_OUTREACH=live`, `leads send` writes each complete message to `<data dir>/outreach-outbox/` and nothing leaves the machine.
  With `LEADHOUND_OUTREACH=live` and a mailbox (`LEADHOUND_SMTP_ADDR`, `LEADHOUND_IMAP_ADDR`, `LEADHOUND_OUTREACH_USER`, `LEADHOUND_OUTREACH_PASSWORD`), a message is sent only from an approved batch, after the sending domain's DNS check passed today (`leads senders check`) and a reply sync has just succeeded.
  `leads sync` reads replies, bounces and opt-outs from the mailbox and suppresses where they say so.
  `leads selftest --to ADDRESS` sends one real message to one of `sender.operator_addresses`.
- **Suppression**: `leads suppress add address|domain VALUE [--reason manual|opt_out|bounce|replied_no]`, `leads suppress list` and `leads suppress check VALUE`; a suppressed address or domain is never drafted or sent to.
- **Migrations**: the leads tables have their own migration set, versioned in `goose_db_version_leads`; `leadhound migrate` applies the core set, then the leads set.
