package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// Fictional values only: the repository is public.
const (
	cafeTestDemo        = "https://demo.example/"
	cafeTestVsStampino  = "https://compare.example/vs/stampino"
	cafeTestVsDefault   = "https://compare.example/vs"
	cafeTestFooterMark  = "Demo Loyalty pentru cafenele"
	cafeTestOptOutRO    = "Nu e momentul potrivit? Răspunde cu „stop” și nu îți mai scriu."
	cafeTestLegacyLabel = "footer-en"
)

// cafeWorld is the example café campaign copied into a temp data dir, with its RO country
// on its own footer (footers/footer-cafe-ro.txt, beside leads.yaml's), an offer link, vendor
// links, and a step 1 that names the vendor. RO has no country in the example leads.yaml, so
// the loaded config gets one in memory: email, Romanian, on leads.yaml's footer-en.
func cafeWorld(t *testing.T, campaignFooter bool) (string, campaign.Spec, leadconfig.Config) {
	t.Helper()
	dir := t.TempDir()
	copyTree := func(rel string) {
		if err := os.CopyFS(filepath.Join(dir, rel), os.DirFS(filepath.Join(examplesDir, rel))); err != nil {
			t.Fatal(err)
		}
	}
	copyTree("leads-templates")
	copyTree(filepath.Join(campaign.Dir, "cafes-loyalty"))
	write := func(rel, text string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(text), 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(examplesDir, leadconfig.FileName))
	if err != nil {
		t.Fatal(err)
	}
	write(leadconfig.FileName, string(b))
	write("leads-templates/footers/footer-cafe-ro.txt", "{{.SenderName}}, "+cafeTestFooterMark+"\n{{.IdentityLine}}\n{{.OptOutLine}}\n")

	specRel := filepath.Join(campaign.Dir, "cafes-loyalty", campaign.FileName)
	sb, err := os.ReadFile(filepath.Join(dir, specRel)) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	spec := string(sb)
	replace := func(old, new string) {
		t.Helper()
		if !strings.Contains(spec, old) {
			t.Fatalf("the example spec no longer holds %q", old)
		}
		spec = strings.Replace(spec, old, new, 1)
	}
	if campaignFooter {
		replace("RO: {email: on, call: on, lang: ro}", "RO: {email: on, call: on, lang: ro, footer: footer-cafe-ro}")
	}
	// The example spec carries the offer link and vendor links these tests rely on.
	for _, want := range []string{`links: ["` + cafeTestDemo + `"]`, `stampino: "` + cafeTestVsStampino + `"`, `default: "` + cafeTestVsDefault + `"`} {
		if !strings.Contains(spec, want) {
			t.Fatalf("the example spec no longer holds %q", want)
		}
	}
	write(specRel, spec)

	step1 := filepath.Join(campaign.Dir, "cafes-loyalty", "templates", "cafe_no_digital_loyalty", "ro", "1.txt")
	tb, err := os.ReadFile(filepath.Join(dir, step1)) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	t1 := strings.Replace(string(tb), "{{.Footer}}",
		"Demo: "+cafeTestDemo+"\n{{if .VendorName}}Față de {{.VendorName}}: {{.VendorLink}}{{else}}Comparația: {{.VendorLink}}{{end}}\n\n{{.Footer}}", 1)
	write(step1, t1)

	cfg, err := leadconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.Clone()
	cfg.Countries["RO"] = leadconfig.Country{Channel: leadconfig.ChannelEmail, Lang: "ro", TZ: "Europe/Bucharest", Footer: cafeTestLegacyLabel}
	cfg.Sender.OptOutLines["ro"] = cafeTestOptOutRO

	sp, err := campaign.Load(filepath.Join(dir, specRel))
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Check(campaign.CheckOptions{KnownKinds: campaign.KnownKinds()}); err != nil {
		t.Fatalf("the test spec fails its checks: %v", err)
	}
	return dir, sp, cfg
}

// cafeLead is a fictional Romanian café lead with an email route and the given stored signals.
func cafeLead(signals ...string) (leadstore.Candidate, leadstore.Evidence) {
	probeID := int64(31)
	c := leadstore.Candidate{LeadID: 7, OrgID: 3, Domain: "cafenea.example", OrgName: "Cafeneaua Exemplu", Country: "RO",
		Kind: "cafe_no_digital_loyalty", Channel: leadconfig.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "salut@cafenea.example"}
	ev := leadstore.Evidence{LeadID: 7, ProbeID: &probeID, FinalHost: "cafenea.example"}
	for _, s := range signals {
		kind, _, _ := strings.Cut(s, ":")
		ev.Signals = append(ev.Signals, leadstore.EvidenceSignal{Signal: leadstore.Signal{Signal: kind, Value: s}})
	}
	return c, ev
}

// draftCafe drafts lead c on the café campaign's own plan (no number to call).
func draftCafe(t *testing.T, dir string, spec campaign.Spec, cfg leadconfig.Config, c leadstore.Candidate, ev leadstore.Evidence) []leadstore.DraftRow {
	t.Helper()
	plan, err := filePlan(dir, spec, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := draftSequence(plan, c, ev, cfg, nil, "sender.example", zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows drafted")
	}
	return rows
}

// legacyFooterText is leads.yaml's RO footer rendered with the lead's own slots.
func legacyFooterText(t *testing.T, cfg leadconfig.Config) string {
	t.Helper()
	var b bytes.Buffer
	if err := cfg.Templates.Footers[cafeTestLegacyLabel].Execute(&b, map[string]string{
		"SenderName": cfg.Sender.Name, "IdentityLine": cfg.Sender.IdentityLine, "OptOutLine": cafeTestOptOutRO,
	}); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(b.String())
}

// TestCafeCampaignFooter: a café RO email ends with the campaign country's own footer, never
// leads.yaml's (sweep over every drafted email); a spec country without a footer keeps
// leads.yaml's.
func TestCafeCampaignFooter(t *testing.T) {
	dir, spec, cfg := cafeWorld(t, true)
	legacy := legacyFooterText(t, cfg)
	c, ev := cafeLead()
	for _, r := range draftCafe(t, dir, spec, cfg, c, ev) {
		if r.Channel != "email" {
			continue
		}
		if !strings.Contains(r.Body, cafeTestFooterMark) {
			t.Errorf("step %d lacks the campaign footer:\n%s", r.Step, r.Body)
		}
		if strings.Contains(r.Body, legacy) {
			t.Errorf("step %d carries leads.yaml's footer:\n%s", r.Step, r.Body)
		}
	}

	dir, spec, cfg = cafeWorld(t, false)
	for _, r := range draftCafe(t, dir, spec, cfg, c, ev) {
		if r.Channel == "email" && (!strings.Contains(r.Body, legacy) || strings.Contains(r.Body, cafeTestFooterMark)) {
			t.Errorf("without a campaign footer, step %d should end with leads.yaml's:\n%s", r.Step, r.Body)
		}
	}
}

// TestCafeCampaignFooterMissing: a campaign footer with no file is refused when the plan
// loads, naming the file.
func TestCafeCampaignFooterMissing(t *testing.T) {
	dir, spec, cfg := cafeWorld(t, true)
	if err := os.Remove(filepath.Join(dir, "leads-templates", "footers", "footer-cafe-ro.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := filePlan(dir, spec, cfg); err == nil || !strings.Contains(err.Error(), "footers/footer-cafe-ro.txt is missing") {
		t.Errorf("filePlan: %v; want the missing footer named", err)
	}
}

// TestCafeVendorSlots: a café on Stampino is named with its own link; a café with only
// wallet words (no vendor) gets no name and the default link; both pass Check with the offer
// link in the email, and the slots are stored with the draft.
func TestCafeVendorSlots(t *testing.T) {
	dir, spec, cfg := cafeWorld(t, true)
	cases := []struct {
		name, signal, vendor, link, line string
	}{
		{"stampino", cafe.SigLoyalty + ":stampino:" + cafe.ViaLink + ":https://stampino.ro/c/exemplu", "Stampino", cafeTestVsStampino,
			"Față de Stampino: " + cafeTestVsStampino},
		{"wallet words", cafe.SigLoyalty + ":wallet-words:" + cafe.ViaWord + ":apple wallet", "", cafeTestVsDefault,
			"Comparația: " + cafeTestVsDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ev := cafeLead(tc.signal)
			rows := draftCafe(t, dir, spec, cfg, c, ev)
			var step1 *leadstore.DraftRow
			for i := range rows {
				if rows[i].Channel == "email" && rows[i].Step == 1 {
					step1 = &rows[i]
				}
			}
			if step1 == nil {
				t.Fatal("no step 1 email")
			}
			if !strings.Contains(step1.Body, tc.line) || !strings.Contains(step1.Body, cafeTestDemo) {
				t.Errorf("step 1 lacks %q or the offer link:\n%s", tc.line, step1.Body)
			}
			var slots map[string]slotJSON
			if err := json.Unmarshal(step1.Slots, &slots); err != nil {
				t.Fatal(err)
			}
			if got := slots["VendorName"].Value; got != tc.vendor {
				t.Errorf("VendorName = %q, want %q", got, tc.vendor)
			}
			if got := slots["VendorLink"].Value; got != tc.link {
				t.Errorf("VendorLink = %q, want %q", got, tc.link)
			}
		})
	}
}

// TestCafeUnlistedLinkRefused: a URL neither the spec nor the lead allows stops the lead.
func TestCafeUnlistedLinkRefused(t *testing.T) {
	dir, spec, cfg := cafeWorld(t, true)
	spec.Offer.Links = nil // the template still carries the demo link
	plan, err := filePlan(dir, spec, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, ev := cafeLead()
	_, err = draftSequence(plan, c, ev, cfg, nil, "sender.example", zeroReader{})
	if err == nil || !strings.Contains(err.Error(), "url: "+cafeTestDemo) {
		t.Errorf("draftSequence: %v; want the unlisted demo link refused", err)
	}
}

// TestCafeVendorSlotsOnlyForCafes: a non-café lead never gets vendor slots, even on a spec
// with vendor links.
func TestCafeVendorSlotsOnlyForCafes(t *testing.T) {
	_, spec, cfg := cafeWorld(t, true)
	probeID := int64(9)
	c := leadstore.Candidate{LeadID: 1, OrgID: 1, Domain: "site.example", Country: "GB", Kind: leadconfig.KindD10}
	ev := leadstore.Evidence{ProbeID: &probeID, FinalHost: "site.example", Major: 10,
		Signals: []leadstore.EvidenceSignal{{Signal: leadstore.Signal{Signal: "meta-generator", Value: "10"}, Fetch: leadstore.EvidenceFetch{ID: 5, URL: "https://site.example/"}}}}
	s, err := leadSlots(c, ev, cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if s.VendorName.Value != "" || s.VendorLink.Value != "" {
		t.Errorf("a d10 lead got vendor slots %q %q", s.VendorName.Value, s.VendorLink.Value)
	}
}
