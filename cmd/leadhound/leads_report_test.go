package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// fakeReportStore is one fictional Drupal 10 site lead in campaign 3.
type fakeReportStore struct{ spec []byte }

func (f fakeReportStore) LeadByID(_ context.Context, id int64) (leadstore.Lead, error) {
	if id != 5 {
		return leadstore.Lead{}, leadstore.ErrLeadNotFound
	}
	probe := int64(412)
	return leadstore.Lead{ID: 5, CampaignID: 3, OrgID: 17, Kind: "d10", ProbeID: &probe}, nil
}

func (fakeReportStore) OrgByID(context.Context, int64) (leadstore.Org, error) {
	return leadstore.Org{ID: 17, Domain: "northgate.example", Name: "Northgate Books", Country: "IE"}, nil
}

func (fakeReportStore) LeadEvidence(context.Context, []int64) (map[int64]leadstore.Evidence, error) {
	probe := int64(412)
	return map[int64]leadstore.Evidence{5: {
		LeadID: 5, ProbeID: &probe, FinalHost: "northgate.example", Major: 10, ExactVersion: "10.3.6",
		ProbedAt: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC),
		Signals: []leadstore.EvidenceSignal{
			{Signal: leadstore.Signal{Signal: "core-asset-v", Value: "10.3.6", FetchID: 88},
				Fetch: leadstore.EvidenceFetch{ID: 88, URL: "https://northgate.example/"}},
			{Signal: leadstore.Signal{Signal: "cf-mitigated", Value: "challenge", FetchID: 88}},
		},
	}}, nil
}

func (f fakeReportStore) Campaigns(context.Context, int64) ([]leadstore.Campaign, error) {
	return []leadstore.Campaign{{ID: 3, Slug: "drupal-eol", Name: "Drupal end-of-life", Spec: f.spec}}, nil
}

func reportConfig() leadconfig.Config {
	var cfg leadconfig.Config
	cfg.Sender.Name = "Alex Example"
	cfg.Sender.IdentityLine = "I build and look after Drupal sites for small organisations."
	cfg.Sender.SiteURL = "https://sender.example/"
	cfg.Facts.D10EOL = leadconfig.ParseDate("2026-12-09")
	return cfg
}

func reportSpec(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(campaign.Spec{Slug: "drupal-eol", Product: campaign.Product{SiteURL: "https://sender.example/"},
		Offer: campaign.Offer{ProofLine: "Recently I took a Drupal 7 site for Harbour Lane Studio to Drupal 10, with no downtime.", Magnet: reportMagnet},
		Copy:  campaign.Copy{ForbidByKind: map[string][]string{"d10": {"(?i)migrat"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var reportTestNow = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

func TestLeadsReportRegistered(t *testing.T) {
	if _, ok := leadsCommands["report"]; !ok {
		t.Fatal(`leadsCommands["report"] is not registered`)
	}
}

// TestLeadsReportRefusesRepoPath: a path inside this repository is refused before the store
// is opened, through the command and through reportToFile.
func TestLeadsReportRefusesRepoPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(wd, "testdata-report-should-not-exist.html")
	err = runLeadsReport(context.Background(), []string{"5", "--out", inside})
	if err == nil || !strings.Contains(err.Error(), "inside the leadhound repository") {
		t.Fatalf("err %v, want the repository refusal", err)
	}
	err = reportToFile(context.Background(), fakeReportStore{spec: reportSpec(t)}, reportConfig(), 5, "", reportTestNow, inside)
	if err == nil || !strings.Contains(err.Error(), "inside the leadhound repository") {
		t.Fatalf("reportToFile: err %v, want the repository refusal", err)
	}
	if _, err := os.Lstat(inside); !os.IsNotExist(err) {
		t.Fatalf("a file was written inside the repository: %v", err)
	}
}

func TestLeadsReportRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.html")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runLeadsReport(context.Background(), []string{"5", "--out", path})
	if err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("err %v, want the overwrite refusal", err)
	}
	err = reportToFile(context.Background(), fakeReportStore{spec: reportSpec(t)}, reportConfig(), 5, "", reportTestNow, path)
	if err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("reportToFile: err %v, want the overwrite refusal", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" { // #nosec G304 -- the test's own temp file
		t.Fatal("the existing file was changed")
	}
}

func TestLeadsReportWrites0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports", "northgate.html")
	if err := reportToFile(context.Background(), fakeReportStore{spec: reportSpec(t)}, reportConfig(), 5, "alex@sender.example", reportTestNow, path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", st.Mode().Perm())
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"Northgate Books", "Drupal 10.3.6", "9 December 2026", "https://northgate.example/", "update to Drupal 11", "alex@sender.example", "Harbour Lane Studio"} {
		if !strings.Contains(s, want) {
			t.Errorf("the report lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(s), "migrat") {
		t.Error("a Drupal 10 report says migrat…")
	}
	if strings.Contains(s, "challenge") {
		t.Error("a non-Drupal signal reached the report")
	}
}

// TestLeadsReportContactFromSettings: with no --contact (the console's download) the
// contact line links leads.yaml's sender site.
func TestLeadsReportContactFromSettings(t *testing.T) {
	cfg := reportConfig()
	b, err := leadReportHTML(context.Background(), fakeReportStore{spec: reportSpec(t)}, cfg, 5, "", reportTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sender.SiteURL == "" || !strings.Contains(string(b), `<p class="contact">To talk it over, get in touch through <a href="`+cfg.Sender.SiteURL+`">`) {
		t.Errorf("no contact line linking %q", cfg.Sender.SiteURL)
	}
}

func TestLeadsReportRefusesNonSiteLead(t *testing.T) {
	_, err := leadReportHTML(context.Background(), noProbeStore{fakeReportStore{spec: reportSpec(t)}}, reportConfig(), 5, "", reportTestNow)
	if err == nil || !strings.Contains(err.Error(), "no Drupal site finding") {
		t.Fatalf("err %v, want the non-site refusal", err)
	}
}

type noProbeStore struct{ fakeReportStore }

func (noProbeStore) LeadEvidence(context.Context, []int64) (map[int64]leadstore.Evidence, error) {
	finding := int64(9)
	return map[int64]leadstore.Evidence{5: {LeadID: 5, AgencyFindingID: &finding, FindingURL: "https://agency.example/drupal"}}, nil
}

func TestLeadsReportUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"5"}, {"x", "--out", "/tmp/r.html"}, {"5", "6", "--out", "/tmp/r.html"}} {
		if err := runLeadsReport(context.Background(), args); err == nil {
			t.Errorf("%q: no usage error", args)
		}
	}
}
