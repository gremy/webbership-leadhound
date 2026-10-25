package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/agencies"
	"github.com/gremy/webbership-leadhound/internal/contacts"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["agencies"] = runLeadsAgencies }

// The run row of an agency import.
const (
	commandLeadsAgencies  = "leads-agencies-import"
	sourceAgencies        = "agencies"
	sourceAgenciesFixture = "agencies-fixture"
)

// agencyDerivedBy names the code that matched an agency finding.
const agencyDerivedBy = "agencies@1"

// agencyGap is the least time between two requests to one host, and agencyClock the clock
// the import waits on; tests replace both.
var (
	agencyGap               = 10 * time.Second
	agencyClock fetch.Clock = wallClock{}
)

// wallClock is the real time.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// hostPacer spaces the requests to one host at least gap apart, on clock. The caller
// waits before each Get, so the wait never counts against the client's timeout.
type hostPacer struct {
	clock fetch.Clock
	gap   time.Duration
	mu    sync.Mutex
	last  map[string]time.Time // host → when it was last asked
}

func newHostPacer(clock fetch.Clock, gap time.Duration) *hostPacer {
	return &hostPacer{clock: clock, gap: gap, last: map[string]time.Time{}}
}

// wait blocks until host may be asked again, then notes the request.
func (p *hostPacer) wait(ctx context.Context, host string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if at, ok := p.last[host]; ok {
		if d := at.Add(p.gap).Sub(p.clock.Now()); d > 0 {
			if err := p.clock.Sleep(ctx, d); err != nil {
				return err
			}
		}
	}
	p.last[host] = p.clock.Now()
	return nil
}

// mark notes host as asked now: the end of a Get, for the host a redirect ended on.
func (p *hostPacer) mark(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last[host] = p.clock.Now()
}

const leadsAgenciesUsage = "usage: leadhound leads agencies import [FILE] [--dry-run --fixtures DIR]"

// runLeadsAgencies is `leads agencies import [FILE] [--dry-run --fixtures DIR]`.
func runLeadsAgencies(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "import" {
		return usageError(leadsAgenciesUsage)
	}
	return runAgenciesImport(ctx, args[1:])
}

// runAgenciesImport reads the agency list (default <data dir>/agencies.yaml) and, for each
// agency with a website, makes its org and its site row (origin agency), reads its homepage
// and its evidence page, robots-checked, and records an agency finding when the finding
// pattern matches the evidence page. Agencies are read one at a time, with at least
// agencyGap between two requests to one host; an evidence page on drupal.org is never read.
// A dry run reads fixtures and imports only agencies on reserved TLDs.
func runAgenciesImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads agencies import", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageError(leadsAgenciesUsage)
	}
	if err := ff.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	setup, err := leadsEnv(cfg, leadsForProbe)
	if err != nil {
		return err
	}
	path := filepath.Join(setup.dataDir, agencies.FileName)
	if len(pos) == 1 {
		path = pos[0]
	}
	list, skipped, problems := agencies.LoadFile(path)
	if len(problems) > 0 {
		return usageError("%s: %s", path, strings.Join(problems, "; "))
	}
	pattern, err := regexp.Compile(setup.leads.AgencyFindingPattern)
	if err != nil {
		return usageError("agency_finding_pattern: %v", err)
	}
	newClient, err := siteClients(cfg, ff, siteLimiter(setup.env))
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "leads agencies import: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	if err != nil {
		return err
	}
	if ff.dryRun {
		kept := list[:0]
		for _, a := range list {
			if isReserved(a.Domain) {
				kept = append(kept, a)
			}
		}
		list = kept
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()

	source := sourceAgencies
	if ff.dryRun {
		source = sourceAgenciesFixture
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsAgencies, source)
	if err != nil {
		return err
	}
	ai := agencyImport{
		pool: s.Pool(), newClient: newClient, pattern: pattern,
		rec:   store.NewFetchRecorder(q, runID, source).SizeOnly(fetch.KindSite, fetch.KindRobots),
		pacer: newHostPacer(agencyClock, agencyGap),
	}
	sum := agencySummary{Agencies: len(list), Skipped: skipped}
	var runErr error
	for _, a := range list {
		if ctx.Err() != nil {
			break
		}
		if err := ai.one(ctx, a, &sum); err != nil {
			runErr = err
			break
		}
	}
	result := store.RunResult{
		Requests: sum.Requests, Failures: sum.Failures,
		Note: fmt.Sprintf("%d agencies, %d findings", sum.Agencies, sum.Findings),
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
	printAgencySummary(stdout, runID, ff.dryRun, sum)
	return ctx.Err()
}

// agencySummary is what one import did.
type agencySummary struct {
	Agencies       int // agencies with a website in the list (on a dry run, on reserved TLDs)
	Created        int // agency orgs created
	Clashes        int // domains already held by an org of another kind
	Findings       int // agency findings recorded in this run
	Known          int // agencies that already had a finding, not read again
	NoFinding      int
	Skipped        int // rows without a website
	DrupalOrgPages int // evidence pages on drupal.org, not read
	RobotsSkipped  int // pages robots.txt kept the import from
	OffDomain      int // homepages whose final URL left the agency's domain
	Requests       int
	Failures       int
}

// agencyImport holds what every agency's import shares.
type agencyImport struct {
	pool      *pgxpool.Pool
	newClient func(fetch.Recorder) *fetch.Client
	rec       *store.FetchRecorder
	pattern   *regexp.Regexp
	pacer     *hostPacer
}

// one imports one agency: at most two pages (the homepage and the evidence page), within
// the three an agency may cost. Only a store error is returned.
func (ai agencyImport) one(ctx context.Context, a agencies.Agency, sum *agencySummary) error {
	lq := leadstore.NewQueries(ai.pool)
	wctx := context.WithoutCancel(ctx) // store reads and writes never fail on a stop
	existing, err := lq.OrgByDomain(wctx, a.Domain)
	switch {
	case errors.Is(err, leadstore.ErrOrgNotFound):
	case err != nil:
		return err
	case existing.Kind != leadstore.OrgAgency:
		return ai.save(wctx, a, nil, nil, sum) // a clash: nothing is read
	default:
		has, err := lq.HasAgencyFinding(wctx, existing.ID)
		if err != nil {
			return err
		}
		if has {
			sum.Known++
			return ai.save(wctx, a, nil, nil, sum)
		}
	}

	pr := &pageRecorder{inner: ai.rec, bodies: map[int64][]byte{}}
	c := ai.newClient(pr)
	counter := countRequests(c)
	pf := pageFetcher{c: c, rec: pr, pacer: ai.pacer}
	hr := probe.NewHostRobotsFunc(pf.robots)

	var homeID *int64
	home, got := fetchHome(ctx, pf, hr, c.UserAgent, a.Website, a.Domain)
	ai.tally(got, sum)
	if got == homeOK && home.FetchID != 0 {
		homeID = &home.FetchID
	}
	var ev *evidencePage
	switch {
	case a.EvidenceURL == "" || ctx.Err() != nil:
	case agencies.IsDrupalOrg(a.EvidenceURL):
		sum.DrupalOrgPages++
	default:
		p, got := fetchHome(ctx, pf, hr, c.UserAgent, a.EvidenceURL, "")
		ai.tally(got, sum)
		if got == homeOK && p.FetchID != 0 {
			if phrase, ok := agencies.FindingPhrase(p.Body, ai.pattern); ok {
				ev = &evidencePage{page: p, phrase: phrase, body: pr.bodies[p.FetchID]}
			}
		}
	}
	sum.Requests += int(counter.n)
	sum.Failures += c.Breaker.Failures()
	if pr.err != nil {
		return pr.err
	}
	if ev == nil {
		sum.NoFinding++
	}
	return ai.save(wctx, a, homeID, ev, sum)
}

// tally counts why a page was not used.
func (agencyImport) tally(got homeResult, sum *agencySummary) {
	switch got {
	case homeRobots:
		sum.RobotsSkipped++
	case homeRefused:
		sum.DrupalOrgPages++ // a redirect that ended on drupal.org, or a not-public host
	case homeOffDomain:
		sum.OffDomain++
	}
}

// evidencePage is an agency page whose visible text matched the finding pattern.
type evidencePage struct {
	page   contacts.Page
	phrase string
	body   []byte // as recorded, for AttachFetchBody
}

// save writes one agency's org, its site row (and the org's link to it) and its finding,
// with the body of the finding's page, in one transaction. ctx carries no cancel.
func (ai agencyImport) save(ctx context.Context, a agencies.Agency, homeID *int64, ev *evidencePage, sum *agencySummary) error {
	tx, err := ai.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("agencies: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	lq, sq := leadstore.NewQueries(tx), store.NewQueries(tx)
	up, err := lq.UpsertAgency(ctx, leadstore.AgencyImport{Name: a.Name, Website: a.Website, Country: a.Country, Domain: a.Domain}, homeID)
	if err != nil {
		return err
	}
	if up.Created {
		sum.Created++
	}
	if up.Clash {
		sum.Clashes++
	}
	if _, err := lq.ImportSites(ctx, leadstore.OriginAgency, []leadstore.SiteImport{{Domain: a.Domain, Country: a.Country}}); err != nil {
		return err
	}
	if !up.Clash {
		siteID, err := lq.SiteIDByDomain(ctx, a.Domain)
		if err != nil {
			return err
		}
		if err := lq.OrgSetSite(ctx, up.OrgID, siteID); err != nil && !errors.Is(err, leadstore.ErrOrgSiteSet) {
			return err
		}
	}
	if ev != nil && !up.Clash {
		if _, err := lq.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{
			OrgID: up.OrgID, URL: ev.page.URL, Phrase: ev.phrase, FetchID: ev.page.FetchID, DerivedBy: agencyDerivedBy,
		}); err != nil {
			return err
		}
		if err := sq.AttachFetchBody(ctx, ev.page.FetchID, ev.body); err != nil {
			return err
		}
		sum.Findings++
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("agencies: commit: %w", err)
	}
	return nil
}

// printAgencySummary prints an import decision first.
func printAgencySummary(w io.Writer, runID int64, dryRun bool, sum agencySummary) {
	mode := ""
	if dryRun {
		mode = " (dry run, fixtures)"
	}
	findings := fmt.Sprintf("%d", sum.Findings)
	if sum.Known > 0 {
		findings += fmt.Sprintf(" (%d more already on file)", sum.Known)
	}
	_, _ = fmt.Fprintf(w, "leads agencies import: run %d%s · agencies %d · created %d · clashes %d · findings %s · no finding %d · "+
		"skipped without website %d · drupal.org pages skipped %d\n",
		runID, mode, sum.Agencies, sum.Created, sum.Clashes, findings, sum.NoFinding, sum.Skipped, sum.DrupalOrgPages)
	_, _ = fmt.Fprintf(w, "  robots-disallowed pages: %d · off-domain homepages: %d · %d requests\n", sum.RobotsSkipped, sum.OffDomain, sum.Requests)
}
