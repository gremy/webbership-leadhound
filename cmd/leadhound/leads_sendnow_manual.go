package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/verify"
	"github.com/jackc/pgx/v5"
)

// The console's manual Send now (console.ManualSendBackend): the operator picks leads (or a
// batch), the backend builds a manual batch through newBatch, and manualSendPlan says per lead
// whether it goes, waits or has a hard stop, with the warnings the operator may go past. Confirm
// re-reads the plan, approves the batch with its first emails due now, writes the send_request
// row in the approval's transaction and then the request file the send-now watcher fires on.
// Hard stops are never codes: nothing the console posts lifts one.

const (
	// manualMaxLeads is the most leads one Send now takes (D2: the send's --max 25 stays).
	manualMaxLeads = 25
	// manualConfirmWindow is how long an unconfirmed manual batch lives: the next preview
	// discards any older one (DiscardStaleManualBatches).
	manualConfirmWindow = 30 * time.Minute
	// verifyNowAction is "Check email now"'s request file under runRequestDir, and its
	// watcher's name (deploy/leadhound-leads-verify-now.path).
	verifyNowAction = "leads-verify-now"
	// verifyNowWait is how long the confirm page waits on a Check email now request before it
	// stops polling: the watcher starts the check at once, and one batch is at most 25 checks.
	verifyNowWait = 10 * time.Minute
)

// The plan-wide hard stops, in the words the confirm page shows on each lead.
const (
	stopShadow    = "Shadow mode: LEADHOUND_OUTREACH is not live on this server, so nothing would be sent for real."
	stopPaused    = "Sending is switched off on Controls. Switch it on first."
	stopSplit     = "Split it: at most 25 per send."
	stopTampered  = "This batch changed after it was drafted, so it can never be sent. Discard it."
	stopWasShadow = "This batch was drafted in shadow mode. Start Send now again to draft it for real."
	stopWasLive   = "This batch was drafted for real, and this server is in shadow mode now."
)

// manualMemo is what the console remembers of each manual batch between the preview and the
// confirm, in memory only: the chosen leads newBatch left out (they never reach the batch, so
// only the preview knows them), its notes, and the lead ids the page last showed with a hard
// stop, so confirm can tell a stop that is new. Lost on a restart, which is safe: a forgotten
// left-out lead is not sent, and a forgotten shown set makes confirm show the plan again.
type manualMemo struct {
	mu sync.Mutex
	m  map[int64]*manualEntry
}

type manualEntry struct {
	at      time.Time
	extra   []console.SendPlanLead // chosen leads not in the batch, each with its Stop
	notes   []string
	shown   map[int64]bool // lead ids shown with a hard stop
	showned bool           // shown was set by a plan the page showed
}

func (mm *manualMemo) get(id int64) manualEntry {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	if e := mm.m[id]; e != nil {
		return manualEntry{at: e.at, extra: slices.Clone(e.extra), notes: slices.Clone(e.notes), shown: maps.Clone(e.shown), showned: e.showned}
	}
	return manualEntry{}
}

func (mm *manualMemo) entry(id int64, now time.Time) *manualEntry {
	if mm.m == nil {
		mm.m = map[int64]*manualEntry{}
	}
	for k, e := range mm.m {
		if now.Sub(e.at) > 2*manualConfirmWindow {
			delete(mm.m, k)
		}
	}
	e := mm.m[id]
	if e == nil {
		e = &manualEntry{at: now}
		mm.m[id] = e
	}
	return e
}

// built records what the preview that built batch id left out and noted.
func (mm *manualMemo) built(id int64, now time.Time, extra []console.SendPlanLead, notes []string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	e := mm.entry(id, now)
	e.at, e.extra, e.notes = now, extra, notes
}

// showed records the hard stops of a plan the page is about to show.
func (mm *manualMemo) showed(id int64, now time.Time, p console.SendPlan) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	e := mm.entry(id, now)
	e.shown, e.showned = map[int64]bool{}, true
	for _, l := range p.Leads {
		if l.Stop != "" {
			e.shown[l.LeadID] = true
		}
	}
}

// manualFacts is what confirm needs from a plan besides what the page shows.
type manualFacts struct {
	batch    leadstore.Batch
	skip     []int // item numbers of the leads with a hard stop the approval leaves out
	now      int   // emails this send's run sends
	later    int   // OK leads held for a shared recipient domain: they go in the next runs
	codes    []string
	operator string
}

// SendNowPreview builds (or reuses) the manual batch of the chosen leads, or takes the batch
// given, and returns its plan (console.ManualSendBackend).
func (b *backend) SendNowPreview(ctx context.Context, req console.ManualSendRequest) (console.SendPlan, error) {
	setup, err := b.leadsSetup(ctx, leadsForOutreach)
	if err != nil {
		return console.SendPlan{}, err
	}
	now := b.now()
	switch {
	case (req.BatchID > 0) == (len(req.LeadIDs) > 0):
		return console.SendPlan{}, refused("Send now takes chosen leads or a batch; choose again.")
	case len(req.LeadIDs) > manualMaxLeads:
		return console.SendPlan{}, refused("Split it: at most %d per send; you chose %d.", manualMaxLeads, len(req.LeadIDs))
	case !setup.env.Live() && !b.dev:
		return console.SendPlan{}, refused("%s", stopShadow)
	}
	if err := leadsInTx(ctx, b.s.Pool(), func(tq leadstore.Queries) error {
		_, err := tq.DiscardStaleManualBatches(ctx, now.Add(-manualConfirmWindow))
		return err
	}); err != nil {
		return console.SendPlan{}, err
	}
	id := req.BatchID
	if id == 0 {
		if id, err = b.buildManual(ctx, setup, now, req.LeadIDs); err != nil {
			return console.SendPlan{}, err
		}
	}
	plan, _, err := b.manualSendPlan(ctx, setup, id, now)
	if err != nil {
		return console.SendPlan{}, err
	}
	b.manual.showed(id, now, plan)
	return plan, nil
}

// SendNowPlan is the plan of an unapproved batch as it stands now; it writes nothing.
func (b *backend) SendNowPlan(ctx context.Context, batchID int64) (console.SendPlan, error) {
	setup, err := b.leadsSetup(ctx, leadsForOutreach)
	if err != nil {
		return console.SendPlan{}, err
	}
	now := b.now()
	plan, _, err := b.manualSendPlan(ctx, setup, batchID, now)
	if err != nil {
		return console.SendPlan{}, err
	}
	b.manual.showed(batchID, now, plan)
	return plan, nil
}

// errManualChanged rolls a confirm back when the batch was approved under it.
var errManualChanged = errors.New("manual send: the batch was approved meanwhile")

// SendNowConfirm checks the press against the plan now and, when it still matches, approves
// the batch (first emails due now, the hard-stopped leads left out), writes its send_request
// row in the same transaction and then asks the send-now watcher for the run.
func (b *backend) SendNowConfirm(ctx context.Context, c console.ManualConfirm) (console.ControlOutcome, error) {
	setup, err := b.leadsSetup(ctx, leadsForOutreach)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	now := b.now()
	if strings.TrimSpace(c.Operator) == "" {
		return console.ControlOutcome{}, refused("The send names no operator; sign in again. Nothing was sent.")
	}
	// A send running, or a Send now waiting for its unit, is refused, so no request file is
	// written over one not read yet.
	asked, runs, err := b.sendNowRead(ctx, now, false)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	locked, err := sendLockHeld(ctx, b.s.Pool())
	if err != nil {
		return console.ControlOutcome{}, err
	}
	if st := sendNowState(asked, locked, runs, now.In(b.loc)); st.busy != "" {
		return console.ControlOutcome{}, refused("%s Nothing was sent.", st.busy)
	}
	plan, f, err := b.manualSendPlan(ctx, setup, c.BatchID, now)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	if manualChanged(plan, c, b.manual.get(c.BatchID)) {
		b.manual.showed(c.BatchID, now, plan)
		return console.ControlOutcome{}, &console.PlanChangedError{Plan: plan}
	}
	if _, err := outreach.ParseOverrides(f.codes); err != nil {
		return console.ControlOutcome{}, err // the plan names only the closed set
	}
	// The warnings as the page showed them, sentences and numbers, for the run note and the record.
	type shownWarning struct {
		Code     string `json:"code"`
		Sentence string `json:"sentence"`
	}
	sw := make([]shownWarning, 0, len(plan.Warnings))
	for _, w := range plan.Warnings {
		sw = append(sw, shownWarning{Code: w.Code, Sentence: w.Sentence})
	}
	shown, err := json.Marshal(sw)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	approved, err := approveBatch(ctx, b.s, setup.leads, func() time.Time { return now }, ApproveRequest{
		ID: c.BatchID, HashPrefix: plan.HashPrefix, Skip: f.skip, DueNow: true,
		before: func(tq leadstore.Queries) error { return tq.MarkBatchManual(ctx, c.BatchID) },
		after: func(tq leadstore.Queries, a leadstore.Approval) error {
			if a.Already {
				return errManualChanged
			}
			_, err := tq.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: c.BatchID, Codes: f.codes,
				HashPrefix: plan.HashPrefix, Operator: strings.TrimSpace(c.Operator), Shown: shown})
			return err
		},
	})
	switch r := asRefusal(err); {
	case errors.Is(err, leadstore.ErrBatchApproved), errors.Is(err, errManualChanged):
		return console.ControlOutcome{}, refused("Batch %d was already sent; nothing more was asked.", c.BatchID)
	case errors.Is(err, leadstore.ErrBatchDiscarded), r != nil && r.Kind == RefuseBatchDiscarded:
		return console.ControlOutcome{}, refused("Batch %d was discarded; nothing was sent. Start Send now again.", c.BatchID)
	case errors.Is(err, leadstore.ErrBatchNotFound), r != nil && r.Kind == RefuseBatchNotFound:
		return console.ControlOutcome{}, console.ErrNotFound
	case r != nil && (r.Kind == RefuseHashMismatch || r.Kind == RefuseBatchChanged):
		return console.ControlOutcome{}, refused("Batch %d changed since it was drafted; nothing was sent. Start Send now again.", c.BatchID)
	case err != nil:
		return console.ControlOutcome{}, err
	}
	// The row is committed; only now does the watcher get its file.
	if err := writeSendNowManualRequest(setup.dataDir, c.BatchID, now); err != nil {
		return console.ControlOutcome{}, fmt.Errorf("manual send: batch %d is approved (%d messages) but the send could not be asked, so the hourly send takes it within today's limits: %w",
			c.BatchID, approved.Approval.Approved, err)
	}
	if b.dev {
		return console.ControlOutcome{Message: "Asked. In dev nothing watches the request: run leadhound leads send --from-request to write them to the shadow outbox."}, nil
	}
	msg := "Asked. " + goesNow(f.now)
	if len(f.codes) > 0 {
		msg += "; the run notes every warning you went past"
	}
	msg += "."
	if f.later > 0 {
		msg += fmt.Sprintf(" %s in the next runs, one per recipient domain each run.", goLater(f.later))
	}
	return console.ControlOutcome{Message: msg}, nil
}

func goesNow(n int) string {
	if n == 1 {
		return "1 email goes out now"
	}
	return fmt.Sprintf("%d emails go out now", n)
}

func goLater(n int) string {
	if n == 1 {
		return "1 more goes"
	}
	return fmt.Sprintf("%d more go", n)
}

// manualChanged reports whether the press no longer matches the plan now: it cannot send,
// its hash is not the one shown, a warning now is not one the page showed, or a lead has a
// hard stop the page did not show (with nothing remembered of the page, any stop is new).
func manualChanged(p console.SendPlan, c console.ManualConfirm, seen manualEntry) bool {
	if !p.CanSend || strings.ToLower(strings.TrimSpace(c.HashPrefix)) != p.HashPrefix {
		return true
	}
	for _, w := range p.Warnings {
		if !slices.Contains(c.Codes, w.Code) {
			return true
		}
	}
	for _, l := range p.Leads {
		if l.Stop != "" && (!seen.showned || !seen.shown[l.LeadID]) {
			return true
		}
	}
	return false
}

// writeSendNowManualRequest writes the manual Send now request file: one JSON line naming the
// batch and when it was asked (sendNowRequest), 0600, as writeRunRequest writes its file. The
// send service reads the batch id only; everything else comes from the send_request row.
func writeSendNowManualRequest(dataDir string, batchID int64, at time.Time) error {
	return writeJSONRequest(dataDir, sendNowAction, sendNowRequest{BatchID: batchID, At: at.UTC()})
}

// verifyNowRequest is Check email now's request file line: the batch whose addresses to check.
type verifyNowRequest struct {
	BatchID int64     `json:"batch_id"`
	At      time.Time `json:"at"`
}

// writeJSONRequest writes v as one JSON line into <dataDir>/run/<action>, 0600.
func writeJSONRequest(dataDir, action string, v any) error {
	if dataDir == "" {
		return errors.New("run now: LEADHOUND_DATA_DIR is not set")
	}
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("run now: %w", err)
	}
	dir := filepath.Join(dataDir, runRequestDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("run now: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, action), append(line, '\n'), 0o600); err != nil {
		return fmt.Errorf("run now: %w", err)
	}
	return nil
}

// SendNowCheckEmail asks the email check to run now for the batch's addresses that have no
// answer yet: the request file the verify-now watcher fires on.
func (b *backend) SendNowCheckEmail(ctx context.Context, batchID int64) (console.ControlOutcome, error) {
	setup, err := b.leadsSetup(ctx, leadsForOutreach)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	now := b.now()
	plan, _, err := b.manualSendPlan(ctx, setup, batchID, now)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	n := 0
	for _, l := range plan.Leads {
		if l.Waiting != "" {
			n++
		}
	}
	if n == 0 {
		return console.ControlOutcome{}, refused("No address in batch %d waits for its email check.", batchID)
	}
	if off, _, err := jobOff(ctx, b.lq(), leadstore.JobVerify); err != nil {
		return console.ControlOutcome{}, err
	} else if off {
		return console.ControlOutcome{}, refused("The Email check is switched off on Controls.")
	}
	if err := writeJSONRequest(setup.dataDir, verifyNowAction, verifyNowRequest{BatchID: batchID, At: now.UTC()}); err != nil {
		return console.ControlOutcome{}, err
	}
	return console.ControlOutcome{Message: fmt.Sprintf("Asked. The email check runs now for %s; this page updates when it is done.",
		plural(n, "address", "addresses"))}, nil
}

// buildManual is the preview's batch of the chosen leads: the unconfirmed manual batch of
// exactly these leads when there is one, else a new one through newBatch (any unconfirmed
// manual batch holding one of them is discarded first, so a lead is in one at a time). What
// newBatch left out is remembered for the page. Nothing drafted is a refusal naming why.
func (b *backend) buildManual(ctx context.Context, setup leadsSetup, now time.Time, ids []int64) (int64, error) {
	lq := b.lq()
	first, err := lq.LeadByID(ctx, ids[0])
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return 0, console.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	for _, id := range ids[1:] {
		if _, err := lq.LeadByID(ctx, id); errors.Is(err, leadstore.ErrLeadNotFound) {
			return 0, console.ErrNotFound
		} else if err != nil {
			return 0, err
		}
	}
	bs := newBatchSetup(setup, b.loc)
	var out NewBatchOutcome
	reused := int64(0)
	err = manualTx(ctx, b, func(tx pgx.Tx, tq leadstore.Queries) error {
		if err := tq.LockBatchNew(ctx); err != nil {
			return err
		}
		open, err := openManualBatches(ctx, tx, ids)
		if err != nil {
			return err
		}
		for id, leads := range open {
			if len(leads) == len(ids) && !slices.ContainsFunc(ids, func(l int64) bool { return !slices.Contains(leads, l) }) {
				reused = id
				return nil
			}
		}
		for _, id := range slices.Sorted(maps.Keys(open)) {
			if _, err := tq.DiscardBatch(ctx, id); err != nil {
				return err
			}
		}
		// A lead whose first email is in another batch never enters this one (batch 0 is none).
		taken, err := firstEmailElsewhere(ctx, tx, 0, ids)
		if err != nil {
			return err
		}
		draft := slices.DeleteFunc(slices.Clone(ids), func(id int64) bool { return taken[id] != "" })
		if len(draft) > 0 {
			if out, err = newBatch(ctx, tq, bs, now, NewBatchRequest{LeadIDs: draft, Manual: true, CampaignID: first.CampaignID}); err != nil {
				return err
			}
		}
		for _, id := range ids {
			if why := taken[id]; why != "" {
				out.Skipped = append(out.Skipped, DroppedLead{LeadID: id, Why: why})
			}
		}
		return nil
	})
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseSettingsIgnored:
			return 0, refused("%s", settingsIgnoredWords)
		case RefuseNotLive:
			return 0, refused("%s", stopShadow)
		}
		return 0, refused("%s", cmp.Or(r.Detail, "The batch could not be drafted; nothing was sent."))
	}
	if err != nil {
		return 0, err
	}
	if reused != 0 {
		return reused, nil
	}
	why := map[int64]string{}
	for _, d := range append(slices.Clone(out.Skipped), out.Dropped...) {
		if d.LeadID != 0 && why[d.LeadID] == "" {
			why[d.LeadID] = d.Why
		}
	}
	in := map[int64]bool{}
	if out.BatchID != 0 {
		msgs, err := lq.BatchMessages(ctx, out.BatchID)
		if err != nil {
			return 0, err
		}
		for _, m := range msgs {
			in[m.LeadID] = true
		}
	}
	var extra []console.SendPlanLead
	var left []string
	for _, id := range ids {
		if in[id] {
			continue
		}
		l := console.SendPlanLead{LeadID: id, Stop: manualStopWords(cmp.Or(why[id], "it could not be drafted"))}
		if lead, err := lq.LeadByID(ctx, id); err == nil {
			if o, err := lq.OrgByID(ctx, lead.OrgID); err == nil {
				l.Name, l.Domain = o.Name, o.Domain
			}
		}
		extra = append(extra, l)
		left = append(left, cmp.Or(l.Name, l.Domain, fmt.Sprintf("lead %d", id))+": "+l.Stop)
	}
	if out.BatchID == 0 {
		return 0, refused("Nothing to send. %s", strings.Join(left, " "))
	}
	var notes []string
	if line := fallbackLine(out.Fallbacks); line != "" {
		notes = append(notes, line+". The console never calls Claude, so these are the campaign's template drafts.")
	}
	b.manual.built(out.BatchID, now, extra, notes)
	return out.BatchID, nil
}

// manualStopWords is a reason newBatch gave, as one sentence that starts with a capital.
func manualStopWords(why string) string {
	why = strings.TrimSpace(why)
	if why == "" {
		return why
	}
	why = strings.ToUpper(why[:1]) + why[1:]
	if !strings.HasSuffix(why, ".") {
		why += "."
	}
	return why
}

// manualTx runs fn in one transaction, with the transaction itself for the one read leadstore
// has no query for (openManualBatches) and the leads queries over it.
func manualTx(ctx context.Context, b *backend, fn func(pgx.Tx, leadstore.Queries) error) error {
	tx, err := b.s.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("manual send: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	if err := fn(tx, leadstore.NewQueries(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("manual send: commit: %w", err)
	}
	return nil
}

// openManualBatches are the unconfirmed manual batches holding any of leads, each with all
// its leads.
func openManualBatches(ctx context.Context, tx pgx.Tx, leads []int64) (map[int64][]int64, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT m.batch_id, m.lead_id FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE b.manual AND b.approved_at IS NULL AND b.discarded_at IS NULL
		   AND m.batch_id IN (SELECT x.batch_id FROM outreach_message x WHERE x.lead_id = ANY($1::bigint[]))
		 ORDER BY 1, 2`, leads)
	if err != nil {
		return nil, fmt.Errorf("manual send: open manual batches: %w", err)
	}
	type row struct {
		BatchID int64 `db:"batch_id"`
		LeadID  int64 `db:"lead_id"`
	}
	rs, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, fmt.Errorf("manual send: open manual batches: %w", err)
	}
	out := map[int64][]int64{}
	for _, r := range rs {
		out[r.BatchID] = append(out[r.BatchID], r.LeadID)
	}
	return out, nil
}

// manualQueue is the queue a manual batch of campaignID drafts from: the chosen leads ids that
// batchQueue's rules still offer, in the order chosen, starred or not (the operator chose
// them; batches_from decides only what the daily batch takes). Every chosen lead it leaves
// out comes back as a DroppedLead with its LeadID and why.
func manualQueue(ctx context.Context, lq leadstore.Queries, bs batchSetup, plan batchPlan, campaignID int64, now time.Time, ids []int64) (
	[]leadstore.Candidate, []DroppedLead, error) {
	opts, err := batchCandidateOptions(ctx, lq, bs, now)
	if err != nil {
		return nil, nil, err
	}
	opts.StarredOnly = false
	opts.CampaignID, opts.AccountID, opts.Countries = campaignID, leadstore.DefaultAccountID, plan.candidateCountries()
	opts.PhoneOnlyCountries = plan.phoneOnlyCountries()
	all, err := lq.CandidateLeads(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]leadstore.Candidate, len(all))
	for _, c := range all {
		byID[c.LeadID] = c
	}
	skips, elsewhere, err := crossCampaignSkips(ctx, lq, campaignID)
	if err != nil {
		return nil, nil, err
	}
	var cands []leadstore.Candidate
	var dropped []DroppedLead
	for _, id := range ids {
		c, ok := byID[id]
		if ok && !elsewhere[c.OrgID] {
			cands = append(cands, c)
			continue
		}
		d, err := manualLeftOut(ctx, lq, bs, campaignID, id, skips)
		if err != nil {
			return nil, nil, err
		}
		dropped = append(dropped, d)
	}
	return cands, dropped, nil
}

// manualLeftOut is why chosen lead id is not in the queue a manual batch drafts from.
func manualLeftOut(ctx context.Context, lq leadstore.Queries, bs batchSetup, campaignID, id int64, skips []DroppedLead) (DroppedLead, error) {
	l, err := lq.LeadByID(ctx, id)
	if err != nil {
		return DroppedLead{}, err
	}
	o, err := lq.OrgByID(ctx, l.OrgID)
	if err != nil {
		return DroppedLead{}, err
	}
	d := DroppedLead{LeadID: id, Domain: o.Domain, Kind: l.Kind, Channel: l.Channel}
	routes, err := lq.RoutesFor(ctx, o.ID)
	if err != nil {
		return d, err
	}
	route := ""
	for _, r := range routes {
		if l.RouteID != nil && r.ID == *l.RouteID && r.Kind == leadstore.RouteEmail {
			route = r.Value
		}
	}
	s, err := lq.SuppressionSet(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return d, err
	}
	switch {
	case l.CampaignID != campaignID:
		d.Why = "it is in another campaign; send it on its own"
	case l.State == leadstore.LeadBatched:
		d.Why = "it is already in a batch that waits for approval; send that batch instead"
	case l.State != leadstore.LeadNew:
		d.Why = "its state is " + leadStateWords(l.State) + ", and only a new lead can be sent"
	case outreach.Suppressed(route, o.Domain, outreach.NewSuppressionSet(s.Addresses, s.Domains)):
		d.Why = "it is suppressed (opted out or blocked)"
	case !bs.shadow && !leadsMail.allowReserved && (outreach.ReservedDomain(o.Domain) || outreach.ReservedDomain(recipientDomain(route))):
		d.Why = "it is on a reserved test domain, never sent for real"
	}
	if d.Why != "" {
		return d, nil
	}
	for _, sk := range skips {
		if strings.EqualFold(sk.Domain, o.Domain) {
			d.Why = "its business is in a conversation in another campaign: " + sk.Why
			return d, nil
		}
	}
	hasEmail := false
	for _, r := range routes {
		hasEmail = hasEmail || r.Kind == leadstore.RouteEmail
	}
	if !hasEmail || l.Channel == leadconfig.ChannelNone {
		d.Why = "it has no email address to send to"
		return d, nil
	}
	d.Why = "it is not in the send queue now: its business was already reached, it is deferred, or its kind, country or rank is left out in Settings"
	return d, nil
}

// manualRoom is the room a manual batch assigns its n leads with: every mailbox ready for new
// leads and assigned to the campaign, with today's room plus n leads on top, so each lead gets
// one whatever the day's room (the confirm page warns about the ramp, and stops at the hard
// ceilings). A mailbox not ready is left out; none ready is an empty map.
func manualRoom(bs batchSetup, now time.Time, n, steps int, mbs []leadstore.MailboxStatus, assigned map[int64]bool) map[int64]int {
	loads := make([]outreach.MailboxLoad, 0, len(mbs))
	for _, m := range mbs {
		ml := outreach.MailboxLoad{ID: m.ID, Domain: m.Domain, SentToday: m.SentToday, Ready: readyForLeads(m) && assigned[m.ID]}
		if bs.leads.Caps.StartDate.IsFirstSend() && m.FirstLiveSend != nil {
			fs := m.FirstLiveSend.In(bs.loc)
			ml.FirstSend = &fs
		}
		loads = append(loads, ml)
	}
	base := outreach.RoomToday(bs.leads, loads, now.In(bs.loc))
	room := map[int64]int{}
	for _, ml := range loads {
		if ml.Ready {
			room[ml.ID] = (base[ml.ID] + n) * max(1, steps)
		}
	}
	return room
}

// manualSendPlan is the one reading of a batch for Send now: what the preview shows, the confirm
// page polls and the confirm checks against. It reads only. A batch that is not there, or
// discarded, is console.ErrNotFound; an approved one is a refusal.
func (b *backend) manualSendPlan(ctx context.Context, setup leadsSetup, id int64, now time.Time) (console.SendPlan, manualFacts, error) {
	lq := b.lq()
	var f manualFacts
	v, err := batchView(ctx, lq, id)
	if r := asRefusal(err); r != nil && r.Kind == RefuseBatchNotFound {
		return console.SendPlan{}, f, console.ErrNotFound
	}
	if err != nil {
		return console.SendPlan{}, f, err
	}
	f.batch = v.Batch
	switch {
	case v.Batch.DiscardedAt != nil:
		return console.SendPlan{}, f, console.ErrNotFound
	case v.Batch.ApprovedAt != nil && v.Batch.Manual:
		return console.SendPlan{}, f, refused("Batch %d was already sent with Send now; nothing more to do here.", id)
	case v.Batch.ApprovedAt != nil:
		return console.SendPlan{}, f, refused("Batch %d is already approved; the hourly send takes it.", id)
	}
	plan := console.SendPlan{BatchID: id, HashPrefix: v.HashPrefix()}
	memo := b.manual.get(id)
	live := setup.env.Live()
	bh := setup.leads.SendingWindow()

	// The stops that hold for every lead.
	all := ""
	sw, err := lq.JobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobVerify)
	if err != nil {
		return plan, f, err
	}
	paused, _, _, err := lq.SendPaused(ctx)
	if err != nil {
		return plan, f, err
	}
	switch {
	case v.Changed:
		all = stopTampered
	case !live && !b.dev:
		all = stopShadow
	case v.Batch.Shadow && live:
		all = stopWasShadow
	case !v.Batch.Shadow && !live:
		all = stopWasLive
	case paused:
		all = stopPaused
	case live && !b.dev && !readTimers(ctx, controlsGetenv).units[unitSend].Enabled:
		all = whyNoTimerSentence + " Send now runs only where it is."
	case len(v.Items) > manualMaxLeads:
		all = stopSplit
	}

	mbs, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(now, b.loc))
	if err != nil {
		return plan, f, err
	}
	byMB := map[int64]leadstore.MailboxStatus{}
	for _, m := range mbs {
		byMB[m.ID] = m
	}
	assigned := map[int64]bool{}
	if !v.Batch.Shadow {
		cms, err := lq.CampaignMailboxes(ctx, v.Batch.CampaignID)
		if err != nil {
			return plan, f, err
		}
		for _, cm := range cms {
			assigned[cm.MailboxID] = true
		}
	}
	ready := func(m leadstore.MailboxStatus) bool {
		if v.Batch.Shadow {
			return m.State == leadstore.MailboxActive
		}
		return readyForLeads(m) && assigned[m.ID]
	}
	s, err := lq.SuppressionSet(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return plan, f, err
	}
	supp := outreach.NewSuppressionSet(s.Addresses, s.Domains)
	gate := &emailGate{checks: map[string][]verify.Check{}}
	if live && sw.On && !v.Batch.Shadow {
		if gate, err = loadEmailGate(ctx, lq, newBatchSetup(setup, b.loc), now); err != nil {
			return plan, f, err
		}
	}
	// batchMode: Approve and send now on a batch that exists (the chosen-leads preview marks its
	// own batch manual at once). There the operator chose the batch, not a lead to leave out, so
	// Send now never skips one: its contact forms, LinkedIn notes and calls are approved as
	// Approve approves them, and any hard stop on one of its emails stops the whole send.
	batchMode := !v.Batch.Manual
	byHand := 0
	emailLead := map[int64]bool{}
	want := leadstore.LeadBatched
	if v.Batch.Shadow {
		want = leadstore.LeadNew
	}

	leadIDs := make([]int64, 0, len(v.Items))
	for _, it := range v.Items {
		leadIDs = append(leadIDs, it.Lead.ID)
	}
	elsewhere, err := firstEmailElsewhere(ctx, b.s.Pool(), id, leadIDs)
	if err != nil {
		return plan, f, err
	}

	type cand struct {
		i    int // index in plan.Leads
		item BatchItem
		msg  leadstore.Message
		loc  *time.Location
	}
	var ok []cand
	// skippable are the item numbers of the leads with a hard stop the approval must leave out
	// by its skip list. A suppressed lead and one no longer in the batch's state are left out by
	// ApproveBatch's own re-check (the first moves to suppressed there), so they are not listed.
	skippable := map[int64]int{}
	for _, it := range v.Items {
		l := console.SendPlanLead{LeadID: it.Lead.ID, Name: it.Org.Name, Domain: it.Org.Domain}
		var first *leadstore.Message
		for k := range it.Msgs {
			if it.Msgs[k].Step == 0 {
				first = &it.Msgs[k]
			}
		}
		if first != nil {
			l.Email = first.ToAddress
		}
		addr := ""
		if first != nil && first.Channel == leadstore.ChannelEmail {
			addr = first.ToAddress
		}
		if addr != "" {
			emailLead[it.Lead.ID] = true
		}
		suppressed := false
		for _, m := range it.Msgs {
			if m.ToAddress != "" && outreach.Suppressed(m.ToAddress, it.Org.Domain, supp) {
				suppressed = true
			}
		}
		if !suppressed && outreach.Suppressed("", it.Org.Domain, supp) {
			suppressed = true
		}
		m, known := byMB[mbOf(first)]
		switch {
		case all != "":
			l.Stop = all
		case suppressed, it.Lead.State != want:
		case addr == "" && batchMode:
			byHand++ // approved as Approve approves it; nothing of it is sent by email now
		case addr == "":
			l.Stop = "Its first step is not an email (a call, a contact form or a LinkedIn note), so Send now leaves it out; it is skipped when the rest are sent."
		case live && !leadsMail.allowReserved && (outreach.ReservedDomain(it.Org.Domain) || outreach.ReservedDomain(recipientDomain(addr))):
			l.Stop = "It is on a reserved test domain, never sent for real."
		case !known || !ready(m):
			l.Stop = manualMailboxWords(m, known, v.Batch.Shadow, assigned)
		}
		if l.Stop != "" {
			skippable[it.Lead.ID] = it.N
		}
		// Suppression and state come first, whatever else holds: ApproveBatch leaves the lead
		// out itself.
		switch {
		case suppressed:
			l.Stop = "It is suppressed (opted out or blocked)."
			delete(skippable, it.Lead.ID)
		case it.Lead.State != want:
			l.Stop = manualStopWords("its state is " + leadStateWords(it.Lead.State) + " now")
			delete(skippable, it.Lead.ID)
		case elsewhere[it.Lead.ID] != "":
			// Never the lead's state: a shadow lead stays new however often it is batched.
			// ApproveBatch would approve it, so it stays on the skip list (chosen leads; a
			// batch on the Approve and send now road is stopped whole instead).
			l.Stop = elsewhere[it.Lead.ID]
			skippable[it.Lead.ID] = it.N
		}
		if l.Stop == "" && gate.on && addr != "" {
			if err := gate.load(ctx, lq, addr); err != nil {
				return plan, f, err
			}
			switch vd := gate.verdict(addr); vd.Decision {
			case verify.Unchecked:
				l.Waiting = "Email not checked yet"
			case verify.Wait:
				l.Waiting = "Email check unsure; it is checked again tomorrow"
			case verify.Block:
				l.Stop = "Email check: " + emailCheckWords(vd.Status) + ". It is never sent to."
				skippable[it.Lead.ID] = it.N
			}
		}
		if l.Stop == "" && l.Waiting == "" && addr != "" {
			ok = append(ok, cand{i: len(plan.Leads), item: it, msg: *first, loc: setup.leads.Policy(it.Org.Country).Location()})
		}
		plan.Leads = append(plan.Leads, l)
	}

	// One recipient domain per run (HoldDomain) and each mailbox's limits, until no new stop:
	// a stop can free a domain for a lead that was held behind it.
	var thisRun []cand
	var held map[int64]bool
	var warns []outreach.LimitWarning
	for {
		items := make([]outreach.PlanItem, 0, len(ok))
		big := map[int64]int{}
		for _, c := range ok {
			items = append(items, outreach.PlanItem{MessageID: c.msg.ID, BatchID: id, LeadID: c.item.Lead.ID, Step: 0,
				Domain: recipientDomain(c.msg.ToAddress), Loc: c.loc, DueAt: now, MailboxID: c.msg.MailboxID})
			big[c.msg.MailboxID] = len(ok) + 1
		}
		send, _ := outreach.PlanMailboxesAnyHour(items, big, now)
		going := map[int64]bool{}
		for _, it := range send {
			going[it.LeadID] = true
		}
		thisRun, held = thisRun[:0], map[int64]bool{}
		need := map[int64]int{}
		for _, c := range ok {
			if going[c.item.Lead.ID] {
				thisRun = append(thisRun, c)
				need[c.msg.MailboxID]++
			} else {
				held[c.item.Lead.ID] = true
			}
		}
		loads := make([]outreach.MailboxLoad, 0, len(mbs))
		for _, m := range mbs {
			ml := outreach.MailboxLoad{ID: m.ID, Domain: m.Domain, SentToday: m.SentToday, Ready: ready(m)}
			if setup.leads.Caps.StartDate.IsFirstSend() && m.FirstLiveSend != nil {
				fs := m.FirstLiveSend.In(b.loc)
				ml.FirstSend = &fs
			}
			loads = append(loads, ml)
		}
		var stops []outreach.LimitStop
		warns, stops = outreach.CheckLimits(setup.leads, loads, now.In(b.loc), need)
		if len(stops) == 0 {
			break
		}
		stopOn := map[int64]outreach.LimitStop{}
		for _, st := range stops {
			stopOn[st.MailboxID] = st
		}
		ok = slices.DeleteFunc(ok, func(c cand) bool {
			st, hit := stopOn[c.msg.MailboxID]
			if hit {
				plan.Leads[c.i].Stop = manualLimitStopWords(st, byMB[st.MailboxID])
				skippable[c.item.Lead.ID] = c.item.N
			}
			return hit
		})
	}
	f.now, f.later = len(thisRun), len(held)
	for _, c := range ok {
		plan.Leads[c.i].OK = true
	}

	// The warnings, each with its numbers, in SendWarningCodes order.
	byCode := map[string][]string{}
	if len(thisRun) > 0 {
		for _, w := range warns {
			byCode[string(w.Code)] = append(byCode[string(w.Code)], manualLimitWarnWords(w, byMB[w.MailboxID]))
		}
		local := now.In(b.loc)
		if !outreach.InBusinessHours(b.loc, bh, now) {
			byCode[console.WarnWindow] = append(byCode[console.WarnWindow], fmt.Sprintf(
				"Your sending window (%s) is closed now: it is %s here.", windowWords(bh), local.Format("Mon 15:04")))
		}
		// The run checks each recipient's hours again as it sends (lifted only by business_hours),
		// so a recipient whose hours close before the run is through counts too: the run spaces
		// its emails MinSpacing apart once spacing_jitter is confirmed, and twice that is the margin.
		outside, closing, waits := 0, 0, 0
		end := now.Add(time.Duration(len(thisRun)) * 2 * outreach.MinSpacing)
		var next time.Time
		for _, c := range thisRun {
			switch {
			case !outreach.InBusinessHours(c.loc, bh, now):
				outside++
			case !outreach.InBusinessHours(c.loc, bh, end):
				closing++
			}
			if d := outreach.NextSlot(c.loc, bh, now); d.IsZero() || d.After(now) {
				waits++
				if !d.IsZero() && (next.IsZero() || d.Before(next)) {
					next = d
				}
			}
		}
		if outside+closing > 0 {
			who := fmt.Sprintf("%d of %d recipients are", outside+closing, len(thisRun))
			if len(thisRun) == 1 {
				who = "The recipient is"
			}
			when := "outside business hours where they are"
			if closing > 0 {
				when += ", or will be before this send is through"
			}
			byCode[console.WarnBusinessHours] = append(byCode[console.WarnBusinessHours], fmt.Sprintf(
				"%s %s (%s, their time).", who, when, windowWords(bh)))
		}
		if waits > 0 {
			when := "no slot in the next two weeks"
			if !next.IsZero() {
				when = "the earliest " + next.In(b.loc).Format("Mon 2 Jan 15:04") + " your time"
			}
			byCode[console.WarnSchedule] = append(byCode[console.WarnSchedule], fmt.Sprintf(
				"%s would wait for the next slot (%s); %s now instead.", firstEmails(waits), when, goWord(waits)))
		}
		if lo, hi := spacingBounds(setup.leads.Schedule.SpacingMinutes); len(thisRun) > 1 && time.Duration(hi)*time.Minute > outreach.MinSpacing {
			usual := fmt.Sprintf("%d to %d minutes", lo, hi)
			if lo == hi {
				usual = plural(hi, "minute", "minutes")
			}
			byCode[console.WarnSpacingJitter] = append(byCode[console.WarnSpacingJitter], fmt.Sprintf(
				"Emails go 1 minute apart instead of the usual %s.", usual))
		}
	}
	for _, code := range console.SendWarningCodes {
		for _, sentence := range byCode[code] {
			plan.Warnings = append(plan.Warnings, console.SendWarning{Code: code, Sentence: sentence})
		}
		if len(byCode[code]) > 0 {
			f.codes = append(f.codes, code)
		}
	}

	// The chosen leads the batch never took, then the notes.
	plan.Leads = append(plan.Leads, memo.extra...)
	plan.Notes = append(plan.Notes, memo.notes...)
	if len(held) > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf("%s share a recipient domain with another lead here, so %s in the next runs: one email per domain each run.",
			plural(len(held), "lead", "leads"), goWord(len(held))))
	}
	waiting := 0
	for _, l := range plan.Leads {
		if l.Waiting != "" {
			waiting++
		}
	}
	if waiting > 0 {
		pending, err := b.verifyNowPending(ctx, id, now)
		if err != nil {
			return plan, f, err
		}
		if pending {
			plan.Waiting = fmt.Sprintf("Checking %s now; this page updates itself.", plural(waiting, "email address", "email addresses"))
		} else {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s for the email check. Check it now, or send the others without %s.",
				waitWords2(waiting), pronoun(waiting)))
		}
	}
	if live && !v.Batch.Shadow && !sw.On {
		plan.Notes = append(plan.Notes, emailCheckOffLine)
	}
	if !live && b.dev {
		plan.Notes = append(plan.Notes, "Dev console in shadow mode: the run writes what it would send to the outbox and sends nothing.")
	}
	f.skip = slices.Sorted(maps.Values(skippable))
	blocked := false
	if batchMode {
		f.skip = nil
		if byHand > 0 {
			what := plural(byHand, "contact form, LinkedIn note or call is", "contact forms, LinkedIn notes or calls are")
			plan.Notes = append(plan.Notes, what+" approved for you to do by hand.")
		}
		for _, l := range plan.Leads {
			if l.Stop != "" && all == "" && emailLead[l.LeadID] {
				blocked = true
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s can't go now: %s Use Approve to send the batch on its normal schedule, or pick the other leads on the Leads list.",
					cmp.Or(l.Name, l.Domain, fmt.Sprintf("Lead %d", l.LeadID)), l.Stop))
			}
		}
	}
	plan.CanSend = len(ok) > 0 && waiting == 0 && plan.Waiting == "" && all == "" && !blocked
	return plan, f, nil
}

// mbOf is the mailbox of message m, 0 for none.
func mbOf(m *leadstore.Message) int64 {
	if m == nil {
		return 0
	}
	return m.MailboxID
}

// manualMailboxWords is why a lead's mailbox cannot send it now.
func manualMailboxWords(m leadstore.MailboxStatus, known, shadow bool, assigned map[int64]bool) string {
	switch {
	case !known:
		return "Its mailbox is gone; start Send now again."
	case shadow || m.State != leadstore.MailboxActive || !sendReady(m):
		return fmt.Sprintf("Mailbox %s is not ready: %s.", m.Address, mailboxBlockWords(m))
	case !assigned[m.ID]:
		return fmt.Sprintf("Mailbox %s is not assigned to this campaign.", m.Address)
	}
	return fmt.Sprintf("Mailbox %s is not ready: today's domain check has not passed; %s.", m.Address, pressCheckNow)
}

// manualLimitStopWords is a hard ceiling (or a mailbox not ready) as the confirm page says it.
func manualLimitStopWords(st outreach.LimitStop, m leadstore.MailboxStatus) string {
	switch st.Reason {
	case "mailbox_ceiling":
		return fmt.Sprintf("Mailbox %s has sent %d today; %d a day is the most any mailbox sends.", m.Address, st.Used, st.Allowed)
	case "domain_ceiling":
		return fmt.Sprintf("%s has sent %d today; %d a day is the most one sending domain sends.", st.Domain, st.Used, st.Allowed)
	}
	return fmt.Sprintf("Mailbox %s is not ready to send.", cmp.Or(m.Address, "of this lead"))
}

// manualLimitWarnWords is an overridable limit with its numbers.
func manualLimitWarnWords(w outreach.LimitWarning, m leadstore.MailboxStatus) string {
	if w.Code == outreach.OverrideRamp {
		return fmt.Sprintf("Mailbox %s has sent %d of %d today (warm-up). This goes %d over.", m.Address, w.Used, w.Allowed, w.Over)
	}
	return fmt.Sprintf("%s has sent %d of its %d today. This goes %d over.", w.Domain, w.Used, w.Allowed, w.Over)
}

// windowWords is the sending window as "09:00 to 17:00, Mon to Fri".
func windowWords(bh leadconfig.BusinessHours) string {
	days := strings.Join(bh.Days, ", ")
	if len(bh.Days) > 2 && slices.Equal(bh.Days, leadconfig.Weekdays()[:len(bh.Days)]) {
		days = bh.Days[0] + " to " + bh.Days[len(bh.Days)-1]
	}
	return fmt.Sprintf("%s to %s, %s", bh.Start, bh.End, days)
}

// spacingBounds are the usual spacing's minutes, lowest first.
func spacingBounds(sp []int) (lo, hi int) {
	var m [2]int
	copy(m[:], sp)
	lo, hi = max(0, m[0]), max(0, m[1])
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo, hi
}

func firstEmails(n int) string {
	if n == 1 {
		return "1 first email"
	}
	return fmt.Sprintf("%d first emails", n)
}

func goWord(n int) string {
	if n == 1 {
		return "it goes"
	}
	return "they go"
}

func waitWords2(n int) string {
	if n == 1 {
		return "1 lead waits"
	}
	return fmt.Sprintf("%d leads wait", n)
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// verifyNowPending reports whether a Check email now for batch id is asked and not answered
// yet: its request names the batch, is younger than verifyNowWait, and no check run has
// finished since it was asked.
func (b *backend) verifyNowPending(ctx context.Context, id int64, now time.Time) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(b.cfg.DataDir, runRequestDir, verifyNowAction)) // #nosec G304 -- a fixed name under the data directory
	if errors.Is(err, os.ErrNotExist) || b.cfg.DataDir == "" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var req verifyNowRequest
	if json.Unmarshal(raw, &req) != nil || req.BatchID != id || now.Sub(req.At) > verifyNowWait || req.At.After(now) {
		return false, nil
	}
	var done bool
	if err := b.s.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM run WHERE command = $1 AND started_at >= $2 AND finished_at IS NOT NULL)`,
		commandLeadsVerify, req.At).Scan(&done); err != nil {
		return false, fmt.Errorf("console: verify runs: %w", err)
	}
	return !done, nil
}

// firstEmailElsewhere is, per lead of ids, why its first email is already taken: a first
// email of it in another batch that is not discarded and not cancelled, skipped or refused
// there. That covers a batch approved, sending or sent (shadow_sent included) and one still
// waiting for approval, daily or manual. It reads messages, never the lead's state, which a
// shadow batch leaves new.
func firstEmailElsewhere(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, batchID int64, ids []int64) (map[int64]string, error) {
	rows, err := db.Query(ctx,
		`SELECT m.lead_id, m.batch_id, m.state FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE m.lead_id = ANY($1::bigint[]) AND m.step = 0 AND m.channel = 'email' AND m.batch_id <> $2
		   AND b.discarded_at IS NULL AND m.state NOT IN ('cancelled', 'skipped', 'refused')
		 ORDER BY m.batch_id`, ids, batchID)
	if err != nil {
		return nil, fmt.Errorf("manual send: first emails elsewhere: %w", err)
	}
	type row struct {
		LeadID  int64  `db:"lead_id"`
		BatchID int64  `db:"batch_id"`
		State   string `db:"state"`
	}
	rs, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, fmt.Errorf("manual send: first emails elsewhere: %w", err)
	}
	out := map[int64]string{}
	for _, r := range rs {
		switch r.State {
		case leadstore.MsgSent, "shadow_sent", "maybe_sent", "done":
			out[r.LeadID] = fmt.Sprintf("Its first email already went in batch %d.", r.BatchID)
		default:
			if out[r.LeadID] == "" {
				out[r.LeadID] = fmt.Sprintf("Its first email is already in batch %d.", r.BatchID)
			}
		}
	}
	return out, nil
}
