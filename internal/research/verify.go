package research

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"github.com/gremy/webbership-leadhound/internal/contacts"
	"github.com/gremy/webbership-leadhound/internal/dial"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// Verifier checks candidates against the pages they cite, with the finder's own parts.
// The runner builds it the way leads contacts builds contacts.Find's arguments: Fetcher
// over a public-only, recorded fetch.Client (the http_fetch row id is the route's FetchID),
// Robots from probe.HostRobots.For, Roles from leads.yaml's role_local_parts.
type Verifier struct {
	Fetcher   contacts.Fetcher
	Robots    contacts.RobotsFor // nil allows nothing: every candidate is dropped unfetched
	Resolver  contacts.Resolver  // MX lookups; nil fails every email
	Roles     []string           // the role local parts (info, office, contact …)
	UserAgent string             // the crawler's User-Agent, for robots.txt matching
}

type verified struct {
	routes []leadstore.RouteRow
	phones []Phone
}

// page is one cited page's fetch, kept so two candidates on one page fetch it once.
type page struct {
	p  contacts.Page
	ok bool
}

// verify keeps the candidates of a wanted kind whose cited page, on the org's own
// registrable domain, robots-allowed and public, holds them. A cited page is fetched with
// contacts.FetchPage, the finder's own step (robots before and after redirects, a 2xx only,
// no off-domain final URL). It stops early, with what it has, when a fetch reports a
// context or breaker stop.
func (v Verifier) verify(ctx context.Context, req Request, cands []candidate) (verified, error) {
	var out verified
	org := contacts.Org{Domain: req.Org.Domain, Roles: v.Roles, UserAgent: v.UserAgent}
	pages := map[string]page{}
	seen := map[string]bool{}
	mx, mxDone := false, false

	for _, c := range cands {
		switch {
		case c.kind == KindEmail && req.Want.Email:
		case c.kind == KindPhone && req.Want.Phone:
		default:
			continue
		}
		if c.value == "" || !onDomain(c.sourceURL, req.Org.Domain) {
			continue
		}
		pg, done := pages[c.sourceURL]
		if !done {
			st := contacts.Routes{Complete: true}
			pg.p, pg.ok = contacts.FetchPage(ctx, v.Fetcher, v.Robots, org, c.sourceURL, &st)
			if st.Stopped != nil {
				return out, st.Stopped
			}
			pg.ok = pg.ok && pg.p.FetchID != 0 // a page with no http_fetch row can back nothing
			pages[c.sourceURL] = pg
		}
		if !pg.ok {
			continue
		}

		switch c.kind {
		case KindEmail:
			addr := strings.ToLower(c.value)
			if seen[KindEmail+addr] || !roleOnPage(addr, pg.p, req.Org.Domain, v.Roles) {
				continue
			}
			if !mxDone {
				mx, mxDone = contacts.MX(ctx, v.Resolver, req.Org.Domain), true
			}
			if !mx {
				continue
			}
			seen[KindEmail+addr] = true
			out.routes = append(out.routes, leadstore.RouteRow{
				OrgID: req.Org.ID, Kind: leadstore.RouteEmail, Value: addr, FetchID: pg.p.FetchID,
				MXOK: true, DerivedBy: DerivedBy,
			})
		case KindPhone:
			shown, norm, ok := enrich.NormalizePhone(c.value)
			// org_fact keeps only + and 8 to 15 digits, so a number printed without its
			// country code is given the org's (dial.Number); when the country or its rule
			// is unknown it is dropped rather than guessed at. The page's numbers are
			// compared in the same E.164 form, so a national and an international printing
			// of one number match either way round.
			if ok {
				norm = e164(norm, req.Org.Country)
			}
			if !ok || norm == "" || seen[KindPhone+norm] || !phonesOnPage(pg.p, req.Org.Domain, req.Org.Country)[norm] {
				continue
			}
			seen[KindPhone+norm] = true
			out.phones = append(out.phones, Phone{
				Fact: leadstore.FactRow{
					OrgID: req.Org.ID, Kind: leadstore.FactPhone, Value: shown, Normalized: norm,
					FetchID: pg.p.FetchID, DerivedBy: DerivedBy,
				},
				LineKind: c.lineKind, Role: c.role,
			})
		}
	}
	return out, nil
}

// onDomain reports whether rawURL is an http(s) URL on domain's registrable domain.
func onDomain(rawURL, domain string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	own, err := sitelist.Registrable(domain)
	if err != nil {
		return false
	}
	d, err := sitelist.Registrable(u.Hostname())
	return err == nil && d == own
}

// roleOnPage reports whether contacts.Extract, the finder's classification, returns addr
// from p as a role address on domain. A person-like, protected or foreign address is not.
func roleOnPage(addr string, p contacts.Page, domain string, roles []string) bool {
	for _, e := range contacts.Extract(p.URL, p.Body, domain, roles).Emails {
		if e == addr {
			return true
		}
	}
	return false
}

// numberRe finds a run that may be a phone number in a text node, as enrich's labelled
// lines do, without needing the label.
var numberRe = regexp.MustCompile(`\+?[0-9(][0-9 \t\x{a0}().\-/]{5,30}[0-9]`)

// e164 is norm, a number as enrich.NormalizePhone leaves it, in E.164 form: an
// international number as is, a national one with country's calling code (dial.Number,
// trunk prefix dropped where the country has one). It is "" unless the result is + and 8
// to 15 digits, the only phone org_fact stores.
func e164(norm, country string) string {
	n := dial.Number(norm, country)
	if d := len(n) - 1; d < 8 || d > 15 {
		return ""
	}
	return n
}

// phonesOnPage returns the numbers printed on p, in E.164 form (national ones given
// country's code by e164; one that cannot be converted is left out): those enrich.Extract
// reads (tel: links, microdata, JSON-LD, labelled lines) and every number-like run of the
// visible text. A run is also tried from each of its later words, so a number written
// right after other digits (a postcode, a fax label's number) is still found.
func phonesOnPage(p contacts.Page, domain, country string) map[string]bool {
	out := map[string]bool{}
	put := func(norm string) {
		if n := e164(norm, country); n != "" {
			out[n] = true
		}
	}
	for _, f := range enrich.Extract(p.Body, p.URL, domain, true) {
		if f.Kind == enrich.KindPhone {
			put(f.Normalized)
		}
	}
	add := func(run string) {
		words := strings.Fields(run)
		for i := range words {
			if _, norm, ok := enrich.NormalizePhone(strings.Join(words[i:], " ")); ok {
				put(norm)
			}
		}
	}
	z := html.NewTokenizer(strings.NewReader(string(capBody(p.Body))))
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken:
			if hidden(z) {
				skip++
			}
		case html.EndTagToken:
			if hidden(z) && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				for _, m := range numberRe.FindAllString(string(z.Text()), -1) {
					add(m)
				}
			}
		}
	}
}

func hidden(z *html.Tokenizer) bool {
	name, _ := z.TagName()
	switch string(name) {
	case "script", "style", "template", "noscript":
		return true
	}
	return false
}

func capBody(b []byte) []byte {
	if len(b) > enrich.MaxBody {
		return b[:enrich.MaxBody]
	}
	return b
}
