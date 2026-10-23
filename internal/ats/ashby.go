package ats

import (
	"encoding/json"
	"net/url"
	"path"
	"strings"
)

// ashbyBoard is GET /posting-api/job-board/<slug>.
type ashbyBoard struct {
	Jobs *[]ashbyJob `json:"jobs"`
}

type ashbyJob struct {
	ID                 json.RawMessage `json:"id"`
	Title              string          `json:"title"`
	Location           string          `json:"location"`
	SecondaryLocations []struct {
		Location string `json:"location"`
	} `json:"secondaryLocations"`
	EmploymentType  string `json:"employmentType"` // FullTime, PartTime, Intern, Contract, Temporary
	IsListed        *bool  `json:"isListed"`
	IsRemote        *bool  `json:"isRemote"`      // true for Remote and Hybrid
	WorkplaceType   string `json:"workplaceType"` // OnSite, Remote or Hybrid
	PublishedAt     string `json:"publishedAt"`
	ApplyURL        string `json:"applyUrl"`
	JobURL          string `json:"jobUrl"`
	DescriptionHTML string `json:"descriptionHtml"`
}

// parseAshby maps an Ashby board, dropping postings marked isListed=false.
func parseAshby(b Board, body []byte) ([]Posting, error) {
	var resp ashbyBoard
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, broke("bad JSON: %v", err)
	}
	if resp.Jobs == nil {
		return nil, broke("no jobs array")
	}
	out := make([]Posting, 0, len(*resp.Jobs))
	for i, j := range *resp.Jobs {
		if j.IsListed != nil && !*j.IsListed {
			continue
		}
		p := Posting{
			ID:              idText(j.ID),
			Title:           strings.TrimSpace(j.Title),
			Company:         b.company(),
			DescriptionHTML: strings.TrimSpace(j.DescriptionHTML),
			EmploymentType:  strings.TrimSpace(j.EmploymentType),
			PostedAt:        parseTime(j.PublishedAt),
			ApplyURL:        strings.TrimSpace(j.ApplyURL),
			URL:             strings.TrimSpace(j.JobURL),
		}
		if p.ID == "" {
			p.ID = ashbyIDFromURL(p.URL)
		}
		if p.ID == "" || p.Title == "" {
			return nil, broke("job %d has no id or no title", i)
		}
		locs := []string{j.Location}
		for _, s := range j.SecondaryLocations {
			locs = append(locs, s.Location)
		}
		// isRemote is true for Hybrid as well as Remote, so it alone states no workplace;
		// only workplaceType does, and without it the gates read the text.
		p.Location = withWorkplace(joinNonEmpty(locs...), workplaceKind(j.WorkplaceType))
		out = append(out, p)
	}
	return out, nil
}

// ashbyIDFromURL takes the posting id from the last path segment of its jobUrl, for a
// response that leaves id out (the public docs do not list it).
func ashbyIDFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return ""
	}
	seg := path.Base(strings.TrimSuffix(u.Path, "/"))
	if seg == "." || seg == "/" {
		return ""
	}
	return seg
}
