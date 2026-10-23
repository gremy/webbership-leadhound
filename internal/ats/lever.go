package ats

import (
	"encoding/json"
	"html"
	"strings"
	"time"
)

// leverPosting is one element of GET /v0/postings/<slug>?mode=json (a JSON array).
type leverPosting struct {
	ID         json.RawMessage `json:"id"`
	Text       string          `json:"text"`
	Categories struct {
		Location     string   `json:"location"`
		AllLocations []string `json:"allLocations"`
		Commitment   string   `json:"commitment"`
	} `json:"categories"`
	WorkplaceType    string `json:"workplaceType"` // unspecified, on-site, remote or hybrid
	CreatedAt        int64  `json:"createdAt"`     // ms since the epoch
	ApplyURL         string `json:"applyUrl"`
	HostedURL        string `json:"hostedUrl"`
	Description      string `json:"description"` // HTML
	DescriptionPlain string `json:"descriptionPlain"`
	Lists            []struct {
		Text    string `json:"text"`
		Content string `json:"content"` // <li> items, HTML
	} `json:"lists"`
	Additional string `json:"additional"` // HTML
}

// parseLever maps a Lever board.
func parseLever(b Board, body []byte) ([]Posting, error) {
	var resp *[]leverPosting
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, broke("bad JSON or not a postings array: %v", err)
	}
	if resp == nil {
		return nil, broke("no postings array")
	}
	out := make([]Posting, 0, len(*resp))
	for i, j := range *resp {
		p := Posting{
			ID:              idText(j.ID),
			Title:           strings.TrimSpace(j.Text),
			Company:         b.company(),
			EmploymentType:  strings.TrimSpace(j.Categories.Commitment),
			ApplyURL:        strings.TrimSpace(j.ApplyURL),
			URL:             strings.TrimSpace(j.HostedURL),
			DescriptionHTML: leverDescription(j),
		}
		if p.ID == "" || p.Title == "" {
			return nil, broke("posting %d has no id or no title", i)
		}
		loc := strings.TrimSpace(j.Categories.Location)
		if loc == "" {
			loc = joinNonEmpty(j.Categories.AllLocations...)
		}
		p.Location = withWorkplace(loc, workplaceKind(j.WorkplaceType))
		if j.CreatedAt > 0 {
			t := time.UnixMilli(j.CreatedAt).UTC()
			p.PostedAt = &t
		}
		out = append(out, p)
	}
	return out, nil
}

// leverDescription assembles the description markup: the description (or the plain text
// as a paragraph when it is empty), each list as a heading and its items, then the closing
// section.
func leverDescription(j leverPosting) string {
	var sb strings.Builder
	if d := strings.TrimSpace(j.Description); d != "" {
		sb.WriteString(d)
	} else if d := strings.TrimSpace(j.DescriptionPlain); d != "" {
		sb.WriteString("<p>" + html.EscapeString(d) + "</p>")
	}
	for _, l := range j.Lists {
		text, content := strings.TrimSpace(l.Text), strings.TrimSpace(l.Content)
		if text == "" && content == "" {
			continue
		}
		if text != "" {
			sb.WriteString("<h3>" + html.EscapeString(text) + "</h3>")
		}
		if content != "" {
			sb.WriteString("<ul>" + content + "</ul>")
		}
	}
	if a := strings.TrimSpace(j.Additional); a != "" {
		sb.WriteString(a)
	}
	return sb.String()
}
