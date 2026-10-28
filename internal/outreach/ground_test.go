package outreach

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// groundLead is the fictional lead the testdata/ground drafts are written for: "d7" (an
// unsupported Drupal 7 site) or "d10".
func groundSheet(t *testing.T, lead string) FactSheet {
	t.Helper()
	major, exact, eol, kind := 7, "7.98", time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC), "unsupported"
	if lead == "d10" {
		major, exact, eol, kind = 10, "10.3.1", time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC), "d10"
	}
	return BuildFactSheet(SheetInput{
		Org:  OrgFacts{Name: "Northfield Museum Trust", Country: "GB", Domain: "northfield-museum.example", Source: Provenance{Table: "org", ID: 7}},
		Kind: kind,
		Finding: &FindingFacts{
			Major: major, Exact: exact, Signal: "core-asset-v " + exact,
			Evidence: "The site's CHANGELOG.txt names Drupal " + exact + ".",
			URL:      "https://northfield-museum.example/CHANGELOG.txt",
			Source:   Provenance{Table: "site_probe", ID: 11},
		},
		EOL: eol, EOLSource: ConfigSource,
		OrgTexts: []TextFact{{Value: "Northfield Museum Trust cares for the town's maritime collection and runs year-round family workshops by the harbour.", Source: Provenance{Table: "org_fact", ID: 3}}},
		Sender: SenderFacts{
			Name: "Sam Carter", Role: "Drupal developer", IdentityLine: "Sam Carter, Drupal developer, sender.example",
			ProofLine: "Recently I took a Drupal 7 site for Harbour Lane Studio to Drupal 10, with no downtime.",
			SiteURL:   "https://sender.example/",
			Extra:     []TextFact{{Label: "works_with", Value: "organisations and agencies that still run older Drupal sites"}},
			Source:    Provenance{Table: "campaign", ID: 1},
		},
		Offer: OfferFacts{
			Report:       "free one-page upgrade-risk report",
			Contents:     []string{"what is at risk", "what an upgrade involves"},
			CallToAction: "Worth 10 minutes to find out?",
			Source:       Provenance{Table: "campaign", ID: 1},
		},
	})
}

func groundRules(lead string) GroundRules {
	r := GroundRules{
		StepRules: StepRules{
			EmailSteps:     []int{0, 1, 3, 4},
			MaxWords:       map[int]int{0: 120, 1: 80, 3: 60, 4: 60},
			BannedWords:    []string{"synergy", "guarantee"},
			NoLinksInStep0: true,
			NoExclamation:  true,
		},
		CallStep:     true,
		AllowedNames: []string{"Drupal", "Webbership"},
		Pains: []Pain{
			{ID: "eol_no_security", Text: "Drupal 7 stopped getting security updates, so new security holes stay open."},
			{ID: "modules_no_fixes", Text: "Modules no longer get fixes, so small faults pile up."},
			{ID: "php_hosting", Text: "Newer PHP versions will not run the old code, so hosting gets harder."},
		},
	}
	if lead == "d10" {
		r.Forbid = []*regexp.Regexp{regexp.MustCompile(`(?i)migrat`)}
		r.Pains = []Pain{
			{ID: "d10_eol", Text: "Drupal 10 stops getting security updates on its end-of-life date; after that new holes stay open."},
			{ID: "modules_d11", Text: "Some modules need new versions for Drupal 11, and a few have none yet."},
		}
	}
	return r
}

type groundFixture struct {
	name  string
	Lead  string       `json:"lead"`
	Want  string       `json:"want"`
	Note  string       `json:"note"`
	Draft WrittenDraft `json:"draft"`
}

func groundFixtures(t *testing.T) []groundFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "ground", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no ground fixtures: %v", err)
	}
	var out []groundFixture
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // test fixture path from a fixed glob
		if err != nil {
			t.Fatal(err)
		}
		var f groundFixture
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		f.name = strings.TrimSuffix(filepath.Base(p), ".json")
		out = append(out, f)
	}
	return out
}

func failureCodes(fs []Failure) []string {
	var codes []string
	for _, f := range fs {
		if !slices.Contains(codes, f.Code) {
			codes = append(codes, f.Code)
		}
	}
	return codes
}

// TestGroundFixtures: each adversarial draft is refused with exactly its code; clean drafts pass.
func TestGroundFixtures(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range groundFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			got := Ground(f.Draft, groundSheet(t, f.Lead), groundRules(f.Lead))
			codes := failureCodes(got)
			if f.Want == "" {
				if len(got) > 0 {
					t.Fatalf("clean draft refused: %v", got)
				}
				return
			}
			if len(codes) != 1 || codes[0] != f.Want {
				t.Fatalf("%s: want exactly %s, got %v", f.Note, f.Want, got)
			}
		})
		seen[f.Want] = true
	}
	for _, code := range GroundCodes {
		if !seen[code] {
			t.Errorf("no fixture for %s", code)
		}
	}
	if !seen[""] {
		t.Error("no clean fixture")
	}
}

// TestGroundMutation disables one rule at a time: only that rule's fixtures then pass, so
// each fixture is held by its own rule and no other.
func TestGroundMutation(t *testing.T) {
	fixtures := groundFixtures(t)
	for _, off := range GroundCodes {
		t.Run(off, func(t *testing.T) {
			on := func(code string) bool { return code != off }
			for _, f := range fixtures {
				got := groundWith(f.Draft, groundSheet(t, f.Lead), groundRules(f.Lead), on)
				switch {
				case f.Want == off && len(got) > 0:
					t.Errorf("%s still refused with %s off: %v", f.name, off, got)
				case f.Want != off && f.Want != "" && len(got) == 0:
					t.Errorf("%s passes with %s off", f.name, off)
				case f.Want == "" && len(got) > 0:
					t.Errorf("clean %s refused: %v", f.name, got)
				}
			}
		})
	}
}

// TestGroundClaimNormalisation: G3 compares dates as dates, versions, numbers, money,
// percentages, phones, emails, URLs and domains in normalised form.
func TestGroundClaimNormalisation(t *testing.T) {
	cases := []struct {
		text, source string
		uncovered    []string
	}{
		{"on 9 December 2026", "2026-12-09", nil},
		{"on December 9, 2026", "2026-12-09", nil},
		{"on 09/12/2026", "2026-12-09", nil},
		{"in December 2026", "2026-12-09", nil},
		{"by 2026", "2026-12-09", nil},
		{"since 2019", "2026-12-09", []string{"2019"}},
		{"on 10 December 2026", "2026-12-09", []string{"10 December 2026"}},
		{"Drupal 10 sites", "10.3.1", nil},
		{"Drupal 10.3", "10.3.1", nil},
		{"Drupal 10.4", "10.3.1", []string{"Drupal 10.4"}},
		{"D7 and 7.x", "Drupal 7", nil},
		{"Drupal 8", "Drupal 7", []string{"Drupal 8"}},
		{"€8,000 a month", "EUR 8000", nil},
		{"8k EUR", "€8,000", nil},
		{"$8,000", "EUR 8000", []string{"$8,000"}},
		{"12% of sites", "12 percent", nil},
		{"call +44 20 7946 0958", "+442079460958", nil},
		{"call +44 20 7946 0959", "+442079460958", []string{"+44 20 7946 0959"}},
		{"write to Info@Example.org", "info@example.org", nil},
		{"see https://Example.org/", "example.org", nil},
		{"see example.org", "https://www.example.org/about", nil},
		{"see https://example.org/other", "https://example.org/about", []string{"https://example.org/other"}},
		{"acquia.com", "example.org", []string{"acquia.com"}},
		{"Worth 10 minutes", "Worth 10 minutes to find out?", nil},
		{"1,000 pages", "1000 pages", nil},
		{"web2 and h2o", "", nil},
	}
	for _, c := range cases {
		var got []string
		for _, cl := range uncoveredClaims(c.text, []string{c.source}) {
			got = append(got, cl.raw)
		}
		if !slices.Equal(got, c.uncovered) {
			t.Errorf("%q against %q: uncovered %q, want %q", c.text, c.source, got, c.uncovered)
		}
	}
}

// TestGroundNames: capitalised runs that do not start a sentence, months and "I" aside.
func TestGroundNames(t *testing.T) {
	got := capitalRuns("Since 5 January, I'm sure your Drupal Commerce shop on Acquia works. Northfield is fine; we met Sam Carter on Monday.")
	want := []string{"Drupal Commerce", "Acquia", "Sam Carter"}
	if !slices.Equal(got, want) {
		t.Fatalf("capitalRuns = %q, want %q", got, want)
	}
	if nameAllowed("Drupal Commerce", []string{"Drupal"}) || !nameAllowed("Drupal-based", []string{"Drupal"}) {
		t.Fatal("nameAllowed")
	}
	if w := orgNameMisuse("for northfield museum trust", "Northfield Museum Trust", nil); w == "" {
		t.Fatal("lower-case org name passed G4")
	}
	if w := orgNameMisuse("for Northfield Museum Trust and the museum", "Northfield Museum Trust", nil); w != "" {
		t.Fatalf("exact use refused: %q", w)
	}
}

// TestGroundSchemaAndCites covers G1/G2 cases beside the fixtures: an extra step, a
// duplicate, talking points on a sequence with no call step, an unknown cite.
func TestGroundSchemaAndCites(t *testing.T) {
	base := groundFixtures(t)[0] // clean_d10 sorts first
	if base.Want != "" {
		t.Fatalf("first fixture %s is not clean", base.name)
	}
	sheet, rules := groundSheet(t, base.Lead), groundRules(base.Lead)
	clone := func() WrittenDraft {
		b, _ := json.Marshal(base.Draft)
		var d WrittenDraft
		_ = json.Unmarshal(b, &d)
		return d
	}

	d := clone()
	d.Steps = append(d.Steps, WrittenStep{Step: 2, Subject: "x", Paragraphs: []Paragraph{{Text: "Hello.", Cites: []string{"S1"}}}})
	if c := failureCodes(Ground(d, sheet, rules)); !slices.Equal(c, []string{CodeSchema}) {
		t.Errorf("extra step: %v", c)
	}
	d = clone()
	d.Steps = append(d.Steps, d.Steps[1])
	if c := failureCodes(Ground(d, sheet, rules)); !slices.Equal(c, []string{CodeSchema}) {
		t.Errorf("duplicate step: %v", c)
	}
	noCall := rules
	noCall.CallStep = false
	if c := failureCodes(Ground(clone(), sheet, noCall)); !slices.Equal(c, []string{CodeSchema}) {
		t.Errorf("talking points without a call step: %v", c)
	}
	d = clone()
	d.Steps[0].Paragraphs[5].Cites = append(d.Steps[0].Paragraphs[5].Cites, "F99")
	got := Ground(d, sheet, rules)
	if c := failureCodes(got); !slices.Equal(c, []string{CodeCites}) || got[0].Token != "F99" || got[0].Step != 0 || got[0].Para != 5 {
		t.Errorf("unknown cite: %v", got)
	}
}

// TestGroundCheckText is the CP6 export: G3 and G8 over any rendered text against a sheet.
func TestGroundCheckText(t *testing.T) {
	sheet := groundSheet(t, "d10")
	forbid := []*regexp.Regexp{regexp.MustCompile(`(?i)migrat`)}
	ok := "Northfield Museum Trust runs Drupal 10.3.1. Security support ends on 9 December 2026. The update to Drupal 11 is the next step."
	if got := CheckText(ok, sheet, []string{"The update to Drupal 11 is the next step."}, forbid); len(got) > 0 {
		t.Fatalf("clean report refused: %v", got)
	}
	bad := "Support ended in 2019. The migration is easy."
	got := CheckText(bad, sheet, nil, forbid)
	if c := failureCodes(got); !slices.Equal(c, []string{CodeClaim, CodeKind}) || got[0].Token != "2019" {
		t.Fatalf("bad report: %v", got)
	}
	// echo_guard facts are no source: the description's words back nothing.
	if got := CheckText("We hold 3 collections.", BuildFactSheet(SheetInput{OrgTexts: []TextFact{{Value: "3 collections"}}}), nil, nil); len(got) != 1 {
		t.Fatalf("echo_guard fact used as a source: %v", got)
	}
}

// TestGroundCheckRendered: G10 maps Check's problems on the rendered email.
func TestGroundCheckRendered(t *testing.T) {
	d, cfg := checkDraft(t)
	if got := CheckRendered(d, 0, cfg, StepRules{}); len(got) > 0 {
		t.Fatalf("clean rendered draft refused: %v", got)
	}
	d.Subject += "!"
	got := CheckRendered(d, 0, cfg, StepRules{})
	if len(got) == 0 || got[0].Code != CodeRendered || got[0].Token != RuleSubjectBang {
		t.Fatalf("G10: %v", got)
	}
}

// TestGroundDecodeWrittenDraft: the model's JSON is read strictly.
func TestGroundDecodeWrittenDraft(t *testing.T) {
	ok := `{"steps":[{"step":0,"subject":"s","paragraphs":[{"text":"t","cites":["S1"],"pain_id":"p"}]}],"talking_points":[]}`
	d, err := DecodeWrittenDraft([]byte(ok))
	if err != nil || d.Steps[0].Paragraphs[0].PainID != "p" {
		t.Fatalf("decode: %v %+v", err, d)
	}
	for _, bad := range []string{`{"steps":[],"extra":1}`, ok + ` {}`, `{"steps":`} {
		if _, err := DecodeWrittenDraft([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestGroundFailureString names the place and the token.
func TestGroundFailureString(t *testing.T) {
	f := Failure{Code: CodeClaim, Step: 0, Para: 1, Token: "2019", Msg: "2019 is not in a cited fact"}
	if s := f.String(); s != `G3 step 0 paragraph 2 "2019": 2019 is not in a cited fact` {
		t.Fatal(s)
	}
}

// TestGroundSubjectRestsOnItsStep: a subject carries no cites, so its names and claims are
// checked against its step's paragraph cites and named pains, and it may always name the org
// (the walk's "Drupal 9 end of life for <org>" was refused by G4b). A name or number its step
// does not back is still refused.
func TestGroundSubjectRestsOnItsStep(t *testing.T) {
	base := groundDraft(t, "clean_d7")
	// no paragraph of step 0 cites the org's name fact
	for i := range base.Steps[0].Paragraphs {
		p := &base.Steps[0].Paragraphs[i]
		p.Cites = slices.DeleteFunc(slices.Clone(p.Cites), func(id string) bool { return id == "F1" })
		p.Text = strings.ReplaceAll(p.Text, " for Northfield Museum Trust", "")
	}
	sheet, rules := groundSheet(t, "d7"), groundRules("d7")
	for _, tc := range []struct {
		subject, code, token string
	}{
		{"Drupal 7 end of life for Northfield Museum Trust", "", ""},
		{"Northfield Museum Trust and the Drupal 7 end of life", "", ""},
		{"Security updates for Northfield Museum Trust", "", ""},
		{"Drupal 7 end of life for Acquia", CodeNames, "Acquia"},
		{"Drupal 8 end of life for Northfield Museum Trust", CodeClaim, "Drupal 8"},
		{"Drupal 7 end of life for Northfield Museum", CodeOrgName, ""},
	} {
		d := base
		d.Steps = slices.Clone(base.Steps)
		d.Steps[0].Subject = tc.subject
		fails := Ground(d, sheet, rules)
		if tc.code == "" {
			if len(fails) != 0 {
				t.Errorf("%q refused: %v", tc.subject, fails)
			}
			continue
		}
		found := false
		for _, f := range fails {
			if f.Code == tc.code && f.Step == 0 && f.Para == ParaSubject && (tc.token == "" || f.Token == tc.token) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: want %s %q, got %v", tc.subject, tc.code, tc.token, fails)
		}
	}
}

// groundDraft is fixture name's draft, a deep copy of its steps.
func groundDraft(t *testing.T, name string) WrittenDraft {
	t.Helper()
	for _, f := range groundFixtures(t) {
		if f.name == name {
			b, err := json.Marshal(f.Draft)
			if err != nil {
				t.Fatal(err)
			}
			d, err := DecodeWrittenDraft(b)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("no ground fixture %q", name)
	return WrittenDraft{}
}

// TestGroundReviewProbes are the CP9 review's reproductions on clean_d10: each passed Ground
// before G5b read reader claims, spelled-out numbers became claim tokens and talking points
// got G5b. Each must now be refused, with its rule among the codes.
func TestGroundReviewProbes(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		mut  func(d *WrittenDraft)
	}{
		{"contact form, cites sender only", CodePains, func(d *WrittenDraft) {
			d.Steps[0].Paragraphs[2] = Paragraph{Text: "Your contact form is broken, so messages may not reach you.", Cites: []string{"S1"}}
		}},
		{"contact form, no cites", CodePains, func(d *WrittenDraft) {
			d.Steps[0].Paragraphs[2] = Paragraph{Text: "Your contact form is broken, so messages may not reach you.", Cites: []string{}}
		}},
		{"spelled version", CodeClaim, func(d *WrittenDraft) {
			d.Steps[0].Paragraphs[2] = Paragraph{Text: "Your site still runs Drupal six, so it was hacked last spring.", Cites: []string{"F5"}, PainID: "d10_eol"}
		}},
		{"spelled boast", CodeProof, func(d *WrittenDraft) {
			d.Steps[0].Paragraphs[2] = Paragraph{Text: "I have upgraded forty such sites over twenty years.", Cites: []string{}}
		}},
		{"talking point invented", CodePains, func(d *WrittenDraft) {
			d.TalkingPoints[0] = TalkingPoint{Text: "Their site was hacked last month and leaks data.", Cites: []string{}}
		}},
	} {
		d := groundDraft(t, "clean_d10")
		tc.mut(&d)
		got := Ground(d, groundSheet(t, "d10"), groundRules("d10"))
		if !slices.Contains(failureCodes(got), tc.code) {
			t.Errorf("%s: want %s among the failures, got %v", tc.name, tc.code, got)
		}
	}
}

// TestGroundReaderClaims: outside a pain paragraph, a sentence about the reader (you, your,
// the Romanian second person, the org's name or domain) passes only as the sender's own action
// or as the offer; a talking point about the org also needs a lead fact behind it.
func TestGroundReaderClaims(t *testing.T) {
	sheet, rules := groundSheet(t, "d10"), groundRules("d10")
	para := func(text string, cites ...string) WrittenDraft {
		d := groundDraft(t, "clean_d10")
		d.Steps[0].Paragraphs[2] = Paragraph{Text: text, Cites: append([]string{}, cites...)}
		return d
	}
	point := func(text string, cites ...string) WrittenDraft {
		d := groundDraft(t, "clean_d10")
		d.TalkingPoints[2] = TalkingPoint{Text: text, Cites: append([]string{}, cites...)}
		return d
	}
	subject := func(i int, text string) WrittenDraft {
		d := groundDraft(t, "clean_d10")
		d.Steps[i].Subject = text
		return d
	}
	for _, tc := range []struct {
		name    string
		d       WrittenDraft
		refused bool
	}{
		{"sender action", para("I tried to call you today about your Drupal site.", "S1"), false},
		{"sender action, contraction", para("I'll send you the details if you reply.", "S1"), false},
		{"sender name opens", para("Sam Carter again, writing to you about the report.", "S1"), false},
		{"no reader word", para("Many older Drupal sites need care.", "S1"), false},
		{"perception verb", para("I know your contact form is broken.", "S1"), true},
		{"perception verb, noticed", para("I noticed you have no backups.", "S1"), true},
		{"reader as subject", para("You lose messages every week.", "S1"), true},
		{"you're", para("Sadly you're behind on updates.", "S1"), true},
		{"capitalised YOUR", para("Messages to YOUR team go nowhere.", "S1"), true},
		{"org name", para("Northfield Museum Trust has a broken contact form.", "S1", "F1"), true},
		{"org name in offer", para("I can write a free one-page upgrade-risk report for Northfield Museum Trust.", "O1", "F1"), false},
		{"offer value", para("The free one-page upgrade-risk report is yours if you want it.", "O1"), false},
		{"offer, yours only", para("I can write a free one-page upgrade-risk report. It is yours to keep.", "O1"), false},
		{"claim hidden in an offer", para("Your contact form is broken. I can write a free one-page upgrade-risk report.", "O1"), true},
		{"romanian possessive", para("Formularul tău de contact nu merge.", "S1"), true},
		{"romanian diacritic-free", para("Site-ul tau e vechi.", "S1"), true},
		{"romanian formal", para("Site-ul dumneavoastra e vechi.", "S1"), true},
		{"romanian sender action", para("Am încercat să te sun azi, despre site-ul tău.", "S1"), false},
		{"romanian perception", para("Am văzut că site-ul tău e vechi.", "S1"), true},
		{"not a reader word", para("Someone younger may take this on.", "S1"), false},
		{"talking point with a lead fact", point("Their site runs Drupal 10.3.1.", "F6"), false},
		{"talking point, offer", point("Offer the free one-page upgrade-risk report, theirs to keep.", "O1"), false},
		{"talking point, no fact", point("They lose bookings through the old site."), true},
		{"talking point, identity fact only", point("Northfield Museum Trust leaks data.", "F1"), true},
		{"talking point, you", point("Ask whether you can speak to whoever runs the site."), true},
		{"talking point, your", point("Your site leaks data.", "S1"), true},
		{"talking point, sender action", point("I can send the report by email.", "S1"), false},
		{"subject, step with no pain", subject(2, "Your contact form is broken"), true},
		{"subject, sender action", subject(2, "I tried to call you"), false},
		{"subject, step with a pain", subject(0, "Your Drupal 10 site and its support"), false},
	} {
		var g5b []Failure
		for _, f := range Ground(tc.d, sheet, rules) {
			if f.Code == CodePains {
				g5b = append(g5b, f)
			}
		}
		if got := len(g5b) > 0; got != tc.refused {
			t.Errorf("%s: refused=%v, want %v (%v)", tc.name, got, tc.refused, g5b)
		}
	}
}

// TestGroundSpelledNumbers: spelled-out cardinals and ordinals are claim tokens, compared as
// numbers; "one", "first", "second", "dozens" and the Romanian articles un/o are not.
func TestGroundSpelledNumbers(t *testing.T) {
	cases := []struct {
		text, source string
		uncovered    []string
	}{
		{"Drupal six", "Drupal 6", nil},
		{"Drupal Seven and 7.x", "Drupal 7", nil},
		{"Drupal six", "Drupal 7", []string{"Drupal six"}},
		{"forty sites", "40 sites", nil},
		{"forty sites", "", []string{"forty"}},
		{"forty-two sites, forty two pages", "42", nil},
		{"one hundred and twenty pages", "120", nil},
		{"two thousand pages", "2,000", nil},
		{"a dozen sites", "12", nil},
		{"a dozen sites", "", []string{"a dozen"}},
		{"the third time", "3", nil},
		{"the twenty-first site", "", []string{"twenty-first"}},
		{"over twenty years", "", []string{"twenty"}},
		{"Worth ten minutes", "Worth 10 minutes to find out?", nil},
		{"one of the modules, someone, the first step, a second look, no one", "", nil},
		{"dozens and hundreds of sites", "", nil},
		{"a one-page report", "", nil},
		{"trei site-uri, două pagini, sapte ani, şase luni", "3 2 7 6", nil},
		{"douăzeci și cinci de ani", "25", nil},
		{"zece ani", "", []string{"zece"}},
		{"un site și o pagină, una dintre ele", "", nil},
		{"o versiune nouă de Drupal, o nouă versiune, noua versiune", "", nil},
		{"nouă ani", "", []string{"nouă"}},
		{"you can opt out, opt-out", "", nil},
		{"opt ani", "", []string{"opt"}},
	}
	for _, c := range cases {
		var got []string
		for _, cl := range uncoveredClaims(c.text, []string{c.source}) {
			got = append(got, cl.raw)
		}
		if !slices.Equal(got, c.uncovered) {
			t.Errorf("%q against %q: uncovered %q, want %q", c.text, c.source, got, c.uncovered)
		}
	}
}
