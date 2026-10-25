package leadconfig

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDemoPriority(t *testing.T) {
	cfg, err := Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.PriorityProfile()
	if !maps.Equal(p.Kinds, map[string]int{KindUnsupported: 20, KindD10: 10, KindAgencyPartner: 15}) {
		t.Errorf("kinds = %v", p.Kinds)
	}
	if !maps.Equal(p.Majors, map[int]int{6: 5, 7: 30, 8: 20, 9: 15, 10: 10}) {
		t.Errorf("majors = %v", p.Majors)
	}
	if !maps.Equal(p.Channels, map[string]int{ChannelEmail: 10, ChannelForm: 0}) || p.ExactVersion != 5 {
		t.Errorf("channels %v, exact %d", p.Channels, p.ExactVersion)
	}
	if len(p.RankBands) != 2 || p.RankBands[0] != (RankBand{Under: 10000, Points: 15}) || p.RankBands[1] != (RankBand{Under: 100000, Points: 8}) {
		t.Errorf("rank bands = %v", p.RankBands)
	}
	if len(p.Countries) != 0 || len(p.ExcludeCountries) != 0 {
		t.Errorf("countries %v, exclude %v; the demo weighs none", p.Countries, p.ExcludeCountries)
	}
	// The profile is a copy: editing it leaves the config alone.
	p.Kinds[KindD10] = 99
	if cfg.Priority.Kinds[KindD10] != 10 {
		t.Error("PriorityProfile shares the config's maps")
	}
}

// TestPriorityFromKindsPriority: with no priority block, or a block without kinds,
// kinds_priority maps into kind points, first listed highest.
func TestPriorityFromKindsPriority(t *testing.T) {
	dir := demoDir(t)
	cut(t, dir, "priority:", "exclude_countries: []")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Priority != nil {
		t.Fatalf("priority = %+v, want none", cfg.Priority)
	}
	want := map[string]int{KindAgencyPartner: 30, KindUnsupported: 20, KindD10: 10}
	if p := cfg.PriorityProfile(); !maps.Equal(p.Kinds, want) || p.Rules() != 3 {
		t.Errorf("no block: profile %+v, want kinds %v only", p, want)
	}

	dir = demoDir(t)
	replaceIn(t, dir, FileName, "  kinds: {unsupported: 20, d10: 10, agency_partner: 15} # replaces kinds_priority's order when present\n", "")
	cfg, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.PriorityProfile(); !maps.Equal(p.Kinds, want) || p.Majors[7] != 30 {
		t.Errorf("block without kinds: profile %+v", p)
	}
}

// cut removes the lines of dir's leads.yaml from the one starting with from through the
// one containing through.
func cut(t *testing.T, dir, from, through string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, FileName)) // #nosec G304 -- test data
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	in := false
	for line := range strings.Lines(string(b)) {
		if strings.HasPrefix(line, from) {
			in = true
		}
		if !in {
			out = append(out, line)
		} else if strings.Contains(line, through) {
			in = false
		}
	}
	writeFile(t, dir, FileName, strings.Join(out, ""))
}

func TestPriorityProblems(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		want     string
	}{
		{"unknown kind", "kinds: {unsupported: 20,", "kinds: {hand: 5, unsupported: 20,", `"hand"`},
		{"points over 100", "majors: {6: 5, 7: 30,", "majors: {6: 5, 7: 101,", "priority.majors.7"},
		{"points under -100", "exact_version: 5", "exact_version: -101", "priority.exact_version"},
		{"major out of range", "majors: {6: 5,", "majors: {5: 5,", "priority.majors: 5"},
		{"bad country", "countries: {}", "countries: {Britain: 10}", `"Britain"`},
		{"bad channel", "channels: {email: 10, form: 0}", "channels: {email: 10, none: 5}", `"none"`},
		{"bands not ascending", "{under: 100000, points: 8}", "{under: 5000, points: 8}", "priority.rank_bands[1]"},
		{"band under zero", "{under: 10000, points: 15}", "{under: 0, points: 15}", "priority.rank_bands[0]"},
		{"bad exclusion", "exclude_countries: []", "exclude_countries: [gb]", `"gb"`},
		{"twice excluded", "exclude_countries: []", "exclude_countries: [GB, GB]", "appears twice"},
		{"unknown key", "exact_version: 5", "exact_version: 5\n  tld_points: {}", "tld_points"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := demoDir(t)
			replaceIn(t, dir, FileName, tc.old, tc.new)
			_, err := Load(dir)
			wantProblem(t, err, tc.want)
		})
	}
}

func TestProfileValidate(t *testing.T) {
	ok := Profile{Kinds: map[string]int{KindD10: -100}, Majors: map[int]int{11: 100}, Countries: map[string]int{"GB": 20},
		Channels: map[string]int{ChannelForm: -5}, RankBands: []RankBand{{1, 1}, {2, 2}}, ExcludeCountries: []string{"DE"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid profile: %v", err)
	}
	if !(Profile{}).IsZero() || ok.IsZero() || ok.Rules() != 7 {
		t.Errorf("IsZero/Rules: %v %v %d", (Profile{}).IsZero(), ok.IsZero(), ok.Rules())
	}
	bad := ok.Clone()
	bad.Majors[12] = 1
	bad.Countries["XX1"] = 1
	err := bad.Validate()
	wantProblem(t, err, "priority.majors: 12", `"XX1"`)
	if ok.Majors[12] != 0 {
		t.Error("Clone shares maps")
	}
}

func TestKindPoints(t *testing.T) {
	if got := KindPoints([]string{KindD10, KindUnsupported}); !maps.Equal(got, map[string]int{KindD10: 20, KindUnsupported: 10}) {
		t.Errorf("KindPoints = %v", got)
	}
	if got := KindPoints(nil); len(got) != 0 {
		t.Errorf("KindPoints(nil) = %v", got)
	}
}
