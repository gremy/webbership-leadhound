# webbership-leadhound — STATE

## Where we left off
CP1–CP5 done and CP6 built: foundation and privacy checks, LinkedIn guest collector, posting sanitiser and code gates, Jev scoring, ranking and eval, the morning brief with recheck and tracker, and the Greenhouse, Lever and Ashby board collector (`collect ats`, `boards verify`).
The private board list is waiting on its first live verify.

## Eval baseline (jev-1.13.0, 2026-10-23)
Private golden set, n=10, 9 of 10 non-English, 3 runs, no disagreements between runs.
Exact match: title 90%, stack 80%, seniority 90%, location 90%, work_rights 100%, gate language 100%, gate workplace 1/1, overall 100%, all criteria 45/50 (90%).
Input tokens 56,106 (about $0.0024).
Early number: grow the set with 10–20 English postings before treating it as the bar.

## Current focus
Slice 1 (jobs).

## Next (1-3)
- [ ] CP6: verify the private board list live (`LEADHOUND_LIVE=ats leadhound boards verify --boards …`).
- [ ] CP6b: the employer's own apply route.
- [ ] CP7: deploy on timers.

## Key files
- `CONTRIBUTING.md` (the public-repo rules)
- `Makefile`
- `cmd/leadhound`
- `internal/config`
- `internal/ai`, `internal/candidate`, `internal/scoring`, `internal/eval`
- `internal/brief`, `internal/mail`, `internal/ats`

## Links
- Live URL ->
