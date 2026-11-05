//go:build integration

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/igsignal"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Every handle, name and link here is invented (example/exemplu handles, .example hosts).

// igScene is cafeScene's four café leads, by name: Harbour Lane Coffee (A, Exampleton,
// holding the Instagram route harbour_lane.example), Bean Example House (B, bean-house.example),
// Cafeneaua Exemplu (C, Orasul Exemplu) and Facebook Only Example (D, no website).
type igScene struct{ A, B, C, D int64 }

func newIGScene(t *testing.T) igScene {
	t.Helper()
	cafeScene(t)
	s := igScene{A: leadIDOf(t, "harbour-lane-coffee.example"), B: leadIDOf(t, "bean-house.example"),
		C: leadIDOf(t, "cafeneaua-exemplu.example"), D: placeLeadID(t, "Facebook Only Example")}
	if _, err := leadstoretest.Q(t).InsertRoute(context.Background(), leadstore.RouteRow{OrgID: igOrgOf(t, s.A),
		Kind: leadstore.RouteInstagram, Value: "harbour_lane.example", DerivedBy: leadstore.DerivedByInstagram}); err != nil {
		t.Fatal(err)
	}
	return s
}

func igOrgOf(t *testing.T, lead int64) int64 {
	t.Helper()
	return int64(dbCount(t, `SELECT org_id FROM lead WHERE id = `+strconv.FormatInt(lead, 10)))
}

// igP is one profile record to write.
type igP struct {
	handle, name, category, bio, link, outcome, source, of, at string
	captions                                                   []string
	leadID                                                     int64
	private                                                    bool
	wall                                                       string
}

func igProfileLine(p igP) string {
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	src := map[string]any{"type": p.source, "of": nul(p.of), "lead_id": nil}
	if p.source == "" {
		src["type"] = "manual"
	}
	if p.leadID != 0 {
		src["lead_id"] = p.leadID
	}
	outcome := p.outcome
	if outcome == "" {
		outcome = "ok"
		if p.private {
			outcome = "private"
		}
	}
	captions := p.captions
	if captions == nil {
		captions = []string{}
	}
	m := map[string]any{"v": 1, "kind": "profile", "handle": p.handle, "url": enrich.InstagramURL(p.handle),
		"captured_at": cmp.Or(p.at, "2026-11-05T21:00:00Z"), "source": src, "outcome": outcome, "ig_user_id": nil,
		"full_name": nul(p.name), "category": nul(p.category), "bio": nul(p.bio), "external_url": nul(p.link),
		"followers": 100, "following": 50, "posts": 10, "is_private": p.private, "captions": captions, "wall": nul(p.wall)}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// igFollowLine is one following record of owner keeping kept (handle, name pairs).
func igFollowLine(owner, at string, kept ...[2]string) string {
	ks := []map[string]any{}
	for _, k := range kept {
		ks = append(ks, map[string]any{"handle": k[0], "full_name": k[1]})
	}
	b, err := json.Marshal(map[string]any{"v": 1, "kind": "following", "of": owner, "captured_at": cmp.Or(at, "2026-11-05T21:30:00Z"),
		"filter": igsignal.CoffeeWordsVersion, "seen": 100 + len(kept), "kept": ks, "complete": true, "wall": nil})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func igFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return path
}

// igStateOf is the state Jev reads for the profile on line.
func igStateOf(t *testing.T, line string) string {
	t.Helper()
	recs, errs := igsignal.Parse(strings.NewReader(line))
	if len(errs) != 0 || len(recs) != 1 || recs[0].Profile == nil {
		t.Fatalf("line does not parse as one profile: %v", errs)
	}
	return leadrate.IGState(igProfileRow(*recs[0].Profile, recs[0].SHA256))
}

func igAns(choice string, conf float64) ai.Answer { return ai.Answer{Choice: choice, Confidence: conf} }

// igStub puts f in place of the import's fake decider for the test.
func igStub(t *testing.T, f *ai.Fake) *ai.Fake {
	t.Helper()
	saved := igFake
	igFake = func() ai.Decider { return f }
	t.Cleanup(func() { igFake = saved })
	return f
}

// igDefaultFake answers every profile a seated café with no scheme seen, sure of it.
func igDefaultFake() *ai.Fake {
	return &ai.Fake{Answers: map[string]ai.Answer{leadrate.IGPlace: igAns(leadrate.IGCafeSeating, 0.9),
		leadrate.IGLoyalty: igAns(leadrate.IGNoneSeen, 0.9), leadrate.IGVendor: igAns(leadrate.IGNoVendor, 0.9)},
		ByState: map[string]map[string]ai.Answer{}}
}

// The scene's capture, record by record.
var (
	igA = igP{handle: "harbour_lane.example", name: "Harbour Lane Coffee", category: "Coffee shop", bio: "Specialty coffee in Exampleton",
		captions: []string{"Stamp card with @stampino.app"}, source: "lead", leadID: 1}
	igB = igP{handle: "bean_house.example", name: "Bean House Ex", category: "Coffee roaster", bio: "We roast and ship beans",
		link: "https://bean-house.example/menu"}
	igC = igP{handle: "cafeneaua.exemplu", name: "Cafeneaua Exemplu", category: "Cafe", bio: "Cafea in Orasul Exemplu"}
	igE = igP{handle: "newcafe.example", name: "New Cafe Example", category: "Cafe", bio: "Coffee and cake, come and sit"}
	igF = igP{handle: "notcoffee.example", name: "Shoe Coffee Example", category: "Shoe store", bio: "Shoes for every season"}
	igL = igP{handle: "lowconf.example", name: "Lowconf Example", category: "Cafe", bio: "Hard to say what this is"}
	igM = igP{handle: "gone.example", outcome: "missing", source: "search"}
	igV = igP{handle: "private.example", name: "Private Example", private: true}
	igW = igP{handle: "wall.example", outcome: "missing", wall: "login form"}
)

// igSceneLines is the scene's capture: a handle match (A, a vendor tag), a domain match (B),
// a weak name match (C), an unmatched café (E), an unmatched shop (F), a low-confidence
// answer (L), a missing and a private handle, a wall, the vendor's following list (an
// account never read, and A) and another owner's list keeping F.
func igSceneLines(at string) []string {
	ps := []igP{igA, igB, igC, igE, igF, igL, igM, igV, igW}
	out := make([]string, 0, len(ps)+2)
	for _, p := range ps {
		if at != "" {
			p.at = at
		}
		out = append(out, igProfileLine(p))
	}
	fat := ""
	if at != "" {
		fat = at
	}
	return append(out,
		igFollowLine("stampino.app", fat, [2]string{"roast_follow.example", "Follow Roast Example"}, [2]string{"harbour_lane.example", "Harbour Lane Coffee"}),
		igFollowLine("list_owner.example", fat, [2]string{"notcoffee.example", "Shoe Coffee Example"}))
}

// igSceneFake is the scene's Jev: A runs Stampino, B is a roastery, F is no coffee place and
// L is answered below the threshold; the rest are the default seated café.
func igSceneFake(t *testing.T) *ai.Fake {
	t.Helper()
	f := igDefaultFake()
	f.ByState[igStateOf(t, igProfileLine(igA))] = map[string]ai.Answer{leadrate.IGPlace: igAns(leadrate.IGCafeSeating, 0.9),
		leadrate.IGLoyalty: igAns(leadrate.IGRunsScheme, 0.9), leadrate.IGVendor: igAns("stampino", 0.8)}
	f.ByState[igStateOf(t, igProfileLine(igB))] = map[string]ai.Answer{leadrate.IGPlace: igAns(leadrate.IGRoasteryOrOnline, 0.9),
		leadrate.IGLoyalty: igAns(leadrate.IGNoneSeen, 0.9), leadrate.IGVendor: igAns(leadrate.IGNoVendor, 0.9)}
	f.ByState[igStateOf(t, igProfileLine(igF))] = map[string]ai.Answer{leadrate.IGPlace: igAns(leadrate.IGNotCoffee, 0.9),
		leadrate.IGLoyalty: igAns(leadrate.IGNoneSeen, 0.9), leadrate.IGVendor: igAns(leadrate.IGNoVendor, 0.9)}
	f.ByState[igStateOf(t, igProfileLine(igL))] = map[string]ai.Answer{leadrate.IGPlace: igAns(leadrate.IGCafeSeating, 0.4),
		leadrate.IGLoyalty: igAns(leadrate.IGNoneSeen, 0.4), leadrate.IGVendor: igAns(leadrate.IGNoVendor, 0.4)}
	return igStub(t, f)
}

// igImport runs leads ig import on lines and returns its output.
func igImport(t *testing.T, lines []string, extra ...string) string {
	t.Helper()
	return mustRun(t, append([]string{"leads", "ig", "import", igFile(t, lines...), "--campaign", "cafes-loyalty"}, extra...)...)
}

// igTables counts the rows of every Instagram table and the instagram routes.
func igTables(t *testing.T) string {
	t.Helper()
	var parts []string
	for _, tbl := range []string{"ig_profile", "ig_follow", "ig_match", "ig_judgment", "ig_signal_hint", "ig_review_status"} {
		parts = append(parts, fmt.Sprintf("%s %d", tbl, dbCount(t, `SELECT count(*) FROM `+tbl)))
	}
	parts = append(parts, fmt.Sprintf("routes %d", dbCount(t, `SELECT count(*) FROM contact_route WHERE kind = 'instagram'`)))
	return strings.Join(parts, ", ")
}

func igFor(t *testing.T, lead int64) *leadstore.IGLead {
	t.Helper()
	ig, err := leadstoretest.Q(t).IGForLead(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	return ig
}

func igStatus(t *testing.T, handle string) string {
	t.Helper()
	items, err := leadstoretest.Q(t).IGReview(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Handle == handle {
			return it.Status
		}
	}
	return ""
}

func igReviewItem(t *testing.T, handle string) *leadstore.IGReviewItem {
	t.Helper()
	items, err := leadstoretest.Q(t).IGReview(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Handle == handle {
			return &it
		}
	}
	return nil
}

func igChoices(ig *leadstore.IGLead) string {
	if ig == nil {
		return "none"
	}
	j := ig.Judgments
	return fmt.Sprintf("%s/%s/%s(%s)", j[leadstore.IGPlace].Choice, j[leadstore.IGLoyalty].Choice, j[leadstore.IGVendor].Choice, j[leadstore.IGVendor].Source)
}

// TestLeadsIGColdStartAndReimport: the first import stores every record shape, matches each
// profile, asks Jev once per profile with text, writes the domain match's route, queues the
// weak match, the unmatched café and the never-read following account, and keeps follow names
// only for matched or reviewed accounts. A second identical import stores nothing and calls
// Jev zero times.
func TestLeadsIGColdStartAndReimport(t *testing.T) {
	s := newIGScene(t)
	fake := igSceneFake(t)
	out := igImport(t, igSceneLines(""))
	wantLines(t, "first import", out,
		"· decider fake · 11 records · 1 wall · 0 lines refused",
		"  profiles: 8 new · 0 already imported · 1 missing · 1 private",
		"  following lists: 2 (0 cut short) · 3 accounts kept · 3 rows new · 2 hints new",
		"  matches: handle 1 · domain 1 · name 1 · tie 0 · none 5 · the operator's call kept 0 · 8 rows written",
		"  jev: calls 6 · unchanged 0 · no text 2 · failed 0 · still due 0 · stop none · vendor by rule 1",
		"  review: 3 added · 0 marked matched",
		"  routes: 1 added")
	if fake.Calls() != 6 {
		t.Errorf("%d decider calls, want 6", fake.Calls())
	}
	for _, h := range []string{"harbour_lane.example", "bean_house.example", "cafeneaua.exemplu", "newcafe.example", "lowconf.example"} {
		if strings.Contains(out, h) {
			t.Errorf("the import printed the handle %s:\n%s", h, out)
		}
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_profile WHERE handle = 'wall.example'`); n != 0 {
		t.Errorf("the wall's record was stored (%d rows)", n)
	}

	// A: handle match, the vendor by rule outranks Jev's, Jev's place and loyalty stand.
	if got := igChoices(igFor(t, s.A)); got != "cafe_seating/runs_scheme/stampino(rule)" {
		t.Errorf("A's card: %s", got)
	}
	if ig := igFor(t, s.A); ig.Match.Tier != leadstore.IGTierHandle || len(ig.Hints) != 1 || ig.Hints[0].Value != "stampino" {
		t.Errorf("A's match %+v, hints %+v", ig.Match, ig.Hints)
	}
	// B: domain match, a route written from no page.
	if ig := igFor(t, s.B); ig == nil || ig.Match.Tier != leadstore.IGTierDomain || ig.Judgments[leadstore.IGPlace].Choice != leadrate.IGRoasteryOrOnline {
		t.Errorf("B's card: %+v", ig)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d AND kind = 'instagram'
		AND value = 'https://www.instagram.com/bean_house.example/' AND derived_by = 'instagram@1' AND fetch_id IS NULL`, igOrgOf(t, s.B))); n != 1 {
		t.Errorf("B's org has %d instagram@1 routes, want 1", n)
	}
	// A already held its route: no second one.
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d AND kind = 'instagram'`, igOrgOf(t, s.A))); n != 1 {
		t.Errorf("A's org has %d instagram routes, want its own 1", n)
	}
	// C: a weak match writes no route and shows no signals; it waits with its candidate.
	if ig := igFor(t, s.C); ig != nil {
		t.Errorf("the weak match shows signals on C: %+v", ig)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d AND kind = 'instagram'`, igOrgOf(t, s.C))); n != 0 {
		t.Errorf("the weak match wrote %d routes", n)
	}
	if it := igReviewItem(t, "cafeneaua.exemplu"); it == nil || it.Status != "new" || len(it.Candidates) != 1 || it.Candidates[0] != s.C {
		t.Errorf("C's review row: %+v", it)
	}
	for h, want := range map[string]string{"newcafe.example": "new", "roast_follow.example": "new", "notcoffee.example": "",
		"lowconf.example": "", "gone.example": "", "private.example": "", "harbour_lane.example": "", "bean_house.example": ""} {
		if got := igStatus(t, h); got != want {
			t.Errorf("%s review status %q, want %q", h, got, want)
		}
	}
	// L: below the threshold, kept as unclear with the raw answer beside it.
	if n := dbCount(t, `SELECT count(*) FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id WHERE p.handle = 'lowconf.example'
		AND j.question = 'ig_place' AND j.choice = 'unclear' AND j.raw_choice = 'cafe_seating' AND j.confidence < 0.6`); n != 1 {
		t.Errorf("lowconf's place rows as written: %d, want 1", n)
	}
	// The missing and the private handle: stored, no Jev, unclear by rule.
	for _, h := range []string{"gone.example", "private.example"} {
		if n := dbCount(t, `SELECT count(*) FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id WHERE p.handle = '`+h+`'
			AND j.source = 'rule' AND j.choice = 'unclear'`); n != 3 {
			t.Errorf("%s: %d unclear rule rows, want 3", h, n)
		}
		if n := dbCount(t, `SELECT count(*) FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id WHERE p.handle = '`+h+`' AND j.source = 'jev'`); n != 0 {
			t.Errorf("%s: %d Jev rows", h, n)
		}
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_profile WHERE handle = 'private.example' AND outcome = 'ok' AND is_private`); n != 1 {
		t.Errorf("the private capture is stored %d times as an ok private read", n)
	}
	// Names: kept for the matched and the reviewed account, not for the read shop.
	for h, want := range map[string]int{"roast_follow.example": 1, "harbour_lane.example": 1, "notcoffee.example": 0} {
		if n := dbCount(t, `SELECT count(*) FROM ig_follow WHERE handle = '`+h+`' AND full_name IS NOT NULL`); n != want {
			t.Errorf("%s: %d follow rows with a name, want %d", h, n, want)
		}
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_signal_hint WHERE value = 'stampino'`); n != 2 {
		t.Errorf("%d followed_by_vendor hints, want 2", n)
	}

	before, calls := igTables(t), fake.Calls()
	out = igImport(t, igSceneLines(""))
	wantLines(t, "identical re-import", out,
		"  profiles: 0 new · 8 already imported",
		"  jev: calls 0 · unchanged 6 · no text 2",
		"0 rows written", "  review: 0 added", "  routes: 0 added", "· 0 rows new · 0 hints new")
	if got := igTables(t); got != before || fake.Calls() != calls {
		t.Errorf("the re-import stored %s (was %s) and called %d times", got, before, fake.Calls()-calls)
	}
}

// TestLeadsIGRecapture: the same profiles captured again with unchanged text and a new
// captured_at are new rows that cost zero calls, and every card's judgments stay the same.
func TestLeadsIGRecapture(t *testing.T) {
	s := newIGScene(t)
	fake := igSceneFake(t)
	igImport(t, igSceneLines(""))
	cards := igChoices(igFor(t, s.A)) + " " + igChoices(igFor(t, s.B))
	calls := fake.Calls()
	out := igImport(t, igSceneLines("2026-11-06T09:00:00Z"))
	wantLines(t, "recapture", out, "  profiles: 8 new · 0 already imported", "  jev: calls 0 · unchanged 6")
	if fake.Calls() != calls {
		t.Errorf("the recapture called Jev %d times", fake.Calls()-calls)
	}
	if got := igChoices(igFor(t, s.A)) + " " + igChoices(igFor(t, s.B)); got != cards {
		t.Errorf("cards after the recapture %s, were %s", got, cards)
	}
	if ig := igFor(t, s.A); ig.Profile == nil || ig.Profile.CapturedAt.Format("2006-01-02") != "2026-11-06" {
		t.Errorf("A's card does not show the newest capture: %+v", ig.Profile)
	}
	// A changed text is asked about again.
	changed := igB
	changed.bio, changed.at = "Now with a small bar to sit at", "2026-11-07T09:00:00Z"
	if out := igImport(t, []string{igProfileLine(changed)}); !strings.Contains(out, "  jev: calls 1 · unchanged 0") {
		t.Errorf("a changed bio:\n%s", out)
	}
}

// TestLeadsIGWrongSourceLeadID: a capture's source.lead_id is never a match: a profile naming
// lead A as its source but matching nothing attaches to nobody.
func TestLeadsIGWrongSourceLeadID(t *testing.T) {
	s := newIGScene(t)
	igStub(t, igDefaultFake())
	stray := igP{handle: "stray.example", name: "Stray Example", bio: "Coffee", source: "lead", leadID: s.A}
	igImport(t, []string{igProfileLine(stray)})
	if ig := igFor(t, s.A); ig != nil {
		t.Errorf("the hint attached to A: %+v", ig.Match)
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_match WHERE handle = 'stray.example' AND lead_id IS NOT NULL`); n != 0 {
		t.Errorf("%d matches naming a lead", n)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM ig_profile WHERE handle = 'stray.example' AND source_lead_id = %d`, s.A)); n != 1 {
		t.Errorf("the hint is not kept for the report (%d rows)", n)
	}
}

// TestLeadsIGOwnVendorNeverReviewed: a café on the operator's own loyalty product is a
// customer and a vendor's own account is no café: neither reaches the review list, though
// both read as seated cafés.
func TestLeadsIGOwnVendorNeverReviewed(t *testing.T) {
	newIGScene(t)
	igStub(t, igDefaultFake())
	own := igP{handle: "own_customer.example", name: "Own Customer Example", category: "Cafe", bio: "Coffee shop",
		link: "https://loyalty-own.example/c/own"}
	vendor := igP{handle: "stampino.app", name: "Stampino Example", category: "App", bio: "Loyalty cards for coffee shops"}
	out := igImport(t, []string{igProfileLine(own), igProfileLine(vendor),
		igFollowLine("list_owner.example", "", [2]string{"stampino.app", "Stampino Coffee Example"})})
	wantLines(t, "own vendor", out, "  review: 0 added", "vendor by rule 1")
	if n := dbCount(t, `SELECT count(*) FROM ig_review_status`); n != 0 {
		t.Errorf("%d review rows, want none", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id
		WHERE p.handle = 'own_customer.example' AND j.source = 'rule' AND j.choice = 'webbership'`); n != 1 {
		t.Errorf("the own vendor's rule row: %d, want 1", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_follow WHERE handle = 'stampino.app' AND full_name IS NOT NULL`); n != 0 {
		t.Errorf("the vendor's account kept its name")
	}
}

// TestLeadsIGMaxCalls: --max-calls stops the calls cleanly and says so; the rest is still
// due, and the next import of the same file asks only those.
func TestLeadsIGMaxCalls(t *testing.T) {
	newIGScene(t)
	fake := igStub(t, igDefaultFake())
	lines := []string{igProfileLine(igC), igProfileLine(igE), igProfileLine(igL)}
	out := igImport(t, lines, "--max-calls", "1")
	wantLines(t, "--max-calls 1", out, "  jev: calls 1 · unchanged 0 · no text 0 · failed 0 · still due 2 · stop budget",
		"  stopped at --max-calls 1: 2 profiles still due; import the same file again to ask them",
		"  profiles: 3 new")
	if fake.Calls() != 1 {
		t.Errorf("%d calls, want 1", fake.Calls())
	}
	if n := dbCount(t, `SELECT count(*) FROM run WHERE command = 'leads-ig-import' AND stop_reason = 'budget' AND requests = 1`); n != 1 {
		t.Errorf("the stopped run as recorded: %d", n)
	}
	out = igImport(t, lines, "--max-calls", "5")
	wantLines(t, "rerun", out, "  profiles: 0 new · 3 already imported", "  jev: calls 2 · unchanged 1 · no text 0 · failed 0 · still due 0 · stop none")
	if out := igImport(t, lines, "--max-calls", "0"); !strings.Contains(out, "  jev: calls 0 · unchanged 3") {
		t.Errorf("--max-calls 0 after all are asked:\n%s", out)
	}
	if code, _, errOut := leadsRun(t, "leads", "ig", "import", igFile(t, lines...), "--max-calls", "-1"); code == 0 || !strings.Contains(errOut, "leads ig import FILE") {
		t.Errorf("--max-calls -1: exit %d, %q", code, errOut)
	}
}

// TestLeadsIGSpendCap: the leads cap stops the calls cleanly, says so, and records the stop.
func TestLeadsIGSpendCap(t *testing.T) {
	newIGScene(t)
	fake := igStub(t, igDefaultFake())
	t.Setenv("LEADHOUND_JEV_LEADS_DAILY_CALL_CAP", "1")
	out := igImport(t, []string{igProfileLine(igC), igProfileLine(igE), igProfileLine(igL)})
	wantLines(t, "cap 1", out, "  jev: calls 1 · unchanged 0 · no text 0 · failed 0 · still due 2 · stop spend_cap",
		"  stopped: the leads cap of 1 calls a day is spent; 2 profiles still due")
	if fake.Calls() != 1 {
		t.Errorf("%d decider calls, want 1", fake.Calls())
	}
	if n := dbCount(t, `SELECT count(*) FROM run WHERE command = 'leads-ig-import' AND stop_reason = 'spend_cap'`); n != 1 {
		t.Errorf("the spend-cap run as recorded: %d", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_profile`); n != 3 {
		t.Errorf("%d profiles stored after the stop, want all 3", n)
	}
}

// TestLeadsIGStatusGrid: the operator's calls on the review list and on matches, against
// later imports. A re-import never changes a dismissed, matched or wanted status; an operator
// match survives a later domain match; an unmatch is never undone by a name match; a repeated
// call writes nothing; a handle in no capture is refused.
func TestLeadsIGStatusGrid(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	igImport(t, igSceneLines(""))
	ops := func(args ...string) string { return mustRun(t, append([]string{"leads", "ig"}, args...)...) }

	wantLines(t, "dismiss", ops("dismiss", "newcafe.example"), "leads ig dismiss: @newcafe.example is dismissed")
	wantLines(t, "dismiss again", ops("dismiss", "newcafe.example"), "is already dismissed; nothing written")
	wantLines(t, "want", ops("want", "@Roast_Follow.example"), "leads ig want: @roast_follow.example is wanted")
	wantLines(t, "match", ops("match", "cafeneaua.exemplu", strconv.FormatInt(s.C, 10)),
		fmt.Sprintf("leads ig match: @cafeneaua.exemplu is lead %d now (Instagram route added)", s.C))
	if ig := igFor(t, s.C); ig == nil || ig.Match.Tier != leadstore.IGTierOperator || ig.Match.Actor != leadstore.ActorGremy {
		t.Fatalf("C after the operator's match: %+v", ig)
	}
	statuses := func() string {
		return igStatus(t, "newcafe.example") + " " + igStatus(t, "roast_follow.example") + " " + igStatus(t, "cafeneaua.exemplu")
	}
	if got := statuses(); got != "dismissed wanted matched" {
		t.Fatalf("statuses %q", got)
	}

	// Re-imports (the same file and a recapture) leave every status as it is.
	igImport(t, igSceneLines(""))
	igImport(t, igSceneLines("2026-11-06T09:00:00Z"))
	if got := statuses(); got != "dismissed wanted matched" {
		t.Errorf("statuses after re-imports %q", got)
	}

	// A later capture of C's handle linking B's website: the operator's match stands.
	toB := igC
	toB.link, toB.at = "https://bean-house.example/", "2026-11-07T09:00:00Z"
	wantLines(t, "domain after operator", igImport(t, []string{igProfileLine(toB)}), "the operator's call kept 1 · 0 rows written")
	if ig := igFor(t, s.C); ig == nil || ig.Match.Tier != leadstore.IGTierOperator {
		t.Errorf("C lost the operator's match: %+v", ig)
	}

	// Unmatch: back on the review list, and a later name match does not re-attach it.
	wantLines(t, "unmatch", ops("unmatch", "cafeneaua.exemplu"), "leads ig unmatch: @cafeneaua.exemplu matches no lead; back on the review list")
	again := igC
	again.at = "2026-11-08T09:00:00Z"
	wantLines(t, "name after unmatch", igImport(t, []string{igProfileLine(again)}), "the operator's call kept 1 · 0 rows written")
	if ig := igFor(t, s.C); ig != nil {
		t.Errorf("C re-attached after the unmatch: %+v", ig.Match)
	}
	if got := igStatus(t, "cafeneaua.exemplu"); got != "new" {
		t.Errorf("C's handle after the unmatch is %q, want new", got)
	}

	// The grid's remaining moves on one handle: each status moves to the next call's.
	for _, step := range []struct{ verb, want string }{{"want", "wanted"}, {"dismiss", "dismissed"}, {"want", "wanted"}} {
		ops(step.verb, "newcafe.example")
		if got := igStatus(t, "newcafe.example"); got != step.want {
			t.Errorf("after %s: %q, want %q", step.verb, got, step.want)
		}
	}
	ops("match", "newcafe.example", strconv.FormatInt(s.D, 10))
	ops("unmatch", "newcafe.example")
	if got := igStatus(t, "newcafe.example"); got != "new" {
		t.Errorf("matched then unmatched: %q, want new", got)
	}

	for _, args := range [][]string{{"match", "nobody.example", "1"}, {"match", "cafeneaua.exemplu", "0"}, {"match", "cafeneaua.exemplu", "999999"},
		{"dismiss", "nobody.example"}, {"unmatch"}, {"want", "p"}, {"nope"}, {}} {
		code, _, errOut := leadsRun(t, append([]string{"leads", "ig"}, args...)...)
		if code != 2 || errOut == "" {
			t.Errorf("ig %q: exit %d, %q", args, code, errOut)
		}
	}
}

// igSnapshot is every row of lead, org, lead_place_type and lead_event, whole.
func igSnapshot(t *testing.T) string {
	t.Helper()
	var parts []string
	for _, tbl := range []string{"lead", "org", "lead_place_type", "lead_event"} {
		var s string
		if err := storetest.OwnerConn(t).QueryRow(context.Background(),
			`SELECT coalesce(md5(string_agg(row_to_json(x)::text, '|' ORDER BY x.id)), '') || ':' || count(*) FROM `+tbl+` x`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, tbl+" "+s)
	}
	return strings.Join(parts, ", ")
}

// TestLeadsIGNeverMovesALead: imports of every record shape (the parser's mixed testdata, the
// scene, a recapture) and the operator's calls change no lead, org, place type or event; a
// dry run writes nothing at all.
func TestLeadsIGNeverMovesALead(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	before := igSnapshot(t)
	tables := igTables(t)
	out := mustRun(t, "leads", "ig", "import", "--dry-run", "../../internal/igsignal/testdata/mixed.jsonl")
	wantLines(t, "dry run", out, "· 13 records · 2 walls · 0 lines refused · nothing written, nobody called")
	if got := igTables(t); got != tables {
		t.Errorf("the dry run wrote: %s, was %s", got, tables)
	}
	if n := dbCount(t, `SELECT count(*) FROM run WHERE command = 'leads-ig-import'`); n != 0 {
		t.Errorf("the dry run recorded %d runs", n)
	}
	mustRun(t, "leads", "ig", "import", "../../internal/igsignal/testdata/mixed.jsonl")
	igImport(t, igSceneLines(""))
	igImport(t, igSceneLines("2026-11-06T09:00:00Z"))
	mustRun(t, "leads", "ig", "match", "cafeneaua.exemplu", strconv.FormatInt(s.C, 10))
	mustRun(t, "leads", "ig", "unmatch", "harbour_lane.example")
	mustRun(t, "leads", "ig", "want", "newcafe.example")
	mustRun(t, "leads", "ig", "report")
	mustRun(t, "leads", "ig", "lookups")
	mustRun(t, "leads", "ig", "review", "--status", "all")
	if got := igSnapshot(t); got != before {
		t.Errorf("Instagram moved a lead:\n%s\nwas\n%s", got, before)
	}
}

// TestLeadsIGReadVerbs: report counts and lists the strong matches with the two sections the
// operator acts on; lookups prints one tab-separated line per café lead, filtered; review
// lists the queue with its candidates. None of them writes.
func TestLeadsIGReadVerbs(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	igImport(t, igSceneLines(""))
	tables := igTables(t)

	out := mustRun(t, "leads", "ig", "report", "--campaign", "cafes-loyalty")
	wantLines(t, "report", out,
		"leads ig report: campaign cafes-loyalty, 2 café leads with a strong Instagram match",
		"  latest match per handle: handle 1 · domain 1 · name 1 · tie 0 · yours 0 · unmatched by you 0 · none 5",
		"  place on matched leads: cafe_seating 1 · roastery_or_online 1",
		"  vendor on matched leads: none 1 · stampino 1",
		"  review: new 3 · dismissed 0 · matched 0 · wanted 0",
		fmt.Sprintf("  lead %d · Harbour Lane Coffee · Exampleton · @harbour_lane.example · now cafe · IG place cafe_seating · loyalty runs_scheme · vendor stampino", s.A),
		fmt.Sprintf("  lead %d · Bean Example House · Springfield · @bean_house.example · now cafe · IG place roastery_or_online · loyalty none_seen · vendor none", s.B),
		"  to correct: 1 (nothing was changed",
		fmt.Sprintf("    lead %d · Bean Example House · Springfield · @bean_house.example · Instagram reads a roastery or online shop; the lead is a coffee shop", s.B),
		"  switch pitch: 1 (already running a loyalty scheme)",
		fmt.Sprintf("    lead %d · Harbour Lane Coffee · Exampleton · @harbour_lane.example · loyalty runs_scheme · vendor stampino", s.A))

	out = mustRun(t, "leads", "ig", "lookups", "--campaign", "cafes-loyalty")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lookups: %d lines, want 4:\n%s", len(lines), out)
	}
	if want := fmt.Sprintf("%d\tHarbour Lane Coffee\tExampleton\tharbour-lane-coffee.example\tharbour_lane.example\tcafe\t", s.A); lines[0] != want {
		t.Errorf("lookups line 1 %q, want %q", lines[0], want)
	}
	if want := fmt.Sprintf("%d\tBean Example House\tSpringfield\tbean-house.example\tbean_house.example\tcafe\t", s.B); lines[1] != want {
		t.Errorf("lookups line 2 %q, want %q (the import's route is a handle it holds)", lines[1], want)
	}
	if want := fmt.Sprintf("%d\tFacebook Only Example\tExampleton\t\t\tcafe\t", s.D); lines[3] != want {
		t.Errorf("lookups line 4 %q, want %q", lines[3], want)
	}
	for _, args := range [][]string{{"--name-word"}, {"--place-type", "roastery"}, {"--place-type", "unclear"}} {
		if out := mustRun(t, append([]string{"leads", "ig", "lookups"}, args...)...); out != "" {
			t.Errorf("lookups %v printed %q, want nothing", args, out)
		}
	}
	ownerExec(t, `UPDATE org SET name = 'Fictive Hill Roasters' WHERE id = $1`, igOrgOf(t, s.D))
	if err := leadstoretest.Q(t).SetPlaceType(context.Background(), s.B, leadstore.PlaceUnclear, leadstore.PlaceFromJev,
		leadrate.PlaceTypeQuestion, make([]byte, 32), leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, "leads", "ig", "lookups", "--name-word"); !strings.HasPrefix(out, fmt.Sprintf("%d\tFictive Hill Roasters\t", s.D)) || strings.Count(out, "\n") != 1 {
		t.Errorf("--name-word: %q", out)
	}
	if out := mustRun(t, "leads", "ig", "lookups", "--place-type", "unclear"); !strings.HasPrefix(out, fmt.Sprintf("%d\t", s.B)) ||
		!strings.HasSuffix(out, "\tunclear\n") || strings.Count(out, "\n") != 1 {
		t.Errorf("--place-type unclear: %q", out)
	}
	if code, _, _ := leadsRun(t, "leads", "ig", "lookups", "--place-type", "cafe"); code != 2 {
		t.Errorf("--place-type cafe: exit %d, want 2", code)
	}

	out = mustRun(t, "leads", "ig", "review")
	wantLines(t, "review", out, "leads ig review: 3 handles at new",
		fmt.Sprintf("  @cafeneaua.exemplu · Cafeneaua Exemplu · Cafe · place cafe_seating · candidates %d", s.C),
		"  @newcafe.example · New Cafe Example · Cafe · place cafe_seating · candidates none",
		"  @roast_follow.example · Follow Roast Example · - · place - · candidates none")
	if out := mustRun(t, "leads", "ig", "review", "--status", "dismissed"); !strings.Contains(out, "0 handles at dismissed") {
		t.Errorf("review --status dismissed:\n%s", out)
	}
	if code, _, _ := leadsRun(t, "leads", "ig", "review", "--status", "old"); code != 2 {
		t.Errorf("review --status old: exit %d, want 2", code)
	}
	if got := igTables(t); got != tables {
		t.Errorf("the read verbs wrote: %s, was %s", got, tables)
	}
}

// TestLeadsIGDeciderDefault: without LEADHOUND_LIVE=1 the import uses the fake, and asking for
// Jev is refused before anything is read or written.
func TestLeadsIGDeciderDefault(t *testing.T) {
	newIGScene(t)
	igStub(t, igDefaultFake())
	path := igFile(t, igProfileLine(igE))
	code, _, errOut := leadsRun(t, "leads", "ig", "import", path, "--decider", "jev")
	if code != 2 || !strings.Contains(errOut, "LEADHOUND_LIVE=1") {
		t.Errorf("--decider jev without live: exit %d, %q", code, errOut)
	}
	if n := dbCount(t, `SELECT count(*) FROM ig_profile`); n != 0 {
		t.Errorf("a refused import stored %d profiles", n)
	}
	wantLines(t, "default", mustRun(t, "leads", "ig", "import", path), "· decider fake ·")
	// Erasing an org and importing again is CP4's test (leads_forget, the ForgetOrg sweep).
}
