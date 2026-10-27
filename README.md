# webbership-leadhound

> **Built for one person first.** LeadHound is public so others can read it, learn from it and run it, but it is built around its author's own job hunt and outreach (Drupal and Go work, Romania, the UK and the EU), and his immediate needs come first.
> Your own profile, templates and settings live in an untracked `data/` folder, starting from `examples/`.
> Expect to adapt it, and expect changes without notice.

- **What**: a single Go binary, `leadhound`, that collects job postings from public sources, filters and scores them, and sends one daily brief by email.
  It runs as one-shot subcommands under systemd timers, with Postgres as its store; the application's database role cannot delete or drop.
- **How to run**: copy `.env.example` to `.env` and fill it in, then use the make targets.
  - `make check`: format check, vet, lint (when installed), unit tests and the tracked-file privacy check.
  - `make test-integration`: database tests against the local `leadhound_test`.
  - `make test-scale`: time and memory budgets on `leadhound_scale`, a local database the size of production: 450,000 jobs (`LEADHOUND_SCALE_JOBS`) and a year of lead finding and outreach (a million sites, 700,000 probed, 5,000 leads, 400 batches).
    It is seeded once, about two minutes for the jobs, and reused; `make deploy` runs it and refuses to ship when a budget fails.
  - `make gate`: everything a deploy must pass, in order: `check`, `test-integration`, `test-scale`, `go vet` with the integration and scale tags, and the deploy tests.
    When it passes on a clean tree (untracked files count), it writes the commit to `bin/gate-passed`, and `make deploy` on that same commit, still clean, skips the gate.
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
  - Leads, in three tabs: the List (`/leads`) with a page per lead, Outreach (`/leads/outreach`) and Settings (`/leads/settings`); see "The Leads console" under Drupal leads.
    Outreach is the summary and the sending state with the pause switch; each batch waiting for approval read in full, with its approve form shown only while the messages still match their hash, and discard; replies with suppress, suppressions, manual items to mark done, mark contacted, and a message to check by hand as sent or not sent.
    The old queue and priorities pages are retired: `/leads/next` goes to the list in your order (`/leads?sort=order`), and `/leads/priorities` goes to the fallback order in Settings.
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
- **The daily loop**: `leads sites probe` → `leads contacts` → `leads enrich` → `leads rate` → `leads build` → star and order leads on the console's List → `leads batch new [--size N]` → read `leads batch show N` → `leads batch approve N --hash H [--skip 3,5]` → `leads send [--max N]` → `leads sync`.
  `leads build` turns every organisation with a finding (an unsupported Drupal, a Drupal 10 site, an agency partner) into a lead and reads no web page.
  `leads batch new` drafts from the starred leads only, in your order, unless `batches_from` is `starred_then_rest`; with nothing starred it makes no batch, says so and exits 0.
  `batch show` prints the approve line with the batch's hash, and approval is refused if the messages changed since they were drafted; the console's Outreach tab shows and approves the same batches.
  A new batch is refused while one of its kind waits, and a live batch is sized to what the warm-up ramp can send over the next three business days; `leads batch discard N` returns its leads to new.
  The ramp starts on a fixed date or, with `caps.start_date: first-send`, on the first real send.
  Contact-form and LinkedIn items are done by hand and closed with `leads done MESSAGE`; `leads mark-contacted DOMAIN [--note TEXT]` records a lead reached some other way, and `leads resolve MESSAGE sent|not-sent` settles a message whose send is uncertain, after checking the mailbox's Sent folder.
- **The Leads console**: the List (`/leads`) shows every lead with its name, country, kind, the finding in plain words, its channels (email, form, phone), its relevance and a star.
  It filters by kind, country, channel, starred, has a phone, minimum relevance and status (not contacted yet, contacted, all), and sorts by relevance, newest or your outreach order; the filters survive paging.
  A lead's page (`/leads/{id}`) shows the finding with its evidence, the ways to reach them and the pages they came from, phone numbers, description and social links, the agency page, the site, its history, its place in the queue, its messages and one line per reason it ranks where it does.
  Nothing on a GET writes.
- **Stars and the order**: a star puts a lead at the bottom of your outreach order; Up, Down and Top move it, Unstar takes it out, Push back defers it (14 days by default), Skip drops it for good and Clear puts it back in its place by score.
  Up, Down and Top on a lead that is not starred are refused with "Star it first."
  Starred leads always come first, in your order; every star and move is kept as a row, and the latest per lead wins.
- **Settings in the console**: Settings (`/leads/settings`) changes the leads settings over `leads.yaml`: who to contact (the never-contact countries), lead kinds on or off and their order, small sites, where batches draw from, the Drupal end-of-life dates, Jev relevance (rating on or off and the leads' daily calls) and the fallback order.
  Each section saves and resets on its own, and a value changed there says "changed in the console" with the day; a change shapes new batches only, never what is already drafted or sent.
  Country channels and languages, the sending pace and ramp, the sender, the templates and the environment show read-only, because the send path reads them from the file.
  A save that does not check out is refused whole and changes nothing, and a leads daily-call value above the server's `LEADHOUND_JEV_LEADS_DAILY_CALL_CAP` is refused with "raise it in .env first".
  No secret is ever shown or stored, and every save is kept as a row.
  `leads settings show` prints each setting in force and where it comes from (`file`, or `console` and the day); `leads settings reset [--section who|kinds|small-sites|batches|dates|relevance|order|all]` puts one section, or every one (the default), back to `leads.yaml`, and refuses any other section name.
- **Enrichment**: `leads enrich [--max N] [--dry-run --fixtures DIR]` reads phone numbers, a description and social links from each organisation's own pages, for up to N organisations with a lead not enriched in the last 30 days (300 by default; `--max` below 1 is refused).
  A phone is a fact, never a contact route, so nothing drafts or sends to it; no fact ever holds an email address, and only a LinkedIn company page is kept.
  It needs `LEADHOUND_LIVE=1`, and without it fetches nothing and exits 0; the details are in "Enrichment" in `deploy/README.md`.
- **Relevance**: `leads rate [--max N] [--decider jev|fake]` asks Jev how likely each open lead is to want the work and stores a number from 0 to 100 with its band (strong, likely, maybe, unlikely, no).
  It asks only about leads whose facts changed since their last rating, starred leads first, and stops after `--max` calls (10,000 by default; below 1 is refused).
  Its calls count against their own daily cap, `LEADHOUND_JEV_LEADS_DAILY_CALL_CAP` (10,000 by default, and the Settings value may lower it, never raise it), so jobs scoring keeps its full cap whatever leads do.
  It refuses while rating is off in Settings, and `--decider jev` (the default) refuses without `TYPESAFE_API_KEY`; without `LEADHOUND_LIVE=1` it asks nothing and exits 0, and a second rating while one runs is skipped.
  A rating stopped by `--max`, the cap, an unavailable provider or five failed calls in a row is recorded and exits 0.
  `--decider fake` rates every lead "maybe" (50) without a key, for tests.
- **Your call**: on a lead's page, "Your call" records the band you would give it (strong, likely, maybe, unlikely or no), or clears it.
  The call is stored in the database with the lead's facts exactly as you saw them, every call is kept, and the latest per lead wins.
  To label (`/leads?to_label=1`) lists 30 leads spread over kinds, Drupal versions and countries, plus every lead you have already labelled.
- **Measuring relevance**: `leads relevance eval [--golden FILE] [--runs N] [--decider fake|jev]` asks about every lead with your call, N times (3 by default, 1 to 5), and prints how often Jev's band matches yours exactly and within one band, the mean error, Spearman's rho, the disagreements between runs, and the input tokens with their estimated cost.
  By default it reads your calls from the database; `--golden FILE` reads a golden file instead (one JSON lead per line, such as the demo `examples/leads-relevance.demo.jsonl`).
  With no call yet it refuses and says to open To label; a golden file with no labelled line is refused too.
  The decider is the fake unless you pass `--decider jev`, which needs `TYPESAFE_API_KEY` and `LEADHOUND_LIVE=1`, says how many calls it will make before the first, and is not capped.
  `leads relevance golden-draft [--n N] --out FILE` writes N open leads (30 by default) spread over kinds, Drupal versions and countries, each with an empty label, at mode 0600; it refuses a path inside a checkout of this repository, because labels are private, and never overwrites a file.
- **The trust gate**: the relevance number is not trusted until it has been measured against your own calls.
  Only an eval with `--decider jev` on at least 30 of your calls is recorded, and it passes when Jev is within one band of you on at least 80% of them and its order agrees with yours (Spearman's rho at least 0.5); the latest recorded eval decides.
  Until one passes, the list's Relevance sort reads "by score for now" and orders by the fallback order's points, and batches do the same; once it passes, relevance orders the leads that are not starred.
  Either way, starred leads come first in your order.
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
  The console's Settings page changes some of these values over the file, in the database; `leads settings show` says which, and `leads settings reset` puts them back.
  `leads transfer export DIR` and `leads transfer import DIR` move lead data from one database to another, re-keyed by natural keys so they never collide with the other side's ids; an import can run again safely, it is refused while a live message may be in flight, and both print counts only.
- **On a server**: five leads timers run the loop (probe daily; contacts daily, which then runs enrich, rate and build; the batch each morning; send hourly in business hours on weekdays; sync every 15 minutes); every deploy installs them, and each stays off until `/etc/leadhound/env` turns it on.
  The rate step also needs `LEADHOUND_LIVE=1` and `TYPESAFE_API_KEY` in that file; without them it rates nothing, and the build after it still runs.
  `LEADHOUND_LEADS_TIMERS=on` turns on finding (probe and contacts).
  Sending (batch, send and sync) needs all three lines, `LEADHOUND_LEADS_TIMERS=on`, `LEADHOUND_LEADS_SEND=on` and `LEADHOUND_OUTREACH=live` (without the first, the next deploy turns them off again), and sends only from a ready mailbox; even then every batch is read and approved by the operator before anything is sent.
  The deploy prints which leads timers are on and, for those off, which lines are missing; the steps are in "Leads on the server" in `deploy/README.md`.
- **Migrations**: the leads tables have their own migration set, versioned in `goose_db_version_leads`; `leadhound migrate` applies the core set (up to 00008), then the leads set (up to 00015).
  The stars, enrichment facts, relevance ratings and evals, console settings and your calls are each append-only tables: every change is a new row, and the app role cannot update or delete them.
