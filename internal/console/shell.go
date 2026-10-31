package console

import (
	"net/http"
	"strings"
)

// shellLink is one link of the shell's navigation. Under are the path prefixes of the
// pages that sit inside it (a lead page under List), which mark it current with
// aria-current="true"; the link's own path marks it aria-current="page".
type shellLink struct {
	Label, Path, Icon string
	Under             []string
}

// leadsNav is the Leads product's sub-nav. Batches, Calls and Reports have no list page of
// their own yet: a batch sits under Outreach, a call under Today, a report under List.
var leadsNav = []shellLink{
	{Label: "Home", Path: "/leads/home"},
	{Label: "List", Path: "/leads", Under: []string{"/leads/"}},
	{Label: "Outreach", Path: "/leads/outreach", Under: []string{"/leads/batches/"}},
	{Label: "Today", Path: "/leads/today", Under: []string{"/leads/calls/"}},
	{Label: "Drafts", Path: "/leads/drafts"},
	{Label: "Campaigns", Path: "/leads/campaigns"},
}

// jobsNav is the Jobs product's sub-nav.
var jobsNav = []shellLink{
	{Label: "Home", Path: "/jobs/home"},
	{Label: "Jobs", Path: "/jobs", Under: []string{"/jobs/"}},
	{Label: "Tracked", Path: "/tracked"},
	{Label: "Runs", Path: "/runs"},
	{Label: "Rules", Path: "/rules"},
	{Label: "Results", Path: "/results"},
}

// gearNav is the gear menu's links; Sign out follows them.
var gearNav = []shellLink{
	{Label: "Settings", Path: "/leads/settings", Icon: "sliders", Under: []string{"/leads/settings/"}},
	{Label: "Controls", Path: "/controls", Icon: "power", Under: []string{"/controls/"}},
	{Label: "Mailboxes", Path: "/mailboxes", Icon: "mail", Under: []string{"/mailboxes/"}},
	{Label: "Account", Path: "/account", Icon: "user"},
}

// products are the switcher's two products, each with its home.
var products = []shellLink{
	{Label: "Leads", Path: "/leads/home"},
	{Label: "Jobs", Path: "/jobs/home"},
}

// navLink is a shell link as the layout renders it. Current is "page", "true" or "".
type navLink struct {
	Label, Path, Icon, Current string
}

// shellView is the shell's navigation for one page.
type shellView struct {
	Switcher []navLink
	Sub      []navLink // the active product's sub-nav; none on a page of no product
	SubLabel string
	Gear     []navLink
	Operator string
}

// productOf is the product a path belongs to: "leads", "jobs", or "" for the dashboard
// and the gear pages (Settings, Controls, Mailboxes, Account).
func productOf(p string) string {
	under := func(prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }
	switch {
	case under("/leads/settings"):
		return ""
	case under("/leads"):
		return "leads"
	case under("/jobs"), under("/tracked"), under("/runs"), under("/rules"), under("/results"):
		return "jobs"
	}
	return ""
}

// shell builds the navigation for the page at path p of product.
func (s *server) shell(p, product string) shellView {
	v := shellView{Gear: current(gearNav, p), Operator: s.opt.Operator}
	for _, l := range products {
		nl := navLink{Label: l.Label, Path: l.Path}
		if strings.EqualFold(l.Label, product) {
			nl.Current = "true"
			if p == l.Path {
				nl.Current = "page"
			}
		}
		v.Switcher = append(v.Switcher, nl)
	}
	switch product {
	case "leads":
		v.Sub, v.SubLabel = current(leadsNav, p), "Leads"
	case "jobs":
		v.Sub, v.SubLabel = current(jobsNav, p), "Jobs"
	}
	return v
}

// current marks the link for path p: the link whose own path it is gets "page"; failing
// that, the link with the longest Under prefix of p gets "true".
func current(links []shellLink, p string) []navLink {
	out := make([]navLink, len(links))
	exact, best, bestLen := -1, -1, 0
	for i, l := range links {
		out[i] = navLink{Label: l.Label, Path: l.Path, Icon: l.Icon}
		if l.Path == p {
			exact = i
		}
		for _, u := range l.Under {
			if strings.HasPrefix(p, u) && len(u) > bestLen {
				best, bestLen = i, len(u)
			}
		}
	}
	switch {
	case exact >= 0:
		out[exact].Current = "page"
	case best >= 0:
		out[best].Current = "true"
	}
	return out
}

// leadsHome is GET /leads/home: the Leads part of the dashboard.
func (s *server) leadsHome(w http.ResponseWriter, r *http.Request) {
	s.renderHome(w, r, http.StatusOK, "leads_home", page{Title: "Leads", Active: "/leads/home", Message: s.takeFlash(w, r)})
}

// jobsHome is GET /jobs/home: the job-hunt part of the dashboard.
func (s *server) jobsHome(w http.ResponseWriter, r *http.Request) {
	s.renderHome(w, r, http.StatusOK, "jobs_home", page{Title: "Jobs", Active: "/jobs/home", Message: s.takeFlash(w, r)})
}

// account is GET /account: who is signed in, and signing out.
func (s *server) account(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "account", page{Title: "Account", Active: "/account", SignedIn: true, Message: s.takeFlash(w, r)})
}
