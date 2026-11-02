# webbership-leadhound — STATE

## Where we left off
The console API and its local tool (2026-11-02, plan `leadhound-apply-api-and-skill`, Checkpoint 1; built locally, not deployed):
- What exists: bearer-token `/api/` routes on the console (next jobs, one job, tracked, mark), off until `LEADHOUND_API_TOKEN_SHA256` is set; the `leadhound api` commands; `make api-token`. Docs: `deploy/README.md` "The console API".
- Not verified: anything on the server; whether its Caddyfile leaves `Authorization` redacted in access logs (Caddy's default; the Caddyfile is not in this repo).
- Next: the review of Checkpoint 1, then the fake application form (fixture) and the skill, both outside this repo; production only after those, on the operator's word.

An unsure email check no longer holds a send (2026-11-02, committed; deploy waits on the operator's yes):
- What exists: Reoon's `unknown` sends on every road (morning batch, hourly, `--now`, Send now, your own addresses), whatever `verify.catch_all` says, and is not asked again until the 90-day answer expires. Invalid, disabled, throwaway, full and trap addresses stay refused. Send now on chosen leads: an unchecked lead no longer takes the button from the others and goes back to new, where the check reaches it. Approve and send now keeps the button held on an unchecked address (no skip list, and an approved address goes unchecked on the hourly send). Send now again goes and says the hourly send takes it with no check.
- Open: with Reoon down or out of credits an address stays unchecked and waits; a lone unchecked lead still needs Check email now. A hand-added address with the Email check off still stops.
- Not verified: the page on a real console; proven by tests and a render test.

Lead outreach for every channel (2026-11-02, plan `leadhound-lead-outreach-manual-channels`, pushed and deployed):
- What exists: LinkedIn as a channel and a route (found only as a link on the organisation's own pages; the leads fetch clients refuse linkedin.com, the job-search side is unchanged); the List's tabs by next action (Email-ready, Manual, Dead end, Contacted) and the Next step card, one SQL definition; Claude LinkedIn notes and messages and form messages, with Copy and Mark sent; editing any draft (`leads drafts edit`), keeping the original; Draft it (`leads drafts make`, `leadhound-leads-draft.path`, a Status row and no Controls row); your own contacts (`leads contacts add|edit|retire|list`), claimed and retired by marks; the proof library and sectors (`leads proof add|list|retire`, `leads sector set`); proposals (`leads proposals add|status|list`); the wider timeline; learning from your saved versions; Look again with LinkedIn (`leads research request --linkedin`); the note limit (`sender.linkedin_note_max`); erasure of all of it.
- Migrations: leads up to 00029, applied in production.
- Decided in the build, each with its why in the plan's Progress: the send readers skip a retired address; a retire cancels everything unsent aimed at that value and a claim does not revive it; an unapproved manual batch holding an email refuses Mark sent; `leads transfer` skips retired routes and takes a lead on a route with no page across with no route.
- Docs: README "Campaigns and leads"; `deploy/README.md` "Draft it", "Leads migration 00029" (the app role can now re-hash an unapproved batch), "LinkedIn and your own contacts", "Erasure".
- Verified: the walk on dev (the List's tabs add up to every lead; Draft it, Copy, edit, a refused note, Mark sent and the follow-up, a contact added and retired, a proposal, erasure twice, 1280 and 390 px in light and dark); one live Claude LinkedIn draft and one form draft (the note in limit, the proof not the opener); live research on three dead ends (one email found, one nothing, one unusable answer; no LinkedIn page in so small a sample); `make gate`, `make test-scale` on a fresh seed, `make prepush`, all integration tests; an independent review of the whole change.
- Verified live: the watcher waits on the server and its service ran with an empty queue (`no request is queued`); the console answers.
- Not verified: a Draft it press in production (it waits for the operator's sign-in); a live LinkedIn page found by research.
- Worth watching: manual drafts often pass on the second attempt (a product name in no cited fact, a line speaking about the reader), so a draft costs about two attempts; they stay flat until the real campaign copy and proof are in.
- Known limit: a person's name typed into a saved edit, with no person row, is not scrubbed from the writer's examples; the proper-noun check (G4b) still refuses it in any draft.
- Known limit: Controls' "next email" line counts a message held on your own address's email check, so it can name a send that the run will hold; the run's own output and note say why.
- Follow-ups: `ForgetOrg` leaves email message bodies and route values; marks do not cross `leads transfer`, so a claimed route crosses unclaimed; an edited person's routes keep the old `person_id`.
- Next: sign in, read the Manual tab's count, add your real proof on the Proof page, and press Draft it on one lead.

Run now for every scheduled job, on a rolling schedule (2026-11-01, pushed and deployed; plan `leadhound-run-now-and-adaptive-timers`, done):
- Committed: CP1 the due-check `timer-guard` (`c9120bb`), CP2 the units: 15-minute ticks, `ExecCondition=`/`ExecStopPost=`, three Run now watchers (`618e5f8`), CP3 the console: Run now on Contacts, Site probe and Morning batch, planned and overdue lines on Controls, Jobs home and Status (`a9d9c43`), review fixes (`14dd738`).
- Live 2026-11-01: three Run now watchers active; `timer-guard --explain` planned every job from its old slot (contacts 10:00, probe 13:00, batch 06:15, ats 01:00, collect 18:30); the first 15-minute tick skipped all five cleanly (Result exec-condition, no failed units). Still to see: the first planned run (collect 18:30) and a live Run now press.
- What exists: each guarded job (leads contacts, probe, batch, ATS, LinkedIn collect) runs one interval after its last start, by hand or timed, rounded to 15 minutes; with no history it keeps its old slot. The collect becomes every 12 hours (06:30, then 18:30).
- Ship rule: CP1 and CP2 deploy together; a unit's `ExecCondition=` naming a missing subcommand would keep the job from ever starting.
- Not verified: anything on real systemd (ExecCondition skips, `$SERVICE_RESULT` in ExecStopPost, the watchers); the planned lines with real timers (integration tests only).
- Known gap: the ATS Run now card (runnow.go) does not treat a deactivating service as busy, so a press in the seconds while an ATS run records its end can be counted as served.
- Open for the operator: after an outage of 1-2 intervals a catch-up run anchors on its missed plan, so the next run can follow within the hour (two LinkedIn collects back to back).
- Next: CP4, push and deploy on the operator's yes, then the live checks in the plan.

Lead page and relevance by lead type (2026-11-01, on local main, not pushed or deployed; plan `leadhound-lead-page-by-campaign-type`, Checkpoints 1-3 built, in CP4):
- Committed: CP1 rating types, the café question and state (`8a73dc3`), CP2 store, queue, list and `--type` on golden-draft and eval (`82b91a6`), CP3 the console by type (`5b48e25`).
- What exists: Drupal leads keep `relevance@1` exactly (pinned); café leads get `relevance-cafe@1`; calls, the 30-call gate and the trust per type; a café's page shows The place, its café findings with vendor names and the comparison link, and no report card; agency pages lose the report card.
- After deploy: the first contacts run re-rates every open café lead once, in the unit's second rate line (`leads rate --campaign cafes-loyalty`; about 2,300 Jev calls, cents); Drupal ratings untouched.
- Next: CP4, `make gate` and one `make test-scale`, the real walk on dev, the review, then push and deploy on the operator's yes.

Console redesign, Dashboard and Status, manual Send now and the CRM (2026-10-31, on local main, not pushed or deployed; plan `leadhound-console-redesign-manual-send`, Checkpoints 1-4b built, in CP5):
- Committed: CP1 the board (`640b3b3`), CP2 the shell with Leads | Jobs, the gear menu and the theme button (`69efac8`), CP3 the restyle (`dcb9739`, `12ea73b`), CP3b the Dashboard cards and the Status page (`6587109`), CP4 the manual Send now (`9600e3a`); CP4b the CRM is built (Replies, Pipeline, Calls, Reports, the lead timeline, note, next action and deal).
- What exists: Send now on chosen leads with a confirm page, overridable warnings (`window`, `business_hours`, `ramp`, `domain_daily`, `spacing_jitter`, `schedule`) and hard stops; `leads send --from-request` runs it from a `send_request` row, and every override lands in the run note and `send_override`; `leads verify --balance-only` and the `leadhound-leads-verify-balance` units feed Status; reply sync keeps a snippet of at most 300 characters, which `leads forget` empties.
- Migrations: leads up to 00028 (00026 Status, 00027 manual send, 00028 CRM); local dev and test DBs redid the first draft of 00027.
- Docs: README console tour, CRM and the two Send now roads; `deploy/README.md` "Manual Send now", "Leads migrations 00026 to 00028", "Reply snippets".
- Not verified: the new units with `systemd-analyze` (server only); the 100-a-day domain ceiling end to end; the empty `SetCredential=reoon.key:` (below) also sits in the balance and Check email now units.
- Next: CP5, `make gate` then `make prepush`, the risk review of the sending and data changes, then push and deploy on the operator's yes and check live (sign in, the shell, a Send now preview; selftest leads can't be picked in the console).

Console controls, mailboxes per campaign, Reoon email check, sending window and Send now: live in production (2026-10-31, `a0dda1e`; plan `leadhound-console-controls-mailbox-campaigns`, Checkpoints 1-5):
- Commits: `2c855ea` job switches + campaign mailboxes, `4230be7` Controls page, `bf62d7e` Reoon email check, `523aad5` sending window + Send now + deploy-data checked by the server's binary, `a0dda1e` gitleaks accepts the fake test key.
- Production after deploy: every switch on (Email check on, Reoon key at `/etc/leadhound/reoon.key`, root 0600); mailbox 1 → `drupal-eol`, mailbox 2 → `cafes-loyalty`, both active; the conversion marker exists; the Send now watcher is enabled with the sending timers; no failed leadhound unit; window 08:30-18:00 Mon-Fri, from the file.
- Not yet seen live: the first batch run with Reoon (Sunday 06:20 Bucharest); the Controls page signed in (timers from `systemctl show` under the hardened console unit); each campaign's landing line after the 31 Oct mailbox checks.
- To do: on Controls, save the window 08:30-18:00 so it no longer depends on `leads.yaml`; optional, remove `LEADHOUND_LEADS_SEND=on` from `/etc/leadhound/env`.
- Fix next: systemd on the server rejects the empty `SetCredential=reoon.key:` ("Invalid syntax, ignoring"), so with the key file missing the batch unit would fail at start instead of skipping the check; use a non-empty placeholder the code reads as no key.
- Known: `TestScaleConsoleDashboard` cold-dashboard budget (600 ms) is tight: 703 ms once right after the full suite, 513-528 ms alone, same as before this work.

Café campaign live in production (2026-10-29, `cccbc69`; plan `leadhound-cafe-campaign-live`, Checkpoints 1-4):
- Registered `cafes-loyalty` (Romania); `drupal-eol` unchanged. Overture release 2026-10-21.0 imported: 3,268 cafés kept, 623 chains skipped (15 on a shared or brand website), 2,293 leads (no website 1,851, no digital loyalty 442; no competitor seen in Romania yet).
- The café mailbox is assigned to `cafes-loyalty` only; the first café batch (3 leads, the new mailbox's warm-up room) waits for approval in the console; nothing café was sent.
- Café build and batch run by hand (`deploy/README.md`, "A places campaign by hand"); the timers batch `drupal-eol` only.
- Known: café leads show "Drupal 0" as their finding in `leads batch show` (display only, not in the emails); café contact-form leads came out 0 on the first production run (89 on dev), to check after the next contacts run; two no-website places sharing one address get two first emails.
- 3b fixes (`cccbc69`): a number inside a café's name counts as sourced; `batches_from` and `countries.<CC>.identity_line|opt_out_line` per campaign; reply sync strips campaign opt-out lines; café form and LinkedIn texts live in the campaign folder; platform websites count as no website and brand or shared domains are skipped like chains.

Checkpoint 1 of the same plan:
- A campaign country can name its own footer (`countries.<CC>.footer`), read from `leads.yaml`'s footers dir; a country without one keeps `leads.yaml`'s.
- `offer.links` lists the extra URLs an email may carry, and any other URL in a draft stops the lead.
- `offer.vendor_links` maps a café's loyalty vendor to a comparison page; templates get the optional `VendorName` and `VendorLink` slots.
- The café detector knows six more loyalty vendors, and an Overture place's own emails and phones become routes through the contacts crawl's filters.
- The example `cafes-loyalty` turns on all three café kinds with fictional templates; `deploy/README.md` gives the hand steps to import places and run a places campaign on the server.

Campaigns, Claude-written outreach and a 5-step mixed sequence (2026-10-28, on local main, not pushed or deployed; plan `leadhound-campaigns-claude-outreach`, in CP9: docs done, the two reviews and `make prepush` next):
- Committed: CP1 accounts and campaigns, Drupal as campaign `drupal-eol` with no change in behaviour (`0023baa`); CP2 the Claude client, the spend ledger and caps, the key as a systemd credential (`408be44`); CP3 the sequence, call outcomes, Today and the call card (`d3472d9`, walk fixes `6c92a9a` and `d9b31ef`, the theme switch `3133223`, polish `73b377d`); CP3b contact research on star, CP4 Claude drafts with the grounding check and the trust gate, CP5 calling rules, screening, phone research and erasure, and CP6 the upgrade-risk report, in one commit because they share files (`f4e819e`); CP7 the café campaign from Overture places, with the walk fixes (`9c5cbc7`).
- What exists: campaigns as versioned specs (`leads campaigns check|register|list|show`); the 5-step sequence with a call step and the call-first sequence for consent countries; Today, the call card and `leads calls list|outcome`; one call rule over the country table (`internal/compliance`); screening by file (`leads phones screen export|import`); contact research on star (`leads research request|list|run`) and phone research (`leads phones research`), each number or address verified on the page it was cited from; Claude drafts (`leads drafts sample|eval|golden-export`, the Drafts tab, the gate on the Campaigns tab); `leads report`; `leads forget org`; `leads places import overture`.
- Migrations: core up to 00009 (`ai_usage` cost), leads up to 00021 (00016 accounts and campaigns, 00017 sequence and calls, 00018 contact research, 00019 drafts, 00020 phones and screening, 00021 places); dev is migrated to both.
- Deploy pieces: `/etc/leadhound/anthropic.key` (root 0600) reaches only `leadhound-leads-batch` and `leadhound-leads-research` through `LoadCredential=`; the console refuses to start if it can see a key; `leadhound-leads-research.path` starts research when a star asks.
- Live on dev (28 Oct, nothing sent, $0.16 in all): research `--max 3` found nothing, correctly, so the found path is unproven live; two drafts samples showed caching working and led to the writer fixes in `9c5cbc7`; the 3-lead re-check found the drafting per-lead cap too small for a retry, which another builder is fixing (the writer's output ceiling and the per-lead cap).
- Docs: README "Campaigns and leads" (with "Café places"), `deploy/README.md` "Campaigns on the server", "The Anthropic key", "Calls, screening and phone research", "Erasure".
- Found while writing the docs: `leads forget` needs the owner's `migrate.env`, so the docs give the hand step; a campaign spec has no caps key although `deploy/env.example` says a campaign may set its own.
  Since then `make deploy-data` checks and copies `campaigns/` whenever `$LEADHOUND_PRIVATE_DIR/campaigns` exists, and `make remote REMOTE_CLAUDE=1 CONFIRM=1` hands the Anthropic key to a command that calls Claude.

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
- Next, for café leads: give your call on 30 café leads under To label, then run `leads relevance eval --type cafe --decider jev` with `LEADHOUND_LIVE=1` and record the numbers under "Eval baseline".

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
- Drupal leads (sites and agencies): not measured yet; it waits for 30 of the operator's Drupal calls under To label, then `leads relevance eval --decider jev`.
- Café leads: not measured yet; it waits for 30 café calls, then `leads relevance eval --type cafe --decider jev`.
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
