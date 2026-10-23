package linkedin

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Detail is one parsed job detail page. Its fields feed a job_observation row (LI5).
type Detail struct {
	Title          string
	Company        string
	CompanyURL     string // company link without query, "" when absent
	CompanySlug    string // <slug> from linkedin.com/company/<slug>, "" when absent
	Location       string
	PostedText     string     // as shown, e.g. "6 days ago"
	PostedAtEst    *time.Time // estimated from PostedText and the fetch time; nil when not understood
	ApplicantText  string     // as shown, e.g. "Over 200 applicants"
	ApplicantCount *int       // nil when unknown; 200 for "Over 200"; 25 for "Be among the first 25"
	Description    string     // raw inner HTML of the description markup; sanitised later
	Seniority      string
	EmploymentType string
	JobFunction    string
	Industries     string
	ApplyType      string // ApplyOffsite, ApplyOnsite, ApplySimple or ""
	Closed         bool   // the top card carries the closed banner
}

// ParseDetail parses a guest job detail page fetched at fetchedAt. A page without a
// title gives ErrParserBroke.
func ParseDetail(body []byte, fetchedAt time.Time) (Detail, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return Detail{}, fmt.Errorf("linkedin: parse detail: %w", err)
	}
	var d Detail
	d.Title = squash(doc.Find(sel.detailTitle).First().Text())
	if d.Title == "" {
		return Detail{}, fmt.Errorf("linkedin: parse detail: no title: %w", ErrParserBroke)
	}

	if a := doc.Find(sel.detailCompany).First(); a.Length() > 0 {
		d.Company = squash(a.Text())
		href, _ := a.Attr("href")
		d.CompanyURL = companyURL(href)
		d.CompanySlug = CompanySlug(href)
	}
	d.Location = squash(doc.Find(sel.detailLocation).Not(sel.notLocation).First().Text())

	d.PostedText = squash(doc.Find(sel.detailPosted).First().Text())
	d.PostedAtEst = ParsePosted(d.PostedText, fetchedAt)
	d.ApplicantText = squash(doc.Find(sel.detailApplied).First().Text())
	d.ApplicantCount = ParseApplicants(d.ApplicantText)

	for _, q := range sel.description {
		if m := doc.Find(q).First(); m.Length() > 0 {
			h, err := m.Html()
			if err != nil {
				return Detail{}, fmt.Errorf("linkedin: parse detail: description: %w", err)
			}
			d.Description = strings.TrimSpace(h)
			break
		}
	}

	doc.Find(sel.criteriaItem).Each(func(_ int, s *goquery.Selection) {
		label := strings.ToLower(squash(s.Find(sel.criteriaLabel).First().Text()))
		value := squash(s.Find(sel.criteriaValue).First().Text())
		switch label {
		case "seniority level":
			d.Seniority = value
		case "employment type":
			d.EmploymentType = value
		case "job function":
			d.JobFunction = value
		case "industries":
			d.Industries = value
		}
	})

	d.ApplyType = applyType(doc.Selection)
	d.Closed = closed(doc.Selection)
	return d, nil
}

// applyType reads the apply button. The onsite and simple markers are exact tracking
// names on the primary button, so they win over the offsite icon or prefix; a closed
// page keeps its apply markup and is reported by its apply type and Closed together.
func applyType(doc *goquery.Selection) string {
	switch {
	case doc.Find(sel.applyOnsite).Length() > 0:
		return ApplyOnsite
	case doc.Find(sel.applySimple).Length() > 0:
		return ApplySimple
	case doc.Find(sel.applyOffsite).Length() > 0:
		return ApplyOffsite
	}
	return ""
}

// closed looks for the closed banner inside the top card only: descriptions quote
// phrases like "no longer accepting applications", and a false closed hides a live job.
// No top card means no evidence, so the job is not closed.
func closed(doc *goquery.Selection) bool {
	top := doc.Find(sel.topCard).First()
	if top.Length() == 0 {
		return false
	}
	if top.Find(sel.closedBanner).Length() > 0 {
		return true
	}
	return strings.Contains(strings.ToLower(squash(top.Text())), closedText)
}
