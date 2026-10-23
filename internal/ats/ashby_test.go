package ats

import (
	"errors"
	"testing"
)

func TestParseAshby(t *testing.T) {
	b := Board{ATS: Ashby, Board: "demolabs", Name: "Demo Labs"}
	const host = "https://jobs.example.com/demolabs/"
	want := []Posting{
		{
			ID:              "c3d4e5f6-0000-4000-8000-000000000001",
			Title:           "AI Engineer",
			Company:         "Demo Labs",
			Location:        "Remote, Exampleville", // secondary locations after; already says remote
			DescriptionHTML: `<p>Build agents in Go.</p>`,
			EmploymentType:  "FullTime",
			PostedAt:        ts(t, "2026-10-10T08:00:00Z"),
			ApplyURL:        host + "c3d4e5f6-0000-4000-8000-000000000001/application",
			URL:             host + "c3d4e5f6-0000-4000-8000-000000000001",
		},
		{
			ID:              "c3d4e5f6-0000-4000-8000-000000000002",
			Title:           "Inginer Backend",
			Company:         "Demo Labs",
			Location:        "Exampleville (On-site)",
			DescriptionHTML: `<p>Căutăm un inginer backend pentru echipa noastră.</p>`,
			EmploymentType:  "Contract",
			PostedAt:        ts(t, "2026-10-11T09:30:00Z"),
			ApplyURL:        host + "c3d4e5f6-0000-4000-8000-000000000002/application",
			URL:             host + "c3d4e5f6-0000-4000-8000-000000000002",
		},
		// c3d4e5f6-…-03 is isListed=false: dropped.
		{
			ID:              "c3d4e5f6-0000-4000-8000-000000000004", // no id field: from jobUrl
			Title:           "Data Engineer",
			Company:         "Demo Labs",
			Location:        "Exampleland", // isRemote alone (it is also true for Hybrid) adds nothing
			DescriptionHTML: `<p>Pipelines and dashboards.</p>`,
			EmploymentType:  "PartTime",
			PostedAt:        nil,
			ApplyURL:        host + "c3d4e5f6-0000-4000-8000-000000000004/application",
			URL:             host + "c3d4e5f6-0000-4000-8000-000000000004",
		},
		{
			ID:              "c3d4e5f6-0000-4000-8000-000000000005",
			Title:           "Product Engineer",
			Company:         "Demo Labs",
			Location:        "Exampleville (Hybrid)", // workplaceType wins over isRemote
			DescriptionHTML: `<p>Ship features end to end.</p>`,
			EmploymentType:  "FullTime",
			PostedAt:        ts(t, "2026-10-12T09:00:00Z"),
			ApplyURL:        host + "c3d4e5f6-0000-4000-8000-000000000005/application",
			URL:             host + "c3d4e5f6-0000-4000-8000-000000000005",
		},
	}
	got, err := Parse(b, readFixture(t, "ashby_demolabs.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertPostings(t, got, want)
}

func TestParseAshbyBroken(t *testing.T) {
	b := Board{ATS: Ashby, Board: "demolabs"}
	for name, body := range map[string]string{
		"garbage":        `{{{`,
		"empty":          ``,
		"no jobs key":    `{"apiVersion":"1"}`,
		"jobs string":    `{"jobs":"none"}`,
		"no id, no url":  `{"jobs":[{"title":"Go Engineer"}]}`,
		"no title":       `{"jobs":[{"id":"x1"}]}`,
		"top-level list": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(b, []byte(body)); !errors.Is(err, ErrParserBroke) {
				t.Fatalf("err = %v, want ErrParserBroke", err)
			}
		})
	}
	got, err := Parse(b, []byte(`{"apiVersion":"1","jobs":[{"id":"x1","title":"Hidden","isListed":false}]}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("only unlisted: got %v, %v; want no postings and no error", got, err)
	}
}

// TestAshbyIsRemoteAloneAddsNothing: isRemote is true for Hybrid as well as Remote, so
// only workplaceType labels the location.
func TestAshbyIsRemoteAloneAddsNothing(t *testing.T) {
	b := Board{ATS: Ashby, Board: "demolabs"}
	for _, tc := range []struct{ name, job, want string }{
		{"isRemote true, no workplaceType", `"isRemote":true`, "Exampleville"},
		{"isRemote false, no workplaceType", `"isRemote":false`, "Exampleville"},
		{"isRemote true, empty workplaceType", `"isRemote":true,"workplaceType":""`, "Exampleville"},
		{"workplaceType Remote decides", `"isRemote":false,"workplaceType":"Remote"`, "Exampleville (Remote)"},
		{"workplaceType Hybrid decides", `"isRemote":true,"workplaceType":"Hybrid"`, "Exampleville (Hybrid)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jobs":[{"id":"x1","title":"Go Engineer","location":"Exampleville",` + tc.job + `}]}`
			got, err := Parse(b, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Location != tc.want {
				t.Fatalf("got %+v, want Location %q", got, tc.want)
			}
		})
	}
}
