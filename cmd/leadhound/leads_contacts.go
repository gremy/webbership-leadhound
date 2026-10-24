package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/agencies"
	"github.com/gremy/webbership-leadhound/internal/contacts"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
	"github.com/gremy/webbership-leadhound/internal/robots"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() {
	leadsCommands["contacts"] = runLeadsContacts
	leadsCommands["export"] = runLeadsExport
}

// The run row of a route search.
const (
	commandLeadsContacts  = "leads-contacts"
	sourceContacts        = "contacts"
	sourceContactsFixture = "contacts-fixture"
)

// defaultContactsMax is how many orgs one route search takes without --max.
const defaultContactsMax = 100

// contactsResolver is the MX resolver of a live route search; tests replace it. A dry run
// uses contactsDryResolver, which looks nothing up unless a test sets it.
var (
	contactsResolver    contacts.Resolver = net.DefaultResolver
	contactsDryResolver contacts.Resolver
)

const leadsContactsUsage = "usage: leadhound leads contacts [--max N (at least 1)] [--org ID]... [--dry-run --fixtures DIR]"

// runLeadsContacts is `leads contacts [--max N] [--org ID]... [--dry-run --fixtures DIR]`:
// it makes a site_owner org for every Drupal site that has none, then reads the contact
// routes of the orgs that need them (only the named ones with --org), at most N of them.
// A dry run reads fixtures and touches only orgs on reserved TLDs.
func runLeadsContacts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads contacts", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	maxOrgs := fs.Int("max", defaultContactsMax, "most orgs to search in this run")
	var named stringList
	fs.Var(&named, "org", "search this org id (repeatable)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *maxOrgs < 1 {
		return usageError(leadsContactsUsage)
	}
	if err := ff.check(); err != nil {
		return err
	}
	var ids []int64
	for _, n := range named {
		id, err := strconv.ParseInt(n, 10, 64)
		if err != nil || id < 1 {
			return usageError("--org %q is not an org id", n)
		}
		ids = append(ids, id)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	setup, err := leadsEnv(cfg, leadsForProbe)
	if err != nil {
		return err
	}
	newClient, err := siteClients(cfg, ff, siteLimiter(setup.env))
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "leads contacts: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())

	newOrgs, err := orgsFromProbes(ctx, lq, ff.dryRun)
	if err != nil {
		return err
	}
	orgs, drupalOrgs, err := orgsToSearch(ctx, lq, ids, ff.dryRun, *maxOrgs)
	if err != nil {
		return err
	}

	source := sourceContacts
	resolver := contactsResolver
	if ff.dryRun {
		source, resolver = sourceContactsFixture, contactsDryResolver
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsContacts, source)
	if err != nil {
		return err
	}
	rec := store.NewFetchRecorder(q, runID, source).SizeOnly(fetch.KindSite, fetch.KindRobots)
	cs := contactSearch{
		pool: s.Pool(), runID: runID, newClient: newClient, rec: rec, resolver: resolver,
		roles: setup.leads.RoleLocalParts, publicSuffixes: setup.leads.PublicBodySuffixes,
	}
	sum := contactSummary{NewOrgs: newOrgs, DrupalOrgs: drupalOrgs}
	var runErr error
	for _, o := range orgs {
		if ctx.Err() != nil {
			break
		}
		if err := cs.search(ctx, o, &sum); err != nil {
			runErr = err
			break
		}
	}
	result := store.RunResult{
		Requests: sum.Requests, Failures: sum.Failures,
		Note: fmt.Sprintf("%d orgs checked, %d routes", sum.Checked, sum.Emails+sum.Forms),
	}
	switch {
	case runErr != nil:
		result.Note += "; " + runErr.Error()
	case ctx.Err() != nil:
		result.Note += "; interrupted"
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	printContactSummary(stdout, runID, ff.dryRun, sum)
	return ctx.Err()
}

// orgsFromProbes makes a site_owner org for every site whose latest probe found Drupal and
// that has no org yet (on a dry run, reserved TLDs only). It returns how many it created.
func orgsFromProbes(ctx context.Context, lq leadstore.Queries, dryRun bool) (int, error) {
	probes, err := lq.DrupalProbesWithoutOrg(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range probes {
		if dryRun && !isReserved(p.Domain) {
			continue
		}
		_, created, err := lq.UpsertSiteOrg(ctx, p.ProbeID)
		if err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}

// orgsToSearch is the named orgs, or the orgs that need routes; never an org on drupal.org
// (counted in drupalOrgs); on a dry run, only those on reserved TLDs; at most maxOrgs.
func orgsToSearch(ctx context.Context, lq leadstore.Queries, ids []int64, dryRun bool, maxOrgs int) (out []leadstore.Org, drupalOrgs int, err error) {
	var all []leadstore.Org
	if len(ids) > 0 {
		for _, id := range ids {
			o, err := lq.OrgByID(ctx, id)
			if errors.Is(err, leadstore.ErrOrgNotFound) {
				return nil, 0, usageError("--org %d: no such org", id)
			}
			if err != nil {
				return nil, 0, err
			}
			all = append(all, o)
		}
	} else {
		limit := maxOrgs
		if dryRun {
			limit = 0 // filtered below, then capped
		}
		if all, err = lq.OrgsNeedingRoutes(ctx, limit, time.Now()); err != nil {
			return nil, 0, err
		}
	}
	for _, o := range all {
		switch {
		case o.Kind == leadstore.OrgSelftest:
			continue
		case agencies.IsDrupalOrg("https://" + o.Domain + "/"):
			drupalOrgs++
			continue
		case dryRun && !isReserved(o.Domain):
			continue
		}
		if len(out) < maxOrgs {
			out = append(out, o)
		}
	}
	return out, drupalOrgs, nil
}

// contactSummary is what one route search did.
type contactSummary struct {
	NewOrgs       int // site_owner orgs made from Drupal probes
	Checked       int // orgs searched
	Emails        int // role email routes found
	EmailsMX      int // of them, on a domain with MX
	Forms         int
	PersonLike    int
	Protected     int
	Foreign       int
	NoRoute       int // searched orgs with no route
	PublicBodies  int // orgs skipped as public bodies
	DrupalOrgs    int // orgs on drupal.org, never searched
	RobotsSkipped int // pages robots.txt kept the search from
	OffDomain     int // pages whose final URL left the org's domain (not read)
	CutShort      int // searches that did not complete: nothing recorded, still due
	Requests      int
	Failures      int
}

// contactSearch holds what every org's search shares.
type contactSearch struct {
	pool           *pgxpool.Pool
	runID          int64
	newClient      func(fetch.Recorder) *fetch.Client
	rec            *store.FetchRecorder
	resolver       contacts.Resolver
	roles          []string
	publicSuffixes []string
}

// isPublicBody reports whether domain is, or sits under, one of the public-body suffixes.
func isPublicBody(domain string, suffixes []string) bool {
	d := strings.ToLower(domain)
	for _, s := range suffixes {
		s = strings.ToLower(strings.Trim(s, ". "))
		if s != "" && (d == s || strings.HasSuffix(d, "."+s)) {
			return true
		}
	}
	return false
}

// search reads one org's routes. A completed search stores its routes, the bodies of the
// pages they came from and the attempt in one transaction; a search cut short (no answer,
// a breaker stop, a cancel) stores nothing, so the org stays due. Only a store error is
// returned. The writes run without the run's cancel, so a stop never breaks a transaction.
func (cs contactSearch) search(ctx context.Context, o leadstore.Org, sum *contactSummary) error {
	wctx := context.WithoutCancel(ctx)
	if isPublicBody(o.Domain, cs.publicSuffixes) {
		sum.PublicBodies++
		return leadstore.NewQueries(cs.pool).RecordContactAttempt(wctx, o.ID, cs.runID, 0)
	}
	h, err := leadstore.NewQueries(cs.pool).OrgHomepage(wctx, o.ID)
	if err != nil {
		return err
	}
	sum.Checked++
	pr := &pageRecorder{inner: cs.rec, bodies: map[int64][]byte{}}
	c := cs.newClient(pr)
	counter := countRequests(c)
	pf := pageFetcher{c: c, rec: pr}
	hr := probe.NewHostRobotsFunc(pf.robots)

	stored := len(h.Body) > 0
	var routes contacts.Routes
	var ev []contacts.Evidence
	home := contacts.Page{FetchID: h.FetchID, URL: h.URL, Body: h.Body}
	if h.FinalURL != "" {
		home.URL = h.FinalURL
	}
	got := homeOK
	if !stored {
		start := home.URL
		if start == "" {
			start = "https://" + o.Domain + "/"
		}
		home, got = fetchHome(ctx, pf, hr, c.UserAgent, start, o.Domain)
	}
	complete := got != homeFailed
	switch got {
	case homeOK:
		routes, ev = contacts.Find(ctx, pf, hr.For, contacts.Org{
			Domain: o.Domain, Roles: cs.roles, UserAgent: c.UserAgent, Resolver: cs.resolver,
		}, home)
		complete = routes.Complete
	case homeRobots:
		sum.RobotsSkipped++
	case homeOffDomain:
		sum.OffDomain++
	}
	sum.Requests += int(counter.n)
	sum.Failures += c.Breaker.Failures()
	if pr.err != nil {
		return pr.err
	}
	sum.RobotsSkipped += routes.RobotsSkipped
	sum.OffDomain += routes.OffDomain
	if !complete || ctx.Err() != nil {
		sum.CutShort++
		return nil
	}
	sum.PersonLike += routes.PersonLike
	sum.Protected += routes.Protected
	sum.Foreign += routes.Foreign

	tx, err := cs.pool.Begin(wctx)
	if err != nil {
		return fmt.Errorf("contacts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(wctx) }() // a no-op after Commit
	lq, sq := leadstore.NewQueries(tx), store.NewQueries(tx)
	found := 0
	for _, r := range routes.Routes {
		if r.FetchID == 0 {
			continue
		}
		if _, err := lq.InsertRoute(wctx, leadstore.RouteRow{
			OrgID: o.ID, Kind: r.Kind, Value: r.Value, FetchID: r.FetchID, MXOK: r.MXOK, DerivedBy: contacts.DerivedBy,
		}); err != nil {
			return err
		}
		found++
		if r.Kind == contacts.KindForm {
			sum.Forms++
			continue
		}
		sum.Emails++
		if r.MXOK {
			sum.EmailsMX++
		}
	}
	for _, e := range ev {
		if e.FetchID == 0 || (stored && e.FetchID == h.FetchID) {
			continue // the stored homepage already keeps its body
		}
		body, ok := pr.bodies[e.FetchID]
		if !ok {
			continue
		}
		if err := sq.AttachFetchBody(wctx, e.FetchID, body); err != nil {
			return err
		}
	}
	if found == 0 {
		sum.NoRoute++
	}
	if err := lq.RecordContactAttempt(wctx, o.ID, cs.runID, found); err != nil {
		return err
	}
	if err := tx.Commit(wctx); err != nil {
		return fmt.Errorf("contacts: commit: %w", err)
	}
	return nil
}

// homeResult is how fetchHome ended. Every result but homeFailed is a complete answer.
type homeResult int

const (
	homeOK        homeResult = iota // a 2xx page, robots-allowed, on the org's domain
	homeRobots                      // robots.txt refused the page, before or after redirects
	homeRefused                     // not public, or on drupal.org: never fetched or never used
	homeOffDomain                   // the final URL left the org's registrable domain
	homeFailed                      // no answer, a breaker stop, a non-2xx answer or a bad URL
)

// fetchHome fetches a page when robots.txt allows it, before and after redirects, and
// returns it on a 2xx. With orgDomain set, a final URL on another registrable domain is
// not used (homeOffDomain).
func fetchHome(ctx context.Context, pf pageFetcher, hr *probe.HostRobots, ua, rawURL, orgDomain string) (contacts.Page, homeResult) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return contacts.Page{}, homeFailed
	}
	if agencies.IsDrupalOrg(rawURL) {
		return contacts.Page{}, homeRefused
	}
	if !robotsAllow(ctx, hr, ua, u) {
		return contacts.Page{}, homeRobots
	}
	resp, id, err := pf.Get(ctx, fetch.KindSite, rawURL)
	switch {
	case errors.Is(err, fetch.ErrNotPublic) || errors.Is(err, errDrupalOrg):
		return contacts.Page{}, homeRefused
	case err != nil || resp.Status/100 != 2:
		return contacts.Page{}, homeFailed
	}
	final := rawURL
	if resp.FinalURL != "" && resp.FinalURL != rawURL {
		final = resp.FinalURL
	}
	fu, err := url.Parse(final)
	if err != nil {
		return contacts.Page{}, homeFailed
	}
	if orgDomain != "" {
		if d, err := sitelist.Registrable(fu.Hostname()); err != nil || d != orgDomain {
			return contacts.Page{}, homeOffDomain
		}
	}
	if final != rawURL && !robotsAllow(ctx, hr, ua, fu) {
		return contacts.Page{}, homeRobots
	}
	return contacts.Page{FetchID: id, URL: final, Body: resp.Body}, homeOK
}

// robotsAllow reports whether the robots.txt of u's scheme and host lets ua fetch u.
func robotsAllow(ctx context.Context, hr *probe.HostRobots, ua string, u *url.URL) bool {
	v, rules := hr.For(ctx, u.Scheme, strings.ToLower(u.Hostname()))
	return v != robots.VerdictSkip && rules.Allowed(ua, u.RequestURI())
}

// idRecorder is the run's recorder: it names the row of the latest Record per URL.
type idRecorder interface {
	fetch.Recorder
	LastIDFor(rawURL string) (int64, bool)
}

// pageRecorder is one org's (or one agency's) view of the run's recorder: it keeps the id of
// the latest row and the body of every fetch, so the pages that are cited can have their
// bodies attached exactly as they arrived.
type pageRecorder struct {
	inner  idRecorder
	bodies map[int64][]byte
	lastID int64
	err    error
}

// Record writes r without the run's cancel, so a fetch cut short is still recorded.
func (p *pageRecorder) Record(ctx context.Context, r fetch.Record) error {
	if err := p.inner.Record(context.WithoutCancel(ctx), r); err != nil {
		if p.err == nil {
			p.err = err
		}
		return err
	}
	id, ok := p.inner.LastIDFor(r.URL)
	if !ok {
		p.err = fmt.Errorf("contacts: no fetch id after recording %s", r.URL)
		return p.err
	}
	p.lastID = id
	if r.Status != 0 {
		p.bodies[id] = r.Body
	}
	return nil
}

// errDrupalOrg refuses a URL on drupal.org, which is only ever read by hand.
var errDrupalOrg = errors.New("leads: drupal.org is not fetched")

// pageFetcher is contacts.Fetcher over one client and its pageRecorder. A URL on drupal.org
// is refused without a request, and an answer that ended on drupal.org is not used. With a
// pacer, each Get first waits its turn for the URL's host, outside the client's timeout,
// and the final host is marked as just asked.
type pageFetcher struct {
	c     *fetch.Client
	rec   *pageRecorder
	pacer *hostPacer
}

func (f pageFetcher) Get(ctx context.Context, kind fetch.Kind, rawURL string) (fetch.Response, int64, error) {
	if agencies.IsDrupalOrg(rawURL) {
		return fetch.Response{}, 0, errDrupalOrg
	}
	if f.pacer != nil {
		if err := f.pacer.wait(ctx, hostOf(rawURL)); err != nil {
			return fetch.Response{}, 0, err
		}
	}
	before := f.rec.lastID
	resp, err := f.c.Get(ctx, kind, rawURL)
	if f.pacer != nil && resp.FinalURL != "" {
		f.pacer.mark(hostOf(resp.FinalURL))
	}
	id := f.rec.lastID
	if id == before {
		id = 0
	}
	if err == nil && resp.FinalURL != "" && agencies.IsDrupalOrg(resp.FinalURL) {
		return fetch.Response{}, id, errDrupalOrg
	}
	return resp, id, err
}

// robots is a robots.txt Get through f, for probe.NewHostRobotsFunc.
func (f pageFetcher) robots(ctx context.Context, rawURL string) (fetch.Response, error) {
	resp, _, err := f.Get(ctx, fetch.KindRobots, rawURL)
	return resp, err
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// requestCounter counts the HTTP requests a client sends, every request of a redirect chain
// included. A client fetches one URL at a time, so it needs no lock.
type requestCounter struct {
	inner http.RoundTripper
	n     int64
}

func (t *requestCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	t.n++
	return t.inner.RoundTrip(req)
}

// countRequests wraps c's transport in a requestCounter and returns it.
func countRequests(c *fetch.Client) *requestCounter {
	rc := &requestCounter{inner: c.HTTP.Transport}
	if rc.inner == nil {
		rc.inner = http.DefaultTransport // what a nil Transport means
	}
	c.HTTP.Transport = rc
	return rc
}

// printContactSummary prints a route search decision first: the routes found, then what
// was dropped or skipped.
func printContactSummary(w io.Writer, runID int64, dryRun bool, sum contactSummary) {
	mode := ""
	if dryRun {
		mode = " (dry run, fixtures)"
	}
	_, _ = fmt.Fprintf(w, "leads contacts: run %d%s · %d orgs checked\n", runID, mode, sum.Checked)
	_, _ = fmt.Fprintf(w, "  role emails found: %d (with MX %d · without %d)\n", sum.Emails, sum.EmailsMX, sum.Emails-sum.EmailsMX)
	_, _ = fmt.Fprintf(w, "  forms: %d\n", sum.Forms)
	_, _ = fmt.Fprintf(w, "  no route: %d\n", sum.NoRoute)
	_, _ = fmt.Fprintf(w, "  dropped: person-like %d · protected (not decoded) %d · foreign %d\n", sum.PersonLike, sum.Protected, sum.Foreign)
	_, _ = fmt.Fprintf(w, "  public bodies skipped: %d · robots-disallowed pages: %d · new orgs from probes: %d\n",
		sum.PublicBodies, sum.RobotsSkipped, sum.NewOrgs)
	_, _ = fmt.Fprintf(w, "  off-domain pages: %d · drupal.org orgs skipped: %d · cut short (still due): %d\n",
		sum.OffDomain, sum.DrupalOrgs, sum.CutShort)
	_, _ = fmt.Fprintf(w, "  %d requests\n", sum.Requests)
}

// defaultExportLimit is how many orgs `leads export routes` writes without --limit.
const defaultExportLimit = 100

// runLeadsExport is `leads export routes [--limit N]`: a CSV of the top orgs with a route,
// their one finding and their best route, at <data dir>/exports/routes-<date>.csv (mode
// 0600, never overwritten). Orgs on reserved TLDs are left out.
func runLeadsExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads export", flag.ContinueOnError)
	limit := fs.Int("limit", defaultExportLimit, "most orgs to write")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "routes" || *limit < 1 {
		return usageError("usage: leadhound leads export routes [--limit N (at least 1)]")
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	setup, err := leadsEnv(cfg, leadsForProbe)
	if err != nil {
		return err
	}
	s, zone, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	rows, err := leadstore.NewQueries(s.Pool()).ExportRoutes(ctx, *limit)
	if err != nil {
		return err
	}
	dir := filepath.Join(setup.dataDir, "exports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	path := filepath.Join(dir, "routes-"+time.Now().In(zone).Format("2006-01-02")+".csv")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- under the data dir
	if errors.Is(err, os.ErrExist) {
		return usageError("%s already exists; move it away first", path)
	}
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	werr := writeRoutesCSV(f, rows)
	if err := f.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		return fmt.Errorf("export: %s: %w", path, werr)
	}
	_, _ = fmt.Fprintf(stdout, "leads export routes: %s · %d rows\n", path, len(rows))
	return nil
}

// writeRoutesCSV writes the export's header and one line per row.
func writeRoutesCSV(w io.Writer, rows []leadstore.ExportRow) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"domain", "kind", "name", "country", "finding", "finding_url", "route_kind", "route", "mx_ok"})
	for _, r := range rows {
		_ = cw.Write([]string{
			r.Domain, r.Kind, cellText(r.Name), r.Country, cellText(exportFinding(r)), r.FindingURL,
			r.RouteKind, r.Route, strconv.FormatBool(r.MXOK),
		})
	}
	cw.Flush()
	return cw.Error()
}

// exportFinding is an agency's phrase, or a site's "D10 10.2.7" (the major alone when no
// exact version is known), or "".
func exportFinding(r leadstore.ExportRow) string {
	switch {
	case r.Phrase != "":
		return r.Phrase
	case r.Major > 0 && r.ExactVersion != "":
		return "D" + strconv.Itoa(r.Major) + " " + r.ExactVersion
	case r.Major > 0:
		return "D" + strconv.Itoa(r.Major)
	}
	return ""
}

// cellText keeps a spreadsheet from reading free text as a formula.
func cellText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
