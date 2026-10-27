package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/contacts"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["enrich"] = runLeadsEnrich }

// The run row of an enrichment.
const (
	commandLeadsEnrich  = "leads-enrich"
	sourceEnrich        = "enrich"
	sourceEnrichFixture = "enrich-fixture"
)

const (
	// defaultEnrichMax is how many orgs one enrichment takes without --max.
	defaultEnrichMax = 300
	// enrichStorageEvery is how many orgs an enrichment reads between two storage checks;
	// it checks before the first.
	enrichStorageEvery = 20
	// enrichMaxFetches is the most contact pages one org's enrichment fetches.
	enrichMaxFetches = 2
)

// enrichClients builds an enrichment's site clients; tests replace it to reach httptest.
var enrichClients = siteClients

const leadsEnrichUsage = "usage: leadhound leads enrich [--max N (at least 1)] [--dry-run --fixtures DIR]"

// runLeadsEnrich is `leads enrich [--max N] [--dry-run --fixtures DIR]`: for at most N orgs
// with a lead and no enrichment in the last 30 days, it reads phones, a description and
// social links from the org's kept pages, and, when none of them is a contact page, from at
// most two contact links of its homepage, fetched under the contact search's rules. A dry
// run reads fixtures and touches only orgs on reserved TLDs.
func runLeadsEnrich(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads enrich", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	maxOrgs := fs.Int("max", defaultEnrichMax, "most orgs to enrich in this run")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *maxOrgs < 1 {
		return usageError(leadsEnrichUsage)
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
	newClient, err := enrichClients(cfg, ff, siteLimiter(setup.env))
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "leads enrich: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())

	orgs, err := orgsToEnrich(ctx, lq, ff.dryRun, *maxOrgs)
	if err != nil {
		return err
	}
	source := sourceEnrich
	if ff.dryRun {
		source = sourceEnrichFixture
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsEnrich, source)
	if err != nil {
		return err
	}
	rec := store.NewFetchRecorder(q, runID, source).SizeOnly(fetch.KindSite, fetch.KindRobots)
	es := enrichRun{pool: s.Pool(), runID: runID, newClient: newClient, rec: rec}
	var sum enrichSummary
	stopStoring := storageCheck(cfg, q)
	var runErr error
	for i, o := range orgs {
		if ctx.Err() != nil {
			break
		}
		if i%enrichStorageEvery == 0 {
			over, why, err := stopStoring(ctx)
			if err != nil {
				runErr = fmt.Errorf("storage check: %w", err)
				break
			}
			if over {
				sum.StopReason, sum.StopNote = stopDiskBudget, why
				break
			}
		}
		if err := es.enrich(ctx, o, &sum); err != nil {
			runErr = err
			break
		}
	}
	result := store.RunResult{
		Requests: sum.Requests, Failures: sum.Failures, StopReason: sum.StopReason,
		Note: fmt.Sprintf("%d orgs enriched, %d new facts", sum.Orgs, sum.Phones+sum.Descriptions+sum.Social),
	}
	switch {
	case runErr != nil:
		result.Note += "; " + runErr.Error()
	case ctx.Err() != nil:
		result.Note += "; interrupted"
	case sum.StopReason != "":
		result.Note += "; stopped storing: " + sum.StopNote
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	printEnrichSummary(stdout, runID, ff.dryRun, sum)
	return ctx.Err()
}

// orgsToEnrich is the orgs due for enrichment, at most maxOrgs; on a dry run, only those on
// reserved TLDs.
func orgsToEnrich(ctx context.Context, lq leadstore.Queries, dryRun bool, maxOrgs int) ([]leadstore.Org, error) {
	limit := maxOrgs
	if dryRun {
		limit = 0 // filtered below, then capped
	}
	all, err := lq.OrgsNeedingEnrich(ctx, limit, time.Now())
	if err != nil {
		return nil, err
	}
	var out []leadstore.Org
	for _, o := range all {
		if dryRun && !isReserved(o.Domain) {
			continue
		}
		if len(out) < maxOrgs {
			out = append(out, o)
		}
	}
	return out, nil
}

// enrichSummary is what one enrichment did. Phones, Descriptions and Social count new facts.
type enrichSummary struct {
	Orgs          int // orgs enriched, an attempt recorded for each
	Phones        int
	Descriptions  int
	Social        int
	Fetched       int // contact pages fetched with a 2xx answer
	RobotsSkipped int // contact pages robots.txt kept the enrichment from
	CutShort      int // enrichments that did not complete: nothing recorded, still due
	Requests      int
	Failures      int
	StopReason    string // disk_budget when the storage check stopped the run
	StopNote      string
}

// enrichRun holds what every org's enrichment shares.
type enrichRun struct {
	pool      *pgxpool.Pool
	runID     int64
	newClient func(fetch.Recorder) *fetch.Client
	rec       *store.FetchRecorder
}

// enrichFact is one fact and the page it was read from.
type enrichFact struct {
	enrich.Fact
	fetchID int64
}

// enrich reads one org's facts. Its kept pages are read first; when none is a contact page,
// at most enrichMaxFetches contact links of its kept homepage are fetched through
// contacts.FetchPage. A completed enrichment stores its new facts, the bodies of the fetched
// pages that gave one and the attempt in one transaction; one cut short (no answer, a
// breaker stop, a cancel) stores nothing, so the org stays due. Only a store error is
// returned. The writes run without the run's cancel, so a stop never breaks a transaction.
func (es enrichRun) enrich(ctx context.Context, o leadstore.Org, sum *enrichSummary) error {
	wctx := context.WithoutCancel(ctx)
	bodies, err := leadstore.NewQueries(es.pool).EnrichBodies(wctx, o.ID)
	if err != nil {
		return err
	}
	var facts []enrichFact
	seen := map[string]bool{}
	add := func(fs []enrich.Fact, fetchID int64) {
		for _, f := range fs {
			key := f.Kind + "\x00" + f.Normalized
			if fetchID == 0 || seen[key] {
				continue
			}
			seen[key] = true
			facts = append(facts, enrichFact{Fact: f, fetchID: fetchID})
		}
	}
	var home *leadstore.EnrichBody
	readContact := false
	for i, b := range bodies {
		add(enrich.Extract(b.Body, b.URL, o.Domain, b.Contact), b.FetchID)
		switch {
		case b.Contact:
			readContact = true
		case home == nil:
			home = &bodies[i]
		}
	}

	var pr *pageRecorder
	if !readContact && home != nil {
		base, err := url.Parse(home.URL)
		var links []string
		if err == nil {
			links = contacts.ContactLinks(base, home.Body, o.Domain)
		}
		if len(links) > enrichMaxFetches {
			links = links[:enrichMaxFetches]
		}
		if len(links) > 0 {
			pr = &pageRecorder{inner: es.rec, bodies: map[int64][]byte{}}
			c := es.newClient(pr)
			counter := countRequests(c)
			pf := pageFetcher{c: c, rec: pr}
			hr := probe.NewHostRobotsFunc(pf.robots)
			org := contacts.Org{Domain: o.Domain, UserAgent: c.UserAgent}
			res := contacts.Routes{Complete: true}
			for _, link := range links {
				if res.Stopped != nil || ctx.Err() != nil {
					break
				}
				p, ok := contacts.FetchPage(ctx, pf, hr.For, org, link, &res)
				if ok {
					sum.Fetched++
					add(enrich.Extract(p.Body, p.URL, o.Domain, true), p.FetchID)
				}
			}
			sum.Requests += int(counter.n)
			sum.Failures += c.Breaker.Failures()
			sum.RobotsSkipped += res.RobotsSkipped
			if pr.err != nil {
				return pr.err
			}
			if !res.Complete || res.Stopped != nil || ctx.Err() != nil {
				sum.CutShort++
				return nil
			}
		}
	}

	tx, err := es.pool.Begin(wctx)
	if err != nil {
		return fmt.Errorf("enrich: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(wctx) }() // a no-op after Commit
	lq, sq := leadstore.NewQueries(tx), store.NewQueries(tx)
	cited := map[int64]bool{}
	for _, f := range facts {
		inserted, err := lq.InsertFact(wctx, leadstore.FactRow{
			OrgID: o.ID, Kind: f.Kind, Value: f.Value, Normalized: f.Normalized, FetchID: f.fetchID, DerivedBy: enrich.DerivedBy,
		})
		if err != nil {
			return err
		}
		if !inserted {
			continue // the org has it already, from an earlier page
		}
		cited[f.fetchID] = true
		switch f.Kind {
		case enrich.KindPhone:
			sum.Phones++
		case enrich.KindDescription:
			sum.Descriptions++
		case enrich.KindSocial:
			sum.Social++
		}
	}
	if pr != nil {
		for id, body := range pr.bodies {
			if !cited[id] {
				continue // a fetched page keeps its body only when it gave a new fact
			}
			if err := sq.AttachFetchBody(wctx, id, body); err != nil {
				return err
			}
		}
	}
	if err := lq.RecordEnrichAttempt(wctx, o.ID, es.runID, len(facts)); err != nil {
		return err
	}
	if err := tx.Commit(wctx); err != nil {
		return fmt.Errorf("enrich: commit: %w", err)
	}
	sum.Orgs++
	return nil
}

// printEnrichSummary prints the run's one line, then what was skipped.
func printEnrichSummary(w io.Writer, runID int64, dryRun bool, sum enrichSummary) {
	mode := ""
	if dryRun {
		mode = " (dry run, fixtures)"
	}
	stop := sum.StopReason
	if stop == "" {
		stop = "none"
	}
	_, _ = fmt.Fprintf(w, "leads enrich: run %d%s · orgs %d · phones %d · descriptions %d · social %d · requests %d · stop %s\n",
		runID, mode, sum.Orgs, sum.Phones, sum.Descriptions, sum.Social, sum.Requests, stop)
	_, _ = fmt.Fprintf(w, "  contact pages fetched: %d · robots-disallowed pages: %d · cut short (still due): %d\n",
		sum.Fetched, sum.RobotsSkipped, sum.CutShort)
	if sum.StopReason != "" {
		_, _ = fmt.Fprintf(w, "  stopped storing: %s\n", sum.StopNote)
	}
}
