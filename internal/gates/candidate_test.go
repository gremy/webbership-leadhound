package gates

import (
	"strings"
	"testing"
)

// TestAllowedCities: a residence requirement naming one of the candidate's allowed cities
// passes, whatever its spelling of case or diacritics; any other city is read as before.
func TestAllowedCities(t *testing.T) {
	c := CandidateGates{
		Languages:        []string{"en"},
		AllowedCountries: []string{"Portugal"},
		AllowedCities:    []string{"Exampleville", "Șamplecity"},
	}
	cases := []struct {
		name, text, verdict, reasonHas string
	}{
		{"based in the allowed city", "Candidates must be based in Exampleville.", Pass, "allowed cities"},
		{"lower case", "You must live in exampleville or nearby.", Pass, "allowed cities"},
		{"diacritics folded", "You must reside in Samplecity.", Pass, "allowed cities"},
		{"relocate to the allowed city", "Relocation to Exampleville is offered.", Pass, "allowed cities"},
		{"unknown city still flags", "Candidates must be based in Otherville.", Flag, "do not know"},
		{"known foreign city still fails", "Candidates must be based in Berlin.", Fail, "outside your allowed"},
		{"a longer word is not the city", "Candidates must be based in Examplevilleshire.", Flag, "do not know"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Title: "Backend Engineer", Description: english + tc.text}, c, nil), GateLocation)
			if r.Verdict != tc.verdict || !strings.Contains(r.Reason, tc.reasonHas) {
				t.Fatalf("got %s %q, want %s containing %q", r.Verdict, r.Reason, tc.verdict, tc.reasonHas)
			}
		})
	}
}

// TestAllowedCityOfficeDays: a remote-only candidate is not failed for office days in a
// city they allow; the days still flag.
func TestAllowedCityOfficeDays(t *testing.T) {
	c := CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Portugal"}, RemoteOnly: true, AllowedCities: []string{"Exampleville"}}
	obs := Observation{Title: "Backend Engineer", Location: "Exampleville, Exampleland", Description: english + "You will spend 2 days a week in the office."}
	r := one(t, Evaluate(obs, c, nil), GateWorkplace)
	if r.Verdict != Flag {
		t.Fatalf("allowed city: got %s %q, want flag", r.Verdict, r.Reason)
	}
	obs.Location = "Otherville, Exampleland"
	if r := one(t, Evaluate(obs, c, nil), GateWorkplace); r.Verdict != Fail {
		t.Fatalf("other city: got %s %q, want fail", r.Verdict, r.Reason)
	}
}

// TestWorkRightsField: the candidate's stated work rights decide EU eligibility; only
// without them is it inferred from an allowed EU country.
func TestWorkRightsField(t *testing.T) {
	const asksEU = "Applicants must hold EU citizenship."
	cases := []struct {
		name    string
		c       CandidateGates
		verdict string
	}{
		{"inferred from an EU country", CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Portugal"}}, Pass},
		{"stated EU citizen outside the EU", CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Canada"}, WorkRights: "EU citizen"}, Pass},
		{"stated other rights in the EU", CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Portugal"}, WorkRights: "Exampleland work visa"}, Flag},
		{"no limit, stated other rights", CandidateGates{Languages: []string{"en"}, WorkRights: "Exampleland citizen"}, Flag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Title: "Backend Engineer", Description: english + asksEU}, tc.c, nil), GateWorkRights)
			if r.Verdict != tc.verdict {
				t.Fatalf("got %s %q, want %s", r.Verdict, r.Reason, tc.verdict)
			}
		})
	}
}
