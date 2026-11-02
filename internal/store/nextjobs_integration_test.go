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

// nextJobObserve appends o as jobID's newest observation.
func nextJobObserve(t *testing.T, q store.Queries, jobID int64, o store.ObservationInsert) {
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
	if _, err := q.InsertObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
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

func nextJobs(t *testing.T, q store.Queries, since time.Time, limit int) []store.NextJob {
	t.Helper()
	jobs, err := q.NextJobs(context.Background(), since, limit)
	if err != nil {
		t.Fatalf("NextJobs: %v", err)
	}
	return jobs
}

// TestNextJobsColdStart: no brief ever sent, or only an unsent one, is an empty result.
func TestNextJobsColdStart(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	if jobs := nextJobs(t, q, nextJobsSince, 20); jobs == nil || len(jobs) != 0 {
		t.Fatalf("no brief = %#v, want empty and not nil", jobs)
	}
	id := nextJobCard(t, q, "Northwind", "linkedin", "1")
	if _, err := q.UpsertBrief(ctx, "2026-10-20", store.BriefDraft{Subject: "s", JobIDs: []int64{id}}); err != nil {
		t.Fatal(err)
	}
	if jobs := nextJobs(t, q, nextJobsSince, 20); len(jobs) != 0 {
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

	jobs := nextJobs(t, q, nextJobsSince, 20)
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

	jobs := nextJobs(t, q, nextJobsSince, 20)
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

	jobs := nextJobs(t, q, nextJobsSince, 20)
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

	if got := nextJobIDs(nextJobs(t, q, nextJobsSince, 50)); !slices.Equal(got, append(want, back)) {
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
	jobs := nextJobs(t, q, since, 20)
	if !slices.Equal(nextJobIDs(jobs), []int64{onDay, both}) {
		t.Fatalf("jobs = %v, want [%d %d]", nextJobIDs(jobs), onDay, both)
	}
	// since's own zone names the date: this instant is already the 21st in Bucharest.
	east := time.FixedZone("east", 2*60*60)
	if jobs := nextJobs(t, q, since.In(east), 20); len(jobs) != 0 {
		t.Fatalf("since on the 21st = %v, want none", nextJobIDs(jobs))
	}
}

// TestNextJobsLimitAndOrder: newest brief first, then brief order, and limit keeps the head.
func TestNextJobsLimitAndOrder(t *testing.T) {
	q := appStore(t).Q()
	a := nextJobCard(t, q, "A", "linkedin", "1")
	b := nextJobCard(t, q, "B", "linkedin", "2")
	c := nextJobCard(t, q, "C", "linkedin", "3")
	d := nextJobCard(t, q, "D", "linkedin", "4")
	nextJobBrief(t, q, "2026-10-21", d, a)
	nextJobBrief(t, q, "2026-10-20", c, b)

	if got := nextJobIDs(nextJobs(t, q, nextJobsSince, 20)); !slices.Equal(got, []int64{d, a, c, b}) {
		t.Fatalf("order = %v, want [%d %d %d %d]", got, d, a, c, b)
	}
	if got := nextJobIDs(nextJobs(t, q, nextJobsSince, 3)); !slices.Equal(got, []int64{d, a, c}) {
		t.Fatalf("limit 3 = %v, want [%d %d %d]", got, d, a, c)
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

	jobs := nextJobs(t, q, nextJobsSince, 20)
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
