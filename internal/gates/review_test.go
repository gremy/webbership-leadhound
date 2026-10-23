package gates

import (
	"testing"
)

// neutral is a profile with no country limit and English only, as the collector uses
// until a profile is loaded.
var neutral = CandidateGates{Languages: []string{"en"}}

func TestNoCountryLimit(t *testing.T) {
	cases := []struct {
		name, text, gate, verdict string
	}{
		{"based in a country flags", english + "You must be based in Portugal.", GateLocation, Flag},
		{"based in the EU passes", english + "You must be based in the EU.", GateLocation, Pass},
		{"remote from a country flags", english + "This role is remote from Spain.", GateLocation, Flag},
		{"right to work in the UK flags", english + "You need the right to work in the UK.", GateLocation, Flag},
		{"EU citizenship passes", english + "EU citizenship required.", GateWorkRights, Pass},
		{"no sponsorship passes", english + "Visa sponsorship is not available.", GateWorkRights, Pass},
		{"US only still fails", english + "US citizens only.", GateWorkRights, Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Title: "Backend Engineer", Description: tc.text}, neutral, nil), tc.gate)
			if r.Verdict != tc.verdict {
				t.Fatalf("%s = %s (%q), want %s", tc.gate, r.Verdict, r.Reason, tc.verdict)
			}
		})
	}
}

func TestNoLanguageLimit(t *testing.T) {
	ro := "Căutăm un inginer backend cu experiență în Go și PostgreSQL. Vei lucra alături de echipa de produs pentru a livra funcționalități noi în fiecare săptămână. Oferim program flexibil și cursuri plătite de firmă."
	cases := []struct {
		name  string
		langs []string
	}{
		{"no languages means any", nil},
		{"romanian allowed", []string{"en", "ro"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Description: ro}, CandidateGates{Languages: tc.langs}, nil), GateLanguage)
			if r.Verdict != Pass {
				t.Fatalf("language = %s (%q), want pass", r.Verdict, r.Reason)
			}
		})
	}
}

func TestWorkplacePhrasing(t *testing.T) {
	cases := []struct {
		name, title, location, text string
		remoteOnly                  bool
		verdict                     string
	}{
		{"hybrid model", "Backend Engineer", "", english + "We work in a hybrid model, 2 days in the office.", false, Flag},
		{"hybrid environment", "Backend Engineer", "", english + "We offer a hybrid environment.", false, Flag},
		{"days slash week", "Backend Engineer", "", english + "Expect 2 days/week at the office.", false, Flag},
		{"bare days in the office", "Backend Engineer", "", english + "You join us 3 days in the office.", false, Flag},
		{"hybrid cloud title", "Hybrid Cloud Engineer", "", english, true, Pass},
		{"fully remote elsewhere, hybrid here", "Backend Engineer", "",
			english + "While some teams are fully remote, this role is hybrid with 3 days a week in the office in Berlin.", false, Flag},
		{"fully remote elsewhere, hybrid here, remote only", "Backend Engineer", "",
			english + "While some teams are fully remote, this role is hybrid with 3 days a week in the office in Berlin.", true, Fail},
		{"fully remote and on-site", "Backend Engineer", "", english + "We are fully remote. This position is on-site.", false, Flag},
		{"negation stops at the comma", "Backend Engineer", "", english + "No meetings culture, fully remote team, or hybrid if you prefer.", false, Pass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := neutral
			c.RemoteOnly = tc.remoteOnly
			r := one(t, Evaluate(Observation{Title: tc.title, Location: tc.location, Description: tc.text}, c, nil), GateWorkplace)
			if r.Verdict != tc.verdict {
				t.Fatalf("workplace = %s (%q, %q), want %s", r.Verdict, r.Reason, r.Snippet, tc.verdict)
			}
		})
	}
}

func TestLocationReview(t *testing.T) {
	pt := candidate
	cases := []struct {
		name, text string
		c          CandidateGates
		verdict    string
	}{
		{"customs clearance", english + "You will handle customs clearance for our shipments.", pt, Pass},
		{"clearance not required", english + "Security clearance is not required.", pt, Pass},
		{"clearance not needed", english + "Security clearance is not needed.", pt, Pass},
		{"clearance not a requirement", english + "Security clearance is not a requirement.", pt, Pass},
		{"SC clearance", english + "You hold SC clearance.", pt, Fail},
		{"DV cleared", english + "Candidates must be DV cleared.", pt, Fail},
		{"TS/SCI", english + "An active TS/SCI is needed.", pt, Fail},
		{"secret clearance", english + "Secret clearance needed.", pt, Fail},
		{"right to work UK or EU", english + "You need the right to work in the UK or the EU.", pt, Pass},
		{"remote from slash list", english + "This role is remote from Spain/Portugal only.", pt, Pass},
		{"remote from comma list", english + "This role is remote from Spain,Portugal only.", pt, Pass},
		{"unknown place flags", english + "You must be based in Fooland.", pt, Flag},
		{"we offer relocation", english + "We offer relocation to Berlin.", pt, Flag},
		{"relocation required", english + "Relocation to Berlin required.", pt, Flag},
		{"US residents only", english + "Open to US residents only.", pt, Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Description: tc.text}, tc.c, nil), GateLocation)
			if r.Verdict != tc.verdict {
				t.Fatalf("location = %s (%q, %q), want %s", r.Verdict, r.Reason, r.Snippet, tc.verdict)
			}
		})
	}
}

func TestWorkRightsReview(t *testing.T) {
	pt := candidate
	cases := []struct {
		name, text, verdict string
	}{
		{"US or EU", english + "You are authorized to work in the United States or the European Union.", Pass},
		{"US residents only", english + "Open to US residents only.", Fail},
		{"US only after a list", english + "You are authorized to work in the United States.", Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Description: tc.text}, pt, nil), GateWorkRights)
			if r.Verdict != tc.verdict {
				t.Fatalf("work_rights = %s (%q, %q), want %s", r.Verdict, r.Reason, r.Snippet, tc.verdict)
			}
		})
	}
}

func TestFluentGermanStillFlags(t *testing.T) {
	r := one(t, Evaluate(Observation{Description: english + "Fluent German is a plus."}, neutral, nil), GateLanguage)
	if r.Verdict != Flag {
		t.Fatalf("language = %s (%q), want flag", r.Verdict, r.Reason)
	}
}

func TestPrecomputedLanguage(t *testing.T) {
	// The text is English, but a caller that already detected French says so; the gate
	// uses the caller's detection.
	obs := Observation{Description: english, Lang: "fr", LangConfidence: 0.9}
	r := one(t, Evaluate(obs, neutral, nil), GateLanguage)
	if r.Verdict != Fail || r.Reason != "posting is in French" {
		t.Fatalf("language = %s (%q), want fail in French", r.Verdict, r.Reason)
	}
	obs = Observation{Description: english, Lang: "en", LangConfidence: 0.3}
	if r := one(t, Evaluate(obs, neutral, nil), GateLanguage); r.Verdict != Flag {
		t.Fatalf("low confidence = %s (%q), want flag", r.Verdict, r.Reason)
	}
}
