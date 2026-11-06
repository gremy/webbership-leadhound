package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Draft it, editing a draft and marking a manual text sent: `leads drafts make`, `leads drafts
// edit`, and the store's Mark sent as the CLI's `leads done` and the console call it.

const (
	draftsMakeUsage = "usage: leadhound leads drafts make LEAD [--channel email|form|linkedin|instagram] [--decider claude|fake] | make --from-request [--decider claude|fake]"
	draftsEditUsage = "usage: leadhound leads drafts edit MESSAGE [--subject S] --body-file F"

	// commandLeadsDraftsMake is the run row of `leads drafts make`.
	commandLeadsDraftsMake = "leads-drafts-make"
	// draftItAction is the request file under <data>/run that starts the drafting service.
	draftItAction = "leads-draft"
	// draftItPerRun is how many requests one run of the service takes.
	draftItPerRun = 5
	// maxDraftBody is the longest text a draft holds, in bytes of the file read.
	maxDraftBody = 1 << 20
)

// runDraftsMake is `leads drafts make`: one lead's Draft it batch, or up to draftItPerRun of the
// console's requests.
func runDraftsMake(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads drafts make", flag.ContinueOnError)
	channel := fs.String("channel", "", "the channel to draft for (default: the lead's own)")
	dec := fs.String("decider", deciderClaude, "claude (LEADHOUND_LIVE=1 and the key) or fake")
	fromRequest := fs.Bool("from-request", false, "take the console's queued Draft it requests (the drafting service's command)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	decider, err := parseDecider(*dec)
	if err != nil {
		return err
	}
	if *fromRequest == (len(pos) == 1) || len(pos) > 1 || (*fromRequest && *channel != "") {
		return usageError("%s", draftsMakeUsage)
	}
	if err := checkDraftChannel(*channel); err != nil {
		return err
	}
	var lead int64
	if !*fromRequest {
		if lead, err = strconv.ParseInt(pos[0], 10, 64); err != nil || lead < 1 {
			return usageError("%q is not a lead number; %s", pos[0], draftsMakeUsage)
		}
	}
	s, setup, err := leadsOpen(ctx, leadsForOutreach)
	if err != nil {
		return err
	}
	defer s.Close()
	loc, err := leadsZone()
	if err != nil {
		return err
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsDraftsMake, sourceLeads)
	if err != nil {
		return err
	}
	var note string
	if *fromRequest {
		note, err = draftFromRequests(ctx, s, setup, loc, decider)
	} else {
		note, err = draftOneLead(ctx, s, setup, loc, decider, lead, *channel)
	}
	if ferr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{Note: cmp.Or(note, errWords(err))}); ferr != nil && err == nil {
		return ferr
	}
	if errors.Is(err, console.ErrUserFixable) {
		return usageError("%s", draftRefusalWords(err))
	}
	return err
}

func errWords(err error) string {
	if err == nil {
		return ""
	}
	return draftRefusalWords(err)
}

// draftRefusalWords is a refused() error's sentence, any other error's text.
func draftRefusalWords(err error) string {
	return strings.TrimPrefix(err.Error(), console.ErrUserFixable.Error()+": ")
}

func checkDraftChannel(channel string) error {
	switch channel {
	case "", leadconfig.ChannelEmail, leadconfig.ChannelForm, leadconfig.ChannelLinkedIn, leadconfig.ChannelInstagram:
		return nil
	}
	return usageError("--channel %q is not email, form, linkedin or instagram", channel)
}

// draftItClaude is what a Draft it batch writes with: the decider's caller, or none and why.
func draftItClaude(s *store.Store, decider string) *claudeDrafting {
	d := &claudeDrafting{rec: leadstore.NewQueries(s.Pool())}
	caller, err := draftsCaller("leads drafts make", decider, s.Q(), claude.PurposeDrafting)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "Claude drafting unavailable (%v): drafting from templates.\n", err)
		d.off = fbUnavailable
		return d
	}
	d.caller = caller
	return d
}

// draftOneLead is `leads drafts make LEAD`: a lead with a request still open is refused, naming its
// time, because the service is about to draft it.
func draftOneLead(ctx context.Context, s *store.Store, setup leadsSetup, loc *time.Location, decider string, lead int64, channel string) (string, error) {
	req, err := leadstore.NewQueries(s.Pool()).DraftRequestFor(ctx, lead)
	if err != nil {
		return "", err
	}
	if req != nil && (req.Status == leadstore.DraftRequestQueued || req.Status == leadstore.DraftRequestDrafting) {
		return "", refused("A draft was already asked for at %s.", req.RequestedAt.In(loc).Format("15:04"))
	}
	out, err := makeDraftIt(ctx, s, setup, loc, draftItClaude(s, decider), lead, channel)
	if err != nil {
		return "", err
	}
	line := out.line(lead)
	_, _ = fmt.Fprintln(stdout, line)
	return line, nil
}

// draftFromRequests claims at most draftItPerRun queued requests and finishes each with its batch or
// a plain reason, so no request holds its lead's open slot. A full claim may have left more queued,
// and nothing else would wake the service for them, so it asks for another run.
func draftFromRequests(ctx context.Context, s *store.Store, setup leadsSetup, loc *time.Location, decider string) (string, error) {
	lq := leadstore.NewQueries(s.Pool())
	reqs, err := lq.ClaimDraftRequests(ctx, draftItPerRun)
	if err != nil {
		return "", err
	}
	if len(reqs) == 0 {
		_, _ = fmt.Fprintln(stdout, "leads drafts make: no request is queued.")
		return "no request queued", nil
	}
	cl := draftItClaude(s, decider)
	done, failed := 0, 0
	bg := context.WithoutCancel(ctx) // a run stopped halfway still finishes what it claimed
	for _, r := range reqs {
		out, err := makeDraftIt(ctx, s, setup, loc, cl, r.LeadID, r.Channel)
		if err != nil {
			failed++
			why := draftRefusalWords(err)
			_, _ = fmt.Fprintf(stdout, "lead %d: %s\n", r.LeadID, why)
			if ferr := lq.FinishDraftRequest(bg, r.ID, leadstore.DraftRequestFailed, why, nil); ferr != nil {
				return "", ferr
			}
			continue
		}
		done++
		_, _ = fmt.Fprintln(stdout, out.line(r.LeadID))
		id := out.BatchID
		if ferr := lq.FinishDraftRequest(bg, r.ID, leadstore.DraftRequestDone, "", &id); ferr != nil {
			return "", ferr
		}
	}
	if len(reqs) == draftItPerRun {
		if err := writeRunRequest(setup.dataDir, draftItAction, leadsNow()); err != nil {
			_, _ = fmt.Fprintf(stdout, "leads drafts make: the next run was not asked for: %v\n", err)
		}
	}
	return fmt.Sprintf("%d drafted, %d failed", done, failed), nil
}

// draftItOutcome is what one Draft it made.
type draftItOutcome struct {
	BatchID  int64
	Channel  string
	Messages int
	Claude   bool
	Fallback string // the fallback line when the templates wrote any of it
}

func (o draftItOutcome) line(lead int64) string {
	writer := "from the templates"
	if o.Claude {
		writer = "by Claude"
	}
	line := fmt.Sprintf("Lead %d: batch %d, %s on %s, written %s. Read: leadhound leads batch show %d", lead, o.BatchID,
		plural(o.Messages, "message", "messages"), o.Channel, writer, o.BatchID)
	if o.Fallback != "" {
		line += ". " + o.Fallback + "."
	}
	return line
}

// makeDraftIt drafts one lead into a batch of its own: origin draft_it, manual, live (Decision 8). Email
// follows the campaign's writer rule, a manual text is Claude's when cl can. A refusal is
// refused(): not a new lead, already batched, suppressed, in a conversation elsewhere, no contact on
// that channel or no mailbox. The model runs outside any transaction; creating the batch checks the
// lead's state again with its row locked.
func makeDraftIt(ctx context.Context, s *store.Store, setup leadsSetup, loc *time.Location, cl *claudeDrafting, leadID int64, channel string) (draftItOutcome, error) {
	out := draftItOutcome{}
	lq := leadstore.NewQueries(s.Pool())
	now := leadsNow()
	lead, err := lq.LeadByID(ctx, leadID)
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return out, refused("There is no lead %d.", leadID)
	}
	if err != nil {
		return out, err
	}
	org, err := lq.OrgByID(ctx, lead.OrgID)
	if err != nil {
		return out, err
	}
	if err := draftableNow(ctx, s, lq, lead, org); err != nil {
		return out, err
	}
	c, err := draftItCandidate(ctx, lq, lead, org, channel)
	if err != nil {
		return out, err
	}
	bs := newBatchSetup(setup, loc)
	bs.shadow, bs.claude = false, cl
	plan, err := resolveBatchPlan(ctx, lq, bs, lead.CampaignID)
	if err != nil {
		return out, err
	}
	mb, err := draftItMailbox(ctx, lq, lead.CampaignID, startOfDay(now, loc))
	if err != nil {
		return out, err
	}
	var phone *leadstore.Fact
	leadPlan, seqName := plan, ""
	if c.Channel == leadconfig.ChannelEmail {
		leftOut := ""
		if phone, leftOut, err = leadDraftPhone(ctx, lq, plan, c, now); err != nil {
			return out, err
		}
		if leadPlan, seqName = plan.forLead(c.Country); seqName != "" && phone == nil {
			return out, refused("Email to this country needs their yes on a call first, and there is no number to call: %s.", cmp.Or(leftOut, callNoNumber))
		}
	}
	drafter, err := newBatchDrafter(ctx, lq, bs, plan, lead.CampaignID)
	if err != nil {
		return out, err
	}
	drafter.fresh = true
	evs, err := lq.LeadEvidence(ctx, []int64{leadID})
	if err != nil {
		return out, err
	}
	rows, err := drafter.draft(ctx, lq, leadPlan, c, evs[leadID], setup.leads, phone, outreach.MessageIDDomain(mb.Address), rand.Reader)
	if sb := (*stopBatchError)(nil); errors.As(err, &sb) {
		return out, err
	}
	if err != nil {
		return out, refused("No draft passes the checks: %v", err)
	}
	from := cmp.Or(strings.TrimSpace(mb.FromName), setup.leads.Sender.Name)
	for i := range rows {
		rows[i].MailboxID, rows[i].FromName = mb.ID, from
		if seqName != "" {
			rows[i].SequenceName = seqName
		}
	}
	if seqName != "" && plan.sequences[seqName] == nil {
		plan.sequences[seqName] = plan.callFirst
	}
	tsha, _ := hex.DecodeString(plan.templates.SHA256)
	meta := leadstore.BatchMeta{CampaignID: lead.CampaignID, Sequences: plan.sequences, Origin: leadstore.BatchOriginDraftIt}
	err = leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		if c.RouteID != nil && (lead.RouteID == nil || *lead.RouteID != *c.RouteID || lead.Channel != c.Channel) {
			if _, err := tq.SetNewLeadRoute(ctx, leadID, c.Channel, *c.RouteID); err != nil {
				return err
			}
		}
		var err error
		out.BatchID, _, err = tq.CreateBatchFor(ctx, meta, false, rows, tsha)
		return err
	})
	if errors.Is(err, leadstore.ErrNotDraftable) {
		return out, refused("The lead can no longer be drafted: %v", err)
	}
	if err != nil {
		return out, err
	}
	out.Channel, out.Messages, out.Claude, out.Fallback = c.Channel, len(rows), drafter.drafted > 0, fallbackLine(drafter.fallbacks)
	return out, nil
}

// draftableNow refuses, in plain words, a lead that may not be drafted now.
func draftableNow(ctx context.Context, s *store.Store, lq leadstore.Queries, lead leadstore.Lead, org leadstore.Org) error {
	switch lead.State {
	case leadstore.LeadNew:
	case leadstore.LeadBatched:
		var batch int64
		err := s.Pool().QueryRow(ctx,
			`SELECT m.batch_id FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
			 WHERE m.lead_id = $1 AND b.discarded_at IS NULL ORDER BY m.batch_id DESC LIMIT 1`, lead.ID).Scan(&batch)
		if errors.Is(err, pgx.ErrNoRows) {
			return refused("It is already in a batch.")
		}
		if err != nil {
			return fmt.Errorf("draft it: open batch: %w", err)
		}
		return refused("It is already in batch %d.", batch)
	case leadstore.LeadContacted, leadstore.LeadReplied, leadstore.LeadBounced, leadstore.LeadOptedOut:
		return refused("Already contacted: it is %s.", leadStateWords(lead.State))
	default:
		return refused("It is %s, and only a new lead can be drafted.", leadStateWords(lead.State))
	}
	set, err := lq.SuppressionSet(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return err
	}
	routes, err := lq.RoutesFor(ctx, org.ID)
	if err != nil {
		return err
	}
	sup := outreach.NewSuppressionSet(set.Addresses, set.Domains)
	suppressed := outreach.Suppressed("", org.Domain, sup)
	for _, r := range routes {
		suppressed = suppressed || (lead.RouteID != nil && r.ID == *lead.RouteID && r.Kind == leadstore.RouteEmail && outreach.Suppressed(r.Value, org.Domain, sup))
	}
	if suppressed {
		return refused("It is suppressed (opted out or blocked).")
	}
	held, err := lq.HeldLeads(ctx, []int64{lead.ID})
	if err != nil {
		return err
	}
	if held[lead.ID] {
		return refused("%s", manualStopWords(roasteryHeldWhy))
	}
	why, err := lq.OpenElsewhere(ctx, leadstore.DefaultAccountID, lead.CampaignID, []int64{org.ID})
	if err != nil {
		return err
	}
	if w, ok := why[org.ID]; ok {
		return refused("Its business is in a conversation in another campaign: %s", w)
	}
	return nil
}

// draftItCandidate is lead on channel ("": its own) with the route it would be drafted on: its own
// when that fits, else the first of that kind RoutesFor gives (an email only with a mail server or
// the operator's word).
func draftItCandidate(ctx context.Context, lq leadstore.Queries, lead leadstore.Lead, org leadstore.Org, channel string) (leadstore.Candidate, error) {
	c := leadstore.Candidate{LeadID: lead.ID, OrgID: org.ID, Domain: org.Domain, OrgName: org.Name, Country: org.Country, Region: org.Region,
		Kind: lead.Kind, ProbeID: lead.ProbeID, AgencyFindingID: lead.AgencyFindingID}
	c.Channel = cmp.Or(channel, lead.Channel)
	if c.Channel == leadconfig.ChannelNone {
		return c, refused("It has no way to reach it yet. Add a contact, or press Look for a contact.")
	}
	if c.Channel == leadconfig.ChannelInstagram && !isCafeKind(lead.Kind) {
		// The DM is written to a café's own account (Decision 8): no other campaign uses it.
		return c, refused("An Instagram DM is for a café lead only. Choose email, contact form or LinkedIn.")
	}
	routes, err := lq.RoutesFor(ctx, org.ID)
	if err != nil {
		return c, err
	}
	fits := func(r leadstore.Route) bool {
		return r.Kind == c.Channel && (r.Kind != leadstore.RouteEmail || r.MXOK || r.Operator)
	}
	i := slices.IndexFunc(routes, func(r leadstore.Route) bool { return lead.RouteID != nil && r.ID == *lead.RouteID && fits(r) })
	if i < 0 {
		i = slices.IndexFunc(routes, fits)
	}
	if i < 0 {
		return c, refused("It has no %s contact to draft for.", channelContactWords(c.Channel))
	}
	id := routes[i].ID
	c.RouteID, c.RouteKind, c.Route = &id, routes[i].Kind, routes[i].Value
	return c, nil
}

// draftItMailbox is the mailbox a Draft it batch is drafted on: one assigned to the campaign, else
// any active one. Its readiness is the send's to judge; a manual text only needs the row to name one.
func draftItMailbox(ctx context.Context, lq leadstore.Queries, campaignID int64, day time.Time) (leadstore.MailboxStatus, error) {
	mbs, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, day)
	if err != nil {
		return leadstore.MailboxStatus{}, err
	}
	assigned, err := campaignAssigned(ctx, lq, campaignID)
	if err != nil {
		return leadstore.MailboxStatus{}, err
	}
	active := slices.DeleteFunc(mbs, func(m leadstore.MailboxStatus) bool { return m.State != leadstore.MailboxActive })
	if len(active) == 0 {
		return leadstore.MailboxStatus{}, refused("Add a mailbox first: Mailboxes in the console, or leads mailboxes add.")
	}
	if i := slices.IndexFunc(active, func(m leadstore.MailboxStatus) bool { return assigned[m.ID] }); i >= 0 {
		return active[i], nil
	}
	return active[0], nil
}

// --- Mark sent ---

// messageBatchRow is what `leads done` reads of a message's batch.
type messageBatchRow struct {
	BatchID  int64
	Channel  string
	Origin   string
	Shadow   bool
	Approved bool
	Hash     []byte
}

// messageBatch reads message id's batch; found is false when there is no such message.
func messageBatch(ctx context.Context, s *store.Store, id int64) (messageBatchRow, bool, error) {
	var m messageBatchRow
	err := s.Pool().QueryRow(ctx,
		`SELECT m.batch_id, m.channel, b.origin, b.shadow, b.approved_at IS NOT NULL, b.content_sha256
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id WHERE m.id = $1`, id).
		Scan(&m.BatchID, &m.Channel, &m.Origin, &m.Shadow, &m.Approved, &m.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("leads done: %w", err)
	}
	return m, true, nil
}

// usesManualSent says `leads done` goes through Mark sent: a live manual text whose batch is a Draft
// it batch or is approved.
func (m messageBatchRow) usesManualSent() bool {
	return manualDraftChannel(m.Channel) && !m.Shadow && (m.Approved || m.Origin == leadstore.BatchOriginDraftIt)
}

func (m messageBatchRow) hashPrefix() string {
	return hex.EncodeToString(m.Hash)[:leadstore.MinHashPrefix]
}

// markManualSent is Mark sent on message id in one transaction: his approval at hashPrefix, the hash
// he was shown, and his sending.
func markManualSent(ctx context.Context, s *store.Store, id int64, hashPrefix string) (leadstore.Done, error) {
	var d leadstore.Done
	err := leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		var err error
		d, err = tq.MarkManualSent(ctx, id, hashPrefix)
		return err
	})
	switch {
	case errors.Is(err, leadstore.ErrMessageNotFound):
		return d, &LeadRefusal{Kind: RefuseMessageNotFound, MessageID: id, err: err}
	case errors.Is(err, leadstore.ErrNotManual):
		return d, &LeadRefusal{Kind: RefuseNotManual, MessageID: id, err: err}
	}
	return d, err
}

// manualSentWords is a Mark sent refusal as one sentence, "" for anything else.
func manualSentWords(err error) string {
	switch {
	case errors.Is(err, leadstore.ErrLeadAnswered):
		return "the lead has replied, bounced or opted out, so this is no longer yours to send."
	case errors.Is(err, leadstore.ErrShadowMarkSent):
		return "shadow mode; nothing is marked sent."
	case errors.Is(err, leadstore.ErrDraftChanged):
		return "this draft changed; reload."
	case errors.Is(err, leadstore.ErrApproveBatchFirst):
		return "approve its batch first."
	case errors.Is(err, leadstore.ErrMessageState):
		return "it is not a draft or approved text any more; it may already be marked sent."
	}
	return ""
}

// --- edit ---

// runDraftsEdit is `leads drafts edit MESSAGE [--subject S] --body-file F`: saveDraftEdit.
func runDraftsEdit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads drafts edit", flag.ContinueOnError)
	subject := fs.String("subject", "", "the new subject (default: the subject as it stands)")
	bodyFile := fs.String("body-file", "", "the file holding the new text")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *bodyFile == "" {
		return usageError("%s", draftsEditUsage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id < 1 {
		return usageError("%q is not a message number; %s", pos[0], draftsEditUsage)
	}
	raw, err := os.ReadFile(*bodyFile) // #nosec G304 -- the operator names his own file
	if err != nil {
		return usageError("--body-file: %v", err)
	}
	if len(raw) > maxDraftBody {
		return usageError("--body-file: the text is too long")
	}
	s, setup, err := leadsOpen(ctx, leadsForOutreach)
	if err != nil {
		return err
	}
	defer s.Close()
	loc, err := leadsZone()
	if err != nil {
		return err
	}
	var sub *string
	if flagWasSet(fs, "subject") {
		sub = subject
	}
	editID, err := saveDraftEdit(ctx, s, setup, loc, id, sub, string(raw))
	if errors.Is(err, console.ErrUserFixable) {
		return usageError("%s", draftRefusalWords(err))
	}
	if err != nil {
		return err
	}
	if editID == 0 {
		_, _ = fmt.Fprintf(stdout, "Message %d: the text is what it already was; nothing changed.\n", id)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Message %d saved (edit %d). The original stays beside your version.\n", id, editID)
	return nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// saveDraftEdit saves a new text for message id (subject nil: as it stands) by its channel's road,
// through CheckIn, CheckText and a note's limit, in one transaction, and returns the edit's id (0:
// nothing changed). Every refusal is refused() and writes nothing.
func saveDraftEdit(ctx context.Context, s *store.Store, setup leadsSetup, loc *time.Location, id int64, subject *string, body string) (int64, error) {
	check, before, err := draftEditCheck(ctx, s, setup, loc, id)
	if errors.Is(err, leadstore.ErrMessageNotFound) {
		return 0, refused("There is no message %d.", id)
	}
	if err != nil {
		return 0, err
	}
	sub := before.Subject
	if subject != nil {
		sub = *subject
	}
	// An editor's or a form's line ending is no change: the text keeps its own.
	body = strings.TrimRight(body, " \t\r\n") + before.Body[len(strings.TrimRight(before.Body, " \t\r\n")):]
	var editID int64
	err = leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		var err error
		editID, err = tq.SaveMessageEdit(ctx, id, sub, body, check)
		return err
	})
	if words := editRefusalWords(err); words != "" {
		return 0, refused("%s", words)
	}
	return editID, err
}

// editRefusalWords is an edit's refusal as one sentence, "" for anything else.
func editRefusalWords(err error) string {
	var bad *leadstore.EditRefusedError
	switch {
	case errors.As(err, &bad):
		return strings.Join(bad.Reasons, " ")
	case errors.Is(err, leadstore.ErrLeadAnswered):
		return "The lead has replied, bounced or opted out, so this draft can no longer be edited."
	case errors.Is(err, leadstore.ErrBatchApproved):
		return "The batch is approved, so this email can no longer be edited."
	case errors.Is(err, leadstore.ErrBatchDiscarded):
		return "The batch was discarded."
	case errors.Is(err, leadstore.ErrBatchTampered):
		return "The batch changed since it was drafted; draft it again."
	case errors.Is(err, leadstore.ErrMessageState):
		return "This message can no longer be edited: it was sent, marked sent, skipped or cancelled."
	case errors.Is(err, leadstore.ErrNotManual):
		return "A call card cannot be edited."
	}
	return ""
}

// draftEditCheck is what a saved text of message id must pass: the checks its first draft passed,
// the same rules for its lead, its text as it stands and as it began counting as the operator's own
// wording, a LinkedIn note's limit and an Instagram DM's. It returns the text as it stands too. A lead with no finding
// on file has no slots to read, and is checked on what remains.
func draftEditCheck(ctx context.Context, s *store.Store, setup leadsSetup, loc *time.Location, id int64) (leadstore.EditCheck, leadstore.EffectiveDraft, error) {
	lq := leadstore.NewQueries(s.Pool())
	before, err := lq.EffectiveText(ctx, id)
	if err != nil {
		return nil, before, err
	}
	m, msgs, batch, err := messageWithBatch(ctx, s, id)
	if err != nil {
		return nil, before, err
	}
	lead, err := lq.LeadByID(ctx, m.LeadID)
	if err != nil {
		return nil, before, err
	}
	org, err := lq.OrgByID(ctx, lead.OrgID)
	if err != nil {
		return nil, before, err
	}
	bs := newBatchSetup(setup, loc)
	plan, err := resolveBatchPlan(ctx, lq, bs, batch.CampaignID)
	if err != nil {
		return nil, before, err
	}
	leadPlan := plan
	if m.SequenceName != "" {
		leadPlan, _ = plan.forLead(org.Country)
	}
	evs, err := lq.LeadEvidence(ctx, []int64{m.LeadID})
	if err != nil {
		return nil, before, err
	}
	c := leadstore.Candidate{LeadID: lead.ID, OrgID: org.ID, Domain: org.Domain, OrgName: org.Name, Country: org.Country, Region: org.Region,
		Kind: lead.Kind, Channel: lead.Channel, ProbeID: lead.ProbeID, AgencyFindingID: lead.AgencyFindingID}
	var sheet outreach.FactSheet
	var slots outreach.Slots
	rules := leadPlan.rulesFor(lead.Kind)
	if in, err := (draftCtx{cfg: setup.leads, plan: plan, campaignID: batch.CampaignID}).leadInput(ctx, lq, leadPlan, c, evs[m.LeadID], true); err == nil {
		sheet, slots = in.Sheet, in.Render.Slots
		rules.Links = slices.Concat(rules.Links, sheet.ProofLinks())
	}
	allowed := []string{before.Subject, before.Body, before.OriginalSubject, before.OriginalBody, m.Subject, m.BodyText}
	base := outreach.Draft{Kind: outreach.DraftForm, Slots: slots, FixedText: strings.Join(allowed, "\n")}
	footer := ""
	switch m.Channel {
	case leadconfig.ChannelEmail:
		base.Kind = outreach.DraftEmail
		if tmpl := leadPlan.footerFor(org.Country, setup.leads); tmpl != nil && slots.Domain.Value != "" {
			footer = renderedFooter(tmpl, slots)
		}
	case leadconfig.ChannelLinkedIn:
		base.Kind = outreach.DraftLinkedIn
	case leadconfig.ChannelInstagram:
		base.Kind = outreach.DraftInstagram
	}
	isNote := m.Channel == leadconfig.ChannelLinkedIn && linkedInNote(m, msgs)
	isDM := m.Channel == leadconfig.ChannelInstagram
	check := func(subject, body string) []string {
		d := base
		d.Subject, d.Body, d.BodyAboveFooter = subject, body, body
		if footer != "" && strings.HasSuffix(body, footer) {
			d.BodyAboveFooter = strings.TrimSuffix(body, footer)
		}
		var why []string
		for _, p := range outreach.CheckIn(d, m.Step, setup.leads, rules) {
			why = append(why, p.Detail+".")
		}
		for _, f := range outreach.CheckText(subject+"\n"+body, sheet, allowed, rules.Forbid) {
			why = append(why, f.Msg+".")
		}
		if isNote {
			for _, f := range outreach.CheckNote(body, setup.leads.Sender.LinkedInNoteMax) {
				why = append(why, f.Msg+".")
			}
		}
		if isDM && dmHoldsLink(body) {
			why = append(why, capitalised(dmNoLinkWords)+".")
		}
		if isDM {
			for _, f := range outreach.CheckDM(body, setup.leads.Sender.InstagramDMMax) {
				why = append(why, f.Msg+".")
			}
		}
		return why
	}
	return check, before, nil
}

// renderedFooter is the footer as slots render it, "" when it does not render.
func renderedFooter(tmpl *template.Template, slots outreach.Slots) string {
	mark := template.Must(template.New("x").Parse("x"))
	d, err := outreach.Render(outreach.EmailTemplate(leadconfig.Email{Subject: mark, Body: mark}, tmpl), slots)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(d.Body, d.BodyAboveFooter)
}

// messageWithBatch is message id with its lead's messages of that batch and the batch.
func messageWithBatch(ctx context.Context, s *store.Store, id int64) (leadstore.Message, []leadstore.Message, leadstore.Batch, error) {
	row, found, err := messageBatch(ctx, s, id)
	if err != nil {
		return leadstore.Message{}, nil, leadstore.Batch{}, err
	}
	if !found {
		return leadstore.Message{}, nil, leadstore.Batch{}, fmt.Errorf("message %d: %w", id, leadstore.ErrMessageNotFound)
	}
	lq := leadstore.NewQueries(s.Pool())
	all, err := lq.BatchMessages(ctx, row.BatchID)
	if err != nil {
		return leadstore.Message{}, nil, leadstore.Batch{}, err
	}
	i := slices.IndexFunc(all, func(m leadstore.Message) bool { return m.ID == id })
	if i < 0 {
		return leadstore.Message{}, nil, leadstore.Batch{}, fmt.Errorf("message %d: %w", id, leadstore.ErrMessageNotFound)
	}
	m := all[i]
	own := slices.DeleteFunc(slices.Clone(all), func(x leadstore.Message) bool { return x.LeadID != m.LeadID })
	b, err := lq.BatchByID(ctx, row.BatchID)
	return m, own, b, err
}

// linkedInNote says LinkedIn message m is its lead's note: the first LinkedIn step of its batch. A
// form lead's note is step 1 and a LinkedIn lead's step 0; each message follows its note.
func linkedInNote(m leadstore.Message, msgs []leadstore.Message) bool {
	for _, x := range msgs {
		if x.LeadID == m.LeadID && x.Channel == leadconfig.ChannelLinkedIn && x.Step < m.Step {
			return false
		}
	}
	return m.Channel == leadconfig.ChannelLinkedIn
}

// channelContactWords names a channel's kind of contact.
func channelContactWords(channel string) string {
	switch channel {
	case leadconfig.ChannelForm:
		return "contact form"
	case leadconfig.ChannelLinkedIn:
		return "LinkedIn"
	case leadconfig.ChannelInstagram:
		return "Instagram"
	}
	return channel
}
