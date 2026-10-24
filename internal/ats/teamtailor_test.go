package ats

import (
	"errors"
	"testing"
)

func TestParseTeamtailor(t *testing.T) {
	b := Board{ATS: Teamtailor, Board: "mockworks", Name: "Mockworks"}
	const host = "https://mockworks.example.com/jobs/"
	want := []Posting{
		{
			ID:              "a1b2c3d4-0000-4000-8000-000000000001",
			Title:           "Senior Go Engineer",
			Company:         "Mockworks",
			Location:        "Exampleville, Exampleland (On-site)", // remoteStatus none
			DescriptionHTML: `<h4>About the role</h4><p>Build services in Go &amp; PostgreSQL.</p>`,
			PostedAt:        ts(t, "2026-10-09T07:05:16Z"),
			ApplyURL:        host + "1000001-senior-go-engineer",
			URL:             host + "1000001-senior-go-engineer",
		},
		{
			ID:              "a1b2c3d4-0000-4000-8000-000000000002",
			Title:           "AI Engineer",
			Company:         "Mockworks",
			Location:        "Exampleville, Exampleland; Sampletown, Demoland (Hybrid)",
			DescriptionHTML: `<p>Build agents in Go.</p>`, // CDATA
			PostedAt:        ts(t, "2026-10-12T14:33:33Z"),
			ApplyURL:        host + "1000002-ai-engineer",
			URL:             host + "1000002-ai-engineer",
		},
		{
			ID:              "a1b2c3d4-0000-4000-8000-000000000003",
			Title:           "Platform Engineer",
			Company:         "Mockworks",
			Location:        "Remote", // fully, no location
			DescriptionHTML: `<p>Run the platform from anywhere.</p>`,
			PostedAt:        ts(t, "2026-10-13T07:00:00Z"),
			ApplyURL:        host + "1000003-platform-engineer",
			URL:             host + "1000003-platform-engineer",
		},
		{
			ID:              "a1b2c3d4-0000-4000-8000-000000000004",
			Title:           "Inginer Backend",
			Company:         "Mockworks",
			Location:        "Demo Remote Hub", // no city/country: the name; no remoteStatus: no label
			DescriptionHTML: `<p>Căutăm un inginer backend pentru echipa noastră.</p>`,
			PostedAt:        nil, // unreadable pubDate
			ApplyURL:        host + "1000004-inginer-backend",
			URL:             host + "1000004-inginer-backend",
		},
		{
			ID:              "1000005", // empty guid: the number in the link
			Title:           "Data Engineer",
			Company:         "Mockworks",
			Location:        "Sampletown, Demoland", // temporary: no label
			DescriptionHTML: `<p>Pipelines and dashboards.</p>`,
			PostedAt:        ts(t, "2026-10-14T08:15:00Z"),
			ApplyURL:        host + "1000005-data-engineer",
			URL:             host + "1000005-data-engineer",
		},
	}
	got, err := parseTeamtailor(b, readFixture(t, "teamtailor_mockworks.rss"))
	if err != nil {
		t.Fatal(err)
	}
	assertPostings(t, got, want)
}

func TestParseTeamtailorCompanyFallsBackToSlug(t *testing.T) {
	b := Board{ATS: Teamtailor, Board: "mockworks"}
	got, err := parseTeamtailor(b, readFixture(t, "teamtailor_mockworks.rss"))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Company != "mockworks" {
		t.Fatalf("Company = %q, want the slug", got[0].Company)
	}
}

func TestTeamtailorWorkplace(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"none", workOnsite},
		{"hybrid", workHybrid},
		{"fully", workRemote},
		{"remote", workRemote},
		{" Fully ", workRemote},
		{"temporary", ""},
		{"", ""},
		{"something-new", ""},
	} {
		if got := teamtailorWorkplace(tc.status); got != tc.want {
			t.Errorf("teamtailorWorkplace(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestTeamtailorPubDate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // RFC 3339 UTC, "" for nil
	}{
		{"Fri, 09 Oct 2026 09:05:16 +0200", "2026-10-09T07:05:16Z"},
		{"Fri, 9 Oct 2026 09:05:16 +0200", "2026-10-09T07:05:16Z"},
		{"Fri, 09 Oct 2026 09:05:16 GMT", "2026-10-09T09:05:16Z"},
		{"2026-10-09T09:05:16+02:00", "2026-10-09T07:05:16Z"},
		{"", ""},
		{"yesterday", ""},
	} {
		got := teamtailorTime(tc.in)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("teamtailorTime(%q) = %v, want nil", tc.in, got)
		case tc.want != "" && (got == nil || !got.Equal(*ts(t, tc.want)) || got.Location().String() != "UTC"):
			t.Errorf("teamtailorTime(%q) = %v, want %s", tc.in, got, tc.want)
		}
	}
}

func TestParseTeamtailorBroken(t *testing.T) {
	b := Board{ATS: Teamtailor, Board: "mockworks"}
	for name, body := range map[string]string{
		"garbage":    `<<<`,
		"empty":      ``,
		"json":       `{"jobs":[]}`,
		"html page":  `<html><body>Not found</body></html>`,
		"atom feed":  `<feed xmlns="http://www.w3.org/2005/Atom"><title>x</title></feed>`,
		"no channel": `<rss version="2.0"></rss>`,
		"no title": `<rss version="2.0"><channel><item><guid>g1</guid>` +
			`<link>https://mockworks.example.com/jobs/1-x</link></item></channel></rss>`,
		"no id": `<rss version="2.0"><channel><item><title>Go Engineer</title>` +
			`<link>https://mockworks.example.com/jobs</link></item></channel></rss>`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTeamtailor(b, []byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
}

func TestParseTeamtailorEmptyChannel(t *testing.T) {
	b := Board{ATS: Teamtailor, Board: "mockworks"}
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<rss version="2.0" xmlns:tt="https://teamtailor.com/locations"><channel>` +
		`<title>Mockworks</title><description>Current job openings</description>` +
		`<link>https://mockworks.example.com/jobs</link></channel></rss>`
	got, err := parseTeamtailor(b, []byte(body))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want an empty non-nil list and no error", got, err)
	}
}

func TestTeamtailorIDFromLink(t *testing.T) {
	for in, want := range map[string]string{
		"https://mockworks.example.com/jobs/1000005-data-engineer":  "1000005",
		"https://mockworks.example.com/jobs/1000005":                "1000005",
		"https://mockworks.example.com/jobs/1000005-data-engineer/": "1000005",
		"https://mockworks.example.com/jobs":                        "",
		"https://mockworks.example.com/jobs/data-engineer":          "",
		"": "",
	} {
		if got := teamtailorIDFromLink(in); got != want {
			t.Errorf("teamtailorIDFromLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTeamtailorPageURL(t *testing.T) {
	b := Board{ATS: Teamtailor, Board: "mockworks"}
	for _, tc := range []struct {
		offset int
		want   string
	}{
		{0, "https://mockworks.teamtailor.com/jobs.rss?offset=0&per_page=100"},
		{100, "https://mockworks.teamtailor.com/jobs.rss?offset=100&per_page=100"},
		{-5, "https://mockworks.teamtailor.com/jobs.rss?offset=0&per_page=100"},
	} {
		if got := teamtailorPageURL(b, tc.offset); got != tc.want {
			t.Errorf("teamtailorPageURL(%d) = %q, want %q", tc.offset, got, tc.want)
		}
	}
	if teamtailorLastPage(make([]Posting, teamtailorPageSize)) {
		t.Error("a full page is not the last")
	}
	if !teamtailorLastPage(make([]Posting, teamtailorPageSize-1)) {
		t.Error("a short page is the last")
	}
}
