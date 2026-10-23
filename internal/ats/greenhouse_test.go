package ats

import (
	"errors"
	"testing"
)

func TestParseGreenhouse(t *testing.T) {
	b := Board{ATS: Greenhouse, Board: "examplecorp", Name: "Example Corp"}
	want := []Posting{
		{
			ID:              "4000000001",
			Title:           "Senior Go Engineer",
			Company:         "Example Corp Labs", // company_name wins over Board.Name
			Location:        "Remote - Europe",
			DescriptionHTML: `<p>Tools &amp; services for &quot;boring&quot; teams.</p><ul><li>Go &amp; PostgreSQL</li><li>5+ years</li></ul>`,
			PostedAt:        ts(t, "2026-10-01T13:30:00Z"), // first_published, not updated_at
			ApplyURL:        "https://jobs.example.com/examplecorp/jobs/4000000001",
			URL:             "https://jobs.example.com/examplecorp/jobs/4000000001",
		},
		{
			ID:              "4000000002",
			Title:           "Platform Engineer",
			Company:         "Example Corp",
			Location:        "Exampleville, Exampleland", // Greenhouse adds no workplace
			DescriptionHTML: `<p>Hybrid role, three days a week in the Exampleville office.</p>`,
			PostedAt:        ts(t, "2026-10-05T10:00:00Z"), // no first_published: updated_at
			ApplyURL:        "https://jobs.example.com/examplecorp/jobs/4000000002",
			URL:             "https://jobs.example.com/examplecorp/jobs/4000000002",
		},
		{
			ID:              "4000000003",
			Title:           "Ingénieur Backend",
			Company:         "Example Corp",
			Location:        "Lyon, France",
			DescriptionHTML: `<p>Nous cherchons un ingénieur backend pour notre équipe à Lyon.</p>`,
			PostedAt:        nil, // neither date
			ApplyURL:        "https://jobs.example.com/examplecorp/jobs/4000000003",
			URL:             "https://jobs.example.com/examplecorp/jobs/4000000003",
		},
	}
	got, err := Parse(b, readFixture(t, "greenhouse_examplecorp.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertPostings(t, got, want)
}

func TestParseGreenhouseCompanyFallsBackToSlug(t *testing.T) {
	b := Board{ATS: Greenhouse, Board: "nameless"}
	got, err := Parse(b, []byte(`{"jobs":[{"id":7,"title":"Go Engineer"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Company != "nameless" || got[0].ID != "7" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseGreenhouseBroken(t *testing.T) {
	b := Board{ATS: Greenhouse, Board: "examplecorp"}
	for name, body := range map[string]string{
		"garbage":     `<html>not json</html>`,
		"empty":       ``,
		"no jobs key": `{"meta":{"total":0}}`,
		"jobs null":   `{"jobs":null}`,
		"jobs object": `{"jobs":{}}`,
		"array":       `[]`,
		"no id":       `{"jobs":[{"title":"Go Engineer"}]}`,
		"no title":    `{"jobs":[{"id":1,"title":"  "}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(b, []byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
	got, err := Parse(b, []byte(`{"jobs":[],"meta":{"total":0}}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty board: got %v, %v; want no postings and no error", got, err)
	}
}
