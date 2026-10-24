package contacts

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/robots"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// MaxPages is how many pages Find reads per organisation, the homepage included.
const MaxPages = 3

// The route kinds, as contact_route.kind stores them.
const (
	KindEmail = "email"
	KindForm  = "form"
)

// Fetcher gets one page and names the http_fetch row it was recorded as. The runner wraps
// a *fetch.Client and its recorder; id is 0 when nothing was recorded.
type Fetcher interface {
	Get(ctx context.Context, kind fetch.Kind, rawURL string) (resp fetch.Response, id int64, err error)
}

// RobotsFor returns what the robots.txt of scheme://host says, fetched (KindRobots, outside
// the page cap) or cached by the caller. VerdictSkip means nothing is fetched from the host;
// a nil *robots.Rules with VerdictAllowedAll allows every path.
type RobotsFor func(ctx context.Context, scheme, host string) (robots.Verdict, *robots.Rules)

// Resolver looks up MX records; *net.Resolver implements it.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// Page is one fetched page: the homepage the caller already holds, or one Find fetched.
type Page struct {
	FetchID int64  // its http_fetch row
	URL     string // the address the body came from (the final URL after redirects)
	Body    []byte
}

// Org is the organisation Find reads routes for.
type Org struct {
	Domain    string   // its registrable domain
	Roles     []string // the role local parts (info, office, contact …)
	UserAgent string   // the crawler's User-Agent, for robots.txt matching
	Resolver  Resolver // when set, email routes carry MXOK; nil leaves MXOK false
}

// Route is one contact route and the page it was seen on.
type Route struct {
	Kind    string // KindEmail or KindForm
	Value   string // the lower-case address, or the form page's URL
	FetchID int64  // the page it was seen on
	URL     string // that page's URL
	MXOK    bool   // email only: the org domain has a usable MX record
}

// Routes is what Find found for one organisation. Counts are of distinct addresses across
// every page read.
type Routes struct {
	Routes        []Route
	PersonLike    int
	Foreign       int
	Protected     int
	Pages         int   // pages read, the homepage included (at most MaxPages)
	RobotsSkipped int   // contact links robots.txt kept Find from fetching or using
	FetchFailed   int   // contact pages that gave no usable 2xx answer (a not-public refusal included)
	OffDomain     int   // contact pages whose final URL left the org's registrable domain (not read)
	Stopped       error // the fetch error that cut Find short (context or breaker), else nil
	// Complete is true when every planned page was read or refused (robots, not public, a
	// non-2xx answer, an off-domain redirect); false after a transport error, a breaker stop
	// or a cancelled context, so the caller records no attempt for this search.
	Complete bool
}

// Evidence is a page that yielded at least one route; the caller keeps its body.
type Evidence struct {
	FetchID int64
	URL     string
	Body    []byte
}

// Find reads the homepage, follows at most two contact links (each one robots-checked on its
// own scheme, host and path, before the fetch and again on the final URL after redirects),
// and extracts routes from each page. A fetch refused as not public is skipped quietly; a
// context or breaker stop ends Find and is reported in Routes.Stopped.
func Find(ctx context.Context, f Fetcher, robotsFor RobotsFor, org Org, home Page) (Routes, []Evidence) {
	out := Routes{Complete: true}
	var ev []Evidence
	s := newSeen()

	read := func(p Page) {
		out.Pages++
		before := len(s.emails)
		formsBefore := len(s.forms)
		if !s.extract(p.URL, p.Body, org.Domain, org.Roles) {
			return
		}
		for _, e := range s.emails[before:] {
			out.Routes = append(out.Routes, Route{Kind: KindEmail, Value: e, FetchID: p.FetchID, URL: p.URL})
		}
		for _, fu := range s.forms[formsBefore:] {
			out.Routes = append(out.Routes, Route{Kind: KindForm, Value: fu, FetchID: p.FetchID, URL: p.URL})
		}
		ev = append(ev, Evidence(p))
	}

	read(home)
	if base, err := url.Parse(home.URL); err == nil {
		for _, link := range ContactLinks(base, home.Body, org.Domain) {
			if out.Pages >= MaxPages || out.Stopped != nil {
				break
			}
			p, ok := fetchPage(ctx, f, robotsFor, org, link, &out)
			if ok {
				read(p)
			}
		}
	}

	r := s.result()
	out.PersonLike, out.Foreign, out.Protected = r.PersonLike, r.Foreign, r.Protected
	if org.Resolver != nil && hasEmail(out.Routes) {
		ok := MX(ctx, org.Resolver, org.Domain)
		for i := range out.Routes {
			if out.Routes[i].Kind == KindEmail {
				out.Routes[i].MXOK = ok
			}
		}
	}
	return out, ev
}

// fetchPage fetches one contact link when robots.txt allows it, and returns it only when
// the answer is a 2xx whose final URL stays on the org's registrable domain and robots.txt
// also allows.
func fetchPage(ctx context.Context, f Fetcher, robotsFor RobotsFor, org Org, link string, out *Routes) (Page, bool) {
	ua := org.UserAgent
	u, err := url.Parse(link)
	if err != nil {
		out.FetchFailed++
		return Page{}, false
	}
	if !allowed(ctx, robotsFor, ua, u) {
		out.RobotsSkipped++
		return Page{}, false
	}
	resp, id, err := f.Get(ctx, fetch.KindSite, link)
	if err != nil {
		var open *fetch.OpenError
		switch {
		case ctx.Err() != nil || errors.As(err, &open):
			out.Stopped, out.Complete = err, false
		case resp.Status == 0 && !errors.Is(err, fetch.ErrNotPublic):
			out.Complete = false // a transport error: the page was never answered
		}
		out.FetchFailed++
		return Page{}, false
	}
	if resp.Status/100 != 2 {
		out.FetchFailed++
		return Page{}, false
	}
	final := link
	if resp.FinalURL != "" && resp.FinalURL != link {
		fu, err := url.Parse(resp.FinalURL)
		if err != nil || !onDomain(fu, org.Domain) {
			out.OffDomain++
			return Page{}, false
		}
		if !allowed(ctx, robotsFor, ua, fu) {
			out.RobotsSkipped++
			return Page{}, false
		}
		final = resp.FinalURL
	}
	return Page{FetchID: id, URL: final, Body: resp.Body}, true
}

// onDomain reports whether u's host is on domain's registrable domain.
func onDomain(u *url.URL, domain string) bool {
	own, err := sitelist.Registrable(domain)
	if err != nil {
		return false
	}
	d, err := sitelist.Registrable(u.Hostname())
	return err == nil && d == own
}

func allowed(ctx context.Context, robotsFor RobotsFor, ua string, u *url.URL) bool {
	if robotsFor == nil {
		return false
	}
	v, rules := robotsFor(ctx, u.Scheme, strings.ToLower(u.Hostname()))
	if v == robots.VerdictSkip {
		return false
	}
	return rules.Allowed(ua, u.RequestURI())
}

func hasEmail(rs []Route) bool {
	for _, r := range rs {
		if r.Kind == KindEmail {
			return true
		}
	}
	return false
}

// MX reports whether domain has at least one MX record naming a host. A lookup error, no
// records, and a null MX (a single "." host) all report false.
func MX(ctx context.Context, r Resolver, domain string) bool {
	if r == nil {
		return false
	}
	recs, err := r.LookupMX(ctx, domain)
	if err != nil {
		return false
	}
	for _, mx := range recs {
		if mx != nil && strings.TrimSuffix(mx.Host, ".") != "" {
			return true
		}
	}
	return false
}
