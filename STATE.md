# webbership-leadhound — STATE

## Where we left off
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
- The console as the place to manage everything: a page per job, rule forms, the Leads pages (batches read and approved in full, the queue with pin, push back and skip, the priority profile, the send pause); `leads transfer` and `leads check-config`; the five leads timers.
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

## Eval baseline (jev-1.13.0, 2026-10-23)
Private golden set, n=10, 9 of 10 non-English, 3 runs, no disagreements between runs.
Exact match: title 90%, stack 80%, seniority 90%, location 90%, work_rights 100%, gate language 100%, gate workplace 1/1, overall 100%, all criteria 45/50 (90%).
Input tokens 56,106 (about $0.0024).
Early number: grow the set with 10–20 English postings before treating it as the bar.

## Current focus
Watch the first full night on the new units (the ATS score with the fixed timeout, the morning collect and brief) and the first real trim around 2026-10-31.
Hand the leads over to the operator: finding first, sending once the mailbox passes its selftest, every batch read and approved in the console.

## Slice 2 notes
- `fetch.WithPublicOnly` (`internal/fetch/guard.go`), `internal/robots` and `probe.HostRobots` exist for slice 1's CP6b to reuse.
- The leads migrations are their own set (`internal/leadstore/migrations/`, six migrations), versioned in `goose_db_version_leads`; `leadhound migrate` runs the core set, then the leads set.
- `storetest.ResetDB` keeps every goose version table.
- Live-unproven until the shadow week: real SMTP, IMAP and DNS (UIDVALIDITY rescan, Junk and Sent discovery, appending to Sent); the fakes cover them in tests.

## Next
Run now, after its deploy:
- [ ] `systemctl list-units 'leadhound-*.path'` shows four watchers waiting; press Show results now and see a `preview` run and the results page; press Fetch new jobs and see collect, score, preview in that order.

Waiting for the operator:
- [ ] The warm-contact suppressions: suppress the addresses and domains of people already known, before any finding or sending.
- [ ] Turn finding on (`LEADHOUND_LEADS_TIMERS=on`), then check the first probe and contacts runs.
- [ ] Write the mailbox settings on the server and send the selftest (`leads selftest --to ADDRESS`).
- [ ] Turn sending on (`LEADHOUND_LEADS_SEND=on` with `LEADHOUND_OUTREACH=live`) and restart the console.
- [ ] Read and approve the first batch in the console.

Known follow-ups:
- [ ] An expired job trimmed after 30 days that comes back on its board but is dropped by the title screen is reopened without a new observation: it stays open with empty text and its old verdicts, out of the score queue, while its old decisions still feed the ranking.
- [ ] `DueSites` may want an index on `site(list_rank, id)` as the site list grows.
- [ ] The app role can still `UPDATE description_clean` directly, outside the trim function.
- [ ] The leads timers have not yet run on the server.
- [ ] The connection worst case at the 15:00 trim is 24 (the afternoon collect and its score can overlap it), within the 32 limit.
- [ ] CP6e/6f/6b: remote-job sites, more sources (each checked for permission first), the employer's own apply route.

## Key files
- `CONTRIBUTING.md` (the public-repo rules)
- `Makefile`
- `cmd/leadhound`
- `internal/config`
- `internal/ai`, `internal/candidate`, `internal/scoring`, `internal/eval`
- `internal/brief`, `internal/mail`, `internal/ats`
- `internal/leadconfig`, `internal/leadstore`, `internal/probe`, `internal/contacts`, `internal/outreach`, `internal/mailbox`, `internal/leadbrief`
- `internal/console`, `internal/diskfree`
- `internal/preview`, `cmd/leadhound/runnow.go`, `cmd/leadhound/preview.go` (the Run now card)
- `deploy/` (units, `install.sh`, `sql/limits.sql`, `README.md`)

## Links
- Live URL ->
