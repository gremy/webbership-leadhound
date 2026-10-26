package outreach

import (
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// checkDraft is a passing d10 first email from the demo; each case breaks one thing.
func checkDraft(t *testing.T) (Draft, leadconfig.Config) {
	t.Helper()
	cfg := renderDemoConfig(t)
	_, tmpls, _ := renderDemoTemplates(t, cfg, leadconfig.KindD10)
	d, err := Render(tmpls[0], renderDemoSlots(t, cfg, leadconfig.KindD10))
	if err != nil {
		t.Fatal(err)
	}
	if p := Check(d, 0, cfg); len(p) > 0 {
		t.Fatalf("base draft fails: %v", p)
	}
	return d, cfg
}

func checkRules(p []Problem) []string {
	var r []string
	for _, x := range p {
		if !slices.Contains(r, x.Rule) {
			r = append(r, x.Rule)
		}
	}
	return r
}

// checkInsert adds text to the copy above the footer, keeping Body and BodyAboveFooter in step.
func checkInsert(d Draft, text string) Draft {
	copyText := strings.TrimRight(d.BodyAboveFooter, "\n")
	above := copyText + text + d.BodyAboveFooter[len(copyText):]
	d.Body = strings.Replace(d.Body, d.BodyAboveFooter, above, 1)
	d.BodyAboveFooter = above
	return d
}

func TestCheckEachRuleFails(t *testing.T) {
	base, cfg := checkDraft(t)
	cases := []struct {
		name string
		edit func(d Draft, cfg *leadconfig.Config) (Draft, int)
		rule string
	}{
		{"too few words", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.BodyAboveFooter = "Hello there.\n"
			return d, 0
		}, RuleWords},
		{"first email bounds on a follow-up", func(d Draft, _ *leadconfig.Config) (Draft, int) { return d, 1 }, RuleWords},
		{"step out of range", func(d Draft, _ *leadconfig.Config) (Draft, int) { return d, leadconfig.Steps }, RuleStep},
		{"leftover placeholder", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			return checkInsert(d, "{{.Major}}"), 0
		}, RulePlaceholder},
		{"no value", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Subject += " <no value>"
			return d, 0
		}, RulePlaceholder},
		{"empty used slot", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Slots.Host = Slot{Source: d.Slots.Host.Source}
			return d, 0
		}, RuleEmptySlot},
		{"slot without a source", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Slots.Major.Source = Provenance{}
			return d, 0
		}, RuleSource},
		{"stored slot without a row id", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Slots.Major.Source.ID = 0
			return d, 0
		}, RuleSource},
		{"banned word, any case", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			return checkInsert(d, " We GUARANTEE it."), 0
		}, RuleBanned},
		{"opt-out line missing", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Body = strings.ReplaceAll(d.Body, d.Slots.OptOutLine.Value, "")
			return d, 0
		}, RuleOptOut},
		{"empty opt-out line", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Slots.OptOutLine = Slot{}
			return d, 0
		}, RuleOptOut},
		// FO2.4: our text may hold an opt-out word only in the opt-out line.
		{"opt-out word in the subject", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Subject = "Drupal 7: unsubscribe anytime"
			return d, 0
		}, RuleOptOutWord},
		{"opt-out word outside the opt-out line", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			return checkInsert(d, " You can opt out at any time."), 0
		}, RuleOptOutWord},
		{"Romanian opt-out word outside the opt-out line", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			return checkInsert(d, " Dezabonare oricând."), 0
		}, RuleOptOutWord},
		{"identity line missing", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Body = strings.ReplaceAll(d.Body, d.Slots.IdentityLine.Value, "")
			return d, 0
		}, RuleIdentity},
		{"a number from nowhere", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			return checkInsert(d, " It has 40 modules."), 0
		}, RuleNumber},
		{"a slot smuggles a second number", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Slots.OrgName.Value = "Example Trust, since 1998"
			d.Body = strings.ReplaceAll(d.Body, "Example Trust", d.Slots.OrgName.Value)
			return d, 0
		}, RuleNumber},
		{"subject too long", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Subject = strings.Repeat("x", 71)
			return d, 0
		}, RuleSubjectLength},
		{"subject shouts", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			d.Subject += "!"
			return d, 0
		}, RuleSubjectBang},
		{"a foreign URL", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			return checkInsert(d, " See https://other.example/page."), 0
		}, RuleURL},
		{"a www link", func(d Draft, _ *leadconfig.Config) (Draft, int) {
			return checkInsert(d, " See www.other.example."), 0
		}, RuleURL},
		{"the site URL when config has none", func(d Draft, cfg *leadconfig.Config) (Draft, int) {
			cfg.Sender.SiteURL = ""
			return checkInsert(d, " Me: https://sender.example/"), 0
		}, RuleURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ccfg := cfg
			d, step := c.edit(base, &ccfg)
			got := checkRules(Check(d, step, ccfg))
			if !slices.Contains(got, c.rule) {
				t.Errorf("rules %v, want %s", got, c.rule)
			}
		})
	}
}

func TestCheckEachRulePasses(t *testing.T) {
	base, cfg := checkDraft(t)
	cases := []struct {
		name string
		edit func(d Draft) (Draft, int)
	}{
		{"the base draft", func(d Draft) (Draft, int) { return d, 0 }},
		{"the footer is not counted", func(d Draft) (Draft, int) {
			d.Body += strings.Repeat(" word", 200)
			return d, 0
		}},
		{"a banned word inside a longer word", func(d Draft) (Draft, int) {
			return checkInsert(d, " guaranteed"), 0
		}},
		{"a fixed-text number and a slot number", func(d Draft) (Draft, int) {
			d.FixedText += " 30 days"
			return checkInsert(d, " Within 30 days, Drupal 10.6.14."), 0
		}},
		{"a name with digits glued to letters", func(d Draft) (Draft, int) {
			return checkInsert(d, " web2 h2o"), 0
		}},
		{"numbers inside an allowed URL", func(d Draft) (Draft, int) {
			const u = "https://www.trust.example/node/42"
			d.Body = strings.ReplaceAll(d.Body, d.Slots.FindingURL.Value, u)
			d.BodyAboveFooter = strings.ReplaceAll(d.BodyAboveFooter, d.Slots.FindingURL.Value, u)
			d.Slots.FindingURL.Value = u
			return checkInsert(d, " ("+u+")."), 0
		}},
		{"the sender's own site, with or without its slash", func(d Draft) (Draft, int) {
			return checkInsert(d, " https://sender.example"), 0
		}},
		{"subject of exactly 70", func(d Draft) (Draft, int) {
			d.Subject = strings.Repeat("x", 70)
			return d, 0
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, step := c.edit(base)
			if p := Check(d, step, cfg); len(p) > 0 {
				t.Errorf("problems %v", p)
			}
		})
	}
}

func TestCheckFollowupBounds(t *testing.T) {
	cfg := renderDemoConfig(t)
	_, tmpls, _ := renderDemoTemplates(t, cfg, leadconfig.KindD10)
	d, err := Render(tmpls[1], renderDemoSlots(t, cfg, leadconfig.KindD10))
	if err != nil {
		t.Fatal(err)
	}
	if p := Check(d, 1, cfg); len(p) > 0 {
		t.Fatalf("follow-up: %v", p)
	}
	if got := checkRules(Check(d, 0, cfg)); !slices.Contains(got, RuleWords) {
		t.Errorf("a follow-up passed the first-email bounds: %v", got)
	}
}

// Form and LinkedIn texts skip the word bounds, the subject and the footer lines, and keep
// every other rule.
func TestCheckTexts(t *testing.T) {
	cfg := renderDemoConfig(t)
	_, tmpls, _ := renderDemoTemplates(t, cfg, leadconfig.KindD10)
	d, err := Render(tmpls[4], renderDemoSlots(t, cfg, leadconfig.KindD10)) // linkedin
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != DraftLinkedIn || d.Subject != "" {
		t.Fatalf("kind %v subject %q", d.Kind, d.Subject)
	}
	if p := Check(d, 0, cfg); len(p) > 0 {
		t.Fatalf("linkedin: %v", p)
	}
	bad := d
	bad.Body += " Over 500 sites, guarantee, https://other.example/"
	bad.BodyAboveFooter = bad.Body
	got := checkRules(Check(bad, 0, cfg))
	for _, r := range []string{RuleNumber, RuleBanned, RuleURL} {
		if !slices.Contains(got, r) {
			t.Errorf("linkedin rules %v, want %s", got, r)
		}
	}
	for _, r := range []string{RuleWords, RuleOptOut, RuleIdentity, RuleSubjectLength} {
		if slices.Contains(got, r) {
			t.Errorf("linkedin got the email-only rule %s", r)
		}
	}
}

func TestNumbers(t *testing.T) {
	got := checkNumbers("Drupal 10.6.14 on 2026-12-09, at 09:30; web2 and v3 aside, 7's (42). See https://x.example/1/2 and 3/4.")
	want := []string{"10.6.14", "2026-12-09", "09:30", "7", "42", "3/4"}
	if !slices.Equal(got, want) {
		t.Errorf("numbers %v, want %v", got, want)
	}
}

// A core-files evidence line names the full version; with no Exact slot its number is
// allowed because the stored signal names the same version, and refused when it does not.
func TestCheckCoreFilesEvidenceNumber(t *testing.T) {
	cfg := renderDemoConfig(t)
	_, tmpls, _ := renderDemoTemplates(t, cfg, leadconfig.KindD10)
	s := renderDemoSlots(t, cfg, leadconfig.KindD10)
	s.Exact = Slot{}
	d, err := Render(tmpls[0], s)
	if err != nil {
		t.Fatal(err)
	}
	if p := Check(d, 0, cfg); len(p) > 0 {
		t.Fatalf("core-files line without Exact: %v", p)
	}
	d.Slots.EvidenceSignal.Value = "10.6.13"
	if got := checkRules(Check(d, 0, cfg)); !slices.Contains(got, RuleNumber) {
		t.Errorf("a version the stored signal does not hold passed: %v", got)
	}
	d.Slots.EvidenceSignal = drupal.Signal{}
	if got := checkRules(Check(d, 0, cfg)); !slices.Contains(got, RuleNumber) {
		t.Errorf("an evidence line without its signal passed: %v", got)
	}
}
