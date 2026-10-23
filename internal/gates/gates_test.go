package gates

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// english is a neutral English posting body: no location, workplace or work-rights terms.
const english = "We are looking for a senior backend engineer to design and maintain services written in Go. " +
	"You will work closely with product managers and other engineers to ship features every week. " +
	"We expect strong experience with distributed systems, PostgreSQL and cloud infrastructure. "

func TestLanguageEdgeCases(t *testing.T) {
	cases := []struct {
		name, text, verdict, reasonHas string
	}{
		{"portuguese allowed",
			"Procuramos um engenheiro backend com experiência em Go e PostgreSQL. Vais trabalhar com a equipa de produto para entregar novas funcionalidades todas as semanas. Oferecemos horário flexível e formação paga pela empresa.",
			Pass, "Portuguese"},
		{"romanian not allowed",
			"Căutăm un inginer backend cu experiență în Go și PostgreSQL. Vei lucra alături de echipa de produs pentru a livra funcționalități noi în fiecare săptămână. Oferim program flexibil și cursuri plătite de firmă.",
			Fail, "Romanian"},
		{"english asks fluent german",
			english + "Fluent German is essential for daily work with our clients.",
			Flag, "asks for fluent German"},
		{"english c1 dutch",
			english + "You speak C1 Dutch.",
			Flag, "asks for fluent Dutch"},
		{"english french required",
			english + "French is required for this role.",
			Flag, "asks for fluent French"},
		{"english spanish a must",
			english + "Spanish is a must.",
			Flag, "asks for fluent Spanish"},
		{"english native english is fine",
			english + "Native English speakers welcome.",
			Pass, "English"},
		{"unclear", "Go, PostgreSQL, Kubernetes", Flag, "language unclear"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := Evaluate(Observation{Title: "Backend Engineer", Description: tc.text}, candidate, nil)
			r := one(t, rs, GateLanguage)
			if r.Verdict != tc.verdict || !strings.Contains(r.Reason, tc.reasonHas) {
				t.Fatalf("got %s %q, want %s containing %q", r.Verdict, r.Reason, tc.verdict, tc.reasonHas)
			}
			if r.Verdict != Fail {
				// A language flag is not a fail: the other gates still run.
				one(t, rs, GateLocation)
			}
		})
	}
}

func TestDetectLanguageEmpty(t *testing.T) {
	if iso, conf := DetectLanguage("   "); iso != "" || conf != 0 {
		t.Errorf("DetectLanguage(blank) = %q, %v", iso, conf)
	}
}

func TestLocationGate(t *testing.T) {
	cases := []struct {
		name, text, verdict, reasonHas, snippetHas string
	}{
		{"none", english, Pass, "", ""},
		{"must be based in Spain", english + "You must be based in Spain.", Fail, "Spain", "must be based in Spain"},
		{"must reside in Portugal", english + "Candidates must reside in Portugal.", Flag, "Portugal", "must reside in Portugal"},
		{"must live in Lisbon", english + "You must live in Lisbon or nearby.", Flag, "Portugal", "Lisbon"},
		{"must be based in unknown city", english + "You must be based in Vilnius.", Fail, "Vilnius", "Vilnius"},
		{"must be based in Fooland", english + "You must be based in Fooland.", Flag, "Fooland", "Fooland"},
		{"must be based in the EU", english + "You must be based in the EU.", Pass, "EU", "EU"},
		{"must live within commuting distance", english + "You must live in commuting distance of the office.", Flag, "", "commuting"},
		{"only candidates located in", english + "We consider only candidates located in Germany.", Fail, "Germany", "Germany"},
		{"exclusively for candidates based in", english + "This role is exclusively for candidates based in the Netherlands.", Fail, "Netherlands", "Netherlands"},
		{"DNI", english + "Imprescindible DNI o NIE vigente.", Fail, "Spain", "DNI"},
		{"nie lowercase is not a NIE", english + "We know it is nie easy.", Pass, "", ""},
		{"right to work UK", english + "You need the right to work in the UK.", Fail, "United Kingdom", "right to work in the UK"},
		{"right to work US", english + "Applicants need the right to work in the US.", Fail, "United States", "US"},
		{"US citizens only", english + "US citizens only.", Fail, "United States", "US citizens only"},
		{"security clearance", english + "An active security clearance is needed.", Fail, "clearance", "security clearance"},
		{"no clearance needed", english + "No security clearance needed.", Pass, "", ""},
		{"relocate to Berlin", english + "You are willing to relocate to Berlin.", Flag, "Germany", "relocate to Berlin"},
		{"relocation required", english + "Relocation required.", Flag, "relocat", "Relocation required"},
		{"no relocation required", english + "No relocation required.", Pass, "", ""},
		{"remote from Spain", english + "This role is remote from Spain.", Fail, "Spain", "remote from Spain"},
		{"remote from Portugal", english + "This role is remote from Portugal.", Pass, "", ""},
		{"remote from anywhere in Europe", english + "Work remote from anywhere in Europe.", Pass, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Title: "Backend Engineer", Description: tc.text}, candidate, nil), GateLocation)
			checkResult(t, r, tc.verdict, tc.reasonHas, tc.snippetHas)
		})
	}
}

func TestLocationAllowedElsewhere(t *testing.T) {
	c := CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Spain", "Portugal"}}
	r := one(t, Evaluate(Observation{Description: english + "You must be based in Spain."}, c, nil), GateLocation)
	checkResult(t, r, Flag, "Spain", "Spain")
}

func TestWorkplaceGate(t *testing.T) {
	cases := []struct {
		name, title, location, text string
		remoteOnly                  bool
		verdict, reasonHas          string
		snippetHas                  string
	}{
		{"none", "Backend Engineer", "", english, true, Pass, "", ""},
		{"hybrid", "Backend Engineer", "", english + "This is a hybrid role.", false, Flag, "hybrid", "hybrid role"},
		{"hybrid in title", "Drupal Developer (Hybrid)", "", english, false, Flag, "hybrid", "Hybrid"},
		{"hybrid cloud title", "Hybrid Cloud Engineer", "", english, true, Pass, "", ""},
		{"as a hybrid cloud engineer", "Backend Engineer", "", "As a hybrid cloud engineer you will " + english, true, Pass, "", ""},
		{"office days flag", "Backend Engineer", "", english + "We work 2 days a week in the office.", false, Flag, "office", "2 days a week in the office"},
		{"office days remote only", "Backend Engineer", "Amsterdam, Netherlands", english + "We work 2 days a week in the office.", true, Fail, "Amsterdam", "2 days a week in the office"},
		{"office days words", "Backend Engineer", "", english + "Expect two days per week at the office.", false, Flag, "office", "two days per week at the office"},
		{"office days romanian", "Inginer", "", english + "Program: 3 zile pe săptămână la birou.", false, Flag, "office", "3 zile pe săptămână la birou"},
		{"onsite", "Backend Engineer", "", english + "This position is on-site.", false, Flag, "on-site", "on-site"},
		{"onsite joined", "Backend Engineer", "", english + "Onsite in our Paris office.", false, Flag, "on-site", "Onsite"},
		{"in-office", "Backend Engineer", "", english + "This is an in-office role.", false, Flag, "office", "in-office"},
		{"remote-first optional office", "Backend Engineer", "Berlin, Germany", english + "We are remote-first, optional office in Berlin.", true, Pass, "remote", "remote-first"},
		{"fully remote beats hybrid", "Backend Engineer", "", english + "Fully remote, or hybrid if you prefer.", true, Pass, "remote", "Fully remote"},
		{"100% remote with on-site visits", "Backend Engineer", "", english + "The job is 100% remote but on-site visits happen.", true, Flag, "also says fully remote", "on-site"},
		{"not fully remote", "Backend Engineer", "", english + "This is not a fully remote role; it is hybrid.", false, Flag, "hybrid", "hybrid"},
		// The office's city comes from the office-days phrase first, then the text, then the location line.
		{"office days in named city", "Backend Engineer", "", english + "Hybrid role: two days a week in our Lisbon office.", true, Fail, "Lisbon", "two days a week in our Lisbon office"},
		{"office days in named city not remote only", "Backend Engineer", "", english + "Hybrid role: two days a week in our Lisbon office.", false, Flag, "hybrid", "Hybrid"},
		{"office days in allowed named city", "Backend Engineer", "Lisbon, Portugal", english + "Hybrid role: two days a week in our Exampleville office.", true, Flag, "hybrid", "Hybrid"},
		{"from named office days a week", "Backend Engineer", "", english + "Work from our Lisbon office three days a week.", true, Fail, "Lisbon", "Lisbon office three days"},
		{"from allowed named office days a week", "Backend Engineer", "Lisbon, Portugal", english + "Work from our Exampleville office three days a week.", true, Flag, "office", "Exampleville office three days"},
		{"named office then days a week", "Backend Engineer", "", english + "Our Lisbon office, 3 days per week.", true, Fail, "Lisbon", "Lisbon office, 3 days per week"},
		{"allowed office named elsewhere beats location", "Backend Engineer", "Lisbon, Portugal", english + "Hybrid role in Exampleville office, two days a week in the office.", true, Flag, "hybrid", "Hybrid"},
		{"named office elsewhere not allowed", "Backend Engineer", "Exampleville, Exampleland", english + "Hybrid role in Lisbon office, two days a week in the office.", true, Fail, "Lisbon", "two days a week in the office"},
		{"main office is not a city", "Backend Engineer", "Lisbon, Portugal", english + "We work two days a week in the Main office.", true, Fail, "Lisbon", "two days a week in the Main office"},
		{"main office in allowed city", "Backend Engineer", "Exampleville, Exampleland", english + "We work two days a week in the Main office.", true, Flag, "office", "two days a week in the Main office"},
		{"bare hybrid in another city stays flag", "Backend Engineer", "Lisbon, Portugal", english + "This is a hybrid role.", true, Flag, "hybrid", "hybrid role"},
		{"office open days is not office days", "Backend Engineer", "", english + "Our Lisbon office is open five days.", true, Pass, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate
			c.RemoteOnly = tc.remoteOnly
			c.AllowedCities = []string{"Exampleville"}
			obs := Observation{Title: tc.title, Location: tc.location, Description: tc.text}
			r := one(t, Evaluate(obs, c, nil), GateWorkplace)
			checkResult(t, r, tc.verdict, tc.reasonHas, tc.snippetHas)
		})
	}
}

func TestWorkRightsGate(t *testing.T) {
	cases := []struct {
		name, text, verdict, reasonHas, snippetHas string
	}{
		{"none", english, Pass, "", ""},
		{"EU citizen", english + "You are an EU citizen or hold an EU work permit.", Pass, "EU", "EU citizen"},
		{"no visa sponsorship", english + "Visa sponsorship is not available for this role.", Pass, "sponsorship", "Visa sponsorship is not available"},
		{"no visa sponsorship provided", english + "Visa sponsorship not provided.", Pass, "sponsorship", "Visa sponsorship not provided"},
		{"US citizens only", english + "US citizens only.", Fail, "United States", "US citizens only"},
		{"must be a US citizen", english + "You must be a US citizen.", Fail, "United States", "US citizen"},
		{"authorized to work in the US", english + "Must be authorized to work in the United States.", Fail, "United States", "authorized to work in the United States"},
		{"lowercase us is not the US", english + "Join us citizens of the web only if you like.", Pass, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, Evaluate(Observation{Title: "Backend Engineer", Description: tc.text}, candidate, nil), GateWorkRights)
			checkResult(t, r, tc.verdict, tc.reasonHas, tc.snippetHas)
		})
	}
}

func TestRules(t *testing.T) {
	obs := Observation{Title: "Senior Drupal Developer", Company: "Example Staffing Ltd", Location: "Remote", Description: english}
	rules := []Rule{
		{ID: 7, Kind: KindSkip, Field: "company", Pattern: "example staffing", Reason: "agency we skip"},
		{ID: 8, Kind: KindFlag, Field: "title", Pattern: `\bsenior\b`},
		{ID: 9, Kind: KindSkip, Field: "description", Pattern: "kotlin"},
		{ID: 10, Kind: KindSkip, Field: "location", Pattern: "("},
	}
	rs := byGate(Evaluate(obs, candidate, rules))[GateRule]
	if len(rs) != 3 {
		t.Fatalf("want 3 rule results (skip, flag, broken pattern), got %+v", rs)
	}
	want := []struct {
		id      int64
		verdict string
		has     string
	}{{7, Fail, "agency we skip"}, {8, Flag, "rule 8"}, {10, Flag, "does not compile"}}
	for i, w := range want {
		r := rs[i]
		if r.RuleID == nil || *r.RuleID != w.id || r.Verdict != w.verdict || !strings.Contains(r.Reason, w.has) {
			t.Errorf("rule result %d = %+v (id %v), want id %d %s containing %q", i, r, deref(r.RuleID), w.id, w.verdict, w.has)
		}
	}
	if !strings.Contains(rs[0].Snippet, "Example Staffing") {
		t.Errorf("skip snippet = %q", rs[0].Snippet)
	}
	// Rules come before every other gate.
	if all := Evaluate(obs, candidate, rules); all[0].Gate != GateRule {
		t.Errorf("first result is %s, want rule", all[0].Gate)
	}
}

func TestEvaluateEmitsEveryGateOnce(t *testing.T) {
	rs := Evaluate(Observation{Title: "Backend Engineer", Description: english}, candidate, nil)
	for _, g := range []string{GateLanguage, GateLocation, GateWorkplace, GateWorkRights} {
		r := one(t, rs, g)
		if r.Reason == "" {
			t.Errorf("gate %s has no reason", g)
		}
	}
	if len(rs) != 4 {
		t.Errorf("want 4 results, got %d", len(rs))
	}
}

func TestSnippetBounds(t *testing.T) {
	long := strings.Repeat("Ünïcode padding text ", 40)
	text := long + "You must be based in Spain. " + long
	for _, r := range Evaluate(Observation{Description: text}, candidate, nil) {
		if n := utf8.RuneCountInString(r.Snippet); n > 120 {
			t.Errorf("gate %s snippet has %d runes", r.Gate, n)
		}
		if !utf8.ValidString(r.Snippet) {
			t.Errorf("gate %s snippet is not valid UTF-8", r.Gate)
		}
	}
	r := one(t, Evaluate(Observation{Description: text}, candidate, nil), GateLocation)
	if !strings.Contains(r.Snippet, "must be based in Spain") {
		t.Errorf("snippet lost the match: %q", r.Snippet)
	}
}

func checkResult(t *testing.T, r Result, verdict, reasonHas, snippetHas string) {
	t.Helper()
	if r.Verdict != verdict {
		t.Fatalf("verdict = %s (%q, snippet %q), want %s", r.Verdict, r.Reason, r.Snippet, verdict)
	}
	if r.Reason == "" || !strings.Contains(strings.ToLower(r.Reason), strings.ToLower(reasonHas)) {
		t.Errorf("reason = %q, want it to contain %q", r.Reason, reasonHas)
	}
	if !strings.Contains(r.Snippet, snippetHas) {
		t.Errorf("snippet = %q, want it to contain %q", r.Snippet, snippetHas)
	}
	if utf8.RuneCountInString(r.Snippet) > 120 {
		t.Errorf("snippet longer than 120 runes: %q", r.Snippet)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
