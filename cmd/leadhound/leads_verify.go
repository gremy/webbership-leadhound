package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/verify"
)

// The email check before sending (Reoon, internal/verify): `leads verify` checks the email
// addresses the next batches would draft and stores each answer (email_check); the gate in
// newBatch keeps every address without a sendable fresh answer out of a live batch while the
// Email check job is on. Switched off, batches draft as they did before the check existed.

func init() { leadsCommands["verify"] = runLeadsVerify }

// commandLeadsVerify is the run row of a check run; its source is verify.Provider.
const commandLeadsVerify = "leads-verify"

const (
	// defaultVerifyMax is the most checks one run makes without --max.
	defaultVerifyMax = 60
	// verifyParallel is how many checks run at once (Reoon allows 5 on its single endpoint).
	verifyParallel = 2
	// defaultReoonDailyCap is LEADHOUND_REOON_DAILY_CAP's default: the most checks a day,
	// counted from the day's email_check rows.
	defaultReoonDailyCap = 200
	// reoonDailyCapEnv names the daily cap's setting.
	reoonDailyCapEnv = "LEADHOUND_REOON_DAILY_CAP"
)

// noCreditsLine is what a run says, and does, when Reoon has no credit of either kind left.
const noCreditsLine = "No Reoon credits left today." //nolint:gosec // G101: a sentence, not a credential

// noReoonKeyNote is the run note of a check run that found no key.
const noReoonKeyNote = "no Reoon key"

// emailCheckOffLine is what Controls and a batch say while the Email check job is off.
const emailCheckOffLine = "Email check is off: addresses are not checked before sending."

const leadsVerifyUsage = "usage: leadhound leads verify [--max N (at least 1)]"

// reoonClient is what leads verify asks Reoon through; tests swap newReoonClient for a fake,
// so no test reaches Reoon.
type reoonClient interface {
	Check(ctx context.Context, address string) (verify.Result, error)
	Balance(ctx context.Context) (verify.Balance, error)
}

// newReoonClient is the real client: the key from the batch unit's credential (or the key
// file for a hand run), live when LEADHOUND_LIVE allows outbound calls.
var newReoonClient = func(live bool) (reoonClient, error) {
	key, err := verify.LoadKey(os.Getenv)
	if err != nil {
		return nil, err
	}
	return verify.New(key, live), nil
}

// reoonDailyCap reads LEADHOUND_REOON_DAILY_CAP: a whole number of at least 0 (0 checks
// nothing), defaultReoonDailyCap when unset.
func reoonDailyCap(getenv func(string) string) (int, error) {
	v := strings.TrimSpace(getenv(reoonDailyCapEnv))
	if v == "" {
		return defaultReoonDailyCap, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, usageError("%s must be a whole number of 0 or more, not %q", reoonDailyCapEnv, v)
	}
	return n, nil
}

// The gate's answers for one lead.
const (
	gateSend = iota // draft it (on its own route, or on the route it fell to)
	gateSkip        // every email route is blocked and it has no other route: not drafted
	gateWait        // its address has no sendable answer yet: left for a later batch
)

// gateResult is the gate's answer for one lead: what to do, the status a skip rests on, the
// address a check is due for now ("" when none), and whether the lead fell to another route.
type gateResult struct {
	kind   int
	status string
	need   string
	fell   bool
}

// emailGate is the email check as a batch (and leads verify) applies it: on while the verify
// job is on and the batch is live. It reads each address's fresh answers once.
type emailGate struct {
	on       bool
	catchAll string // verify.catch_all in force
	now      time.Time
	dayStart time.Time // the start of the operator's day, for the next-day recheck of an unknown
	checks   map[string][]verify.Check
	supp     *outreach.SuppressionSet
}

// loadEmailGate is the gate for a batch drafted now with bs: off for a shadow batch or while
// the verify job is switched off.
func loadEmailGate(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time) (*emailGate, error) {
	g := &emailGate{catchAll: bs.leads.Verify.CatchAllRule(), now: now, dayStart: startOfDay(now, bs.loc), checks: map[string][]verify.Check{}}
	if bs.shadow {
		return g, nil
	}
	sw, err := lq.JobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobVerify)
	if err != nil {
		return nil, err
	}
	g.on = sw.On
	return g, nil
}

// load reads the fresh answers of every address in addrs not read yet.
func (g *emailGate) load(ctx context.Context, lq leadstore.Queries, addrs ...string) error {
	var want []string
	for _, a := range addrs {
		a = strings.ToLower(strings.TrimSpace(a))
		if _, ok := g.checks[a]; !ok && a != "" && !slices.Contains(want, a) {
			want = append(want, a)
		}
	}
	if len(want) == 0 {
		return nil
	}
	got, err := lq.EmailChecks(ctx, leadstore.DefaultAccountID, want, g.now.Add(-verify.FreshFor))
	if err != nil {
		return err
	}
	for _, a := range want {
		hs := make([]verify.Check, 0, len(got[a]))
		for _, r := range got[a] {
			hs = append(hs, verify.Check{Status: r.Status, CheckedAt: r.CheckedAt})
		}
		g.checks[a] = hs
	}
	return nil
}

// verdict is addr's verdict over the answers read (load first).
func (g *emailGate) verdict(addr string) verify.Verdict {
	return verify.Decide(g.checks[strings.ToLower(strings.TrimSpace(addr))], g.catchAll, g.now, g.dayStart)
}

// route applies the email check to candidate c, whose country's channel policy is policy: a
// lead whose route is not an email goes as it is. A sendable address goes. An address with no
// answer yet, or a first unknown, waits (need names it when a check is due now). A blocked
// address makes the lead fall to its next route, as a missing email does at build time
// (outreach.PickChannel over its routes without the blocked addresses): another email route,
// judged the same way, or a form; with none it is skipped with the first blocked status. An
// email route the lead falls to must be on its org's own domain or its first address's, and
// not suppressed, so the queue's suppression and reached-domain rules (CandidateLeads) still
// hold for it.
func (g *emailGate) route(ctx context.Context, lq leadstore.Queries, policy string, c leadstore.Candidate) (leadstore.Candidate, gateResult, error) {
	if c.Channel != leadconfig.ChannelEmail || c.RouteKind != leadstore.RouteEmail || c.Route == "" {
		return c, gateResult{kind: gateSend}, nil
	}
	blocked := map[string]bool{}
	first := ""
	cur, fell := c, false
	var routes []outreach.Route
	for {
		if err := g.load(ctx, lq, cur.Route); err != nil {
			return c, gateResult{}, err
		}
		v := g.verdict(cur.Route)
		switch v.Decision {
		case verify.Send:
			return cur, gateResult{kind: gateSend, fell: fell}, nil
		case verify.Unchecked, verify.Wait:
			res := gateResult{kind: gateWait}
			if v.Recheck {
				res.need = strings.ToLower(strings.TrimSpace(cur.Route))
			}
			return cur, res, nil
		}
		blocked[strings.ToLower(strings.TrimSpace(cur.Route))] = true
		if first == "" {
			first = v.Status
		}
		if routes == nil {
			var err error
			if routes, err = g.fallbackRoutes(ctx, lq, c); err != nil {
				return c, gateResult{}, err
			}
		}
		left := slices.DeleteFunc(slices.Clone(routes), func(r outreach.Route) bool {
			return r.Kind == outreach.RouteEmail && blocked[strings.ToLower(strings.TrimSpace(r.Value))]
		})
		ch, id := outreach.PickChannel(policy, left)
		if id == nil {
			return c, gateResult{kind: gateSkip, status: first}, nil
		}
		i := slices.IndexFunc(left, func(r outreach.Route) bool { return r.ID == *id })
		cur.Channel, cur.RouteID, cur.Route = ch, id, left[i].Value
		cur.RouteKind, fell = left[i].Kind, true
		if ch != leadconfig.ChannelEmail {
			return cur, gateResult{kind: gateSend, fell: true}, nil
		}
	}
}

// fallbackRoutes are org c.OrgID's routes a blocked lead may fall to: every form route, and
// every email route on the org's domain or on the domain of c's own address that no
// suppression covers.
func (g *emailGate) fallbackRoutes(ctx context.Context, lq leadstore.Queries, c leadstore.Candidate) ([]outreach.Route, error) {
	stored, err := lq.RoutesFor(ctx, c.OrgID)
	if err != nil {
		return nil, err
	}
	if g.supp == nil {
		s, err := lq.SuppressionSet(ctx, leadstore.DefaultAccountID)
		if err != nil {
			return nil, err
		}
		set := outreach.NewSuppressionSet(s.Addresses, s.Domains)
		g.supp = &set
	}
	domains := []string{strings.ToLower(c.Domain), recipientDomain(strings.ToLower(c.Route))}
	out := []outreach.Route{}
	for _, r := range stored {
		if r.Kind == leadstore.RouteEmail {
			d := recipientDomain(strings.ToLower(r.Value))
			if !slices.Contains(domains, d) || outreach.Suppressed(r.Value, c.Domain, *g.supp) {
				continue
			}
		}
		out = append(out, outreach.Route{ID: r.ID, Kind: r.Kind, Value: r.Value, MXOK: r.MXOK})
	}
	return out, nil
}

// verifyOutcome is what one leads verify run did.
type verifyOutcome struct {
	Checked, Sendable, Blocked, Waiting, Failed int
	Balance                                     verify.Balance
	BalanceRead                                 bool
}

// note is the run's note: its counts and the credits left after it.
func (o verifyOutcome) note() string {
	n := fmt.Sprintf("checked %d · sendable %d · blocked %d · waiting %d", o.Checked, o.Sendable, o.Blocked, o.Waiting)
	if o.Failed > 0 {
		n += fmt.Sprintf(" · %d failed", o.Failed)
	}
	if o.BalanceRead {
		n += fmt.Sprintf(" · credits left %d today, %d one-off", o.Balance.Daily, o.Balance.Instant)
	}
	return n
}

// runLeadsVerify is `leads verify [--max 60]`: it honours the Email check switch, refuses
// without LEADHOUND_LIVE (no call made, exit 0), reads the credits first and stops when none
// is left, then checks the email addresses the next live batches would draft (verifyQueue),
// at most --max, the day's cap (LEADHOUND_REOON_DAILY_CAP, counted from today's checks) and
// the credits left, two at a time, storing each answer. A missing key is said and exits 0, so
// the batch unit goes on to draft (a smaller batch). Every run that reached Reoon is recorded
// with its counts and the credits left.
func runLeadsVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads verify", flag.ContinueOnError)
	maxChecks := fs.Int("max", defaultVerifyMax, "most addresses checked in this run")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *maxChecks < 1 {
		return usageError(leadsVerifyUsage)
	}
	s, setup, err := leadsOpen(ctx, leadsForOutreach)
	if err != nil {
		return err
	}
	defer s.Close()
	if off, err := switchedOff(ctx, s, leadstore.JobVerify, commandLeadsVerify, verify.Provider); off || err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	if cfg.Live != config.LiveAll {
		_, _ = fmt.Fprintf(stdout, "leads verify: %s (LEADHOUND_LIVE is %s); no Reoon call made\n", liveOffMessage, cfg.Live)
		return nil
	}
	dailyCap, err := reoonDailyCap(os.Getenv)
	if err != nil {
		return err
	}
	loc, err := leadsZone()
	if err != nil {
		return err
	}
	bs := newBatchSetup(setup, loc)
	if bs.shadow {
		_, _ = fmt.Fprintln(stdout, "leads verify: shadow mode, so no live batch is drafted and nothing needs checking.")
		return nil
	}
	client, err := newReoonClient(true)
	if errors.Is(err, verify.ErrNoKey) {
		// Recorded, so Controls can say the key is missing.
		q := s.Q()
		runID, rerr := q.StartRun(ctx, commandLeadsVerify, verify.Provider)
		if rerr != nil {
			return rerr
		}
		if rerr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{Note: noReoonKeyNote}); rerr != nil {
			return rerr
		}
		_, _ = fmt.Fprintln(stdout, "leads verify: nothing was checked, because the Reoon key is not at /etc/leadhound/reoon.key (or LEADHOUND_REOON_KEY_FILE for a hand run). Until it is, switch the Email check off in Controls, or batches draft no unchecked address.")
		return nil
	}
	if err != nil {
		return err
	}
	out, err := verifyRun(ctx, s, client, bs, leadsNow(), *maxChecks, dailyCap)
	if err != nil {
		return err
	}
	if out.BalanceRead && out.Balance.Empty() && out.Checked == 0 {
		_, _ = fmt.Fprintln(stdout, noCreditsLine)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "leads verify: %s\n", out.note())
	return nil
}

// verifyRun is one check run after the switch, live and key checks: the balance first (no
// credit left: recorded, nothing checked), then the queue's addresses up to max, the day's cap
// and the credits, verifyParallel at a time, each answer stored as it comes. A check that
// fails is counted and leaves its address unchecked. The run row carries the counts.
func verifyRun(ctx context.Context, s *store.Store, client reoonClient, bs batchSetup, now time.Time, maxChecks, dailyCap int) (verifyOutcome, error) {
	var out verifyOutcome
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsVerify, verify.Provider)
	if err != nil {
		return out, err
	}
	finish := func(runErr error) (verifyOutcome, error) {
		res := store.RunResult{Requests: out.Checked + out.Failed, Failures: out.Failed, Note: out.note()}
		if runErr != nil {
			res.Note = runErr.Error()
		} else if out.BalanceRead && out.Balance.Empty() && out.Checked == 0 {
			res.Note = noCreditsLine
		}
		if err := q.FinishRun(context.WithoutCancel(ctx), runID, res); err != nil && runErr == nil {
			return out, err
		}
		return out, runErr
	}
	bal, err := client.Balance(ctx)
	if err != nil {
		return finish(err)
	}
	out.Balance, out.BalanceRead = bal, true
	if bal.Empty() {
		return finish(nil)
	}
	lq := leadstore.NewQueries(s.Pool())
	today, err := lq.EmailChecksSince(ctx, leadstore.DefaultAccountID, startOfDay(now, bs.loc))
	if err != nil {
		return finish(err)
	}
	limit := min(maxChecks, dailyCap-len(today), bal.Daily+bal.Instant)
	if limit <= 0 {
		return finish(nil)
	}
	addrs, err := verifyQueue(ctx, lq, bs, now, limit)
	if err != nil {
		return finish(err)
	}
	type answer struct {
		addr string
		res  verify.Result
		err  error
	}
	answers := make(chan answer)
	sem := make(chan struct{}, verifyParallel)
	var wg sync.WaitGroup
	go func() {
		for _, a := range addrs {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				r, err := client.Check(ctx, a)
				answers <- answer{a, r, err}
			}()
		}
		wg.Wait()
		close(answers)
	}()
	var storeErr error
	for a := range answers {
		if a.err != nil {
			out.Failed++
			_, _ = fmt.Fprintf(stdout, "leads verify: %s not checked: %v\n", a.addr, a.err)
			continue
		}
		if storeErr != nil {
			continue
		}
		if _, err := lq.AddEmailCheck(ctx, leadstore.EmailCheck{Address: a.addr, Provider: verify.Provider, Status: a.res.Status,
			Score: a.res.Score, CatchAll: a.res.CatchAll, Role: a.res.Role, CheckedAt: now}); err != nil {
			storeErr = err
			continue
		}
		out.Checked++
		switch d := verify.Decide([]verify.Check{{Status: a.res.Status, CheckedAt: now}}, bs.leads.Verify.CatchAllRule(), now, now).Decision; d {
		case verify.Send:
			out.Sendable++
		case verify.Block:
			out.Blocked++
		default:
			out.Waiting++
		}
	}
	if storeErr != nil {
		return finish(storeErr)
	}
	// What the run spent; a failed read leaves the balance read first.
	if b, err := client.Balance(ctx); err == nil {
		out.Balance = b
	}
	return finish(nil)
}

// verifyQueue is the addresses due a check, at most limit: for each active campaign with at
// least one mailbox assigned (a campaign with none drafts no live batch, so checking its
// addresses would spend credits for nothing), the candidates its next live batch would draft, in the batch's own order (starred first, then
// the outreach order), taken in turn from each campaign; for each lead the address the gate
// waits on (its own, or the one it would fall to), each once. An address with a fresh answer
// is not due, except a first unknown from an earlier day.
func verifyQueue(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time, limit int) ([]string, error) {
	gate, err := loadEmailGate(ctx, lq, bs, now)
	if err != nil {
		return nil, err
	}
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	type queue struct {
		plan  batchPlan
		cands []leadstore.Candidate
	}
	var queues []queue
	for _, c := range cs {
		if c.RetiredAt != nil {
			continue
		}
		cms, err := lq.CampaignMailboxes(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		if len(cms) == 0 {
			continue
		}
		plan, err := resolveBatchPlan(ctx, lq, bs, c.ID)
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "leads verify: campaign %s skipped: %v\n", c.Slug, err)
			continue
		}
		cands, _, _, _, err := batchQueue(ctx, lq, bs, plan, c.ID, now)
		if err != nil {
			return nil, err
		}
		queues = append(queues, queue{plan, cands})
	}
	var out []string
	for i := 0; len(out) < limit; i++ {
		more := false
		for _, qu := range queues {
			if i >= len(qu.cands) || len(out) >= limit {
				continue
			}
			more = true
			c := qu.cands[i]
			if _, seq := qu.plan.forLead(c.Country); seq != "" {
				continue // a call-first lead's emails go to the address given on the call
			}
			_, res, err := gate.route(ctx, lq, bs.leads.Policy(c.Country).Channel, c)
			if err != nil {
				return nil, err
			}
			if res.need != "" && !slices.Contains(out, res.need) {
				out = append(out, res.need)
			}
		}
		if !more {
			break
		}
	}
	return out, nil
}

// emailWaitingLine is the batch's line about the leads left for their email check, "" for
// none: "3 leads wait for their email check".
func emailWaitingLine(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 lead waits for its email check"
	}
	return fmt.Sprintf("%d leads wait for their email check", n)
}

// emailCheckToday is today's checks in Controls' words: how many were checked, how many may
// be sent and how many are blocked, by verify.catch_all in force.
type emailCheckToday struct{ Checked, Sendable, Blocked int }

// countEmailChecks counts a day's checks.
func countEmailChecks(rows []leadstore.EmailCheck, catchAll string) emailCheckToday {
	var t emailCheckToday
	for _, r := range rows {
		t.Checked++
		switch verify.Decide([]verify.Check{{Status: r.Status, CheckedAt: r.CheckedAt}}, catchAll, r.CheckedAt, r.CheckedAt).Decision {
		case verify.Send:
			t.Sendable++
		case verify.Block:
			t.Blocked++
		}
	}
	return t
}

// emailCheckLine is an email route's latest check (history newest first) in the lead page's
// words, with when it was made: "Email check: safe, may be sent"; "Email check: not checked
// yet" with no time when it never was. A result older than verify.FreshFor says it is checked
// again before the address can be sent to.
func emailCheckLine(history []leadstore.EmailCheck, catchAll string, now, dayStart time.Time) (string, *time.Time) {
	if len(history) == 0 {
		return "Email check: not checked yet", nil
	}
	hs := make([]verify.Check, len(history))
	for i, h := range history {
		hs[i] = verify.Check{Status: h.Status, CheckedAt: h.CheckedAt}
	}
	at := history[0].CheckedAt
	words := "Email check: " + emailCheckWords(history[0].Status)
	switch v := verify.Decide(hs, catchAll, now, dayStart); v.Decision {
	case verify.Send:
		words += ", may be sent"
	case verify.Block:
		words += ", not sent"
		if v.Status == verify.StatusUnknown {
			words += " (unknown twice in a row)"
		}
	case verify.Wait:
		words += ", checked again the next day"
	default:
		words += ", too old: checked again before sending"
	}
	return words, &at
}

// emailCheckWords is a result in the lead page's words: "safe", "catch-all", "role address".
func emailCheckWords(status string) string {
	switch status {
	case verify.StatusRole:
		return "role address"
	case verify.StatusInboxFull:
		return "inbox full"
	case verify.StatusCatchAll:
		return "catch-all"
	}
	return cmp.Or(strings.ReplaceAll(status, "_", " "), "not checked")
}
