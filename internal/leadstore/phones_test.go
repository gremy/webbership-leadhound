package leadstore_test

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestScreenRegistersMatchCountries keeps the register vocabulary in one shape everywhere:
// the registers of internal/compliance/countries.yaml, leadstore.ScreenRegisters and the
// CHECK list of migration 00020 are the same set.
func TestScreenRegistersMatchCountries(t *testing.T) {
	var yaml []string
	for code := range compliance.Default().Registers {
		yaml = append(yaml, code)
	}
	sort.Strings(yaml)
	if !slices.Equal(yaml, leadstore.ScreenRegisters) {
		t.Errorf("leadstore.ScreenRegisters = %v, countries.yaml registers = %v", leadstore.ScreenRegisters, yaml)
	}
	if !slices.IsSorted(leadstore.ScreenRegisters) {
		t.Errorf("leadstore.ScreenRegisters is not sorted: %v", leadstore.ScreenRegisters)
	}
	b, err := os.ReadFile("migrations/00020_phones_screening.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`register IN \(([^)]*)\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("00020: no register IN (...) check")
	}
	var check []string
	for _, v := range strings.Split(string(m[1]), ",") {
		check = append(check, strings.Trim(strings.TrimSpace(v), "'"))
	}
	sort.Strings(check)
	if !slices.Equal(check, yaml) {
		t.Errorf("00020 phone_screening_register_check = %v, countries.yaml registers = %v", check, yaml)
	}
}
