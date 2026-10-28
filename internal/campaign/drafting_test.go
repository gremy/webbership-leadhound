package campaign

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// The CP4 drafting keys: copy.allowed_names, research_openers, inflation_words, echo_words;
// offer.pains, report, report_contains, call_to_action; countries.<CC>.acknowledge.

func draftingSpec(t *testing.T) Spec {
	t.Helper()
	s, err := Load(filepath.Join("testdata", "drafting.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDraftingKeysDecode(t *testing.T) {
	s := draftingSpec(t)
	if err := s.Check(known); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !reflect.DeepEqual(s.Copy.AllowedNames, []string{"Drupal", "Webbership"}) ||
		!reflect.DeepEqual(s.Copy.ResearchOpeners, []string{`(?i)\bI noticed\b`}) ||
		!reflect.DeepEqual(s.Copy.InflationWords, []string{"dozens", "best"}) || s.Copy.EchoWords != 7 {
		t.Errorf("copy = %+v", s.Copy)
	}
	if len(s.Offer.Pains) != 3 || s.Offer.Pains[0].ID != "eol_no_security" ||
		!reflect.DeepEqual(s.Offer.Pains[0].Needs, []string{"kind == unsupported", "finding.major in 6..9"}) ||
		s.Offer.Pains[0].Text != "Security support has ended." {
		t.Errorf("pains = %+v", s.Offer.Pains)
	}
	if s.Offer.Report != "free one-page upgrade-risk report" || len(s.Offer.ReportContains) != 2 ||
		s.Offer.CallToAction != "Worth 10 minutes to find out?" {
		t.Errorf("offer = %+v", s.Offer)
	}
	if s.Countries["DE"].Acknowledge == "" || s.Countries["GB"].Acknowledge != "" {
		t.Errorf("acknowledge = %+v", s.Countries)
	}
}

// A spec without the drafting keys hashes as it did before them: every new field is
// omitempty, so registered campaign versions keep their hashes.
func TestDraftingKeysKeepOldHash(t *testing.T) {
	j, _, err := Canonical(validSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pains", "report", "call_to_action", "allowed_names", "research_openers",
		"inflation_words", "echo_words", "acknowledge"} {
		if strings.Contains(string(j), `"`+k+`"`) {
			t.Errorf("canonical form of a spec without drafting keys names %q: %s", k, j)
		}
	}
	d := draftingSpec(t)
	_, h1, _ := Canonical(d)
	d.Offer.Pains[0].Text = "changed"
	if _, h2, _ := Canonical(d); h1 == h2 {
		t.Error("a changed pain kept the hash")
	}
}

func TestDraftingKeysStrict(t *testing.T) {
	for _, bad := range []string{
		"copy:\n  allowed_name: [x]\n",
		"offer:\n  pains:\n    - {id: a, needs: [kind == d10], text: t, weight: 2}\n",
		"countries: {GB: {email: on, lang: en, acknowledged: yes}}\n",
	} {
		if _, err := Decode([]byte("slug: x\n" + bad)); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("Decode(%q) = %v, want an unknown-key error", bad, err)
		}
	}
}

func TestDraftingCheckRules(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Spec)
		wants []string
	}{
		{"allowed name blank", func(s *Spec) { s.Copy.AllowedNames = append(s.Copy.AllowedNames, " ") }, []string{"copy.allowed_names"}},
		{"allowed name twice", func(s *Spec) { s.Copy.AllowedNames = append(s.Copy.AllowedNames, "drupal") }, []string{"listed twice"}},
		{"allowed name newline", func(s *Spec) { s.Copy.AllowedNames = []string{"A\nB"} }, []string{"copy.allowed_names"}},
		{"opener bad regexp", func(s *Spec) { s.Copy.ResearchOpeners = []string{"(?i)I (noticed"} }, []string{"copy.research_openers", "does not compile"}},
		{"opener blank", func(s *Spec) { s.Copy.ResearchOpeners = []string{""} }, []string{"copy.research_openers: a pattern is blank"}},
		{"inflation twice", func(s *Spec) { s.Copy.InflationWords = []string{"best", "Best"} }, []string{"copy.inflation_words", "listed twice"}},
		{"echo words too low", func(s *Spec) { s.Copy.EchoWords = 2 }, []string{"copy.echo_words 2"}},
		{"echo words too high", func(s *Spec) { s.Copy.EchoWords = 21 }, []string{"copy.echo_words 21"}},
		{"pain id duplicate", func(s *Spec) { s.Offer.Pains[1].ID = s.Offer.Pains[0].ID }, []string{`id "eol_no_security" is used twice`}},
		{"pain id bad", func(s *Spec) { s.Offer.Pains[0].ID = "Bad Id" }, []string{"offer.pains[0]: id"}},
		{"pain text blank", func(s *Spec) { s.Offer.Pains[0].Text = "" }, []string{"text must be one non-blank line"}},
		{"pain needs empty", func(s *Spec) { s.Offer.Pains[0].Needs = nil }, []string{"needs is empty"}},
		{"pain need unknown field", func(s *Spec) { s.Offer.Pains[0].Needs = []string{"org.country == GB"} }, []string{`need "org.country == GB" is not one of`}},
		{"pain need unknown operator", func(s *Spec) { s.Offer.Pains[0].Needs = []string{"finding.major >= 7"} }, []string{"is not one of"}},
		{"pain need or", func(s *Spec) { s.Offer.Pains[0].Needs = []string{"kind == d10 || kind == unsupported"} }, []string{"is not one of"}},
		{"pain need range down", func(s *Spec) { s.Offer.Pains[0].Needs = []string{"finding.major in 9..6"} }, []string{"must run upwards"}},
		{"pain need major 0", func(s *Spec) { s.Offer.Pains[0].Needs = []string{"finding.major == 0"} }, []string{"a major is 1 or more"}},
		{"pain need kind not in campaign", func(s *Spec) { s.Offer.Pains[0].Needs = []string{"kind == cafe"} }, []string{"names a kind the campaign does not have"}},
		{"report newline", func(s *Spec) { s.Offer.Report = "a\nb" }, []string{"offer.report must be one line"}},
		{"report contains without report", func(s *Spec) { s.Offer.Report = "" }, []string{"offer.report_contains needs offer.report"}},
		{"report contains blank", func(s *Spec) { s.Offer.ReportContains = []string{""} }, []string{"offer.report_contains"}},
		{"call to action too long", func(s *Spec) { s.Offer.CallToAction = strings.Repeat("a", 121) }, []string{"offer.call_to_action"}},
		{"acknowledge newline", func(s *Spec) { s.Countries["DE"] = Country{Call: true, Lang: "de", Acknowledge: "a\nb"} }, []string{"countries.DE.acknowledge"}},
		{"acknowledge blank", func(s *Spec) { s.Countries["DE"] = Country{Call: true, Lang: "de", Acknowledge: "  "} }, []string{"countries.DE.acknowledge"}},
		{"acknowledge too long", func(s *Spec) {
			s.Countries["DE"] = Country{Call: true, Lang: "de", Acknowledge: strings.Repeat("a", MaxAcknowledge+1)}
		}, []string{"countries.DE.acknowledge"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := draftingSpec(t)
			tc.edit(&s)
			err := s.Check(known)
			if err == nil {
				t.Fatal("Check passed; want a problem")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			for _, w := range tc.wants {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// The writer value is parsed and validated here; whether writer: claude is allowed is the
// gate's (CheckOptions.DraftsTrusted).
func TestDraftingWriterValue(t *testing.T) {
	s := draftingSpec(t)
	s.Writer = WriterClaude
	if err := s.Check(CheckOptions{DraftsTrusted: true, KnownKinds: leadconfig.Kinds}); err != nil {
		t.Errorf("writer claude, trusted: %v", err)
	}
	s.Writer = "templates"
	if err := s.Check(known); err == nil || !strings.Contains(err.Error(), `writer "templates"`) {
		t.Errorf("writer templates: %v, want refused (the value is %q)", err, WriterTemplate)
	}
}

func TestNeedEvaluator(t *testing.T) {
	cases := []struct {
		need string
		sig  Signals
		want bool
	}{
		{"finding.major in 6..9", Signals{Major: 6}, true},
		{"finding.major in 6..9", Signals{Major: 9}, true},
		{"finding.major in 6..9", Signals{Major: 10}, false},
		{"finding.major in 6..9", Signals{Major: 5}, false},
		{"finding.major in 6..9", Signals{}, false}, // unknown major meets no major need
		{"finding.major  in  7 .. 7", Signals{Major: 7}, true},
		{"finding.major == 10", Signals{Major: 10}, true},
		{"finding.major==10", Signals{Major: 11}, false},
		{"kind == d10", Signals{Kind: "d10", Major: 10}, true},
		{"kind == d10", Signals{Kind: "unsupported", Major: 10}, false},
		{" kind == agency_partner ", Signals{Kind: "agency_partner"}, true},
	}
	for _, c := range cases {
		n, err := ParseNeed(c.need)
		if err != nil {
			t.Errorf("ParseNeed(%q): %v", c.need, err)
			continue
		}
		if got := n.Holds(c.sig); got != c.want {
			t.Errorf("%q on %+v = %t, want %t", c.need, c.sig, got, c.want)
		}
	}
	for _, bad := range []string{"", "kind = d10", "kind == D10", "finding.major in 6", "finding.major in 6..100",
		"finding.minor == 3", "kind != d10", "finding.major in 6..9 and kind == d10", "kind == d10; drop table"} {
		if _, err := ParseNeed(bad); err == nil {
			t.Errorf("ParseNeed(%q) passed; want refused", bad)
		}
	}
}

func TestPainsFor(t *testing.T) {
	s := draftingSpec(t)
	ids := func(sig Signals) []string {
		p, err := s.PainsFor(sig)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, x := range p {
			out = append(out, x.ID)
		}
		return out
	}
	for _, c := range []struct {
		sig  Signals
		want []string
	}{
		{Signals{Kind: "unsupported", Major: 7}, []string{"eol_no_security", "d7_migration"}},
		{Signals{Kind: "unsupported", Major: 8}, []string{"eol_no_security"}},
		{Signals{Kind: "d10", Major: 10}, []string{"d10_eol"}},
		{Signals{Kind: "agency_partner"}, []string{}},
		{Signals{Kind: "unsupported"}, []string{}}, // major unknown: no major pain
	} {
		if got := ids(c.sig); !reflect.DeepEqual(got, c.want) {
			t.Errorf("pains for %+v = %v, want %v", c.sig, got, c.want)
		}
	}
	s.Offer.Pains[0].Needs = []string{"bogus"}
	if _, err := s.PainsFor(Signals{Kind: "unsupported", Major: 7}); err == nil {
		t.Error("PainsFor with an unparsable need passed")
	}
}

func TestGroundRulesFromSpec(t *testing.T) {
	s := draftingSpec(t)
	r, err := s.GroundRules(Signals{Kind: "unsupported", Major: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Pains, []outreach.Pain{{ID: "eol_no_security", Text: "Security support has ended."},
		{ID: "d7_migration", Text: "Leaving it is a migration."}}) {
		t.Errorf("pains = %+v", r.Pains)
	}
	if !reflect.DeepEqual(r.AllowedNames, []string{"Drupal", "Webbership"}) || r.EchoWords != 7 ||
		!reflect.DeepEqual(r.InflationWords, []string{"dozens", "best"}) {
		t.Errorf("rules = %+v", r)
	}
	if len(r.ResearchOpeners) != 1 || !r.ResearchOpeners[0].MatchString("Hello, I noticed it") {
		t.Errorf("openers = %v", r.ResearchOpeners)
	}
	// The spec's lists alias nothing in the rules.
	r.AllowedNames[0] = "X"
	if s.Copy.AllowedNames[0] != "Drupal" {
		t.Error("GroundRules shares the spec's slice")
	}

	// A spec with no drafting keys leaves every list nil: Ground's defaults apply.
	d, err := validSpec(t).GroundRules(Signals{Kind: "unsupported", Major: 7})
	if err != nil {
		t.Fatal(err)
	}
	if d.Pains != nil || d.AllowedNames != nil || d.ResearchOpeners != nil || d.InflationWords != nil || d.EchoWords != 0 {
		t.Errorf("rules without drafting keys = %+v, want all defaults", d)
	}

	s.Copy.ResearchOpeners = []string{"("}
	if _, err := s.GroundRules(Signals{}); err == nil {
		t.Error("GroundRules with a bad regexp passed")
	}
}

// The example campaign the operator copies still checks, and its pains follow the
// templates: a Drupal 10 lead never gets a migration pain.
func TestExampleDrupalEOLDrafting(t *testing.T) {
	s, err := Load(filepath.Join(examplesDir, "campaigns", "drupal-eol", FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(known); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if s.Offer.CallToAction != "Worth 10 minutes to find out?" || s.Offer.Report == "" || len(s.Copy.AllowedNames) == 0 {
		t.Errorf("example drafting keys missing: %+v %+v", s.Offer, s.Copy)
	}
	for _, sig := range []Signals{{Kind: "unsupported", Major: 7}, {Kind: "unsupported", Major: 9}, {Kind: "d10", Major: 10},
		{Kind: "agency_partner"}} {
		r, err := s.GroundRules(sig)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Pains) < 3 {
			t.Errorf("%+v: %d pains, want at least 3", sig, len(r.Pains))
		}
		for _, p := range r.Pains {
			if sig.Kind == "d10" && strings.Contains(strings.ToLower(p.Text), "migrat") {
				t.Errorf("d10 pain %s says migration: %q", p.ID, p.Text)
			}
		}
	}
	r, _ := s.GroundRules(Signals{Kind: "unsupported", Major: 9})
	for _, p := range r.Pains {
		if p.ID == "d7_migration" {
			t.Error("a Drupal 9 lead got the Drupal 7 migration pain")
		}
	}
}
