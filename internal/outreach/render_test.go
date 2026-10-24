package outreach

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// renderDemoConfig loads the fictional policy and templates the repository ships.
func renderDemoConfig(t *testing.T) leadconfig.Config {
	t.Helper()
	cfg, err := leadconfig.Load("../../examples")
	if err != nil {
		t.Fatalf("load demo config: %v", err)
	}
	return cfg
}

// renderDemoSlots are slots as the drafting step would fill them for one kind.
func renderDemoSlots(t *testing.T, cfg leadconfig.Config, kind string) Slots {
	t.Helper()
	fetched := func(v string, id int64) Slot { return Slot{Value: v, Source: Provenance{Table: "probe", ID: id}} }
	s := Slots{
		OrgName:      Slot{Value: "Example Trust", Source: Provenance{Table: "org", ID: 7}},
		Domain:       Slot{Value: "trust.example", Source: Provenance{Table: "org", ID: 7}},
		Host:         Slot{Value: "www.trust.example", Source: Provenance{Table: "site", ID: 3}},
		FindingURL:   Slot{Value: "https://www.trust.example/", Source: Provenance{Table: "fetch", ID: 11}},
		SenderName:   ConfigSlot(cfg.Sender.Name),
		IdentityLine: ConfigSlot(cfg.Sender.IdentityLine),
		OptOutLine:   ConfigSlot("If you would rather not hear from me, reply with the word stop."),
	}
	var err error
	switch kind {
	case leadconfig.KindD10:
		s.Major = fetched("10", 21)
		s.Exact = fetched("10.6.14", 21)
		s.EOLDate = ConfigSlot(FormatDate(cfg.Facts.D10EOL.Time))
		s.EvidenceLine.Value, err = EvidenceLine(drupal.Signal{Name: drupal.SigCoreAssetV, Value: "10.6.14"}, 10)
		s.EvidenceLine.Source = Provenance{Table: "probe", ID: 21}
	case leadconfig.KindUnsupported:
		s.Major = fetched("7", 22)
		s.EOLDate = ConfigSlot(FormatDate(cfg.Facts.D7EOL.Time))
		s.EvidenceLine.Value, err = EvidenceLine(drupal.Signal{Name: drupal.SigD7Settings}, 7)
		s.EvidenceLine.Source = Provenance{Table: "probe", ID: 22}
	case leadconfig.KindAgencyPartner:
		s.Major = fetched("9", 23)
		s.EvidenceLine.Value, err = EvidenceLine(drupal.Signal{Name: drupal.SigXGenerator, Value: "9"}, 9)
		s.EvidenceLine.Source = Provenance{Table: "probe", ID: 23}
	}
	if err != nil {
		t.Fatalf("evidence line: %v", err)
	}
	return s
}

// renderDemoTemplates lists every demo text of one kind: the three emails, the form and the
// LinkedIn note, each with the step Check is given.
func renderDemoTemplates(t *testing.T, cfg leadconfig.Config, kind string) (names []string, tmpls []Template, steps []int) {
	t.Helper()
	set, ok := cfg.Templates.Set(kind, "en")
	if !ok {
		t.Fatalf("no demo set for %s/en", kind)
	}
	footer := cfg.Templates.Footers[cfg.Policy("GB").Footer]
	for i, e := range set.Steps {
		names = append(names, kind+"/"+string(rune('0'+i)))
		tmpls = append(tmpls, EmailTemplate(e, footer))
		steps = append(steps, i)
	}
	names = append(names, kind+"/form", kind+"/linkedin")
	tmpls = append(tmpls, FormTemplate(set.Form), LinkedInTemplate(set.LinkedIn))
	steps = append(steps, 0, 0)
	return names, tmpls, steps
}

func TestSlotNamesMatchConfig(t *testing.T) {
	var got []string
	for _, ns := range (Slots{}).named() {
		got = append(got, ns.name)
	}
	if !slices.Equal(got, leadconfig.SlotNames) {
		t.Fatalf("slots %v, config slot names %v", got, leadconfig.SlotNames)
	}
}

func TestRenderDemoTemplatesPassCheck(t *testing.T) {
	cfg := renderDemoConfig(t)
	for _, kind := range leadconfig.Kinds {
		names, tmpls, steps := renderDemoTemplates(t, cfg, kind)
		slots := renderDemoSlots(t, cfg, kind)
		for i, tm := range tmpls {
			d, err := Render(tm, slots)
			if err != nil {
				t.Errorf("%s: render: %v", names[i], err)
				continue
			}
			if p := Check(d, steps[i], cfg); len(p) > 0 {
				t.Errorf("%s: problems %v\n%s", names[i], p, d.Body)
			}
		}
	}
}

func TestRenderFooterSeparate(t *testing.T) {
	cfg := renderDemoConfig(t)
	_, tmpls, _ := renderDemoTemplates(t, cfg, leadconfig.KindD10)
	d, err := Render(tmpls[0], renderDemoSlots(t, cfg, leadconfig.KindD10))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.BodyAboveFooter, cfg.Sender.IdentityLine) {
		t.Errorf("the copy above the footer holds the footer:\n%s", d.BodyAboveFooter)
	}
	if !strings.HasPrefix(d.Body, d.BodyAboveFooter) || !strings.Contains(d.Body, d.Slots.Footer.Value) {
		t.Errorf("the body is not the copy then the footer:\n%s", d.Body)
	}
	if d.Slots.Footer.Source != ConfigSource {
		t.Errorf("footer source %v", d.Slots.Footer.Source)
	}
	if d.Kind != DraftEmail || d.Subject == "" || strings.Contains(d.Subject, "\n") {
		t.Errorf("kind %v subject %q", d.Kind, d.Subject)
	}
}

func TestRenderAppendsFooterWhenTemplateOmitsIt(t *testing.T) {
	tm := EmailTemplate(leadconfig.Email{
		Subject: template.Must(template.New("s").Option("missingkey=error").Parse("Hi {{.OrgName}}")),
		Body:    template.Must(template.New("b").Option("missingkey=error").Parse("Hello {{.OrgName}}.\n")),
	}, template.Must(template.New("f").Option("missingkey=error").Parse("{{.SenderName}}")))
	d, err := Render(tm, Slots{OrgName: Slot{Value: "Org", Source: Provenance{"org", 1}}, SenderName: ConfigSlot("Sender")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "Hello Org.\n\nSender\n" || d.BodyAboveFooter != "Hello Org.\n\n" {
		t.Errorf("body %q above %q", d.Body, d.BodyAboveFooter)
	}
}

func TestRenderMissingFactMeansNoDraft(t *testing.T) {
	cfg := renderDemoConfig(t)
	_, tmpls, _ := renderDemoTemplates(t, cfg, leadconfig.KindD10)
	s := renderDemoSlots(t, cfg, leadconfig.KindD10)
	s.EOLDate = Slot{}
	if _, err := Render(tmpls[0], s); err == nil {
		t.Fatal("a template that uses an empty slot rendered")
	}
	// A slot the template does not use may stay empty.
	s = renderDemoSlots(t, cfg, leadconfig.KindAgencyPartner)
	s.EOLDate, s.Exact = Slot{}, Slot{}
	if _, err := Render(tmpls[0], s); err == nil {
		t.Fatal("d10 email rendered without its end-of-life date")
	}
	_, atm, _ := renderDemoTemplates(t, cfg, leadconfig.KindAgencyPartner)
	if _, err := Render(atm[0], s); err != nil {
		t.Fatalf("an unused empty slot refused the draft: %v", err)
	}
}

func TestRenderTextHasNoFooterSlot(t *testing.T) {
	tm := FormTemplate(template.Must(template.New("f").Option("missingkey=error").Parse("x {{.Footer}}")))
	if _, err := Render(tm, Slots{Footer: ConfigSlot("smuggled")}); err == nil {
		t.Fatal("a form text rendered a footer")
	}
}

func TestRenderRefusesTextAfterFooter(t *testing.T) {
	tm := EmailTemplate(leadconfig.Email{
		Subject: template.Must(template.New("s").Parse("Hi")),
		Body:    template.Must(template.New("b").Option("missingkey=error").Parse("Hello.\n{{.Footer}}\nP.S. more")),
	}, template.Must(template.New("f").Parse("Sender")))
	if _, err := Render(tm, Slots{}); err == nil {
		t.Fatal("copy below the footer rendered")
	}
}

// TestSlotProvenanceSweep renders every demo text twice: once with a marker for each slot,
// to learn where each slot sits, and once with real values. Every real value must occur in
// the body exactly as often as the slots that carry it (itself, or a slot whose value
// contains it) put it there: never through the template's own text. Every slot the text
// uses must name a source.
func TestSlotProvenanceSweep(t *testing.T) {
	cfg := renderDemoConfig(t)
	for _, kind := range leadconfig.Kinds {
		names, tmpls, _ := renderDemoTemplates(t, cfg, kind)
		real := renderDemoSlots(t, cfg, kind)
		var marked Slots
		for _, ns := range real.named() {
			if ns.name != "Footer" && ns.slot.Value != "" {
				*marked.field(ns.name) = Slot{Value: "⟦" + ns.name + "⟧", Source: ns.slot.Source}
			}
		}
		for i, tm := range tmpls {
			md, err := Render(tm, marked)
			if err != nil {
				t.Fatalf("%s: marked render: %v", names[i], err)
			}
			rd, err := Render(tm, real)
			if err != nil {
				t.Fatalf("%s: render: %v", names[i], err)
			}
			text := rd.Subject + "\n" + rd.Body
			mtext := md.Subject + "\n" + md.Body
			for _, ns := range real.named() {
				if ns.name == "Footer" || ns.slot.Value == "" {
					continue
				}
				want := 0
				for _, other := range real.named() {
					if other.name == "Footer" || other.slot.Value == "" {
						continue
					}
					want += strings.Count(mtext, "⟦"+other.name+"⟧") * strings.Count(other.slot.Value, ns.slot.Value)
				}
				if got := strings.Count(text, ns.slot.Value); got != want {
					t.Errorf("%s: %s %q occurs %d times, its slots put it there %d times", names[i], ns.name, ns.slot.Value, got, want)
				}
			}
			for _, name := range rd.Used {
				s := *rd.Slots.field(name)
				if s.Source.Table == "" {
					t.Errorf("%s: slot %s has no source", names[i], name)
				}
			}
		}
	}
}

func TestEvidenceLine(t *testing.T) {
	cases := []struct {
		sig   drupal.Signal
		major int
		want  string
	}{
		{drupal.Signal{Name: drupal.SigMetaGenerator, Value: "10"}, 10, "the page's Generator tag names Drupal 10"},
		{drupal.Signal{Name: drupal.SigXGenerator, Value: "9"}, 9, "the X-Generator header names Drupal 9"},
		{drupal.Signal{Name: drupal.SigD7Settings}, 7, "the site's settings block is Drupal 7's"},
		{drupal.Signal{Name: drupal.SigCoreAssetV, Value: "10.6.14"}, 10, "the page loads Drupal's core files at version 10.6.14"},
	}
	for _, c := range cases {
		got, err := EvidenceLine(c.sig, c.major)
		if err != nil || got != c.want {
			t.Errorf("%v: %q, %v; want %q", c.sig, got, err, c.want)
		}
	}
	bad := []struct {
		sig   drupal.Signal
		major int
	}{
		{drupal.Signal{Name: drupal.SigMetaGenerator, Value: "10"}, 9}, // the value disagrees with the major
		{drupal.Signal{Name: drupal.SigCoreAssetV, Value: "9.5.1"}, 10},
		{drupal.Signal{Name: drupal.SigD7Settings}, 0},
		{drupal.Signal{Name: drupal.SigCorePaths}, 10}, // a signal that names no version
		{drupal.Signal{Name: drupal.SigXGenerator, Value: ""}, 10},
	}
	for _, b := range bad {
		if got, err := EvidenceLine(b.sig, b.major); err == nil {
			t.Errorf("%v major %d: %q, want an error", b.sig, b.major, got)
		}
	}
}

func TestOrgNameFallsBackToDomain(t *testing.T) {
	dom := Slot{Value: "trust.example", Source: Provenance{"org", 4}}
	if got := OrgNameSlot(Slot{}, dom); got != dom {
		t.Errorf("empty name: %v", got)
	}
	name := Slot{Value: "Example Trust", Source: Provenance{"org", 4}}
	if got := OrgNameSlot(name, dom); got != name {
		t.Errorf("named: %v", got)
	}
	if got := OrgNameSlot(Slot{Value: "  ", Source: Provenance{"org", 4}}, dom); got != dom {
		t.Errorf("blank name: %v", got)
	}
}

func TestFormatDate(t *testing.T) {
	if got := FormatDate(time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC)); got != "9 December 2026" {
		t.Errorf("got %q", got)
	}
}

func TestNewMessageID(t *testing.T) {
	id, err := NewMessageID("mail.example", bytes.NewReader(bytes.Repeat([]byte{0xab}, 16)))
	if err != nil || id != strings.Repeat("ab", 16)+"@mail.example" {
		t.Fatalf("%q, %v", id, err)
	}
	if _, err := NewMessageID("mail.example", bytes.NewReader([]byte{1, 2})); err == nil {
		t.Error("a short random read made an id")
	}
	for _, d := range []string{"", "a@b.example", "has space.example", "<x>.example"} {
		if _, err := NewMessageID(d, bytes.NewReader(make([]byte, 16))); err == nil {
			t.Errorf("domain %q made an id", d)
		}
	}
	if _, err := NewMessageID("mail.example", renderErrReader{}); err == nil {
		t.Error("a failing reader made an id")
	}
}

type renderErrReader struct{}

func (renderErrReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestMessageIDDomain(t *testing.T) {
	cases := map[string]string{
		"outreach@Send.Example": "send.example",
		"":                      ShadowDomain,
		"no-at-sign":            ShadowDomain,
		"user@":                 ShadowDomain,
		"user@localhost":        ShadowDomain,
	}
	for in, want := range cases {
		if got := MessageIDDomain(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
