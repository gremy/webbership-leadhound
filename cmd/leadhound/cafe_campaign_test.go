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
		replace("RO: {email: on, call: on, lang: ro,", "RO: {email: on, call: on, lang: ro, footer: footer-cafe-ro,")
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

// legacyFooterText is leads.yaml's RO footer rendered with the lead's own slots (the
// campaign's RO identity and opt-out lines).
func legacyFooterText(t *testing.T, cfg leadconfig.Config, spec campaign.Spec) string {
	t.Helper()
	var b bytes.Buffer
	ro := spec.Countries["RO"]
	if err := cfg.Templates.Footers[cafeTestLegacyLabel].Execute(&b, map[string]string{
		"SenderName": cfg.Sender.Name, "IdentityLine": ro.IdentityLine, "OptOutLine": ro.OptOutLine,
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
	legacy := legacyFooterText(t, cfg, spec)
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

// TestCafeCountryFooterLines (G4): the campaign's RO identity_line and opt_out_line replace
// leads.yaml's in every café email, and Check still demands both; a lead on the legacy
// (Drupal) spec keeps leads.yaml's lines.
func TestCafeCountryFooterLines(t *testing.T) {
	dir, spec, cfg := cafeWorld(t, true)
	ro := spec.Countries["RO"]
	if ro.IdentityLine == "" || ro.OptOutLine == "" {
		t.Fatal("the example spec's RO country no longer words its own footer lines")
	}
	c, ev := cafeLead()
	for _, r := range draftCafe(t, dir, spec, cfg, c, ev) {
		if r.Channel != "email" {
			continue
		}
		if !strings.Contains(r.Body, ro.IdentityLine) || !strings.Contains(r.Body, ro.OptOutLine) {
			t.Errorf("step %d lacks the campaign's lines:\n%s", r.Step, r.Body)
		}
		if strings.Contains(r.Body, cfg.Sender.IdentityLine) || strings.Contains(r.Body, cafeTestOptOutRO) {
			t.Errorf("step %d carries leads.yaml's lines:\n%s", r.Step, r.Body)
		}
	}

	// The opt-out guarantee holds for the override: a footer that drops it stops the lead.
	if err := os.WriteFile(filepath.Join(dir, "leads-templates", "footers", "footer-cafe-ro.txt"),
		[]byte("{{.SenderName}}\n{{.IdentityLine}}\n"), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	plan, err := filePlan(dir, spec, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := draftSequence(plan, c, ev, cfg, nil, "sender.example", zeroReader{}); err == nil || !strings.Contains(err.Error(), "opt-out: the opt-out line is not in the email") {
		t.Errorf("draftSequence: %v; want the missing opt-out line refused", err)
	}

	// drupal-eol: leads.yaml's lines, whatever a café spec says.
	probeID := int64(9)
	d := leadstore.Candidate{LeadID: 1, OrgID: 1, Domain: "site.example", Country: "GB", Kind: leadconfig.KindD10}
	dev := leadstore.Evidence{ProbeID: &probeID, FinalHost: "site.example", Major: 10,
		Signals: []leadstore.EvidenceSignal{{Signal: leadstore.Signal{Signal: "meta-generator", Value: "10"}, Fetch: leadstore.EvidenceFetch{ID: 5, URL: "https://site.example/"}}}}
	s, err := leadSlots(d, dev, cfg, campaign.Legacy(&cfg))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := cfg.Sender.OptOutLine("en")
	if s.IdentityLine.Value != cfg.Sender.IdentityLine || s.OptOutLine.Value != want {
		t.Errorf("legacy lead lines %q / %q, want leads.yaml's", s.IdentityLine.Value, s.OptOutLine.Value)
	}
}

// TestCafeFormLead (F): a café form lead gets the campaign folder's form.txt and
// linkedin.txt; a café kind's folder without them is refused when the plan loads.
func TestCafeFormLead(t *testing.T) {
	dir, spec, cfg := cafeWorld(t, true)
	c, ev := cafeLead()
	c.Channel, c.RouteKind, c.Route = leadconfig.ChannelForm, "", ""
	rows := draftCafe(t, dir, spec, cfg, c, ev)
	if len(rows) != 2 || rows[0].Channel != "form" || rows[1].Channel != "linkedin" {
		t.Fatalf("form lead rows %+v, want a form text and a LinkedIn note", rows)
	}
	if !strings.Contains(rows[0].Body, "Cafeneaua Exemplu") || rows[0].Subject != "" {
		t.Errorf("form text:\n%s", rows[0].Body)
	}

	rel := filepath.Join(dir, campaign.Dir, "cafes-loyalty", "templates", "cafe_no_website", "ro", leadconfig.LinkedInFile)
	if err := os.Remove(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := filePlan(dir, spec, cfg); err == nil || !strings.Contains(err.Error(), "cafe_no_website/ro/linkedin.txt is missing") {
		t.Errorf("filePlan: %v; want the missing LinkedIn note named", err)
	}
}

// TestCampaignBatchesFrom (G3): a spec's batches_from wins for its own batches; a spec
// without one, and the legacy plan, keep leads.yaml's (the console's) value in force.
func TestCampaignBatchesFrom(t *testing.T) {
	_, spec, cfg := cafeWorld(t, true)
	cfg.BatchSource = leadconfig.BatchesStarred
	if got := (batchPlan{spec: spec}).batchesFrom(cfg); got != leadconfig.BatchesStarredThenRest {
		t.Errorf("café plan draws from %q, want the spec's %q", got, leadconfig.BatchesStarredThenRest)
	}
	spec.BatchesFrom = ""
	if got := (batchPlan{spec: spec}).batchesFrom(cfg); got != leadconfig.BatchesStarred {
		t.Errorf("a spec without batches_from draws from %q, want leads.yaml's", got)
	}
	cfg.BatchSource = leadconfig.BatchesStarredThenRest
	legacy := legacyPlan(cfg)
	legacy.spec.BatchesFrom = leadconfig.BatchesStarred // never read for the legacy plan
	if got := legacy.batchesFrom(cfg); got != leadconfig.BatchesStarredThenRest {
		t.Errorf("legacy plan draws from %q, want leads.yaml's", got)
	}
}

// TestCafeFooterLinkInStep0 (G5): a campaign footer carrying an allowed link (the
// signature) passes on step 0 under links_in_step0: false; the copy above it stays link-free.
func TestCafeFooterLinkInStep0(t *testing.T) {
	dir, spec, cfg := cafeWorld(t, true)
	if spec.Copy.LinksInStep0 {
		t.Fatal("the example spec allows links in step 0")
	}
	if err := os.WriteFile(filepath.Join(dir, "leads-templates", "footers", "footer-cafe-ro.txt"),
		[]byte("{{.SenderName}}, "+cafeTestFooterMark+" · "+cafeTestDemo+"\n{{.IdentityLine}}\n{{.OptOutLine}}\n"), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	c, ev := cafeLead()
	rows := draftCafe(t, dir, spec, cfg, c, ev)
	if rows[0].Step != 0 || !strings.Contains(rows[0].Body, cafeTestDemo) {
		t.Errorf("step 0 lacks the footer link:\n%s", rows[0].Body)
	}
}
