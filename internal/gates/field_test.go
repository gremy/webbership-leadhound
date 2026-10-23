package gates

import (
	"strings"
	"testing"
)

// fieldCandidate is remote only, allowed in Romania and in one fictional allowed city.
var fieldCandidate = CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Romania"}, AllowedCities: []string{"Exampleville"}, RemoteOnly: true}

// TestLocationField runs the gates over the location strings most common in a live ATS
// sample (with an English description that says nothing about place or workplace).
func TestLocationField(t *testing.T) {
	cases := []struct {
		location        string
		wantLoc, wantWP string
	}{
		{"Remote", Pass, Pass},
		{"Menlo Park, CA (On-site)", Pass, Fail},
		{"San Carlos, CA (On-site)", Pass, Fail},
		{"Vilnius, Kaunas (Hybrid)", Pass, Fail},
		{"San Francisco (On-site)", Pass, Fail},
		{"London, United Kingdom", Pass, Pass},
		{"Home based - Worldwide", Pass, Pass},
		{"Home based - EMEA", Pass, Pass},
		{"United States (Remote)", Fail, Pass},
		{"Lisbon (On-site)", Pass, Fail},
		{"Berlin Metropolitain Area (Remote)", Flag, Pass}, //nolint:misspell // the spelling a live field used
		{"Berlin Metropolitan Area (Remote)", Flag, Pass},
		{"Madrid (Remote)", Flag, Pass},
		{"Paris (Remote)", Flag, Pass},
		// Edge cases.
		{"Remote - US or Canada", Fail, Pass},
		{"Remote (Europe)", Pass, Pass},
		{"US Remote", Fail, Pass},
		{"Canada (Remote)", Fail, Pass},
		{"California (Remote)", Fail, Pass},
		{"Remote - LATAM", Fail, Pass},
		{"Remote, CET +/- 2 hours", Pass, Pass},
		{"Global (Remote)", Pass, Pass},
		{"Bucharest (Remote)", Pass, Pass},
		{"Remote - US or Romania", Pass, Pass},
		{"Menlo Park, CA (Remote)", Flag, Pass},
		{"Exampleville (On-site)", Pass, Flag},
		{"Exampleville, Otherville (Hybrid)", Pass, Flag},
		{"Exampleville (Remote)", Pass, Pass},
		{"On-site", Pass, Fail},
		{"In office - Paris", Pass, Fail},
		{"Remote, London (Hybrid)", Pass, Flag}, // says two workplaces: unchanged (the hybrid word flags)
		{"Georgia (Remote)", Pass, Pass},        // neither reading is decided on
		{"Atlanta, GA (Remote)", Flag, Pass},
		{"Somewhere (Remote)", Pass, Pass},
		{"", Pass, Pass},
	}
	for _, tc := range cases {
		t.Run(tc.location, func(t *testing.T) {
			rs := Evaluate(Observation{Title: "Backend Engineer", Location: tc.location, Description: english}, fieldCandidate, nil)
			loc, wp := one(t, rs, GateLocation), one(t, rs, GateWorkplace)
			if loc.Verdict != tc.wantLoc {
				t.Errorf("location = %s (%q), want %s", loc.Verdict, loc.Reason, tc.wantLoc)
			}
			if wp.Verdict != tc.wantWP {
				t.Errorf("workplace = %s (%q), want %s", wp.Verdict, wp.Reason, tc.wantWP)
			}
			for _, r := range []Result{loc, wp} {
				if r.Verdict != Pass && strings.Contains(r.Reason, "location field") && r.Snippet != squash(tc.location) {
					t.Errorf("%s snippet = %q, want the location field", r.Gate, r.Snippet)
				}
			}
		})
	}
}

// TestLocationFieldReasons: the reason names the field and its places.
func TestLocationFieldReasons(t *testing.T) {
	rs := Evaluate(Observation{Title: "Backend Engineer", Location: "United States (Remote)", Description: english}, fieldCandidate, nil)
	checkResult(t, one(t, rs, GateLocation), Fail, "remote, limited to United States", "United States (Remote)")
	rs = Evaluate(Observation{Title: "Backend Engineer", Location: "Menlo Park, CA (On-site)", Description: english}, fieldCandidate, nil)
	checkResult(t, one(t, rs, GateWorkplace), Fail, "on-site in Menlo Park, CA (United States)", "Menlo Park, CA (On-site)")
}

// TestLocationFieldAllowedCountryCity: hybrid in an allowed country but not an allowed
// city fails a remote-only candidate (no office days outside the home city).
func TestLocationFieldAllowedCountryCity(t *testing.T) {
	c := CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Romania"}, RemoteOnly: true}
	rs := Evaluate(Observation{Title: "Backend Engineer", Location: "Bucharest (Hybrid)", Description: english}, c, nil)
	checkResult(t, one(t, rs, GateWorkplace), Fail, "hybrid in Bucharest (Romania)", "Bucharest (Hybrid)")
	if r := one(t, rs, GateLocation); r.Verdict != Pass {
		t.Errorf("location = %s (%q), want pass", r.Verdict, r.Reason)
	}
}

// TestLocationFieldNotRemoteOnly: a candidate open to office work keeps today's flags.
func TestLocationFieldNotRemoteOnly(t *testing.T) {
	c := fieldCandidate
	c.RemoteOnly = false
	for _, loc := range []string{"Menlo Park, CA (On-site)", "Vilnius, Kaunas (Hybrid)", "In office - Paris"} {
		rs := Evaluate(Observation{Title: "Backend Engineer", Location: loc, Description: english}, c, nil)
		if r := one(t, rs, GateWorkplace); r.Verdict != Flag {
			t.Errorf("%s: workplace = %s (%q), want flag", loc, r.Verdict, r.Reason)
		}
	}
}

// TestLocationFieldDescriptionSaysRemote: an on-site field against a fully remote
// description only flags.
func TestLocationFieldDescriptionSaysRemote(t *testing.T) {
	rs := Evaluate(Observation{Title: "Backend Engineer", Location: "Lisbon (On-site)", Description: english + "This role is fully remote."}, fieldCandidate, nil)
	checkResult(t, one(t, rs, GateWorkplace), Flag, "also says fully remote", "Lisbon (On-site)")
}

// TestLocationFieldNoCountryLimit mirrors the description patterns: a place can flag but
// never fail when the candidate set no country limit.
func TestLocationFieldNoCountryLimit(t *testing.T) {
	c := CandidateGates{Languages: []string{"en"}}
	for loc, want := range map[string]string{"United States (Remote)": Flag, "Remote (Europe)": Pass, "Remote": Pass} {
		rs := Evaluate(Observation{Title: "Backend Engineer", Location: loc, Description: english}, c, nil)
		if r := one(t, rs, GateLocation); r.Verdict != want {
			t.Errorf("%s: location = %s (%q), want %s", loc, r.Verdict, r.Reason, want)
		}
	}
}

// TestPlaceCodes: a state or province code counts only right after a comma, and a
// country's own code keeps its city.
func TestPlaceCodes(t *testing.T) {
	cases := []struct {
		text  string
		names []string
	}{
		{"Menlo Park, CA", []string{"Menlo Park, CA (United States)"}},
		{"Austin, TX (Remote)", []string{"Austin, TX (United States)"}},
		{"Cambridge, MA", []string{"Cambridge, MA (United States)"}},
		{"Cambridge, United Kingdom", []string{"Cambridge (United Kingdom)", "United Kingdom"}},
		{"Toronto, CA", []string{"Toronto (Canada)"}},
		{"Tel Aviv, IL", []string{"Tel Aviv (Israel)"}},
		{"Berlin, DE", []string{"Berlin (Germany)"}},
		{"Washington, DC", []string{"Washington, DC (United States)"}},
		{"We rotate CA certificates yearly.", nil},
		{"Experience with PKI, CA and HSMs.", nil},
		{"Based in Lisbon, CA licence a plus", []string{"Lisbon (Portugal)"}},
		{"Georgia", nil},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			var got []string
			for _, p := range findPlaces(tc.text) {
				got = append(got, p.name)
			}
			if strings.Join(got, "|") != strings.Join(tc.names, "|") {
				t.Errorf("findPlaces(%q) = %q, want %q", tc.text, got, tc.names)
			}
		})
	}
}

// TestCAInProse: "CA" in a description sentence is not a place.
func TestCAInProse(t *testing.T) {
	rs := Evaluate(Observation{Title: "Backend Engineer", Description: english + "You must be based in the EU, CA experience is a plus."}, candidate, nil)
	checkResult(t, one(t, rs, GateLocation), Pass, "EU", "based in the EU")
}

// TestHubTableHasNoHomeCity: allowed cities come from the private profile only, so the
// public table holds none of the candidate's; a fictional allowed city is never in it.
func TestHubTableHasNoHomeCity(t *testing.T) {
	if _, ok := cityCountry["exampleville"]; ok {
		t.Fatal("the hub table holds a test candidate's allowed city")
	}
	n := 0
	for _, cs := range hubCities {
		n += len(cs)
	}
	if n < 200 {
		t.Errorf("hub table has %d names, want a few hundred", n)
	}
}
