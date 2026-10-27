//go:build integration

package leadstore_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestQueueFollowsStarOrder: the starred leads come first in the operator's order (star,
// pin to top, up, down), whatever their score or relevance; the unstarred follow by points,
// or by relevance (unrated last) only when the trust gate passed; an unstarred lead goes
// back among the unstarred.
func TestQueueFollowsStarOrder(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	// By points alone (all Drupal 7, no country): rank decides, a before b before … e.
	a := f.siteLead(t, "a.localhost", intp(1), 7, "info@a.localhost")
	b := f.siteLead(t, "b.localhost", intp(2), 7, "info@b.localhost")
	c := f.siteLead(t, "c.localhost", intp(3), 7, "info@c.localhost")
	d := f.siteLead(t, "d.localhost", intp(4), 7, "info@d.localhost")
	e := f.siteLead(t, "e.localhost", intp(5), 7, "info@e.localhost")
	order := func(byRelevance bool) []int64 {
		t.Helper()
		return leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile, SortByRelevance: byRelevance}))
	}
	if got := order(false); !reflect.DeepEqual(got, []int64{a, b, c, d, e}) {
		t.Fatalf("no stars = %v", got)
	}
	for _, id := range []int64{d, b, e} {
		f.star(t, id)
	}
	if got := order(false); !reflect.DeepEqual(got, []int64{d, b, e, a, c}) {
		t.Errorf("starred d, b, e = %v, want d, b, e, a, c", got)
	}
	if _, err := f.q.MoveTop(ctx, e, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.MoveDown(ctx, d, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	want := []int64{e, b, d, a, c}
	if got := order(false); !reflect.DeepEqual(got, want) {
		t.Errorf("e to the top, d down = %v, want %v", got, want)
	}
	// The flags and positions are the star order's.
	stars, err := f.q.StarOrder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cs := candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile})
	for i, s := range stars {
		if !cs[i].Starred || cs[i].Position == nil || *cs[i].Position != s.Position || cs[i].LeadID != s.LeadID {
			t.Errorf("candidate %d: lead %d starred %v position %v, want lead %d at %d", i, cs[i].LeadID, cs[i].Starred, cs[i].Position, s.LeadID, s.Position)
		}
	}
	for _, x := range cs[len(stars):] {
		if x.Starred || x.Position != nil {
			t.Errorf("lead %d: starred %v position %v, want neither", x.LeadID, x.Starred, x.Position)
		}
	}

	// Relevance orders only the unstarred, and only when trusted. A starred lead rated high
	// stays where the operator put it.
	f.rate(t, c, 90, leadstore.BandStrong)
	f.rate(t, d, 100, leadstore.BandStrong)
	if got := order(false); !reflect.DeepEqual(got, want) {
		t.Errorf("rated, not trusted = %v, want %v (points)", got, want)
	}
	if got := order(true); !reflect.DeepEqual(got, []int64{e, b, d, c, a}) {
		t.Errorf("rated, trusted = %v, want e, b, d, c, a (c rated, a unrated)", got)
	}
	cs = candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile, SortByRelevance: true})
	if cs[3].Relevance == nil || *cs[3].Relevance != 90 || cs[4].Relevance != nil {
		t.Errorf("relevance: %v / %v", cs[3].Relevance, cs[4].Relevance)
	}
	// The latest rating wins.
	f.rate(t, c, 5, leadstore.BandNo)
	f.rate(t, a, 6, leadstore.BandNo)
	if got := order(true); !reflect.DeepEqual(got, []int64{e, b, d, a, c}) {
		t.Errorf("re-rated = %v, want e, b, d, a, c", got)
	}

	// Unstar b: it goes back among the unstarred, by points.
	if _, err := f.q.Unstar(ctx, b, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if got := order(false); !reflect.DeepEqual(got, []int64{e, d, a, b, c}) {
		t.Errorf("b unstarred = %v, want e, d, a, b, c", got)
	}
	// Limit keeps the order: the first two are the starred.
	if got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile, Limit: 2})); !reflect.DeepEqual(got, []int64{e, d}) {
		t.Errorf("limit 2 = %v, want e, d", got)
	}
}

// TestQueueStarredOnly: with StarredOnly the queue is the starred leads alone, in the
// operator's order; no stars means no candidates (batches draw from starred, D6).
func TestQueueStarredOnly(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", intp(1), 7, "info@a.localhost")
	b := f.siteLead(t, "b.localhost", intp(2), 7, "info@b.localhost")
	f.siteLead(t, "c.localhost", intp(3), 7, "info@c.localhost")
	only := leadstore.CandidateOptions{Profile: testProfile, StarredOnly: true}
	if got := candidates(t, f.q, only); len(got) != 0 {
		t.Errorf("no stars: %v, want none", leadIDs(got))
	}
	if got := candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile}); len(got) != 3 {
		t.Errorf("no stars, not starred only: %d, want 3", len(got))
	}
	f.star(t, b)
	f.star(t, a)
	if got := leadIDs(candidates(t, f.q, only)); !reflect.DeepEqual(got, []int64{b, a}) {
		t.Errorf("starred only = %v, want b, a", got)
	}
	if next, err := f.q.NextUp(ctx, leadstore.CandidateOptions{Profile: testProfile, StarredOnly: true, IncludeReserved: true, Today: priorityDay}, 0); err != nil ||
		!reflect.DeepEqual(leadIDs(next), []int64{b, a}) {
		t.Errorf("NextUp starred only = %v, %v", leadIDs(next), err)
	}
	for _, id := range []int64{a, b} {
		if _, err := f.q.Unstar(ctx, id, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	if got := candidates(t, f.q, only); len(got) != 0 {
		t.Errorf("all unstarred: %v, want none", leadIDs(got))
	}
}

// TestQueueStarredStatusGrid: a star never brings a lead past new back: a starred lead in
// any other state is never a candidate and keeps its star; skip takes a starred lead out
// for good; a deferred starred lead is hidden through its date and back, first, the day
// after.
func TestQueueStarredStatusGrid(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	states := []string{
		leadstore.LeadBatched, leadstore.LeadContacted, leadstore.LeadReplied, leadstore.LeadBounced,
		leadstore.LeadOptedOut, leadstore.LeadSuppressed, leadstore.LeadSkipped, leadstore.LeadClosed,
	}
	open := f.siteLead(t, "open.localhost", intp(100), 7, "info@open.localhost")
	moved := map[string]int64{}
	for i, st := range states {
		dom := fmt.Sprintf("grid%d.localhost", i)
		id := f.siteLead(t, dom, intp(i+1), 7, "info@"+dom)
		f.star(t, id)
		f.setState(t, id, st)
		moved[st] = id
	}
	f.star(t, open)
	for _, o := range []leadstore.CandidateOptions{
		{Profile: testProfile},
		{Profile: testProfile, StarredOnly: true},
		{Profile: testProfile, SortByRelevance: true},
	} {
		if got := leadIDs(candidates(t, f.q, o)); !reflect.DeepEqual(got, []int64{open}) {
			t.Errorf("%+v: candidates = %v, want only the new lead %d", o, got, open)
		}
	}
	stars, err := f.q.StarOrder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stars) != len(states)+1 {
		t.Errorf("starred = %d, want %d: a lead past new keeps its star", len(stars), len(states)+1)
	}

	// Skip a starred new lead: out of the queue, still starred.
	skipped := f.siteLead(t, "skip-me.localhost", intp(1), 7, "info@skip-me.localhost")
	f.star(t, skipped)
	if ok, err := f.q.SkipLead(ctx, skipped); err != nil || !ok {
		t.Fatalf("skip: %v %v", ok, err)
	}
	if got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile})); slices.Contains(got, skipped) {
		t.Errorf("a skipped starred lead is offered: %v", got)
	}

	// Defer a starred lead (push back): hidden through until, first the day after.
	late := f.siteLead(t, "late.localhost", intp(1), 7, "info@late.localhost")
	f.star(t, late)
	if _, err := f.q.MoveTop(ctx, late, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	until := priorityDay.AddDate(0, 0, 14)
	if err := f.q.DeferLead(ctx, late, until); err != nil {
		t.Fatal(err)
	}
	for _, day := range []time.Time{priorityDay, until} {
		got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile, Today: day, StarredOnly: true}))
		if slices.Contains(got, late) {
			t.Errorf("on %s a starred lead deferred until %s is offered: %v", day.Format(time.DateOnly), until.Format(time.DateOnly), got)
		}
	}
	got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile, Today: until.AddDate(0, 0, 1)}))
	if !reflect.DeepEqual(got, []int64{late, open}) {
		t.Errorf("the day after = %v, want late, open", got)
	}
}

// TestQueueKindsOffAndSmallSites: a kind turned off is never offered; SkipRankOver leaves
// out the site leads with an unknown or larger list rank (a rank equal to it stays), starred
// or not, and never an agency lead (no rank); 0 is off.
func TestQueueKindsOffAndSmallSites(t *testing.T) {
	f := newOutreachFixture(t)
	big := f.siteLead(t, "big.localhost", intp(500), 7, "info@big.localhost")
	edge := f.siteLead(t, "edge.localhost", intp(1000), 6, "info@edge.localhost")
	small := f.siteLead(t, "small.localhost", intp(50000), 7, "info@small.localhost")
	unranked := f.siteLead(t, "unranked.localhost", nil, 8, "info@unranked.localhost")
	d10 := f.siteLead(t, "ten.localhost", intp(100), 10, "info@ten.localhost")
	agency := f.agencyLead(t, "agency.localhost", "hello@agency.localhost")
	f.star(t, small)
	set := func(o leadstore.CandidateOptions) []int64 {
		t.Helper()
		o.Profile = testProfile
		return sortedIDs(leadIDs(candidates(t, f.q, o))...)
	}
	all := sortedIDs(big, edge, small, unranked, d10, agency)
	if got := set(leadstore.CandidateOptions{}); !reflect.DeepEqual(got, all) {
		t.Errorf("nothing off = %v, want %v", got, all)
	}
	if got := set(leadstore.CandidateOptions{KindsOff: []string{}}); !reflect.DeepEqual(got, all) {
		t.Errorf("empty kinds off = %v, want %v", got, all)
	}
	if got, want := set(leadstore.CandidateOptions{KindsOff: []string{leadstore.LeadD10}}), sortedIDs(big, edge, small, unranked, agency); !reflect.DeepEqual(got, want) {
		t.Errorf("d10 off = %v, want %v", got, want)
	}
	if got, want := set(leadstore.CandidateOptions{KindsOff: []string{leadstore.LeadAgencyPartner, leadstore.LeadUnsupported}}), sortedIDs(d10); !reflect.DeepEqual(got, want) {
		t.Errorf("agency and unsupported off = %v, want %v", got, want)
	}
	if got, want := set(leadstore.CandidateOptions{SkipRankOver: 1000}), sortedIDs(big, edge, d10, agency); !reflect.DeepEqual(got, want) {
		t.Errorf("skip rank over 1000 = %v, want %v (small and unranked left out, starred or not)", got, want)
	}
	if got, want := set(leadstore.CandidateOptions{SkipRankOver: 999}), sortedIDs(big, d10, agency); !reflect.DeepEqual(got, want) {
		t.Errorf("skip rank over 999 = %v, want %v", got, want)
	}
	if got, want := set(leadstore.CandidateOptions{SkipRankOver: 1000, KindsOff: []string{leadstore.LeadD10}, StarredOnly: true}), []int64{}; !reflect.DeepEqual(got, want) {
		t.Errorf("starred only, the starred one small = %v, want none", got)
	}
}

// TestBatchMessagesInDraftOrder: a batch reads back in the order its leads were drafted
// (not by lead id), each lead's steps in order; the content hash is unchanged by that
// order, and approval's item numbers follow it.
func TestBatchMessagesInDraftOrder(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", intp(1), 7, "info@a.localhost")
	b := f.siteLead(t, "b.localhost", intp(2), 7, "info@b.localhost")
	c := f.siteLead(t, "c.localhost", intp(3), 7, "info@c.localhost")
	drafts := f.drafts(t, c, a, b)
	// Steps out of order within the drafts too: step 1 of a before its step 0.
	drafts[2], drafts[3] = drafts[3], drafts[2]
	id, sum, err := f.q.CreateBatch(ctx, false, drafts, []byte("templates"))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := f.q.BatchMessages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	type ls struct {
		lead int64
		step int
	}
	var got []ls
	for _, m := range ms {
		got = append(got, ls{m.LeadID, m.Step})
	}
	want := []ls{{c, 0}, {c, 1}, {a, 0}, {a, 1}, {b, 0}, {b, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("batch messages = %v, want %v", got, want)
	}
	// The same drafts in lead order hash the same.
	canon, err := leadstore.ContentSHA256(f.drafts(t, a, b, c))
	if err != nil {
		t.Fatal(err)
	}
	re, err := f.q.BatchContentSHA256(ctx, id)
	if err != nil || hex.EncodeToString(re) != hex.EncodeToString(sum) || hex.EncodeToString(canon) != hex.EncodeToString(sum) {
		t.Errorf("content hash: stored %x, recomputed %x (%v), canonical %x", sum, re, err, canon)
	}
	// Item 1 is c, the first drafted.
	if _, err := f.q.ApproveBatch(ctx, id, hex.EncodeToString(sum)[:12], []int{1}, dueAt); err != nil {
		t.Fatal(err)
	}
	if f.leadState(t, c) != leadstore.LeadSkipped || f.leadState(t, a) != leadstore.LeadBatched || f.leadState(t, b) != leadstore.LeadBatched {
		t.Errorf("skip item 1: c %s, a %s, b %s; want c skipped", f.leadState(t, c), f.leadState(t, a), f.leadState(t, b))
	}
}
