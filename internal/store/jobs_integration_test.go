//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func TestUpsertCompany(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	id, err := q.UpsertCompany(ctx, "Acme Widgets, Inc.", "https://acme.example/", "")
	if err != nil {
		t.Fatalf("UpsertCompany: %v", err)
	}
	// The same company under another spelling is the same row, and it is idempotent.
	for _, name := range []string{"Acme Widgets, Inc.", "  ACME widgets inc ", "Acme-Widgets Inc"} {
		again, err := q.UpsertCompany(ctx, name, "", "")
		if err != nil {
			t.Fatalf("UpsertCompany %q: %v", name, err)
		}
		if again != id {
			t.Fatalf("UpsertCompany %q = %d, want %d", name, again, id)
		}
	}

	// The slug is set when first known, and never replaced after that.
	if _, err := q.UpsertCompany(ctx, "Acme Widgets Inc", "", "acme-widgets"); err != nil {
		t.Fatalf("UpsertCompany with slug: %v", err)
	}
	if _, err := q.UpsertCompany(ctx, "Acme Widgets Inc", "", "other-slug"); err != nil {
		t.Fatalf("UpsertCompany with another slug: %v", err)
	}
	if _, err := q.UpsertCompany(ctx, "Acme Widgets Inc", "", ""); err != nil {
		t.Fatalf("UpsertCompany without slug: %v", err)
	}
	var name, norm, src, slug string
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT name, norm_name, source_url, linkedin_slug FROM company WHERE id = $1`, id).
		Scan(&name, &norm, &src, &slug); err != nil {
		t.Fatalf("read company: %v", err)
	}
	if name != "Acme Widgets, Inc." || norm != "acme widgets inc" || src != "https://acme.example/" || slug != "acme-widgets" {
		t.Fatalf("company = %q %q %q %q", name, norm, src, slug)
	}

	other, err := q.UpsertCompany(ctx, "Globex", "", "")
	if err != nil || other == id {
		t.Fatalf("UpsertCompany Globex = %d, %v; want a new id", other, err)
	}
	if _, err := q.UpsertCompany(ctx, " -- ", "", ""); !errors.Is(err, store.ErrEmptyName) {
		t.Fatalf("UpsertCompany punctuation only = %v, want ErrEmptyName", err)
	}
}

// seedJob creates a run, a fetch, a company and a job, returning the job id and fetch id.
func seedJob(t *testing.T, q store.Queries, sourceJobID string) (jobID, fetchID int64) {
	t.Helper()
	ctx := context.Background()
	runID, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	rec := store.NewFetchRecorder(q, runID, "linkedin")
	if err := rec.Record(ctx, fetch.Record{Kind: fetch.KindDetail, URL: "https://jobs.example/" + sourceJobID, Status: 200, Body: []byte("x")}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	companyID, err := q.UpsertCompany(ctx, "Initech", "", "")
	if err != nil {
		t.Fatalf("UpsertCompany: %v", err)
	}
	jobID, err = q.UpsertJob(ctx, store.JobUpsert{
		Source: "linkedin", SourceJobID: sourceJobID, CompanyID: companyID,
		Title: "Go Engineer", URL: "https://jobs.example/" + sourceJobID,
	})
	if err != nil {
		t.Fatalf("UpsertJob: %v", err)
	}
	return jobID, rec.LastID()
}

func TestUpsertJobFirstSeenWins(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	c1, err := q.UpsertCompany(ctx, "Initech", "", "")
	if err != nil {
		t.Fatalf("UpsertCompany: %v", err)
	}
	c2, err := q.UpsertCompany(ctx, "Globex", "", "")
	if err != nil {
		t.Fatalf("UpsertCompany: %v", err)
	}

	id, err := q.UpsertJob(ctx, store.JobUpsert{Source: "linkedin", SourceJobID: "1000000001", CompanyID: c1, Title: "Go Engineer", URL: "https://jobs.example/a"})
	if err != nil {
		t.Fatalf("UpsertJob: %v", err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ownerExec(t, "UPDATE job SET last_seen_at = $2, first_seen_at = $2 WHERE id = $1", id, old)

	again, err := q.UpsertJob(ctx, store.JobUpsert{Source: "linkedin", SourceJobID: "1000000001", CompanyID: c2, Title: "Renamed", URL: "https://jobs.example/b"})
	if err != nil {
		t.Fatalf("UpsertJob again: %v", err)
	}
	if again != id {
		t.Fatalf("UpsertJob again = %d, want %d", again, id)
	}
	job, err := q.JobBySourceID(ctx, "linkedin", "1000000001")
	if err != nil {
		t.Fatalf("JobBySourceID: %v", err)
	}
	if job.Title != "Go Engineer" || job.URL != "https://jobs.example/a" || job.CompanyID != c1 {
		t.Fatalf("job = %+v, want the first-seen title, url and company", job)
	}
	if !job.FirstSeenAt.Equal(old) || !job.LastSeenAt.After(old) {
		t.Fatalf("first_seen_at = %v, last_seen_at = %v; want first kept, last bumped", job.FirstSeenAt, job.LastSeenAt)
	}
	if job.ListingStatus != "open" {
		t.Fatalf("listing_status = %q, want open", job.ListingStatus)
	}

	// The same source job id under another source is another job.
	other, err := q.UpsertJob(ctx, store.JobUpsert{Source: "greenhouse", SourceJobID: "1000000001", CompanyID: c1, Title: "Go Engineer", URL: "https://jobs.example/c"})
	if err != nil || other == id {
		t.Fatalf("UpsertJob other source = %d, %v; want a new id", other, err)
	}
	if _, err := q.JobBySourceID(ctx, "linkedin", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("JobBySourceID missing = %v, want ErrNotFound", err)
	}
}

func TestJobsWithoutObservation(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	observed, fetchID := seedJob(t, q, "1000000001")
	seedJob(t, q, "1000000002")
	if _, err := q.InsertObservation(ctx, store.ObservationInsert{
		JobID: observed, FetchID: fetchID, DerivedBy: "test-parser@1", Title: "Go Engineer",
	}); err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}

	// 1000000002 has a job row and no observation; 1000000003 has no row at all. Both are new.
	got, err := q.JobsWithoutObservation(ctx, "linkedin", []string{"1000000003", "1000000001", "1000000002", "1000000003"})
	if err != nil {
		t.Fatalf("JobsWithoutObservation: %v", err)
	}
	if want := []string{"1000000003", "1000000002"}; !slices.Equal(got, want) {
		t.Fatalf("JobsWithoutObservation = %v, want %v (input order, once each)", got, want)
	}
	// An observation under another source does not count.
	got, err = q.JobsWithoutObservation(ctx, "greenhouse", []string{"1000000001"})
	if err != nil || !slices.Equal(got, []string{"1000000001"}) {
		t.Fatalf("JobsWithoutObservation other source = %v, %v", got, err)
	}
	got, err = q.JobsWithoutObservation(ctx, "linkedin", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("JobsWithoutObservation empty = %v, %v", got, err)
	}
}

func TestObservationRoundTripAndLatest(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	jobID, fetchID := seedJob(t, q, "1000000001")

	if _, err := q.LatestObservation(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestObservation before any = %v, want ErrNotFound", err)
	}

	count := 25
	posted := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	full := store.ObservationInsert{
		JobID: jobID, FetchID: fetchID, DerivedBy: "test-parser@1",
		Title: "Senior Go Engineer", LocationText: "Remote", DescriptionClean: "Build things in Go.",
		GuardFindings: json.RawMessage(`{"removed": 2}`), Lang: "en", LangConfidence: 0.97,
		Seniority: "Mid-Senior level", EmploymentType: "Full-time", Industries: "Software",
		ApplicantCount: &count, ApplicantText: "Be among the first 25 applicants",
		PostedText: "3 days ago", PostedAtEst: &posted, ApplyType: "offsite",
		ApplyURL: "https://careers.example/apply", Closed: false,
	}
	firstID, err := q.InsertObservation(ctx, full)
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}
	got, err := q.LatestObservation(ctx, jobID)
	if err != nil {
		t.Fatalf("LatestObservation: %v", err)
	}
	if got.ID != firstID || got.JobID != jobID || got.FetchID != fetchID || got.DerivedBy != "test-parser@1" ||
		got.Title != full.Title || got.LocationText != "Remote" || got.DescriptionClean != full.DescriptionClean ||
		got.Lang != "en" || got.LangConfidence < 0.969 || got.LangConfidence > 0.971 ||
		got.Seniority != full.Seniority || got.EmploymentType != "Full-time" || got.Industries != "Software" ||
		got.ApplicantCount == nil || *got.ApplicantCount != 25 || got.ApplicantText != full.ApplicantText ||
		got.PostedText != "3 days ago" || got.PostedAtEst == nil || !got.PostedAtEst.Equal(posted) ||
		got.ApplyType != "offsite" || got.ApplyURL != full.ApplyURL || got.Closed {
		t.Fatalf("observation = %+v", got)
	}
	var findings map[string]int
	if err := json.Unmarshal(got.GuardFindings, &findings); err != nil || findings["removed"] != 2 {
		t.Fatalf("guard_findings = %s (%v)", got.GuardFindings, err)
	}

	// Minimal insert: guard_findings defaults to {}, applicant count and posted time stay NULL.
	// Two inserts in one transaction share now(), so the tie goes to the higher id.
	var aID, bID int64
	if err := s.InTx(ctx, func(tx store.Queries) error {
		var err error
		if aID, err = tx.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test-parser@1", Title: "A"}); err != nil {
			return err
		}
		bID, err = tx.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test-parser@1", Title: "B", Closed: true})
		return err
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	got, err = q.LatestObservation(ctx, jobID)
	if err != nil {
		t.Fatalf("LatestObservation: %v", err)
	}
	if got.ID != bID || got.Title != "B" || !got.Closed || got.ApplicantCount != nil || got.PostedAtEst != nil || string(got.GuardFindings) != "{}" {
		t.Fatalf("tie-break latest = %+v (a=%d b=%d)", got, aID, bID)
	}

	// Latest is by time first: an older id observed later wins.
	ownerExec(t, "UPDATE job_observation SET observed_at = now() + interval '1 hour' WHERE id = $1", firstID)
	got, err = q.LatestObservation(ctx, jobID)
	if err != nil || got.ID != firstID {
		t.Fatalf("LatestObservation by time = %d, %v; want %d", got.ID, err, firstID)
	}

	// A failing transaction leaves nothing behind.
	boom := errors.New("boom")
	if err := s.InTx(ctx, func(tx store.Queries) error {
		if _, err := tx.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "x", Title: "rolled back"}); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("InTx error = %v, want boom", err)
	}
	var n int
	if err := storetest.OwnerConn(t).QueryRow(ctx, "SELECT count(*) FROM job_observation WHERE title = 'rolled back'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rolled-back rows = %d, %v; want 0", n, err)
	}

	// The CHECK on apply_type holds.
	if _, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "x", Title: "x", ApplyType: "bogus"}); err == nil {
		t.Fatal("InsertObservation with an unknown apply_type succeeded")
	}
}

func TestStatusEventsAndLatest(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	jobID, _ := seedJob(t, q, "1000000001")

	if _, err := q.LatestStatus(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestStatus before any = %v, want ErrNotFound", err)
	}
	newID, err := q.InsertStatusEvent(ctx, jobID, "new", "", "system", nil)
	if err != nil {
		t.Fatalf("InsertStatusEvent: %v", err)
	}
	got, err := q.LatestStatus(ctx, jobID)
	if err != nil || got.ID != newID || got.Status != "new" || got.Actor != "system" || got.RuleID != nil || got.JobID != jobID {
		t.Fatalf("LatestStatus = %+v, %v", got, err)
	}

	// Same instant: the higher id wins.
	var shortID int64
	if err := s.InTx(ctx, func(tx store.Queries) error {
		if _, err := tx.InsertStatusEvent(ctx, jobID, "skipped", "first", "gremy", nil); err != nil {
			return err
		}
		var err error
		shortID, err = tx.InsertStatusEvent(ctx, jobID, "shortlisted", "looks good", "gremy", nil)
		return err
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	got, err = q.LatestStatus(ctx, jobID)
	if err != nil || got.ID != shortID || got.Status != "shortlisted" || got.Reason != "looks good" || got.Actor != "gremy" {
		t.Fatalf("LatestStatus tie-break = %+v, %v; want %d", got, err, shortID)
	}

	// By time first.
	ownerExec(t, "UPDATE status_event SET at = now() + interval '1 hour' WHERE id = $1", newID)
	got, err = q.LatestStatus(ctx, jobID)
	if err != nil || got.ID != newID {
		t.Fatalf("LatestStatus by time = %+v, %v; want %d", got, err, newID)
	}

	// A rule id is kept; an unknown status or actor is refused.
	var ruleID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`INSERT INTO rule (kind, field, pattern, reason) VALUES ('skip', 'title', 'intern', 'test') RETURNING id`).Scan(&ruleID); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	ruled, err := q.InsertStatusEvent(ctx, jobID, "skipped", "rule", "system", &ruleID)
	if err != nil {
		t.Fatalf("InsertStatusEvent with rule: %v", err)
	}
	var stored *int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, "SELECT rule_id FROM status_event WHERE id = $1", ruled).Scan(&stored); err != nil || stored == nil || *stored != ruleID {
		t.Fatalf("rule_id = %v, %v; want %d", stored, err, ruleID)
	}
	if _, err := q.InsertStatusEvent(ctx, jobID, "bogus", "", "system", nil); err == nil {
		t.Fatal("InsertStatusEvent with an unknown status succeeded")
	}
	if _, err := q.InsertStatusEvent(ctx, jobID, "new", "", "someone", nil); err == nil {
		t.Fatal("InsertStatusEvent with an unknown actor succeeded")
	}
}
