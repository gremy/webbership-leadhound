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
		{"London, United Kingdom", Pass, Fail}, // a bare place outside the allowed ones is an office
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

// TestLocationFieldBarePlace: a location field that names only places, none of them
// allowed, is an office there; a remote-only candidate fails unless the description says
// fully remote. Unknown places, allowed places, workplace labels and candidates open to
// office work keep today's verdicts.
func TestLocationFieldBarePlace(t *testing.T) {
	const remoteDesc = english + "This role is fully remote."
	openToOffice := fieldCandidate
	openToOffice.RemoteOnly = false
	noLimit := CandidateGates{Languages: []string{"en"}, RemoteOnly: true}
	cases := []struct {
		name, location, description string
		c                           CandidateGates
		want, reasonHas             string
	}{
		{"city with state code", "San Carlos, CA", english, fieldCandidate, Fail, "names San Carlos, CA (United States) and nothing says remote"},
		{"two-word city with state code", "Redwood City, CA", english, fieldCandidate, Fail, "names Redwood City, CA (United States) and nothing says remote"},
		{"city, state, country", "Saint Paul, Minnesota, United States", english, fieldCandidate, Fail, "names Minnesota (United States) or United States, outside your allowed countries, and nothing says remote"},
		{"city with short state code", "Burlington, VT", english, fieldCandidate, Fail, "names Burlington, VT (United States) and nothing says remote"},
		{"city with state code, fully remote", "San Carlos, CA", remoteDesc, fieldCandidate, Pass, "fully remote"},
		{"two-word city, fully remote", "Redwood City, CA", remoteDesc, fieldCandidate, Pass, "fully remote"},
		{"city, state, country, fully remote", "Saint Paul, Minnesota, United States", remoteDesc, fieldCandidate, Pass, "fully remote"},
		{"short state code, fully remote", "Burlington, VT", remoteDesc, fieldCandidate, Pass, "fully remote"},
		{"city in allowed country", "Bucharest", english, fieldCandidate, Pass, ""},
		{"city and allowed country", "Cluj-Napoca, Romania", english, fieldCandidate, Pass, ""},
		{"region holding allowed country", "Europe", english, fieldCandidate, Pass, ""},
		{"allowed city", "Exampleville", english, fieldCandidate, Pass, ""},
		{"unrecognised field", "Somewhere", english, fieldCandidate, Pass, ""},
		{"unrecognised city alone", "Burlington", english, fieldCandidate, Pass, ""},
		{"not remote only", "San Carlos, CA", english, openToOffice, Pass, ""},
		{"no country limit, city", "San Carlos, CA", english, noLimit, Fail, "nothing says remote"},
		{"no country limit, region only", "Europe", english, noLimit, Pass, ""},
		{"remote label", "San Carlos, CA (Remote)", english, fieldCandidate, Pass, ""},
		{"on-site label", "San Carlos, CA (On-site)", english, fieldCandidate, Fail, "on-site in San Carlos"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := Evaluate(Observation{Title: "Backend Engineer", Location: tc.location, Description: tc.description}, tc.c, nil)
			snip := ""
			if tc.want == Fail {
				snip = squash(tc.location)
			}
			checkResult(t, one(t, rs, GateWorkplace), tc.want, tc.reasonHas, snip)
		})
	}
}

// TestLocationFieldBarePlaceLosesToText: a failing office-days phrase in the description
// gives the reason over a bare location field.
func TestLocationFieldBarePlaceLosesToText(t *testing.T) {
	rs := Evaluate(Observation{Title: "Backend Engineer", Location: "San Carlos, CA", Description: english + "We work 2 days a week in our Lisbon office."}, fieldCandidate, nil)
	checkResult(t, one(t, rs, GateWorkplace), Fail, "Lisbon", "2 days a week in our Lisbon office")
}

// TestLocationFieldBareListedRemote: a source that lists remote jobs only (LinkedIn's
// remote-filtered search) gives a location field with no workplace label; the bare-field
// rule never reads it as an office, so the verdicts stay what they were before the rule.
func TestLocationFieldBareListedRemote(t *testing.T) {
	cases := []struct {
		name, title, location, description string
		wantLoc, wantWP                    string
	}{
		{"city, region, country", "Backend Engineer", "Berlin, Berlin, Germany", english, Pass, Pass},
		{"city with state code", "Backend Engineer", "San Carlos, CA", english, Pass, Pass},
		{"region outside allowed", "Backend Engineer", "APAC", english, Pass, Pass},
		{"remote word in title", "Backend Engineer (Remote, EU)", "Berlin, Germany", english, Pass, Pass},
		{"office days still fail", "Backend Engineer", "Berlin, Germany", english + "We work 2 days a week in our Lisbon office.", Pass, Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := Observation{Title: tc.title, Location: tc.location, Description: tc.description, ListedRemote: true}
			rs := Evaluate(obs, fieldCandidate, nil)
			loc, wp := one(t, rs, GateLocation), one(t, rs, GateWorkplace)
			if loc.Verdict != tc.wantLoc {
				t.Errorf("location = %s (%q), want %s", loc.Verdict, loc.Reason, tc.wantLoc)
			}
			if wp.Verdict != tc.wantWP {
				t.Errorf("workplace = %s (%q), want %s", wp.Verdict, wp.Reason, tc.wantWP)
			}
			if strings.Contains(wp.Reason, "location field") {
				t.Errorf("workplace reason %q reads the location field; a listed-remote source must not", wp.Reason)
			}
		})
	}
	// The same field from a source that lists every workplace is read as an office.
	rs := Evaluate(Observation{Title: "Backend Engineer", Location: "Berlin, Berlin, Germany", Description: english}, fieldCandidate, nil)
	checkResult(t, one(t, rs, GateWorkplace), Fail, "names Berlin (Germany)", "Berlin, Berlin, Germany")
}

// TestLocationFieldBareRemoteWord: a bare field outside the allowed places, with a plain
// un-negated remote word in the title or description, flags rather than fails: the posting
// says remote but not from where.
func TestLocationFieldBareRemoteWord(t *testing.T) {
	cases := []struct {
		name, title, description string
		want, reasonHas          string
	}{
		{"remote in title", "Backend Engineer (Remote, EU)", english, Flag, "the posting says remote but not where from"},
		{"remote role in description", "Backend Engineer", english + "This is a remote role open to candidates across the EU.", Flag, "says remote but not where from"},
		{"work remotely", "Backend Engineer", english + "You can work remotely from anywhere in Europe.", Flag, "says remote but not where from"},
		{"negated remote", "Backend Engineer", english + "Please note this is not a remote role.", Fail, "nothing says remote"},
		{"remote word inside another word", "Backend Engineer", english + "We build remoteness tooling.", Fail, "nothing says remote"},
		// No pattern reads "anywhere" as remote; see the report's uncertainties.
		{"work from anywhere", "Backend Engineer", english + "Work from anywhere.", Fail, "nothing says remote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := Evaluate(Observation{Title: tc.title, Location: "Berlin, Germany", Description: tc.description}, fieldCandidate, nil)
			checkResult(t, one(t, rs, GateWorkplace), tc.want, tc.reasonHas, "Berlin, Germany")
		})
	}
}

// TestLocationFieldBareNoCity: a bare field naming only countries or regions reads as
// outside the allowed countries; with no country limit it gives none.
func TestLocationFieldBareNoCity(t *testing.T) {
	noLimit := CandidateGates{Languages: []string{"en"}, RemoteOnly: true}
	cases := []struct {
		name, location  string
		c               CandidateGates
		want, reasonHas string
	}{
		{"region", "APAC", fieldCandidate, Fail, "The location field names APAC, outside your allowed countries, and nothing says remote; you want remote only."},
		{"country", "Germany", fieldCandidate, Fail, "names Germany, outside your allowed countries, and nothing says remote"},
		{"no limit, country", "Germany", noLimit, Pass, ""},
		{"no limit, region", "APAC", noLimit, Pass, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := Evaluate(Observation{Title: "Backend Engineer", Location: tc.location, Description: english}, tc.c, nil)
			snip := ""
			if tc.want == Fail {
				snip = tc.location
			}
			checkResult(t, one(t, rs, GateWorkplace), tc.want, tc.reasonHas, snip)
		})
	}
	// A city in the field keeps the city wording.
	rs := Evaluate(Observation{Title: "Backend Engineer", Location: "Berlin, Germany", Description: english}, fieldCandidate, nil)
	r := one(t, rs, GateWorkplace)
	if strings.Contains(r.Reason, "outside your allowed countries") {
		t.Errorf("reason = %q; a field naming a city keeps the city wording", r.Reason)
	}
}
