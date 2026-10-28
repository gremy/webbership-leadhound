package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/phones"
	"github.com/gremy/webbership-leadhound/internal/probe"
	"github.com/gremy/webbership-leadhound/internal/research"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["phones"] = runLeadsPhones }

const leadsPhonesUsage = "usage: leadhound leads phones screen export --register R --out FILE | screen import --register R FILE | " +
	"research --campaign SLUG [--max N]"

// commandLeadsPhonesResearch is the run row of a phone research run.
const commandLeadsPhonesResearch = "leads-phones-research"

// phoneResearchWindow is how long after a phone research of an org the next may run.
const phoneResearchWindow = 90 * 24 * time.Hour

// defaultPhonesResearchMax is how many orgs one phone research run takes without --max.
const defaultPhonesResearchMax = 10

// runLeadsPhones is `leads phones screen export|import | research`.
func runLeadsPhones(ctx context.Context, args []string) error {
	switch {
	case len(args) >= 2 && args[0] == "screen" && args[1] == "export":
		return runPhonesScreenExport(ctx, args[2:])
	case len(args) >= 2 && args[0] == "screen" && args[1] == "import":
		return runPhonesScreenImport(ctx, args[2:])
	case len(args) >= 1 && args[0] == "research":
		return runPhonesResearch(ctx, args[1:])
	}
	return usageError(leadsPhonesUsage)
}

// screenRegister is register code's entry in the country table, or a usage error naming the
// known ones.
func screenRegister(code string) (compliance.Register, error) {
	t := compliance.Default()
	reg, ok := t.Registers[strings.ToLower(strings.TrimSpace(code))]
	if !ok || reg.Code == "" {
		known := make([]string, 0, len(t.Registers))
		for c := range t.Registers {
			known = append(known, c)
		}
		slices.Sort(known)
		return compliance.Register{}, usageError("--register %q is not one of %s", code, strings.Join(known, ", "))
	}
	return reg, nil
}

// screeningStore is phones.ScreeningStore over leadstore's phones.go for the operator's
// account. Where the register's country calls landlines only (IE), the export leaves out
// every number not known to be a landline: a mobile is never called, so never screened.
type screeningStore struct {
	lq         leadstore.Queries
	table      *compliance.Table
	suppressed int // phone suppressions RecordScreening wrote
}

var _ phones.ScreeningStore = (*screeningStore)(nil)

func (s *screeningStore) RecordScreening(ctx context.Context, sc phones.Screening, source, actor string) error {
	r, err := s.lq.RecordScreening(ctx, leadstore.DefaultAccountID,
		leadstore.Screening{Number: sc.Number, Register: sc.Register, Result: sc.Result, CheckedAt: sc.CheckedAt}, source, actor)
	if err != nil {
		return err
	}
	s.suppressed += r.Suppressed
	return nil
}

func (s *screeningStore) LatestScreening(ctx context.Context, number, register string) (phones.Screening, bool, error) {
	r, ok, err := s.lq.LatestScreening(ctx, number, register)
	if err != nil || !ok {
		return phones.Screening{}, ok, err
	}
	return phones.Screening{Number: r.Number, Register: r.Register, Result: r.Result, CheckedAt: r.CheckedAt}, true, nil
}

func (s *screeningStore) ScreeningsMissingOrOlder(ctx context.Context, register, country string, before time.Time) ([]string, error) {
	nums, err := s.lq.ScreeningsMissingOrOlder(ctx, leadstore.DefaultAccountID, register, country, before)
	if err != nil {
		return nil, err
	}
	c, ok := s.table.Lookup(country)
	if !ok || !c.Call.LandlineOnly {
		return nums, nil
	}
	return slices.DeleteFunc(nums, func(n string) bool {
		return compliance.PhoneType(phones.E164(n, country)) != "landline"
	}), nil
}

// latestApproval is the latest approval of a batch not discarded; zero when none.
func latestApproval(ctx context.Context, db store.DBTX) (time.Time, error) {
	var at *time.Time
	if err := db.QueryRow(ctx,
		`SELECT max(approved_at) FROM outreach_batch WHERE approved_at IS NOT NULL AND discarded_at IS NULL`).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
}

// runPhonesScreenExport is `leads phones screen export --register R --out FILE`: the numbers
// of the register's country's open leads whose screening against R is missing or too old,
// one E.164 number per line, written to FILE (0600, never over an existing file, never inside
// the repository). A register with an age (TPS: 28 days) counts back from now; one with
// none is screened before each campaign, so it counts from the latest batch approval.
func runPhonesScreenExport(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("leads phones screen export", flag.ContinueOnError)
	regCode := fset.String("register", "", "the register: tps, ctps, dncm, ndd, dncr, rpo, robinson, gr_art11")
	out := fset.String("out", "", "the file to write (new; outside the repository)")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *regCode == "" || *out == "" {
		return usageError(leadsPhonesUsage)
	}
	reg, err := screenRegister(*regCode)
	if err != nil {
		return err
	}
	path, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if root, inside := insideRepo(path); inside {
		return usageError("--out %s is inside the leadhound repository at %s; the export holds phone numbers, so write it outside", path, root)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	now := leadsNow()
	start := now
	if reg.MaxAgeDays == 0 {
		at, err := latestApproval(ctx, s.Pool())
		if err != nil {
			return err
		}
		if !at.IsZero() {
			start = at
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator's own --out path, checked outside the repo
	if errors.Is(err, fs.ErrExist) {
		return usageError("%s already exists: choose a new file (an export never overwrites one)", path)
	}
	if err != nil {
		return err
	}
	st := &screeningStore{lq: leadstore.NewQueries(s.Pool()), table: compliance.Default()}
	sum, err := phones.Export(ctx, st, reg, start, now, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	line := fmt.Sprintf("Wrote %s to screen against %s to %s.", plural(sum.Written, "number", "numbers"), registerWords(reg), path)
	if sum.Undialable > 0 {
		line += fmt.Sprintf(" %s could not be put in international form and were left out.", plural(sum.Undialable, "number", "numbers"))
	}
	_, _ = fmt.Fprintln(stdout, line)
	_, _ = fmt.Fprintf(stdout, "Once screened: leadhound leads phones screen import --register %s RESULT_FILE\n", reg.Code)
	return nil
}

func registerWords(r compliance.Register) string {
	if r.Name != "" {
		return r.Name + " (" + strings.ToUpper(r.Code) + ")"
	}
	return strings.ToUpper(r.Code)
}

// runPhonesScreenImport is `leads phones screen import --register R FILE`: records each
// result of the register's file (phone_screening, append-only); a listed number gets a phone
// suppression through the store. Lines that do not parse are counted and named by number,
// never guessed and never printed.
func runPhonesScreenImport(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("leads phones screen import", flag.ContinueOnError)
	regCode := fset.String("register", "", "the register the file is from")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *regCode == "" {
		return usageError(leadsPhonesUsage)
	}
	reg, err := screenRegister(*regCode)
	if err != nil {
		return err
	}
	f, err := os.Open(pos[0])
	if err != nil {
		return usageError("cannot open %s: %v", pos[0], errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	now := leadsNow()
	sum, err := phones.ParseResults(f, reg, now, now)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	st := &screeningStore{lq: leadstore.NewQueries(s.Pool()), table: compliance.Default()}
	n, err := phones.Import(ctx, st, sum, filepath.Base(pos[0]), leadstore.ActorGremy)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, importWords(reg, sum, n, st.suppressed))
	return nil
}

// importWords is what an import recorded, counts and line numbers only.
func importWords(reg compliance.Register, sum phones.ImportSummary, recorded, suppressed int) string {
	listed := 0
	for _, s := range sum.Screenings {
		if s.Result == phones.Listed {
			listed++
		}
	}
	line := fmt.Sprintf("%s: recorded %s (%d clean, %d listed); %s.", registerWords(reg),
		plural(recorded, "screening", "screenings"), recorded-listed, listed,
		plural(suppressed, "new phone suppression", "new phone suppressions"))
	if sum.Refused > 0 {
		nums := make([]string, 0, len(sum.Lines))
		for _, l := range sum.Lines {
			nums = append(nums, strconv.Itoa(l))
		}
		line += fmt.Sprintf(" %s refused (line %s): open the file there; nothing was guessed.",
			plural(sum.Refused, "line", "lines"), strings.Join(nums, ", "))
	}
	return line
}

// callLeadSource is phones.CallLeadSource over the store: the leads of a campaign with a
// call step ahead (a call message drafted or approved, not yet done or cancelled), leaving
// out leads suppressed or deferred, leads in a country whose calls the table marks off, and
// orgs with a phone research inside phoneResearchWindow. Each lead's usable phones carry the
// line kind their research gave them.
type callLeadSource struct {
	db    store.DBTX
	lq    leadstore.Queries
	table *compliance.Table
	now   time.Time
}

func (s callLeadSource) CallLeadsAhead(ctx context.Context, campaignID int64) ([]phones.CallLead, error) {
	type row struct {
		LeadID  int64  `db:"lead_id"`
		OrgID   int64  `db:"org_id"`
		OrgName string `db:"org_name"`
		Domain  string `db:"domain"`
		Country string `db:"country"`
		Starred bool   `db:"starred"`
	}
	rows, err := s.db.Query(ctx,
		`SELECT l.id AS lead_id, o.id AS org_id, o.name AS org_name, o.domain, o.country,
		   coalesce((SELECT ls.action <> 'unstar' FROM lead_star ls WHERE ls.lead_id = l.id ORDER BY ls.id DESC LIMIT 1), false) AS starred
		 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.campaign_id = $1 AND l.state NOT IN ('suppressed', 'done')
		   AND EXISTS (SELECT 1 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		     WHERE m.lead_id = l.id AND m.channel = 'phone' AND m.state IN ('draft', 'approved') AND b.discarded_at IS NULL)
		   AND NOT coalesce((SELECT lp.action = 'defer' AND lp.until >= current_date FROM lead_priority lp
		     WHERE lp.lead_id = l.id ORDER BY lp.id DESC LIMIT 1), false)
		   AND NOT EXISTS (SELECT 1 FROM phone_research pr WHERE pr.org_id = o.id AND pr.at > $2)
		 ORDER BY (SELECT min(m.due_at) FROM outreach_message m WHERE m.lead_id = l.id AND m.channel = 'phone') NULLS LAST, l.id`,
		campaignID, s.now.Add(-phoneResearchWindow))
	if err != nil {
		return nil, err
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, err
	}
	var out []phones.CallLead
	var orgs []int64
	for _, r := range got {
		if c, ok := s.table.Lookup(r.Country); !ok || c.Call.Verdict == compliance.VerdictOff {
			continue
		}
		out = append(out, phones.CallLead{LeadID: r.LeadID, OrgID: r.OrgID, OrgName: r.OrgName, Domain: r.Domain, Country: r.Country, Starred: r.Starred})
		orgs = append(orgs, r.OrgID)
	}
	facts, err := s.lq.UsableOrgFacts(ctx, orgs)
	if err != nil {
		return nil, err
	}
	kinds, err := s.lineKinds(ctx, orgs)
	if err != nil {
		return nil, err
	}
	byOrg := map[int64][]phones.LeadPhone{}
	for _, f := range facts {
		if f.Kind == leadstore.FactPhone {
			byOrg[f.OrgID] = append(byOrg[f.OrgID], phones.LeadPhone{Normalized: f.Normalized, LineKind: kinds[f.ID]})
		}
	}
	for i := range out {
		out[i].Phones = byOrg[out[i].OrgID]
	}
	return out, nil
}

// lineKinds is the latest research line kind of each researched phone fact of orgs.
func (s callLeadSource) lineKinds(ctx context.Context, orgs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(orgs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT ON (fact_id) fact_id, line_kind FROM phone_research
		 WHERE org_id = ANY($1) AND fact_id IS NOT NULL ORDER BY fact_id, id DESC`, orgs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// runPhonesResearch is `leads phones research --campaign S [--max N]`: phone-only research
// (internal/phones over internal/research: one Claude call per org with web search and
// fetch, each number verified on the page it cites) for the campaign's leads with a call
// step ahead and no phone, or only a switchboard; starred first. Spend is capped by the
// research purpose cap and the per-lead cap; each org gets a phone_research row whether or
// not a number was found, so it is not asked again for 90 days. With live calls off it does
// nothing.
func runPhonesResearch(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("leads phones research", flag.ContinueOnError)
	slug := fset.String("campaign", "", "the campaign whose call leads to research")
	maxN := fset.Int("max", defaultPhonesResearchMax, "the most orgs to research (at least 1)")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *slug == "" || *maxN < 1 {
		return usageError(leadsPhonesUsage)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	if cfg.Live != config.LiveAll {
		_, _ = fmt.Fprintln(stdout, phonesLiveOffWords)
		return nil
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	camp, err := activeCampaign(ctx, lq, *slug)
	if err != nil {
		return err
	}
	q := s.Q()
	caller, err := newResearchCaller(q)
	if err != nil {
		return err
	}
	if caller == nil {
		_, _ = fmt.Fprintln(stdout, phonesLiveOffWords)
		return nil
	}
	newClient, err := researchClients(cfg, fetchFlags{}, siteLimiter(setup.env))
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintln(stdout, phonesLiveOffWords)
		return nil
	}
	if err != nil {
		return err
	}
	runID, err := q.StartRun(ctx, commandLeadsPhonesResearch, sourceResearch)
	if err != nil {
		return err
	}
	rr := researchRun{
		pool: s.Pool(), newClient: newClient, caller: caller, roles: setup.leads.RoleLocalParts,
		rec: store.NewFetchRecorder(q, runID, sourceResearch).SizeOnly(fetch.KindSite, fetch.KindRobots),
	}
	src := callLeadSource{db: s.Pool(), lq: lq, table: compliance.Default(), now: leadsNow()}
	var sum researchSummary
	var storeErr error
	researcher := func(ctx context.Context, req research.Request) (research.Result, error) {
		res, runErr := rr.phoneOne(ctx, req, &sum)
		if runErr == nil || errors.Is(runErr, errPhoneStore) {
			storeErr = errors.Join(storeErr, runErr)
		}
		return res, runErr
	}
	_, runErr := phones.Run(ctx, src, researcher, camp.ID, *maxN)
	runErr = errors.Join(runErr, storeErr)
	result := store.RunResult{Requests: sum.Requests, Failures: sum.Failures,
		Note: fmt.Sprintf("%d orgs phone-researched: %d found, %d nothing, %d capped, %d error; %s",
			sum.Orgs, sum.Found, sum.Nothing, sum.Capped, sum.Errors, researchUSD(sum.CostMicros))}
	if runErr != nil {
		result.Note += "; stopped: " + researchStatusOf(runErr, 0)
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, researchStoreError("finishing the run", err))
	}
	printResearchSummary(stdout, runID, sum)
	if errors.Is(runErr, errPhoneStore) {
		return runErr
	}
	return nil
}

// phonesLiveOffWords is what a phone research run says when it may not call Claude.
const phonesLiveOffWords = "Live Claude calls are off (LEADHOUND_LIVE=1 and the key file): no phone research ran."

// errPhoneStore marks a phone research run's store failure: the run stops on it.
var errPhoneStore = errors.New("leads phones research: the store failed")

// phoneOne researches one org's phones: research.Run, then the verified phones stored with
// the pages that back them (rr.store), then one phone_research row per verified phone (its
// line kind and role, never a name) or one with no fact when none was verified, so the org
// waits 90 days. The model's quote never leaves research.Run.
func (rr researchRun) phoneOne(ctx context.Context, req research.Request, sum *researchSummary) (research.Result, error) {
	wctx := context.WithoutCancel(ctx)
	pr := &pageRecorder{inner: rr.rec, bodies: map[int64][]byte{}}
	c := rr.newClient(pr)
	counter := countRequests(c)
	pf := pageFetcher{c: c, rec: pr}
	hr := probe.NewHostRobotsFunc(pf.robots)
	v := research.Verifier{Fetcher: pf, Robots: hr.For, Resolver: contactsResolver, Roles: rr.roles, UserAgent: c.UserAgent}
	req.Want = research.Want{Phone: true}
	res, runErr := research.Run(ctx, rr.caller, v, req)
	sum.Requests += int(counter.n)
	sum.Failures += c.Breaker.Failures()
	sum.Orgs++
	sum.CostMicros += res.CostMicros
	res.Routes = nil // phone research stores phones only
	_, found, storeErr := rr.store(wctx, res, pr)
	if storeErr == nil && pr.err != nil {
		storeErr = researchStoreError("recording a fetch", pr.err)
	}
	if storeErr == nil {
		storeErr = rr.recordPhoneResearch(wctx, req.Org.ID, res)
	}
	status := researchStatusOf(runErr, res.Verified)
	line := "  " + req.Org.Domain + ": "
	switch {
	case storeErr != nil:
		sum.Errors++
		line += "error: what was found could not be stored"
	case status == leadstore.ResearchFound:
		sum.Found++
		sum.Phones += found
		line += "found " + foundWords(0, found, res.Verified)
	case status == leadstore.ResearchNothing:
		sum.Nothing++
		line += "nothing"
	case status == leadstore.ResearchCapped:
		sum.Capped++
		line += "capped: the research spend cap is reached"
	default:
		sum.Errors++
		line += "error: " + runErr.Error()
	}
	line += " · " + researchUSD(res.CostMicros)
	sum.Lines = append(sum.Lines, line)
	if storeErr != nil {
		return res, fmt.Errorf("%w: %w", errPhoneStore, storeErr)
	}
	return res, runErr
}

// recordPhoneResearch writes org orgID's phone_research rows for res.
func (rr researchRun) recordPhoneResearch(ctx context.Context, orgID int64, res research.Result) error {
	lq := leadstore.NewQueries(rr.pool)
	model := res.Model
	if model == "" {
		model = research.Model
	}
	cost := res.CostMicros
	wrote := false
	for _, p := range res.Phones {
		var id int64
		err := rr.pool.QueryRow(ctx, `SELECT id FROM org_fact WHERE org_id = $1 AND kind = 'phone' AND normalized = $2`,
			orgID, p.Fact.Normalized).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return researchStoreError("reading a stored phone", err)
		}
		row := leadstore.PhoneResearchRow{OrgID: orgID, FactID: &id, LineKind: p.LineKind, Role: p.Role, Model: model}
		if !wrote {
			row.CostMicros = &cost // the call's cost on its first row only
		}
		if _, err := lq.RecordPhoneResearch(ctx, row); err != nil {
			return researchStoreError("recording the research", err)
		}
		wrote = true
	}
	if wrote {
		return nil
	}
	if _, err := lq.RecordPhoneResearch(ctx, leadstore.PhoneResearchRow{OrgID: orgID, Model: model, CostMicros: &cost}); err != nil {
		return researchStoreError("recording the research", err)
	}
	return nil
}
