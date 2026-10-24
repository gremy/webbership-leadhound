package ats

import (
	"bytes"
	"encoding/xml"
	"html"
	"net/url"
	"strings"
	"time"
)

// personioLanguage is the language the feed is asked in first: without ?language= the
// feed was seen to list positions with empty descriptions. A feed that comes back with no
// position described is asked once more without it (personioEmptyWithLanguage).
const personioLanguage = "en"

// personioTLD maps a board's region to its Personio host's top-level domain: no region →
// "de" (jobs.personio.de), RegionCom → "com" (jobs.personio.com).
func personioTLD(region string) string {
	if region == RegionCom {
		return "com"
	}
	return "de"
}

// personioHost is "<slug>.jobs.personio.<tld>".
func personioHost(b Board) string {
	return b.Board + ".jobs.personio." + personioTLD(b.Region)
}

// personioFeedURL is the public XML feed listing every open position; language ("en")
// is added as ?language=, and left out when empty. Without it the feed was seen to list
// the positions with empty jobDescriptions.
func personioFeedURL(b Board, language string) string {
	u := "https://" + personioHost(b) + "/xml"
	if language != "" {
		u += "?language=" + url.QueryEscape(language)
	}
	return u
}

// personioFeed is GET https://<slug>.jobs.personio.<tld>/xml. The root element must be
// <workzag-jobs>; encoding/xml refuses any other root.
type personioFeed struct {
	XMLName   xml.Name           `xml:"workzag-jobs"`
	Positions []personioPosition `xml:"position"`
}

type personioPosition struct {
	ID                string   `xml:"id"`
	Subcompany        string   `xml:"subcompany"`
	Office            string   `xml:"office"`
	AdditionalOffices []string `xml:"additionalOffices>office"`
	Name              string   `xml:"name"`
	JobDescriptions   []struct {
		Name  string `xml:"name"`
		Value string `xml:"value"` // HTML, usually in CDATA
	} `xml:"jobDescriptions>jobDescription"`
	EmploymentType string `xml:"employmentType"` // permanent, intern, trainee, freelance, ...
	Schedule       string `xml:"schedule"`       // full-time, part-time, ...
	CreatedAt      string `xml:"createdAt"`      // 2016-05-31T12:14:07+0200 or RFC 3339
}

// decodePersonio reads a feed; any XML error or a root other than <workzag-jobs> is
// ErrParserBroke.
func decodePersonio(body []byte) (personioFeed, error) {
	var feed personioFeed
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&feed); err != nil {
		return feed, broke("bad XML: %v", err)
	}
	return feed, nil
}

// parsePersonio maps a Personio XML feed. Company is the position's subcompany when set
// (a Personio account can hold several legal entities), else Board.Name, else the slug.
// Personio states no workplace, so Location is the offices as listed, with no label added;
// the gates read the text. A feed with no positions is a valid empty board.
func parsePersonio(b Board, body []byte) ([]Posting, error) {
	feed, err := decodePersonio(body)
	if err != nil {
		return nil, err
	}
	out := make([]Posting, 0, len(feed.Positions))
	for i, pos := range feed.Positions {
		p := Posting{
			ID:              strings.TrimSpace(pos.ID),
			Title:           strings.TrimSpace(pos.Name),
			Company:         strings.TrimSpace(pos.Subcompany),
			Location:        personioLocation(pos),
			DescriptionHTML: personioDescription(pos),
			EmploymentType:  joinNonEmpty(pos.EmploymentType, pos.Schedule),
			PostedAt:        personioTime(pos.CreatedAt),
		}
		if p.ID == "" || p.Title == "" {
			return nil, broke("position %d has no id or no name", i)
		}
		if p.Company == "" {
			p.Company = b.company()
		}
		p.URL = "https://" + personioHost(b) + "/job/" + url.PathEscape(p.ID)
		p.ApplyURL = p.URL
		out = append(out, p)
	}
	return out, nil
}

// personioLocation is the main office then the additional ones, each listed once.
func personioLocation(pos personioPosition) string {
	seen := map[string]bool{}
	var offices []string
	for _, o := range append([]string{pos.Office}, pos.AdditionalOffices...) {
		o = strings.TrimSpace(o)
		if o == "" || seen[strings.ToLower(o)] {
			continue
		}
		seen[strings.ToLower(o)] = true
		offices = append(offices, o)
	}
	return joinNonEmpty(offices...)
}

// personioDescription joins the description blocks in order, each as "<h3>name</h3>"
// followed by its HTML value. A block with an empty value is dropped; a block with no
// name gets no heading.
func personioDescription(pos personioPosition) string {
	var sb strings.Builder
	for _, d := range pos.JobDescriptions {
		value := strings.TrimSpace(d.Value)
		if value == "" {
			continue
		}
		if name := strings.TrimSpace(d.Name); name != "" {
			sb.WriteString("<h3>" + html.EscapeString(name) + "</h3>")
		}
		sb.WriteString(value)
	}
	return sb.String()
}

// personioTime reads createdAt: RFC 3339, or the documented form with a colonless offset
// ("2016-05-31T12:14:07+0200"). nil when empty or not understood; UTC otherwise.
func personioTime(s string) *time.Time {
	if t := parseTime(s); t != nil {
		return t
	}
	t, err := time.Parse("2006-01-02T15:04:05-0700", strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// personioEmptyWithLanguage says whether a feed fetched with ?language= should be fetched
// again without it: true when the body is a well-formed feed that lists no positions, or
// whose positions all have empty descriptions (a board not published in that language).
// A body of the wrong shape gives false, so the parser reports it. Observed: without
// ?language= the feed lists positions with empty descriptions, so a retry may still give
// none; what ?language=en returns on a board not published in English is unverified.
func personioEmptyWithLanguage(body []byte) bool {
	feed, err := decodePersonio(body)
	if err != nil {
		return false
	}
	for _, pos := range feed.Positions {
		if personioDescription(pos) != "" {
			return false
		}
	}
	return true
}
