//go:build integration

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// setLeadKind makes the lead on domain one of kind, whatever its probe says.
func setLeadKind(t *testing.T, domain, kind string) int64 {
	t.Helper()
	id := leadIDOf(t, domain)
	ownerExec(t, `UPDATE lead SET kind = $2 WHERE id = $1`, id, kind)
	return id
}

// rateUnderQuestion appends one rating of lead under question with score, as an earlier
// run would have written it.
func rateUnderQuestion(t *testing.T, lead int64, question string, score int, band string, sha []byte) {
	t.Helper()
	ctx := context.Background()
	runID, err := store.NewQueries(storetest.AppConn(t)).StartRun(ctx, commandLeadsRate, rateDeciderFake)
	if err != nil {
		t.Fatal(err)
	}
	if sha == nil {
		sum := leadrate.InputHash(fmt.Sprintf("seed-%d-%s-%d", lead, question, score), leadrate.Input{Kind: leadstore.LeadUnsupported})
		sha = sum
	}
	if _, err := leadstoretest.Q(t).InsertRelevance(ctx, leadstore.RelevanceRow{
		LeadID: lead, Question: question, Decider: rateDeciderFake, Model: rateDeciderFake, InputSHA256: sha,
		Band: band, Score: score, Probabilities: map[string]float64{band: 1}, RunID: runID,
	}); err != nil {
		t.Fatalf("rate lead %d under %s: %v", lead, question, err)
	}
}

// recordEval records a measured eval of question, trusted or not, as a jev eval would.
func recordEval(t *testing.T, question string, trusted bool) {
	t.Helper()
	if _, err := leadstoretest.Q(t).RecordRelevanceEval(context.Background(), leadstore.RelevanceEval{
		Question: question, Model: "jev-test", N: leadrate.TrustMinN, Exact: 0.6, WithinOne: 0.9, MAE: 12, Spearman: 0.7,
		Runs: 3, Trusted: trusted,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRatingTypeContractDrift: for every kind a campaign can make, the store's rating type
// (the SQL rtype the list, LeadsToRate and RateLeadByID read, and the type the calls are
// counted under) is leadstore.TypeOf's. A selftest lead is a Drupal lead to TypeOf and is
// never read by any of them.
func TestRatingTypeContractDrift(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	id := leadIDOf(t, "lucerne.example")
	band := leadstore.BandMaybe
	if _, err := lq.SetLeadLabel(ctx, id, &band, []byte(`{"kind":"unsupported"}`), leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	kinds := campaign.KnownKinds()
	if !slices.Contains(kinds, cafe.KindNoDigitalLoyalty) || !slices.Contains(kinds, leadstore.LeadAgencyPartner) {
		t.Fatalf("known kinds %v lack a café or the agency kind", kinds)
	}
	for _, k := range kinds {
		setLeadKind(t, "lucerne.example", k)
		want := leadstore.TypeOf(k)
		rl, err := lq.RateLeadByID(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		rows, _, err := lq.LeadList(ctx, leadstore.ListFilter{LeadIDs: []int64{id}, Status: leadstore.ListAll}, leadstore.ListContext{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: list %d rows, %v", k, len(rows), err)
		}
		open, err := lq.LeadsToRate(ctx, leadstore.ListContext{})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(open, func(l leadstore.RateLead) bool { return l.LeadID == id })
		if i < 0 {
			t.Fatalf("%s: lead not open", k)
		}
		if rl.RType != want || rows[0].RType != want || open[i].RType != want {
			t.Errorf("%s: rtype rate %q list %q open %q, TypeOf %q", k, rl.RType, rows[0].RType, open[i].RType, want)
		}
		for _, tp := range leadstore.RatingTypes {
			n, err := lq.LabelCount(ctx, tp)
			if err != nil {
				t.Fatal(err)
			}
			if wantN := map[bool]int{true: 1, false: 0}[tp == want]; n != wantN {
				t.Errorf("%s: %d calls of type %s, want %d", k, n, tp, wantN)
			}
		}
	}
	if leadstore.TypeOf(leadstore.LeadSelftest) != leadstore.TypeDrupal {
		t.Errorf("TypeOf(selftest) = %q", leadstore.TypeOf(leadstore.LeadSelftest))
	}
	setLeadKind(t, "lucerne.example", leadstore.LeadSelftest)
	if _, err := lq.RateLeadByID(ctx, id); err == nil {
		t.Error("a selftest lead was read for rating")
	}
	if rows, _, err := lq.LeadList(ctx, leadstore.ListFilter{LeadIDs: []int64{id}, Status: leadstore.ListAll}, leadstore.ListContext{}); err != nil || len(rows) != 0 {
		t.Errorf("a selftest lead is listed: %d rows, %v", len(rows), err)
	}
}

// pinLeadInDB makes the lead on domain carry exactly the facts of the pinned lead l
// (rateinput_pin_test.go), as the store reads them: its kind, country, probe version and
// site name, finding version, list rank, description, social links, form route and phone.
// The orderScene lead already has an email route.
func pinLeadInDB(t *testing.T, domain string, l leadstore.RateLead) int64 {
	t.Helper()
	if !l.HasEmail {
		t.Fatalf("%s: pinLeadInDB needs a pinned lead with email", domain)
	}
	id := setLeadKind(t, domain, l.Kind)
	ownerExec(t, `UPDATE org SET country = $2 WHERE id = (SELECT org_id FROM lead WHERE id = $1)`, id, l.Country)
	ownerExec(t, `UPDATE site_probe SET major = $2, exact_version = $3, site_name = $4 WHERE id = (SELECT probe_id FROM lead WHERE id = $1)`,
		id, l.Major, l.ExactVersion, l.SiteName)
	ownerExec(t, `UPDATE finding SET subject = subject || jsonb_build_object('major', $2::int, 'exact_version', $3::text)
		WHERE id = (SELECT finding_id FROM lead WHERE id = $1)`, id, l.Major, l.ExactVersion)
	ownerExec(t, `UPDATE site SET list_rank = $2 WHERE id = (SELECT p.site_id FROM lead x JOIN site_probe p ON p.id = x.probe_id WHERE x.id = $1)`,
		id, l.ListRank)
	fact := func(kind, value, normalized string) {
		ownerExec(t, `INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, derived_by)
			SELECT org_id, $2, $3, $4, (SELECT max(id) FROM http_fetch), 'pin-fixture' FROM lead WHERE id = $1`, id, kind, value, normalized)
	}
	if l.Description != "" {
		fact("description", l.Description, strings.ToLower(l.Description))
	}
	for i := range l.SocialLinks {
		u := fmt.Sprintf("https://social.example/%s/%d", domain, i)
		fact("social", u, u)
	}
	if l.HasPhone {
		fact("phone", "+44 20 7946 0000", "+442079460000")
	}
	if l.HasForm {
		ownerExec(t, `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by)
			SELECT org_id, 'form', $2, (SELECT max(id) FROM http_fetch), 'pin-fixture' FROM lead WHERE id = $1`, id, "https://"+domain+"/contact")
	}
	return id
}

// TestRateDrupalUntouched: a site lead and an agency lead, each stored with the facts of a
// pinned lead and a rating whose input hash is the pinned digest recorded before café leads
// had their own question, are unchanged to leads rate: nothing is asked and no row is
// written, while the other Drupal leads are rated.
//
// The pinned digests were recorded under the decider name pinRateDecider and the EOL facts
// of pinRateConfig. The input hash covers the decider's name, and `leads rate --decider
// fake` names its decider "fake", so the CLI can never see a pinned digest as unchanged.
// The test therefore runs the command's own rating (rateRun.rate, which runLeadsRate calls)
// over the leads LeadsToRate reads from the database, with the decider's name and the
// config the digests were recorded under: the store's read, rateInput and the hash are all
// the production path, and only the two values the pins fixed are supplied.
func TestRateDrupalUntouched(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	type pin struct {
		domain, hash string
		lead         leadstore.RateLead
	}
	onDomain := map[string]string{"unsupported": "lucerne.example", "agency_partner": "margie.example"}
	pins := map[string]pin{}
	for _, c := range pinRateLeads() {
		if d, ok := onDomain[c.name]; ok {
			pins[c.name] = pin{d, c.hash, c.lead}
		}
	}
	if len(pins) != 2 {
		t.Fatalf("pinned leads %v, want the unsupported and the agency one", pins)
	}
	pinned := map[int64]string{}
	for name, p := range pins {
		id := pinLeadInDB(t, p.domain, p.lead)
		sha, err := hex.DecodeString(p.hash)
		if err != nil {
			t.Fatal(err)
		}
		rateUnderQuestion(t, id, leadstore.QuestionDrupal, 75, leadstore.BandLikely, sha)
		pinned[id] = name
	}

	leads, err := lq.LeadsToRate(ctx, leadstore.ListContext{})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leads {
		name, ok := pinned[l.LeadID]
		if !ok {
			continue
		}
		in := rateInput(l, pinRateConfig())
		if got := hex.EncodeToString(leadrate.InputHash(pinRateDecider, in)); got != pins[name].hash {
			t.Fatalf("%s: the stored lead hashes %s, want the pin %s\nread  %+v\npinned %+v", name, got, pins[name].hash,
				in, rateInput(pins[name].lead, pinRateConfig()))
		}
	}
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, commandLeadsRate, rateDeciderFake)
	if err != nil {
		t.Fatal(err)
	}
	fake := relevanceFake()
	rr := rateRun{
		lq:      lq,
		decider: &ai.Capped{Decider: fake, Store: core, DailyCalls: 100, Loc: time.UTC, Purpose: ai.PurposeLeads},
		kind:    rateDeciderFake, name: pinRateDecider, runID: runID, max: 100, cfg: pinRateConfig(),
	}
	before := dbCount(t, `SELECT count(*) FROM lead_relevance`)
	sum, err := rr.rate(ctx, leads)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unchanged != 2 || sum.Rated != len(leads)-2 || sum.Calls != len(leads)-2 {
		t.Errorf("rate: %+v; want the 2 pinned leads unchanged and the other %d rated", sum, len(leads)-2)
	}
	for id, name := range pinned {
		if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM lead_relevance WHERE lead_id = %d`, id)); n != 1 {
			t.Errorf("%s: %d ratings, want only the seeded one", name, n)
		}
	}
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance`); n != before+len(leads)-2 {
		t.Errorf("%d ratings after, want %d", n, before+len(leads)-2)
	}
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance WHERE question <> 'relevance@1'`); n != 0 {
		t.Errorf("%d Drupal ratings under another question", n)
	}
}

// cafeScene is a fresh leads database with the demo café import probed and built into the
// cafes-loyalty campaign's four café leads (three on .example sites, one with no website);
// the Romanian café has an email route. It returns the campaign's id.
func cafeScene(t *testing.T) int64 {
	t.Helper()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	importDemoPlaces(t, dir)
	if out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", cafeFixtures(t)); !strings.Contains(out, "· 4 sites probed") {
		t.Fatalf("probe:\n%s", out)
	}
	cafeRoute(t, "cafeneaua-exemplu.example")
	wantLines(t, "café build", mustRun(t, "leads", "build", "--campaign", "cafes-loyalty"), "leads build: 4 leads (4 new")
	c, err := leadstoretest.Q(t).CampaignBySlug(context.Background(), leadstore.DefaultAccountID, "cafes-loyalty")
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// TestRateCafeColdStart: café leads never rated are each asked the café question once and
// stored under relevance-cafe@1, with their café facts read from the store (the place, the
// homepage's findings, the site language); a second run asks nothing.
func TestRateCafeColdStart(t *testing.T) {
	cafeScene(t)
	ctx := context.Background()
	out := mustRun(t, "leads", "rate", "--decider", "fake", "--campaign", "cafes-loyalty")
	wantLines(t, "first café rate", out, "· decider fake · rated 4 · unchanged 0 · stop none")
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance WHERE question = 'relevance-cafe@1' AND band = 'maybe' AND score = 50`); n != 4 {
		t.Errorf("%d café ratings, want 4", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance WHERE question <> 'relevance-cafe@1'`); n != 0 {
		t.Errorf("%d café leads rated under another question", n)
	}
	wantLines(t, "second café rate", mustRun(t, "leads", "rate", "--decider", "fake", "--campaign", "cafes-loyalty"),
		"· rated 0 · unchanged 4 · stop none")

	rl, err := leadstoretest.Q(t).RateLeadByID(ctx, leadIDOf(t, "cafeneaua-exemplu.example"))
	if err != nil {
		t.Fatal(err)
	}
	in := rateInput(rl, pinRateConfig())
	if c := in.Cafe; c == nil || !c.OwnWebsite || !c.OrderingSeen || c.Language != "ro" || c.Category == "" || c.Locality == "" {
		t.Errorf("the Romanian café's facts %+v from %+v", in.Cafe, rl)
	}
	state := leadrate.State(in)
	for _, want := range []string{"Lead type: Café with a website and no digital loyalty", "Own website: yes", "Online ordering on the homepage: ", "Site language: ro"} {
		if !strings.Contains(state, want) {
			t.Errorf("café state lacks %q:\n%s", want, state)
		}
	}
	if strings.Contains(state, "Drupal") || strings.Contains(state, "@") {
		t.Errorf("café state names Drupal or carries an address:\n%s", state)
	}
}

// TestRateCafeSupersede: a café lead holding only a rating under the Drupal question has no
// relevance on the list or the batch queue; leads rate asks it the café question, and the
// list and the queue then show the café number.
func TestRateCafeSupersede(t *testing.T) {
	campaignID := cafeScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	id := leadIDOf(t, "cafeneaua-exemplu.example")
	rateUnderQuestion(t, id, leadstore.QuestionDrupal, 90, leadstore.BandStrong, nil)

	relevance := func() (list, queue *int) {
		t.Helper()
		rows, _, err := lq.LeadList(ctx, leadstore.ListFilter{LeadIDs: []int64{id}, Status: leadstore.ListAll}, leadstore.ListContext{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("list %d rows, %v", len(rows), err)
		}
		cs, err := lq.CandidateLeads(ctx, leadstore.CandidateOptions{CampaignID: campaignID, AccountID: leadstore.DefaultAccountID, IncludeReserved: true})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(cs, func(c leadstore.Candidate) bool { return c.LeadID == id })
		if i < 0 {
			t.Fatalf("the café lead is not in the queue (%d candidates)", len(cs))
		}
		return rows[0].Relevance, cs[i].Relevance
	}
	if l, q := relevance(); l != nil || q != nil {
		t.Errorf("before rating: list %v queue %v, want no number from the Drupal question", l, q)
	}
	wantLines(t, "café rate", mustRun(t, "leads", "rate", "--decider", "fake", "--campaign", "cafes-loyalty"), "· rated 4 · unchanged 0")
	if l, q := relevance(); l == nil || q == nil || *l != 50 || *q != 50 {
		t.Errorf("after rating: list %v queue %v, want the café number 50", l, q)
	}
}

// TestLeadListTrustGrid: the console's Leads list orders a rating type's leads by relevance
// only when that type's own eval passed its trust gate (drupal trusted × café trusted, 4
// cells): the trusted types' rated leads first, best first, then every other lead in points
// order. A café eval never flips the Drupal gate, nor the other way.
func TestLeadListTrustGrid(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	b := leadsBackend(t)
	// Points order (unsupported 20 + email 10 + major): lucerne/margie/woodgrove (7) 60, then
	// the 8s 50, then the 9s 45. Relevance runs against points within each type.
	cafeLow := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	cafeHigh := setLeadKind(t, "proseware.example", cafe.KindNoDigitalLoyalty)
	drupalLow := leadIDOf(t, "margie.example")
	drupalHigh := leadIDOf(t, "cpandl.example")
	rateUnderQuestion(t, cafeLow, leadstore.QuestionCafe, 20, leadstore.BandUnlikely, nil)
	rateUnderQuestion(t, cafeHigh, leadstore.QuestionCafe, 80, leadstore.BandStrong, nil)
	rateUnderQuestion(t, drupalLow, leadstore.QuestionDrupal, 10, leadstore.BandNo, nil)
	rateUnderQuestion(t, drupalHigh, leadstore.QuestionDrupal, 90, leadstore.BandStrong, nil)
	domain := map[int64]string{cafeLow: "lucerne.example", cafeHigh: "proseware.example", drupalLow: "margie.example", drupalHigh: "cpandl.example"}

	list := func() []string {
		t.Helper()
		p, err := b.LeadList(ctx, console.LeadListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range p.Rows {
			out = append(out, r.Domain)
		}
		return out
	}
	points := list()
	if len(points) != len(orderSites) {
		t.Fatalf("listed %v", points)
	}
	byType := map[string][]string{
		leadstore.TypeDrupal: {domain[drupalHigh], domain[drupalLow]},
		leadstore.TypeCafe:   {domain[cafeHigh], domain[cafeLow]},
	}
	score := map[string]int{domain[drupalHigh]: 90, domain[cafeHigh]: 80, domain[cafeLow]: 20, domain[drupalLow]: 10}
	for _, drupalOn := range []bool{false, true} {
		for _, cafeOn := range []bool{false, true} {
			t.Run(fmt.Sprintf("drupal %v café %v", drupalOn, cafeOn), func(t *testing.T) {
				recordEval(t, leadstore.QuestionDrupal, drupalOn)
				recordEval(t, leadstore.QuestionCafe, cafeOn)
				var trusted []string
				if drupalOn {
					trusted = append(trusted, leadstore.TypeDrupal)
				}
				if cafeOn {
					trusted = append(trusted, leadstore.TypeCafe)
				}
				got, err := trustedTypes(ctx, b.lq())
				if err != nil || !slices.Equal(got, trusted) {
					t.Fatalf("trustedTypes = %v, %v; want %v", got, err, trusted)
				}
				var first []string
				for _, tp := range trusted {
					first = append(first, byType[tp]...)
				}
				slices.SortStableFunc(first, func(a, b string) int { return score[b] - score[a] })
				want := slices.Clone(first)
				for _, d := range points {
					if !slices.Contains(first, d) {
						want = append(want, d)
					}
				}
				if got := list(); !reflect.DeepEqual(got, want) {
					t.Errorf("listed %v, want %v", got, want)
				}
			})
		}
	}
}

// TestCallsPerType: calls are counted and evaluated per rating type: two café calls and one
// Drupal call count 2 and 1; eval --type cafe reads only the café calls (and records under
// the café question), eval with no --type only the Drupal one; a café call stored before
// café leads had their own question (its input without café facts) is rebuilt from the
// lead's facts now and counted. An unknown --type and a golden file holding no line of the
// type are refused in plain words.
func TestCallsPerType(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	b := leadsBackend(t)
	lq := b.lq()
	c1 := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	c2 := setLeadKind(t, "proseware.example", cafe.KindHasLoyalty)
	call := func(id int64, band string) {
		t.Helper()
		if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLabel, ID: id, Value: band}); err != nil {
			t.Fatal(err)
		}
	}
	call(c1, "likely")
	call(c2, "no")
	call(leadIDOf(t, "margie.example"), "maybe")
	_, in, ok := latestCall(t, c1)
	if !ok || in.Cafe == nil || in.Kind != cafe.KindNoDigitalLoyalty {
		t.Errorf("a café call's input %+v, want café facts", in)
	}
	counts := func() (drupal, cafeN, all int) {
		t.Helper()
		var err error
		if drupal, err = lq.LabelCount(ctx, leadstore.TypeDrupal); err != nil {
			t.Fatal(err)
		}
		if cafeN, err = lq.LabelCount(ctx, leadstore.TypeCafe); err != nil {
			t.Fatal(err)
		}
		if all, err = lq.LabelCount(ctx, ""); err != nil {
			t.Fatal(err)
		}
		return drupal, cafeN, all
	}
	if d, c, a := counts(); d != 1 || c != 2 || a != 3 {
		t.Errorf("calls drupal %d café %d all %d, want 1 2 3", d, c, a)
	}
	if ids, err := lq.LabelledLeadIDs(ctx, leadstore.TypeCafe); err != nil || !slices.Equal(ids, []int64{min(c1, c2), max(c1, c2)}) {
		t.Errorf("café labelled ids %v %v", ids, err)
	}

	out := mustRun(t, "leads", "relevance", "eval", "--type", "cafe", "--runs", "1")
	wantLines(t, "café eval", out, "leads relevance eval: Café leads · 2 calls\n",
		"relevance eval: 2 labelled lead(s), 0 unlabelled skipped · question relevance-cafe@1 · decider fake · 1 run(s)")
	out = mustRun(t, "leads", "relevance", "eval", "--runs", "1")
	wantLines(t, "default eval", out, "leads relevance eval: Drupal leads · 1 call\n",
		"relevance eval: 1 labelled lead(s), 0 unlabelled skipped · question relevance@1")

	// An old-shape café call: the input a call stored before café leads had their own
	// question, with no café facts.
	c3 := setLeadKind(t, "fourthcoffee.example", cafe.KindNoDigitalLoyalty)
	old, err := json.Marshal(leadrate.Input{Kind: cafe.KindNoDigitalLoyalty, Country: "GB", SiteName: "Old Shape", Channels: []string{leadstore.ChannelEmail}})
	if err != nil {
		t.Fatal(err)
	}
	band := leadstore.BandMaybe
	if _, err := lq.SetLeadLabel(ctx, c3, &band, old, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	out = mustRun(t, "leads", "relevance", "eval", "--type", "cafe", "--runs", "1")
	wantLines(t, "café eval with an old call", out,
		"leads relevance eval: Café leads · 3 calls · 1 call made before café leads had their own question, rebuilt from the lead's facts now",
		"relevance eval: 3 labelled lead(s)")
	if _, in, _ := latestCall(t, c3); in.Cafe != nil || in.SiteName != "Old Shape" {
		t.Errorf("the stored old call was changed: %+v", in)
	}

	// A café eval never flips the Drupal gate, nor a Drupal eval the café one.
	recordEval(t, leadstore.QuestionCafe, true)
	if got, err := trustedTypes(ctx, lq); err != nil || !slices.Equal(got, []string{leadstore.TypeCafe}) {
		t.Errorf("after a trusted café eval: %v %v, want café alone", got, err)
	}
	recordEval(t, leadstore.QuestionDrupal, true)
	recordEval(t, leadstore.QuestionCafe, false)
	if got, err := trustedTypes(ctx, lq); err != nil || !slices.Equal(got, []string{leadstore.TypeDrupal}) {
		t.Errorf("after a trusted Drupal eval and an untrusted café one: %v %v, want Drupal alone", got, err)
	}

	if code, _, errOut := leadsRun(t, "leads", "relevance", "eval", "--type", "agency"); code != 2 ||
		!strings.Contains(errOut, `--type "agency" is not a lead type: choose drupal (Drupal sites and agencies) or cafe (cafés)`) {
		t.Errorf("unknown --type: exit %d, %q", code, errOut)
	}
	if code, _, errOut := leadsRun(t, "leads", "relevance", "golden-draft", "--type", "bar", "--out", filepath.Join(t.TempDir(), "g.jsonl")); code != 2 ||
		!strings.Contains(errOut, "choose drupal (Drupal sites and agencies) or cafe (cafés)") {
		t.Errorf("golden-draft unknown --type: exit %d, %q", code, errOut)
	}
	demo := filepath.Join(examplesDir, "leads-relevance.demo.jsonl")
	if code, _, errOut := leadsRun(t, "leads", "relevance", "eval", "--type", "cafe", "--golden", demo, "--runs", "1"); code != 2 ||
		!strings.Contains(errOut, "has no line of café leads: 0 lines kept, 8 lines of other types skipped") {
		t.Errorf("café eval of the all-Drupal demo: exit %d, %q", code, errOut)
	}
	wantLines(t, "demo eval", mustRun(t, "leads", "relevance", "eval", "--golden", demo, "--runs", "1"),
		"leads relevance eval: Drupal leads · 8 lines kept · 0 lines of other types skipped", "relevance eval: 7 labelled lead(s), 1 unlabelled skipped")
}

// TestGoldenDraftByType: golden-draft --type cafe writes only café leads, with their café
// facts; the default drafts only the Drupal ones.
func TestGoldenDraftByType(t *testing.T) {
	orderScene(t)
	setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	setLeadKind(t, "proseware.example", cafe.KindNoDigitalLoyalty)
	dir := t.TempDir()
	cafePath, drupalPath := filepath.Join(dir, "cafe.jsonl"), filepath.Join(dir, "drupal.jsonl")
	wantLines(t, "café draft", mustRun(t, "leads", "relevance", "golden-draft", "--type", "cafe", "--n", "30", "--out", cafePath),
		"leads relevance golden-draft: Café leads · 2 leads (of 2 open)", "run leads relevance eval --type cafe --golden "+cafePath)
	wantLines(t, "Drupal draft", mustRun(t, "leads", "relevance", "golden-draft", "--n", "30", "--out", drupalPath),
		"leads relevance golden-draft: Drupal leads · 6 leads (of 6 open)")
	for path, want := range map[string]string{cafePath: leadstore.TypeCafe, drupalPath: leadstore.TypeDrupal} {
		lines, err := leadrate.LoadGolden(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range lines {
			if leadstore.TypeOf(g.Input.Kind) != want || (want == leadstore.TypeCafe) != (g.Input.Cafe != nil) {
				t.Errorf("%s: line %s of kind %s, café facts %v", want, g.ID, g.Input.Kind, g.Input.Cafe != nil)
			}
		}
	}
}
