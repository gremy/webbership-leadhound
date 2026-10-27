package console

import (
	"cmp"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxFilterValues is the most values one repeatable filter (kind, country, channel) takes
// from a request; the rest are dropped before the backend sees them.
const maxFilterValues = 50

// leadListView is what the Leads list renders: the backend's page, the line from the last
// action, and the pager's links, which keep the filter.
type leadListView struct {
	P          LeadListPage
	Message    string
	Error      bool
	Pages      int
	Prev, Next string // "" when there is no such page
	NoLeads    bool   // there are no leads at all
	NoneMatch  bool   // there are leads, but none matches the filter
}

// leadsTabView is the Leads sub-nav: which tab is current, and whether the page is that
// tab itself (aria-current="page") or a page inside it (aria-current="true").
type leadsTabView struct {
	Tab   string // list | outreach | settings
	Exact bool
}

// leadList is GET /leads: the leads under the query's filter, one page of them.
func (s *server) leadList(w http.ResponseWriter, r *http.Request) {
	msg := s.takeFlash(w, r)
	p, err := s.b.LeadList(r.Context(), leadListQuery(r.URL.Query()))
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	s.render(w, r, http.StatusOK, "leads_list", page{Title: "Leads", Active: leadsPath, SignedIn: true,
		Data: newLeadListView(p, msg)})
}

// leadDetail is GET /leads/{id}: everything stored about one lead.
func (s *server) leadDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	msg := s.takeFlash(w, r)
	d, err := s.b.LeadDetail(r.Context(), id)
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	title := cmp.Or(d.Name, d.Domain, "Lead "+strconv.FormatInt(id, 10))
	s.render(w, r, http.StatusOK, "lead_detail", page{Title: title, Active: leadsPath, SignedIn: true,
		Data: leadsView{Data: d, Message: msg}})
}

// leadListQuery reads the list's filter from a query: every value trimmed, empty ones
// dropped, each repeatable filter capped at maxFilterValues.
func leadListQuery(v url.Values) LeadListQuery {
	one := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	return LeadListQuery{
		Kinds:        filterValues(v["kind"]),
		Countries:    filterValues(v["country"]),
		Channels:     filterValues(v["channel"]),
		Starred:      one("starred"),
		HasPhone:     one("has_phone"),
		MinRelevance: one("min_relevance"),
		Status:       one("status"),
		Sort:         one("sort"),
		Page:         one("page"),
	}
}

func filterValues(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && len(out) < maxFilterValues {
			out = append(out, v)
		}
	}
	return out
}

// newLeadListView works out the pager and the empty states for p.
func newLeadListView(p LeadListPage, msg string) leadListView {
	v := leadListView{P: p, Message: msg}
	if p.PerPage > 0 {
		v.Pages = (p.Total + p.PerPage - 1) / p.PerPage
	}
	if p.Page > 1 {
		v.Prev = leadListURL(p.Filter, p.Page-1)
	}
	if p.PerPage > 0 && p.Page*p.PerPage < p.Total {
		v.Next = leadListURL(p.Filter, p.Page+1)
	}
	v.NoLeads = p.TotalLeads == 0
	v.NoneMatch = !v.NoLeads && len(p.Rows) == 0
	return v
}

// leadListURL is the list under f at page n: the filter kept, the page set (left out for
// the first).
func leadListURL(f LeadListQuery, n int) string {
	q := url.Values{}
	for k, vs := range map[string][]string{"kind": f.Kinds, "country": f.Countries, "channel": f.Channels} {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	for k, v := range map[string]string{"starred": f.Starred, "has_phone": f.HasPhone, "min_relevance": f.MinRelevance,
		"status": f.Status, "sort": f.Sort} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if n > 1 {
		q.Set("page", strconv.Itoa(n))
	}
	if len(q) == 0 {
		return leadsPath
	}
	return leadsPath + "?" + q.Encode()
}

// webURL reports whether s is an http or https URL with a host: only such a URL becomes a
// link on a lead's page; anything else is shown as text.
func webURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	}
	return false
}
