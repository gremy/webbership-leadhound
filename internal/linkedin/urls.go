package linkedin

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	searchEndpoint = "https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search"
	detailEndpoint = "https://www.linkedin.com/jobs-guest/jobs/api/jobPosting/"
)

// SearchURL builds the guest search URL for one page of remote (f_WT=2) results posted
// in the last tprSeconds. Pages count from 1; each holds up to 10 cards.
func SearchURL(keywords, location string, tprSeconds, page int) string {
	if page < 1 {
		page = 1
	}
	var b strings.Builder
	b.WriteString(searchEndpoint)
	b.WriteString("?keywords=")
	b.WriteString(url.QueryEscape(keywords))
	b.WriteString("&location=")
	b.WriteString(url.QueryEscape(location))
	b.WriteString("&f_WT=2&f_TPR=r")
	b.WriteString(strconv.Itoa(tprSeconds))
	b.WriteString("&start=")
	b.WriteString(strconv.Itoa((page - 1) * 10))
	return b.String()
}

// DetailURL builds the guest detail URL for one job ID.
func DetailURL(id string) string {
	return detailEndpoint + url.PathEscape(id)
}

// CanonicalJobURL normalises a job link: any linkedin.com host (fr., ro., bare) becomes
// www.linkedin.com over https, and the query and fragment are dropped. Other hosts keep
// their host and only lose the query and fragment. Unparseable input comes back as is.
func CanonicalJobURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if host == "linkedin.com" || strings.HasSuffix(host, ".linkedin.com") {
		u.Scheme = "https"
		u.Host = "www.linkedin.com"
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

var companySlugRE = regexp.MustCompile(`(?i)linkedin\.com/company/([^/?#\s]+)`)

// CompanySlug returns the <slug> of a linkedin.com/company/<slug> link, or "".
func CompanySlug(href string) string {
	m := companySlugRE.FindStringSubmatch(href)
	if m == nil {
		return ""
	}
	return m[1]
}

// companyURL strips the query and fragment from a company link, keeping its host.
func companyURL(href string) string {
	href = strings.TrimSpace(href)
	if i := strings.IndexAny(href, "?#"); i >= 0 {
		href = href[:i]
	}
	return href
}
