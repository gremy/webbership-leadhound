package outreach

import (
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// TestCheckCampaignLinks: a URL in StepRules.Links (a file campaign's product.site_url and
// offer.links) and the lead's VendorLink pass the URL rule; an unlisted URL still fails, and
// the privacy notice still belongs in the footer only.
func TestCheckCampaignLinks(t *testing.T) {
	base, cfg := checkDraft(t)
	rules := StepRules{Links: []string{"https://demo.example/", "https://product.example"}}
	urlProblems := func(d Draft, cfg leadconfig.Config, r StepRules) []Problem {
		var out []Problem
		for _, p := range CheckIn(d, 0, cfg, r) {
			if p.Rule == RuleURL {
				out = append(out, p)
			}
		}
		return out
	}
	for _, c := range []struct {
		name string
		edit func(Draft) Draft
		r    StepRules
		fail bool
	}{
		{"an offer link", func(d Draft) Draft { return checkInsert(d, " Demo: https://demo.example") }, rules, false},
		{"the product site, any case and slash", func(d Draft) Draft { return checkInsert(d, " See HTTPS://product.example/") }, rules, false},
		{"an offer link without the campaign's rules", func(d Draft) Draft { return checkInsert(d, " Demo: https://demo.example") }, StepRules{}, true},
		{"an unlisted URL", func(d Draft) Draft { return checkInsert(d, " See https://unlisted.example/page") }, rules, true},
		{"the lead's vendor link", func(d Draft) Draft {
			d.Slots.VendorLink = ConfigSlot("https://compare.example/vs/stampino")
			return checkInsert(d, " Compare: https://compare.example/vs/stampino")
		}, StepRules{}, false},
		{"another vendor's link", func(d Draft) Draft {
			d.Slots.VendorLink = ConfigSlot("https://compare.example/vs/stampino")
			return checkInsert(d, " Compare: https://compare.example/vs/other")
		}, StepRules{}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := urlProblems(c.edit(base), cfg, c.r)
			if c.fail != (len(got) > 0) {
				t.Errorf("url problems %v, want fail=%v", got, c.fail)
			}
		})
	}

	// The privacy URL above the footer still fails, whatever the campaign allows.
	pcfg := cfg
	pcfg.Sender.PrivacyURL = "https://sender.example/privacy"
	if got := urlProblems(checkInsert(base, " https://sender.example/privacy"), pcfg, rules); len(got) == 0 {
		t.Error("the privacy URL above the footer passed")
	}
}

// TestOptionalVendorSlots: a template may test VendorName and VendorLink while they are
// empty (Render passes them, Check does not call them missing), and prints them when set.
func TestOptionalVendorSlots(t *testing.T) {
	_, cfg := checkDraft(t)
	_, tmpls, _ := renderDemoTemplates(t, cfg, leadconfig.KindD10)
	tm := tmpls[0]
	body := strings.Replace(tm.Body.Root.String(), "{{.Footer}}", "", 1)
	body += "{{if .VendorName}}On {{.VendorName}}: {{.VendorLink}}{{else}}Compare: {{.VendorLink}}{{end}}\n\n{{.Footer}}"
	tm.Body = template.Must(template.New("b").Option("missingkey=error").Parse(body))
	slots := renderDemoSlots(t, cfg, leadconfig.KindD10)
	cases := []struct {
		name, link, want string
		vendor           string
	}{
		{"no vendor, default link", "https://compare.example/vs", "Compare: https://compare.example/vs", ""},
		{"a vendor and its link", "https://compare.example/vs/stampino", "On Stampino: https://compare.example/vs/stampino", "Stampino"},
		{"no vendor, no link", "", "Compare: ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := slots
			if c.vendor != "" {
				s.VendorName = Slot{Value: c.vendor, Source: Provenance{Table: "site_probe", ID: 7}}
			}
			if c.link != "" {
				s.VendorLink = ConfigSlot(c.link)
			}
			d, err := Render(tm, s)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(d.Body, c.want) {
				t.Errorf("body lacks %q:\n%s", c.want, d.Body)
			}
			for _, p := range CheckIn(d, 1, cfg, StepRules{}) {
				if slices.Contains([]string{RuleEmptySlot, RuleSource, RulePlaceholder, RuleURL}, p.Rule) {
					t.Errorf("problem %v", p)
				}
			}
		})
	}
}
