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
  - Controls (`/controls`): one sentence on why nothing is going out (or when the next email does), the Sending switch, a switch per leads job (morning batch, inbox sync, site probe, contacts, research, and the email check, with today's checks and the Reoon credits left) with its timer read from systemd and its last run, and the three server-only lines (`LEADHOUND_OUTREACH`, `LEADHOUND_LIVE`, `LEADHOUND_LEADS_TIMERS`) shown read-only; see "Switches in the console" in `deploy/README.md`.
  - Leads, in six tabs: Today (`/leads/today`), the List (`/leads`) with a page per lead, Outreach (`/leads/outreach`), Drafts (`/leads/drafts`), Campaigns (`/leads/campaigns`) and Settings (`/leads/settings`); see "The Leads console" and "Calls" under Campaigns and leads.
    Outreach is the summary and the sending state with the pause switch, under the Controls sentence on why nothing is going out (also on the dashboard's Leads card); each batch waiting for approval read in full, with its approve form shown only while the messages still match their hash, and discard; replies with suppress, suppressions, manual items to mark done, mark contacted, and a message to check by hand as sent or not sent.
    The old queue and priorities pages are retired: `/leads/next` goes to the list in your order (`/leads?sort=order`), and `/leads/priorities` goes to the fallback order in Settings.
    The console never approves a batch on its own and never drafts a shadow batch.
  It signs in with an email and a bcrypt hash from the environment: `make console-password` writes the hash, and `make console-local` serves it locally in dev mode.
  A device that signed in before can still sign in during a flood of failed attempts, and a new password forgets every device.
  On a server it runs as `leadhound-console.service`; see the Console section of `deploy/README.md`.
- **Contributing**: see `CONTRIBUTING.md`; ported work is credited in `NOTICE`.
- **State**: see `STATE.md` (the head-pointer for where this left off).

## Campaigns and leads

`leadhound leads` finds organisations worth contacting, finds a way to reach each one, and drafts a short sequence of emails and calls per lead for the operator to read and approve.
Each kind of outreach is a campaign: websites still running an old or ending Drupal major version (`drupal-eol`) is the first, independent cafés (`cafes-loyalty`, see "Café places" below) the second.
Nothing leaves the machine that the operator did not approve.
A draft is either a template filled with stored facts or, once a campaign has earned it, written by Claude; Claude never decides a fact, and a deterministic check refuses any claim that is not a stored row.

- **Data directory**: the leads commands read `leads.yaml` from `LEADHOUND_DATA_DIR`, with the templates under `leads-templates/` (the `templates_dir` key), the agency list in `agencies.yaml` and the campaigns under `campaigns/`.
  `examples/leads.yaml`, `examples/leads-templates/`, `examples/agencies.demo.yaml` and `examples/campaigns/` are a fictional demo with reserved names only.
- **Campaigns**: a campaign is a spec file, `<data dir>/campaigns/<slug>/campaign.yaml`, beside its templates: its source, detectors, lead kinds, countries (email and call on or off, and the language), the offer, the copy rules, the writer (`template` or `claude`) and its sequence, with an optional `call_first` sequence.
  Without a `campaigns/` folder, `drupal-eol` is read from `leads.yaml` as before: three emails from the templates, no calls.
  `leads campaigns check` reads every spec and its templates without the database and names every problem; `leads campaigns register` records each spec as a new version only when it changed; `leads campaigns list` and `leads campaigns show SLUG` read what is registered.
  `leads batch new` refuses a campaign whose file has no registered version or changed since it was registered, so run `register` after every change to a spec.
  A batch stores the sequence it was drafted with, so a later change to the spec never changes a batch already approved.
  The campaign's own country list decides which leads it drafts for; a country it does not list gets no email and no call.
  Suppressions, mailboxes and Claude spend belong to the account, not the campaign, so an opt-out or a do-not-call in one campaign blocks every campaign; each campaign sends only from the mailboxes assigned to it.
  `leads build`, `leads rate` and `leads batch new` take `--campaign SLUG` (default `drupal-eol`, which the timers use).
  The Campaigns tab shows each campaign with its countries and steps, whether its Claude drafts are trusted yet, and the account's Claude spend today, and is where its mailboxes are chosen.
- **Finding sites**: `leads sites import majestic|tranco|httparchive FILE` or `leads sites import manual DOMAIN...` adds domains, keeping only the suffixes in `site_lists.tlds`.
  `leads sites probe [--max N] [--domain D]...` reads each due site's homepage and records its Drupal major and, when shown, its exact version; `leads sites eval --labels FILE` compares a run with a hand-checked CSV.
  `leads agencies import [FILE]` reads the agency list, and `leads contacts [--max N] [--org ID]...` looks for contact routes on each organisation's own site; `leads export routes [--limit N]` writes them to a file.
- **Fetching rules**: site fetching needs `LEADHOUND_LIVE=1`, and `--dry-run --fixtures DIR` runs probe, contacts and agencies import on local files instead.
  Only hosts that resolve to public network addresses are fetched, robots.txt is honoured on every path, and a bot check is recorded and skipped, never worked around.
  Only role addresses (the `role_local_parts` list) are stored; an address that looks like a person's is counted and dropped.
  `LEADHOUND_PROBE_CONCURRENCY` and `LEADHOUND_PROBE_RATE` set how many sites run at once and the requests per second across a run.
- **The daily loop**: `leads sites probe` → `leads contacts` → `leads enrich` → `leads rate` → `leads build` → star and order leads on the console's List → `leads verify [--max N]` → `leads batch new [--size N]` → read `leads batch show N` → `leads batch approve N --hash H [--skip 3,5]` → `leads send [--max N]` → `leads sync`.
  `leads build` turns every organisation with a finding (an unsupported Drupal, a Drupal 10 site, an agency partner) into a lead and reads no web page.
  `leads batch new` drafts from the starred leads only, in your order, unless `batches_from` is `starred_then_rest`; with nothing starred it makes no batch, says so and exits 0.
  `batch show` prints the approve line with the batch's hash, and approval is refused if the messages changed since they were drafted; the console's Outreach tab shows and approves the same batches.
  A new batch is refused while one of its kind waits, and a live batch is sized to what the warm-up ramp can send over the next three business days; `leads batch discard N` returns its leads to new.
  The ramp starts on a fixed date or, with `caps.start_date: first-send`, on the first real send.
  Contact-form and LinkedIn items are done by hand and closed with `leads done MESSAGE`; `leads mark-contacted DOMAIN [--note TEXT]` records a lead reached some other way, and `leads resolve MESSAGE sent|not-sent` settles a message whose send is uncertain, after checking the mailbox's Sent folder.
  Calls are made by hand from the console's Today tab and recorded there or with `leads calls outcome`; `leads send` never touches a call.
- **Email check**: `leads verify [--max N]` checks with Reoon the email addresses the next live batches would draft (starred first, in the batch's order), two at a time, within the credits left and `LEADHOUND_REOON_DAILY_CAP` (default 200 a day), and stores each answer; it needs `LEADHOUND_LIVE=1` and the key file (`/etc/leadhound/reoon.key` on the server, `LEADHOUND_REOON_KEY_FILE` locally).
  While the Email check job is on (Controls), a live batch drafts an email lead only when its address has a sendable answer from the last 90 days: safe and role addresses go, invalid, disabled, disposable, full and trap addresses never do, catch-all goes unless Leads → Settings → Email check leaves them out, and unknown is asked again the next day and blocked after a second unknown.
  A blocked address falls to the lead's next route (another address on its domain, or its contact form) or skips the lead with the answer as its reason; an address not checked yet waits for a later batch.
  Switched off, batches draft as before.
  The morning batch unit runs it between `leads build` and `leads batch new`; see "The Reoon key" and "The email check" in `deploy/README.md`.
- **The sequence**: every step of a lead's sequence is drafted when the batch is made and approved with it, calls included.
  The example `drupal-eol` sequence has five steps: an email, a follow-up email 3 business days later, a call 2 business days after that when the lead has a callable phone, an email saying you tried to call 1 business day later when the call did not connect, and a last email 5 business days later.
  A lead with no callable phone at batch time gets no call and no "tried to call" email.
  Each step falls due a number of business days after the last step sent or completed, and the call's outcome decides what follows:

  | Outcome | "Tried to call" email | Last email | Also |
  | --- | --- | --- | --- |
  | Got through (`connected`) | cancelled | cancelled | the sequence ends |
  | No answer (`no_answer`), voicemail (`voicemail`) | due 1 business day later | after it | |
  | Wrong number (`wrong_number`) | cancelled | due after its days | the number is marked wrong and never drafted or called again |
  | Do not call (`do_not_call`) | cancelled | cancelled | the number is suppressed for every campaign; the sequence ends |
  | Skipped (`skipped`) | cancelled | due after its days | |

  A reply, opt-out, bounce or suppression at any step cancels every pending step of that lead, calls included.
  Batches made before the sequences existed finish on the old three-email rule.
- **Email first, everywhere**: since 29 Oct 2026 every country in the table runs the same sequence (email, follow-up, call, an email if the call did not connect, last email); the operator opened the countries the research marked consent-only or off (Romania, Italy, Spain, Germany, Denmark, the Netherlands, Latvia, Austria, Poland, the Czech Republic, Bulgaria), on the 27 Oct terms: companies only, role addresses, an opt-out in every email, suppressed on the first objection.
- **Call first (dormant)**: a country whose table entry says email needs consent would put a lead of a campaign with a call step on the `call_first` sequence instead: a call, a second call 2 business days later if the first did not connect, then two emails only after they said yes on the call. No country in the table does today.
  A campaign that defines no `call_first` sequence gets that one, and its templates are read from `<kind>/<lang>/call_first/`; a lead with no number in such a country is dropped from the batch with the reason, never sent a cold email.
  "Got through" on the call card has a tick box for "They agreed to get an email" and the address they gave; ticked, the emails fall due, and unticked they are cancelled.
- **Calls**: the Today tab (`/leads/today`) lists calls to make now, overdue calls, replies waiting, emails due today and manual items, per campaign.
  A call card (`/leads/calls/{message}`) shows the organisation, the number as a link that dials it with the country code, where the number came from, its line kind when known, three talking points, the screening status and the six outcome buttons.
  `leads calls list [--campaign SLUG] [--due]` lists the call steps, and `leads calls outcome MESSAGE connected|no_answer|voicemail|wrong_number|do_not_call|skipped [--note TEXT] [--consent] [--consent-address A]` records one.
  An outcome is refused on a call that is not due yet, on one whose lead has replied, and a second time once the sequence has moved on.
- **When a call is callable**: one rule decides it everywhere (Today, the card, `calls list`, the lead page and the batch), in this order: there is a number to dial, it is not suppressed, it was not marked wrong, the country table allows the call for this organisation, the number's screening is clean and recent enough, and it is inside the calling hours in the callee's time.
  The country table (`internal/compliance/countries.yaml`, researched on 27 Oct 2026, not legal advice) gives each country's email and call rules, its calling hours (09:30 to 17:00 on weekdays where the law names none), its registers and its confidence; a country it marks off (none today) or does not list is never callable, and no campaign can open it.
  A campaign whose own countries have `call: off` still gets the call drafted, with a warning on the card, Today, the lead page, `batch show` and `calls list` that it is your call whether to ring.
  A US, Canadian or Australian lead is callable only when its state or region is known, which today only a café place gives.
  Once any campaign has a call step, set `sender.privacy_url` in `leads.yaml` to your privacy notice: every email's footer then ends "How I handle your details: <url>", and `leads campaigns check` warns while it is missing.
- **Screening**: a number in a country with a do-not-call register is callable only with a clean result from that register within the register's age (TPS and CTPS for Great Britain: 28 days; a register with no age, such as Italy's or Spain's, after the latest batch approval).
  `leads phones screen export --register R --out FILE` writes the numbers that need screening, a header line and then one E.164 number per line (mode 0600, outside the repository, never over an existing file); you screen the file with the register's own service, and `leads phones screen import --register R FILE` records the results.
  The registers are `tps`, `ctps`, `dncm`, `ndd`, `dncr`, `rpo`, `robinson` and `gr_art11`; a listed number is suppressed, and only the operator lifts a suppression.
  `leads suppress add phone +NUMBER --reason do_not_call|tps|ctps` suppresses one by hand; a phone suppression stores a hash of the number, never the number.
- **Contact research**: starring a new lead that has no usable way to reach it queues a search for one, and the star control says so first ("No email yet: starring it looks for one", or "Calls-first country: starring it looks for a business phone").
  `leads research run [--max N]` asks Claude (Haiku) to find a published, impersonal role address, or a business phone where the country is written to by form only, then fetches each page Claude cites itself and keeps only what it finds printed there on the organisation's own domain; the next `leads build` gives the lead its channel.
  Nothing is looked for where the campaign does not list the country, the country is off in `leads.yaml`, or, for a phone, the country table does not let the campaign call.
  One attempt per organisation every 90 days; "Look again" on the lead page and `leads research request LEAD` ask for another, and `leads research list [--status S] [--limit N]` lists them.
  Batch refusals list each starred lead and why it is out (no email yet, looking for a contact, found nothing, country rule, suppressed, deferred, ranked out).
- **Phone research**: `leads phones research --campaign SLUG [--max N]` (10 by default) looks for a business phone for the campaign's leads with a call step ahead and no phone, or only a switchboard, starred first, under the same verification: a number counts only when it is printed on the cited page.
  It keeps the line kind and a job title, never a person's name, and never stores Claude's quote of the page.
- **Claude drafts**: a campaign with `writer: claude` has Claude (Sonnet) write each lead's emails and the call card's talking points from a fact sheet of stored rows: the organisation, its finding and evidence, the end-of-life dates, the sender's own facts and the offer.
  Every paragraph cites the facts it rests on, and the grounding check refuses a draft with any number, date, version, name or link that is not in a cited fact, an opener that starts with research about them, a pain the lead's signals do not allow, a boast, an echo of their own site's text, or "migration" on a Drupal 10 lead.
  A refused draft is retried once with the failures; a lead refused again, or reached after a Claude cap or while Claude is unavailable, is drafted from the campaign's templates, and the batch says how many and why.
  The identity line, the opt-out line and the footer always come from code, as for templates, and every attempt is stored with its cost.
- **The trust gate for drafts**: `writer: claude` is refused by `campaigns check` and batches draft from templates until the campaign has earned it.
  `leads drafts sample --campaign S --n N [--decider claude|fake]` writes drafts for review only, never sendable; on the Drafts tab you approve or reject each one with reasons.
  `leads drafts eval [--campaign S] [--runs N] [--decider fake|claude]` drafts every reviewed lead again and records, with `--decider claude` and at least 20 reviewed leads, how many pass the check on the first try and after the retry.
  The gate passes when the latest recorded eval covers at least 20 reviewed leads, at least 90% pass first time and 98% after the retry, and you approved at least 80% of the last 20 reviewed drafts; the Campaigns tab shows where it stands.
  `leads drafts golden-export --out FILE [--campaign S]` writes the reviewed drafts to a file outside the repository.
- **Claude spend**: every Claude call is priced at its worst case before it is made and refused when that would pass a cap, then its real cost is recorded per lead and per purpose (drafting, research, drafts eval).
  `LEADHOUND_CLAUDE_DAILY_USD_CAP` is the ceiling for a day across every purpose; each purpose has its own daily cap and a cap per lead under it (see `deploy/env.example`).
  Claude is called only with `LEADHOUND_LIVE=1` and the key file; the key is never read from the environment (see "The Anthropic key" in `deploy/README.md`).
- **The upgrade-risk report**: `leads report LEAD --out FILE [--contact ADDRESS]` writes a Drupal lead's free one-page report, one self-contained HTML page, from stored signals only: its Drupal version and where it was seen, the end-of-life date and the days left, and what an update or migration involves for that version (Drupal 10 to 11 is an update, never a migration).
  Every line names its source, and the same grounding check refuses any number that is not a stored fact.
  The file is written at mode 0600, outside the repository, never over an existing file; the lead page's Report link (`/leads/reports/{id}`) serves the same page to the signed-in operator.
  The emails offer the report and you send it when the lead replies: no email carries a link to it or attaches it, and there is no public address for it.
- **Erasure**: `leads forget org ID --reason TEXT` erases an organisation's personal data in one transaction, as the owner role: its phone facts, phone research and screenings, its call notes, its kept page bodies, its description and social facts and its map listing's phones and emails.
  It suppresses the organisation's domain and its numbers (hashed), so it is never collected, researched, enriched or called again, and cancels its pending messages.
  It prints counts only, and run again it finds nothing left; see "Erasure" in `deploy/README.md` for backups.
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
  `leads send pause [--reason TEXT]` and `leads send resume`, or Sending on the console's Controls page, stop and restart sending; the pause is checked before every message.
  The other leads jobs have switches too (`leads switches list`, `leads switches set JOB on|off [--reason R]`): a job switched off runs nothing, records its run as "switched off" and exits 0.
  `leads sync` reads replies, bounces and opt-outs from every mailbox and suppresses where they say so; it exits 1 when any mailbox could not be read.
  `leads selftest --to ADDRESS [--mailbox ID]` sends one real message from one mailbox to one of `sender.operator_addresses`.
- **Mailboxes**: the sending mailboxes live in the database, not in the environment; add any number on the console's Mailboxes page, or with `leads mailboxes add`, and each is checked (both sign-ins, the Sent and Junk folders, its domain's SPF, DKIM and DMARC) without sending anything.
  Each password is sealed with a public key as it is entered, and only the send, sync and check jobs hold the private key; `make mailbox-key` makes a local one, and the first deploy makes the server's.
  Each mailbox has its own warm-up ramp (at most 40 a day), and each sending domain a daily limit over all its mailboxes (`caps.domain_per_day`, default 50) and a cap on its mailboxes (`caps.mailboxes_per_domain`, default 2), because the large providers judge reputation by the sending domain: see [Google's sender guidelines](https://support.google.com/mail/answer/81126), [their FAQ](https://support.google.com/a/answer/14229414) and [Microsoft's 550 5.7.515](https://support.microsoft.com/en-us/outlook/fix-ndr-error-550-5-7-515-in-outlook-com).
  A mailbox pauses itself on repeated hard bounces or a refused login, and a policy block pauses its whole domain; only the operator resumes.
  The old mailbox lines in the env file are no longer read.
- **Campaign mailboxes**: a live batch drafts only onto the mailboxes assigned to its campaign, ticked on the Campaigns tab or set with `leads mailboxes assign|unassign ID CAMPAIGN`, and a campaign with none drafts no live batch; an unassigned mailbox's sends still count toward its domain's limit.
  A message whose mailbox is not assigned to its campaign is held, never moved to another mailbox.
  The mailbox check also opens `https://<host>/` and keeps where it lands, and the Campaigns tab warns when a mailbox's domain sends visitors away from the campaign's site.
  Adding a first mailbox, the key and its backup, the limits, the pauses and the first test are in "Mailboxes" in `deploy/README.md`.
- **Suppression**: `leads suppress add address|domain VALUE [--reason manual|opt_out|bounce|replied_no]`, `leads suppress list` and `leads suppress check VALUE`; a suppressed address or domain is never drafted or sent to.
  An opt-out is read from the subject and the sender's own words only, never from quoted text or our own lines, and a draft whose text holds an opt-out word outside the opt-out line is refused.
  A suppression recorded by mistake is lifted by the operator only, by number, with a reason (`leads suppress lift ID --domain DOMAIN --reason TEXT`, or Lift a suppression on the console's Leads page); the lift is kept as a record, lead states do not change, and a new opt-out suppresses again.
  `leads suppress review` reads past opt-outs again and lists those that were only quoted text, with the lift line for each; it writes nothing.
  The details are in "Opt-outs, suppressions and lifting one" in `deploy/README.md`.
- **Settings and data**: `leads check-config [--agencies FILE]` checks `leads.yaml`, the templates and the agency list without printing any value; `make deploy-data` runs it, then copies `leads.yaml`, `agencies.yaml` and `leads-templates/` to the server with the jobs settings (`leadhound.yaml`, `ats-boards.yaml`); `examples/leads.yaml` is the starting point for your own.
  The console's Settings page changes some of these values over the file, in the database; `leads settings show` says which, and `leads settings reset` puts them back.
  `leads transfer export DIR` and `leads transfer import DIR` move lead data from one database to another, re-keyed by natural keys so they never collide with the other side's ids; an import can run again safely, it is refused while a live message may be in flight, and both print counts only.
  A transfer does not yet carry café places or an organisation's region.
- **On a server**: five leads timers run the loop (probe daily; contacts daily, which then runs enrich, rate and build; the batch each morning; send hourly in business hours on weekdays; sync every 15 minutes); every deploy installs them, each stays off until `/etc/leadhound/env` allows it, and each job has its own switch on the console's Controls page.
  The rate step also needs `LEADHOUND_LIVE=1` and `TYPESAFE_API_KEY` in that file; without them it rates nothing, and the build after it still runs.
  `LEADHOUND_LEADS_TIMERS=on` turns on finding (probe and contacts).
  Sending (batch, send and sync) needs both lines, `LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live` (without the first, the next deploy turns them off again), Sending switched on on the Controls page, and a ready mailbox assigned to the campaign; even then every batch is read and approved by the operator before anything is sent.
  `LEADHOUND_LEADS_SEND` is retired: the first deploy with the console switches carries it over once, pausing sending when it was not `on`.
  The deploy prints which leads timers are on and, for those off, which lines are missing; the steps are in "Leads on the server" in `deploy/README.md`.
  The batch and contact research units hold the Anthropic key as a systemd credential and fail to start without it; nothing else holds it, the console least of all.
- **Migrations**: the leads tables have their own migration set, versioned in `goose_db_version_leads`; `leadhound migrate` applies the core set (up to 00009), then the leads set (up to 00024).
  The stars, enrichment facts, relevance ratings and evals, console settings and your calls are each append-only tables, and so are the campaign versions, call outcomes, email consents, Claude drafts and their reviews, screenings, places, job switches and campaign mailbox assignments: every change is a new row, and the app role cannot update or delete them.
  Only `leads forget`, run as the owner role, removes an organisation's personal data.

### Café places

The café campaign (`examples/campaigns/cafes-loyalty/`) finds independent cafés from Overture Maps Places instead of a site list.
LeadHound reads no parquet: you export one country, or a few, to NDJSON with [DuckDB](https://duckdb.org/) and import the file.
Save this as `export.sql`, with your release and country codes in place of `2026-10-21.0` and `'RO'`:

```sql
INSTALL spatial; LOAD spatial; INSTALL httpfs; LOAD httpfs; SET s3_region = 'us-west-2';
COPY (
  SELECT id, names, basic_category, taxonomy, confidence, websites, socials, emails, phones, brand, addresses, sources,
         ST_AsGeoJSON(geometry)::JSON AS geometry
  FROM read_parquet('s3://overturemaps-us-west-2/release/2026-10-21.0/theme=places/type=place/*', hive_partitioning = 1)
  WHERE (taxonomy.primary IN ('coffee_shop', 'cafe')
         OR (taxonomy.primary IS NULL AND basic_category IN ('coffee_shop', 'cafe')))
    AND addresses[1].country = 'RO'
) TO 'places-ro.ndjson' (FORMAT json);
```

- **Running it**: `uv run --with duckdb python -c "import duckdb; duckdb.connect().execute(open('export.sql').read())"` runs it without installing DuckDB; the `duckdb` command line does the same with `duckdb < export.sql`.
  It reads the Places theme straight from Overture's public bucket, with no account or key.
- **The release**: each release is a folder under `s3://overturemaps-us-west-2/release/`, and the newest is the last one listed by `curl -s 'https://overturemaps-us-west-2.s3.amazonaws.com/?list-type=2&prefix=release/&delimiter=/'` (or `aws s3 ls --no-sign-request s3://overturemaps-us-west-2/release/`).
  Pass the same release to the import.
- **Categories**: since release 2026-10-21.0 a place has no `categories` column; its category is `taxonomy.primary`, with `basic_category` for a place that has no taxonomy.
  The import reads all three, so an export from an older release that still selects `categories` imports too.
- **Import**: `leads places import overture --campaign cafes-loyalty --release 2026-10-21.0 places-ro.ndjson` keeps the places the spec's `source` allows (categories, countries, `min_confidence`), skips chains (`chains`: a name or website on more than `max_locations` places, a listed name, or an Overture brand unless `keep_branded`), and makes a site and an organisation for each independent café with its state or region.
  It prints counts only; a malformed line is named by its number, never its text, and a release is imported once (run it again and nothing is stored twice).
  `examples/places.demo.ndjson` is a fictional export with reserved names only.
- **The loop**: `leads sites probe` reads each café's homepage with the `cafe@1` detector (digital loyalty, online ordering, Instagram, language), then `leads contacts`, then `leads build --campaign cafes-loyalty` and `leads batch new --campaign cafes-loyalty`.
  The timers build and batch `drupal-eol` only, so the café campaign's build and batch run by hand.
  A café whose homepage links your own loyalty product is a customer and gets no lead: list your product's hosts in `leads.yaml` as `probe.cafe_own_vendor_hosts` (private; never in the repository).
- **Kinds**: `cafe@1` sorts each café into one of three kinds, and a kind the spec's `kinds` does not list makes no lead.
  `cafe_no_digital_loyalty` is a café with a website and no digital loyalty on it.
  `cafe_has_loyalty` is a café already on another loyalty platform; the email can name that platform.
  `cafe_no_website` is a café with no site of its own, built from its place alone.
  The example turns all three on, and each kind needs its own templates in every language its countries use.
- **Overture contacts**: a place's own emails and phones become routes marked as coming from Overture, through the same filters the contacts crawl applies (role addresses, public bodies, phone screening).
  A route the crawl or research found is never overwritten, and importing the same release again adds nothing twice.
  That is how a café with no website can still be written to or called.
- **Footer, links and the vendor slot**: `countries.<CC>.footer` names a footer (`footers/<name>.txt` in `leads.yaml`'s templates dir) that this campaign's emails to that country end with; a country without one keeps `leads.yaml`'s footer.
  `offer.links` lists the URLs an email may carry beside `product.site_url` (absolute `https://`, at most 10); any other URL in a draft stops the lead.
  `offer.vendor_links` maps a loyalty vendor id from `internal/cafe/signals.yaml` to a page, with `default` for a vendor that has no entry of its own.
  A café lead's templates get two slots from it: `VendorName` (the first loyalty vendor its homepage links) and `VendorLink` (that vendor's page, else `default`).
  Both may be empty, so a template tests them: `{{if .VendorName}}…{{else}}…{{end}}`, never a bare `{{.VendorName}}`.
- **Countries**: every country in the example runs the same email-first sequence; every email country also needs its `leads.yaml` policy and opt-out line, and a footer: the campaign country's own or the one its `leads.yaml` policy names.
  A US, Canadian or Australian café is callable only with its state known, which the import stores from the place.
- **Attribution**: the place data comes from Overture Maps Foundation (overturemaps.org); Overture publishes the Places theme under CDLA Permissive 2.0, and a lead from a place says so on its page.
  Check Overture's attribution page for the release you export: themes that include OpenStreetMap data are under ODbL and need "© OpenStreetMap contributors" as well.
