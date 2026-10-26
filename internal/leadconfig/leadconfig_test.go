package leadconfig

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/gremy/webbership-leadhound/internal/config"
)

const examplesDir = "../../examples"

// demoDir copies the demo leads.yaml and its templates into a fresh data directory.
func demoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	copyFile(t, filepath.Join(examplesDir, FileName), filepath.Join(dir, FileName))
	src := filepath.Join(examplesDir, "leads-templates")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, "leads-templates", rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o750)
		}
		copyFile(t, path, dst)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from) // #nosec G304 -- test data
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil { // #nosec G703 -- test data in a temp dir
		t.Fatal(err)
	}
}

// replaceIn swaps old for new in dir/rel, failing when old is not there.
func replaceIn(t *testing.T, dir, rel, old, new string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	b, err := os.ReadFile(path) // #nosec G304 -- test data
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s does not contain %q", rel, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o600); err != nil { // #nosec G703 -- test data in a temp dir
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// wantProblem asserts err is ErrInvalid and names every fragment.
func wantProblem(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error; want problems %q", fragments)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error %v is not ErrInvalid", err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not name %q", err, f)
		}
	}
}

func TestDemoLoads(t *testing.T) {
	cfg, err := Load(examplesDir)
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if cfg.Sender.DKIMSelector != "sel1" || cfg.Sender.SiteURL != "https://sender.example/" {
		t.Errorf("sender = %+v", cfg.Sender)
	}
	if len(cfg.Sender.OperatorAddresses) != 1 {
		t.Errorf("operator_addresses = %d entries", len(cfg.Sender.OperatorAddresses))
	}
	if got := cfg.Facts.D10EOL.Format("2006-01-02"); got != "2026-12-09" {
		t.Errorf("d10_eol = %s", got)
	}
	if got := cfg.Caps.StartDate.Format("2006-01-02"); got != "2026-11-16" {
		t.Errorf("start_date = %s", got)
	}
	if !slices.Equal(cfg.KindsPriority, []string{KindAgencyPartner, KindUnsupported, KindD10}) {
		t.Errorf("kinds_priority = %v", cfg.KindsPriority)
	}
	if !slices.Equal(cfg.Copy.FirstWords, []int{90, 120}) || !slices.Equal(cfg.Copy.FollowupWords, []int{70, 80}) {
		t.Errorf("copy = %+v", cfg.Copy)
	}
	for _, kind := range cfg.KindsPriority {
		set, ok := cfg.Templates.Set(kind, "en")
		if !ok || len(set.Steps) != 3 || set.Form == nil || set.LinkedIn == nil {
			t.Errorf("templates %s/en = %+v, %v", kind, set, ok)
		}
	}
	if cfg.Templates.Footers["footer-en"] == nil {
		t.Error("footer-en not loaded")
	}
	if len(cfg.Templates.SHA256) != 64 {
		t.Errorf("templates sha256 = %q", cfg.Templates.SHA256)
	}
	if !cfg.AgencyFinding().MatchString("We migrate Drupal 7 sites") {
		t.Error("agency_finding_pattern not compiled")
	}
	gb := cfg.Policy("GB")
	if gb.Channel != ChannelEmail || gb.Location().String() != "Europe/London" {
		t.Errorf("GB policy = %+v", gb)
	}
	if got := cfg.Policy("FR").Channel; got != ChannelNone {
		t.Errorf("a country missing from the policy has channel %q, want none", got)
	}
}

func TestDemoLoadsForProbe(t *testing.T) {
	cfg, err := LoadForProbe(examplesDir)
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if !slices.Contains(cfg.SiteLists.TLDs, "uk") || cfg.Probe.ReprobeAfterDays != 30 {
		t.Errorf("probe fields = %+v %+v", cfg.SiteLists, cfg.Probe)
	}
	if cfg.Templates.Sets != nil {
		t.Error("LoadForProbe read the templates")
	}
	if cfg.AgencyFinding() == nil || cfg.Policy("DE").Channel != ChannelForm {
		t.Error("probe config not prepared")
	}
}

func TestOperatorAddressesRedacted(t *testing.T) {
	cfg, err := Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sender.OperatorAddresses[0] != config.Secret("operator@sender.example") {
		t.Errorf("operator address = %q", cfg.Sender.OperatorAddresses[0].Reveal())
	}
}

func TestUnknownKeyRefused(t *testing.T) {
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "templates_dir:", "drupalorg_directory: x\ntemplates_dir:")
	_, err := Load(dir)
	wantProblem(t, err, "drupalorg_directory")
	_, err = LoadForProbe(dir)
	wantProblem(t, err, "drupalorg_directory")
}

func TestMissingFile(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want not exist", err)
	}
}

func TestEmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, FileName, "")
	_, err := Load(dir)
	wantProblem(t, err, "empty")
}

func TestTemplateProblems(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(t *testing.T, dir string)
		want   []string
	}{
		{"missing step", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "leads-templates/d10/en/2.txt")); err != nil {
				t.Fatal(err)
			}
		}, []string{"d10/en/2.txt", "missing"}},
		{"missing form", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "leads-templates/unsupported/en/form.txt")); err != nil {
				t.Fatal(err)
			}
		}, []string{"unsupported/en/form.txt", "missing"}},
		{"missing footer", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "leads-templates/footers/footer-en.txt")); err != nil {
				t.Fatal(err)
			}
		}, []string{"footers/footer-en.txt", "missing"}},
		{"unparsable", func(t *testing.T, dir string) {
			replaceIn(t, dir, "leads-templates/d10/en/0.txt", "{{.Host}} and", "{{.Host and")
		}, []string{"d10/en/0.txt", "does not parse"}},
		{"unknown slot", func(t *testing.T, dir string) {
			replaceIn(t, dir, "leads-templates/agency_partner/en/1.txt", "Hello again,", "Hello {{.FirstName}},")
		}, []string{"agency_partner/en/1.txt", "FirstName"}},
		{"unknown slot in subject", func(t *testing.T, dir string) {
			replaceIn(t, dir, "leads-templates/agency_partner/en/0.txt", "Subject: Drupal", "Subject: {{.Price}} Drupal")
		}, []string{"agency_partner/en/0.txt", "Price"}},
		{"footer inside a footer", func(t *testing.T, dir string) {
			replaceIn(t, dir, "leads-templates/footers/footer-en.txt", "{{.OptOutLine}}", "{{.OptOutLine}} {{.Footer}}")
		}, []string{"footers/footer-en.txt", "Footer"}},
		{"no subject line", func(t *testing.T, dir string) {
			replaceIn(t, dir, "leads-templates/d10/en/1.txt", "Subject: ", "Topic: ")
		}, []string{"d10/en/1.txt", "Subject:"}},
		{"no blank line after subject", func(t *testing.T, dir string) {
			replaceIn(t, dir, "leads-templates/d10/en/1.txt", "end of life\n\n", "end of life\n")
		}, []string{"d10/en/1.txt", "blank line"}},
		{"empty form", func(t *testing.T, dir string) {
			writeFile(t, dir, "leads-templates/d10/en/linkedin.txt", "  \n")
		}, []string{"d10/en/linkedin.txt", "empty"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := demoDir(t)
			tc.break_(t, dir)
			_, err := Load(dir)
			wantProblem(t, err, tc.want...)
			if _, err := LoadForProbe(dir); err != nil {
				t.Errorf("LoadForProbe checked the templates: %v", err)
			}
		})
	}
}

func TestConfigProblems(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		want     string
		probe    bool // LoadForProbe must report it too
	}{
		{"ramp over the ceiling", "per_day: 25}", "per_day: 41}", "caps.ramp", false},
		{"ramp not from day 0", "{from_day: 0, per_day: 5}", "{from_day: 1, per_day: 5}", "from_day 0", false},
		{"bad tz", "tz: Europe/London", "tz: Europe/Nowhere", "Europe/Nowhere", true},
		{"bad regexp", `'(?i)drupal\s*(7|8|9|10|11)|upgrade|migrat'`, `'(drupal'`, "agency_finding_pattern", true},
		{"unknown channel", "channel: form", "channel: fax", `"fax"`, true},
		{"bad country code", "DE: {", "Germany: {", "Germany", true},
		{"bad lang", "lang: en, tz: Europe/Berlin", "lang: english, tz: Europe/Berlin", "english", true},
		{"email without footer", ", footer: footer-en}", "}", "footer", false},
		{"no tlds", "tlds: [uk, ie, nl, be, ro, de, at, ch]", "tlds: []", "site_lists.tlds", true},
		{"reprobe zero", "reprobe_after_days: 30", "reprobe_after_days: 0", "reprobe_after_days", true},
		{"role part with @", "office, contact", "office@, contact", "role_local_parts", true},
		{"unknown kind", "kinds_priority: [agency_partner,", "kinds_priority: [hand,", `"hand"`, false},
		{"bad date", "d7_eol: 2025-01-05", "d7_eol: soon", "facts.d7_eol", false},
		{"bad words bound", "first_words: [90, 120]", "first_words: [120, 90]", "copy.first_words", false},
		{"bad words length", "followup_words: [70, 80]", "followup_words: [70]", "copy.followup_words", false},
		{"bad hours", `end: "16:30"`, `end: "08:00"`, "business_hours", false},
		{"bad day", "Thu, Fri]", "Thu, Friday]", "Friday", false},
		{"bad spacing", "spacing_minutes: [3, 9]", "spacing_minutes: [9, 3]", "spacing_minutes", false},
		{"no batch", "batch_size: 10", "batch_size: 0", "batch_size", false},
		{"bad site url", "site_url: https://sender.example/", "site_url: sender.example", "sender.site_url", false},
		{"bad operator address", "[operator@sender.example]", "[operator]", "sender.operator_addresses", false},
		{"email language without opt-out line", "    en: ", "    de: ", "sender.opt_out_lines.en", false},
		{"blank opt-out line", `    en: "Not the right time? Reply \"unsubscribe\" and I will not write to you again."`, `    en: "  "`, "sender.opt_out_lines.en", false},
		{"bad d6 date", "d6_eol: 2016-02-24", "d6_eol: later", "facts.d6_eol", false},
		{"no identity line", `identity_line: "Demo Ltd, 1 Example Street, Example City"`, `identity_line: ""`, "sender.identity_line", false},
		{"absolute templates dir", "templates_dir: leads-templates", "templates_dir: /etc", "templates_dir", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := demoDir(t)
			replaceIn(t, dir, FileName, tc.old, tc.new)
			_, err := Load(dir)
			wantProblem(t, err, tc.want)
			_, perr := LoadForProbe(dir)
			if tc.probe {
				wantProblem(t, perr, tc.want)
			} else if perr != nil {
				t.Errorf("LoadForProbe checked a field outside its set: %v", perr)
			}
		})
	}
}

func TestCeilingConstant(t *testing.T) {
	if HardDailyCeiling != 40 {
		t.Fatalf("HardDailyCeiling = %d, want 40", HardDailyCeiling)
	}
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "per_day: 25}", "per_day: 40}")
	if _, err := Load(dir); err != nil {
		t.Errorf("a ramp at the ceiling is allowed: %v", err)
	}
}

func TestDKIMSelectorDefault(t *testing.T) {
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "  dkim_selector: sel1 # the DKIM selector (the name before ._domainkey) for a mailbox added without a preset or its own\n", "")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sender.DKIMSelector != DefaultDKIMSelector {
		t.Errorf("dkim_selector = %q", cfg.Sender.DKIMSelector)
	}
}

func TestEveryProblemAtOnce(t *testing.T) {
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "tz: Europe/London", "tz: Mars/Base")
	replaceIn(t, dir, FileName, "per_day: 25}", "per_day: 99}")
	replaceIn(t, dir, FileName, "batch_size: 10", "batch_size: 0")
	if err := os.Remove(filepath.Join(dir, "leads-templates/d10/en/0.txt")); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	wantProblem(t, err, "Mars/Base", "caps.ramp", "batch_size", "d10/en/0.txt")
}

// TestRequiredTemplatesFollowPolicy: a language used only by form countries needs form.txt
// and linkedin.txt for each kind, and no email steps.
func TestRequiredTemplatesFollowPolicy(t *testing.T) {
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "DE: {channel: form, lang: en,", "DE: {channel: form, lang: de,")
	_, err := Load(dir)
	wantProblem(t, err, "agency_partner/de/form.txt", "d10/de/linkedin.txt")
	if strings.Contains(err.Error(), "de/0.txt") {
		t.Errorf("a form-only language asked for email steps: %v", err)
	}
	for _, kind := range []string{KindAgencyPartner, KindUnsupported, KindD10} {
		writeFile(t, dir, "leads-templates/"+kind+"/de/form.txt", "Hallo {{.OrgName}}.\n")
		writeFile(t, dir, "leads-templates/"+kind+"/de/linkedin.txt", "Hallo.\n")
	}
	if _, err := Load(dir); err != nil {
		t.Errorf("form-only language with form and linkedin: %v", err)
	}
	// A country with channel none needs no templates at all.
	replaceIn(t, dir, FileName, "DE: {channel: form, lang: de,", "FR: {channel: none, lang: fr,")
	replaceIn(t, dir, FileName, "  GB:", "  DE: {channel: form, lang: de, tz: Europe/Berlin}\n  GB:")
	if _, err := Load(dir); err != nil {
		t.Errorf("channel none asked for templates: %v", err)
	}
}

func TestLoadForProbeNeedsNoTemplates(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join(examplesDir, FileName), filepath.Join(dir, FileName))
	if _, err := LoadForProbe(dir); err != nil {
		t.Fatalf("LoadForProbe without templates: %v", err)
	}
	_, err := Load(dir)
	wantProblem(t, err, "missing")
}

// demoSlots are plausible slot values for rendering the demo; kind picks the evidence.
func demoSlots(t *testing.T, cfg Config, kind string) map[string]string {
	t.Helper()
	s := map[string]string{
		"OrgName":      "Northwind Digital",
		"Domain":       "northwind.example",
		"Host":         "www.northwind.example",
		"Major":        "7",
		"Exact":        "7.98",
		"EvidenceLine": "the page's Generator tag names Drupal 7",
		"FindingURL":   "https://www.northwind.example/",
		"EOLDate":      cfg.Facts.D7EOL.Format("2 January 2006"),
		"SenderName":   cfg.Sender.Name,
		"IdentityLine": cfg.Sender.IdentityLine,
	}
	line, ok := cfg.Sender.OptOutLine("en")
	if !ok {
		t.Fatal("demo has no English opt-out line")
	}
	s["OptOutLine"] = line
	switch kind {
	case KindAgencyPartner:
		s["EvidenceLine"] = "your services page offers Drupal 7 migrations"
		s["FindingURL"] = "https://www.northwind.example/services/"
	case KindD10:
		s["Major"], s["Exact"] = "10", "10.4.2"
		s["EvidenceLine"] = "the page's Generator tag names Drupal 10"
		s["EOLDate"] = cfg.Facts.D10EOL.Format("2 January 2006")
	}
	return s
}

func render(t *testing.T, tmpl *template.Template, slots map[string]string) string {
	t.Helper()
	var b strings.Builder
	if err := tmpl.Execute(&b, slots); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestDemoTemplatesWithinBounds renders every demo email with plausible slots and checks the
// word bounds, the subject rules and the identity and opt-out lines (Task 4.2's Check runs
// the full set later).
func TestDemoTemplatesWithinBounds(t *testing.T) {
	cfg, err := Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	footer := cfg.Templates.Footers[cfg.Policy("GB").Footer]
	for _, kind := range cfg.KindsPriority {
		set, _ := cfg.Templates.Set(kind, "en")
		slots := demoSlots(t, cfg, kind)
		slots["Footer"] = strings.TrimSpace(render(t, footer, slots))
		for step, e := range set.Steps {
			subject, body := render(t, e.Subject, slots), render(t, e.Body, slots)
			// The word bounds count the rendered body above the footer.
			copySlots := maps.Clone(slots)
			copySlots["Footer"] = ""
			words := len(strings.Fields(render(t, e.Body, copySlots)))
			bounds := cfg.Copy.FollowupWords
			if step == 0 {
				bounds = cfg.Copy.FirstWords
			}
			t.Logf("%s/en/%d.txt: %d words (bounds %v), subject %d chars", kind, step, words, bounds, len(subject))
			if words < bounds[0] || words > bounds[1] {
				t.Errorf("%s/en/%d.txt: %d words, want %d-%d", kind, step, words, bounds[0], bounds[1])
			}
			if len(subject) > 70 || strings.Contains(subject, "!") {
				t.Errorf("%s/en/%d.txt: subject %q", kind, step, subject)
			}
			for _, line := range []string{slots["IdentityLine"], slots["OptOutLine"]} {
				if !strings.Contains(body, line) {
					t.Errorf("%s/en/%d.txt lacks %q", kind, step, line)
				}
			}
			for _, banned := range cfg.Copy.BannedWords {
				if strings.Contains(strings.ToLower(body), banned) {
					t.Errorf("%s/en/%d.txt uses %q", kind, step, banned)
				}
			}
		}
		for name, tmpl := range map[string]*template.Template{"form": set.Form, "linkedin": set.LinkedIn} {
			if out := render(t, tmpl, slots); strings.Contains(out, "<no value>") || strings.TrimSpace(out) == "" {
				t.Errorf("%s/en/%s.txt renders %q", kind, name, out)
			}
		}
	}
}

func TestFactsEOL(t *testing.T) {
	cfg, err := Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{6: "2016-02-24", 7: "2025-01-05", 8: "2021-11-17", 9: "2023-11-01", 10: "2026-12-09"}
	for major, date := range want {
		d, ok := cfg.Facts.EOL(major)
		if !ok || d.Format("2006-01-02") != date {
			t.Errorf("EOL(%d) = %v, %v; want %s", major, d, ok, date)
		}
	}
	for _, major := range []int{5, 11, 0} {
		if _, ok := cfg.Facts.EOL(major); ok {
			t.Errorf("EOL(%d) found a date", major)
		}
	}
}

func TestOptOutLine(t *testing.T) {
	cfg, err := Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	if line, ok := cfg.Sender.OptOutLine("en"); !ok || strings.TrimSpace(line) == "" {
		t.Errorf("OptOutLine(en) = %q, %v", line, ok)
	}
	if _, ok := cfg.Sender.OptOutLine("ro"); ok {
		t.Error("OptOutLine(ro) found a line the demo does not have")
	}
}

// TestStartDateFirstSend: caps.start_date takes the word first-send besides a date; no other
// date field does.
func TestStartDateFirstSend(t *testing.T) {
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "start_date: 2026-11-16", "start_date: "+FirstSend)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("first-send refused: %v", err)
	}
	if !cfg.Caps.StartDate.IsFirstSend() || !cfg.Caps.StartDate.IsZero() {
		t.Errorf("start_date = %+v, want first-send", cfg.Caps.StartDate)
	}
	demo, err := Load(examplesDir)
	if err != nil || demo.Caps.StartDate.IsFirstSend() {
		t.Errorf("a fixed date read as first-send (%v)", err)
	}

	dir = demoDir(t)
	replaceIn(t, dir, FileName, "d7_eol: 2025-01-05", "d7_eol: "+FirstSend)
	_, err = Load(dir)
	wantProblem(t, err, "facts.d7_eol")

	dir = demoDir(t)
	replaceIn(t, dir, FileName, "start_date: 2026-11-16", "start_date: first-sent")
	_, err = Load(dir)
	wantProblem(t, err, "caps.start_date", "first-send")
}

// TestDomainCapsAndHealthDefaults: a file without caps.domain_per_day,
// caps.mailboxes_per_domain or a health block loads with the defaults, as the operator's own
// file does before it names them.
func TestDomainCapsAndHealthDefaults(t *testing.T) {
	if HardDomainDailyCeiling != 100 || HardMailboxesPerDomain != 5 {
		t.Fatalf("ceilings = %d, %d, want 100, 5", HardDomainDailyCeiling, HardMailboxesPerDomain)
	}
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "  domain_per_day: 50", "")
	replaceIn(t, dir, FileName, "  mailboxes_per_domain: 2", "")
	replaceIn(t, dir, FileName, "health:", "")
	replaceIn(t, dir, FileName, "  bounce_pause: {bounces: 2, within_sends: 50}", "")
	b, err := os.ReadFile(filepath.Join(dir, FileName)) // #nosec G304 -- test data in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"domain_per_day", "mailboxes_per_domain", "health", "bounce_pause"} {
		if strings.Contains(string(b), key) {
			t.Fatalf("the trimmed file still names %s", key)
		}
	}
	for name, load := range map[string]func(string) (Config, error){"Load": Load, "LoadForProbe": LoadForProbe} {
		cfg, err := load(dir)
		if err != nil {
			t.Fatalf("%s without the new keys: %v", name, err)
		}
		if cfg.Caps.DomainPerDay != DefaultDomainPerDay || cfg.Caps.MailboxesPerDomain != DefaultMailboxesPerDomain {
			t.Errorf("%s caps = %d, %d, want %d, %d", name, cfg.Caps.DomainPerDay, cfg.Caps.MailboxesPerDomain, DefaultDomainPerDay, DefaultMailboxesPerDomain)
		}
		if bp := cfg.Health.BouncePause; bp.Bounces != DefaultBouncePauseBounces || bp.WithinSends != DefaultBouncePauseWithinSends {
			t.Errorf("%s bounce_pause = %+v, want %d within %d", name, bp, DefaultBouncePauseBounces, DefaultBouncePauseWithinSends)
		}
	}
	if DefaultDomainPerDay != 50 || DefaultMailboxesPerDomain != 2 || DefaultBouncePauseBounces != 2 || DefaultBouncePauseWithinSends != 50 {
		t.Error("the defaults moved from 50, 2, 2 and 50")
	}

	demo, err := Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	if demo.Caps.DomainPerDay != 50 || demo.Caps.MailboxesPerDomain != 2 || demo.Health.BouncePause != (BouncePause{Bounces: 2, WithinSends: 50}) {
		t.Errorf("demo caps %+v health %+v", demo.Caps, demo.Health)
	}
}

// TestDomainCapsAndHealthBounds: each new value is checked against its bounds, the ceilings
// included; a value at a bound loads.
func TestDomainCapsAndHealthBounds(t *testing.T) {
	bad := []struct{ old, new, want string }{
		{"domain_per_day: 50", "domain_per_day: 101", "caps.domain_per_day"},
		{"domain_per_day: 50", "domain_per_day: -1", "caps.domain_per_day"},
		{"mailboxes_per_domain: 2", "mailboxes_per_domain: 6", "caps.mailboxes_per_domain"},
		{"mailboxes_per_domain: 2", "mailboxes_per_domain: -1", "caps.mailboxes_per_domain"},
		{"bounces: 2,", "bounces: 11,", "health.bounce_pause.bounces"},
		{"bounces: 2,", "bounces: -1,", "health.bounce_pause.bounces"},
		{"within_sends: 50}", "within_sends: 9}", "health.bounce_pause.within_sends"},
		{"within_sends: 50}", "within_sends: 501}", "health.bounce_pause.within_sends"},
	}
	for _, tc := range bad {
		t.Run(tc.new, func(t *testing.T) {
			dir := demoDir(t)
			replaceIn(t, dir, FileName, tc.old, tc.new)
			_, err := Load(dir)
			wantProblem(t, err, tc.want)
			if _, perr := LoadForProbe(dir); perr != nil {
				t.Errorf("LoadForProbe checked a field outside its set: %v", perr)
			}
		})
	}
	good := []struct{ old, new string }{
		{"domain_per_day: 50", "domain_per_day: 100"},
		{"domain_per_day: 50", "domain_per_day: 1"},
		{"mailboxes_per_domain: 2", "mailboxes_per_domain: 5"},
		{"mailboxes_per_domain: 2", "mailboxes_per_domain: 1"},
		{"bounces: 2,", "bounces: 1,"},
		{"bounces: 2,", "bounces: 10,"},
		{"within_sends: 50}", "within_sends: 10}"},
		{"within_sends: 50}", "within_sends: 500}"},
	}
	for _, tc := range good {
		dir := demoDir(t)
		replaceIn(t, dir, FileName, tc.old, tc.new)
		if _, err := Load(dir); err != nil {
			t.Errorf("%s refused: %v", tc.new, err)
		}
	}
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "health:", "health:\n  bounce_pase: {}")
	_, err := Load(dir)
	wantProblem(t, err, "bounce_pase")
}
