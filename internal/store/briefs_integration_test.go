//go:build integration

package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// TestBriefLogLifecycle: a brief is written unsent; while a try may have delivered it is
// never rewritten; after a refused only try it is rewritten, list and key included; it is
// marked sent once and then refused for good.
func TestBriefLogLifecycle(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	const day = "2026-10-23"

	if _, err := q.LatestSentBrief(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestSentBrief on empty = %v, want ErrNotFound", err)
	}
	if _, err := q.BeginBriefSend(ctx, day); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("BeginBriefSend with no row = %v, want ErrNotFound", err)
	}
	b, err := q.UpsertBrief(ctx, day, store.BriefDraft{Subject: "first", Text: "body 1", HTML: "<p>1</p>"})
	if err != nil {
		t.Fatalf("UpsertBrief: %v", err)
	}
	if b.Day != day || b.SentAt != nil || len(b.JobIDs) != 0 || b.BodyHTML != "<p>1</p>" || b.SendState != store.BriefPending || b.Key() != "brief-"+day {
		t.Fatalf("brief = %+v", b)
	}
	// Untried, the row is still never rewritten: a crashed try may have delivered it.
	if _, err := q.UpsertBrief(ctx, day, store.BriefDraft{Subject: "x"}); !errors.Is(err, store.ErrBriefPending) {
		t.Fatalf("UpsertBrief over a pending row = %v, want ErrBriefPending", err)
	}
	prev, err := q.BeginBriefSend(ctx, day)
	if err != nil || prev != store.BriefPending {
		t.Fatalf("BeginBriefSend = %q, %v", prev, err)
	}
	if prev, err := q.BeginBriefSend(ctx, day); err != nil || prev != store.BriefMaybeSent {
		t.Fatalf("second BeginBriefSend = %q, %v, want maybe_sent", prev, err)
	}
	if _, err := q.UpsertBrief(ctx, day, store.BriefDraft{Subject: "x"}); !errors.Is(err, store.ErrBriefPending) {
		t.Fatalf("UpsertBrief over a maybe-sent row = %v, want ErrBriefPending", err)
	}
	// A refused only try: the day may be written afresh under a new key.
	if err := q.MarkBriefRefused(ctx, day); err != nil {
		t.Fatal(err)
	}
	b2, err := q.UpsertBrief(ctx, day, store.BriefDraft{Subject: "second", Text: "body 2", HTML: "<p>2</p>", Key: "brief-" + day + "-r9", JobIDs: []int64{3, 1}})
	if err != nil {
		t.Fatalf("UpsertBrief after a refusal: %v", err)
	}
	if b2.ID != b.ID || b2.Subject != "second" || !slices.Equal(b2.JobIDs, []int64{3, 1}) || b2.Key() != "brief-"+day+"-r9" || b2.SendState != store.BriefPending {
		t.Fatalf("rewritten brief = %+v", b2)
	}
	if _, err := q.LatestSentBrief(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unsent brief counts as sent: %v", err)
	}

	if err := q.MarkBriefSent(ctx, day, "msg-1"); err != nil {
		t.Fatalf("MarkBriefSent: %v", err)
	}
	if err := q.MarkBriefSent(ctx, day, "msg-2"); !errors.Is(err, store.ErrBriefSent) {
		t.Fatalf("second MarkBriefSent = %v, want ErrBriefSent", err)
	}
	if err := q.MarkBriefSent(ctx, "2026-10-24", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("MarkBriefSent on a day with no row = %v, want ErrNotFound", err)
	}
	if _, err := q.UpsertBrief(ctx, day, store.BriefDraft{Subject: "third"}); !errors.Is(err, store.ErrBriefSent) {
		t.Fatalf("UpsertBrief after the send = %v, want ErrBriefSent", err)
	}
	if _, err := q.BeginBriefSend(ctx, day); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("BeginBriefSend on a sent day = %v, want ErrNotFound", err)
	}
	if err := q.MarkBriefRefused(ctx, day); err != nil {
		t.Fatal(err)
	}
	got, err := q.BriefByDay(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if got.SentAt == nil || got.ProviderID != "msg-1" || got.Subject != "second" || got.BodyHTML != "<p>2</p>" || !slices.Equal(got.JobIDs, []int64{3, 1}) || got.SendState == store.BriefRefused {
		t.Fatalf("sent brief = %+v", got)
	}
	latest, err := q.LatestSentBrief(ctx)
	if err != nil || latest.ID != b.ID {
		t.Fatalf("LatestSentBrief = %+v, %v", latest, err)
	}
	if _, err := q.BriefByDay(ctx, "2026-10-01"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("BriefByDay with no row = %v", err)
	}
}

// seedBriefJob writes a company, a job and one observation linked to a fetch made at fetchedAt.
func seedBriefJob(t *testing.T, q store.Queries, sourceID string, fetchedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	cid, err := q.UpsertCompany(ctx, "Northwind Labs", "", "")
	if err != nil {
		t.Fatal(err)
	}
	jid, err := q.UpsertJob(ctx, store.JobUpsert{Source: "linkedin", SourceJobID: sourceID, CompanyID: cid, Title: "Go Engineer", URL: "https://www.linkedin.com/jobs/view/" + sourceID})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatal(err)
	}
	fid, err := q.InsertFetch(ctx, runID, "linkedin", fetch.Record{Kind: fetch.KindDetail, URL: "https://www.linkedin.com/x/" + sourceID, Status: 200, FetchedAt: fetchedAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: jid, FetchID: fid, DerivedBy: "test", Title: "Go Engineer " + sourceID}); err != nil {
		t.Fatal(err)
	}
	return jid
}

func TestRecheckTargetsAndListingStatus(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	stale := seedBriefJob(t, q, "1", now.Add(-25*time.Hour))
	fresh := seedBriefJob(t, q, "2", now.Add(-time.Hour))
	expired := seedBriefJob(t, q, "3", now.Add(-48*time.Hour))
	unreachable := seedBriefJob(t, q, "4", now.Add(-time.Hour))
	if err := q.SetListingStatus(ctx, expired, store.ListingExpired); err != nil {
		t.Fatal(err)
	}
	if err := q.SetListingStatus(ctx, unreachable, store.ListingUnreachable); err != nil {
		t.Fatal(err)
	}
	if err := q.SetListingStatus(ctx, 999, store.ListingOpen); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown job = %v", err)
	}
	if err := q.SetListingStatus(ctx, stale, "gone"); !errors.Is(err, store.ErrBadStatus) {
		t.Fatalf("bad status = %v", err)
	}

	// Order follows the ids asked about; the expired and the fresh one are not due.
	targets, err := q.RecheckTargets(ctx, "linkedin", []int64{unreachable, fresh, expired, stale}, now.Add(-20*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, tg := range targets {
		ids = append(ids, tg.JobID)
	}
	if !slices.Equal(ids, []int64{unreachable, stale}) {
		t.Fatalf("targets = %v, want [%d %d]", ids, unreachable, stale)
	}
	if targets[1].SourceJobID != "1" || targets[1].Company != "Northwind Labs" || targets[0].ListingStatus != store.ListingUnreachable {
		t.Fatalf("target fields = %+v", targets)
	}
	// Another source's jobs are never targets.
	if other, err := q.RecheckTargets(ctx, "linkedin-fixture", []int64{stale}, now); err != nil || len(other) != 0 {
		t.Fatalf("other source = %v, %v", other, err)
	}
	// A check stamps status_checked_at, so the stale job is no longer due once it is open.
	if err := q.SetListingStatus(ctx, stale, store.ListingOpen); err != nil {
		t.Fatal(err)
	}
	if again, err := q.RecheckTargets(ctx, "linkedin", []int64{stale}, now.Add(-20*time.Hour)); err != nil || len(again) != 0 {
		t.Fatalf("checked job still due: %v, %v", again, err)
	}
}

func TestStatusHelpers(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	a := seedBriefJob(t, q, "1", now)
	b := seedBriefJob(t, q, "2", now)
	c := seedBriefJob(t, q, "3", now)

	if s, err := q.CurrentStatus(ctx, a); err != nil || s != "new" {
		t.Fatalf("CurrentStatus with no event = %q, %v", s, err)
	}
	if _, err := q.InsertStatusEvent(ctx, b, "applied", "", "gremy", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertStatusEvent(ctx, c, "new", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	n, err := q.ShortlistNew(ctx, []int64{a, b, c}, "listed in the brief of 2026-10-23")
	if err != nil || n != 2 {
		t.Fatalf("ShortlistNew = %d, %v; want 2", n, err)
	}
	for id, want := range map[int64]string{a: "shortlisted", b: "applied", c: "shortlisted"} {
		if s, _ := q.CurrentStatus(ctx, id); s != want {
			t.Errorf("job %d status = %q, want %q", id, s, want)
		}
	}

	sum, err := q.JobSummaryByID(ctx, b)
	if err != nil || sum.Title != "Go Engineer 2" || sum.Company != "Northwind Labs" || sum.Status != "applied" || sum.ListingStatus != "open" {
		t.Fatalf("summary = %+v, %v", sum, err)
	}
	if _, err := q.JobSummaryByID(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown job summary = %v", err)
	}

	rows, total, err := q.TrackList(ctx, "", 0)
	if err != nil || len(rows) != 3 || total != 3 {
		t.Fatalf("TrackList = %+v, %d, %v", rows, total, err)
	}
	if rows, total, _ := q.TrackList(ctx, "applied", 0); len(rows) != 1 || rows[0].JobID != b || total != 1 {
		t.Fatalf("TrackList applied = %+v, %d", rows, total)
	}
	if _, _, err := q.TrackList(ctx, "dreaming", 0); !errors.Is(err, store.ErrBadStatus) {
		t.Fatalf("bad filter = %v", err)
	}
	if n, err := q.CountJobsFirstSeen(ctx, "linkedin", time.UTC, now); err != nil || n != 3 {
		t.Fatalf("CountJobsFirstSeen = %d, %v", n, err)
	}
	if n, err := q.CountJobsFirstSeen(ctx, "linkedin", time.UTC, now.AddDate(0, 0, -2)); err != nil || n != 0 {
		t.Fatalf("CountJobsFirstSeen two days ago = %d, %v", n, err)
	}
}

// TestTrackListLatestEventDecides: a job's current status is its latest event over all its
// events, so a job set back to new after a non-new event is new, not active; the cap keeps
// the latest changes and total counts every match.
func TestTrackListLatestEventDecides(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	back := seedBriefJob(t, q, "1", now)   // applied, then new again
	active := seedBriefJob(t, q, "2", now) // new, then shortlisted
	fresh := seedBriefJob(t, q, "3", now)  // only new
	for _, e := range []struct {
		job    int64
		status string
		actor  string
		ago    time.Duration
	}{
		{back, "new", "system", 5 * time.Hour},
		{back, "applied", leadstore.ActorGremy, 4 * time.Hour},
		{back, "new", leadstore.ActorGremy, time.Hour},
		{active, "new", "system", 3 * time.Hour},
		{active, "shortlisted", leadstore.ActorGremy, 2 * time.Hour},
		{fresh, "new", "system", 30 * time.Minute},
	} {
		id, err := q.InsertStatusEvent(ctx, e.job, e.status, "", e.actor, nil)
		if err != nil {
			t.Fatal(err)
		}
		ownerExec(t, `UPDATE status_event SET at = $1 WHERE id = $2`, now.Add(-e.ago), id)
	}

	ids := func(rows []store.TrackRow) []int64 {
		var out []int64
		for _, r := range rows {
			out = append(out, r.JobID)
		}
		return out
	}
	for _, tc := range []struct {
		status string
		limit  int
		want   []int64
		total  int
	}{
		{"", 0, []int64{active}, 1},
		{"applied", 0, nil, 0},
		{"new", 0, []int64{fresh, back}, 2},
		{"new", 1, []int64{fresh}, 2},
		{"all", 0, []int64{fresh, back, active}, 3},
		{"all", 2, []int64{fresh, back}, 3},
	} {
		rows, total, err := q.TrackList(ctx, tc.status, tc.limit)
		if err != nil || !slices.Equal(ids(rows), tc.want) || total != tc.total {
			t.Errorf("TrackList(%q, %d) = %v, %d, %v; want %v, %d", tc.status, tc.limit, ids(rows), total, err, tc.want, tc.total)
		}
	}
	if rows, _, _ := q.TrackList(ctx, "new", 0); len(rows) == 2 && (rows[1].Status != "new" || rows[1].Title != "Go Engineer 1") {
		t.Errorf("job set back to new = %+v", rows[1])
	}
}
