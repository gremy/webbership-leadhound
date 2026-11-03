//go:build integration

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// nextJobsSince is before every brief these tests send, unless a test says otherwise.
var nextJobsSince = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// nextJobCard stores a job of company with no observation and no status event.
func nextJobCard(t *testing.T, q store.Queries, company, source, sourceJobID string) int64 {
	t.Helper()
	ctx := context.Background()
	cid, err := q.UpsertCompany(ctx, company, "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := q.UpsertJob(ctx, store.JobUpsert{Source: source, SourceJobID: sourceJobID, CompanyID: cid,
		Title: "Card " + sourceJobID, URL: "https://jobs.example/" + source + "/" + sourceJobID})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// nextJobObserve appends o as jobID's newest observation and returns its id.
func nextJobObserve(t *testing.T, q store.Queries, jobID int64, o store.ObservationInsert) int64 {
	t.Helper()
	ctx := context.Background()
	runID, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatal(err)
	}
	fid, err := q.InsertFetch(ctx, runID, "linkedin", fetch.Record{Kind: fetch.KindDetail, URL: "https://www.linkedin.com/x", Status: 200, FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	o.JobID, o.FetchID, o.DerivedBy = jobID, fid, "test"
	id, err := q.InsertObservation(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nextJobStatus(t *testing.T, q store.Queries, jobID int64, status string) {
	t.Helper()
	if _, err := q.InsertStatusEvent(context.Background(), jobID, status, "", "gremy", nil); err != nil {
		t.Fatal(err)
	}
}

// nextJobBrief writes the day's brief and marks it sent.
func nextJobBrief(t *testing.T, q store.Queries, day string, jobIDs ...int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := q.UpsertBrief(ctx, day, store.BriefDraft{Subject: "s", Text: "t", HTML: "h", JobIDs: jobIDs}); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkBriefSent(ctx, day, "msg-"+day); err != nil {
		t.Fatal(err)
	}
}

func nextJobIDs(jobs []store.NextJob) []int64 {
	ids := []int64{}
	for _, j := range jobs {
		ids = append(ids, j.JobID)
	}
	return ids
}

func nextJobs(t *testing.T, q store.Queries, since time.Time) []store.NextJob {
	t.Helper()
	jobs, err := q.NextJobs(context.Background(), since, false)
	if err != nil {
		t.Fatalf("NextJobs: %v", err)
	}
	return jobs
}

// TestNextJobsColdStart: no brief ever sent, or only an unsent one, is an empty result.
func TestNextJobsColdStart(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	if jobs := nextJobs(t, q, nextJobsSince); jobs == nil || len(jobs) != 0 {
		t.Fatalf("no brief = %#v, want empty and not nil", jobs)
	}
	id := nextJobCard(t, q, "Northwind", "linkedin", "1")
	if _, err := q.UpsertBrief(ctx, "2026-10-20", store.BriefDraft{Subject: "s", JobIDs: []int64{id}}); err != nil {
		t.Fatal(err)
	}
	if jobs := nextJobs(t, q, nextJobsSince); len(jobs) != 0 {
		t.Fatalf("an unsent brief lists: %+v", jobs)
	}
}

// TestNextJobFields: a job carries its latest observation's facts and its brief's number; one
// with no observation still lists with the card's title, empty fields and a non-nil Prior.
func TestNextJobFields(t *testing.T) {
	q := appStore(t).Q()
	bare := nextJobCard(t, q, "Bare Co", "linkedin", "1")
	seen := nextJobCard(t, q, "Seen Co", "linkedin", "2")
	nextJobObserve(t, q, seen, store.ObservationInsert{Title: "Old", LocationText: "Old town", ApplyType: "simple", ApplyURL: "https://old.example"})
	nextJobObserve(t, q, seen, store.ObservationInsert{Title: "Go Engineer", LocationText: "Remote", DescriptionClean: "We write Go.", ApplyType: "offsite", ApplyURL: "https://apply.example/2"})
	nextJobStatus(t, q, seen, "shortlisted")
	nextJobBrief(t, q, "2026-10-20", bare, seen)

	jobs := nextJobs(t, q, nextJobsSince)
	if !slices.Equal(nextJobIDs(jobs), []int64{bare, seen}) {
		t.Fatalf("jobs = %v, want [%d %d]", nextJobIDs(jobs), bare, seen)
	}
	b := jobs[0]
	if b.Title != "Card 1" || b.Company != "Bare Co" || b.Location != "" || b.ApplyURL != "" || b.ApplyType != "" ||
		b.Status != "new" || b.ListingStatus != store.ListingOpen || b.URL != "https://jobs.example/linkedin/1" || b.BriefN != 1 {
		t.Errorf("bare job = %+v", b)
	}
	if b.Prior == nil || len(b.Prior) != 0 {
		t.Errorf("bare job's Prior = %#v, want empty and not nil", b.Prior)
	}
	if got := b.BriefDay.Format(time.DateOnly); got != "2026-10-20" {
		t.Errorf("BriefDay = %v", b.BriefDay)
	}
	s := jobs[1]
	if s.Title != "Go Engineer" || s.Company != "Seen Co" || s.Location != "Remote" || s.ApplyURL != "https://apply.example/2" ||
		s.ApplyType != "offsite" || s.Status != "shortlisted" || s.BriefN != 2 {
		t.Errorf("seen job = %+v", s)
	}
}

// TestNextJobsOneRowNewestBrief: a job in two briefs appears once under its newest, even when
// the older brief is the one that numbers it first; a job twice in one brief keeps its first
// number.
func TestNextJobsOneRowNewestBrief(t *testing.T) {
	q := appStore(t).Q()
	both := nextJobCard(t, q, "A", "linkedin", "1")
	other := nextJobCard(t, q, "B", "linkedin", "2")
	twice := nextJobCard(t, q, "C", "linkedin", "3")
	nextJobBrief(t, q, "2026-10-20", both, other)
	nextJobBrief(t, q, "2026-10-21", twice, both, twice)

	jobs := nextJobs(t, q, nextJobsSince)
	if !slices.Equal(nextJobIDs(jobs), []int64{twice, both, other}) {
		t.Fatalf("jobs = %v, want [%d %d %d]", nextJobIDs(jobs), twice, both, other)
	}
	if d := jobs[1].BriefDay.Format(time.DateOnly); d != "2026-10-21" || jobs[1].BriefN != 2 {
		t.Errorf("job in two briefs = %s #%d, want 2026-10-21 #2", d, jobs[1].BriefN)
	}
	if jobs[0].BriefN != 1 {
		t.Errorf("job twice in one brief = #%d, want #1", jobs[0].BriefN)
	}
}

// TestNextJobsListingLeftOut: an expired, unreachable or closed-by-observation listing is left
// out, and only the latest observation decides closed. The brief's numbers do not shift.
func TestNextJobsListingLeftOut(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	expired := nextJobCard(t, q, "A", "linkedin", "1")
	unreachable := nextJobCard(t, q, "A", "linkedin", "2")
	closed := nextJobCard(t, q, "A", "linkedin", "3")
	reopened := nextJobCard(t, q, "A", "linkedin", "4")
	if err := q.SetListingStatus(ctx, expired, store.ListingExpired); err != nil {
		t.Fatal(err)
	}
	if err := q.SetListingStatus(ctx, unreachable, store.ListingUnreachable); err != nil {
		t.Fatal(err)
	}
	nextJobObserve(t, q, closed, store.ObservationInsert{Title: "open then"})
	nextJobObserve(t, q, closed, store.ObservationInsert{Title: "closed now", Closed: true})
	nextJobObserve(t, q, reopened, store.ObservationInsert{Title: "closed then", Closed: true})
	nextJobObserve(t, q, reopened, store.ObservationInsert{Title: "open now"})
	nextJobBrief(t, q, "2026-10-20", expired, unreachable, closed, reopened)

	jobs := nextJobs(t, q, nextJobsSince)
	if !slices.Equal(nextJobIDs(jobs), []int64{reopened}) {
		t.Fatalf("jobs = %v, want [%d]", nextJobIDs(jobs), reopened)
	}
	if jobs[0].BriefN != 4 {
		t.Errorf("BriefN = %d, want 4 as the brief numbered it", jobs[0].BriefN)
	}
}

// TestNextJobsStatusGrid: only a current status of new or shortlisted lists, and the latest
// event is the current one.
func TestNextJobsStatusGrid(t *testing.T) {
	q := appStore(t).Q()
	var ids, want []int64
	for i, status := range store.JobStatuses {
		id := nextJobCard(t, q, "Co "+status, "linkedin", "s"+status)
		if i%2 == 0 {
			nextJobStatus(t, q, id, "new") // an earlier event never decides
		}
		nextJobStatus(t, q, id, status)
		ids = append(ids, id)
		if status == "new" || status == "shortlisted" {
			want = append(want, id)
		}
	}
	back := nextJobCard(t, q, "Co back", "linkedin", "back")
	nextJobStatus(t, q, back, "skipped")
	nextJobStatus(t, q, back, "shortlisted")
	gone := nextJobCard(t, q, "Co gone", "linkedin", "gone")
	nextJobStatus(t, q, gone, "shortlisted")
	nextJobStatus(t, q, gone, "applied")
	nextJobBrief(t, q, "2026-10-20", append(ids, back, gone)...)

	if got := nextJobIDs(nextJobs(t, q, nextJobsSince)); !slices.Equal(got, append(want, back)) {
		t.Fatalf("jobs = %v, want %v", got, append(want, back))
	}
}

// TestNextJobsSince: a brief before since's date is left out and one on that date is kept,
// whatever the time of day; a job in both keeps the one inside the window.
func TestNextJobsSince(t *testing.T) {
	q := appStore(t).Q()
	old := nextJobCard(t, q, "A", "linkedin", "1")
	onDay := nextJobCard(t, q, "B", "linkedin", "2")
	both := nextJobCard(t, q, "C", "linkedin", "3")
	nextJobBrief(t, q, "2026-10-19", old, both)
	nextJobBrief(t, q, "2026-10-20", onDay, both)

	since := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)
	jobs := nextJobs(t, q, since)
	if !slices.Equal(nextJobIDs(jobs), []int64{onDay, both}) {
		t.Fatalf("jobs = %v, want [%d %d]", nextJobIDs(jobs), onDay, both)
	}
	// since's own zone names the date: this instant is already the 21st in Bucharest.
	east := time.FixedZone("east", 2*60*60)
	if jobs := nextJobs(t, q, since.In(east)); len(jobs) != 0 {
		t.Fatalf("since on the 21st = %v, want none", nextJobIDs(jobs))
	}
}

// TestNextJobsOrderAndNoLimit: newest brief first, then brief order, and every job comes back:
// the limit is the caller's, after the skip rules.
func TestNextJobsOrderAndNoLimit(t *testing.T) {
	q := appStore(t).Q()
	a := nextJobCard(t, q, "A", "linkedin", "1")
	b := nextJobCard(t, q, "B", "linkedin", "2")
	c := nextJobCard(t, q, "C", "linkedin", "3")
	d := nextJobCard(t, q, "D", "linkedin", "4")
	nextJobBrief(t, q, "2026-10-21", d, a)
	nextJobBrief(t, q, "2026-10-20", c, b)

	if got := nextJobIDs(nextJobs(t, q, nextJobsSince)); !slices.Equal(got, []int64{d, a, c, b}) {
		t.Fatalf("order = %v, want [%d %d %d %d]", got, d, a, c, b)
	}
}

// TestNextJobsDescriptionAndActor: the description comes only when asked for, and the latest
// status event names the actor, which reads as system when a job has none.
func TestNextJobsDescriptionAndActor(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	bare := nextJobCard(t, q, "A", "linkedin", "1")
	seen := nextJobCard(t, q, "B", "linkedin", "2")
	nextJobObserve(t, q, seen, store.ObservationInsert{Title: "Go Engineer", DescriptionClean: "We write Go."})
	if _, err := q.InsertStatusEvent(ctx, seen, "new", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	marked := nextJobCard(t, q, "C", "linkedin", "3")
	if _, err := q.InsertStatusEvent(ctx, marked, "shortlisted", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	nextJobStatus(t, q, marked, "shortlisted") // the same status, as `api mark ID maybe` writes it
	nextJobBrief(t, q, "2026-10-20", bare, seen, marked)

	jobs := nextJobs(t, q, nextJobsSince)
	if jobs[0].LatestActor != "system" || jobs[1].LatestActor != "system" || jobs[2].LatestActor != "gremy" {
		t.Errorf("actors = %q, %q, %q", jobs[0].LatestActor, jobs[1].LatestActor, jobs[2].LatestActor)
	}
	if jobs[1].Description != "" {
		t.Errorf("description read unasked: %q", jobs[1].Description)
	}
	asked, err := q.NextJobs(ctx, nextJobsSince, true)
	if err != nil {
		t.Fatal(err)
	}
	if asked[0].Description != "" || asked[1].Description != "We write Go." {
		t.Errorf("descriptions = %q, %q", asked[0].Description, asked[1].Description)
	}
}

// TestNeverBriefedJobs: who is in the read and who is not, each kept out for its own reason.
func TestNeverBriefedJobs(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	now := time.Now()
	since, firstSeen := now.AddDate(0, 0, -14), now.AddDate(0, 0, -30)
	gate := func(job, obs int64, name, verdict string) {
		t.Helper()
		if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: job, ObservationID: obs, Gate: name, Verdict: verdict, DerivedBy: "gates@1"}); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name string) int64 {
		t.Helper()
		return nextJobCard(t, q, "Co "+name, "linkedin", name)
	}

	var want []int64
	in := func(id int64) int64 { want = append(want, id); return id }

	in(mk("card")) // no observation and no status event
	ruleFailed := in(mk("rule-failed"))
	gate(ruleFailed, nextJobObserve(t, q, ruleFailed, store.ObservationInsert{Title: "a"}), "rule", "fail")
	flagged := in(mk("flagged"))
	gate(flagged, nextJobObserve(t, q, flagged, store.ObservationInsert{Title: "a"}), "location", "flag")
	shortlisted := in(mk("shortlisted"))
	nextJobStatus(t, q, shortlisted, "shortlisted")
	oldBrief := in(mk("old-brief"))
	unsent := in(mk("unsent"))

	outOfWindow := mk("first-seen-long-ago")
	ownerExec(t, "UPDATE job SET first_seen_at = $2 WHERE id = $1", outOfWindow, now.AddDate(0, 0, -31))
	otherGate := mk("other-gate")
	gate(otherGate, nextJobObserve(t, q, otherGate, store.ObservationInsert{Title: "a"}), "location", "fail")
	bothGates := in(mk("both-gates")) // the rule gate's fail is what the queue must not lose
	bothObs := nextJobObserve(t, q, bothGates, store.ObservationInsert{Title: "a"})
	gate(bothGates, bothObs, "location", "fail")
	gate(bothGates, bothObs, "rule", "fail")
	// A rescued job is read however old it is.
	agedRescue := in(mk("aged-rescue"))
	nextJobStatus(t, q, agedRescue, "new")
	ownerExec(t, "UPDATE job SET first_seen_at = $2 WHERE id = $1", agedRescue, now.AddDate(0, 0, -90))
	// A failure on an older observation decides nothing: it is back in through the newest.
	refit := in(mk("refit"))
	gate(refit, nextJobObserve(t, q, refit, store.ObservationInsert{Title: "a"}), "location", "fail")
	nextJobObserve(t, q, refit, store.ObservationInsert{Title: "b"})
	briefed := mk("briefed")
	applied := mk("applied")
	nextJobStatus(t, q, applied, "applied")
	closed := mk("closed")
	nextJobObserve(t, q, closed, store.ObservationInsert{Title: "a", Closed: true})
	expired := mk("expired")
	if err := q.SetListingStatus(ctx, expired, store.ListingExpired); err != nil {
		t.Fatal(err)
	}

	recent := now.AddDate(0, 0, -1).Format(time.DateOnly)
	nextJobBrief(t, q, recent, briefed)
	nextJobBrief(t, q, now.AddDate(0, 0, -20).Format(time.DateOnly), oldBrief)
	if _, err := q.UpsertBrief(ctx, now.AddDate(0, 0, -2).Format(time.DateOnly), store.BriefDraft{Subject: "s", JobIDs: []int64{unsent}}); err != nil {
		t.Fatal(err)
	}

	all := func(store.NextJob) bool { return true }
	jobs, err := q.NeverBriefedJobs(ctx, since, firstSeen, false, true, all)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	if !slices.Equal(nextJobIDs(jobs), want) {
		t.Fatalf("jobs = %v, want %v", nextJobIDs(jobs), want)
	}
	for _, j := range jobs {
		if !j.BriefDay.IsZero() || j.BriefN != 0 || j.Prior == nil {
			t.Errorf("job %d: day %v, n %d, prior %#v", j.JobID, j.BriefDay, j.BriefN, j.Prior)
		}
	}
	jobs, err = q.NeverBriefedJobs(ctx, since, firstSeen, false, true, func(j store.NextJob) bool { return j.JobID == flagged })
	if err != nil || !slices.Equal(nextJobIDs(jobs), []int64{flagged}) {
		t.Errorf("keep one: %v, %v", nextJobIDs(jobs), err)
	}
	empty, err := q.NeverBriefedJobs(ctx, since, firstSeen, false, true, func(store.NextJob) bool { return false })
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("keep none = %#v, %v; want empty and not nil", empty, err)
	}
	// With no live rule only a job with a rule-gate fail is read, and only that fail's observation
	// fills the reason. refit's older fail is another gate's, so it is not one of them.
	stored, err := q.NeverBriefedJobs(ctx, since, firstSeen, false, false, all)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{ruleFailed, bothGates}; !slices.Equal(nextJobIDs(stored), want) {
		t.Errorf("no live rule: %v, want %v", nextJobIDs(stored), want)
	}
	for _, j := range stored {
		if j.StoredRuleReason == "" {
			t.Errorf("job %d has no stored reason", j.JobID)
		}
	}
}

// TestNeverBriefedJobsStoredReason: the rule's reason first, then the gate's; only the current
// observation counts, and a flag is not a fail.
func TestNeverBriefedJobsStoredReason(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	rule, err := q.AddRule(ctx, "skip", "title", "noise", "the rule's reason", nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := func(job, obs int64, verdict, reason string, ruleID *int64) {
		t.Helper()
		if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: job, ObservationID: obs, Gate: "rule", Verdict: verdict, Reason: reason, RuleID: ruleID, DerivedBy: "gates@1"}); err != nil {
			t.Fatal(err)
		}
	}
	byRule := nextJobCard(t, q, "A", "linkedin", "1")
	gate(byRule, nextJobObserve(t, q, byRule, store.ObservationInsert{Title: "a"}), "fail", "the gate's words", &rule)
	noRule := nextJobCard(t, q, "B", "linkedin", "2")
	gate(noRule, nextJobObserve(t, q, noRule, store.ObservationInsert{Title: "a"}), "fail", "the gate's words", nil)
	replaced := nextJobCard(t, q, "C", "linkedin", "3")
	gate(replaced, nextJobObserve(t, q, replaced, store.ObservationInsert{Title: "a"}), "fail", "old", &rule)
	nextJobObserve(t, q, replaced, store.ObservationInsert{Title: "b"})
	flagged := nextJobCard(t, q, "D", "linkedin", "4")
	gate(flagged, nextJobObserve(t, q, flagged, store.ObservationInsert{Title: "a"}), "flag", "only a flag", &rule)

	jobs, err := q.NeverBriefedJobs(ctx, now, now.AddDate(0, 0, -30), false, true, func(store.NextJob) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{byRule: "the rule's reason", noRule: "the gate's words", replaced: "", flagged: ""}
	if len(jobs) != len(want) {
		t.Fatalf("jobs = %v", nextJobIDs(jobs))
	}
	for _, j := range jobs {
		if j.StoredRuleReason != want[j.JobID] {
			t.Errorf("job %d: reason %q, want %q", j.JobID, j.StoredRuleReason, want[j.JobID])
		}
	}
}

// TestNeverBriefedJobsPriorAndDescription: Prior is read for the company of a job whose latest
// actor is not system and no other, and the description comes only when asked for.
func TestNeverBriefedJobsPriorAndDescription(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	rescued := nextJobCard(t, q, "Northwind", "linkedin", "1")
	nextJobObserve(t, q, rescued, store.ObservationInsert{Title: "Go Engineer", DescriptionClean: "We write Go."})
	nextJobStatus(t, q, rescued, "new")
	system := nextJobCard(t, q, "Elsewhere", "linkedin", "2")
	if _, err := q.InsertStatusEvent(ctx, system, "new", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	applied := nextJobCard(t, q, "Northwind", "linkedin", "3")
	nextJobStatus(t, q, applied, "applied")
	elsewhere := nextJobCard(t, q, "Elsewhere", "linkedin", "4")
	nextJobStatus(t, q, elsewhere, "applied")

	read := func(desc bool) []store.NextJob {
		t.Helper()
		jobs, err := q.NeverBriefedJobs(ctx, now, now.AddDate(0, 0, -30), desc, true, func(store.NextJob) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(nextJobIDs(jobs), []int64{rescued, system}) {
			t.Fatalf("jobs = %v", nextJobIDs(jobs))
		}
		return jobs
	}
	jobs := read(false)
	if len(jobs[0].Prior) != 1 || jobs[0].Prior[0].JobID != applied || jobs[0].LatestActor != "gremy" {
		t.Errorf("rescued job = %+v", jobs[0])
	}
	if len(jobs[1].Prior) != 0 || jobs[1].LatestActor != "system" {
		t.Errorf("system job = %+v", jobs[1])
	}
	if jobs[0].Description != "" {
		t.Errorf("description read unasked: %q", jobs[0].Description)
	}
	if jobs := read(true); jobs[0].Description != "We write Go." {
		t.Errorf("description = %q", jobs[0].Description)
	}
}

// TestNextJobsPrior: Prior is every other job of the same company past new, from any source,
// whatever its listing and whether or not a brief named it; never the job itself, a job still
// new, or another company's. Each listed job gets its own Prior, latest change first.
func TestNextJobsPrior(t *testing.T) {
	q := appStore(t).Q()
	job := nextJobCard(t, q, "Northwind", "linkedin", "1")
	listed := nextJobCard(t, q, "Northwind", "linkedin", "2")
	nextJobStatus(t, q, listed, "shortlisted")
	applied := nextJobCard(t, q, "Northwind", "greenhouse", "1") // one posting, another source
	nextJobObserve(t, q, applied, store.ObservationInsert{Title: "Go Engineer (ATS)"})
	nextJobStatus(t, q, applied, "new")
	nextJobStatus(t, q, applied, "applied")
	skipped := nextJobCard(t, q, "Northwind", "lever", "1")
	nextJobStatus(t, q, skipped, "skipped")
	if err := q.SetListingStatus(context.Background(), skipped, store.ListingExpired); err != nil {
		t.Fatal(err)
	}
	backToNew := nextJobCard(t, q, "Northwind", "ashby", "1")
	nextJobStatus(t, q, backToNew, "skipped")
	nextJobStatus(t, q, backToNew, "new")
	nextJobCard(t, q, "Northwind", "ashby", "2") // no event: new
	stranger := nextJobCard(t, q, "Elsewhere", "linkedin", "9")
	nextJobStatus(t, q, stranger, "applied")

	t1 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	t3 := t2.Add(24 * time.Hour)
	ownerExec(t, "UPDATE status_event SET at = $2 WHERE job_id = $1 AND status = 'shortlisted'", listed, t1)
	ownerExec(t, "UPDATE status_event SET at = $2 WHERE job_id = $1 AND status = 'skipped'", skipped, t3)
	ownerExec(t, "UPDATE status_event SET at = $2 WHERE job_id = $1 AND status = 'new'", applied, t1)
	ownerExec(t, "UPDATE status_event SET at = $2 WHERE job_id = $1 AND status = 'applied'", applied, t2)
	nextJobBrief(t, q, "2026-10-20", job, listed)

	jobs := nextJobs(t, q, nextJobsSince)
	if !slices.Equal(nextJobIDs(jobs), []int64{job, listed}) {
		t.Fatalf("jobs = %v, want [%d %d]", nextJobIDs(jobs), job, listed)
	}
	want := []store.PriorJob{
		{JobID: skipped, Title: "Card 1", Status: "skipped", Since: t3},
		{JobID: applied, Title: "Go Engineer (ATS)", Status: "applied", Since: t2},
		{JobID: listed, Title: "Card 2", Status: "shortlisted", Since: t1},
	}
	if !priorEqual(jobs[0].Prior, want) {
		t.Errorf("Prior = %+v, want %+v", jobs[0].Prior, want)
	}
	// The listed job is the other side of the same pair: it never names itself.
	if !priorEqual(jobs[1].Prior, want[:2]) {
		t.Errorf("listed job's Prior = %+v, want %+v", jobs[1].Prior, want[:2])
	}
}

func priorEqual(a, b []store.PriorJob) bool {
	return slices.EqualFunc(a, b, func(x, y store.PriorJob) bool {
		return x.JobID == y.JobID && x.Title == y.Title && x.Status == y.Status && x.Since.Equal(y.Since)
	})
}
