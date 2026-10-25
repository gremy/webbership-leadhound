package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The leads cores the CLI and the console share (the trackcore.go pattern): each takes its
// inputs as values and returns what it did, or a *LeadRefusal saying why it did nothing. The
// CLI prints from them in its own words; the console renders them in its words, never
// naming a CLI command.

// RefusalKind names why a leads core did nothing.
type RefusalKind int

const (
	RefuseBatchWaiting    RefusalKind = iota + 1 // newBatch: BatchID, of the same kind, still waits for approval
	RefuseNoRoom                                 // newBatch: the ramp has no room for one lead (Sizing)
	RefuseNotLive                                // newBatch: sending is not set up, so the batch would be shadow
	RefuseBatchNotFound                          // BatchID does not exist
	RefuseHashMismatch                           // approveBatch: the hash is not the batch's, or too short
	RefuseBatchChanged                           // approveBatch: the rows no longer hash to what was drafted
	RefuseBadSkip                                // approveBatch: a skip number outside 1..Items
	RefuseBatchDiscarded                         // approveBatch: BatchID was discarded
	RefuseBatchApproved                          // discardBatch: BatchID is approved
	RefuseMessageNotFound                        // MessageID does not exist
	RefuseNotManual                              // markDone: MessageID is an email
	RefuseMessageState                           // markDone, resolveMaybeSent: MessageID is not in a state for the move
	RefuseSendRunning                            // resolveMaybeSent: a send run holds the send lock
	RefuseSelftest                               // resolveMaybeSent: MessageID is a selftest's, never checked by hand
	RefuseNotDomain                              // markContacted: Value is not a domain
	RefuseSuppressReason                         // suppressAdd: the kind or reason is not one the store knows
	RefuseNotRegistrable                         // suppressAdd: Value has no registrable domain
	RefuseBadValue                               // suppressAdd: Value is not a usable address or domain
	RefuseRuleInvalid                            // ruleAdd, ruleDisable: Detail says what is wrong
	RefuseRuleNotFound                           // ruleDisable: RuleID does not exist
	RefuseRuleDisabled                           // ruleDisable: RuleID is already disabled
)

// LeadRefusal is a core's refusal with the facts its words need. Nothing was written.
type LeadRefusal struct {
	Kind      RefusalKind
	BatchID   int64
	MessageID int64
	RuleID    int64
	Items     int         // RefuseBadSkip: the batch's items
	Value     string      // what was given, for the value refusals
	Detail    string      // RefuseRuleInvalid: the store's reason
	Sizing    BatchSizing // RefuseNoRoom
	err       error       // the store's error, when one caused it
}

func (r *LeadRefusal) Error() string {
	if r.err != nil {
		return fmt.Sprintf("leads: refused (%d): %v", r.Kind, r.err)
	}
	return fmt.Sprintf("leads: refused (%d)", r.Kind)
}

func (r *LeadRefusal) Unwrap() error { return r.err }

// asRefusal returns err's refusal, or nil when err is not one.
func asRefusal(err error) *LeadRefusal {
	var r *LeadRefusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// sizingDays is how many sending days a new live batch is sized for.
const sizingDays = 3

// BatchSizing is how many leads a live batch may draft: as many as the ramp can still send
// over the next sizingDays business days once what waits has gone, each lead counting its
// first email and every follow-up.
type BatchSizing struct {
	Days      int  // the business days counted
	Today     bool // today is one of them: its sending window is still to come or open
	SentToday int  // emails today's cap has used (as leads send counts them), when Today
	Ramp      int  // emails the ramp still allows over those days: today's cap less SentToday, then full days
	Waiting   int  // live emails that may still go: approved, plus ToCheck
	ToCheck   int  // of those, maybe sent or left sending: they may go again once checked by hand
	Room      int  // Ramp - Waiting, never below 0
	Steps     int  // emails per lead: the first and its follow-ups
	Size      int  // the most leads asked for (--size, or caps.batch_size)
	Leads     int  // min(Room / Steps, Size)
}

// Words says the sizing rule in plain words, for the CLI and the console alike.
func (z BatchSizing) Words() string {
	days := fmt.Sprintf("the next %d days", z.Days)
	if !z.Today {
		days = fmt.Sprintf("the next %d business days from tomorrow (today's sending is over)", z.Days)
	}
	room := fmt.Sprintf("room for %s over %s", plural(z.Room, "email", "emails"), days)
	var why []string
	if z.Waiting > 0 || (z.Today && z.SentToday > 0) {
		ramp := fmt.Sprintf("%d by the warm-up ramp", z.Ramp)
		if z.Today && z.SentToday > 0 {
			ramp = fmt.Sprintf("%d left in the warm-up ramp after %d sent today", z.Ramp, z.SentToday)
		}
		why = append(why, ramp)
	}
	if n := z.Waiting - z.ToCheck; n > 0 {
		why = append(why, fmt.Sprintf("%d approved and waiting", n))
	}
	if z.ToCheck > 0 {
		why = append(why, fmt.Sprintf("%d to check by hand", z.ToCheck))
	}
	if len(why) > 0 {
		room += " (" + strings.Join(why, ", ") + ")"
	}
	lead := plural(z.Leads, "lead", "leads")
	if z.Leads == z.Size && z.Room/z.Steps > z.Size {
		lead += " (the most asked for)"
	}
	return fmt.Sprintf("%s: %s, and each lead has %s", lead, room, plural(z.Steps, "step", "steps"))
}

// sizingLoad is what batchSizing reads from the store.
type sizingLoad struct {
	firstSend *time.Time // the first live send, in the operator's zone; nil when none has gone
	sentToday int        // leads send's count for today's cap (SentSince the start of today)
	approved  int        // live emails approved and not yet sent
	toCheck   int        // live emails maybe sent or left sending
}

// batchSizing sizes a live batch at now (in the operator's zone) for at most size leads.
// Today counts only while its sending window is still to come or open (todayOpen), with
// what today's sends have used taken off its cap; the other days count their whole cap.
func batchSizing(cfg leadconfig.Config, ld sizingLoad, now time.Time, size int) BatchSizing {
	z := BatchSizing{Waiting: ld.approved + ld.toCheck, ToCheck: ld.toCheck, Steps: 1 + len(cfg.Schedule.FollowupBusinessDays), Size: size}
	firstSend := ld.firstSend
	if firstSend == nil && cfg.Caps.StartDate.IsFirstSend() {
		// No send yet: today is day 0 for every day counted, not each day its own day 0.
		today := now
		firstSend = &today
	}
	days := cfg.Schedule.BusinessHours.Days
	for i := 0; z.Days < sizingDays && i < 15; i++ {
		day := time.Date(now.Year(), now.Month(), now.Day()+i, 12, 0, 0, 0, now.Location())
		if i == 0 {
			if !todayOpen(cfg, now) {
				continue
			}
			z.Days, z.Today, z.SentToday = 1, true, ld.sentToday
			z.Ramp += max(0, outreach.CapOn(cfg, firstSend, day)-ld.sentToday)
			continue
		}
		if !slices.Contains(days, day.Weekday().String()[:3]) {
			continue
		}
		z.Days++
		z.Ramp += outreach.CapOn(cfg, firstSend, day)
	}
	z.Room = max(0, z.Ramp-z.Waiting)
	z.Leads = min(z.Room/z.Steps, size)
	return z
}

// todayOpen reports whether a send could still go out on the operator's calendar day of
// now: whether the next business slot (outreach.NextSlot, the window leads send obeys per
// recipient) of any email country's zone starts before the operator's day ends. That is the
// latest window the send path allows today. With no email country it is the operator's own
// zone.
func todayOpen(cfg leadconfig.Config, now time.Time) bool {
	end := startOfDay(now, now.Location()).AddDate(0, 0, 1)
	var locs []*time.Location
	for _, c := range cfg.Countries {
		if c.Channel == leadconfig.ChannelEmail {
			locs = append(locs, c.Location())
		}
	}
	if len(locs) == 0 {
		locs = []*time.Location{now.Location()}
	}
	for _, loc := range locs {
		if slot := outreach.NextSlot(loc, cfg.Schedule.BusinessHours, now); !slot.IsZero() && slot.Before(end) {
			return true
		}
	}
	return false
}

// leadsZone is the operator's zone, LEADHOUND_TZ: the days the ramp counts.
func leadsZone() (*time.Location, error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return nil, usageError("LEADHOUND_TZ: %v", err)
	}
	return loc, nil
}

// batchSetup is what newBatch drafts with.
type batchSetup struct {
	leads    leadconfig.Config
	shadow   bool           // batchMode: the batch would be shadow
	idDomain string         // the domain its Message-IDs end in
	loc      *time.Location // the operator's zone (LEADHOUND_TZ): the days the ramp counts
}

func newBatchSetup(setup leadsSetup, loc *time.Location) batchSetup {
	shadow, idDomain := batchMode(setup.env)
	return batchSetup{leads: setup.leads, shadow: shadow, idDomain: idDomain, loc: loc}
}

// NewBatchRequest is one `batch new`.
type NewBatchRequest struct {
	Size     int  // most leads; 0 is caps.batch_size
	LiveOnly bool // refuse rather than draft a shadow batch (the console never drafts shadow)
}

// BatchCounts are a batch's first emails, contact forms and LinkedIn notes.
type BatchCounts struct{ Emails, Forms, LinkedIn int }

func (c BatchCounts) String() string {
	return fmt.Sprintf("%s, %s, %s", plural(c.Emails, "email", "emails"),
		plural(c.Forms, "contact form", "contact forms"), plural(c.LinkedIn, "LinkedIn note", "LinkedIn notes"))
}

// DroppedLead is a candidate whose drafts failed the checks.
type DroppedLead struct{ Domain, Kind, Channel, Why string }

func (d DroppedLead) String() string {
	return fmt.Sprintf("%s (%s, %s): %s", d.Domain, d.Kind, d.Channel, d.Why)
}

// NewBatchOutcome is what newBatch did. BatchID 0 means no candidate lead had drafts that
// pass the checks, so nothing was written.
type NewBatchOutcome struct {
	BatchID int64
	Shadow  bool
	Leads   int
	Counts  BatchCounts
	Dropped []DroppedLead
	Sizing  *BatchSizing // a live batch's sizing; nil for shadow
}

// newBatch drafts the next batch with lq, which the caller runs in one transaction: it
// waits for no other batch new (LockBatchNew), refuses while a batch of the same kind waits
// for approval, takes the candidates in order, sizes a live batch to what the ramp can send
// (batchSizing), drafts the whole sequence for each lead, drops a lead whose drafts fail
// Check, and writes the batch. It never approves anything.
func newBatch(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time, req NewBatchRequest) (NewBatchOutcome, error) {
	out := NewBatchOutcome{Shadow: bs.shadow}
	if bs.shadow && req.LiveOnly {
		return out, &LeadRefusal{Kind: RefuseNotLive}
	}
	if err := lq.LockBatchNew(ctx); err != nil {
		return out, err
	}
	if id, ok, err := lq.UnapprovedBatch(ctx, bs.shadow); err != nil {
		return out, err
	} else if ok {
		return out, &LeadRefusal{Kind: RefuseBatchWaiting, BatchID: id}
	}
	n := req.Size
	if n <= 0 {
		n = bs.leads.Caps.BatchSize
	}
	cands, err := lq.CandidateLeads(ctx, leadstore.CandidateOptions{
		KindsPriority: bs.leads.KindsPriority, Profile: bs.leads.PriorityProfile(), Today: now.In(bs.loc), IncludeReserved: bs.shadow || leadsMail.allowReserved,
	})
	if err != nil || len(cands) == 0 {
		return out, err
	}
	if !bs.shadow {
		z, err := liveSizing(ctx, lq, bs, now, n)
		if err != nil {
			return out, err
		}
		out.Sizing = &z
		if z.Leads < 1 {
			return out, &LeadRefusal{Kind: RefuseNoRoom, Sizing: z}
		}
		n = z.Leads
	}
	var drafts []leadstore.DraftRow
	for start := 0; start < len(cands) && out.Leads < n; start += evidenceSpan {
		span := cands[start:min(start+evidenceSpan, len(cands))]
		ids := make([]int64, len(span))
		for i, c := range span {
			ids[i] = c.LeadID
		}
		ev, err := lq.LeadEvidence(ctx, ids)
		if err != nil {
			return out, err
		}
		for _, c := range span {
			if out.Leads >= n {
				break
			}
			ds, err := draftLead(c, ev[c.LeadID], bs.leads, bs.idDomain, rand.Reader)
			if err != nil {
				out.Dropped = append(out.Dropped, DroppedLead{Domain: c.Domain, Kind: c.Kind, Channel: c.Channel, Why: err.Error()})
				continue
			}
			drafts = append(drafts, ds...)
			out.Leads++
		}
	}
	if len(drafts) == 0 {
		return out, nil
	}
	tsha, _ := hex.DecodeString(bs.leads.Templates.SHA256)
	if out.BatchID, _, err = lq.CreateBatch(ctx, bs.shadow, drafts, tsha); err != nil {
		return out, err
	}
	out.Counts = countDrafts(drafts)
	return out, nil
}

// liveSizing reads what batchSizing needs from the store: the waiting emails, today's
// sends as leads send counts them for its cap, and the first live send.
func liveSizing(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time, size int) (BatchSizing, error) {
	now = now.In(bs.loc)
	var (
		ld  sizingLoad
		err error
	)
	if ld.approved, ld.toCheck, err = lq.WaitingEmails(ctx); err != nil {
		return BatchSizing{}, err
	}
	if ld.sentToday, err = lq.SentSince(ctx, startOfDay(now, bs.loc)); err != nil {
		return BatchSizing{}, err
	}
	if bs.leads.Caps.StartDate.IsFirstSend() {
		if ld.firstSend, err = lq.FirstLiveSend(ctx); err != nil {
			return BatchSizing{}, err
		}
		if ld.firstSend != nil {
			fs := ld.firstSend.In(bs.loc)
			ld.firstSend = &fs
		}
	}
	return batchSizing(bs.leads, ld, now, size), nil
}

// makeBatch is one batch new with its run row: newBatch in one transaction, the run noted
// with what it did.
func makeBatch(ctx context.Context, s *store.Store, bs batchSetup, now time.Time, req NewBatchRequest) (NewBatchOutcome, error) {
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsBatch, sourceLeads)
	if err != nil {
		return NewBatchOutcome{}, err
	}
	var out NewBatchOutcome
	runErr := leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		var err error
		out, err = newBatch(ctx, tq, bs, now, req)
		return err
	})
	note := fmt.Sprintf("batch %d, %d messages, %d dropped", out.BatchID, out.Counts.Emails+out.Counts.Forms+out.Counts.LinkedIn, len(out.Dropped))
	switch r := asRefusal(runErr); {
	case r != nil && r.Kind == RefuseBatchWaiting:
		note = fmt.Sprintf("batch %d still waits for approval", r.BatchID)
	case r != nil && r.Kind == RefuseNoRoom:
		note = "no room in the ramp"
	case runErr != nil:
		note = runErr.Error()
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{Note: note}); err != nil && runErr == nil {
		return out, err
	}
	return out, runErr
}

// countDrafts counts first emails, contact-form texts and LinkedIn notes.
func countDrafts(drafts []leadstore.DraftRow) BatchCounts {
	var c BatchCounts
	for _, d := range drafts {
		switch {
		case d.Channel == "email" && d.Step == 0:
			c.Emails++
		case d.Channel == "form":
			c.Forms++
		case d.Channel == "linkedin":
			c.LinkedIn++
		}
	}
	return c
}

// BatchView is one batch as it stands now, to read in full before approving.
type BatchView struct {
	Batch         leadstore.Batch
	Items         []BatchItem
	Evidence      map[int64]leadstore.Evidence
	ContentSHA256 []byte // recomputed from the rows now
	Changed       bool   // the rows no longer hash to what batch new stored: never approvable
}

// Approvable reports whether the view may carry the approve form or line.
func (v BatchView) Approvable() bool {
	return !v.Changed && v.Batch.ApprovedAt == nil && v.Batch.DiscardedAt == nil
}

// HashPrefix is the hash an approval must name.
func (v BatchView) HashPrefix() string {
	return hex.EncodeToString(v.Batch.ContentSHA256)[:leadstore.MinHashPrefix]
}

// batchView reads batch id whole: its items numbered as approval's skip list, their
// evidence, and its content hash recomputed now.
func batchView(ctx context.Context, lq leadstore.Queries, id int64) (BatchView, error) {
	b, err := lq.BatchByID(ctx, id)
	if errors.Is(err, leadstore.ErrBatchNotFound) {
		return BatchView{}, &LeadRefusal{Kind: RefuseBatchNotFound, BatchID: id, err: err}
	}
	if err != nil {
		return BatchView{}, err
	}
	msgs, err := lq.BatchMessages(ctx, id)
	if err != nil {
		return BatchView{}, err
	}
	items, err := batchItems(ctx, lq, msgs)
	if err != nil {
		return BatchView{}, err
	}
	leadIDs := make([]int64, len(items))
	for i, it := range items {
		leadIDs[i] = it.Lead.ID
	}
	ev, err := lq.LeadEvidence(ctx, leadIDs)
	if err != nil {
		return BatchView{}, err
	}
	sum, err := lq.BatchContentSHA256(ctx, id)
	if err != nil {
		return BatchView{}, err
	}
	return BatchView{Batch: b, Items: items, Evidence: ev, ContentSHA256: sum, Changed: !bytes.Equal(sum, b.ContentSHA256)}, nil
}

// ApproveRequest is one approval: the batch, the hash its reader was shown, and the item
// numbers left out.
type ApproveRequest struct {
	ID         int64
	HashPrefix string
	Skip       []int
}

// ApproveOutcome is what approveBatch did.
type ApproveOutcome struct {
	BatchID  int64
	Shadow   bool
	Items    int
	Approval leadstore.Approval
	FirstDue time.Time // the earliest first email's due time; zero when none
	Manual   []int64   // the approved contact forms and LinkedIn notes, to use by hand
}

// approveBatch approves what the operator read (the hash binds it, RA2): each first email is
// due at the next business slot in its recipient's time zone; follow-ups and the manual
// items have no due time. A wrong hash, a changed or discarded batch and a bad skip number
// are refusals, and nothing is approved then.
func approveBatch(ctx context.Context, s *store.Store, cfg leadconfig.Config, now func() time.Time, req ApproveRequest) (ApproveOutcome, error) {
	lq := leadstore.NewQueries(s.Pool())
	out := ApproveOutcome{BatchID: req.ID}
	b, err := lq.BatchByID(ctx, req.ID)
	if errors.Is(err, leadstore.ErrBatchNotFound) {
		return out, &LeadRefusal{Kind: RefuseBatchNotFound, BatchID: req.ID, err: err}
	}
	if err != nil {
		return out, err
	}
	out.Shadow = b.Shadow
	msgs, err := lq.BatchMessages(ctx, req.ID)
	if err != nil {
		return out, err
	}
	items, err := batchItems(ctx, lq, msgs)
	if err != nil {
		return out, err
	}
	out.Items = len(items)
	locs := map[int64]*time.Location{}
	for _, it := range items {
		locs[it.Lead.ID] = cfg.Policy(it.Org.Country).Location()
	}
	t := now()
	var manual []int64
	due := func(m leadstore.Message) time.Time {
		if m.Channel != "email" {
			manual = append(manual, m.ID)
			return time.Time{}
		}
		if m.Step != 0 {
			return time.Time{}
		}
		d := outreach.NextSlot(locs[m.LeadID], cfg.Schedule.BusinessHours, t)
		if !d.IsZero() && (out.FirstDue.IsZero() || d.Before(out.FirstDue)) {
			out.FirstDue = d
		}
		return d
	}
	err = leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		var err error
		out.Approval, err = tq.ApproveBatch(ctx, req.ID, req.HashPrefix, req.Skip, due)
		return err
	})
	refuse := func(k RefusalKind) (ApproveOutcome, error) {
		return ApproveOutcome{BatchID: req.ID, Shadow: b.Shadow, Items: len(items)},
			&LeadRefusal{Kind: k, BatchID: req.ID, Items: len(items), err: err}
	}
	switch {
	case errors.Is(err, leadstore.ErrHashMismatch), errors.Is(err, leadstore.ErrHashTooShort):
		return refuse(RefuseHashMismatch)
	case errors.Is(err, leadstore.ErrBatchTampered):
		return refuse(RefuseBatchChanged)
	case errors.Is(err, leadstore.ErrBadSkip):
		return refuse(RefuseBadSkip)
	case errors.Is(err, leadstore.ErrBatchDiscarded):
		return refuse(RefuseBatchDiscarded)
	case errors.Is(err, leadstore.ErrBatchNotFound):
		return refuse(RefuseBatchNotFound)
	case err != nil:
		return out, err
	}
	out.Manual = manual // nil for a batch approved before: due never ran
	return out, nil
}

// DiscardOutcome is what discardBatch did.
type DiscardOutcome struct {
	BatchID int64
	leadstore.Discard
}

// discardBatch throws away batch id before approval, in one transaction (DiscardBatch): its
// pending messages cancelled and its leads back to new. An approved batch is refused.
func discardBatch(ctx context.Context, s *store.Store, id int64) (DiscardOutcome, error) {
	out := DiscardOutcome{BatchID: id}
	err := leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		var err error
		out.Discard, err = tq.DiscardBatch(ctx, id)
		return err
	})
	switch {
	case errors.Is(err, leadstore.ErrBatchNotFound):
		return out, &LeadRefusal{Kind: RefuseBatchNotFound, BatchID: id, err: err}
	case errors.Is(err, leadstore.ErrBatchApproved):
		return out, &LeadRefusal{Kind: RefuseBatchApproved, BatchID: id, err: err}
	}
	return out, err
}

// ResolveOutcome is what resolveMaybeSent did.
type ResolveOutcome struct {
	MessageID int64
	Sent      bool // the operator found it in the Sent folder
	leadstore.Resolved
}

// resolveMaybeSent records the operator's check by hand of live email id (maybe_sent, or
// left sending by a broken run): sent (it is sent at its attempt time, its lead contacted,
// its next step scheduled as after any send) or not sent (back to approved, no attempt
// time). It runs only while holding the send lock, because a sending row is only known
// stuck while no send runs; a send in progress is a refusal.
func resolveMaybeSent(ctx context.Context, s *store.Store, setup leadsSetup, loc *time.Location, id int64, sent bool) (ResolveOutcome, error) {
	out := ResolveOutcome{MessageID: id, Sent: sent}
	lock, err := leadstore.TryLock(ctx, s.Pool(), leadstore.SendLockKey)
	if errors.Is(err, leadstore.ErrLockHeld) {
		return out, &LeadRefusal{Kind: RefuseSendRunning, MessageID: id, err: err}
	}
	if err != nil {
		return out, err
	}
	defer lock.Release()
	err = leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		if !sent {
			var err error
			out.Resolved, err = tq.ResolveNotSent(ctx, id)
			return err
		}
		r, err := tq.ResolveSent(ctx, id)
		if err != nil {
			return err
		}
		out.Resolved = r
		row, err := tq.SendRowByID(ctx, id)
		if err != nil {
			return err
		}
		// The next step's due time from the same code a send uses (scheduleNext reads only
		// the setup and the zone).
		return (&sendRun{setup: setup, loc: loc}).scheduleNext(ctx, tq, row, r.SentAt)
	})
	switch {
	case errors.Is(err, leadstore.ErrMessageNotFound):
		return out, &LeadRefusal{Kind: RefuseMessageNotFound, MessageID: id, err: err}
	case errors.Is(err, leadstore.ErrMessageState):
		return out, &LeadRefusal{Kind: RefuseMessageState, MessageID: id, err: err}
	case errors.Is(err, leadstore.ErrSelftestMessage):
		return out, &LeadRefusal{Kind: RefuseSelftest, MessageID: id, err: err}
	}
	return out, err
}

// markDone records that the operator used approved contact-form or LinkedIn message id.
func markDone(ctx context.Context, s *store.Store, id int64) (leadstore.Done, error) {
	var d leadstore.Done
	err := leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		d, err = lq.MarkDone(ctx, id, leadstore.ActorGremy)
		return err
	})
	switch {
	case errors.Is(err, leadstore.ErrMessageNotFound):
		return d, &LeadRefusal{Kind: RefuseMessageNotFound, MessageID: id, err: err}
	case errors.Is(err, leadstore.ErrNotManual):
		return d, &LeadRefusal{Kind: RefuseNotManual, MessageID: id, err: err}
	case errors.Is(err, leadstore.ErrMessageState):
		return d, &LeadRefusal{Kind: RefuseMessageState, MessageID: id, err: err}
	}
	return d, err
}

// The sources a suppression added by hand records.
const (
	suppressFromCLI     = "cli"
	suppressFromConsole = "console"
)

// suppressAdd records that kind value must never be written to again, with reason, and
// applies it (leadstore.Suppress). source is suppressFromCLI or suppressFromConsole.
func suppressAdd(ctx context.Context, s *store.Store, kind, value, reason, source string) (leadstore.Suppressed, error) {
	var res leadstore.Suppressed
	err := leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		res, err = lq.Suppress(ctx, kind, value, reason, source)
		return err
	})
	switch {
	case errors.Is(err, leadstore.ErrUnknownSuppression):
		return res, &LeadRefusal{Kind: RefuseSuppressReason, Value: reason, err: err}
	case errors.Is(err, leadstore.ErrSuppressNotRegistrable):
		return res, &LeadRefusal{Kind: RefuseNotRegistrable, Value: value, err: err}
	case errors.Is(err, leadstore.ErrBadSuppressValue):
		return res, &LeadRefusal{Kind: RefuseBadValue, Value: value, err: err}
	}
	return res, err
}

// markContacted records that the operator reached domain by hand (RA15), so no batch drafts
// it again.
func markContacted(ctx context.Context, s *store.Store, domain, note string) (leadstore.Contacted, error) {
	var c leadstore.Contacted
	err := leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		c, err = lq.MarkContacted(ctx, domain, note)
		return err
	})
	if errors.Is(err, leadstore.ErrNoDomain) {
		return c, &LeadRefusal{Kind: RefuseNotDomain, Value: domain, err: err}
	}
	return c, err
}

// RuleAddRequest is one rule to add.
type RuleAddRequest struct {
	Kind, Field, Pattern, Reason string
	FromJob                      *int64 // the job the rule was made from, when there is one
}

// ruleAdd adds a rule, validated by the store (kind, field, an RE2 pattern that compiles
// case-insensitively, a reason), and returns its id.
func ruleAdd(ctx context.Context, s *store.Store, req RuleAddRequest) (int64, error) {
	id, err := s.Q().AddRule(ctx, req.Kind, req.Field, req.Pattern, req.Reason, req.FromJob)
	if errors.Is(err, store.ErrInvalidRule) {
		return 0, &LeadRefusal{Kind: RefuseRuleInvalid, Detail: err.Error(), err: err}
	}
	return id, err
}

// ruleDisable disables rule id with reason; it is never deleted, and adding it again undoes
// that.
func ruleDisable(ctx context.Context, s *store.Store, id int64, reason string) error {
	err := s.Q().DisableRule(ctx, id, reason)
	switch {
	case errors.Is(err, store.ErrInvalidRule):
		return &LeadRefusal{Kind: RefuseRuleInvalid, RuleID: id, Detail: err.Error(), err: err}
	case errors.Is(err, store.ErrNotFound):
		return &LeadRefusal{Kind: RefuseRuleNotFound, RuleID: id, err: err}
	case errors.Is(err, store.ErrRuleDisabled):
		return &LeadRefusal{Kind: RefuseRuleDisabled, RuleID: id, err: err}
	}
	return err
}
