//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// Migration 00018: contact research, and why each starred lead is or is not ready.

var (
	wantEmail = []string{leadstore.WantEmail}
	op        = leadstore.ActorOperator
)

func i64(n int64) *int64 { return &n }

// request asks research for lead and fails the test on an error.
func (f outreachFixture) request(t *testing.T, lead int64, trigger string) (int64, bool) {
	t.Helper()
	id, created, err := f.q.RequestResearch(context.Background(), lead, trigger, wantEmail, op)
	if err != nil {
		t.Fatalf("request research lead %d (%s): %v", lead, trigger, err)
	}
	return id, created
}

// researchStatus returns row id's status.
func (f outreachFixture) researchStatus(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := f.owner.QueryRow(context.Background(), "SELECT status FROM contact_research WHERE id = $1", id).Scan(&s); err != nil {
		t.Fatalf("research %d status: %v", id, err)
	}
	return s
}

// ageResearch moves row id's times back by days, as if it ran that long ago.
func (f outreachFixture) ageResearch(t *testing.T, id int64, days int) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(),
		`UPDATE contact_research SET requested_at = requested_at - make_interval(days => $2),
		   started_at = started_at - make_interval(days => $2), finished_at = finished_at - make_interval(days => $2)
		 WHERE id = $1`, id, days); err != nil {
		t.Fatalf("age research %d: %v", id, err)
	}
}

// claim claims every claimable row and wants exactly ids, in that order.
func (f outreachFixture) claim(t *testing.T, want ...int64) {
	t.Helper()
	rows, err := f.q.ClaimResearch(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := []int64{}
	for _, r := range rows {
		got = append(got, r.ID)
		if r.Status != leadstore.ResearchLooking || r.StartedAt == nil || r.Domain == "" {
			t.Errorf("claimed row %+v: want looking with started_at and its org", r)
		}
	}
	if !reflect.DeepEqual(got, append([]int64{}, want...)) {
		t.Fatalf("claimed %v, want %v", got, want)
	}
}

// TestResearchStatusGrid walks one org's attempts through every status and operation.
func TestResearchStatusGrid(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "grid.localhost", nil, 7, "")
	if _, err := f.q.Star(ctx, a, op); err != nil {
		t.Fatal(err)
	}

	// star → queued; star again while queued → no duplicate.
	id, created := f.request(t, a, leadstore.ResearchStar)
	if !created || f.researchStatus(t, id) != leadstore.ResearchQueued {
		t.Fatalf("star: id %d created %v status %s, want a new queued row", id, created, f.researchStatus(t, id))
	}
	for _, trig := range []string{leadstore.ResearchStar, leadstore.ResearchButton, leadstore.ResearchCLI} {
		if again, created := f.request(t, a, trig); again != id || created {
			t.Errorf("%s while queued: %d %v, want %d false", trig, again, created, id)
		}
	}
	lr, err := f.q.ResearchForLead(ctx, a)
	if err != nil || lr.Latest == nil || lr.Latest.ID != id || lr.NextAt != nil {
		t.Fatalf("lead page while queued: %+v %v, want the queued row and no next date", lr, err)
	}

	// claim → looking; a second claim takes nothing; a request while looking duplicates nothing.
	f.claim(t, id)
	f.claim(t)
	if again, created := f.request(t, a, leadstore.ResearchButton); again != id || created {
		t.Errorf("button while looking: %d %v, want %d false", again, created, id)
	}

	// finish found; a second finish is refused.
	if err := f.q.FinishResearch(ctx, id, leadstore.ResearchFound, "claude-haiku-5-5", i64(1234), 2, 1); err != nil {
		t.Fatalf("finish found: %v", err)
	}
	if err := f.q.FinishResearch(ctx, id, leadstore.ResearchNothing, "m", nil, 0, 0); !errors.Is(err, leadstore.ErrResearchNotLooking) {
		t.Errorf("finish twice: %v, want ErrResearchNotLooking", err)
	}
	lr, err = f.q.ResearchForLead(ctx, a)
	if err != nil || lr.Latest == nil || lr.Latest.Status != leadstore.ResearchFound || lr.Latest.CostMicros == nil ||
		*lr.Latest.CostMicros != 1234 || lr.Latest.Verified != 1 || lr.Latest.Candidates != 2 || lr.Latest.FinishedAt == nil {
		t.Fatalf("lead page after found: %+v %v", lr.Latest, err)
	}
	if lr.NextAt == nil || lr.NextAt.Sub(*lr.Latest.StartedAt) != leadstore.ResearchWindow {
		t.Errorf("lead page next date = %v, want started_at + 90 days", lr.NextAt)
	}

	// button and cli inside 90 days → refused with the date; star → no row, no error.
	for _, trig := range []string{leadstore.ResearchButton, leadstore.ResearchCLI} {
		_, _, err := f.q.RequestResearch(ctx, a, trig, wantEmail, op)
		var soon *leadstore.ResearchTooSoonError
		if !errors.As(err, &soon) || !errors.Is(err, leadstore.ErrResearchTooSoon) || soon.LastID != id ||
			soon.NextAt.Sub(*lr.NextAt).Abs() > time.Second {
			t.Errorf("%s inside 90 days: %v, want ResearchTooSoonError for %d next %v", trig, err, id, lr.NextAt)
		}
	}
	if got, created := f.request(t, a, leadstore.ResearchStar); got != id || created {
		t.Errorf("star inside 90 days: %d %v, want the last attempt %d, created false", got, created, id)
	}

	// 91 days later another may run.
	f.ageResearch(t, id, 91)
	id2, created := f.request(t, a, leadstore.ResearchButton)
	if !created || id2 == id {
		t.Fatalf("button after 91 days: %d %v, want a new row", id2, created)
	}

	// nothing, capped, error: capped (refused before any call) does not count toward the
	// window; nothing and error do.
	for _, c := range []struct {
		status      string
		cand, verif int
		counts      bool
	}{
		{leadstore.ResearchNothing, 3, 0, true},
		{leadstore.ResearchCapped, 0, 0, false},
		{leadstore.ResearchError, 0, 0, true},
	} {
		f.claim(t, id2)
		if err := f.q.FinishResearch(ctx, id2, c.status, "claude-haiku-5-5", i64(0), c.cand, c.verif); err != nil {
			t.Fatalf("finish %s: %v", c.status, err)
		}
		if got := f.researchStatus(t, id2); got != c.status {
			t.Errorf("finish %s: status %s", c.status, got)
		}
		next, created, err := f.q.RequestResearch(ctx, a, leadstore.ResearchCLI, wantEmail, op)
		switch {
		case c.counts && !errors.Is(err, leadstore.ErrResearchTooSoon):
			t.Errorf("cli after %s: %d %v %v, want refused", c.status, next, created, err)
		case !c.counts && (err != nil || !created):
			t.Errorf("cli after %s: %d %v %v, want a new row", c.status, next, created, err)
		}
		f.ageResearch(t, id2, 91)
		if c.counts {
			if next, created, err = f.q.RequestResearch(ctx, a, leadstore.ResearchCLI, wantEmail, op); err != nil || !created {
				t.Fatalf("cli 91 days after %s: %d %v %v", c.status, next, created, err)
			}
		}
		id2 = next
	}

	// Bad finishes are refused before the table sees them, and the row stays looking.
	f.claim(t, id2)
	for _, bad := range []struct {
		status      string
		cand, verif int
		cost        *int64
	}{
		{leadstore.ResearchFound, 1, 0, nil},
		{leadstore.ResearchNothing, 1, 1, nil},
		{leadstore.ResearchQueued, 0, 0, nil},
		{leadstore.ResearchFound, 1, 2, nil},
		{leadstore.ResearchCapped, 0, 0, i64(-1)},
	} {
		if err := f.q.FinishResearch(ctx, id2, bad.status, "m", bad.cost, bad.cand, bad.verif); !errors.Is(err, leadstore.ErrBadResearch) {
			t.Errorf("finish %+v: %v, want ErrBadResearch", bad, err)
		}
	}
	if got := f.researchStatus(t, id2); got != leadstore.ResearchLooking {
		t.Errorf("after bad finishes: %s, want looking", got)
	}
	list, err := f.q.ResearchList(ctx, leadstore.ResearchListOptions{})
	if err != nil || len(list) != 5 || list[0].ID != id2 {
		t.Errorf("list: %d rows (%v), want 5 newest first", len(list), err)
	}
	looking, err := f.q.ResearchList(ctx, leadstore.ResearchListOptions{Status: leadstore.ResearchLooking})
	if err != nil || len(looking) != 1 || looking[0].ID != id2 {
		t.Errorf("list looking: %+v %v", looking, err)
	}
}

// TestResearchQueuedNeverRanDoesNotCount: a queued row asked for long ago, which never ran,
// starts no 90-day window.
func TestResearchQueuedNeverRanDoesNotCount(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "queued.localhost", nil, 7, "") // never starred: its star row is never claimed
	id, _ := f.request(t, a, leadstore.ResearchStar)
	f.ageResearch(t, id, 200)
	f.claim(t) // a star row of a lead not starred is skipped
	if got, created, err := f.q.RequestResearch(ctx, a, leadstore.ResearchButton, wantEmail, op); err != nil || created || got != id {
		t.Errorf("button with an old queued row: %d %v %v, want the queued row, no refusal", got, created, err)
	}
	lr, err := f.q.ResearchForLead(ctx, a)
	if err != nil || lr.NextAt != nil {
		t.Errorf("lead page: next %v (%v), want none: nothing ran", lr.NextAt, err)
	}
}

// TestResearchClaimOrder: starred leads' rows first, then the rest, each oldest first; a
// star row whose lead was unstarred is skipped and left queued until it is starred again.
func TestResearchClaimOrder(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	b := f.siteLead(t, "b.localhost", nil, 7, "")
	c := f.siteLead(t, "c.localhost", nil, 7, "")
	d := f.siteLead(t, "d.localhost", nil, 7, "")
	for _, l := range []int64{b, d} {
		if _, err := f.q.Star(ctx, l, op); err != nil {
			t.Fatal(err)
		}
	}
	rb, _ := f.request(t, b, leadstore.ResearchStar)
	rc, _ := f.request(t, c, leadstore.ResearchCLI) // older than d's, but not starred
	rd, _ := f.request(t, d, leadstore.ResearchStar)
	if _, err := f.q.Unstar(ctx, b, op); err != nil {
		t.Fatal(err)
	}
	f.claim(t, rd, rc)
	if got := f.researchStatus(t, rb); got != leadstore.ResearchQueued {
		t.Fatalf("unstarred while queued: %s, want left queued", got)
	}
	if _, err := f.q.Star(ctx, b, op); err != nil {
		t.Fatal(err)
	}
	f.claim(t, rb)

	// max limits the take.
	e := f.siteLead(t, "e.localhost", nil, 7, "")
	g := f.siteLead(t, "g.localhost", nil, 7, "")
	re, _ := f.request(t, e, leadstore.ResearchCLI)
	f.request(t, g, leadstore.ResearchCLI)
	rows, err := f.q.ClaimResearch(ctx, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != re {
		t.Fatalf("claim 1: %+v %v, want only %d", rows, err, re)
	}
}

// TestResearchRequestRefusals: a suppressed, deferred or past-new lead gets no research (a
// star succeeds quietly), and bad input is refused.
func TestResearchRequestRefusals(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	sup := f.siteLead(t, "sup.localhost", nil, 7, "")
	if _, err := f.q.Suppress(ctx, 0, leadstore.SuppressDomain, "sup.localhost", leadstore.ReasonManual, "test"); err != nil {
		t.Fatal(err)
	}
	def := f.siteLead(t, "def.localhost", nil, 7, "")
	if err := f.q.DeferLead(ctx, def, time.Now().AddDate(0, 0, 3)); err != nil {
		t.Fatal(err)
	}
	skip := f.siteLead(t, "skip.localhost", nil, 7, "")
	f.setState(t, skip, leadstore.LeadSkipped)
	for _, l := range []int64{sup, def, skip} {
		if id, created, err := f.q.RequestResearch(ctx, l, leadstore.ResearchStar, wantEmail, op); err != nil || created || id != 0 {
			t.Errorf("star on blocked lead %d: %d %v %v, want 0 false nil", l, id, created, err)
		}
		if _, _, err := f.q.RequestResearch(ctx, l, leadstore.ResearchButton, wantEmail, op); !errors.Is(err, leadstore.ErrResearchBlocked) {
			t.Errorf("button on blocked lead %d: %v, want ErrResearchBlocked", l, err)
		}
	}
	ok := f.siteLead(t, "ok.localhost", nil, 7, "")
	for name, call := range map[string]func() error{
		"trigger": func() error { _, _, err := f.q.RequestResearch(ctx, ok, "cron", wantEmail, op); return err },
		"want": func() error {
			_, _, err := f.q.RequestResearch(ctx, ok, leadstore.ResearchCLI, []string{"fax"}, op)
			return err
		},
		"empty": func() error { _, _, err := f.q.RequestResearch(ctx, ok, leadstore.ResearchCLI, nil, op); return err },
	} {
		if err := call(); !errors.Is(err, leadstore.ErrBadResearch) {
			t.Errorf("bad %s: %v, want ErrBadResearch", name, err)
		}
	}
	if _, _, err := f.q.RequestResearch(ctx, ok, leadstore.ResearchCLI, wantEmail, "gremy"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("bad actor: %v, want ErrUnknownActor", err)
	}
	if _, _, err := f.q.RequestResearch(ctx, 999999, leadstore.ResearchCLI, wantEmail, op); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v, want ErrLeadNotFound", err)
	}
	if _, err := f.q.ResearchForLead(ctx, 999999); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("lead page of a missing lead: %v, want ErrLeadNotFound", err)
	}
	// Both wants in one request are stored sorted, each once.
	id, created, err := f.q.RequestResearch(ctx, ok, leadstore.ResearchCLI, []string{"phone", "email", "phone"}, op)
	if err != nil || !created {
		t.Fatalf("request both: %v", err)
	}
	var want []string
	if err := f.owner.QueryRow(ctx, "SELECT want FROM contact_research WHERE id = $1", id).Scan(&want); err != nil ||
		!reflect.DeepEqual(want, []string{"email", "phone"}) {
		t.Errorf("stored want %v (%v), want [email phone]", want, err)
	}
}

// TestResearchColdStartTwin: an empty database, then an org with no facts or routes at all
// beside one with a phone fact, both researched the same way.
func TestResearchColdStartTwin(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	if rows, err := f.q.ClaimResearch(ctx, 5); err != nil || len(rows) != 0 {
		t.Fatalf("cold claim: %v %v", rows, err)
	}
	if rows, err := f.q.ResearchList(ctx, leadstore.ResearchListOptions{}); err != nil || len(rows) != 0 {
		t.Fatalf("cold list: %v %v", rows, err)
	}
	if st, err := f.q.StarredStatus(ctx, leadstore.StarredStatusOptions{}); err != nil || len(st) != 0 {
		t.Fatalf("cold starred status: %v %v", st, err)
	}
	bare := f.siteLead(t, "bare.localhost", nil, 7, "")
	lr, err := f.q.ResearchForLead(ctx, bare)
	if err != nil || lr.Latest != nil || lr.NextAt != nil {
		t.Fatalf("never researched lead page: %+v %v", lr, err)
	}
	rich := f.siteLead(t, "rich.localhost", nil, 7, "")
	var orgID int64
	if err := f.owner.QueryRow(ctx, "SELECT org_id FROM lead WHERE id = $1", rich).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertFact(ctx, leadstore.FactRow{OrgID: orgID, Kind: leadstore.FactPhone, Value: "+44 20 7946 0000",
		Normalized: "+442079460000", FetchID: f.fid, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []int64{bare, rich} {
		if _, err := f.q.Star(ctx, l, op); err != nil {
			t.Fatal(err)
		}
	}
	o := leadstore.StarredStatusOptions{CandidateOptions: leadstore.CandidateOptions{IncludeReserved: true, StarredOnly: true}}
	st, err := f.q.StarredStatus(ctx, o)
	if err != nil || len(st) != 2 || st[0].Reason != leadstore.StarredNoEmailYet || st[1].Reason != leadstore.StarredNoEmailYet {
		t.Fatalf("starred status: %+v %v, want both no_email_yet", st, err)
	}
	rb, cb := f.request(t, bare, leadstore.ResearchStar)
	rr, cr := f.request(t, rich, leadstore.ResearchStar)
	if !cb || !cr {
		t.Fatal("twin requests not created")
	}
	f.claim(t, rb, rr)
	st, err = f.q.StarredStatus(ctx, o)
	if err != nil || st[0].Reason != leadstore.StarredLooking || st[1].Reason != leadstore.StarredLooking {
		t.Fatalf("starred status while looking: %+v %v", st, err)
	}
}

// TestStarredStatusContract builds one starred lead per reason and checks each reason, and
// that the leads StarredStatus calls ready are exactly the starred leads CandidateLeads
// offers for the same options.
func TestStarredStatusContract(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	today := time.Now()
	leads := map[string]int64{}
	add := func(name, email, country string) int64 {
		t.Helper()
		id := f.siteLead(t, name+".localhost", nil, 7, email)
		if _, err := f.owner.Exec(ctx, "UPDATE org SET country = $1 WHERE id = (SELECT org_id FROM lead WHERE id = $2)", country, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.q.Star(ctx, id, op); err != nil {
			t.Fatal(err)
		}
		leads[name] = id
		return id
	}
	add("ready", "info@ready.localhost", "GB")
	add("ranked", "info@ranked.localhost", "GB") // offered, but after the limit
	add("noemail", "", "GB")
	looking := add("looking", "", "GB")
	nothing := add("nothing", "", "GB")
	add("off", "", "US")                             // not in the country map: off
	add("excluded", "info@excluded.localhost", "FR") // the profile excludes FR
	add("formonly", "", "DE")                        // a form country with no form
	add("suppressed", "info@suppressed.localhost", "GB")
	deferred := add("deferred", "info@deferred.localhost", "GB")
	notnew := add("notnew", "info@notnew.localhost", "GB")
	// An unstarred lead the batch would take is not in the status at all.
	f.siteLead(t, "unstarred.localhost", nil, 7, "info@unstarred.localhost")

	rl, _ := f.request(t, looking, leadstore.ResearchStar)
	rn, _ := f.request(t, nothing, leadstore.ResearchStar)
	f.claim(t, rl, rn)
	if err := f.q.FinishResearch(ctx, rn, leadstore.ResearchNothing, "claude-haiku-5-5", i64(900), 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.Suppress(ctx, 0, leadstore.SuppressDomain, "suppressed.localhost", leadstore.ReasonManual, "test"); err != nil {
		t.Fatal(err)
	}
	until := today.AddDate(0, 0, 4)
	if err := f.q.DeferLead(ctx, deferred, until); err != nil {
		t.Fatal(err)
	}
	f.setState(t, notnew, leadstore.LeadSkipped)

	o := leadstore.StarredStatusOptions{
		CandidateOptions: leadstore.CandidateOptions{
			Limit: 1, IncludeReserved: true, StarredOnly: true, Today: today,
			Profile: leadconfig.Profile{Kinds: map[string]int{leadstore.LeadUnsupported: 10}, ExcludeCountries: []string{"FR"}},
		},
		Countries: map[string]string{"GB": leadconfig.ChannelEmail, "FR": leadconfig.ChannelEmail, "DE": leadconfig.ChannelForm},
	}
	st, err := f.q.StarredStatus(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ready": leadstore.StarredReady, "ranked": leadstore.StarredRankedOut, "noemail": leadstore.StarredNoEmailYet,
		"looking": leadstore.StarredLooking, "nothing": leadstore.StarredFoundNothing, "off": leadstore.StarredCountryRule,
		"excluded": leadstore.StarredCountryRule, "formonly": leadstore.StarredCountryRule,
		"suppressed": leadstore.StarredSuppressed, "deferred": leadstore.StarredDeferred, "notnew": leadstore.StarredNotNew,
	}
	byLead := map[int64]leadstore.StarredState{}
	for _, s := range st {
		byLead[s.LeadID] = s
	}
	if len(st) != len(want) {
		t.Errorf("starred status has %d leads, want %d (the unstarred lead left out)", len(st), len(want))
	}
	for name, reason := range want {
		s, ok := byLead[leads[name]]
		if !ok || s.Reason != reason {
			t.Errorf("%s: reason %q (%q), want %q", name, s.Reason, s.Detail, reason)
		}
	}
	if s := byLead[leads["nothing"]]; s.Date == nil {
		t.Error("found_nothing carries no date")
	}
	if s := byLead[leads["deferred"]]; s.Date == nil || s.Date.Format(time.DateOnly) != until.Format(time.DateOnly) {
		t.Errorf("deferred date = %v, want %s", s.Date, until.Format(time.DateOnly))
	}
	for _, name := range []string{"off", "excluded", "formonly"} {
		if d := byLead[leads[name]].Detail; d == "" {
			t.Errorf("%s: country rule names no country", name)
		}
	}
	// In the operator's order.
	for i := 1; i < len(st); i++ {
		if st[i-1].Position > st[i].Position {
			t.Errorf("status out of order at %d", i)
		}
	}

	// The contract, at the batch's limit and with none.
	for _, limit := range []int{1, 0} {
		o.Limit = limit
		st, err := f.q.StarredStatus(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		cands, err := f.q.CandidateLeads(ctx, o.CandidateOptions)
		if err != nil {
			t.Fatal(err)
		}
		var ready, offered []int64
		for _, s := range st {
			if s.Reason == leadstore.StarredReady {
				ready = append(ready, s.LeadID)
			}
		}
		for _, c := range cands {
			if c.Starred {
				offered = append(offered, c.LeadID)
			}
		}
		slices.Sort(ready)
		slices.Sort(offered)
		if len(ready) == 0 || !reflect.DeepEqual(ready, offered) {
			t.Errorf("limit %d: ready %v, candidate query %v: want the same non-empty set", limit, ready, offered)
		}
	}
}

// startResearchAgo moves looking row id's requested_at and started_at back by d, as if the
// job took it d ago.
func (f outreachFixture) startResearchAgo(t *testing.T, id int64, d time.Duration) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(),
		`UPDATE contact_research SET requested_at = requested_at - make_interval(secs => $2),
		   started_at = started_at - make_interval(secs => $2)
		 WHERE id = $1 AND status = 'looking'`, id, d.Seconds()); err != nil {
		t.Fatalf("start research %d %v ago: %v", id, d, err)
	}
}

// TestResearchStaleLookingReaped: a row left looking by a crashed run, older than
// ResearchStaleAfter, is finished as error (cost unknown, nothing verified) by the next
// claim or request, never put back in the queue, so it is never paid twice; it counts toward
// the 90-day window like any attempt that ran, and once that passes the org is researched
// again. A row looking inside the threshold is untouched.
func TestResearchStaleLookingReaped(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	stale := f.siteLead(t, "stale.localhost", nil, 7, "")
	fresh := f.siteLead(t, "fresh.localhost", nil, 7, "")
	asked := f.siteLead(t, "asked.localhost", nil, 7, "")
	rs, _ := f.request(t, stale, leadstore.ResearchCLI)
	rf, _ := f.request(t, fresh, leadstore.ResearchCLI)
	ra, _ := f.request(t, asked, leadstore.ResearchCLI)
	f.claim(t, rs, rf, ra)
	f.startResearchAgo(t, rs, leadstore.ResearchStaleAfter+time.Minute)
	f.startResearchAgo(t, rf, leadstore.ResearchStaleAfter-10*time.Minute)
	f.startResearchAgo(t, ra, leadstore.ResearchStaleAfter+time.Minute)

	// The request path reaps its own org: asked's stale row is no longer the open row.
	_, _, err := f.q.RequestResearch(ctx, asked, leadstore.ResearchButton, wantEmail, op)
	var soon *leadstore.ResearchTooSoonError
	if !errors.As(err, &soon) || soon.LastID != ra {
		t.Fatalf("button on a stale looking row: %v, want ResearchTooSoonError naming %d (it ran)", err, ra)
	}
	if got := f.researchStatus(t, ra); got != leadstore.ResearchError {
		t.Fatalf("stale row after a request: %s, want error", got)
	}
	if got := f.researchStatus(t, rs); got != leadstore.ResearchLooking {
		t.Fatalf("another org's stale row after a request: %s, want looking (reaped per org)", got)
	}

	// The claim reaps every org's stale row, takes nothing new, and leaves the fresh one.
	f.claim(t)
	if got := f.researchStatus(t, rs); got != leadstore.ResearchError {
		t.Fatalf("stale row after a claim: %s, want error", got)
	}
	if got := f.researchStatus(t, rf); got != leadstore.ResearchLooking {
		t.Fatalf("fresh looking row after a claim: %s, want looking (untouched)", got)
	}
	var cost *int64
	var cand, verif int
	var model string
	var started, finished *time.Time
	if err := f.owner.QueryRow(ctx,
		`SELECT cost_micros, candidates, verified, model, started_at, finished_at FROM contact_research WHERE id = $1`, rs).
		Scan(&cost, &cand, &verif, &model, &started, &finished); err != nil {
		t.Fatal(err)
	}
	if cost != nil || cand != 0 || verif != 0 || model != "" || started == nil || finished == nil || finished.Before(*started) {
		t.Errorf("reaped row: cost %v cand %d verif %d model %q started %v finished %v, want unknown cost, nothing, times kept",
			cost, cand, verif, model, started, finished)
	}
	// The crashed run, if it ever came back, finds its row finished.
	if err := f.q.FinishResearch(ctx, rs, leadstore.ResearchNothing, "m", i64(5), 0, 0); !errors.Is(err, leadstore.ErrResearchNotLooking) {
		t.Errorf("finish a reaped row: %v, want ErrResearchNotLooking", err)
	}
	if err := f.q.FinishResearch(ctx, rf, leadstore.ResearchNothing, "m", i64(5), 0, 0); err != nil {
		t.Errorf("finish the fresh row: %v", err)
	}

	// It counts toward the window: no fresh row inside 90 days, no second bill.
	_, _, err = f.q.RequestResearch(ctx, stale, leadstore.ResearchButton, wantEmail, op)
	if !errors.As(err, &soon) || soon.LastID != rs {
		t.Fatalf("button inside 90 days of a reaped row: %v, want ResearchTooSoonError naming %d", err, rs)
	}
	if got, created := f.request(t, stale, leadstore.ResearchStar); got != rs || created {
		t.Errorf("star inside 90 days of a reaped row: %d %v, want %d false", got, created, rs)
	}
	// After the window the org is researched again: the open slot is free.
	f.ageResearch(t, rs, 91)
	id, created := f.request(t, stale, leadstore.ResearchButton)
	if !created || id == rs {
		t.Fatalf("button after the window: %d %v, want a new row", id, created)
	}
	f.claim(t, id)
}
