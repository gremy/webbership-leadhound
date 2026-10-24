package ats

import (
	"errors"
	"testing"
)

// personioBoard is a board line before 6d.2 adds the Personio constant to ats.go.
func personioBoard(slug, name, region string) Board {
	return Board{ATS: Personio, Board: slug, Name: name, Region: region}
}

func TestParsePersonio(t *testing.T) {
	b := personioBoard("musterfabrik", "Musterfabrik", "")
	const host = "https://musterfabrik.jobs.personio.de/job/"
	want := []Posting{
		{
			ID:      "4100001",
			Title:   "Senior Go Engineer",
			Company: "Musterfabrik", // no subcompany: Board.Name
			// additional offices follow the main one; no workplace label is added
			Location: "Exampleville, Sampletown, Demoburg",
			DescriptionHTML: `<h3>Your role at Musterfabrik</h3><p>Build services in Go.</p>` +
				`<h3>What you&#39;ll bring</h3><ul><li>Go in production</li><li>SQL</li></ul>`,
			EmploymentType: "permanent, full-time",
			PostedAt:       ts(t, "2026-10-10T08:00:00Z"), // +0200 form
			ApplyURL:       host + "4100001",
			URL:            host + "4100001",
		},
		{
			ID:              "4100002",
			Title:           "AI Engineer",
			Company:         "Musterfabrik",
			Location:        "Remote (Exampleland)", // the office text itself says remote
			DescriptionHTML: `<h3>About the job</h3><p>Ship agents to customers.</p>`,
			EmploymentType:  "permanent, part-time",
			PostedAt:        ts(t, "2026-10-11T09:30:00Z"), // RFC 3339 form
			ApplyURL:        host + "4100002",
			URL:             host + "4100002",
		},
		{
			ID:              "4100003",
			Title:           "Data Engineer",
			Company:         "Musterfabrik",
			Location:        "Sampletown",
			DescriptionHTML: "", // empty jobDescriptions
			EmploymentType:  "freelance",
			PostedAt:        nil,
			ApplyURL:        host + "4100003",
			URL:             host + "4100003",
		},
		{
			ID:              "4100004",
			Title:           "Platform Engineer",
			Company:         "Musterfabrik Labs GmbH",   // subcompany wins over Board.Name
			Location:        "Demoburg",                 // a repeated office is listed once
			DescriptionHTML: `<p>Run the platform.</p>`, // no heading without a name; an empty block is dropped
			EmploymentType:  "permanent, full-time",
			PostedAt:        ts(t, "2026-10-12T07:00:00Z"),
			ApplyURL:        host + "4100004",
			URL:             host + "4100004",
		},
	}
	got, err := parsePersonio(b, readFixture(t, "personio_musterfabrik.xml"))
	if err != nil {
		t.Fatal(err)
	}
	assertPostings(t, got, want)
}

func TestParsePersonioCompanyAndURL(t *testing.T) {
	body := []byte(`<workzag-jobs><position><id>7</id><name>Go Engineer</name></position></workzag-jobs>`)
	for _, tc := range []struct {
		name, slug, boardName, region string
		wantCompany, wantURL          string
	}{
		{"name set, de", "musterfabrik", "Musterfabrik", "", "Musterfabrik", "https://musterfabrik.jobs.personio.de/job/7"},
		{"no name: slug", "musterfabrik", "", "", "musterfabrik", "https://musterfabrik.jobs.personio.de/job/7"},
		{"com region", "samplesoft", "SampleSoft", RegionCom, "SampleSoft", "https://samplesoft.jobs.personio.com/job/7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePersonio(personioBoard(tc.slug, tc.boardName, tc.region), body)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d postings, want 1", len(got))
			}
			if got[0].Company != tc.wantCompany {
				t.Errorf("Company = %q, want %q", got[0].Company, tc.wantCompany)
			}
			if got[0].URL != tc.wantURL || got[0].ApplyURL != tc.wantURL {
				t.Errorf("URL = %q, ApplyURL = %q, want %q", got[0].URL, got[0].ApplyURL, tc.wantURL)
			}
		})
	}
}

func TestParsePersonioEmptyBoard(t *testing.T) {
	b := personioBoard("leerwerk", "Leerwerk", "")
	got, err := parsePersonio(b, readFixture(t, "personio_leerwerk.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty, non-nil list", got)
	}
}

func TestParsePersonioBroken(t *testing.T) {
	b := personioBoard("musterfabrik", "", "")
	for name, body := range map[string]string{
		"empty":        ``,
		"garbage":      `<<<`,
		"json":         `{"jobs":[]}`,
		"wrong root":   `<jobs><position><id>1</id><name>X</name></position></jobs>`,
		"html page":    `<!DOCTYPE html><html><body>Not found</body></html>`,
		"no id":        `<workzag-jobs><position><name>Go Engineer</name></position></workzag-jobs>`,
		"no title":     `<workzag-jobs><position><id>1</id></position></workzag-jobs>`,
		"unclosed tag": `<workzag-jobs><position><id>1</id>`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePersonio(b, []byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
}

func TestPersonioTime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // "" for nil
	}{
		{"2016-05-31T12:14:07+0200", "2016-05-31T10:14:07Z"},
		{"2024-11-13T14:10:41+00:00", "2024-11-13T14:10:41Z"},
		{"2026-01-02T03:04:05Z", "2026-01-02T03:04:05Z"},
		{"  2026-01-02T03:04:05-0500 ", "2026-01-02T08:04:05Z"},
		{"", ""},
		{"yesterday", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got := personioTime(tc.in)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("got %v, want nil", got)
			case tc.want == "":
			case got == nil:
				t.Fatalf("got nil, want %s", tc.want)
			case !got.Equal(*ts(t, tc.want)) || got.Location().String() != "UTC":
				t.Fatalf("got %v, want %s in UTC", got, tc.want)
			}
		})
	}
}

func TestPersonioHostAndFeedURL(t *testing.T) {
	for _, tc := range []struct {
		region, lang, wantHost, wantFeed string
	}{
		{"", "en", "musterfabrik.jobs.personio.de", "https://musterfabrik.jobs.personio.de/xml?language=en"},
		{"", "", "musterfabrik.jobs.personio.de", "https://musterfabrik.jobs.personio.de/xml"},
		{RegionCom, "en", "musterfabrik.jobs.personio.com", "https://musterfabrik.jobs.personio.com/xml?language=en"},
	} {
		t.Run(tc.region+"/"+tc.lang, func(t *testing.T) {
			b := personioBoard("musterfabrik", "", tc.region)
			if got := personioHost(b); got != tc.wantHost {
				t.Errorf("personioHost = %q, want %q", got, tc.wantHost)
			}
			if got := personioFeedURL(b, tc.lang); got != tc.wantFeed {
				t.Errorf("personioFeedURL = %q, want %q", got, tc.wantFeed)
			}
		})
	}
}

func TestPersonioEmptyWithLanguage(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"fixture with descriptions", "", false}, // filled from the fixture below
		{"no positions", `<workzag-jobs></workzag-jobs>`, true},
		{"every description empty", `<workzag-jobs>
			<position><id>1</id><name>A</name><jobDescriptions></jobDescriptions></position>
			<position><id>2</id><name>B</name><jobDescriptions><jobDescription><name>X</name><value></value></jobDescription></jobDescriptions></position>
		</workzag-jobs>`, true},
		{"one description filled", `<workzag-jobs>
			<position><id>1</id><name>A</name><jobDescriptions></jobDescriptions></position>
			<position><id>2</id><name>B</name><jobDescriptions><jobDescription><name>X</name><value><![CDATA[<p>y</p>]]></value></jobDescription></jobDescriptions></position>
		</workzag-jobs>`, false},
		{"broken body: let the parser say so", `<jobs/>`, false},
		{"garbage", `<<<`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if tc.body == "" {
				body = readFixture(t, "personio_musterfabrik.xml")
			}
			if got := personioEmptyWithLanguage(body); got != tc.want {
				t.Fatalf("personioEmptyWithLanguage = %v, want %v", got, tc.want)
			}
		})
	}
}
