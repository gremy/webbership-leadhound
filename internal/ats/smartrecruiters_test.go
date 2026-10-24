package ats

import (
	"errors"
	"testing"
)

func TestParseSmartRecruiters(t *testing.T) {
	b := Board{ATS: SmartRecruiters, Board: "ExampleRobotics", Name: "Example Robotics Board"}
	const host = "https://jobs.smartrecruiters.com/ExampleRobotics/"
	want := []Posting{
		{
			ID:             "700000000000001",
			Title:          "Go Platform Engineer",
			Company:        "Example Robotics", // company.name wins over Board.Name
			Location:       "Exampleville, North Region, Exampleland (Remote)",
			EmploymentType: "Full-time",
			PostedAt:       ts(t, "2026-10-14T09:15:00Z"),
			URL:            host + "700000000000001",
		},
		{
			ID:             "700000000000002",
			Title:          "AI Engineer",
			Company:        "Example Robotics",
			Location:       "Sampletown, Exampleland (Hybrid)",
			EmploymentType: "Contract",
			PostedAt:       ts(t, "2026-10-15T09:30:00Z"), // +03:00 to UTC
			URL:            host + "700000000000002",
		},
		{
			ID:             "700000000000003",
			Title:          "Inginer Backend",
			Company:        "Example Robotics",
			Location:       "Exampleville, North Region, XL (On-site)", // no fullLocation: parts joined
			EmploymentType: "Full-time",
			PostedAt:       ts(t, "2026-10-16T08:00:00Z"),
			URL:            host + "700000000000003",
		},
		{
			ID:       "700000000000004",
			Title:    "Data Engineer",
			Company:  "Example Robotics Board", // no company.name: Board.Name
			Location: "",                       // no location object: no workplace stated
			URL:      host + "700000000000004",
		},
		{
			ID:       "700000000000005",
			Title:    "Product Engineer",
			Company:  "Example Robotics",
			Location: "Hybrid", // empty text, remote and hybrid both true: hybrid
			PostedAt: nil,      // unreadable date
			URL:      host + "700000000000005",
		},
	}
	got, err := parseSmartRecruiters(b, readFixture(t, "smartrecruiters_examplerobotics.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertPostings(t, got, want)
}

func TestSmartRecruitersCompanyFallsBackToSlug(t *testing.T) {
	b := Board{ATS: SmartRecruiters, Board: "examplerobotics"}
	got, err := parseSmartRecruiters(b, []byte(`{"totalFound":1,"content":[{"id":"1","name":"Go Engineer"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Company != "examplerobotics" {
		t.Fatalf("got %+v, want Company examplerobotics", got)
	}
	if got[0].URL != "https://jobs.smartrecruiters.com/examplerobotics/1" {
		t.Fatalf("URL = %q", got[0].URL)
	}
}

func TestSmartRecruitersWorkplace(t *testing.T) {
	b := Board{ATS: SmartRecruiters, Board: "examplerobotics"}
	for _, tc := range []struct{ name, loc, want string }{
		{"remote", `"fullLocation":"Exampleville","remote":true,"hybrid":false`, "Exampleville (Remote)"},
		{"hybrid", `"fullLocation":"Exampleville","remote":false,"hybrid":true`, "Exampleville (Hybrid)"},
		{"both true is hybrid", `"fullLocation":"Exampleville","remote":true,"hybrid":true`, "Exampleville (Hybrid)"},
		{"both false is on-site", `"fullLocation":"Exampleville","remote":false,"hybrid":false`, "Exampleville (On-site)"},
		{"only remote false is on-site", `"fullLocation":"Exampleville","remote":false`, "Exampleville (On-site)"},
		{"neither key states nothing", `"fullLocation":"Exampleville"`, "Exampleville"},
		{"text already says remote", `"fullLocation":"Remote, Exampleland","remote":true`, "Remote, Exampleland"},
		{"empty on-site is the bare word", `"remote":false,"hybrid":false`, "On-site"},
		{"parts when no fullLocation", `"city":"Exampleville","country":"xl","remote":true`, "Exampleville, XL (Remote)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"totalFound":1,"content":[{"id":"1","name":"Go Engineer","location":{` + tc.loc + `}}]}`
			got, err := parseSmartRecruiters(b, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Location != tc.want {
				t.Fatalf("got %+v, want Location %q", got, tc.want)
			}
		})
	}
}

func TestParseSmartRecruitersBroken(t *testing.T) {
	b := Board{ATS: SmartRecruiters, Board: "examplerobotics"}
	for name, body := range map[string]string{
		"garbage":         `{{{`,
		"empty":           ``,
		"no content key":  `{"totalFound":0}`,
		"content string":  `{"content":"none"}`,
		"no id":           `{"content":[{"name":"Go Engineer"}]}`,
		"no name":         `{"content":[{"id":"1"}]}`,
		"top-level list":  `[]`,
		"location string": `{"content":[{"id":"1","name":"Go Engineer","location":"Exampleville"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSmartRecruiters(b, []byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
	got, err := parseSmartRecruiters(b, []byte(`{"offset":0,"limit":100,"totalFound":0,"content":[]}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty board: got %v, %v; want no postings and no error", got, err)
	}
}

func TestSmartRecruitersPage(t *testing.T) {
	total, offset, limit, err := smartRecruitersPage(readFixture(t, "smartrecruiters_examplerobotics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || offset != 0 || limit != 100 {
		t.Fatalf("got total %d offset %d limit %d, want 5 0 100", total, offset, limit)
	}
	total, offset, limit, err = smartRecruitersPage([]byte(`{"offset":200,"limit":100,"totalFound":250,"content":[]}`))
	if err != nil || total != 250 || offset != 200 || limit != 100 {
		t.Fatalf("got %d %d %d %v, want 250 200 100 nil", total, offset, limit, err)
	}
	for name, body := range map[string]string{
		"garbage":           `{{{`,
		"no totalFound":     `{"offset":0,"limit":100,"content":[]}`,
		"totalFound string": `{"totalFound":"5","content":[]}`,
		"negative total":    `{"totalFound":-1,"content":[]}`,
		"no content":        `{"totalFound":5}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := smartRecruitersPage([]byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
}

func TestParseSmartRecruitersDetail(t *testing.T) {
	desc, apply, posting, err := parseSmartRecruitersDetail(readFixture(t, "smartrecruiters_examplerobotics_detail.json"))
	if err != nil {
		t.Fatal(err)
	}
	const wantDesc = `<h3>Company Description</h3>` + "\n" + `<p>We build friendly robots.</p>` + "\n" +
		`<h3>Job Description</h3>` + "\n" + `<p>Run the Go platform &amp; its agents.</p>` + "\n" +
		`<h3>Qualifications</h3>` + "\n" + `<ul><li>Go in production</li></ul>` + "\n" +
		`<h3>Additional Information</h3>` + "\n" + `<p>Remote within Exampleland.</p>`
	if desc != wantDesc {
		t.Errorf("description:\n got %q\nwant %q", desc, wantDesc)
	}
	if want := "https://jobs.example.com/ExampleRobotics/700000000000001-go-platform-engineer?oga=true"; apply != want {
		t.Errorf("applyURL = %q, want %q", apply, want)
	}
	if want := "https://jobs.example.com/ExampleRobotics/700000000000001-go-platform-engineer"; posting != want {
		t.Errorf("postingURL = %q, want %q", posting, want)
	}
}

func TestParseSmartRecruitersDetailPartial(t *testing.T) {
	for _, tc := range []struct{ name, body, desc, apply, posting string }{
		{
			"sections out of order, some missing, one without title",
			`{"applyUrl":"https://jobs.example.com/a","jobAd":{"sections":{"qualifications":{"title":"Q","text":"<p>q</p>"},"jobDescription":{"text":"<p>j</p>"},"companyDescription":{"title":"C","text":"  "}}}}`,
			"<p>j</p>\n<h3>Q</h3>\n<p>q</p>", "https://jobs.example.com/a", "",
		},
		{
			"title escaped",
			`{"jobAd":{"sections":{"jobDescription":{"title":"R&D <team>","text":"<p>x</p>"}}}}`,
			"<h3>R&amp;D &lt;team&gt;</h3>\n<p>x</p>", "", "",
		},
		{"no sections", `{"postingUrl":"https://jobs.example.com/p","jobAd":{}}`, "", "", "https://jobs.example.com/p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, apply, posting, err := parseSmartRecruitersDetail([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if desc != tc.desc || apply != tc.apply || posting != tc.posting {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", desc, apply, posting, tc.desc, tc.apply, tc.posting)
			}
		})
	}
}

func TestParseSmartRecruitersDetailBroken(t *testing.T) {
	for name, body := range map[string]string{
		"garbage":          `{{{`,
		"empty":            ``,
		"top-level list":   `[]`,
		"no jobAd":         `{"id":"1","applyUrl":"https://jobs.example.com/a"}`,
		"jobAd string":     `{"jobAd":"none"}`,
		"sections list":    `{"jobAd":{"sections":[]}}`,
		"section text int": `{"jobAd":{"sections":{"jobDescription":{"text":5}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseSmartRecruitersDetail([]byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
}
