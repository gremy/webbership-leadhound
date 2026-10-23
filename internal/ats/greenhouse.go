package ats

import (
	"encoding/json"
	"html"
	"strings"
)

// greenhouseBoard is GET /v1/boards/<slug>/jobs?content=true.
type greenhouseBoard struct {
	Jobs *[]greenhouseJob `json:"jobs"`
}

type greenhouseJob struct {
	ID       json.RawMessage `json:"id"`
	Title    string          `json:"title"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
	Content        string `json:"content"` // HTML, entity-escaped once more by the API
	AbsoluteURL    string `json:"absolute_url"`
	FirstPublished string `json:"first_published"`
	UpdatedAt      string `json:"updated_at"`
	CompanyName    string `json:"company_name"`
}

// parseGreenhouse maps a Greenhouse board. Greenhouse states no workplace, so the gates
// decide remote from the text.
func parseGreenhouse(b Board, body []byte) ([]Posting, error) {
	var resp greenhouseBoard
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, broke("bad JSON: %v", err)
	}
	if resp.Jobs == nil {
		return nil, broke("no jobs array")
	}
	out := make([]Posting, 0, len(*resp.Jobs))
	for i, j := range *resp.Jobs {
		p := Posting{
			ID:              idText(j.ID),
			Title:           strings.TrimSpace(j.Title),
			Company:         strings.TrimSpace(j.CompanyName),
			Location:        strings.TrimSpace(j.Location.Name),
			DescriptionHTML: strings.TrimSpace(html.UnescapeString(j.Content)),
			ApplyURL:        strings.TrimSpace(j.AbsoluteURL),
			URL:             strings.TrimSpace(j.AbsoluteURL),
		}
		if p.ID == "" || p.Title == "" {
			return nil, broke("job %d has no id or no title", i)
		}
		if p.Company == "" {
			p.Company = b.company()
		}
		p.PostedAt = parseTime(j.FirstPublished)
		if p.PostedAt == nil && strings.TrimSpace(j.FirstPublished) == "" {
			p.PostedAt = parseTime(j.UpdatedAt)
		}
		out = append(out, p)
	}
	return out, nil
}
