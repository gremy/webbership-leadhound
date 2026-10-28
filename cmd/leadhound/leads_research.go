package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
	"github.com/gremy/webbership-leadhound/internal/research"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["research"] = runLeadsResearch }

// The run row of a research job.
const (
	commandLeadsResearch = "leads-research"
	sourceResearch       = "research"
)

// defaultResearchMax is how many queued attempts one research run takes without --max.
const defaultResearchMax = 10

// researchLiveOffWords is what a research run says when it may not call Claude: it claims
// nothing, so every queued attempt stays queued for a run that may.
const researchLiveOffWords = "Live Claude calls are off (LEADHOUND_LIVE=1 and the key file): nothing claimed."

const leadsResearchUsage = "usage: leadhound leads research request LEAD | list [--status S] [--limit N] | run [--max N (at least 1)]"

// researchClients builds a research run's site clients; tests replace it to reach httptest.
var researchClients = siteClients

// newResearchCaller builds the spend-capped Claude caller of a research run on q's usage
// rows; tests replace it with a fake. It runs only once the live check has passed.
var newResearchCaller = func(q store.Queries) (research.Caller, error) {
	c, err := claudeCapped("leads research run", q, claude.PurposeResearch, claudeCampaignCaps{})
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}

// runLeadsResearch is `leads research request|list|run`.
func runLeadsResearch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsResearchUsage)
	}
	switch args[0] {
	case "request":
		return runResearchRequest(ctx, args[1:])
	case "list":
		return runResearchList(ctx, args[1:])
	case "run":
		return runResearchRun(ctx, args[1:])
	}
	return usageError("unknown research subcommand %q; %s", args[0], leadsResearchUsage)
}

// parseResearchRequest reads `request LEAD`.
func parseResearchRequest(args []string) (int64, error) {
	fs := flag.NewFlagSet("leads research request", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError(leadsResearchUsage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id < 1 {
		return 0, usageError("%q is not a lead id; %s", pos[0], leadsResearchUsage)
	}
	return id, nil
}

// researchStatuses are the statuses `list --status` takes.
var researchStatuses = []string{
	leadstore.ResearchQueued, leadstore.ResearchLooking, leadstore.ResearchFound,
	leadstore.ResearchNothing, leadstore.ResearchCapped, leadstore.ResearchError,
}

// parseResearchList reads `list [--status S] [--limit N]`.
func parseResearchList(args []string) (leadstore.ResearchListOptions, error) {
	fs := flag.NewFlagSet("leads research list", flag.ContinueOnError)
	status := fs.String("status", "", "only attempts in this status: "+strings.Join(researchStatuses, ", "))
	limit := fs.Int("limit", 50, "most attempts to show")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return leadstore.ResearchListOptions{}, err
	}
	if len(pos) != 0 || *limit < 1 {
		return leadstore.ResearchListOptions{}, usageError(leadsResearchUsage)
	}
	if *status != "" && !slices.Contains(researchStatuses, *status) {
		return leadstore.ResearchListOptions{}, usageError("--status %q is not one of %s", *status, strings.Join(researchStatuses, ", "))
	}
	return leadstore.ResearchListOptions{Status: *status, Limit: *limit}, nil
}

// parseResearchRun reads `run [--max N]`.
func parseResearchRun(args []string) (int, error) {
	fs := flag.NewFlagSet("leads research run", flag.ContinueOnError)
	maxRows := fs.Int("max", defaultResearchMax, "most queued attempts to run")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 || *maxRows < 1 {
		return 0, usageError(leadsResearchUsage)
	}
	return *maxRows, nil
}

// ResearchAsk is what asking for one lead's research did: the attempt queued (Created) or
// already there (ID set, Created false), or, when NoRequest is set, why none was asked for,
// in plain words. Domain and Want name the org and what is looked for; CallFirst says a
// phone is looked for because the lead is called before any email.
type ResearchAsk struct {
	ID        int64
	Created   bool
	Domain    string
	Want      []string
	CallFirst bool
	NoRequest string
}

// requestLeadResearch asks research for lead leadID's org by trigger (star, button or cli)
// as the operator, looking for what researchRules.forLead gives at now (the lead's campaign's
// countries and plan, leads.yaml's country rule, the law table for a phone, and what the lead
// already has: its channel, and whether its org has a phone a call could dial): the star's
// note and the lead page's "Look again" read the same decision. A country the campaign does
// not list, one that is off, a phone the law or the campaign gives no call for, a lead that
// already has what would be looked for, a lead past new, suppressed or deferred, and
// (button, cli) an attempt within 90 days are a NoRequest in plain words, never an error.
// The CLI and the console share it. A lead that does not exist is leadstore.ErrLeadNotFound.
func requestLeadResearch(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, leadID int64, trigger string, now time.Time) (ResearchAsk, error) {
	l, err := lq.LeadByID(ctx, leadID)
	if err != nil {
		return ResearchAsk{}, err
	}
	o, err := lq.OrgByID(ctx, l.OrgID)
	if err != nil {
		return ResearchAsk{}, err
	}
	ask := ResearchAsk{Domain: o.Domain}
	rr, err := loadResearchRules(ctx, lq, cfg, now)
	if err != nil {
		return ResearchAsk{}, err
	}
	if rr, err = rr.withPhones(ctx, lq, []int64{o.ID}); err != nil {
		return ResearchAsk{}, err
	}
	v := rr.forLead(researchLead{Channel: l.Channel, CampaignID: l.CampaignID, Country: o.Country, Phone: rr.phones[o.ID]}, trigger)
	if ask.Want, ask.CallFirst = v.Want, v.CallFirst; ask.Want == nil {
		ask.NoRequest = v.NoRequest
		return ask, nil
	}
	ask.ID, ask.Created, err = lq.RequestResearch(ctx, leadID, trigger, ask.Want, leadstore.ActorOperator)
	var soon *leadstore.ResearchTooSoonError
	switch {
	case errors.As(err, &soon):
		ask.ID, ask.NoRequest = 0, fmt.Sprintf("research last ran on %s; it may run again from %s",
			plainDate(soon.StartedAt), plainDate(soon.NextAt))
		return ask, nil
	case errors.Is(err, leadstore.ErrResearchBlocked):
		ask.NoRequest = "the lead is no longer new, or it is suppressed or deferred"
		return ask, nil
	case err != nil:
		return ResearchAsk{}, err
	case trigger == leadstore.ResearchStar && ask.ID == 0:
		ask.NoRequest = "the lead is no longer new, or it is suppressed or deferred"
	}
	return ask, nil
}

// countryWords names a country code in a sentence; "" is "an unknown country".
func countryWords(code string) string {
	if code == "" {
		return "an unknown country"
	}
	return code
}

// plainDate is a date in plain words, "28 Oct 2026".
func plainDate(t time.Time) string { return t.Format("2 Jan 2006") }

// wantWords is what an attempt looks for, in plain words.
func wantWords(want []string) string {
	var w []string
	for _, x := range want {
		switch x {
		case leadstore.WantEmail:
			w = append(w, "a role email")
		case leadstore.WantPhone:
			w = append(w, "a business phone")
		}
	}
	if len(w) == 0 {
		return "nothing"
	}
	return strings.Join(w, " and ")
}

// runResearchRequest is `leads research request LEAD`.
func runResearchRequest(ctx context.Context, args []string) error {
	leadID, err := parseResearchRequest(args)
	if err != nil {
		return err
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	ask, err := requestLeadResearch(ctx, leadstore.NewQueries(s.Pool()), setup.leads, leadID, leadstore.ResearchCLI, leadsNow())
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return usageError("lead %d does not exist", leadID)
	}
	if err != nil {
		return err
	}
	switch {
	case ask.NoRequest != "":
		_, _ = fmt.Fprintf(stdout, "no request: %s: %s\n", ask.Domain, ask.NoRequest)
	case ask.Created:
		_, _ = fmt.Fprintf(stdout, "Research %d queued for %s: it looks for %s at the next research run.\n", ask.ID, ask.Domain, wantWords(ask.Want))
	default:
		_, _ = fmt.Fprintf(stdout, "Research %d for %s is already queued or running; nothing added.\n", ask.ID, ask.Domain)
	}
	return nil
}

// runResearchList is `leads research list [--status S] [--limit N]`: attempts newest first,
// with the org's domain and counts, never a found value.
func runResearchList(ctx context.Context, args []string) error {
	opts, err := parseResearchList(args)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	rows, err := leadstore.NewQueries(s.Pool()).ResearchList(ctx, opts)
	if err != nil {
		return err
	}
	printResearchList(stdout, rows)
	return nil
}

func printResearchList(w io.Writer, rows []leadstore.Research) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "No research attempts.")
		return
	}
	for _, r := range rows {
		line := fmt.Sprintf("%d. %s · lead %d · %s · looks for %s · asked by %s %s",
			r.ID, r.Domain, r.LeadID, r.Status, wantWords(r.Want), r.Trigger, plainDate(r.RequestedAt))
		if r.FinishedAt != nil {
			line += fmt.Sprintf(" · %d of %d candidates verified", r.Verified, r.Candidates)
			if r.CostMicros != nil {
				line += " · " + researchUSD(*r.CostMicros)
			}
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// researchUSD words micro-dollars as dollars to the hundredth of a cent, "$0.0123".
func researchUSD(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

// researchStatusOf maps a research run to the status its attempt finishes with: a spend-cap
// refusal is capped, any other error is error, nothing verified is nothing, else found.
func researchStatusOf(err error, verified int) string {
	switch {
	case errors.Is(err, ai.ErrSpendCap):
		return leadstore.ResearchCapped
	case err != nil:
		return leadstore.ResearchError
	case verified == 0:
		return leadstore.ResearchNothing
	}
	return leadstore.ResearchFound
}

// researchSummary is what one research run did.
type researchSummary struct {
	Orgs       int
	Found      int
	Nothing    int
	Capped     int
	Errors     int
	Emails     int // role email routes stored
	Phones     int // phone facts stored
	CostMicros int64
	Requests   int
	Failures   int
	Lines      []string // one per org, plain words, never a found value
}

// researchRun holds what every attempt of a run shares.
type researchRun struct {
	pool      *pgxpool.Pool
	newClient func(fetch.Recorder) *fetch.Client
	rec       *store.FetchRecorder
	caller    research.Caller
	roles     []string
}

// runResearchRun is `leads research run [--max N]`: it claims queued attempts one at a time,
// starred leads first, oldest first, and runs each through internal/research with the
// spend-capped Claude caller and the contact finder's own fetch rules. With live calls off it
// claims nothing.
func runResearchRun(ctx context.Context, args []string) error {
	maxRows, err := parseResearchRun(args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	if cfg.Live != config.LiveAll {
		_, _ = fmt.Fprintln(stdout, researchLiveOffWords)
		return nil
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()
	caller, err := newResearchCaller(q)
	if err != nil {
		return err
	}
	if caller == nil {
		_, _ = fmt.Fprintln(stdout, researchLiveOffWords)
		return nil
	}
	newClient, err := researchClients(cfg, fetchFlags{}, siteLimiter(setup.env))
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintln(stdout, researchLiveOffWords)
		return nil
	}
	if err != nil {
		return err
	}
	runID, err := q.StartRun(ctx, commandLeadsResearch, sourceResearch)
	if err != nil {
		return err
	}
	rr := researchRun{
		pool: s.Pool(), newClient: newClient, caller: caller, roles: setup.leads.RoleLocalParts,
		rec: store.NewFetchRecorder(q, runID, sourceResearch).SizeOnly(fetch.KindSite, fetch.KindRobots),
	}
	lq := leadstore.NewQueries(s.Pool())
	var sum researchSummary
	var runErr error
	for range maxRows {
		if ctx.Err() != nil {
			break
		}
		rows, err := lq.ClaimResearch(ctx, 1)
		if err != nil {
			runErr = researchStoreError("claiming an attempt", err)
			break
		}
		if len(rows) == 0 {
			break
		}
		if err := rr.one(ctx, rows[0], &sum); err != nil {
			runErr = err
			break
		}
	}
	result := store.RunResult{
		Requests: sum.Requests, Failures: sum.Failures,
		Note: fmt.Sprintf("%d orgs researched: %d found, %d nothing, %d capped, %d error; %s",
			sum.Orgs, sum.Found, sum.Nothing, sum.Capped, sum.Errors, researchUSD(sum.CostMicros)),
	}
	switch {
	case runErr != nil:
		result.Note += "; " + runErr.Error()
	case ctx.Err() != nil:
		result.Note += "; interrupted"
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, researchStoreError("finishing the run", err))
	}
	printResearchSummary(stdout, runID, sum)
	if runErr != nil {
		return runErr
	}
	return ctx.Err()
}

// one runs claimed attempt row: research.Run, then the verified routes and phones stored
// with the bodies of the pages that back them (one transaction), then the attempt finished.
// What was verified is stored even when the run ended in an error (a fetch stop after some
// candidates); a store error finishes the attempt as error and is returned. The writes run
// without the run's cancel.
func (rr researchRun) one(ctx context.Context, row leadstore.Research, sum *researchSummary) error {
	wctx := context.WithoutCancel(ctx)
	pr := &pageRecorder{inner: rr.rec, bodies: map[int64][]byte{}}
	c := rr.newClient(pr)
	counter := countRequests(c)
	pf := pageFetcher{c: c, rec: pr}
	hr := probe.NewHostRobotsFunc(pf.robots)
	v := research.Verifier{Fetcher: pf, Robots: hr.For, Resolver: contactsResolver, Roles: rr.roles, UserAgent: c.UserAgent}
	req := research.Request{
		LeadID: row.LeadID,
		Org:    research.Org{ID: row.OrgID, Name: row.OrgName, Domain: row.Domain, Country: row.Country},
		Want:   research.Want{Email: slices.Contains(row.Want, leadstore.WantEmail), Phone: slices.Contains(row.Want, leadstore.WantPhone)},
	}
	res, runErr := research.Run(ctx, rr.caller, v, req)
	sum.Requests += int(counter.n)
	sum.Failures += c.Breaker.Failures()
	sum.Orgs++
	sum.CostMicros += res.CostMicros

	emails, phones, storeErr := rr.store(wctx, res, pr)
	if storeErr == nil && pr.err != nil {
		storeErr = researchStoreError("recording a fetch", pr.err)
	}
	sum.Emails += emails
	sum.Phones += phones
	status := researchStatusOf(runErr, res.Verified)
	if storeErr != nil {
		status = leadstore.ResearchError
	}
	verified := min(res.Verified, res.Candidates)
	if status == leadstore.ResearchNothing || (status == leadstore.ResearchError && storeErr != nil) {
		verified = 0
	}
	cost := res.CostMicros
	model := res.Model
	if model == "" {
		model = research.Model
	}
	finishErr := leadstore.NewQueries(rr.pool).FinishResearch(wctx, row.ID, status, model, &cost, res.Candidates, verified)
	if finishErr != nil {
		finishErr = researchStoreError(fmt.Sprintf("finishing attempt %d", row.ID), finishErr)
	}

	line := "  " + row.Domain + ": "
	switch status {
	case leadstore.ResearchFound:
		sum.Found++
		line += "found " + foundWords(emails, phones, res.Verified)
	case leadstore.ResearchNothing:
		sum.Nothing++
		line += "nothing"
	case leadstore.ResearchCapped:
		sum.Capped++
		line += "capped: the research spend cap is reached; it may be asked again"
	default:
		sum.Errors++
		if storeErr != nil {
			line += "error: what was found could not be stored" // the store's error can name a value
		} else {
			line += "error: " + runErr.Error()
		}
	}
	if res.Candidates > 0 {
		line += fmt.Sprintf(" (%d of %d candidates verified)", res.Verified, res.Candidates)
	}
	line += " · " + researchUSD(res.CostMicros)
	sum.Lines = append(sum.Lines, line)
	return errors.Join(storeErr, finishErr)
}

// foundWords is what a found attempt stored, e.g. "1 email" or "1 email and 1 phone"; a
// verified value the org already had counts as found and reads "nothing new".
func foundWords(emails, phones, verified int) string {
	var w []string
	if emails > 0 {
		w = append(w, plural(emails, "email", "emails"))
	}
	if phones > 0 {
		w = append(w, plural(phones, "phone", "phones"))
	}
	if len(w) == 0 {
		return plural(verified, "contact", "contacts") + ", nothing new"
	}
	return strings.Join(w, " and ")
}

// researchStoreSentinels are the store errors researchStoreCause names by their own name.
var researchStoreSentinels = []struct {
	err  error
	name string
}{
	{leadstore.ErrUnknownRouteKind, "ErrUnknownRouteKind"},
	{leadstore.ErrEmptyRoute, "ErrEmptyRoute"},
	{leadstore.ErrUnknownFactKind, "ErrUnknownFactKind"},
	{leadstore.ErrEmptyFact, "ErrEmptyFact"},
	{leadstore.ErrFactTooLong, "ErrFactTooLong"},
	{leadstore.ErrResearchNotLooking, "ErrResearchNotLooking"},
	{leadstore.ErrBadResearch, "ErrBadResearch"},
	{store.ErrFetchBodyMismatch, "ErrFetchBodyMismatch"},
	{pgx.ErrTxClosed, "ErrTxClosed"},
	{context.Canceled, "context canceled"},
	{context.DeadlineExceeded, "context deadline exceeded"},
}

// researchStoreCause names err without its text: a Postgres error by its SQLSTATE (and
// constraint, a schema name), a known store error by its sentinel's name, anything else
// "unclassified". A store error's text can hold a found value: a Postgres message can quote
// its input (22P02 invalid input syntax, a RAISE in a trigger or function), a wrap can
// format its argument, and the run's note keeps what is printed.
func researchStoreCause(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if pg.ConstraintName != "" {
			return "SQLSTATE " + pg.Code + " on " + pg.ConstraintName
		}
		return "SQLSTATE " + pg.Code
	}
	for _, s := range researchStoreSentinels {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	return "unclassified"
}

// researchStoreError is a store error of a research run in fixed words plus its
// researchStoreCause, never err's own text, which can name a found address or phone. It
// keeps no link to err, so no caller can print that text by unwrapping it.
func researchStoreError(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("leads research run: the store failed %s (%s); its own words are not shown, they can name a found contact",
		what, researchStoreCause(err))
}

// store writes res's verified routes and phone facts and attaches the bodies of the pages
// they cite, in one transaction. It returns how many routes and facts were new. Its error
// is a researchStoreError, never the store's own text.
func (rr researchRun) store(ctx context.Context, res research.Result, pr *pageRecorder) (emails, phones int, err error) {
	if len(res.Routes) == 0 && len(res.Phones) == 0 {
		return 0, 0, nil
	}
	tx, err := rr.pool.Begin(ctx)
	if err != nil {
		return 0, 0, researchStoreError("beginning the write", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	lq, sq := leadstore.NewQueries(tx), store.NewQueries(tx)
	cited := map[int64]bool{}
	for _, r := range res.Routes {
		ok, err := lq.InsertRoute(ctx, r)
		if err != nil {
			return 0, 0, researchStoreError("storing a route", err)
		}
		cited[r.FetchID] = true
		if ok {
			emails++
		}
	}
	for _, p := range res.Phones {
		ok, err := lq.InsertFact(ctx, p.Fact)
		if err != nil {
			return 0, 0, researchStoreError("storing a phone", err)
		}
		cited[p.Fact.FetchID] = true
		if ok {
			phones++
		}
	}
	for id := range cited {
		body, ok := pr.bodies[id]
		if !ok {
			continue
		}
		if err := sq.AttachFetchBody(ctx, id, body); err != nil {
			return 0, 0, researchStoreError(fmt.Sprintf("keeping the body of fetch %d", id), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, researchStoreError("committing the write", err)
	}
	return emails, phones, nil
}

// printResearchSummary prints the run's line, then one line per org.
func printResearchSummary(w io.Writer, runID int64, sum researchSummary) {
	_, _ = fmt.Fprintf(w, "leads research run: run %d · %s · found %d · nothing %d · capped %d · error %d · cost %s\n",
		runID, plural(sum.Orgs, "org", "orgs"), sum.Found, sum.Nothing, sum.Capped, sum.Errors, researchUSD(sum.CostMicros))
	if sum.Orgs == 0 {
		_, _ = fmt.Fprintln(w, "  nothing was queued")
	}
	for _, l := range sum.Lines {
		_, _ = fmt.Fprintln(w, l)
	}
	if sum.Emails+sum.Phones > 0 {
		_, _ = fmt.Fprintf(w, "  stored: %d new emails, %d new phones; the next leads build gives a lead its channel\n", sum.Emails, sum.Phones)
	}
}
