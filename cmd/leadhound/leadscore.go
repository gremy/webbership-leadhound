package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
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
	RefuseNoStarred                              // newBatch: batches draw from starred leads and none is ready (D6)
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
	RefuseNoSuppression                          // liftSuppression: SuppressionID does not exist
	RefuseAlreadyLifted                          // liftSuppression: SuppressionID was lifted before (Lifted holds when and where)
	RefuseLiftDomain                             // liftSuppression: Value, the domain typed to confirm, is not the suppression's
	RefuseLiftReason                             // liftSuppression: the reason is not 3 to 300 characters on one line without an @
	RefuseRuleInvalid                            // ruleAdd, ruleDisable: Detail says what is wrong
	RefuseRuleNotFound                           // ruleDisable: RuleID does not exist
	RefuseRuleDisabled                           // ruleDisable: RuleID is already disabled
	RefuseSettingsIgnored                        // newBatch: the saved settings fail leads.yaml's checks, so kinds_off and the rest are not applied
)

// LeadRefusal is a core's refusal with the facts its words need. Nothing was written.
type LeadRefusal struct {
	Kind      RefusalKind
	BatchID   int64
	MessageID int64
	RuleID    int64
	// SuppressionID is the suppression a lift refusal is about.
	SuppressionID int64
	// Lifted is the suppression as it stands, for RefuseAlreadyLifted's words.
	Lifted leadstore.SuppressionRow
	Items  int         // RefuseBadSkip: the batch's items
	Value  string      // what was given, for the value refusals
	Detail string      // RefuseRuleInvalid: the store's reason
	Sizing BatchSizing // RefuseNoRoom
	// Starred is RefuseNoStarred's every starred lead in the operator's order, each with why
	// it is out (leadstore.StarredStatus; a lead whose org is open in another campaign reads
	// starredOpenElsewhere). noStarredText words it.
	Starred []leadstore.StarredState
	err     error // the store's error, when one caused it
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
	Leads     int  // min(Room / Steps, Size); over mailboxes, the leads Assign can place
	Mailboxes int  // the mailboxes ready for new leads the room is counted over (0: one, before mailboxes)
}

// Words says the sizing rule in plain words, for the CLI and the console alike.
func (z BatchSizing) Words() string {
	days := fmt.Sprintf("the next %d days", z.Days)
	if !z.Today {
		days = fmt.Sprintf("the next %d business days from tomorrow (today's sending is over)", z.Days)
	}
	room := fmt.Sprintf("room for %s over %s", plural(z.Room, "email", "emails"), days)
	if z.Mailboxes > 1 {
		room += fmt.Sprintf(" on %d mailboxes", z.Mailboxes)
	}
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
	// steps is how many emails each lead counts against the ramp: the email steps of the
	// batch's sequence (a call step sends nothing). 0: the legacy sequence's 1 +
	// len(followup_business_days).
	steps int
}

// batchSizing sizes a live batch at now (in the operator's zone) for at most size leads.
// Today counts only while its sending window is still to come or open (todayOpen), with
// what today's sends have used taken off its cap; the other days count their whole cap.
func batchSizing(cfg leadconfig.Config, ld sizingLoad, now time.Time, size int) BatchSizing {
	steps := ld.steps
	if steps <= 0 {
		steps = 1 + len(cfg.Schedule.FollowupBusinessDays)
	}
	z := BatchSizing{Waiting: ld.approved + ld.toCheck, ToCheck: ld.toCheck, Steps: steps, Size: size}
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

// mailboxSizing is batchSizing over the sending mailboxes (decision 7): each mailbox's room
// over the same days is outreach.RoomOver (its own ramp from its own first send, then the
// domain's caps.domain_per_day), and the leads are those outreach.Assign can place, steps
// each. A mailbox that is not Ready gets no room. Ramp is the room before the waiting mail;
// Waiting, SentToday and ToCheck (toCheck, the store's count of mail to check by hand) are
// the ready mailboxes'. With exactly one ready mailbox it is batchSizing for that mailbox's
// load, field for field, Mailboxes aside (MB-R7). It returns each mailbox's room too.
func mailboxSizing(cfg leadconfig.Config, mbs []outreach.MailboxLoad, toCheck int, now time.Time, size, steps int) (BatchSizing, map[int64]int) {
	ld := sizingLoad{steps: steps}
	ready := 0
	bare := slices.Clone(mbs)
	for i, m := range mbs {
		bare[i].Waiting = 0
		if m.Ready {
			ready++
			ld.sentToday += m.SentToday
			ld.approved += m.Waiting
		}
	}
	ld.toCheck = min(max(toCheck, 0), ld.approved)
	ld.approved -= ld.toCheck
	// The days counted, the steps and the waiting mail, as the single-mailbox sizing has them.
	z := batchSizing(cfg, ld, now, size)
	z.Mailboxes, z.Ramp, z.Room = ready, 0, 0
	for _, r := range outreach.RoomOver(cfg, bare, now, sizingDays, cfg.Schedule.BusinessHours) {
		z.Ramp += r
	}
	room := outreach.RoomOver(cfg, mbs, now, sizingDays, cfg.Schedule.BusinessHours)
	for _, r := range room {
		z.Room += r
	}
	leads := make([]int64, max(size, 0))
	for i := range leads {
		leads[i] = int64(i + 1)
	}
	z.Leads = len(outreach.Assign(leads, room, z.Steps))
	return z, room
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

// batchSetup is what newBatch drafts with. Each draft's Message-ID ends in its mailbox's
// domain.
type batchSetup struct {
	leads  leadconfig.Config
	shadow bool           // batchMode: the batch would be shadow
	loc    *time.Location // the operator's zone (LEADHOUND_TZ): the days the ramp counts
	// dataDir is LEADHOUND_DATA_DIR, where a campaign's own folder is read (resolveBatchPlan);
	// "" drafts every campaign on the legacy plan.
	dataDir string
	// settingsIgnored: the saved settings no longer pass leads.yaml's checks, so leads is the
	// file alone, without the operator's kinds_off, skip_rank_over and the rest.
	settingsIgnored bool
	// claude is what a writer: claude campaign may draft with (leads batch new sets it); nil
	// drafts every lead from templates (the console), counted as Claude drafting off.
	claude *claudeDrafting
}

func newBatchSetup(setup leadsSetup, loc *time.Location) batchSetup {
	return batchSetup{leads: setup.leads, shadow: batchMode(setup.env), loc: loc, dataDir: setup.dataDir, settingsIgnored: len(setup.settings.Ignored) > 0}
}

// NewBatchRequest is one `batch new`.
type NewBatchRequest struct {
	Size     int  // most leads; 0 is caps.batch_size
	LiveOnly bool // refuse rather than draft a shadow batch (the console never drafts shadow)
	// CampaignID is the campaign whose leads the batch drafts; 0 is the legacy Drupal
	// campaign (leadstore.DefaultCampaignID).
	CampaignID int64
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
	// Skipped are the campaign's new leads whose business is already in a conversation in
	// another campaign of the account (one business, one conversation), each with why.
	Skipped []DroppedLead
	// Calls are the call steps drafted; CallsLeftOut counts the email leads whose call steps
	// (and the steps that wait on a call) were left out, by why (callNoNumber,
	// callSuppressed). CallsOff counts the call steps drafted in a country whose call policy
	// is off in the campaign's rules, by country code ("" unknown): the operator's call. All
	// stay empty when the sequence has no call step.
	Calls        int
	CallsLeftOut map[string]int
	CallsOff     map[string]int
	// ClaudeDrafted counts the leads whose sequence Claude wrote (a writer: claude campaign the
	// gate trusts); Fallbacks counts that campaign's email leads drafted from templates
	// instead, by why (fbCap, fbRefused, fbUntrusted …). fallbackLine words them.
	ClaudeDrafted int
	Fallbacks     map[string]int
	Sizing        *BatchSizing  // a live batch's sizing; nil for shadow
	Rooms         map[int64]int // a live batch's room per mailbox over the sizing window, by mailbox id
}

// newBatch drafts the next batch with lq, which the caller runs in one transaction: it
// waits for no other batch new (LockBatchNew), refuses while a batch of the same kind waits
// for approval, refuses with no active mailbox, takes the candidates in order, sizes a live
// batch to what the ready mailboxes can send (mailboxSizing), gives each lead the mailbox
// with the most room left (outreach.Assign; its follow-ups inherit it, decision 6), drafts
// the whole sequence for each lead, drops a lead whose drafts fail Check, and writes the
// batch. It never approves anything, never reaches the network (the console calls it) and
// never opens a password; a live batch reads only the DNS checks already recorded today
// (runBatchNew runs them first). A shadow batch reads no readiness: every active mailbox
// takes leads.
func newBatch(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time, req NewBatchRequest) (NewBatchOutcome, error) {
	out := NewBatchOutcome{Shadow: bs.shadow}
	if bs.shadow && req.LiveOnly {
		return out, &LeadRefusal{Kind: RefuseNotLive}
	}
	// Ignored settings would draft from the file's kinds and ranks, so a kind the operator
	// turned off could be drafted: no batch until the settings are fixed or reset.
	if bs.settingsIgnored {
		return out, &LeadRefusal{Kind: RefuseSettingsIgnored}
	}
	if err := lq.LockBatchNew(ctx); err != nil {
		return out, err
	}
	campaignID := cmp.Or(req.CampaignID, leadstore.DefaultCampaignID)
	if id, ok, err := lq.UnapprovedBatch(ctx, leadstore.DefaultAccountID, bs.shadow); err != nil {
		return out, err
	} else if ok {
		return out, &LeadRefusal{Kind: RefuseBatchWaiting, BatchID: id}
	}
	n := req.Size
	if n <= 0 {
		n = bs.leads.Caps.BatchSize
	}
	mbs, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(now, bs.loc))
	if err != nil {
		return out, err
	}
	byID := map[int64]leadstore.MailboxStatus{}
	for _, m := range mbs {
		if m.State == leadstore.MailboxActive {
			byID[m.ID] = m
		}
	}
	if len(byID) == 0 {
		return out, refused("Add a mailbox first: Mailboxes in the console, or leads mailboxes add.")
	}
	plan, err := resolveBatchPlan(ctx, lq, bs, campaignID)
	if err != nil {
		return out, err
	}
	steps := plan.emailSteps()
	drafter, err := newBatchDrafter(ctx, lq, bs, plan, campaignID)
	if err != nil {
		return out, err
	}
	opts, err := batchCandidateOptions(ctx, lq, bs, now)
	if err != nil {
		return out, err
	}
	opts.CampaignID, opts.AccountID, opts.Countries = campaignID, leadstore.DefaultAccountID, plan.candidateCountries()
	opts.PhoneOnlyCountries = plan.phoneOnlyCountries()
	cands, err := lq.CandidateLeads(ctx, opts)
	if err != nil {
		return out, err
	}
	var elsewhere map[int64]bool
	if out.Skipped, elsewhere, err = crossCampaignSkips(ctx, lq, campaignID); err != nil {
		return out, err
	}
	cands = slices.DeleteFunc(cands, func(c leadstore.Candidate) bool { return elsewhere[c.OrgID] })
	if len(cands) == 0 {
		if opts.StarredOnly {
			st, err := lq.StarredStatus(ctx, leadstore.StarredStatusOptions{CandidateOptions: opts, Countries: countryChannels(bs.leads)})
			if err != nil {
				return out, err
			}
			return out, &LeadRefusal{Kind: RefuseNoStarred, Starred: markOpenElsewhere(st, elsewhere, out.Skipped)}
		}
		return out, nil
	}
	room := map[int64]int{}
	if bs.shadow {
		for id := range byID {
			room[id] = n * steps
		}
	} else {
		z, r, err := liveSizing(ctx, lq, bs, now, n, steps, mbs)
		if err != nil {
			return out, err
		}
		if z.Mailboxes == 0 {
			return out, notReadyRefusal(ctx, lq, mbs, startOfDay(now, bs.loc))
		}
		out.Sizing, out.Rooms = &z, r
		if z.Leads < 1 {
			return out, &LeadRefusal{Kind: RefuseNoRoom, Sizing: z}
		}
		room, n = maps.Clone(r), z.Leads
	}
	var drafts []leadstore.DraftRow
	suppressedPhones := map[string]bool{}
	if plan.hasCall() {
		if suppressedPhones, err = suppressedPhoneSet(ctx, lq); err != nil {
			return out, err
		}
	}
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
		facts := map[int64][]leadstore.Fact{}
		if plan.hasCall() {
			orgs := make([]int64, len(span))
			for i, c := range span {
				orgs[i] = c.OrgID
			}
			// A phone fact marked wrong is never drafted again (UsableOrgFacts leaves it out).
			fs, err := lq.UsableOrgFacts(ctx, orgs)
			if err != nil {
				return out, err
			}
			for _, f := range fs {
				facts[f.OrgID] = append(facts[f.OrgID], f)
			}
		}
		for _, c := range span {
			if out.Leads >= n {
				break
			}
			id, ok := outreach.Assign([]int64{c.LeadID}, room, steps)[c.LeadID]
			if !ok {
				break // no mailbox has room for one more lead
			}
			mb := byID[id]
			var phone *leadstore.Fact
			leftOut := ""
			if plan.hasCall() && (c.Channel == leadconfig.ChannelEmail || c.Channel == leadconfig.ChannelNone) {
				// has_callable_phone (callPhone; the Claude sample and eval use the same rule).
				if phone, leftOut, err = callPhone(ctx, lq, plan, c, facts[c.OrgID], suppressedPhones, now); err != nil {
					return out, err
				}
			}
			// A form lead keeps its contact-form text whatever the country: only an email lead
			// moves to call first.
			leadPlan, seqName := plan, ""
			if c.Channel == leadconfig.ChannelEmail || c.Channel == leadconfig.ChannelNone {
				leadPlan, seqName = plan.forLead(c.Country)
			}
			if c.Channel == leadconfig.ChannelNone && seqName == "" {
				// A lead with no route is offered only for a call-first country; this one is not.
				out.Dropped = append(out.Dropped, DroppedLead{Domain: c.Domain, Kind: c.Kind, Channel: c.Channel,
					Why: "no route, and email to this country needs no call first"})
				continue
			}
			if seqName != "" && phone == nil {
				// Email to this country needs a yes won on a call: no call, no contact at all.
				out.Dropped = append(out.Dropped, DroppedLead{Domain: c.Domain, Kind: c.Kind, Channel: c.Channel,
					Why: "call first (email needs their yes on a call): " + cmp.Or(leftOut, callNoNumber)})
				continue
			}
			ds, err := drafter.draft(ctx, lq, leadPlan, c, ev[c.LeadID], bs.leads, phone, outreach.MessageIDDomain(mb.Address), rand.Reader)
			if sb := (*stopBatchError)(nil); errors.As(err, &sb) {
				return out, err
			}
			if err != nil {
				out.Dropped = append(out.Dropped, DroppedLead{Domain: c.Domain, Kind: c.Kind, Channel: c.Channel, Why: err.Error()})
				continue
			}
			if leftOut != "" {
				if out.CallsLeftOut == nil {
					out.CallsLeftOut = map[string]int{}
				}
				out.CallsLeftOut[leftOut]++
			}
			if seqName != "" {
				if plan.sequences[seqName] == nil {
					plan.sequences[seqName] = plan.callFirst
				}
				for i := range ds {
					ds[i].SequenceName = seqName
				}
			}
			for _, d := range ds {
				if d.Channel == leadstore.ChannelPhone {
					out.Calls++
					if campaignCallsOff(plan.overrides, c.Country) {
						if out.CallsOff == nil {
							out.CallsOff = map[string]int{}
						}
						out.CallsOff[countryCode(c.Country)]++
					}
				}
			}
			from := cmp.Or(strings.TrimSpace(mb.FromName), bs.leads.Sender.Name)
			for i := range ds {
				ds[i].MailboxID, ds[i].FromName = mb.ID, from
			}
			room[id] -= steps
			drafts = append(drafts, ds...)
			out.Leads++
		}
	}
	if len(drafter.fallbacks) > 0 {
		out.Fallbacks = drafter.fallbacks
	}
	out.ClaudeDrafted = drafter.drafted
	if len(drafts) == 0 {
		return out, nil
	}
	tsha, _ := hex.DecodeString(plan.templates.SHA256)
	meta := leadstore.BatchMeta{CampaignID: campaignID, Sequences: plan.sequences}
	if out.BatchID, _, err = lq.CreateBatchFor(ctx, meta, bs.shadow, drafts, tsha); err != nil {
		return out, err
	}
	out.Counts = countDrafts(drafts)
	return out, nil
}

// crossCampaignSkips are the new leads of campaign campaignID whose org already has a
// conversation in another campaign of the operator's account (leadstore.OpenElsewhere: a
// reply, or a message approved, sending or maybe sent), by domain, each with why, and those
// orgs. A batch never drafts them: one business, one conversation.
func crossCampaignSkips(ctx context.Context, lq leadstore.Queries, campaignID int64) ([]DroppedLead, map[int64]bool, error) {
	leads, err := lq.SharedNewLeads(ctx, leadstore.DefaultAccountID, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if len(leads) == 0 {
		return nil, nil, nil
	}
	ids := make([]int64, len(leads))
	for i, l := range leads {
		ids[i] = l.OrgID
	}
	why, err := lq.OpenElsewhere(ctx, leadstore.DefaultAccountID, campaignID, ids)
	if err != nil {
		return nil, nil, err
	}
	var out []DroppedLead
	orgs := map[int64]bool{}
	for _, l := range leads {
		if w, ok := why[l.OrgID]; ok {
			out = append(out, DroppedLead{Domain: l.Domain, Kind: l.Kind, Channel: l.Channel, Why: w})
			orgs[l.OrgID] = true
		}
	}
	return out, orgs, nil
}

// batchCandidateOptions is the queue a batch drafts from (D6, D11): starred leads only
// unless batches_from is starred_then_rest, without the kinds turned off or the site leads
// skip_rank_over leaves out, and the unstarred by relevance only once relevance passed its
// trust gate (D9), as the Leads list sorts them. bs.leads is the effective config (leadsOpen
// and backend.leadsSetup apply the console's saved settings), so batches_from, kinds_off,
// kinds_priority and skip_rank_over are the values in force, not the file's alone.
func batchCandidateOptions(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time) (leadstore.CandidateOptions, error) {
	e, ok, err := lq.LatestRelevanceEval(ctx, relevanceQuestion)
	if err != nil {
		return leadstore.CandidateOptions{}, err
	}
	return leadstore.CandidateOptions{
		KindsPriority: bs.leads.KindsPriority, Profile: bs.leads.PriorityProfile(), Today: now.In(bs.loc),
		IncludeReserved: bs.shadow || leadsMail.allowReserved,
		StarredOnly:     bs.leads.BatchesFrom() == leadconfig.BatchesStarred,
		KindsOff:        bs.leads.KindsOff, SkipRankOver: bs.leads.SkipRankOver,
		SortByRelevance: ok && e.Trusted,
	}, nil
}

// starredOut is every starred lead of campaignID a batch would not take now, each with its
// reason, by the batch's own rules (its plan's countries, the candidate options, the
// cross-campaign skips): the Outreach page's standing list, which a shadow-only server
// cannot reach through "Make the next batch now".
func starredOut(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time, campaignID int64) ([]leadstore.StarredState, error) {
	plan, err := resolveBatchPlan(ctx, lq, bs, campaignID)
	if err != nil {
		return nil, err
	}
	opts, err := batchCandidateOptions(ctx, lq, bs, now)
	if err != nil {
		return nil, err
	}
	opts.CampaignID, opts.AccountID, opts.Countries = campaignID, leadstore.DefaultAccountID, plan.candidateCountries()
	skipped, elsewhere, err := crossCampaignSkips(ctx, lq, campaignID)
	if err != nil {
		return nil, err
	}
	st, err := lq.StarredStatus(ctx, leadstore.StarredStatusOptions{CandidateOptions: opts, Countries: countryChannels(bs.leads)})
	if err != nil {
		return nil, err
	}
	st = markOpenElsewhere(st, elsewhere, skipped)
	return slices.DeleteFunc(st, func(s leadstore.StarredState) bool { return s.Reason == leadstore.StarredReady }), nil
}

// noStarredWords is RefuseNoStarred in plain words when nothing is starred, for the CLI and
// the console alike.
const noStarredWords = "No starred leads are ready: star some on Leads."

// noStarredHead heads RefuseNoStarred's list when some leads are starred.
const noStarredHead = "No starred leads are ready. Why each starred lead is out:"

// starredOpenElsewhere is the reason markOpenElsewhere gives a starred lead the batch would
// take but for its org's conversation in another campaign (crossCampaignSkips).
const starredOpenElsewhere = "open_elsewhere"

// countryChannels is leads.yaml's country rule as StarredStatus reads it: each country's
// channel policy (a country missing from it is off, as cfg.Policy has it).
func countryChannels(cfg leadconfig.Config) map[string]string {
	out := make(map[string]string, len(cfg.Countries))
	for code, c := range cfg.Countries {
		out[code] = c.Channel
	}
	return out
}

// markOpenElsewhere gives the starred leads the candidate query offered but
// crossCampaignSkips removed (their org in elsewhere) the reason starredOpenElsewhere, with
// the skip's own words as the detail.
func markOpenElsewhere(st []leadstore.StarredState, elsewhere map[int64]bool, skipped []DroppedLead) []leadstore.StarredState {
	why := map[string]string{}
	for _, d := range skipped {
		why[d.Domain] = d.Why
	}
	for i, s := range st {
		if elsewhere[s.OrgID] && (s.Reason == leadstore.StarredReady || s.Reason == leadstore.StarredRankedOut) {
			st[i].Reason, st[i].Detail, st[i].Date = starredOpenElsewhere, why[s.Domain], nil
		}
	}
	return st
}

// noStarredText is RefuseNoStarred in plain words, for the CLI and the console alike:
// noStarredWords when nothing is starred, else noStarredHead and one line per starred lead,
// "<place>. <domain> — <reason>[: detail][, date]", in the operator's order.
func noStarredText(r *LeadRefusal) string {
	if r == nil || len(r.Starred) == 0 {
		return noStarredWords
	}
	var b strings.Builder
	b.WriteString(noStarredHead)
	for i, s := range r.Starred {
		fmt.Fprintf(&b, "\n%d. %s — %s", i+1, s.Domain, starredLine(s))
	}
	return b.String()
}

// starredLine is one starred lead's reason, detail and date in plain words.
func starredLine(s leadstore.StarredState) string {
	var reason, date string
	switch s.Reason {
	case leadstore.StarredReady:
		reason = "ready"
	case leadstore.StarredNoEmailYet:
		reason = "no email yet"
	case leadstore.StarredLooking:
		reason = "looking for a contact"
	case leadstore.StarredFoundNothing:
		reason = "research found nothing"
	case leadstore.StarredCountryRule:
		reason = "country rule"
	case leadstore.StarredSuppressed:
		reason = "suppressed"
	case leadstore.StarredDeferred:
		reason = "deferred"
	case leadstore.StarredRankedOut:
		reason = "ranked out"
	case leadstore.StarredNotNew:
		reason = "no longer new"
	case starredOpenElsewhere:
		reason = "open in another campaign"
	default:
		reason = "left out"
	}
	if s.Date != nil {
		date = plainDate(*s.Date)
		if s.Reason == leadstore.StarredDeferred {
			date = "until " + date
		}
	}
	out := reason
	if s.Detail != "" {
		out += ": " + s.Detail
	}
	if date != "" {
		out += ", " + date
	}
	return out
}

// settingsIgnoredWords is RefuseSettingsIgnored in plain words, for the CLI and the console
// alike.
const settingsIgnoredWords = "Your saved settings no longer fit leads.yaml, so they are not applied: open Leads → Settings to fix or reset them before making a batch."

// readyForLeads is decision 5's readiness to be given new leads: active, its latest check
// passed, and a DNS pass today for its domain and selector (mbs was read from the start of
// the operator's day).
func readyForLeads(m leadstore.MailboxStatus) bool {
	return m.State == leadstore.MailboxActive && m.LastCheck != nil && m.LastCheck.Outcome == leadstore.CheckPassed && m.DNSPassedToday
}

// liveSizing sizes a live batch over mbs (every mailbox, so a paused one's sends today still
// count toward its domain): each mailbox's waiting emails, today's sends as leads send counts
// them for its cap and, with caps.start_date first-send, its first live send; only those
// readyForLeads get room. It reads the store only.
func liveSizing(ctx context.Context, lq leadstore.Queries, bs batchSetup, now time.Time, size, steps int, mbs []leadstore.MailboxStatus) (BatchSizing, map[int64]int, error) {
	now = now.In(bs.loc)
	_, toCheck, err := lq.WaitingEmails(ctx)
	if err != nil {
		return BatchSizing{}, nil, err
	}
	waiting, err := lq.WaitingByMailbox(ctx)
	if err != nil {
		return BatchSizing{}, nil, err
	}
	loads := make([]outreach.MailboxLoad, 0, len(mbs))
	for _, m := range mbs {
		ml := outreach.MailboxLoad{ID: m.ID, Domain: m.Domain, SentToday: m.SentToday, Waiting: waiting[m.ID], Ready: readyForLeads(m)}
		if bs.leads.Caps.StartDate.IsFirstSend() && m.FirstLiveSend != nil {
			fs := m.FirstLiveSend.In(bs.loc)
			ml.FirstSend = &fs
		}
		loads = append(loads, ml)
	}
	z, room := mailboxSizing(bs.leads, loads, toCheck, now, size, steps)
	return z, room, nil
}

// notReadyRefusal says why no active mailbox in mbs is ready for new leads (decision 5),
// from what is recorded since today (the start of the operator's day): for each host and
// selector whose DNS check has not passed today, that today's check has not run, or what
// today's latest check found missing; otherwise that no mailbox's latest check passed.
func notReadyRefusal(ctx context.Context, lq leadstore.Queries, mbs []leadstore.MailboxStatus, today time.Time) error {
	var why []string
	seen := map[string]bool{}
	for _, m := range mbs {
		if m.State != leadstore.MailboxActive || m.DNSPassedToday {
			continue
		}
		key := m.Host() + "\x00" + m.DKIMSelector
		if seen[key] {
			continue
		}
		seen[key] = true
		r, ok, err := lq.LatestSenderCheckSelector(ctx, leadstore.SenderDomainSHA256(m.Host()), m.DKIMSelector, today)
		if err != nil {
			return err
		}
		if !ok {
			why = append(why, fmt.Sprintf("Today's domain check has not run for %s yet: press Check on its mailbox, or wait for the morning batch.", m.Host()))
			continue
		}
		why = append(why, fmt.Sprintf("Today's domain check failed for %s: %s.", m.Host(), missingRecords(r, m.DKIMSelector)))
	}
	if len(why) == 0 {
		return refused("No mailbox is ready for new leads: its latest check has not passed. Press Check on its mailbox.")
	}
	return refused("%s", strings.Join(why, " "))
}

// missingRecords names the records a failed DNS check did not find: "missing SPF, DKIM
// (selector s1)"; the check's own note when it names none.
func missingRecords(r mailbox.DNSResult, selector string) string {
	var miss []string
	if !r.SPF {
		miss = append(miss, "SPF")
	}
	if !r.DKIM {
		miss = append(miss, fmt.Sprintf("DKIM (selector %s)", selector))
	}
	if !r.DMARC {
		miss = append(miss, "DMARC")
	}
	if len(miss) == 0 {
		return cmp.Or(strings.TrimSpace(r.Note), "the records did not pass")
	}
	return "missing " + strings.Join(miss, ", ")
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
	if line := fallbackLine(out.Fallbacks); line != "" {
		note += "; " + line
	}
	switch r := asRefusal(runErr); {
	case r != nil && r.Kind == RefuseBatchWaiting:
		note = fmt.Sprintf("batch %d still waits for approval", r.BatchID)
	case r != nil && r.Kind == RefuseNoRoom:
		note = "no room in the ramp"
	case r != nil && r.Kind == RefuseNoStarred:
		note = fmt.Sprintf("no starred leads ready (%d starred)", len(r.Starred))
	case r != nil && r.Kind == RefuseSettingsIgnored:
		note = "saved settings ignored"
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
	// CallsOff are the lead ids whose call step is drafted where the campaign's rules have
	// calls off (the operator's call), with the country code.
	CallsOff map[int64]string
}

// callsOffCounts counts v.CallsOff by country.
func (v BatchView) callsOffCounts() map[string]int {
	out := map[string]int{}
	for _, c := range v.CallsOff {
		out[c]++
	}
	return out
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
	rules, err := loadCallRules(ctx, lq)
	if err != nil {
		return BatchView{}, err
	}
	off := map[int64]string{}
	for _, it := range items {
		for _, m := range it.Msgs {
			if m.Channel == leadstore.ChannelPhone && rules.off(b.CampaignID, it.Org.Country) {
				off[it.Lead.ID] = countryCode(it.Org.Country)
			}
		}
	}
	return BatchView{Batch: b, Items: items, Evidence: ev, ContentSHA256: sum, Changed: !bytes.Equal(sum, b.ContentSHA256), CallsOff: off}, nil
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
		if m.Channel == leadstore.ChannelPhone {
			return time.Time{} // a call falls due as the sequence reaches it, never at approval
		}
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
		res, err = lq.Suppress(ctx, leadstore.DefaultAccountID, kind, value, reason, source)
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

// liftSuppression records that suppression id was recorded by mistake (FO6): one
// suppression_lift row with reason, confirmed by domain (the suppression's domain, or its
// address's). source is leadstore.LiftFromCLI or leadstore.LiftFromConsole. It is the one
// core the CLI and the console both call; liftWords says what it did, or why it did nothing.
// Lead states are never changed.
func liftSuppression(ctx context.Context, s *store.Store, id int64, domain, reason, source string) (leadstore.Lifted, error) {
	var res leadstore.Lifted
	err := leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		res, err = lq.LiftSuppression(ctx, id, domain, reason, source)
		return err
	})
	switch {
	case errors.Is(err, leadstore.ErrLiftReason):
		return res, &LeadRefusal{Kind: RefuseLiftReason, SuppressionID: id, err: err}
	case errors.Is(err, leadstore.ErrNoSuppression):
		return res, &LeadRefusal{Kind: RefuseNoSuppression, SuppressionID: id, err: err}
	case errors.Is(err, leadstore.ErrLiftDomainMismatch):
		return res, &LeadRefusal{Kind: RefuseLiftDomain, SuppressionID: id, Value: strings.ToLower(strings.TrimSpace(domain)), err: err}
	case errors.Is(err, leadstore.ErrAlreadyLifted):
		return res, &LeadRefusal{Kind: RefuseAlreadyLifted, SuppressionID: id, Lifted: res.Row, err: err}
	}
	return res, err
}

// wordsFor is who a core's words are for: the CLI may name its own commands; the console
// never names a CLI command.
type wordsFor int

const (
	forCLI wordsFor = iota
	forConsole
)

// liftWords is what a lift did, or why it did nothing, in the plain sentences the CLI prints
// and the console flashes, so the two never word it differently. Only two things differ by
// audience, and only where the CLI names a command: the wrong-domain refusal's ending, and a
// success's lead-state hint (two lines for the CLI; one line for the console, the hint folded
// into its last sentence as "; lead states are unchanged."). Addresses are always masked; a
// domain typed to confirm that holds an @ is masked too. An error that is not a lift refusal
// has no words here ("").
func liftWords(res leadstore.Lifted, err error, to wordsFor) string {
	if err != nil {
		r := asRefusal(err)
		if r == nil {
			return ""
		}
		switch r.Kind {
		case RefuseNoSuppression:
			return fmt.Sprintf("No suppression %d.", r.SuppressionID)
		case RefuseAlreadyLifted:
			if r.Lifted.LiftedAt == nil {
				return fmt.Sprintf("Suppression %d was already lifted.", r.SuppressionID)
			}
			return fmt.Sprintf("Suppression %d was already lifted on %s (%s).", r.SuppressionID, leadsDay(*r.Lifted.LiftedAt), r.Lifted.LiftSource)
		case RefuseLiftDomain:
			typed := r.Value
			if strings.Contains(typed, "@") {
				typed = maskAddress(typed)
			}
			check := "Check the id with leads suppress list."
			if to == forConsole {
				check = "Check the number in the list."
			}
			return fmt.Sprintf("Suppression %d is not on %s: nothing was lifted. %s", r.SuppressionID, typed, check)
		case RefuseLiftReason:
			return "A lift needs a reason, 3 to 300 characters on one line, with no address in it."
		}
		return ""
	}
	row := res.Row
	then := "It may be written to again"
	if len(res.StillBlockedBy) > 0 {
		by := make([]string, len(res.StillBlockedBy))
		for i, b := range res.StillBlockedBy {
			by[i] = fmt.Sprintf("#%d (%s %s, %s)", b.ID, b.Kind, shown(b.Kind, b.Value), b.Reason)
		}
		then = "It is still blocked by " + strings.Join(by, ", ")
	}
	line := fmt.Sprintf("Lifted suppression %d: %s %s (%s, from %s). %s", row.ID, row.Kind, shown(row.Kind, row.Value), row.Reason, row.Source, then)
	if to == forConsole {
		return line + "; lead states are unchanged."
	}
	line += "."
	hint := "Lead states are unchanged."
	if res.LeadsSuppressed > 0 {
		value := "…"
		if row.Kind == leadstore.SuppressDomain {
			value = row.Value
		}
		hint = fmt.Sprintf("Lead states are unchanged (%s suppressed or opted_out); to block it again: leads suppress add %s %s --reason manual.",
			plural(res.LeadsSuppressed, "lead still reads", "leads still read"), row.Kind, value)
	}
	return line + "\n" + hint
}

// leadsDay is t's date in the operator's zone (LEADHOUND_TZ, or the default zone).
func leadsDay(t time.Time) string {
	cfg, _ := config.Load(os.Getenv)
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format(time.DateOnly)
}

// markContacted records that the operator reached domain by hand (RA15), so no batch drafts
// it again.
func markContacted(ctx context.Context, s *store.Store, domain, note string) (leadstore.Contacted, error) {
	var c leadstore.Contacted
	err := leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		c, err = lq.MarkContacted(ctx, leadstore.DefaultCampaignID, domain, note)
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
