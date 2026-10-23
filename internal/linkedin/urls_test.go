package linkedin

import "testing"

func TestCanonicalJobURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fr.linkedin.com/jobs/view/x-123?position=1&refId=abc%3D%3D&trackingId=def", "https://www.linkedin.com/jobs/view/x-123"},
		{"https://fr.linkedin.com/jobs/view/x-123?position=1&refId=abc", "https://www.linkedin.com/jobs/view/x-123"},
		{"https://ro.linkedin.com/jobs/view/senior-go-engineer-1000000001", "https://www.linkedin.com/jobs/view/senior-go-engineer-1000000001"},
		{"https://www.linkedin.com/jobs/view/x-123", "https://www.linkedin.com/jobs/view/x-123"},
		{"https://linkedin.com/jobs/view/x-123#frag", "https://www.linkedin.com/jobs/view/x-123"},
		{"http://WWW.LinkedIn.com/jobs/view/x-123?trk=y", "https://www.linkedin.com/jobs/view/x-123"},
		{"https://jobs.example/view/1?x=1", "https://jobs.example/view/1"},
	}
	for _, tc := range cases {
		if got := CanonicalJobURL(tc.in); got != tc.want {
			t.Errorf("CanonicalJobURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSearchURL(t *testing.T) {
	cases := []struct {
		kw, loc   string
		tpr, page int
		want      string
	}{
		{"golang", "European Union", 86400, 1,
			"https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search?keywords=golang&location=European+Union&f_WT=2&f_TPR=r86400&start=0"},
		{"go developer", "Romania", 604800, 3,
			"https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search?keywords=go+developer&location=Romania&f_WT=2&f_TPR=r604800&start=20"},
		{"C++ & Go/AI", "Zürich", 86400, 0,
			"https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search?keywords=C%2B%2B+%26+Go%2FAI&location=Z%C3%BCrich&f_WT=2&f_TPR=r86400&start=0"},
	}
	for _, tc := range cases {
		if got := SearchURL(tc.kw, tc.loc, tc.tpr, tc.page); got != tc.want {
			t.Errorf("SearchURL(%q,%q,%d,%d)\n got %s\nwant %s", tc.kw, tc.loc, tc.tpr, tc.page, got, tc.want)
		}
	}
}

func TestDetailURL(t *testing.T) {
	if got, want := DetailURL("1000000001"), "https://www.linkedin.com/jobs-guest/jobs/api/jobPosting/1000000001"; got != want {
		t.Errorf("DetailURL = %q, want %q", got, want)
	}
}

func TestCompanySlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://www.linkedin.com/company/northwind-labs", "northwind-labs"},
		{"https://ro.linkedin.com/company/northwind-labs?trk=public_jobs", "northwind-labs"},
		{"https://www.linkedin.com/company/northwind-labs/life/", "northwind-labs"},
		{"https://www.linkedin.com/school/some-school", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := CompanySlug(tc.in); got != tc.want {
			t.Errorf("CompanySlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
