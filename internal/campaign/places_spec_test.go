package campaign

import (
	"os"
	"strings"
	"testing"
)

func cafeSpec(t *testing.T) Spec {
	t.Helper()
	s, err := Load("../../examples/campaigns/cafes-loyalty/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The example café campaign passes its checks: an overture source, cafe@1, a café kind, the
// call_first word limits.
func TestCafeExampleChecks(t *testing.T) {
	s := cafeSpec(t)
	if err := s.Check(CheckOptions{KnownKinds: KnownKinds()}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if s.Source.Kind != SourceOverture || s.Chains == nil || s.Chains.MaxLocations != 5 || s.Copy.LinksInStep0 {
		t.Errorf("spec read as %+v", s)
	}
	if got := s.MaxWordsFor(SequenceCallFirst); got[2] != 110 || got[3] != 60 {
		t.Errorf("call_first limits %v", got)
	}
	if got := s.MaxWordsFor(SequenceDefault); got[0] != 125 {
		t.Errorf("default limits %v", got)
	}
}

func TestPlacesSpecRules(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"drupal detector on overture", func(s *Spec) { s.Detectors = []string{DetectorDrupal} }, `"drupal@1" runs on a "site_list" source`},
		{"drupal kind on cafe detector", func(s *Spec) { s.Kinds = []string{"d10"} }, "not a kind the campaign's detectors make"},
		{"bad category", func(s *Spec) { s.Source.Categories = []string{"Coffee Shop"} }, "not an Overture category"},
		{"bad country", func(s *Spec) { s.Source.Countries = []string{"gb"} }, "source.countries"},
		{"confidence above 1", func(s *Spec) { s.Source.MinConfidence = 1.5 }, "min_confidence"},
		{"negative max locations", func(s *Spec) { s.Chains.MaxLocations = -1 }, "chains.max_locations"},
		{"words on a call step", func(s *Spec) { s.Copy.MaxWordsBySequence = map[string]map[int]int{SequenceCallFirst: {0: 50}} }, "not an email step"},
		{"words for an unknown sequence", func(s *Spec) { s.Copy.MaxWordsBySequence = map[string]map[int]int{"later": {0: 50}} }, "not a sequence"},
		{"words for the default", func(s *Spec) { s.Copy.MaxWordsBySequence = map[string]map[int]int{SequenceDefault: {0: 50}} }, "copy.max_words"},
	} {
		s := cafeSpec(t)
		c.edit(&s)
		err := s.Check(CheckOptions{KnownKinds: KnownKinds()})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// The new spec fields are omitempty: the Drupal example's canonical JSON carries none of
// them, so its registered hash does not move.
func TestDrupalSpecHashKeepsShape(t *testing.T) {
	b, err := os.ReadFile("../../examples/campaigns/drupal-eol/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := Canonical(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"chains", "categories", "min_confidence", "max_words_by_sequence"} {
		if strings.Contains(string(j), `"`+key+`"`) {
			t.Errorf("canonical JSON names %q:\n%s", key, j)
		}
	}
}
