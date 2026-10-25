//go:build integration

package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// rankFixture is a store with every status, listing state, gate and decision verdict, both
// deciders, jobs seen inside and before the day, and one job with no observation. It returns
// the day the rows seen "today" fall in.
func rankFixture(t *testing.T, s *store.Store) (dayStart, dayEnd time.Time) {
	t.Helper()
	ctx := context.Background()
	q := s.Q()
	posted := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	applicants := 42
	observe := func(jobID, fetchID int64, title, desc string, closed bool, gv ...string) int64 {
		t.Helper()
		in := store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test@1", Title: title,
			LocationText: "Remote, EU", DescriptionClean: desc, ApplyType: "offsite", Closed: closed}
		if title == "Senior Go Engineer" {
			in.PostedAtEst, in.ApplicantCount, in.ApplyType = &posted, &applicants, "ats"
		}
		obsID, err := q.InsertObservation(ctx, in)
		if err != nil {
			t.Fatalf("InsertObservation: %v", err)
		}
		for i := 0; i+1 < len(gv); i += 2 {
			if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: jobID, ObservationID: obsID, Gate: gv[i],
				Verdict: gv[i+1], Reason: gv[i] + " reason", Snippet: gv[i] + " words", DerivedBy: "gates@1"}); err != nil {
				t.Fatalf("InsertGateResult: %v", err)
			}
		}
		return obsID
	}
	decide := func(jobID, obsID int64, decider string, verdicts ...string) {
		t.Helper()
		var ds []store.DecisionInsert
		for i, c := range []string{"title", "stack", "seniority", "location", "work_rights"} {
			if i >= len(verdicts) {
				break
			}
			ds = append(ds, store.DecisionInsert{JobID: jobID, ObservationID: obsID, Decider: decider, Criterion: c,
				Verdict: verdicts[i], Confidence: 0.9, Reason: c + " by " + decider, InputSHA256: []byte("h")})
		}
		if err := s.InsertDecisions(ctx, ds); err != nil {
			t.Fatal(err)
		}
	}
	status := func(jobID int64, statuses ...string) {
		t.Helper()
		for _, st := range statuses {
			if _, err := q.InsertStatusEvent(ctx, jobID, st, "", "gremy", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	backdate := func(jobID int64) {
		t.Helper()
		ownerExec(t, "UPDATE job SET first_seen_at = now() - interval '3 days', last_seen_at = now() - interval '3 days' WHERE id = $1", jobID)
		ownerExec(t, "UPDATE job_observation SET observed_at = now() - interval '3 days' WHERE job_id = $1", jobID)
	}
	const real = "jev-1.13.0"

	// a: seen today, all passed and decided by the real decider; a later observation's gates
	// are the current ones and the decisions stay.
	a, fa := seedJob(t, q, "1000000001")
	obsA := observe(a, fa, "Go Engineer", "first text", false, "language", "pass")
	decide(a, obsA, real, "pass", "pass", "pass", "pass", "pass")
	observe(a, fa, "Go Engineer II", "a: Go and Postgres", false, "language", "pass", "workplace", "flag")

	// b: closed, a failing gate, skipped, seen days ago: its description is never needed.
	b, fb := seedSourced(t, q, "greenhouse", "acme/2", fetch.KindBoard)
	observe(b, fb, "Go Engineer", "b: failed long ago", true, "language", "fail")
	status(b, "skipped")
	backdate(b)

	// c: a failing gate, but seen today.
	c, fc := seedSourced(t, q, "lever", "acme/3", fetch.KindBoard)
	observe(c, fc, "Go Engineer", "c: failed today", false, "location", "fail", "rule", "pass")

	// d: expired listing, applied, only the fake decider decided it.
	d, fd := seedSourced(t, q, "ashby", "acme/4", fetch.KindBoard)
	obsD := observe(d, fd, "Go Engineer", "d: expired", false, "language", "pass")
	decide(d, obsD, "fake", "pass", "pass", "pass", "pass", "pass")
	status(d, "applied")
	ownerExec(t, "UPDATE job SET listing_status = 'expired' WHERE id = $1", d)

	// e: unreachable, shortlisted then interviewing; a real fail, then a newer fake pass on
	// the same criterion; days ago, no failing gate.
	e, fe := seedSourced(t, q, "greenhouse-fixture", "acme/5", fetch.KindDetail)
	obsE := observe(e, fe, "Senior Go Engineer", "e: unreachable", false, "work_rights", "flag")
	decide(e, obsE, real, "fail", "pass", "unclear", "pass", "pass")
	decide(e, obsE, "fake", "pass")
	status(e, "shortlisted", "interviewing")
	ownerExec(t, "UPDATE job SET listing_status = 'unreachable' WHERE id = $1", e)
	backdate(e)

	// f: no gates at all, never decided, seen days ago.
	f, ff := seedSourced(t, q, "smartrecruiters", "acme/6", fetch.KindDetail)
	observe(f, ff, "Go Developer", "f: no gates", false)
	backdate(f)

	// g: flags only, partly decided with unclear verdicts, seen days ago.
	g, fg := seedJob(t, q, "1000000007")
	obsG := observe(g, fg, "Backend Engineer", "g: flags", false, "language", "flag", "workplace", "flag")
	decide(g, obsG, real, "unclear", "unclear")
	backdate(g)

	// h: a fail among passes on an older observation; the current one passes. Seen days ago.
	h, fh := seedJob(t, q, "1000000008")
	observe(h, fh, "Go Engineer", "h: old fail", false, "language", "fail")
	observe(h, fh, "Go Engineer", "h: now passing", false, "language", "pass")
	backdate(h)

	seedJob(t, q, "1000000009") // no observation: never ranked

	now := time.Now()
	return now.Add(-time.Hour), now.Add(time.Hour)
}

// streamRows collects the pass's rows, for a test fixture only.
func streamRows(t *testing.T, s *store.Store, opt store.RankStreamOptions) []store.RankRow {
	t.Helper()
	var out []store.RankRow
	if err := s.ForEachRankRow(context.Background(), opt, func(r store.RankRow) error {
		out = append(out, r)
		return nil
	}); err != nil {
		t.Fatalf("ForEachRankRow: %v", err)
	}
	return out
}

// wantRows builds the rows the stream must give from the oracle, plus the facts the oracle
// does not carry (source, last seen, the current observation id) read directly.
func wantRows(t *testing.T, s *store.Store, opt store.RankStreamOptions) []store.RankRow {
	t.Helper()
	ctx := context.Background()
	ins, err := storetest.OracleRankInputs(ctx, s, opt.AllowFake)
	if err != nil {
		t.Fatal(err)
	}
	type extra struct {
		source   string
		lastSeen time.Time
		obsID    int64
	}
	rows, err := s.Pool().Query(ctx,
		`SELECT j.id, j.source, j.last_seen_at, o.id FROM job j
		 JOIN LATERAL (SELECT id FROM job_observation WHERE job_id = j.id
		               ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true`)
	if err != nil {
		t.Fatal(err)
	}
	extras := map[int64]extra{}
	for rows.Next() {
		var id int64
		var x extra
		if err := rows.Scan(&id, &x.source, &x.lastSeen, &x.obsID); err != nil {
			t.Fatal(err)
		}
		extras[id] = x
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	var out []store.RankRow
	for _, in := range ins {
		x := extras[in.JobID]
		r := store.RankRow{
			JobID: in.JobID, ObservationID: x.obsID, Source: x.source, ListingStatus: in.ListingStatus,
			Status: in.Status, Company: in.Company, Title: in.Title, Location: in.Location, ApplyType: in.ApplyType,
			Closed: in.Closed, PostedAtEst: in.PostedAtEst, ApplicantCount: in.ApplicantCount,
			SeenAt: in.SeenAt, LastSeenAt: x.lastSeen,
		}
		failed := false
		for _, g := range in.Gates {
			r.Gates = append(r.Gates, store.GateVerdictRow{Gate: g.Gate, Verdict: g.Verdict})
			failed = failed || g.Verdict == "fail"
		}
		for _, d := range in.Decisions {
			r.Decisions = append(r.Decisions, store.DecisionVerdictRow{Criterion: d.Criterion, Verdict: d.Verdict})
		}
		seenInDay := !in.SeenAt.Before(opt.DayStart) && in.SeenAt.Before(opt.DayEnd)
		switch opt.DescriptionFor {
		case store.DescAll:
			r.Description = in.Description
		case store.DescWhereRanked:
			if seenInDay || !failed {
				r.Description = in.Description
			}
		}
		out = append(out, r)
	}
	return out
}

// TestRankRowsMatchOracle: the streamed pass gives, job by job and in job id order, the
// facts the old read gave, for every description mode and both fake-decision settings; the
// where-ranked mode drops exactly the descriptions of rows seen before the day with a
// failing gate.
func TestRankRowsMatchOracle(t *testing.T) {
	s := appStore(t)
	start, end := rankFixture(t, s)
	for _, mode := range []store.DescMode{store.DescNone, store.DescWhereRanked, store.DescAll} {
		for _, allowFake := range []bool{false, true} {
			opt := store.RankStreamOptions{AllowFake: allowFake, DescriptionFor: mode, DayStart: start, DayEnd: end}
			got, want := streamRows(t, s, opt), wantRows(t, s, opt)
			if len(want) != 8 {
				t.Fatalf("oracle read %d jobs, want 8", len(want))
			}
			if len(got) != len(want) {
				t.Fatalf("mode %d fake %v: %d rows, want %d", mode, allowFake, len(got), len(want))
			}
			for i := range want {
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Errorf("mode %d fake %v job %d:\n got %+v\nwant %+v", mode, allowFake, want[i].JobID, got[i], want[i])
				}
			}
		}
	}

	// The where-ranked mode spares exactly job b's description (failed, seen days ago).
	opt := store.RankStreamOptions{DescriptionFor: store.DescWhereRanked, DayStart: start, DayEnd: end}
	var empty []string
	for _, r := range streamRows(t, s, opt) {
		if r.Description == "" {
			empty = append(empty, r.Title+"/"+r.Source)
		}
	}
	if !slices.Equal(empty, []string{"Go Engineer/greenhouse"}) {
		t.Errorf("where-ranked left out %v, want only job b", empty)
	}
}

// TestRankRowsEmptyStore: with no jobs, fn is never called and the pass succeeds.
func TestRankRowsEmptyStore(t *testing.T) {
	s := appStore(t)
	calls := 0
	err := s.ForEachRankRow(context.Background(), store.RankStreamOptions{DescriptionFor: store.DescAll},
		func(store.RankRow) error { calls++; return nil })
	if err != nil || calls != 0 {
		t.Fatalf("empty store: %d calls, %v", calls, err)
	}
}

// TestRankRowsStopOnError: fn's error stops the pass at once and is returned as it is.
func TestRankRowsStopOnError(t *testing.T) {
	s := appStore(t)
	rankFixture(t, s)
	stop := errors.New("stop here")
	calls := 0
	err := s.ForEachRankRow(context.Background(), store.RankStreamOptions{}, func(store.RankRow) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("got %d calls, %v; want 1 call and the fn's error", calls, err)
	}
	// The connection went back to the pool in a usable state.
	if got := streamRows(t, s, store.RankStreamOptions{}); len(got) != 8 {
		t.Fatalf("after a stopped pass: %d rows", len(got))
	}
}

// TestRankDetailsMatchOracle: the details of the asked jobs only are the oracle's
// description, full gate results and latest decisions; an unknown job is absent and no
// jobs give an empty map.
func TestRankDetailsMatchOracle(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	rankFixture(t, s)
	for _, allowFake := range []bool{false, true} {
		ins, err := storetest.OracleRankInputs(ctx, s, allowFake)
		if err != nil {
			t.Fatal(err)
		}
		ask := []int64{ins[0].JobID, ins[4].JobID, ins[5].JobID, 99999}
		got, err := s.Q().RankDetails(ctx, ask, allowFake)
		if err != nil {
			t.Fatalf("RankDetails: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("fake %v: %d details, want 3", allowFake, len(got))
		}
		for _, in := range []store.RankInput{ins[0], ins[4], ins[5]} {
			want := store.RankDetail{Description: in.Description, Gates: in.Gates, Decisions: in.Decisions}
			if !reflect.DeepEqual(got[in.JobID], want) {
				t.Errorf("fake %v job %d:\n got %+v\nwant %+v", allowFake, in.JobID, got[in.JobID], want)
			}
		}
	}
	got, err := s.Q().RankDetails(ctx, nil, false)
	if err != nil || len(got) != 0 {
		t.Fatalf("no jobs: %v, %v", got, err)
	}
}
