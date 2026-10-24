package ats

import (
	"encoding/json"
	"html"
	"net/url"
	"strings"
)

// smartRecruitersList is one page of
// GET https://api.smartrecruiters.com/v1/companies/<slug>/postings?limit=100&offset=N.
// The list gives no description and no apply URL; the detail endpoint has them (see
// parseSmartRecruitersDetail).
type smartRecruitersList struct {
	Offset     int                       `json:"offset"`
	Limit      int                       `json:"limit"`
	TotalFound *int                      `json:"totalFound"`
	Content    *[]smartRecruitersPosting `json:"content"`
}

type smartRecruitersPosting struct {
	ID      json.RawMessage `json:"id"`
	Name    string          `json:"name"`
	Company struct {
		Identifier string `json:"identifier"`
		Name       string `json:"name"`
	} `json:"company"`
	ReleasedDate string `json:"releasedDate"`
	Location     *struct {
		City         string `json:"city"`
		Region       string `json:"region"`
		Country      string `json:"country"` // a lower-case country code
		Remote       *bool  `json:"remote"`
		Hybrid       *bool  `json:"hybrid"`
		FullLocation string `json:"fullLocation"`
	} `json:"location"`
	TypeOfEmployment struct {
		Label string `json:"label"`
	} `json:"typeOfEmployment"`
}

// parseSmartRecruiters maps one list page of a SmartRecruiters board. Description and
// ApplyURL stay empty: the list has neither (parseSmartRecruitersDetail reads them). URL is
// the public page https://jobs.smartrecruiters.com/<board>/<id>, the board slug as spelled.
func parseSmartRecruiters(b Board, body []byte) ([]Posting, error) {
	var resp smartRecruitersList
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, broke("bad JSON: %v", err)
	}
	if resp.Content == nil {
		return nil, broke("no content array")
	}
	out := make([]Posting, 0, len(*resp.Content))
	for i, j := range *resp.Content {
		p := Posting{
			ID:             idText(j.ID),
			Title:          strings.TrimSpace(j.Name),
			Company:        strings.TrimSpace(j.Company.Name),
			EmploymentType: strings.TrimSpace(j.TypeOfEmployment.Label),
			PostedAt:       parseTime(j.ReleasedDate),
		}
		if p.ID == "" || p.Title == "" {
			return nil, broke("posting %d has no id or no name", i)
		}
		if p.Company == "" {
			p.Company = b.company()
		}
		p.URL = "https://jobs.smartrecruiters.com/" + url.PathEscape(b.Board) + "/" + url.PathEscape(p.ID)
		if l := j.Location; l != nil {
			text := strings.TrimSpace(l.FullLocation)
			if text == "" {
				text = joinNonEmpty(l.City, l.Region, strings.ToUpper(l.Country))
			}
			p.Location = withWorkplace(text, smartRecruitersWorkplace(l.Remote, l.Hybrid))
		}
		out = append(out, p)
	}
	return out, nil
}

// smartRecruitersWorkplace reads the remote and hybrid flags: hybrid=true is hybrid (even
// with remote=true), remote=true is remote, and a flag present but neither true is
// on-site. With neither flag present the posting states nothing ("").
func smartRecruitersWorkplace(remote, hybrid *bool) string {
	switch {
	case remote == nil && hybrid == nil:
		return ""
	case hybrid != nil && *hybrid:
		return workHybrid
	case remote != nil && *remote:
		return workRemote
	}
	return workOnsite
}

// smartRecruitersPage reads a list page's paging fields, for the caller that walks the
// pages: totalFound is every posting on the board, offset and limit echo the request. The
// next page starts at offset+len(content) while that is below total. A page with no
// totalFound or no content array, or a negative total, gives ErrParserBroke.
func smartRecruitersPage(body []byte) (total, offset, limit int, err error) {
	var resp smartRecruitersList
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, 0, broke("bad JSON: %v", err)
	}
	if resp.TotalFound == nil || resp.Content == nil {
		return 0, 0, 0, broke("no totalFound or no content array")
	}
	if *resp.TotalFound < 0 {
		return 0, 0, 0, broke("negative totalFound %d", *resp.TotalFound)
	}
	return *resp.TotalFound, resp.Offset, resp.Limit, nil
}

// smartRecruitersDetail is GET https://api.smartrecruiters.com/v1/companies/<slug>/postings/<id>.
type smartRecruitersDetail struct {
	PostingURL string `json:"postingUrl"`
	ApplyURL   string `json:"applyUrl"`
	JobAd      *struct {
		Sections map[string]struct {
			Title string `json:"title"`
			Text  string `json:"text"` // HTML
		} `json:"sections"`
	} `json:"jobAd"`
}

// smartRecruitersSections is the order the job ad's sections are joined in.
var smartRecruitersSections = []string{"companyDescription", "jobDescription", "qualifications", "additionalInformation"}

// parseSmartRecruitersDetail reads one posting's detail: the job ad's sections joined in
// order, each as <h3>title</h3> then its HTML (a section with no text is left out, one with
// no title has no heading), the apply URL and the public posting URL. A body with no jobAd
// object gives ErrParserBroke; missing sections or URLs are just empty.
func parseSmartRecruitersDetail(body []byte) (descriptionHTML, applyURL, postingURL string, err error) {
	var resp smartRecruitersDetail
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", "", broke("bad JSON: %v", err)
	}
	if resp.JobAd == nil {
		return "", "", "", broke("no jobAd object")
	}
	var parts []string
	for _, key := range smartRecruitersSections {
		s, ok := resp.JobAd.Sections[key]
		text := strings.TrimSpace(s.Text)
		if !ok || text == "" {
			continue
		}
		if title := strings.TrimSpace(s.Title); title != "" {
			parts = append(parts, "<h3>"+html.EscapeString(title)+"</h3>")
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n"), strings.TrimSpace(resp.ApplyURL), strings.TrimSpace(resp.PostingURL), nil
}
