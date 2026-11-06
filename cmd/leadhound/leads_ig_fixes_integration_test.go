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

// igLater is p captured again, a day after the first capture.
func igLater(p igP) igP {
	p.at = "2026-11-06T09:00:00Z"
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

	recap := []string{igProfileLine(igLater(igA)), igProfileLine(igLater(igE))}
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
	moved := igLater(igE)
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

	turned := igLater(igB)
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
	igImport(t, []string{igProfileLine(igLater(theme))})
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

// igRoasterName gives lead a roastery row from its name, as `leads place-types apply` writes one.
func igRoasterName(t *testing.T, lead int64) {
	t.Helper()
	if err := leadstoretest.Q(t).SetPlaceType(context.Background(), lead, leadstore.PlaceRoastery, leadstore.PlaceFromName, "roasters",
		nil, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
}

// igHeld is which of leads are held out of café outreach.
func igHeld(t *testing.T, leads ...int64) map[int64]bool {
	t.Helper()
	held, err := leadstoretest.Q(t).HeldLeads(context.Background(), leads)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// igPlaceOf is lead's effective place type and its source, as "roastery/instagram".
func igPlaceOf(t *testing.T, lead int64) string {
	t.Helper()
	pts, err := leadstoretest.Q(t).PlaceTypes(context.Background(), []int64{lead})
	if err != nil {
		t.Fatal(err)
	}
	return pts[lead].Type + "/" + pts[lead].Source
}

// igPlaceRows counts the instagram place type rows.
func igPlaceRows(t *testing.T) int {
	t.Helper()
	return dbCount(t, `SELECT count(*) FROM lead_place_type WHERE source = 'instagram'`)
}

// wantInOrder fails unless every one of lines is in out, each after the one before it.
func wantInOrder(t *testing.T, what, out string, lines ...string) {
	t.Helper()
	at := 0
	for _, l := range lines {
		i := strings.Index(out[at:], l)
		if i < 0 {
			t.Errorf("%s lacks %q after byte %d:\n%s", what, l, at, out)
			return
		}
		at += i + len(l)
	}
}

// TestLeadsIGPlaceTypeFromInstagram: on real Jev's answers an import types the leads it matches
// strongly. A roastery profile (B, a domain match) is held at once; a café with seating on a
// roastery-named lead (A) is a café again and listed as released, before the held one. The run's
// note carries the counts. A re-import and a dry run write no place row. The report lists A as
// released by Instagram with the read it overrode, B as set by Instagram (marked when Jev's
// website read says otherwise), and to correct only where Instagram disagrees with the
// operator's own call, which stands.
func TestLeadsIGPlaceTypeFromInstagram(t *testing.T) {
	s := newIGScene(t)
	igStubJev(t, igSceneFake(t))
	igRoasterName(t, s.A)
	lines := []string{igProfileLine(igA), igProfileLine(igB)}

	out := igImport(t, lines)
	wantInOrder(t, "import", out, "· decider jev ·",
		"  routes: 1 added",
		"  place types from Instagram: roastery 1 · café 1 · silent 0\n",
		fmt.Sprintf("    released: lead %d: café from Instagram, was a roastery from its name\n", s.A),
		fmt.Sprintf("    held: lead %d: roastery from Instagram, was a café by default\n", s.B))
	if got := igPlaceOf(t, s.A) + " " + igPlaceOf(t, s.B); got != "cafe/instagram roastery/instagram" {
		t.Errorf("place types after the import: %s", got)
	}
	if held := igHeld(t, s.A, s.B); held[s.A] || !held[s.B] {
		t.Errorf("held after the import: %v, want only B", held)
	}
	if n := dbCount(t, `SELECT count(*) FROM run WHERE command = 'leads-ig-import'
		AND note LIKE '%; place types from Instagram: roastery 1, café 1, silent 0'`); n != 1 {
		t.Errorf("%d runs noting the place types, want 1", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM lead_place_type WHERE source = 'instagram' AND actor = 'system' AND input_sha256 IS NULL
		AND detail IN ('@harbour_lane.example: cafe_seating', '@bean_house.example: roastery_or_online')`); n != 2 || igPlaceRows(t) != 2 {
		t.Errorf("instagram rows as written: %d of %d", n, igPlaceRows(t))
	}

	rows := igPlaceRows(t)
	again := igImport(t, lines)
	wantLines(t, "re-import", again, "  place types from Instagram: roastery 0 · café 0 · silent 0")
	if strings.Contains(again, "released:") || strings.Contains(again, "held:") || igPlaceRows(t) != rows {
		t.Errorf("the re-import wrote place rows (%d, was %d):\n%s", igPlaceRows(t), rows, again)
	}
	dry := mustRun(t, "leads", "ig", "import", "--dry-run", igFile(t, lines...))
	wantLines(t, "dry run", dry, "  place types from Instagram: would write roastery 0 · café 0 · silent 0 (from what is stored now, before this file)")
	if igPlaceRows(t) != rows {
		t.Errorf("the dry run wrote %d place rows", igPlaceRows(t)-rows)
	}
	if out := mustRun(t, "leads", "ig", "lookups", "--place-type", "roastery"); !strings.HasPrefix(out, fmt.Sprintf("%d\t", s.B)) || strings.Count(out, "\n") != 1 {
		t.Errorf("lookups --place-type roastery: %q, want B alone", out)
	}

	// Jev's website read says café on B: Instagram still decides, and the report marks it.
	if err := leadstoretest.Q(t).SetPlaceType(context.Background(), s.B, leadstore.PlaceCafe, leadstore.PlaceFromJev,
		leadrate.PlaceTypeQuestion, make([]byte, 32), leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	tables := igTables(t)
	report := mustRun(t, "leads", "ig", "report", "--campaign", "cafes-loyalty")
	t.Logf("leads ig report:\n%s", report)
	wantInOrder(t, "report", report,
		fmt.Sprintf("  lead %d · Harbour Lane Coffee · Exampleton · @harbour_lane.example · now cafe (instagram) · IG place cafe_seating", s.A),
		fmt.Sprintf("  lead %d · Bean Example House · Springfield · @bean_house.example · now roastery (instagram) · IG place roastery_or_online", s.B),
		"  released by Instagram: 1 (its café read overrides a roastery read: these get café outreach)\n",
		fmt.Sprintf("    lead %d · Harbour Lane Coffee · Exampleton · @harbour_lane.example · café from Instagram, overriding a roastery from its name\n", s.A),
		"  set by Instagram: 1 (wrong? \"It's a coffee shop\" / \"It's a roastery\" on the lead's page)\n",
		fmt.Sprintf("    lead %d · Bean Example House · Springfield · @bean_house.example · roastery · Jev's website read says café\n", s.B),
		"  to correct: 0 (Instagram disagrees with your call; nothing was changed)\n",
		"  not applied: 0 (",
		"  switch pitch: 1")

	// The operator's call wins: B is a coffee shop by his word, a sync writes nothing over it,
	// and the report lists B to correct, no longer as set by Instagram.
	if err := leadstoretest.Q(t).SetPlaceType(context.Background(), s.B, leadstore.PlaceCafe, leadstore.PlaceFromOperator, "", nil,
		leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	wantLines(t, "sync after the operator's call", mustRun(t, "leads", "ig", "place-types"), "  place types from Instagram: roastery 0 · café 0 · silent 0")
	if got := igPlaceOf(t, s.B); got != "cafe/operator" {
		t.Errorf("B after the operator's call: %s", got)
	}
	report = mustRun(t, "leads", "ig", "report")
	wantInOrder(t, "report after the operator's call", report,
		"  released by Instagram: 1 (",
		"  set by Instagram: 0 (",
		"  to correct: 1 (Instagram disagrees with your call; nothing was changed)\n",
		fmt.Sprintf("    lead %d · Bean Example House · Springfield · @bean_house.example · Instagram reads a roastery or online shop; you called it a coffee shop\n", s.B),
		"  not applied: 0 (")
	if got := igTables(t); got != tables {
		t.Errorf("the report wrote: %s, was %s", got, tables)
	}
}

// TestLeadsIGPlaceTypeFollowsOperatorCalls: the type moves with the handle. The operator's
// unmatch of a roastery read releases its lead ("a café again"), his match of it to a lead holds
// that lead ("now a roastery, from Instagram's read"), his match of it to another lead moves the
// hold there and releases the first, and the console's Match says the same. An unmatch on a
// roastery-named lead Instagram read as a café gives it back to its name.
func TestLeadsIGPlaceTypeFollowsOperatorCalls(t *testing.T) {
	s := newIGScene(t)
	igStubJev(t, igSceneFake(t))
	igRoasterName(t, s.A)
	igImport(t, []string{igProfileLine(igA), igProfileLine(igB)})
	if held := igHeld(t, s.A, s.B); held[s.A] || !held[s.B] {
		t.Fatalf("held after the import: %v, want only B", held)
	}
	ops := func(args ...string) string { return mustRun(t, append([]string{"leads", "ig"}, args...)...) }
	b, d := strconv.FormatInt(s.B, 10), strconv.FormatInt(s.D, 10)

	out := ops("unmatch", "bean_house.example")
	wantInOrder(t, "unmatch", out, "leads ig unmatch: @bean_house.example matches no lead", fmt.Sprintf("\n  lead %d is a café again\n", s.B))
	if held := igHeld(t, s.B); held[s.B] || igPlaceOf(t, s.B) != "cafe/" {
		t.Errorf("B after the unmatch: %s, held %v", igPlaceOf(t, s.B), held)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM lead_place_type WHERE lead_id = %d AND source = 'instagram'
		AND place_type = 'unclear' AND detail = 'no strong match'`, s.B)); n != 1 {
		t.Errorf("B's silent rows after the unmatch: %d, want 1", n)
	}
	wantLines(t, "unmatch again", ops("unmatch", "bean_house.example"), "matches no lead")
	if strings.Contains(ops("unmatch", "bean_house.example"), "café again") {
		t.Error("a repeated unmatch says the type moved again")
	}

	out = ops("match", "bean_house.example", b)
	wantInOrder(t, "match", out, fmt.Sprintf("leads ig match: @bean_house.example is lead %d now", s.B),
		fmt.Sprintf("\n  lead %d is now a roastery, from Instagram's read\n", s.B))
	if !igHeld(t, s.B)[s.B] {
		t.Error("B is not held after the operator's match of a roastery read")
	}

	// A re-match to D: B is released (listed first), D held.
	wantInOrder(t, "re-match", ops("match", "bean_house.example", d),
		fmt.Sprintf("\n  lead %d is a café again\n", s.B), fmt.Sprintf("  lead %d is now a roastery, from Instagram's read\n", s.D))
	if held := igHeld(t, s.B, s.D); held[s.B] || !held[s.D] {
		t.Errorf("held after the re-match: %v, want D alone", held)
	}

	// The console's Match names the move in its message.
	ops("unmatch", "bean_house.example")
	msg, err := leadsBackend(t).InstagramAct(context.Background(), console.IGAction{Do: console.IGMatch, Handle: "bean_house.example", LeadID: s.B})
	if want := fmt.Sprintf(" Lead %d is now a roastery, from Instagram's read.", s.B); err != nil ||
		!strings.HasPrefix(msg, fmt.Sprintf("@bean_house.example is lead %d now; ", s.B)) || !strings.HasSuffix(msg, want) {
		t.Errorf("the console's match: %q, %v; want it to end %q", msg, err, want)
	}
	if !igHeld(t, s.B)[s.B] {
		t.Error("B is not held after the console's match")
	}

	// A, roastery-named, a café by Instagram's read: the unmatch gives it back to its name.
	wantLines(t, "unmatch A", ops("unmatch", igA.handle), fmt.Sprintf("  lead %d is a roastery again, from its name", s.A))
	if got := igPlaceOf(t, s.A); got != "roastery/name" || !igHeld(t, s.A)[s.A] {
		t.Errorf("A after the unmatch: %s", got)
	}
}

// TestLeadsIGPlaceTypesVerb: `leads ig place-types` is the backfill. On a store whose matches
// and real answers predate the sync (the fake decider's import, which writes no instagram place
// row, then real Jev's answers), its dry run and the import's dry run say what would be written
// and write nothing; the run writes it, with the release listed first; a rerun writes nothing.
// It reads no capture and calls no model.
func TestLeadsIGPlaceTypesVerb(t *testing.T) {
	s := newIGScene(t)
	fake := igSceneFake(t)
	igRoasterName(t, s.A)
	lines := []string{igProfileLine(igA), igProfileLine(igB)}
	wantLines(t, "fake import", igImport(t, lines), "· decider fake ·", "  place types from Instagram: roastery 0 · café 0 · silent 0")
	if n := igPlaceRows(t); n != 0 {
		t.Fatalf("the fake decider's import wrote %d instagram place rows", n)
	}
	if got := igPlaceOf(t, s.B); got != "cafe/" {
		t.Errorf("B after the fake's roastery read: %s, want the café default", got)
	}

	// Real Jev's answers on the newest captures, as an earlier engine stored them.
	conf := 0.9
	for h, choice := range map[string]string{igA.handle: leadrate.IGCafeSeating, igB.handle: leadrate.IGRoasteryOrOnline} {
		id := int64(dbCount(t, `SELECT max(id) FROM ig_profile WHERE handle = '`+h+`'`))
		if _, err := leadstoretest.Q(t).AddIGJudgment(context.Background(), leadstore.IGJudgment{ProfileID: id, Question: leadstore.IGPlace,
			Choice: choice, RawChoice: choice, Confidence: &conf, Source: leadstore.IGFromJev, Model: "jev-test-1",
			InputSHA: bytes.Repeat([]byte{0x1e}, 32)}); err != nil {
			t.Fatal(err)
		}
	}
	calls := fake.Calls()

	dry := mustRun(t, "leads", "ig", "place-types", "--dry-run")
	wantInOrder(t, "dry run", dry, "leads ig place-types --dry-run: nothing written; no model called, no capture read\n",
		"  place types from Instagram: would write roastery 1 · café 1 · silent 0\n",
		fmt.Sprintf("    would release: lead %d: café from Instagram, was a roastery from its name\n", s.A),
		fmt.Sprintf("    would hold: lead %d: roastery from Instagram, was a café by default\n", s.B))
	if n := igPlaceRows(t); n != 0 {
		t.Errorf("the dry run wrote %d rows", n)
	}
	wantLines(t, "import dry run", mustRun(t, "leads", "ig", "import", "--dry-run", igFile(t, lines...)),
		"  place types from Instagram: would write roastery 1 · café 1 · silent 0 (from what is stored now, before this file)",
		fmt.Sprintf("    would hold: lead %d: roastery from Instagram, was a café by default", s.B))
	if n := igPlaceRows(t); n != 0 {
		t.Errorf("the import's dry run wrote %d rows", n)
	}

	out := mustRun(t, "leads", "ig", "place-types")
	t.Logf("leads ig place-types:\n%s", out)
	wantInOrder(t, "cold run", out, "leads ig place-types: synced from what is stored; no model called, no capture read\n",
		"  place types from Instagram: roastery 1 · café 1 · silent 0\n",
		fmt.Sprintf("    released: lead %d: café from Instagram, was a roastery from its name\n", s.A),
		fmt.Sprintf("    held: lead %d: roastery from Instagram, was a café by default\n", s.B))
	if n := igPlaceRows(t); n != 2 {
		t.Errorf("the run wrote %d rows, want 2", n)
	}
	if held := igHeld(t, s.A, s.B); held[s.A] || !held[s.B] {
		t.Errorf("held after the run: %v, want only B", held)
	}
	rerun := mustRun(t, "leads", "ig", "place-types")
	wantLines(t, "rerun", rerun, "  place types from Instagram: roastery 0 · café 0 · silent 0")
	if strings.Contains(rerun, "released:") || strings.Contains(rerun, "held:") || igPlaceRows(t) != 2 {
		t.Errorf("the rerun wrote (%d rows):\n%s", igPlaceRows(t), rerun)
	}
	if fake.Calls() != calls {
		t.Errorf("place-types called the decider %d times", fake.Calls()-calls)
	}
	wantLines(t, "place-types report", mustRun(t, "leads", "place-types", "report"), "  roasteries: 1, of them 0 from a name match, 1 from Instagram")
	if code, _, errOut := leadsRun(t, "leads", "ig", "place-types", "extra"); code != 2 || !strings.Contains(errOut, "place-types [--dry-run]") {
		t.Errorf("place-types with an argument: exit %d, %q", code, errOut)
	}
}
