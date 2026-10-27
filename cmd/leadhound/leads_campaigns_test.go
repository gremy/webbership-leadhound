package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// campaignsDataDir sets LEADHOUND_DATA_DIR to a copy of the examples data directory with the
// given campaign files, folder → spec.
func campaignsDataDir(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../examples")); err != nil {
		t.Fatal(err)
	}
	for folder, body := range files {
		d := filepath.Join(dir, campaign.Dir, folder)
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, campaign.FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LEADHOUND_DATA_DIR", dir)
}

// specYAML is the campaign package's valid spec under slug.
func specYAML(t *testing.T, slug string) string {
	t.Helper()
	b, err := os.ReadFile("../../internal/campaign/testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(string(b), "slug: drupal-eol", "slug: "+slug, 1)
}

func runCampaigns(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	saved := stdout
	stdout = &out
	defer func() { stdout = saved }()
	code := run(context.Background(), append([]string{"leads", "campaigns"}, args...), &errOut)
	return code, out.String(), errOut.String()
}

// TestCampaignsCheck: check reads every campaign file and the legacy one without a
// database, prints each as ok, and a bad spec exits 2 naming every problem of every file.
func TestCampaignsCheck(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none") // never reached
	campaignsDataDir(t, map[string]string{"agencies": specYAML(t, "agencies")})
	code, out, errOut := runCampaigns(t, "check")
	if code != 0 {
		t.Fatalf("check exited %d: %s", code, errOut)
	}
	for _, want := range []string{"agencies: ok · ", "drupal-eol: ok · Drupal end-of-life · 3 kinds · 3 steps · writer template"} {
		if !strings.Contains(out, want) {
			t.Errorf("check output lacks %q:\n%s", want, out)
		}
	}

	bad := strings.Replace(specYAML(t, "agencies"), "writer: template", "writer: claude", 1)
	bad = strings.Replace(bad, "detectors: [drupal@1, agency@1]", "detectors: [cafe@9]", 1)
	campaignsDataDir(t, map[string]string{
		"agencies": bad,
		"other":    specYAML(t, "elsewhere"),
		"broken":   specYAML(t, "broken") + "budget: 1\n",
	})
	code, out, errOut = runCampaigns(t, "check")
	if code != 2 {
		t.Fatalf("a bad spec exited %d, want 2:\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"draft gate", "cafe@9", "does not match its folder", "budget"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("check does not name %q:\n%s", want, errOut)
		}
	}
	if n := strings.Count(errOut, "\n  "); n < 4 {
		t.Errorf("%d problem lines, want every problem on its own line:\n%s", n, errOut)
	}
	if out != "" {
		t.Errorf("a failed check printed campaigns:\n%s", out)
	}
}

func TestCampaignsUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"show"}, {"show", "a", "b"}, {"list", "x"}, {"check", "x"}, {"register", "x"}} {
		if code, _, errOut := runCampaigns(t, args...); code != 2 || !strings.Contains(errOut, "usage") && !strings.Contains(errOut, "unknown") {
			t.Errorf("%q exited %d: %s", args, code, errOut)
		}
	}
}

// TestCampaignViewWording: the Campaigns tab gets the latest version decoded, the legacy
// spec for campaign 1 with no version yet, and the name alone for any other.
func TestCampaignViewWording(t *testing.T) {
	cfg := demoLeadsConfig(t)
	kinds := []leadKind{{Name: "unsupported", Label: "Drupal 7 or older"}, {Name: "d10", Label: "Drupal 10"}}
	spec, err := campaign.Decode([]byte(specYAML(t, "agencies")))
	if err != nil {
		t.Fatal(err)
	}
	b, sha, err := campaign.Canonical(spec)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	retired := at
	v, err := campaignView(leadstore.Campaign{ID: 2, Slug: "agencies", Name: "Agencies", Spec: b, SpecSHA256: sha, VersionAt: &at, RetiredAt: &retired},
		leadstore.CampaignFromFile, kinds, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := console.CampaignView{Slug: "agencies", Name: "Agencies", Retired: true, SpecSHA: sha, SpecAt: at, SpecSource: "file",
		Kinds: []string{"Drupal 7 or older", "Drupal 10", "agency_partner"},
		Countries: []console.CampaignCountry{
			{Code: "DE", Label: "Germany"},
			{Code: "GB", Label: "United Kingdom", Email: true, Call: true, Language: "English"},
		},
		Steps: []console.CampaignStep{
			{Step: 0, Type: "email"},
			{Step: 1, Type: "email", AfterBusinessDays: 3},
			{Step: 2, Type: "call", AfterBusinessDays: 2, When: "has_callable_phone"},
			{Step: 3, Type: "email", AfterBusinessDays: 1, When: "call_not_connected"},
			{Step: 4, Type: "email", AfterBusinessDays: 5},
		},
		Writer: "template"}
	gotJ, _ := json.Marshal(v)
	wantJ, _ := json.Marshal(want)
	if !bytes.Equal(gotJ, wantJ) {
		t.Errorf("view =\n%s\nwant\n%s", gotJ, wantJ)
	}

	legacy, err := campaignView(leadstore.Campaign{ID: 1, Slug: campaign.LegacySlug, Name: campaign.LegacyName}, "", kinds, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.SpecSHA != "" || len(legacy.Steps) != 3 || legacy.Writer != "template" || len(legacy.Countries) != len(cfg.Countries) {
		t.Errorf("legacy view = %+v", legacy)
	}

	bare, err := campaignView(leadstore.Campaign{ID: 3, Slug: "cafes", Name: "Cafes"}, "", kinds, cfg)
	if err != nil || bare.Name != "Cafes" || bare.Kinds != nil || bare.Steps != nil || bare.Writer != "" {
		t.Errorf("a campaign with no version = %+v, %v", bare, err)
	}

	if _, err := campaignView(leadstore.Campaign{Slug: "x", Spec: []byte("[1]")}, "file", kinds, cfg); err == nil {
		t.Error("a spec that is not an object decoded")
	}
}
