//go:build integration

package main

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// Every handle, name and link here is invented (the igScene's).

// TestLeadsIGConsoleCard: the console's lead page shows the Instagram card of a strong match only:
// the handle match carries its vendor found by a tag and its hint, the domain match its
// disagreement with the lead's place type in the shared words, and a weak (name) match and a
// lead with no Instagram show no card. Reading it changes nothing.
func TestLeadsIGConsoleCard(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	igImport(t, igSceneLines(""))
	b := leadsBackend(t)
	ctx := context.Background()
	card := func(lead int64) *console.LeadInstagramView {
		t.Helper()
		p, err := b.LeadDetail(ctx, lead)
		if err != nil {
			t.Fatal(err)
		}
		return p.Instagram
	}
	before := igSnapshot(t)

	a := card(s.A)
	if a == nil {
		t.Fatal("the handle match has no card")
	}
	if a.Handle != "harbour_lane.example" || a.URL != "https://www.instagram.com/harbour_lane.example/" || a.Matched != "its handle" ||
		!a.Counted || a.Followers != 100 || a.CapturedAt == nil || a.Private || a.Missing {
		t.Errorf("A's card head: %+v", a)
	}
	if a.Place != "a café with seating" || a.Loyalty != "runs a loyalty or stamp scheme" || a.Vendor != "Stampino" || a.VendorVia != "from a tag" {
		t.Errorf("A's answers: place %q loyalty %q vendor %q via %q", a.Place, a.Loyalty, a.Vendor, a.VendorVia)
	}
	if !slices.Equal(a.Hints, []string{"followed by Stampino"}) {
		t.Errorf("A's hints %q", a.Hints)
	}

	bc := card(s.B)
	if bc == nil {
		t.Fatal("the domain match has no card")
	}
	page, err := b.LeadDetail(ctx, s.B)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Matched != "its website" || bc.Place != "a roastery or online shop" || bc.VendorVia != "" {
		t.Errorf("B's card: %+v", bc)
	}
	if want := igPlaceDisagrees("roastery_or_online", page.PlaceType); bc.Disagree != want {
		t.Errorf("B's disagreement %q with place type %q, want %q", bc.Disagree, page.PlaceType, want)
	}
	if page.PlaceType == leadstore.PlaceCafe && bc.Disagree != leadstore.PlaceRoastery {
		t.Errorf("a coffee shop Instagram reads as a roastery does not say so: %q", bc.Disagree)
	}
	// The fake decider's read sets no type: the chip is the café default, and the card disagrees.
	if page.PlaceType != leadstore.PlaceCafe || page.PlaceTypeSource != "" {
		t.Errorf("B's chip on the fake's read: %s from %q, want the café default", page.PlaceType, page.PlaceTypeSource)
	}

	if c := card(s.C); c != nil {
		t.Errorf("a weak name match shows its signals: %+v", c)
	}
	if d := card(s.D); d != nil {
		t.Errorf("a lead with no Instagram has a card: %+v", d)
	}
	if after := igSnapshot(t); after != before {
		t.Error("reading the lead page changed a lead, an org, a place type or an event")
	}
}

// TestLeadsIGConsoleCardRealJev: on real Jev's answers the card and the place-type chip agree.
// B, read as a roastery, is a roastery by Instagram's read with no disagreement line; A, a café
// with seating, a coffee shop by the same; the operator's own call over B shows the card
// disagreeing with his call, which stands.
func TestLeadsIGConsoleCardRealJev(t *testing.T) {
	s := newIGScene(t)
	igStubJev(t, igSceneFake(t))
	igImport(t, igSceneLines(""))
	b := leadsBackend(t)
	ctx := context.Background()
	for _, tc := range []struct {
		lead              int64
		placeType, source string
		place             string
	}{
		{s.B, leadstore.PlaceRoastery, leadstore.PlaceFromInstagram, "a roastery or online shop"},
		{s.A, leadstore.PlaceCafe, leadstore.PlaceFromInstagram, "a café with seating"},
	} {
		page, err := b.LeadDetail(ctx, tc.lead)
		if err != nil {
			t.Fatal(err)
		}
		if page.PlaceType != tc.placeType || page.PlaceTypeSource != tc.source || page.Instagram == nil ||
			page.Instagram.Place != tc.place || page.Instagram.Disagree != "" {
			t.Errorf("lead %d: chip %s from %q, card %+v; want %s from %s, %q, no disagreement",
				tc.lead, page.PlaceType, page.PlaceTypeSource, page.Instagram, tc.placeType, tc.source, tc.place)
		}
	}
	if err := b.lq().SetPlaceType(ctx, s.B, leadstore.PlaceCafe, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	page, err := b.LeadDetail(ctx, s.B)
	if err != nil {
		t.Fatal(err)
	}
	if page.PlaceType != leadstore.PlaceCafe || page.PlaceTypeSource != leadstore.PlaceFromOperator || page.Instagram == nil ||
		page.Instagram.Disagree != leadstore.PlaceRoastery {
		t.Errorf("B after the operator's call: chip %s from %q, card %+v", page.PlaceType, page.PlaceTypeSource, page.Instagram)
	}
}

// TestLeadsIGConsoleReview: the review page lists each status with its counts and its candidates
// named, and its three calls write exactly what `leads ig match|dismiss|want` write: an operator's
// match, its route and the matched status; the status rows; the same refusals (a handle in no
// capture, a lead that is not a café lead) as console refusals that write nothing.
func TestLeadsIGConsoleReview(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	igImport(t, igSceneLines(""))
	b := leadsBackend(t)
	ctx := context.Background()
	review := func(status string) console.IGReviewPage {
		t.Helper()
		p, err := b.InstagramReview(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	row := func(p console.IGReviewPage, handle string) *console.IGReviewRow {
		for i := range p.Rows {
			if p.Rows[i].Handle == handle {
				return &p.Rows[i]
			}
		}
		return nil
	}

	p := review(console.IGReviewNew)
	c := row(p, "cafeneaua.exemplu")
	if c == nil || len(c.Candidates) != 1 || c.Candidates[0].LeadID != s.C || !c.Candidates[0].Matchable ||
		c.Candidates[0].Name == "" || c.URL != "https://www.instagram.com/cafeneaua.exemplu/" || c.Place != "a café with seating" {
		t.Fatalf("the weak match's row: %+v", c)
	}
	e, f := row(p, "newcafe.example"), row(p, "roast_follow.example")
	if e == nil || f == nil {
		t.Fatalf("the unmatched café or the following account is missing: %+v", p.Rows)
	}
	// The read profile carries what its capture read; the account never read carries none of it.
	if e.Bio != "Coffee and cake, come and sit" || !e.Counted || e.Followers != 100 || e.Posts != 10 || e.CapturedAt == nil ||
		e.Website != "" || e.LatestPost != "" || e.Private || e.Missing || e.FoundIn != "" {
		t.Errorf("the read café's row: %+v", e)
	}
	if f.Bio != "" || f.Counted || f.CapturedAt != nil || f.Private || f.Missing {
		t.Errorf("the never-read account's row: %+v", f)
	}
	if p.Counts[console.IGReviewNew] != len(p.Rows) {
		t.Errorf("new count %d for %d rows", p.Counts[console.IGReviewNew], len(p.Rows))
	}

	before := igTables(t)
	for _, tc := range []struct {
		a     console.IGAction
		words string
	}{
		{console.IGAction{Do: console.IGDismiss, Handle: "nobody.example"}, "@nobody.example is in no imported capture; import it first"},
		{console.IGAction{Do: console.IGMatch, Handle: "nobody.example", LeadID: s.C}, "@nobody.example is in no imported capture"},
		{console.IGAction{Do: console.IGMatch, Handle: "cafeneaua.exemplu", LeadID: 999999}, "lead 999999 is not a café lead Instagram can be matched to"},
		{console.IGAction{Do: console.IGWant, Handle: "p"}, `"p" is not an Instagram handle`},
	} {
		_, err := b.InstagramAct(ctx, tc.a)
		if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), tc.words) {
			t.Errorf("%+v: %v, want a refusal saying %q", tc.a, err, tc.words)
		}
	}
	if after := igTables(t); after != before {
		t.Errorf("a refusal wrote: %s, was %s", after, before)
	}

	// The console's match of C and the CLI's match of D write the same kind of rows.
	msg, err := b.InstagramAct(ctx, console.IGAction{Do: console.IGMatch, Handle: "@Cafeneaua.Exemplu", LeadID: s.C})
	if err != nil || msg != "@cafeneaua.exemplu is lead "+strconv.FormatInt(s.C, 10)+" now; its Instagram route is added." {
		t.Fatalf("match: %q, %v", msg, err)
	}
	mustRun(t, "leads", "ig", "match", "newcafe.example", strconv.FormatInt(s.D, 10))
	for lead, handle := range map[int64]string{s.C: "cafeneaua.exemplu", s.D: "newcafe.example"} {
		ig := igFor(t, lead)
		if ig == nil || ig.Match.Handle != handle || ig.Match.Tier != leadstore.IGTierOperator || ig.Match.Actor != leadstore.ActorGremy ||
			ig.Match.Detail != "your call" {
			t.Errorf("lead %d after the match: %+v", lead, ig)
		}
		if got := igStatus(t, handle); got != leadstore.IGReviewMatched {
			t.Errorf("%s after the match: %q", handle, got)
		}
		if n := dbCount(t, `SELECT count(*) FROM contact_route WHERE kind = 'instagram' AND org_id = `+strconv.FormatInt(igOrgOf(t, lead), 10)); n != 1 {
			t.Errorf("lead %d holds %d instagram routes, want 1", lead, n)
		}
	}
	if c := row(review(console.IGReviewMatched), "cafeneaua.exemplu"); c == nil || len(c.Candidates) != 1 || c.Candidates[0].LeadID != s.C {
		t.Errorf("the matched tab: %+v", c)
	}
	if card, err := b.LeadDetail(ctx, s.C); err != nil || card.Instagram == nil || card.Instagram.Matched != "your call" {
		t.Errorf("C's card after the console's match: %+v, %v", card.Instagram, err)
	}

	// Dismiss and want: the console's rows are the CLI's, and a repeat writes nothing.
	for _, step := range []struct {
		do, status, msg string
	}{
		{console.IGWant, leadstore.IGReviewWanted, "@roast_follow.example is wanted as a lead."},
		{console.IGWant, leadstore.IGReviewWanted, "@roast_follow.example was already wanted as a lead; nothing changed."},
		{console.IGDismiss, leadstore.IGReviewDismissed, "@roast_follow.example is not a lead."},
	} {
		rows := dbCount(t, `SELECT count(*) FROM ig_review_status WHERE handle = 'roast_follow.example'`)
		msg, err := b.InstagramAct(ctx, console.IGAction{Do: step.do, Handle: "roast_follow.example"})
		if err != nil || msg != step.msg {
			t.Errorf("%s: %q, %v", step.do, msg, err)
		}
		if got := igStatus(t, "roast_follow.example"); got != step.status {
			t.Errorf("%s: status %q", step.do, got)
		}
		grew := dbCount(t, `SELECT count(*) FROM ig_review_status WHERE handle = 'roast_follow.example'`) - rows
		if want := map[bool]int{true: 0, false: 1}[strings.Contains(step.msg, "already")]; grew != want {
			t.Errorf("%s wrote %d rows, want %d", step.do, grew, want)
		}
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_review_status WHERE handle = 'roast_follow.example' AND actor <> 'gremy' AND status <> 'new'`); n != 0 {
		t.Errorf("%d operator's calls not by the operator", n)
	}
	wantLines(t, "the CLI reads the console's call", mustRun(t, "leads", "ig", "review", "--status", "dismissed"), "@roast_follow.example")
	if got := review(console.IGReviewDismissed); row(got, "roast_follow.example") == nil {
		t.Error("the dismissed tab lacks the dismissed handle")
	}
}
