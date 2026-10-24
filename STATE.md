# webbership-leadhound — STATE

## Where we left off
CP1–CP6 and CP6c done: foundation and privacy checks, LinkedIn guest collector, posting sanitiser and code gates, Jev scoring, ranking and eval, the morning brief with recheck and tracker, the Greenhouse, Lever and Ashby board collector, and the wide net: a board registry (`boards import|list|check|harvest`), a title screen before storage, and the posting's location field read as a requirement (a field naming only a place, with nothing saying remote, is an office job).
Measured on a random 300-board sample: about 3.6 minutes to collect, roughly 6,200 postings stored, under a quarter passing the gates, and about 1,600 scorer calls (2.1M input tokens, under $0.10) to score them all.

## Eval baseline (jev-1.13.0, 2026-10-23)
Private golden set, n=10, 9 of 10 non-English, 3 runs, no disagreements between runs.
Exact match: title 90%, stack 80%, seniority 90%, location 90%, work_rights 100%, gate language 100%, gate workplace 1/1, overall 100%, all criteria 45/50 (90%).
Input tokens 56,106 (about $0.0024).
Early number: grow the set with 10–20 English postings before treating it as the bar.

## Current focus
Slice 1 (jobs).
Slice 2 (Drupal leads) is built to CP6 on its task branch: site import and probe, agencies and contact routes, leads, batches and suppression, the sender with reply sync and the sending-domain check, and the leads block in the brief.

## Slice 2 notes
- `fetch.WithPublicOnly` (`internal/fetch/guard.go`), `internal/robots` and `probe.HostRobots` exist for slice 1's CP6b to reuse.
- The leads migrations are their own set (`internal/leadstore/migrations/`, three migrations), versioned in `goose_db_version_leads`; `leadhound migrate` runs the core set, then the leads set.
- `storetest.ResetDB` keeps every goose version table.
- Live-unproven until the shadow week: real SMTP, IMAP and DNS (UIDVALIDITY rescan, Junk and Sent discovery, appending to Sent); the fakes cover them in tests.

## Next (1-3)
- [x] CP6d: SmartRecruiters, Teamtailor and Personio (paged reads, SmartRecruiters details after the list-data gates). Recruitee and Workable left out: no documented public feed without a key.
- [ ] CP7: deploy on timers, with the leads units beside the jobs units.
- [ ] CP6e/6f/6b: remote-job sites, more sources (each checked for permission first), the employer's own apply route.

## Key files
- `CONTRIBUTING.md` (the public-repo rules)
- `Makefile`
- `cmd/leadhound`
- `internal/config`
- `internal/ai`, `internal/candidate`, `internal/scoring`, `internal/eval`
- `internal/brief`, `internal/mail`, `internal/ats`
- `internal/leadconfig`, `internal/leadstore`, `internal/probe`, `internal/contacts`, `internal/outreach`, `internal/mailbox`, `internal/leadbrief`

## Links
- Live URL ->
