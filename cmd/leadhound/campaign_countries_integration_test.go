//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestCampaignCountriesWinOverLeadsYAML (D3): a campaign whose spec lists its countries
// (the example: GB, IE, US on, DE off) never batches a lead in another country, even one
// leads.yaml has email on for (BE here); a starred BE lead says why it is out. leads.yaml
// still supplies the GB leads' footer and opt-out.
func TestCampaignCountriesWinOverLeadsYAML(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC) // a Tuesday
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })

	cfgPath := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	be := strings.Replace(string(raw), "  DE: {channel: form,", "  BE: {channel: email, lang: en, tz: Europe/Brussels, footer: footer-en}\n  DE: {channel: form,", 1)
	if be == string(raw) {
		t.Fatal("leads.yaml countries not rewritten")
	}
	if err := os.WriteFile(cfgPath, []byte(be), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(dir, campaign.Dir), os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}

	batchFixture(t)
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE org SET country = 'BE' WHERE domain = 'northwind.example'`); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")

	// leads.yaml alone would offer the BE lead: the legacy rule still does.
	lq := leadstoretest.Q(t)
	all, err := lq.CandidateLeads(ctx, leadstore.CandidateOptions{IncludeReserved: true, CampaignID: leadstore.DefaultCampaignID})
	if err != nil {
		t.Fatal(err)
	}
	if !candidateHas(all, "northwind.example") {
		t.Fatal("the BE lead is not a candidate under leads.yaml alone: the fixture does not test the campaign rule")
	}
	listed, err := lq.CandidateLeads(ctx, leadstore.CandidateOptions{IncludeReserved: true, CampaignID: leadstore.DefaultCampaignID,
		Countries: []string{"GB", "IE", "US"}})
	if err != nil {
		t.Fatal(err)
	}
	if candidateHas(listed, "northwind.example") || !candidateHas(listed, "tailspin.example") {
		t.Fatalf("with the campaign's countries: %v", candidateDomains(listed))
	}

	mustRun(t, "leads", "batch", "new")
	var n int
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id
		JOIN org o ON o.id = l.org_id WHERE o.country = 'BE'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d messages drafted for the BE lead", n)
	}
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id
		JOIN org o ON o.id = l.org_id WHERE o.country = 'GB'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no GB lead drafted")
	}

	// starred, the BE lead says it is not in the campaign's countries
	var beLead int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id
		WHERE o.domain = 'northwind.example'`).Scan(&beLead); err != nil {
		t.Fatal(err)
	}
	if _, err := lq.Star(ctx, beLead, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	st, err := lq.StarredStatus(ctx, leadstore.StarredStatusOptions{
		CandidateOptions: leadstore.CandidateOptions{IncludeReserved: true, CampaignID: leadstore.DefaultCampaignID, Countries: []string{"GB", "IE", "US"}},
		Countries:        map[string]string{"BE": "email", "GB": "email"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Reason != leadstore.StarredCountryRule || st[0].Detail != leadstore.CampaignCountryWords {
		t.Fatalf("starred status %+v", st)
	}
}

func candidateHas(cs []leadstore.Candidate, domain string) bool {
	for _, c := range cs {
		if c.Domain == domain {
			return true
		}
	}
	return false
}

func candidateDomains(cs []leadstore.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Domain + " " + c.Country
	}
	return out
}
