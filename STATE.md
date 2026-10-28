# webbership-leadhound — STATE

## Where we left off
Campaigns, Claude-written outreach and a 5-step mixed sequence (2026-10-28, on local main, not pushed or deployed; plan `leadhound-campaigns-claude-outreach`, in CP9: docs done, the two reviews and `make prepush` next):
- Committed: CP1 accounts and campaigns, Drupal as campaign `drupal-eol` with no change in behaviour (`0023baa`); CP2 the Claude client, the spend ledger and caps, the key as a systemd credential (`408be44`); CP3 the sequence, call outcomes, Today and the call card (`d3472d9`, walk fixes `6c92a9a` and `d9b31ef`, the theme switch `3133223`, polish `73b377d`); CP3b contact research on star, CP4 Claude drafts with the grounding check and the trust gate, CP5 calling rules, screening, phone research and erasure, and CP6 the upgrade-risk report, in one commit because they share files (`f4e819e`); CP7 the café campaign from Overture places, with the walk fixes (`9c5cbc7`).
- What exists: campaigns as versioned specs (`leads campaigns check|register|list|show`); the 5-step sequence with a call step and the call-first sequence for consent countries; Today, the call card and `leads calls list|outcome`; one call rule over the country table (`internal/compliance`); screening by file (`leads phones screen export|import`); contact research on star (`leads research request|list|run`) and phone research (`leads phones research`), each number or address verified on the page it was cited from; Claude drafts (`leads drafts sample|eval|golden-export`, the Drafts tab, the gate on the Campaigns tab); `leads report`; `leads forget org`; `leads places import overture`.
- Migrations: core up to 00009 (`ai_usage` cost), leads up to 00021 (00016 accounts and campaigns, 00017 sequence and calls, 00018 contact research, 00019 drafts, 00020 phones and screening, 00021 places); dev is migrated to both.
- Deploy pieces: `/etc/leadhound/anthropic.key` (root 0600) reaches only `leadhound-leads-batch` and `leadhound-leads-research` through `LoadCredential=`; the console refuses to start if it can see a key; `leadhound-leads-research.path` starts research when a star asks.
- Live on dev (28 Oct, nothing sent, $0.16 in all): research `--max 3` found nothing, correctly, so the found path is unproven live; two drafts samples showed caching working and led to the writer fixes in `9c5cbc7`; the 3-lead re-check found the drafting per-lead cap too small for a retry, which another builder is fixing (the writer's output ceiling and the per-lead cap).
- Docs: README "Campaigns and leads" (with "Café places"), `deploy/README.md` "Campaigns on the server", "The Anthropic key", "Calls, screening and phone research", "Erasure".
- Found while writing the docs: `make deploy-data` does not copy `campaigns/`, `make remote` hands the Anthropic key to no command, and `leads forget` needs the owner's `migrate.env`, so the docs give the hand steps; a campaign spec has no caps key although `deploy/env.example` says a campaign may set its own.

The Leads console (2026-10-27, pushed and deployed to production; plan `leadhound-leads-console`, archived):
- Committed: CP1 the data model (`ed6e42e`: findings, kinds and channels as rows, Jev usage counted per purpose), CP2 the list and the lead page with Outreach moved (`fc24933`), CP4 enrichment (`55afa3e`: phones, descriptions and social links from the org's own pages), CP3 stars and the outreach order (`2f48058`), CP5 Settings in the console with the priorities page retired (`cafca15`), CP6 Jev relevance with a golden set and an eval (`8efa0f7`).
- Then `5ae1a3f`: your calls in the database (leads migration 00015 `lead_label`, "Your call" on the lead page, the To label filter, the eval reading the calls from the database by default), the Settings help text, the console polish, and the final review's fixes (the probe waits for the fetch lock, `batch new` refuses while saved settings are ignored, Jev calls per purpose on the dashboard, a huge `?page=` shows the last page).
- Then `114f782`: `make gate` runs the whole gate once and stamps the commit; `make deploy` skips the gate on a stamped clean HEAD; the scale seed analyses each step and reseeds on an emptied database.
- Deployed 27 Oct from `5ae1a3f`: the console answers, the finding timers (probe, contacts) are on, sending timers stay off.
- New commands: `leads enrich`, `leads rate`, `leads relevance golden-draft|eval`, `leads settings show|reset`.
- The contacts unit now runs contacts, enrich, rate and build, with `TimeoutStartSec=7h`; rating needs `LEADHOUND_LIVE=1` and the Jev key on the server.
- Leads have their own Jev cap, `LEADHOUND_JEV_LEADS_DAILY_CALL_CAP` (10,000 by default), apart from the jobs cap.
- Migrations: core up to 00008, leads up to 00015; dev is migrated to both.
- Dev: 1,185 leads built, enriched (phones on 666 orgs, descriptions on 823, social links on 988), and screenshots of every Leads page at 1280 and 390 px in light and dark, outside the repo.
- Not checked from here (the server could not be read): the leads migration level in production and whether the server env has `LEADHOUND_LIVE=1` and the Jev key; the dashboard's "for leads" Jev line after the next contacts run shows it.
- Next: give your call on 30 leads under Leads → To label in production, then run `leads relevance eval --decider jev` with `LEADHOUND_LIVE=1` (about 90 calls) and record the numbers under "Eval baseline".

No false opt-outs (2026-10-26, on local main, not yet pushed or deployed; plan `leadhound-false-opt-outs`):
- CP1 (detection, `b47f703`) and CP2 (lift, `4bb4a5d`) are committed: an opt-out is read from the subject and the sender's own words only, our own lines and quoted text are taken out, a multipart reply's first text part is decoded, and a draft with an opt-out word outside the opt-out line is refused.
- A suppression is lifted by number with a reason (`leads suppress lift`, or Lift a suppression on the console's Leads page); the lift is a record, lead states do not change, transfer carries active suppressions only.
- CP3 is in progress: `leads suppress review` (reads past opt-outs again, writes nothing) and the docs ("Opt-outs, suppressions and lifting one" in `deploy/README.md`).
- Left for CP4: the risk review, then push and deploy on the operator's yes, lift suppression 2 on production (the selftest reply), and run the review there. Sending stays off.

Mailboxes are built (2026-10-26, on local main, not yet pushed or deployed; plan `leadhound-mailboxes`):
- Any number of sending mailboxes live in the database and the console's Mailboxes page (add, check, pause, resume, retire), with the same verbs in `leads mailboxes`; the four old env mailbox lines are no longer read.
- Each password is sealed with a public key at entry; only the send, sync and mailbox-check jobs get the private key (`/etc/leadhound/mailbox.key`, a systemd credential), and the console never can open one.
- A guided check (both sign-ins, the Sent and Junk folders, SPF, DKIM, DMARC on the address's own host) runs in `leadhound-mailbox-check.service`, started by a `.path` watcher; nothing is sent.
- Each mailbox has its own ramp under the 40 ceiling; each registrable domain a daily limit (50) and a mailbox limit (2); new leads go to the ready mailbox with the most room, follow-ups from the same one.
- Health pauses: hard bounces, a refused login, an unusable password; a policy block refuses that email and pauses the domain.
- Docs: "Mailboxes" in `deploy/README.md`. Next: the security review, the local walk, then push and deploy.

The Run now card is built (2026-10-26, not yet deployed):
- The console dashboard can ask for four runs: show results now, fetch new jobs then show, send today's email now (only while today is unsent), and the nightly ATS collect behind a confirm.
- The console only writes a request file under `/opt/leadhound/data/run/`; one systemd `.path` unit per action starts its fixed service, so the console gains no privilege.
- `leadhound preview` composes the brief's jobs part exactly as `brief` does (`composeNow`) and writes `preview.json`; it never mails, never writes `brief_log`, never shortlists. `/results` renders it with the track actions.
- The card's state is read step by step from the request time and the `run` table, so one action's run never ends another's; a press while the same action waits or runs is refused with its time.
- Walked locally against the dev database: preview wrote 10 jobs (5 apply, 5 your call), the card turned done with a results link, a second ATS press was refused.

Jobs (slice 1) and Drupal leads (slice 2) are both built and run from the production server only, on systemd timers; the leads timers are installed but switched off.
The release for the shared server is in, in seven commits from `a00d310` to `92c4150`:
- Six indexes for the hot lookups, built concurrently, and per-role Postgres limits applied by every deploy (3-minute statement timeout, no parallel workers, no JIT, 512 MB temp cap, 32 connections).
- A streamed ranking with bounded top-k per tier for every caller (brief, recheck, leave-out-stale, score summary, console), with drift tests against the old query.
- The scorer's time budget (`score --max-duration`) and one scorer at a time; the console served from a background snapshot.
- Memory ceilings with `GOMEMLIMIT` and no `MemoryHigh`, so a unit over its ceiling is killed and restarted instead of hanging.
- `make test-scale` (450,000 jobs and a year of leads), which `make deploy` runs before it builds.
- The console as the place to manage everything: a page per job, rule forms, the Leads pages (batches read and approved in full, the queue with pin, push back and skip, the priority profile, the send pause; the queue and the priority profile have since given way to the List's stars and order and to Settings); `leads transfer` and `leads check-config`; the five leads timers.
- The storage budget (`LEADHOUND_DB_MAX_BYTES`, `LEADHOUND_DISK_MIN_FREE_PCT`, stop reason `disk_budget`), dumps kept within 4 GiB, and the nightly `trim`.

Measured on the production-sized local database:
- The score queue hit its 10-minute statement timeout without the indexes and takes 8.3 s with them; score inputs for 500 jobs 110 ms → 27 ms; funnel, first-seen counts, tracked lists and collector lookups each under 300 ms.
- Brief build 2,903 MB of heap and 75 s → 4.1 MB and 16.5 s; score summary 2,778 MB → 4.0 MB.
- Dashboard 6 ms fresh, 1 ms while updating, 510 ms cold.
- Leads: candidate queue 2.0 s → 100 ms, next-up 1.96 s → 33 ms, new batch 2.1 s → 91 ms, the brief's probe line 927 → 47 ms, the dashboard with leads 926 → 73 ms; a transfer import of a million sites at 63 MB of heap.
- One trim batch of 5,000 in 155 ms; the score queue with the trim's exclusion 8.4 s.

Measured on the server (two deploys: the indexes, limits, streamed ranking and scorer budget, then the console, leads timers and transfer):
- A LinkedIn collect and score: peak memory 31.2 MB under a 1G ceiling; 85 requests, 0 failed, 214 new; the score scored 10,005 jobs with 0 failures and stopped at the daily call cap, the first stored decisions; load average 0.07.
- A brief without sending: 30.6 s and 24.5 MB peak (it was 994 MB and a one-hour timeout).
- The night's ATS run under the new units: the collect read every board in 1 h 47 min (all six sources finished, 0 failed requests) at a 473 MB peak under a 1200M ceiling, load about 0.2.
  Its score step never started: GNU `timeout` refuses a compound duration (`2h20m`), so the unit exited 125; the night's new ATS jobs wait for the next score run.
  Fixed in `92c4150` (`140m`, and the leads send unit's `3h45m` → `225m`); a deploy test now refuses any duration GNU `timeout` would refuse.
- The lead data moved to the server: 134,441 sites, 277,334 fetches and 134,441 probes (804.7 MB exported in 3.8 s, imported in 52 s at 312 MB peak); the probe eval agrees on both sides, 92 of 93 labels.
  No organisations, routes or leads moved: the server's contacts timer builds them.

The third deploy (the storage budget and the trim): core migrations at 7, the trim function owned by the owner role and executable by the app role, the trim timer at 15:00, every leads timer still off.
Before it the database was 2,178 MB, `job_observation` 1,098 MB; a fresh dump of 445 MB was written before the first trim.
The first trim, run by hand at a quiet hour: 87 batches over all 434,802 observations in 22 s at 8 MB peak, trimmed nothing.
That is right: the oldest observation is from 2026-10-24, so nothing is past the 7-day rejected or 30-day expired age yet.
The wide-net night's rejected postings (about 380,000) reach 7 days on 2026-10-31, and the 15:00 timer trims them from then on.

## Eval baseline: lead relevance
Not measured yet.
It waits for 30 of the operator's calls under To label, then `leads relevance eval --decider jev`.
Until a recorded eval passes the trust gate (30 or more labelled leads, within one band on at least 80%, Spearman's rho at least 0.5), the list and the queue order by the fallback order's points and the number shows as early.

## Eval baseline (jev-1.13.0, 2026-10-23)
Private golden set, n=10, 9 of 10 non-English, 3 runs, no disagreements between runs.
Exact match: title 90%, stack 80%, seniority 90%, location 90%, work_rights 100%, gate language 100%, gate workplace 1/1, overall 100%, all criteria 45/50 (90%).
Input tokens 56,106 (about $0.0024).
Early number: grow the set with 10–20 English postings before treating it as the bar.

## Current focus
Finish CP9 of the campaigns plan: the Fable risk review and the Opus review, their fixes, `make prepush`; then push and deploy on the operator's yes (11 to 13 Nov), for the first sends on 16 Nov.
Land the Leads console: the final review, push and deploy on the operator's yes, then the production steps in order ("Turning the leads on in production" in `deploy/README.md`), then the 30 calls and the relevance eval.
Watch the first full night on the new units (the ATS score with the fixed timeout, the morning collect and brief) and the first real trim around 2026-10-31.
Hand the leads over to the operator: finding first, sending once the mailbox passes its selftest, every batch read and approved in the console.

## Slice 2 notes
- `fetch.WithPublicOnly` (`internal/fetch/guard.go`), `internal/robots` and `probe.HostRobots` exist for slice 1's CP6b to reuse.
- The leads migrations are their own set (`internal/leadstore/migrations/`, now up to 00021), versioned in `goose_db_version_leads`; `leadhound migrate` runs the core set, then the leads set.
- `storetest.ResetDB` keeps every goose version table.
- Live-unproven until the shadow week: real SMTP, IMAP and DNS (UIDVALIDITY rescan, Junk and Sent discovery, appending to Sent); the fakes cover them in tests.

## Next
Campaigns plan, before push:
- [ ] Land the writer's output ceiling and the drafting per-lead cap so one retry always fits, then re-run the 3-lead live drafts check.
- [ ] The Fable risk review (key path, phone and call data, erasure, screening) and the Opus review (campaigns, sequence, grounding, console, report, places); fix and verify each finding; `make prepush` green.
- [ ] Live checks still open: a contact research that finds an address; `leads phones research --max 3` on dev with each stored number's page opened by hand; which web tools Haiku took on the first live run.

Campaigns plan, the operator's decisions:
- [ ] Confirm the country rule settled for the build: a country the law table marks off (AT, PL, CZ, BG) blocks calls with no override, and a campaign's own `call: off` warns and leaves the call to you.
- [ ] Confirm the country table's readings: CH unlisted (never emailed or called by the table; form-only stays), 09:30 to 17:00 weekdays where the law names no hours, DE calls need a written `acknowledge:`, IE calls landlines only, RO's entry has no source URL, most entries are `unverified` or `operator_decision`.
- [ ] Approve the wording: the `drupal-eol` templates and the `call_first` templates for every kind, the report's per-version texts (`report.DefaultMajorTexts()`), and write your real proof line in `data/campaigns/drupal-eol/campaign.yaml`.
- [ ] Make the Claude Console key in its own workspace with a monthly spend limit, and place it on the server before the deploy (the batch unit fails to start without it), or decide on `ImportCredential=` after checking the server's systemd is 254 or later.
- [ ] Review 20 or more sampled drafts on the Drafts tab (around 4 to 6 Nov), then run `leads drafts eval --decider claude` for the trust gate.
- [ ] Write the privacy notice and set `sender.privacy_url` by 7 Nov, before any call campaign is batched.
- [ ] Screen GB numbers against TPS and CTPS before the first GB call.
- [ ] Decide whether to rewrite the unpushed `f4e819e`, which holds the operator's name in two test files (fixed forward in `9c5cbc7`), so it never reaches GitHub.
- [ ] Decide whether a daily timer should sweep contact research attempts left queued past a run's 20.

Campaigns plan, on the server after the deploy:
- [ ] Check `make deploy` migrated core to 00009 and leads to 00021; copy `campaigns/` by hand and run `leads campaigns register`; check `systemctl list-units 'leadhound-*.path'` shows `leadhound-leads-research.path` waiting.

The Leads console, after its deploy:
- [ ] The final review of the plan's diff and `make prepush`; commit the labels and help-text work.
- [ ] Push and deploy on the operator's yes; check that `make deploy` migrated the core set to 00008 and the leads set to 00015.
- [ ] Label 30 leads under To label (`/leads?to_label=1`), then run `leads relevance eval --decider jev` and record the numbers in "Eval baseline: lead relevance".

Run now, after its deploy:
- [ ] `systemctl list-units 'leadhound-*.path'` shows five watchers active and waiting (the four Run now ones and, after the mailboxes deploy, `leadhound-mailbox-check.path`); press Show results now and see a `preview` run and the results page; press Fetch new jobs and see collect, score, preview in that order.

Waiting for the operator:
- [ ] The warm-contact suppressions: suppress the addresses and domains of people already known, before any finding or sending.
- [ ] Turn finding on (`LEADHOUND_LEADS_TIMERS=on`), then check the first probe and contacts runs.
- [ ] After the mailboxes deploy: copy `/etc/leadhound/mailbox.key` off the server by hand (losing it means re-entering every mailbox password).
- [ ] Add the outreach mailbox on the console's Mailboxes page and get a passed check.
- [ ] Add `LEADHOUND_OUTREACH=live` (sending timers stay off), then send the selftest: `leads selftest --to ADDRESS --mailbox ID`.
- [ ] Turn sending on (`LEADHOUND_LEADS_SEND=on`, beside `LEADHOUND_LEADS_TIMERS=on` and `LEADHOUND_OUTREACH=live`) and restart the console.
- [ ] Read and approve the first batch in the console.

Known follow-ups:
- [ ] An expired job trimmed after 30 days that comes back on its board but is dropped by the title screen is reopened without a new observation: it stays open with empty text and its old verdicts, out of the score queue, while its old decisions still feed the ranking.
- [ ] `DueSites` may want an index on `site(list_rank, id)` as the site list grows.
- [ ] The app role can still `UPDATE description_clean` directly, outside the trim function.
- [ ] The leads timers have not yet run on the server.
- [ ] The connection worst case at the 15:00 trim is 24 (the afternoon collect and its score can overlap it), within the 32 limit.
- [ ] The brief says "recheck not run" when the recheck ran with nothing due (`internal/brief/compose.go:751`, `cmd/leadhound/recheck.go:52`).
- [ ] Rename `actor` `gremy` to `operator` in the existing lead tables (a migration over append-only tables, as owner); the mailbox tables already use `operator | system`.
- [ ] CP6e/6f/6b: remote-job sites, more sources (each checked for permission first), the employer's own apply route.
- [ ] Harden `org.site_id` with `UNIQUE (site_id)` in a later leads migration: today a site shared by two orgs would fail 00010 or a build loudly, never corrupt; the writers prevent it and dev has none.
- [ ] An editable sending pace, sender and country channels, with a review of the send path, since the send reads them from the file (the Claude API and the call step landed in the campaigns plan).
- [ ] From the campaigns plan: `make deploy-data` should copy `campaigns/`; a way to run the Claude commands and `leads forget` through `make remote`; the org id on the lead page (`leads forget` takes it); caps per campaign in the spec (or drop the line in `deploy/env.example` that says a campaign may set them); `leads transfer` does not carry places or `org.region`; the `phones.retain_days` trim is not built; no `entity_type` fact is stored (unknown counts as an individual); US, CA and AU calls need a region, which only café places give; the grounding check's echo rule has no org text to compare yet; the greeting is English only; the café example's Italian and Spanish leads use English templates; a café domain already known from Drupal keeps its old org with no region; the Overture attribution wording is unchecked against the release page.
- [ ] Plan C: more lead sources (job postings, tracking, registries), and the migration for an org with no domain.
- [ ] The everything-configurable plan, after plan A lands: the brand voice, the templates and the message structure editable in the console, with a preview and versions; an API a local AI skill can call; a LIVE→LOCAL database sync.
- [ ] Data gaps found while writing the Settings help text, for that plan: each kind's description, the real batch size, the env defaults, and when the rating run happens.
- [ ] `make remote … ARGS='leads enrich …'` does not take the leads fetch lock as the contacts unit does, and `leads settings show` needs `CONFIRM=1` though it only reads.

## Key files
- `CONTRIBUTING.md` (the public-repo rules)
- `Makefile`
- `cmd/leadhound`
- `internal/config`
- `internal/ai`, `internal/candidate`, `internal/scoring`, `internal/eval`
- `internal/brief`, `internal/mail`, `internal/ats`
- `internal/leadconfig`, `internal/leadstore`, `internal/probe`, `internal/contacts`, `internal/outreach`, `internal/mailbox`, `internal/leadbrief`
- `internal/console`, `internal/diskfree`
- `internal/leadview`, `internal/enrich`, `internal/leadrate` (the Leads console, enrichment, relevance)
- `internal/preview`, `cmd/leadhound/runnow.go`, `cmd/leadhound/preview.go` (the Run now card)
- `internal/campaign`, `internal/claude`, `internal/compliance`, `internal/research`, `internal/phones`, `internal/dial`, `internal/report`, `internal/places`, `internal/cafe`; `internal/outreach/{factsheet,ground,writer,gate}.go` (the campaigns plan)
- `examples/campaigns/` (the fictional `drupal-eol` and `cafes-loyalty` specs and templates)
- `deploy/` (units, `install.sh`, `sql/limits.sql`, `README.md`)

## Links
- Live URL ->
