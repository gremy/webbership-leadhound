package linkedin

import (
	"bytes"
	"fmt"
	"regexp"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Card is one job card from a search page.
type Card struct {
	ID          string    // the LinkedIn job ID
	Title       string    // entities decoded, whitespace collapsed
	Company     string    // company display name
	CompanyURL  string    // company link without query, "" when the card has none
	CompanySlug string    // <slug> from linkedin.com/company/<slug>, "" when absent
	Location    string    // location text as shown
	ListDate    time.Time // the card's datetime attribute (a date, at UTC midnight); zero when absent
	Benefits    []string  // benefit badges, e.g. "Be an early applicant"
	URL         string    // canonical job view URL (www.linkedin.com, no query)
}

var jobURNRE = regexp.MustCompile(`urn:li:jobPosting:(\d+)`)

// ParseSearch parses a guest search page. A page with no job URNs gives zero cards and no
// error. A page with cards where any card lacks an ID, title or company gives
// ErrParserBroke and no cards, so nothing from that page is written. So does a page whose
// job URNs, counted apart from the card selector, are not exactly the cards parsed: the
// card markup drifted, and a drifted page must never read as an empty one.
func ParseSearch(body []byte) ([]Card, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("linkedin: parse search: %w", err)
	}
	nodes := doc.Find(sel.card)
	cards := make([]Card, 0, nodes.Length())
	var broke error
	nodes.EachWithBreak(func(i int, s *goquery.Selection) bool {
		c := parseCard(s)
		switch {
		case c.ID == "":
			broke = fmt.Errorf("linkedin: parse search: card %d has no job ID: %w", i, ErrParserBroke)
		case c.Title == "":
			broke = fmt.Errorf("linkedin: parse search: card %s has no title: %w", c.ID, ErrParserBroke)
		case c.Company == "":
			broke = fmt.Errorf("linkedin: parse search: card %s has no company: %w", c.ID, ErrParserBroke)
		default:
			cards = append(cards, c)
			return true
		}
		return false
	})
	if broke != nil {
		return nil, broke
	}
	if err := checkURNs(doc, cards); err != nil {
		return nil, err
	}
	return cards, nil
}

// checkURNs compares the distinct job IDs in the page's job URNs with the cards parsed.
func checkURNs(doc *goquery.Document, cards []Card) error {
	onPage := map[string]bool{}
	doc.Find(sel.jobURN).Each(func(_ int, s *goquery.Selection) {
		urn, _ := s.Attr("data-entity-urn")
		if m := jobURNRE.FindStringSubmatch(urn); m != nil {
			onPage[m[1]] = true
		}
	})
	parsed := map[string]bool{}
	for _, c := range cards {
		parsed[c.ID] = true
	}
	missing := 0
	for id := range onPage {
		if !parsed[id] {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("linkedin: parse search: %d job URNs on the page, %d read as cards (%d missed): %w",
			len(onPage), len(parsed), missing, ErrParserBroke)
	}
	return nil
}

func parseCard(s *goquery.Selection) Card {
	var c Card
	if urn, ok := s.Attr("data-entity-urn"); ok {
		if m := jobURNRE.FindStringSubmatch(urn); m != nil {
			c.ID = m[1]
		}
	}
	c.Title = squash(s.Find(sel.cardTitle).First().Text())

	if a := s.Find(sel.cardCompany).First(); a.Length() > 0 {
		c.Company = squash(a.Text())
		href, _ := a.Attr("href")
		c.CompanyURL = companyURL(href)
		c.CompanySlug = CompanySlug(href)
	} else {
		c.Company = squash(s.Find(sel.cardSubtitle).First().Text())
	}

	c.Location = squash(s.Find(sel.cardLocation).First().Text())

	if dt, ok := s.Find(sel.cardListDate).First().Attr("datetime"); ok {
		if d, err := time.Parse(time.DateOnly, dt); err == nil {
			c.ListDate = d
		}
	}

	c.Benefits = []string{}
	s.Find(sel.cardBenefits).Each(func(_ int, b *goquery.Selection) {
		if t := squash(b.Text()); t != "" {
			c.Benefits = append(c.Benefits, t)
		}
	})

	if href, ok := s.Find(sel.cardLink).First().Attr("href"); ok && href != "" {
		c.URL = CanonicalJobURL(href)
	} else if c.ID != "" {
		c.URL = "https://www.linkedin.com/jobs/view/" + c.ID
	}
	return c
}
