//go:build private

package candidate

import (
	"os"
	"regexp"
	"testing"
)

// quoted is a value the YAML decoder quotes in a type error; the test masks it.
var quoted = regexp.MustCompile("`[^`]*`")

// TestLoadRealConfig loads the private leadhound.yaml from LEADHOUND_DATA_DIR. It prints
// only counts, never a value from the file.
func TestLoadRealConfig(t *testing.T) {
	dir := os.Getenv("LEADHOUND_DATA_DIR")
	if dir == "" {
		t.Fatal("LEADHOUND_DATA_DIR is not set")
	}
	cfg, err := Load(dir)
	if err != nil {
		// Validation errors name fields only; a decoder type error quotes the value, so it
		// is masked.
		t.Fatalf("load: %s", quoted.ReplaceAllString(err.Error(), "`…`"))
	}
	c := cfg.Candidate
	t.Logf("languages %d · allowed countries %d · allowed cities %d · criteria %d · evidence lists %d · keywords %d · locations %d",
		len(c.Languages), len(c.AllowedCountries), len(c.AllowedCities), len(c.Criteria.List()),
		len(c.EvidenceKeywords), len(cfg.LinkedIn.Keywords), len(cfg.LinkedIn.Locations))
	if len(c.AllowedCountries) == 0 || len(c.AllowedCities) == 0 || c.WorkRights == "" {
		t.Error("allowed countries, allowed cities and work rights must all be set")
	}
}
