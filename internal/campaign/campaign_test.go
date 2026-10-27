package campaign

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// examplesDir holds the fictional leads.yaml the leadconfig tests load too.
const examplesDir = "../../examples"

func validSpec(t *testing.T) Spec {
	t.Helper()
	s, err := Load(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var known = CheckOptions{KnownKinds: leadconfig.Kinds}

func TestValidSpecPasses(t *testing.T) {
	s := validSpec(t)
	if err := s.Check(known); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !s.Countries["GB"].Email || !s.Countries["GB"].Call || s.Countries["DE"].Email {
		t.Errorf("on/off not decoded: %+v", s.Countries)
	}
	if len(s.Sequence) != 5 || s.Sequence[2].Type != StepCall || s.Copy.MaxWords[3] != 60 {
		t.Errorf("decoded spec off: %+v", s)
	}
}

func TestCheckRules(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Spec)
		opts  *CheckOptions
		wants []string
	}{
		{"slug pattern", func(s *Spec) { s.Slug = "Drupal_EOL" }, nil, []string{"slug"}},
		{"slug too short", func(s *Spec) { s.Slug = "d" }, nil, []string{"slug"}},
		{"name empty", func(s *Spec) { s.Name = " " }, nil, []string{"name is empty"}},
		{"source places", func(s *Spec) { s.Source.Kind = SourcePlaces }, nil, []string{"comes in CP7"}},
		{"source unknown", func(s *Spec) { s.Source.Kind = "csv" }, nil, []string{"source.kind", "unknown"}},
		{"detector unknown", func(s *Spec) { s.Detectors = []string{"drupal@2"} }, nil, []string{"not a known detector"}},
		{"detectors empty", func(s *Spec) { s.Detectors = nil }, nil, []string{"detectors is empty"}},
		{"detector twice", func(s *Spec) { s.Detectors = []string{"drupal@1", "drupal@1"} }, nil, []string{"listed twice"}},
		{"kind unknown", func(s *Spec) { s.Kinds = append(s.Kinds, "cafe") }, nil, []string{"not a known lead kind"}},
		{"kinds empty", func(s *Spec) { s.Kinds = nil; s.Copy.ForbidByKind = nil }, nil, []string{"kinds is empty"}},
		{"step gap", func(s *Spec) { s.Sequence[3].Step = 4; s.Sequence[4].Step = 5 }, nil, []string{"no gap"}},
		{"step not from 0", func(s *Spec) {
			for i := range s.Sequence {
				s.Sequence[i].Step++
			}
		}, nil, []string{"sequence[0]: step is 1"}},
		{"too many steps", func(s *Spec) {
			for i := 5; i <= 10; i++ {
				s.Sequence = append(s.Sequence, Step{Step: i, Type: StepEmail})
			}
		}, nil, []string{"at most 10"}},
		{"sequence empty", func(s *Spec) { s.Sequence = nil; s.Copy.MaxWords = nil }, nil, []string{"sequence is empty"}},
		{"starts with call", func(s *Spec) { s.Sequence[0].Type = StepCall }, nil, []string{"must start with an email"}},
		{"step type unknown", func(s *Spec) { s.Sequence[1].Type = "sms" }, nil, []string{`type "sms"`}},
		{"negative days", func(s *Spec) { s.Sequence[1].AfterBusinessDays = -1 }, nil, []string{"below 0"}},
		{"when unknown", func(s *Spec) { s.Sequence[1].When = "sometimes" }, nil, []string{`when "sometimes"`}},
		{"call_not_connected before a call", func(s *Spec) { s.Sequence[1].When = WhenCallNotConnected }, nil, []string{"needs a call step before it"}},
		{"country lower case", func(s *Spec) { s.Countries["gb"] = Country{} }, nil, []string{`"gb" is not an ISO 3166-1`}},
		{"country not assigned", func(s *Spec) { s.Countries["UK"] = Country{} }, nil, []string{`"UK" is not an ISO 3166-1`}},
		{"country lang bad", func(s *Spec) { s.Countries["FR"] = Country{Email: true, Lang: "FRA"} }, nil, []string{"countries.FR.lang"}},
		{"country lang missing", func(s *Spec) { s.Countries["FR"] = Country{Call: true} }, nil, []string{"lang is needed"}},
		{"writer unknown", func(s *Spec) { s.Writer = "gpt" }, nil, []string{`writer "gpt"`}},
		{"writer claude untrusted", func(s *Spec) { s.Writer = WriterClaude }, nil, []string{"refused until the draft gate passes"}},
		{"forbid kind not in campaign", func(s *Spec) { s.Copy.ForbidByKind["cafe"] = nil }, nil, []string{"copy.forbid_by_kind"}},
		{"forbid regexp bad", func(s *Spec) { s.Copy.ForbidByKind["d10"] = []string{"("} }, nil, []string{"does not compile"}},
		{"max_words step outside", func(s *Spec) { s.Copy.MaxWords[7] = 50 }, nil, []string{"step 7 is not in the sequence"}},
		{"max_words zero", func(s *Spec) { s.Copy.MaxWords[0] = 0 }, nil, []string{"max_words.0 must be above 0"}},
		{"templates_dir escapes", func(s *Spec) { s.TemplatesDir = "../other" }, nil, []string{"templates_dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec(t)
			tc.edit(&s)
			opts := known
			if tc.opts != nil {
				opts = *tc.opts
			}
			err := s.Check(opts)
			if err == nil {
				t.Fatal("Check passed; want a problem")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			for _, w := range tc.wants {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestClaudeAllowedWhenTrusted(t *testing.T) {
	s := validSpec(t)
	s.Writer = WriterClaude
	if err := s.Check(CheckOptions{DraftsTrusted: true, KnownKinds: leadconfig.Kinds}); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestKindsUncheckedWithoutKnownKinds(t *testing.T) {
	s := validSpec(t)
	s.Kinds = append(s.Kinds, "cafe")
	if err := s.Check(CheckOptions{}); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestCheckCollectsEveryProblem(t *testing.T) {
	s := validSpec(t)
	s.Slug = "X"
	s.Writer = "gpt"
	s.Sequence[0].Type = StepCall
	err := s.Check(known)
	for _, w := range []string{"slug", `writer "gpt"`, "must start with an email"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("error %v lacks %q", err, w)
		}
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(append(b, []byte("budget: 100\n")...)); err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("Decode with an unknown key: %v", err)
	}
	nested := strings.Replace(string(b), "writer: template", "writer: template\noffer2: x", 1)
	nested = strings.Replace(nested, "kind: site_list", "kind: site_list, region: eu", 1)
	if _, err := Decode([]byte(nested)); err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("Decode with an unknown nested key: %v", err)
	}
	if _, err := Decode(nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("Decode of an empty file: %v", err)
	}
}

func TestCanonicalStable(t *testing.T) {
	a := validSpec(t)
	b, err := Load(filepath.Join("testdata", "reordered.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ja, ha, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, hb, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb || string(ja) != string(jb) {
		t.Fatalf("key order changed the canonical form:\n%s\n%s", ja, jb)
	}
	for range 20 { // map iteration order varies run to run
		if _, h, _ := Canonical(a); h != ha {
			t.Fatal("Canonical is not stable across calls")
		}
	}
	if len(ha) != 64 {
		t.Errorf("sha %q is not 64 hex digits", ha)
	}
	b.Copy.MaxWords[0] = 121
	if _, hc, _ := Canonical(b); hc == ha {
		t.Error("a changed spec kept its hash")
	}
}

func demoConfig(t *testing.T) *leadconfig.Config {
	t.Helper()
	cfg, err := leadconfig.Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func TestLegacyFromLeadsYAML(t *testing.T) {
	cfg := demoConfig(t)
	s := Legacy(cfg)
	if err := s.Check(known); err != nil {
		t.Fatalf("Legacy fails Check: %v", err)
	}
	if s.Slug != LegacySlug || s.Writer != WriterTemplate || s.Source.Kind != SourceSiteList {
		t.Errorf("legacy identity off: %+v", s)
	}
	if len(s.Sequence) != 3 {
		t.Fatalf("legacy has %d steps; want 3", len(s.Sequence))
	}
	for i, st := range s.Sequence {
		if st.Step != i || st.Type != StepEmail {
			t.Errorf("step %d = %+v; want an email", i, st)
		}
	}
	fd := cfg.Schedule.FollowupBusinessDays
	if s.Sequence[0].AfterBusinessDays != 0 || s.Sequence[1].AfterBusinessDays != fd[0] || s.Sequence[2].AfterBusinessDays != fd[1] {
		t.Errorf("spacing %+v; want 0, %d, %d", s.Sequence, fd[0], fd[1])
	}
	if gb := s.Countries["GB"]; !gb.Email || gb.Call || gb.Lang != "en" {
		t.Errorf("GB = %+v; want email on, call off, en", gb)
	}
	if de := s.Countries["DE"]; de.Email || de.Call { // channel form
		t.Errorf("DE = %+v; want email and call off", de)
	}
	if s.Copy.MaxWords[0] != cfg.Copy.FirstWords[1] || s.Copy.MaxWords[2] != cfg.Copy.FollowupWords[1] {
		t.Errorf("max_words %v", s.Copy.MaxWords)
	}
	if s.TemplatesDir != cfg.TemplatesDir || len(s.Copy.BannedWords) != len(cfg.Copy.BannedWords) {
		t.Errorf("templates or banned words not carried: %+v", s)
	}
}

func TestLoadAllWithoutFolder(t *testing.T) {
	specs, err := LoadAll(t.TempDir(), demoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Slug != LegacySlug || len(specs[0].Sequence) != 3 {
		t.Fatalf("specs = %+v; want the legacy campaign only", specs)
	}
	if _, err := LoadAll(t.TempDir(), nil); err == nil {
		t.Fatal("LoadAll with no folder and no config passed")
	}
}

func writeCampaign(t *testing.T, dataDir, folder, body string) {
	t.Helper()
	dir := filepath.Join(dataDir, Dir, folder)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validYAML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoadAllWithFolder(t *testing.T) {
	cfg := demoConfig(t)
	t.Run("file drupal-eol replaces legacy", func(t *testing.T) {
		dir := t.TempDir()
		writeCampaign(t, dir, "drupal-eol", validYAML(t))
		writeCampaign(t, dir, "agencies", strings.Replace(validYAML(t), "slug: drupal-eol", "slug: agencies", 1))
		specs, err := LoadAll(dir, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 2 || specs[0].Slug != "agencies" || specs[1].Slug != "drupal-eol" {
			t.Fatalf("specs = %v; want agencies, drupal-eol sorted", slugs(specs))
		}
		if len(specs[1].Sequence) != 5 {
			t.Errorf("drupal-eol came from legacy, not the file")
		}
	})
	t.Run("legacy kept beside other campaigns", func(t *testing.T) {
		dir := t.TempDir()
		writeCampaign(t, dir, "agencies", strings.Replace(validYAML(t), "slug: drupal-eol", "slug: agencies", 1))
		specs, err := LoadAll(dir, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := slugs(specs); got != "agencies,drupal-eol" || len(specs[1].Sequence) != 3 {
			t.Fatalf("specs = %s", got)
		}
	})
	t.Run("empty folder is legacy only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, Dir), 0o750); err != nil {
			t.Fatal(err)
		}
		specs, err := LoadAll(dir, cfg)
		if err != nil || slugs(specs) != LegacySlug {
			t.Fatalf("specs = %v, %v", specs, err)
		}
	})
	t.Run("slug must match folder", func(t *testing.T) {
		dir := t.TempDir()
		writeCampaign(t, dir, "other", validYAML(t))
		if _, err := LoadAll(dir, cfg); err == nil || !strings.Contains(err.Error(), "does not match its folder") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a bad spec fails the load", func(t *testing.T) {
		dir := t.TempDir()
		writeCampaign(t, dir, "drupal-eol", strings.Replace(validYAML(t), "writer: template", "writer: claude", 1))
		if _, err := LoadAll(dir, cfg); err == nil || !strings.Contains(err.Error(), "draft gate") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an unknown key fails the load", func(t *testing.T) {
		dir := t.TempDir()
		writeCampaign(t, dir, "drupal-eol", validYAML(t)+"budget: 1\n")
		if _, err := LoadAll(dir, cfg); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("err = %v", err)
		}
	})
}

func slugs(specs []Spec) string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Slug)
	}
	return strings.Join(out, ",")
}

func TestCountryList(t *testing.T) {
	if n := len(countries); n != 249 {
		t.Errorf("%d ISO codes; want 249", n)
	}
	for _, c := range []string{"GB", "DE", "RO", "IE", "NL", "US"} {
		if !isCountry(c) {
			t.Errorf("%s missing", c)
		}
	}
}

func TestDetectors(t *testing.T) {
	if !KnownDetector(DetectorDrupal) || !KnownDetector(DetectorAgency) || KnownDetector("cafe@1") {
		t.Error("detector registry off")
	}
}
