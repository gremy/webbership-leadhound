//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
)

// Every handle, name and link here is invented (example/exemplu handles, .example hosts).

// igRealJev is a scripted decider under a real model's name: what --decider jev runs in a test,
// never reaching Jev.
type igRealJev struct{ *ai.Fake }

func (igRealJev) Name() string { return "jev-test-1" }

// igStubJev puts f in place of import's real decider, with LEADHOUND_LIVE=1, for the test.
func igStubJev(t *testing.T, f *ai.Fake) *ai.Fake {
	t.Helper()
	saved := igJev
	igJev = func(config.Config) (ai.Decider, error) { return igRealJev{f}, nil }
	t.Cleanup(func() { igJev = saved })
	t.Setenv("LEADHOUND_LIVE", "1")
	return f
}

// igAt is p captured at at.
func igAt(p igP, at string) igP {
	p.at = at
	return p
}

// igInstagramRoutes are the live instagram routes of lead's org, as a channel is picked from.
func igInstagramRoutes(t *testing.T, lead int64) []leadstore.Route {
	t.Helper()
	routes, err := leadstoretest.Q(t).RoutesFor(context.Background(), igOrgOf(t, lead))
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(routes, func(r leadstore.Route) bool { return r.Kind != leadstore.RouteInstagram })
}

func igLeadChannel(t *testing.T, lead int64) string {
	t.Helper()
	l, err := leadstoretest.Q(t).LeadByID(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	return l.Channel
}

// TestLeadsIGFakeNeverOverwritesJev: real Jev's answers stand. A fake import after a Jev import
// (a recapture, so the fake would write on a newer capture) asks nothing and writes no answer,
// the cards and the review list read Jev's; a fake answer stored over Jev's before this was
// fixed never stands either; and a Jev import after the fake one compares with Jev's own rows,
// so it asks nothing again.
func TestLeadsIGFakeNeverOverwritesJev(t *testing.T) {
	s := newIGScene(t)
	real := igStubJev(t, igDefaultFake())
	fake := igStub(t, &ai.Fake{Answers: igNotCoffeeAns()})
	lines := []string{igProfileLine(igA), igProfileLine(igE)}
	wantLines(t, "jev import", igImport(t, lines, "--decider", "jev"), "· decider jev ·", "  jev: calls 2 ·")
	card := igChoices(igFor(t, s.A))
	if card != "cafe_seating/none_seen/stampino(rule)" {
		t.Fatalf("A after the Jev import: %s", card)
	}
	jevRows := func() int { return dbCount(t, `SELECT count(*) FROM ig_judgment WHERE source = 'jev'`) }
	rows := jevRows()

	recap := []string{igProfileLine(igAt(igA, "2026-11-06T09:00:00Z")), igProfileLine(igAt(igE, "2026-11-06T09:00:00Z"))}
	wantLines(t, "fake import", igImport(t, recap, "--decider", "fake"), "· decider fake ·", "  jev: calls 0 ·",
		"real Jev's answers kept from the fake 2")
	if fake.Calls() != 0 || jevRows() != rows {
		t.Errorf("the fake was asked %d times and wrote %d answers", fake.Calls(), jevRows()-rows)
	}
	if got := igChoices(igFor(t, s.A)); got != card {
		t.Errorf("A after the fake import: %s, was %s", got, card)
	}
	if it := igReviewItem(t, "newcafe.example"); it == nil || it.Place != leadrate.IGCafeSeating {
		t.Errorf("the unmatched café's review row after the fake import: %+v", it)
	}

	// A fake answer written over Jev's on the newest capture (as before this fix) never stands.
	newest := int64(dbCount(t, `SELECT max(id) FROM ig_profile WHERE handle = 'harbour_lane.example'`))
	conf := 0.9
	if _, err := leadstoretest.Q(t).AddIGJudgment(context.Background(), leadstore.IGJudgment{ProfileID: newest, Question: leadstore.IGPlace,
		Choice: leadrate.IGNotCoffee, RawChoice: leadrate.IGNotCoffee, Confidence: &conf, Source: leadstore.IGFromJev,
		Model: leadstore.IGFakeModel, InputSHA: bytes.Repeat([]byte{0xfa}, 32)}); err != nil {
		t.Fatal(err)
	}
	if got := igChoices(igFor(t, s.A)); got != card {
		t.Errorf("A with a stale fake answer on its newest capture: %s, was %s", got, card)
	}

	wantLines(t, "jev re-import", igImport(t, recap, "--decider", "jev"), "  jev: calls 0 · unchanged 2")
	if real.Calls() != 2 {
		t.Errorf("Jev was asked %d times in all, want 2", real.Calls())
	}
}

// TestLeadsIGRecaptureCountsMove: a recapture whose counts moved a little (a follower gained, a
// post added) reads the same to Jev and costs no call.
func TestLeadsIGRecaptureCountsMove(t *testing.T) {
	newIGScene(t)
	f := igStub(t, igDefaultFake())
	igImport(t, []string{igProfileLine(igE)})
	calls := f.Calls()
	moved := igAt(igE, "2026-11-06T09:00:00Z")
	moved.followers, moved.following, moved.posts = 104, 51, 11
	wantLines(t, "recapture", igImport(t, []string{igProfileLine(moved)}), "  profiles: 1 new", "  jev: calls 0 · unchanged 1")
	if f.Calls() != calls {
		t.Errorf("the recapture called Jev %d times", f.Calls()-calls)
	}
}

// TestLeadsIGVendorAccountNeverARoute: a café's link to its loyalty vendor's own account (as the
// 00031 backfill read it) is never picked as its channel, and the operator matching the vendor's
// account to a lead writes it as no route.
func TestLeadsIGVendorAccountNeverARoute(t *testing.T) {
	s := newIGScene(t)
	igStub(t, igDefaultFake())
	igAddRoute(t, s.D, "stampino.app")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if ch := igLeadChannel(t, s.D); ch == leadstore.RouteInstagram {
		t.Errorf("D's channel is its vendor's Instagram account")
	}
	vendor := igP{handle: "stampino.app", name: "Stampino Example", category: "App", bio: "Stamp cards for cafes"}
	igImport(t, []string{igProfileLine(vendor)})
	mustRun(t, "leads", "ig", "match", "stampino.app", strconv.FormatInt(s.B, 10))
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d AND value LIKE '%%stampino.app%%'`, igOrgOf(t, s.B))); n != 0 {
		t.Errorf("the operator's match wrote the vendor's account as %d routes", n)
	}
}

// TestLeadsIGTakeBackOnNotCoffee: a route an import wrote (a domain match) is taken back when a
// recapture reads the account as no coffee place: retired by the system, the lead's channel
// picked again without it, the lead page calling it "from Instagram" before. The operator's "it's
// this lead" claims it back as his, and his unmatch never takes back a route he claimed.
func TestLeadsIGTakeBackOnNotCoffee(t *testing.T) {
	s := newIGScene(t)
	f := igStub(t, igDefaultFake())
	wantLines(t, "import", igImport(t, []string{igProfileLine(igB)}), "  routes: 1 added · 0 taken back")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if ch := igLeadChannel(t, s.B); ch != leadstore.RouteInstagram {
		t.Fatalf("B's channel after the build: %q, want instagram", ch)
	}
	wantLines(t, "contacts", mustRun(t, "leads", "contacts", "list", strconv.FormatInt(s.B, 10)),
		"instagram: https://www.instagram.com/bean_house.example/ (from Instagram)")

	turned := igAt(igB, "2026-11-06T09:00:00Z")
	turned.category, turned.bio = "Web designer", "We design websites"
	f.ByState[igStateOf(t, igProfileLine(turned))] = igNotCoffeeAns()
	wantLines(t, "not coffee", igImport(t, []string{igProfileLine(turned)}), "not coffee 1", "  routes: 0 added · 1 taken back")
	if rs := igInstagramRoutes(t, s.B); len(rs) != 0 {
		t.Errorf("B still holds its Instagram route: %+v", rs)
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_mark m JOIN contact_route r ON r.id = m.route_id
		WHERE r.kind = 'instagram' AND m.mark = 'retire' AND m.actor = 'system' AND m.note = '`+igTakeBackNote+`'`); n != 1 {
		t.Errorf("%d system retire marks, want 1", n)
	}
	if ch := igLeadChannel(t, s.B); ch != leadstore.ChannelNone {
		t.Errorf("B's channel after the take-back: %q, want none", ch)
	}
	wantLines(t, "same capture again", igImport(t, []string{igProfileLine(turned)}), "0 taken back")

	wantLines(t, "operator match", mustRun(t, "leads", "ig", "match", "bean_house.example", strconv.FormatInt(s.B, 10)), "Instagram route added")
	if rs := igInstagramRoutes(t, s.B); len(rs) != 1 || !rs[0].Operator {
		t.Fatalf("B after the operator's match: %+v, want its route claimed back", rs)
	}
	if ch := igLeadChannel(t, s.B); ch != leadstore.RouteInstagram {
		t.Errorf("B's channel after the claim: %q", ch)
	}
	mustRun(t, "leads", "ig", "unmatch", "bean_house.example")
	if rs := igInstagramRoutes(t, s.B); len(rs) != 1 {
		t.Errorf("the operator's unmatch took back the route he claimed: %+v", rs)
	}
}

// TestLeadsIGTakeBackOnOperatorCalls: the operator matching a handle to another lead takes back
// the route an import wrote on the lead it matched before, and his unmatch takes back the route
// his match wrote. A route the org's site gave (not the import's) is never touched.
func TestLeadsIGTakeBackOnOperatorCalls(t *testing.T) {
	s := newIGScene(t)
	igStub(t, igDefaultFake())
	p := igP{handle: "cafeneaua_ex.example", name: "Cafeneaua Exemplu Two", category: "Cafe", bio: "Cafea la noi",
		link: "https://cafeneaua-exemplu.example/"}
	wantLines(t, "import", igImport(t, []string{igProfileLine(p)}), "  routes: 1 added")
	if rs := igInstagramRoutes(t, s.C); len(rs) != 1 {
		t.Fatalf("C's routes after the domain match: %+v", rs)
	}

	mustRun(t, "leads", "ig", "match", p.handle, strconv.FormatInt(s.D, 10))
	if rs := igInstagramRoutes(t, s.C); len(rs) != 0 {
		t.Errorf("C keeps the route of a handle now matched to D: %+v", rs)
	}
	if rs := igInstagramRoutes(t, s.D); len(rs) != 1 || rs[0].Value != enrich.InstagramURL(p.handle) {
		t.Fatalf("D after the operator's match: %+v", rs)
	}

	mustRun(t, "leads", "ig", "unmatch", p.handle)
	if rs := igInstagramRoutes(t, s.D); len(rs) != 0 {
		t.Errorf("D keeps the route after the unmatch: %+v", rs)
	}
	// A's route came from its site: an unmatch of its handle leaves it.
	mustRun(t, "leads", "ig", "import", igFile(t, igProfileLine(igA)), "--campaign", "cafes-loyalty")
	mustRun(t, "leads", "ig", "unmatch", igA.handle)
	if rs := igInstagramRoutes(t, s.A); len(rs) != 1 {
		t.Errorf("the unmatch took back A's site route: %+v", rs)
	}
}

// TestLeadsIGSiteHandleNeedsTheName: a handle a lead holds from its website alone attaches only
// when the profile's name shares a word with the lead's names (not a coffee word); otherwise it
// waits for review as a weak match naming the lead. A handle the operator claimed, or a profile
// linking the lead's own website, attaches under any name.
func TestLeadsIGSiteHandleNeedsTheName(t *testing.T) {
	s := newIGScene(t)
	igStub(t, igDefaultFake())
	igAddRoute(t, s.C, "studio_tema.example")
	igAddRoute(t, s.C, "terasa_doi.example")
	igAddRoute(t, s.B, "sitelink.example")
	theme := igP{handle: "studio_tema.example", name: "Studio Tema Cafeneaua", category: "Web designer", bio: "Site-uri pentru cafenele"}
	same := igP{handle: "terasa_doi.example", name: "Terasa Exemplu", category: "Cafe", bio: "Cafea si prajituri"}
	linked := igP{handle: "sitelink.example", name: "Totally Other Name", category: "Cafe", bio: "Come and sit",
		link: "https://bean-house.example/"}
	wantLines(t, "import", igImport(t, []string{igProfileLine(theme), igProfileLine(same), igProfileLine(linked)}),
		"a site's handle under another name 1")
	tier := func(h string) string {
		t.Helper()
		m, err := leadstoretest.Q(t).IGMatches(context.Background(), []string{h})
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s %d", m[h].Tier, m[h].LeadID)
	}
	for h, want := range map[string]string{theme.handle: fmt.Sprintf("name %d", s.C), same.handle: fmt.Sprintf("handle %d", s.C),
		linked.handle: fmt.Sprintf("handle %d", s.B)} {
		if got := tier(h); got != want {
			t.Errorf("%s matched %s, want %s", h, got, want)
		}
	}
	if it := igReviewItem(t, theme.handle); it == nil || it.Status != leadstore.IGReviewNew || it.Note != igSiteHandleNote ||
		!slices.Equal(it.Candidates, []int64{s.C}) {
		t.Errorf("the theme author's review row: %+v", it)
	}
	if ig := igFor(t, s.C); ig == nil || ig.Match.Handle != same.handle {
		t.Errorf("C's card: %+v, want %s", ig, same.handle)
	}

	// The operator claims the site's route as the café's: the handle needs no name any more.
	route := slices.IndexFunc(igInstagramRoutes(t, s.C), func(r leadstore.Route) bool { return r.Value == enrich.InstagramURL(theme.handle) })
	if route < 0 {
		t.Fatal("C's site route of the theme author is gone")
	}
	if err := leadstoretest.Q(t).MarkContact(context.Background(), leadstore.Mark{OrgID: igOrgOf(t, s.C),
		RouteID: igInstagramRoutes(t, s.C)[route].ID, Mark: leadstore.MarkClaim}); err != nil {
		t.Fatal(err)
	}
	igImport(t, []string{igProfileLine(igAt(theme, "2026-11-06T09:00:00Z"))})
	if got := tier(theme.handle); got != fmt.Sprintf("handle %d", s.C) {
		t.Errorf("the claimed handle matched %s", got)
	}
	if got := igStatus(t, theme.handle); got != leadstore.IGReviewMatched {
		t.Errorf("the claimed handle's review status %q", got)
	}
}

// TestLeadsIGConsoleReviewLoyalty: a review row shows Instagram's loyalty and vendor answers in
// words, standing as the card reads them; none seen and none show nothing.
func TestLeadsIGConsoleReviewLoyalty(t *testing.T) {
	newIGScene(t)
	f := igStub(t, igDefaultFake())
	stamps := igP{handle: "stamps_cafe.example", name: "Stamps Cafe Example", category: "Cafe", bio: "Ask for our stamp card"}
	f.ByState[igStateOf(t, igProfileLine(stamps))] = map[string]ai.Answer{leadrate.IGPlace: igAns(leadrate.IGCafeSeating, 0.9),
		leadrate.IGLoyalty: igAns(leadrate.IGRunsScheme, 0.9), leadrate.IGVendor: igAns(leadrate.IGPaperCard, 0.9)}
	igImport(t, []string{igProfileLine(stamps), igProfileLine(igE)})
	p, err := leadsBackend(t).InstagramReview(context.Background(), console.IGReviewNew)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range p.Rows {
		got[r.Handle] = r.Loyalty + " | " + r.Vendor
	}
	if want := "runs a loyalty or stamp scheme | a paper card"; got[stamps.handle] != want {
		t.Errorf("the stamp-card café's row: %q, want %q", got[stamps.handle], want)
	}
	if got[igE.handle] != " | " {
		t.Errorf("the café with none seen shows %q", got[igE.handle])
	}
	if !strings.Contains(fmt.Sprint(p.Rows), stamps.handle) {
		t.Errorf("rows %+v", p.Rows)
	}
}
