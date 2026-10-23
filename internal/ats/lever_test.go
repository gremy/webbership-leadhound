package ats

import (
	"errors"
	"testing"
)

func TestParseLever(t *testing.T) {
	b := Board{ATS: Lever, Board: "samplesoft", Name: "SampleSoft"}
	const host = "https://jobs.example.com/samplesoft/"
	want := []Posting{
		{
			ID:              "a1b2c3d4-0000-4000-8000-000000000001",
			Title:           "Backend Engineer, Go",
			Company:         "SampleSoft",
			Location:        "Berlin, Germany (Remote)",
			DescriptionHTML: `<div>We write services in Go.</div><h3>What you&#39;ll do</h3><ul><li>Build APIs</li><li>Own services</li></ul><h3>Tools &amp; stack</h3><ul><li>Go</li><li>PostgreSQL</li></ul><div>Benefits: a learning budget.</div>`,
			EmploymentType:  "Full-time",
			PostedAt:        ts(t, "2026-09-21T14:13:20Z"),
			ApplyURL:        host + "a1b2c3d4-0000-4000-8000-000000000001/apply",
			URL:             host + "a1b2c3d4-0000-4000-8000-000000000001",
		},
		{
			ID:              "a1b2c3d4-0000-4000-8000-000000000002",
			Title:           "Ingeniero de Plataforma",
			Company:         "SampleSoft",
			Location:        "Madrid (Hybrid), Valencia", // allLocations; already says hybrid
			DescriptionHTML: `<p>Escribimos software &lt;fiable&gt; &amp; más.</p>`,
			EmploymentType:  "Full-time",
			PostedAt:        nil, // no createdAt
			ApplyURL:        host + "a1b2c3d4-0000-4000-8000-000000000002/apply",
			URL:             host + "a1b2c3d4-0000-4000-8000-000000000002",
		},
		{
			ID:              "a1b2c3d4-0000-4000-8000-000000000003",
			Title:           "Site Reliability Engineer",
			Company:         "SampleSoft",
			Location:        "Exampleville (On-site)",
			DescriptionHTML: `<div>Keep the lights on.</div><h3>Requirements</h3><ul><li>Linux</li></ul>`,
			EmploymentType:  "Contract",
			PostedAt:        ts(t, "2026-10-03T04:00:00Z"),
			ApplyURL:        host + "a1b2c3d4-0000-4000-8000-000000000003/apply",
			URL:             host + "a1b2c3d4-0000-4000-8000-000000000003",
		},
	}
	got, err := Parse(b, readFixture(t, "lever_samplesoft.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertPostings(t, got, want)
}

func TestParseLeverEUUnspecifiedWorkplace(t *testing.T) {
	b := Board{ATS: Lever, Board: "beispielwerk", Region: RegionEU}
	got, err := Parse(b, readFixture(t, "lever_beispielwerk.json"))
	if err != nil {
		t.Fatal(err)
	}
	const host = "https://jobs.example.com/beispielwerk/"
	assertPostings(t, got, []Posting{{
		ID:              "b2c3d4e5-0000-4000-8000-000000000001",
		Title:           "Softwareentwickler Go",
		Company:         "beispielwerk", // no Name: the slug
		Location:        "Musterstadt",  // unspecified adds nothing
		DescriptionHTML: `<div>Wir entwickeln Software für den Mittelstand.</div>`,
		EmploymentType:  "Vollzeit",
		PostedAt:        ts(t, "2026-10-14T17:46:40Z"),
		ApplyURL:        host + "b2c3d4e5-0000-4000-8000-000000000001/apply",
		URL:             host + "b2c3d4e5-0000-4000-8000-000000000001",
	}})
}

func TestParseLeverBroken(t *testing.T) {
	b := Board{ATS: Lever, Board: "samplesoft"}
	for name, body := range map[string]string{
		"garbage":   `not json`,
		"empty":     ``,
		"null":      `null`,
		"error doc": `{"ok":false,"error":"Document not found"}`,
		"no id":     `[{"text":"Go Engineer"}]`,
		"no title":  `[{"id":"x1"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(b, []byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
	got, err := Parse(b, []byte(`[]`))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty board: got %v, %v; want no postings and no error", got, err)
	}
}
