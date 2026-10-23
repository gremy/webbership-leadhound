package gates

import "testing"

// candidate is the fictional gate settings every test uses unless it says otherwise:
// English and Portuguese, employable from Portugal, open to office work.
var candidate = CandidateGates{Languages: []string{"en", "pt"}, AllowedCountries: []string{"Portugal"}}

// byGate indexes results by gate; a test that expects one result per gate uses it.
func byGate(rs []Result) map[string][]Result {
	m := map[string][]Result{}
	for _, r := range rs {
		m[r.Gate] = append(m[r.Gate], r)
	}
	return m
}

// one returns the single result for gate, failing the test when there is not exactly one.
func one(t *testing.T, rs []Result, gate string) Result {
	t.Helper()
	got := byGate(rs)[gate]
	if len(got) != 1 {
		t.Fatalf("gate %s: want 1 result, got %d (%+v)", gate, len(got), got)
	}
	return got[0]
}
