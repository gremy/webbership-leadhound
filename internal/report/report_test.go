package report

import (
	"bytes"
	"flag"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/gremy/webbership-leadhound/internal/outreach"
)

var update = flag.Bool("update", false, "rewrite the golden HTML in testdata")

var reportNow = time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)

func sender() outreach.SenderFacts {
	return outreach.SenderFacts{
		Name:         "Alex Example",
		IdentityLine: "I build and look after Drupal sites for small organisations.",
		ProofLine:    "Recently I took a Drupal 7 site for Harbour Lane Studio to Drupal 10, with no downtime.",
		SiteURL:      "https://sender.example/",
		Source:       outreach.ConfigSource,
	}
}

func probe(id int64) outreach.Provenance { return outreach.Provenance{Table: "site_probe", ID: id} }

// d7Input is a fictional Drupal 7 lead.
func d7Input() Input {
	p := probe(412)
	return Input{
		Sheet: outreach.SheetInput{
			Org:  outreach.OrgFacts{Name: "Riverside Trust", Country: "GB", Domain: "riverside.example", Source: outreach.Provenance{Table: "org", ID: 17}},
			Kind: "unsupported",
			Finding: &outreach.FindingFacts{Major: 7, Exact: "7.98", Signal: "core-asset-v 7.98",
				Evidence: "the page loads Drupal's core files at version 7.98", URL: "https://www.riverside.example/", Source: p},
			EOL:       time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC),
			EOLSource: outreach.ConfigSource,
			Sender:    sender(),
		},
		ProbedAt: time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC),
		Signals: []outreach.TextFact{
			{Value: "the page loads Drupal's core files at version 7.98", Source: p},
			{Value: plainSignals["Drupal.settings"], Source: p},
		},
		ContactEmail:  "alex@sender.example",
		ContactSource: outreach.ConfigSource,
		Major:         DefaultMajorTexts()[7],
		Now:           reportNow,
	}
}

// d10Input is a fictional Drupal 10 lead.
func d10Input() Input {
	p := probe(9001)
	return Input{
		Sheet: outreach.SheetInput{
			Org:  outreach.OrgFacts{Name: "Northgate Books", Country: "IE", Domain: "northgate.example", Source: outreach.Provenance{Table: "org", ID: 23}},
			Kind: "d10",
			Finding: &outreach.FindingFacts{Major: 10, Exact: "10.3.6", Signal: "core-asset-v 10.3.6",
				Evidence: "the page loads Drupal's core files at version 10.3.6", URL: "https://northgate.example/", Source: p},
			EOL:       time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC),
			EOLSource: outreach.ConfigSource,
			Sender:    sender(),
		},
		ProbedAt: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC),
		Signals: []outreach.TextFact{
			{Value: "the page loads Drupal's core files at version 10.3.6", Source: p},
			{Value: plainSignals["x-drupal-dynamic-cache"], Source: p},
		},
		Major:  DefaultMajorTexts()[10],
		Forbid: []*regexp.Regexp{regexp.MustCompile(`(?i)migrat`)},
		Now:    reportNow,
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- a test's own golden file
	if err != nil {
		t.Fatalf("%v (run go test ./internal/report/ -run Report -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the rendered page; rerun with -update if the change is meant", path)
	}
}

func TestReportGoldenD7(t *testing.T) {
	b, err := UpgradeRisk(d7Input())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "upgrade_risk_d7.html", b)
	text, err := VisibleText(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Drupal 7.98", "5 January 2025", "661", "days since end of life", "20 October 2026",
		"https://www.riverside.example/", "What a migration involves", "Alex Example", "alex@sender.example", "28 October 2026"} {
		if !strings.Contains(text, want) {
			t.Errorf("the D7 report lacks %q", want)
		}
	}
}

func TestReportGoldenD10(t *testing.T) {
	b, err := UpgradeRisk(d10Input())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "upgrade_risk_d10.html", b)
	text, err := VisibleText(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Drupal 10.3.6", "9 December 2026", "42", "days left", "update to Drupal 11"} {
		if !strings.Contains(text, want) {
			t.Errorf("the D10 report lacks %q", want)
		}
	}
}

// TestReportD10NeverMigration: a Drupal 10 report never says "migrat", in any case, even when
// the caller passes no forbid list, and a D10 text that does is refused.
func TestReportD10NeverMigration(t *testing.T) {
	in := d10Input()
	in.Forbid = nil
	b, err := UpgradeRisk(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "migrat") {
		t.Fatal("the D10 report says migrat…")
	}
	in.Major = DefaultMajorTexts()[7] // the migration wording on a D10 lead
	if _, err := UpgradeRisk(in); err == nil || !strings.Contains(err.Error(), "G8") {
		t.Fatalf("migration wording on a D10 report: err %v, want a G8 refusal", err)
	}
}

// TestReportRefusesInventedNumber is the mutation: a number written into the template, not
// backed by any fact, makes UpgradeRisk refuse the page and name the token.
func TestReportRefusesInventedNumber(t *testing.T) {
	for name, inject := range map[string]string{
		"count":   "We have rescued 37 Drupal sites.",
		"year":    "Support ended in 2019.",
		"version": "Drupal 12 is coming.",
		"url":     "See https://evil.example/offer for more.",
		"money":   "It costs €4,000.",
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(pageSource, "<h2>What we found</h2>", "<h2>What we found</h2><p>"+inject+"</p>", 1)
			tmpl := template.Must(template.New("mutant").Parse(src))
			for _, in := range []Input{d7Input(), d10Input()} {
				_, err := render(in, tmpl)
				if err == nil || !strings.Contains(err.Error(), "G3") {
					t.Fatalf("injected %q: err %v, want a G3 refusal", inject, err)
				}
			}
		})
	}
	// The unmutated template passes, so the refusal above is the injection's.
	if _, err := render(d7Input(), pageTmpl); err != nil {
		t.Fatal(err)
	}
}

func TestReportRequiredFacts(t *testing.T) {
	cases := map[string]func(*Input){
		"no finding": func(in *Input) { in.Sheet.Finding = nil },
		"no eol":     func(in *Input) { in.Sheet.EOL = time.Time{} },
		"no probe":   func(in *Input) { in.ProbedAt = time.Time{} },
		"no now":     func(in *Input) { in.Now = time.Time{} },
		"no sender":  func(in *Input) { in.Sheet.Sender.Name = "" },
		"no text":    func(in *Input) { in.Major = MajorText{} },
		"no signals": func(in *Input) { in.Signals = nil; in.Sheet.Finding.Evidence = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			in := d7Input()
			f := *in.Sheet.Finding
			in.Sheet.Finding = &f
			mut(&in)
			if _, err := UpgradeRisk(in); err == nil {
				t.Fatal("rendered without a required fact")
			}
		})
	}
}

// TestReportSelfContained: no script, no external asset, and the only link is the
// operator's own site; the probe URL is plain text.
func TestReportSelfContained(t *testing.T) {
	for name, in := range map[string]Input{"d7": d7Input(), "d10": d10Input()} {
		t.Run(name, func(t *testing.T) {
			b, err := UpgradeRisk(in)
			if err != nil {
				t.Fatal(err)
			}
			z := html.NewTokenizer(bytes.NewReader(b))
			var hrefs []string
			for tt := z.Next(); tt != html.ErrorToken; tt = z.Next() {
				if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
					continue
				}
				tok := z.Token()
				switch tok.Data {
				case "script", "iframe", "img", "link", "object", "embed":
					t.Errorf("the page has a <%s>", tok.Data)
				}
				for _, a := range tok.Attr {
					switch a.Key {
					case "src", "srcset", "action":
						t.Errorf("<%s %s=%q>", tok.Data, a.Key, a.Val)
					case "href":
						hrefs = append(hrefs, a.Val)
					}
				}
			}
			if strings.Contains(strings.ToLower(string(b)), "<script") || strings.Contains(string(b), "@import") || strings.Contains(string(b), "url(") {
				t.Error("the page loads or runs something external")
			}
			for _, h := range hrefs {
				if h != in.Sheet.Sender.SiteURL {
					t.Errorf("link to %q: only the operator's own site may be a link", h)
				}
			}
			if strings.Contains(string(b), `href="`+in.Sheet.Finding.URL+`"`) {
				t.Error("the probe URL is a link")
			}
			if !strings.Contains(string(b), in.Sheet.Finding.URL) {
				t.Error("the probe URL is missing")
			}
		})
	}
}

// TestReportEverySourced: every fact line on the page carries its "Source:" note, in plain
// public words: no stored row reference, no internal name, one note for the sender block.
func TestReportEverySourced(t *testing.T) {
	b, err := UpgradeRisk(d7Input())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if n := strings.Count(s, `<span class="src">Source: `); n < 10 {
		t.Errorf("%d source notes, want one per line (at least 10)", n)
	}
	if strings.Contains(s, "Source: not recorded") || strings.Contains(s, "Source: </span>") {
		t.Error("a line has no source")
	}
	if !strings.Contains(s, "worked out from the end-of-life date above and this report&#39;s date, 28 October 2026") {
		t.Error("days left does not say how it is worked out")
	}
	for _, want := range []string{
		"Source: a scan of riverside.example on 20 October 2026</span>",
		"Source: Drupal&#39;s published support schedule</span>",
		"Source: the organisation&#39;s public details</span>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if n := strings.Count(s, "Source: the sender</span>"); n != 1 {
		t.Errorf("%d source notes for the sender block, want 1", n)
	}
	for _, internal := range []string{"ref ", "412", " 17<", "LeadHound", "campaign settings", "operator", "site probe", "org_fact", "<!--"} {
		if strings.Contains(s, internal) {
			t.Errorf("the lead-facing page shows the internal %q", internal)
		}
	}
	if !strings.Contains(s, `<p class="contact">To talk it over, write to alex@sender.example.</p>`) {
		t.Error("no contact line with the address")
	}
}

// TestReportContactAndDomain: without a contact address the contact line links the sender's
// site; an org with no name of its own shows its domain once, as the heading; at a phone's
// width the three figures stack.
func TestReportContactAndDomain(t *testing.T) {
	in := d7Input()
	in.ContactEmail, in.ContactURL = "", "https://sender.example/"
	in.Sheet.Org.Name = ""
	b, err := UpgradeRisk(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `<p class="contact">To talk it over, get in touch through <a href="https://sender.example/">https://sender.example/</a>.</p>`) {
		t.Error("no contact line with the site")
	}
	body := s[strings.Index(s, "<body>"):]
	if n := strings.Count(body, "riverside.example</"); n != 1 || !strings.Contains(s, "<h1>riverside.example</h1>") || strings.Contains(s, `class="domain"`) {
		t.Errorf("the domain shows %d times as a line of its own", n)
	}
	if !strings.Contains(s, "@media screen and (max-width: 560px)") || !strings.Contains(s, ".figures { grid-template-columns: 1fr; }") {
		t.Error("the figures do not stack at a narrow width")
	}
	in.ContactURL = ""
	if b, err = UpgradeRisk(in); err != nil || strings.Contains(string(b), `class="contact"`) {
		t.Errorf("a contact line with nothing to reach: %v", err)
	}
}

func TestReportDaysLeft(t *testing.T) {
	in := d10Input()
	in.Now = time.Date(2026, 12, 9, 23, 0, 0, 0, time.UTC)
	b, err := UpgradeRisk(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "reaches its end of life today") {
		t.Error("EOL day not worded as today")
	}
	in.Now = time.Date(2026, 12, 8, 0, 0, 0, 0, time.UTC)
	if b, err = UpgradeRisk(in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `<span class="big">1</span> day left`) {
		t.Error("one day left not singular")
	}
}

func TestReportSignalWords(t *testing.T) {
	if w, ok := SignalWords("core-asset-v", "7.98", 7); !ok || !strings.Contains(w, "7.98") {
		t.Errorf("core-asset-v: %q %v", w, ok)
	}
	if _, ok := SignalWords("cf-mitigated", "challenge", 7); ok {
		t.Error("a bot challenge was worded as a Drupal signal")
	}
	for name, w := range plainSignals {
		if regexp.MustCompile(`\d`).MatchString(w) {
			t.Errorf("%s words carry a digit: %q", name, w)
		}
	}
}
