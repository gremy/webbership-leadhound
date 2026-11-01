//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00029: a queued "Draft it", claimed and finished by the drafting service.

// dr15Request asks a draft for lead on its own channel and fails the test on an error.
func dr15Request(t *testing.T, f outreachFixture, lead int64) int64 {
	t.Helper()
	id, err := f.q.RequestDraft(context.Background(), lead, "")
	if err != nil {
		t.Fatalf("request draft lead %d: %v", lead, err)
	}
	return id
}

// dr15Claim claims up to limit rows and returns their ids in the order they came back.
func dr15Claim(t *testing.T, f outreachFixture, limit int) []int64 {
	t.Helper()
	rows, err := f.q.ClaimDraftRequests(context.Background(), limit)
	if err != nil {
		t.Fatalf("claim draft requests: %v", err)
	}
	ids := []int64{}
	for _, r := range rows {
		ids = append(ids, r.ID)
		if r.Status != leadstore.DraftRequestDrafting || r.StartedAt == nil || r.FinishedAt != nil {
			t.Errorf("claimed row %+v: want drafting with started_at only", r)
		}
	}
	return ids
}

// dr15Latest returns lead's latest request and fails the test when it has none.
func dr15Latest(t *testing.T, f outreachFixture, lead int64) leadstore.DraftRequest {
	t.Helper()
	r, err := f.q.DraftRequestFor(context.Background(), lead)
	if err != nil || r == nil {
		t.Fatalf("draft request for lead %d: %+v, %v", lead, r, err)
	}
	return *r
}

// dr15AgeStart moves row id's started_at back, as if the service took it that long ago.
func dr15AgeStart(t *testing.T, f outreachFixture, id int64, ago time.Duration) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(),
		`UPDATE draft_request SET started_at = now() - make_interval(secs => $2) WHERE id = $1`, id, ago.Seconds()); err != nil {
		t.Fatalf("age draft request %d: %v", id, err)
	}
}

func TestDraftRequestOpenRefused(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "open.localhost", nil, 7, "")

	if r, err := f.q.DraftRequestFor(ctx, lead); err != nil || r != nil {
		t.Fatalf("before any request: %+v, %v", r, err)
	}
	id, err := f.q.RequestDraft(ctx, lead, leadstore.ChannelForm)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	first := dr15Latest(t, f, lead)
	if first.ID != id || first.LeadID != lead || first.Channel != leadstore.ChannelForm || first.Status != leadstore.DraftRequestQueued ||
		first.StartedAt != nil || first.FinishedAt != nil || first.Reason != "" || first.BatchID != nil {
		t.Fatalf("queued row = %+v", first)
	}

	// Queued, then drafting: both refuse a second request and name the open row and its time.
	for _, status := range []string{leadstore.DraftRequestQueued, leadstore.DraftRequestDrafting} {
		if status == leadstore.DraftRequestDrafting {
			if got := dr15Claim(t, f, 1); !slices.Equal(got, []int64{id}) {
				t.Fatalf("claim = %v, want %d", got, id)
			}
		}
		_, err := f.q.RequestDraft(ctx, lead, "")
		var open *leadstore.DraftRequestOpenError
		if !errors.Is(err, leadstore.ErrDraftRequestOpen) || !errors.As(err, &open) {
			t.Fatalf("second request while %s: err = %v", status, err)
		}
		if open.ID != id || open.Status != status || !open.RequestedAt.Equal(first.RequestedAt) {
			t.Errorf("open error while %s = %+v, want row %d asked at %s", status, open, id, first.RequestedAt)
		}
	}

	var rows int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM draft_request WHERE lead_id = $1`, lead).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("rows for the lead = %d, %v; want 1", rows, err)
	}
}

// TestDraftRequestAfterFinished: a finished request, done or failed, leaves the lead free, and
// DraftRequestFor shows the newest.
func TestDraftRequestAfterFinished(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.formLead(t, "again.localhost")
	batch := f.manualBatch(t, false, lead)[0].BatchID

	first := dr15Request(t, f, lead)
	dr15Claim(t, f, 1)
	if err := f.q.FinishDraftRequest(ctx, first, leadstore.DraftRequestDone, "", &batch); err != nil {
		t.Fatalf("finish done: %v", err)
	}
	done := dr15Latest(t, f, lead)
	if done.Status != leadstore.DraftRequestDone || done.BatchID == nil || *done.BatchID != batch || done.Reason != "" ||
		done.StartedAt == nil || done.FinishedAt == nil {
		t.Fatalf("done row = %+v", done)
	}

	second := dr15Request(t, f, lead)
	if second == first {
		t.Fatalf("second request reused row %d", first)
	}
	if got := dr15Latest(t, f, lead); got.ID != second || got.Status != leadstore.DraftRequestQueued {
		t.Fatalf("latest after the second request = %+v", got)
	}
	dr15Claim(t, f, 1)
	if err := f.q.FinishDraftRequest(ctx, second, leadstore.DraftRequestFailed, "  no route to draft for  ", nil); err != nil {
		t.Fatalf("finish failed: %v", err)
	}
	failed := dr15Latest(t, f, lead)
	if failed.Status != leadstore.DraftRequestFailed || failed.Reason != "no route to draft for" || failed.BatchID != nil || failed.FinishedAt == nil {
		t.Fatalf("failed row = %+v", failed)
	}
	if third := dr15Request(t, f, lead); third == second {
		t.Fatalf("third request reused row %d", second)
	}
}

func TestDraftRequestBadInput(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "bad.localhost", nil, 7, "")

	if _, err := f.q.RequestDraft(ctx, lead, "phone"); !errors.Is(err, leadstore.ErrBadDraftRequest) {
		t.Errorf("unknown channel: err = %v", err)
	}
	if _, err := f.q.RequestDraft(ctx, 999999, ""); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: err = %v", err)
	}
	if id, err := f.q.RequestDraft(ctx, lead, "linkedin"); err != nil {
		t.Errorf("linkedin channel: %v", err)
	} else if got := dr15Latest(t, f, lead); got.ID != id || got.Channel != "linkedin" {
		t.Errorf("linkedin row = %+v", got)
	}

	id := dr15Claim(t, f, 1)[0]
	batch := int64(1)
	for _, c := range []struct {
		name, status, reason string
		batch                *int64
	}{
		{"done without a batch", leadstore.DraftRequestDone, "", nil},
		{"done with a reason", leadstore.DraftRequestDone, "why", &batch},
		{"failed without a reason", leadstore.DraftRequestFailed, "  ", nil},
		{"failed with a batch", leadstore.DraftRequestFailed, "why", &batch},
		{"queued is not an ending", leadstore.DraftRequestQueued, "", nil},
		{"drafting is not an ending", leadstore.DraftRequestDrafting, "", nil},
	} {
		if err := f.q.FinishDraftRequest(ctx, id, c.status, c.reason, c.batch); !errors.Is(err, leadstore.ErrBadDraftRequest) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	// A batch that does not exist is the foreign key's refusal, and the row stays drafting.
	missing := int64(999999)
	if err := f.q.FinishDraftRequest(ctx, id, leadstore.DraftRequestDone, "", &missing); err == nil {
		t.Error("finish with a missing batch succeeded")
	}
	if got := dr15Latest(t, f, lead); got.Status != leadstore.DraftRequestDrafting {
		t.Errorf("after refused finishes = %+v, want drafting", got)
	}
}

// TestDraftRequestFinishNotDrafting: finishing a row that is not drafting is an error, never a
// silent success, and changes nothing.
func TestDraftRequestFinishNotDrafting(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "finish.localhost", nil, 7, "")

	if err := f.q.FinishDraftRequest(ctx, 999999, leadstore.DraftRequestFailed, "why", nil); !errors.Is(err, leadstore.ErrDraftRequestNotDrafting) {
		t.Errorf("missing row: err = %v", err)
	}
	id := dr15Request(t, f, lead)
	if err := f.q.FinishDraftRequest(ctx, id, leadstore.DraftRequestFailed, "why", nil); !errors.Is(err, leadstore.ErrDraftRequestNotDrafting) {
		t.Errorf("queued row: err = %v", err)
	}
	if got := dr15Latest(t, f, lead); got.Status != leadstore.DraftRequestQueued || got.Reason != "" {
		t.Errorf("queued row after the refused finish = %+v", got)
	}
	dr15Claim(t, f, 1)
	if err := f.q.FinishDraftRequest(ctx, id, leadstore.DraftRequestFailed, "first", nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := f.q.FinishDraftRequest(ctx, id, leadstore.DraftRequestFailed, "second", nil); !errors.Is(err, leadstore.ErrDraftRequestNotDrafting) {
		t.Errorf("finished row: err = %v", err)
	}
	if got := dr15Latest(t, f, lead); got.Reason != "first" {
		t.Errorf("reason after the refused finish = %q, want first", got.Reason)
	}
}

// TestDraftRequestLongReason: a reason past the table's limit, counted in characters, is cut to
// fit rather than leaving the row drafting.
func TestDraftRequestLongReason(t *testing.T) {
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "long.localhost", nil, 7, "")
	id := dr15Request(t, f, lead)
	dr15Claim(t, f, 1)
	long := strings.Repeat("ș", leadstore.DraftRequestReasonMax+50)
	if err := f.q.FinishDraftRequest(context.Background(), id, leadstore.DraftRequestFailed, long, nil); err != nil {
		t.Fatalf("finish with a long reason: %v", err)
	}
	got := dr15Latest(t, f, lead)
	if got.Status != leadstore.DraftRequestFailed || utf8.RuneCountInString(got.Reason) != leadstore.DraftRequestReasonMax {
		t.Errorf("row = status %s, reason of %d characters", got.Status, utf8.RuneCountInString(got.Reason))
	}
}

func TestDraftRequestClaimOrder(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := dr15Request(t, f, f.siteLead(t, "a.localhost", nil, 7, ""))
	b := dr15Request(t, f, f.siteLead(t, "b.localhost", nil, 7, ""))
	c := dr15Request(t, f, f.siteLead(t, "c.localhost", nil, 7, ""))
	// Oldest first, by when it was asked for and not by id.
	if _, err := f.owner.Exec(ctx, `UPDATE draft_request SET requested_at = requested_at - interval '1 hour' WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if got := dr15Claim(t, f, 0); !slices.Equal(got, []int64{c}) { // limit <= 0 takes one
		t.Errorf("claim with limit 0 = %v, want [%d]", got, c)
	}
	if got := dr15Claim(t, f, 10); !slices.Equal(got, []int64{a, b}) {
		t.Errorf("claim = %v, want [%d %d]", got, a, b)
	}
	if got := dr15Claim(t, f, 10); len(got) != 0 {
		t.Errorf("claim of nothing = %v", got)
	}
}

// TestDraftRequestClaimSkipsLocked: a row another claim holds is passed over, without waiting.
func TestDraftRequestClaimSkipsLocked(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	held := dr15Request(t, f, f.siteLead(t, "held.localhost", nil, 7, ""))
	free := dr15Request(t, f, f.siteLead(t, "free.localhost", nil, 7, ""))

	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the deferred rollback of a test transaction
	if _, err := tx.Exec(ctx, `SELECT id FROM draft_request WHERE id = $1 FOR UPDATE`, held); err != nil {
		t.Fatal(err)
	}
	timed, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := f.q.ClaimDraftRequests(timed, 10)
	if err != nil {
		t.Fatalf("claim past a locked row: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != free {
		t.Fatalf("claim = %+v, want only row %d", rows, free)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dr15Claim(t, f, 10); !slices.Equal(got, []int64{held}) {
		t.Errorf("claim after the lock went = %v, want [%d]", got, held)
	}
}

// TestDraftRequestConcurrentClaims: two services claiming at once never take the same row, and
// between them take every row.
func TestDraftRequestConcurrentClaims(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	var want []int64
	for _, d := range strings.Split("a b c d e f g h i j k l m n o p", " ") {
		want = append(want, dr15Request(t, f, f.siteLead(t, d+".localhost", nil, 7, "")))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	taken := make([][]int64, 2)
	errs := make([]error, 2)
	for i := range 2 {
		q := leadstore.NewQueries(storetest.AppConn(t))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				rows, err := q.ClaimDraftRequests(ctx, 3)
				if err != nil {
					errs[i] = err
					return
				}
				if len(rows) == 0 {
					return
				}
				for _, r := range rows {
					taken[i] = append(taken[i], r.ID)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent claims: %v, %v", errs[0], errs[1])
	}
	got := slices.Concat(taken[0], taken[1])
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("rows taken = %v, want each of %v once", got, want)
	}
}

// TestDraftRequestConcurrentRequests: two presses at once make one row; the loser is told of it.
func TestDraftRequestConcurrentRequests(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "race.localhost", nil, 7, "")
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]int64, 2)
	errs := make([]error, 2)
	for i := range 2 {
		q := leadstore.NewQueries(storetest.AppConn(t))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = q.RequestDraft(ctx, lead, "")
		}()
	}
	close(start)
	wg.Wait()
	winner, loser := 0, 1
	if errs[0] != nil {
		winner, loser = 1, 0
	}
	var open *leadstore.DraftRequestOpenError
	if errs[winner] != nil || !errors.As(errs[loser], &open) || open.ID != ids[winner] {
		t.Fatalf("requests = %v, %v; want one row and the other told of it", ids, errs)
	}
}

// TestDraftRequestStale: a row drafting over DraftRequestStaleAfter is failed by the next claim,
// with its reason, and one younger is left alone.
func TestDraftRequestStale(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	staleLead := f.siteLead(t, "stale.localhost", nil, 7, "")
	youngLead := f.siteLead(t, "young.localhost", nil, 7, "")
	waitLead := f.siteLead(t, "wait.localhost", nil, 7, "")
	stale := dr15Request(t, f, staleLead)
	young := dr15Request(t, f, youngLead)
	dr15Claim(t, f, 2)
	dr15AgeStart(t, f, stale, leadstore.DraftRequestStaleAfter+time.Minute)
	dr15AgeStart(t, f, young, leadstore.DraftRequestStaleAfter-time.Minute)
	waiting := dr15Request(t, f, waitLead)

	if got := dr15Claim(t, f, 10); !slices.Equal(got, []int64{waiting}) {
		t.Errorf("claim = %v, want the queued row %d and never the stale one", got, waiting)
	}
	got := dr15Latest(t, f, staleLead)
	if got.ID != stale || got.Status != leadstore.DraftRequestFailed || got.Reason != "stopped before it finished" ||
		got.BatchID != nil || got.StartedAt == nil || got.FinishedAt == nil ||
		got.FinishedAt.Before(*got.StartedAt) {
		t.Errorf("stale row after the claim = %+v", got)
	}
	if got := dr15Latest(t, f, youngLead); got.Status != leadstore.DraftRequestDrafting || got.Reason != "" {
		t.Errorf("young row after the claim = %+v, want still drafting", got)
	}
	// The dead run reporting in late is refused; the lead is free for a new request.
	if err := f.q.FinishDraftRequest(ctx, stale, leadstore.DraftRequestFailed, "late", nil); !errors.Is(err, leadstore.ErrDraftRequestNotDrafting) {
		t.Errorf("finishing a reaped row: err = %v", err)
	}
	dr15Request(t, f, staleLead)
}

// TestDraftRequestStaleFreedByRequest: a refused request wakes no service, so a request reaps
// its own lead's stale row, and only that lead's, or the lead would stay locked.
func TestDraftRequestStaleFreedByRequest(t *testing.T) {
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "locked.localhost", nil, 7, "")
	other := f.siteLead(t, "other.localhost", nil, 7, "")
	id := dr15Request(t, f, lead)
	otherID := dr15Request(t, f, other)
	dr15Claim(t, f, 2)
	dr15AgeStart(t, f, id, leadstore.DraftRequestStaleAfter+time.Minute)
	dr15AgeStart(t, f, otherID, leadstore.DraftRequestStaleAfter+time.Minute)

	next := dr15Request(t, f, lead)
	if next == id {
		t.Fatalf("request reused the stale row %d", id)
	}
	var status, reason string
	if err := f.owner.QueryRow(context.Background(), `SELECT status, reason FROM draft_request WHERE id = $1`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != leadstore.DraftRequestFailed || reason != leadstore.DraftRequestStopped {
		t.Errorf("stale row after the request = %s, %q", status, reason)
	}
	if got := dr15Latest(t, f, other); got.Status != leadstore.DraftRequestDrafting {
		t.Errorf("another lead's stale row = %+v, want left for the next claim", got)
	}
}
