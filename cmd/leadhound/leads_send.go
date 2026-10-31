package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/verify"
)

func init() { leadsCommands["send"] = runLeadsSend }

// The run rows of the send commands.
const (
	commandLeadsSend     = "leads-send"
	sourceOutreach       = "outreach"
	sourceOutreachShadow = "outreach-shadow"
)

const (
	// resyncAfter: send syncs again before a message when the last sync is older than this.
	resyncAfter = 10 * time.Minute
	// retryAfter is how long a message waits after a temporary SMTP failure.
	retryAfter = time.Hour
	// outboxDir is where a shadow send writes, under the data directory.
	outboxDir = "outreach-outbox"
)

// The words a send's run note uses for what stopped or refused something; never a value.
const (
	noteDNS         = "dns"
	noteSync        = "sync"
	noteSyncRunning = "sync running"
	noteTampered    = "tampered"
	noteConsent     = "consent"
	noteReserved    = "reserved"
	noteAppend      = "append"
	noteInterrupted = "interrupted"
	noteAccount     = "smtp-account"
	noteFollowup    = "followup"
	noteStuck       = "stuck-sending"
	notePaused      = "paused by the operator"
	// noteKeyMissing: a live run found no mailbox key and sent nothing (the brief reads it).
	noteKeyMissing    = "mailbox key missing"
	noteNoMailbox     = "no mailbox"
	noteMailboxPaused = "mailbox paused"
	notePolicyBlock   = "policy-block"
	// noteSendNow marks a run started by Send now (leads send --now), which skipped the
	// sending window, so Controls and the journal tell it from a timer run.
	noteSendNow = "send now"
	// noteNoRequest: a leads send --from-request found no confirmed manual request to claim.
	noteNoRequest = "no request"
	// noteRequestRefused: the Send now request, or the batch it names, did not pass its checks.
	noteRequestRefused = "request refused"
	// noteEmailCheck: a manual send held or refused a message on its email check.
	noteEmailCheck = "email check"
	// noteFirstGone: a first email was refused because its lead's first email already went in
	// another batch.
	noteFirstGone = "first email already went"
	// noteOverride heads the override codes a manual send ran with ("override: ramp,window").
	noteOverride = "override: "
)

// sendRequestMaxAge is how old a manual send's send_request row may be when its run claims it.
const sendRequestMaxAge = 15 * time.Minute

// sendRequestMax is --max for a run from the Send now request when none is given: the
// hourly send's 25.
const sendRequestMax = 25

// holdMailbox holds a due message whose mailbox may not send in this run: paused, not
// checked, its DNS or sync failed, its login refused, or no mailbox at all.
const holdMailbox outreach.HoldReason = "mailbox"

// holdEmailCheck holds a due message of a manual send whose address has no fresh passing
// email check while the email check is on.
const holdEmailCheck outreach.HoldReason = "email_check"

// holdUnassigned holds a due live message whose mailbox is not (or no longer) assigned to
// its batch's campaign. It keeps its mailbox: a thread is never moved to another address.
const holdUnassigned outreach.HoldReason = "unassigned"

// cancelMailboxRetired is the cancel reason of a message whose mailbox is retired: a thread
// must come from the same address.
const cancelMailboxRetired = "mailbox retired"

// errPaused stops a send run before a message is claimed: the operator paused sending.
var errPaused = errors.New("leads send: paused by the operator")

// selftestNotSent is the cancel reason of a selftest message that could not go in its run:
// it is never left for a later ordinary send.
const selftestNotSent = "selftest-not-sent"

// sendOptions shapes one send run.
type sendOptions struct {
	max int // at most this many messages (0: as many as Plan picks)
	// selftestTo makes the run a selftest: one message to this operator address.
	selftestTo string
	// selftestMailbox is the selftest's mailbox, its id or address; "" with only one.
	selftestMailbox string
	// now skips the sending window (hours and days) for every message of this run, and
	// nothing else: the sending switch, shadow mode, the reply sync first, the warm-up and
	// domain caps, the spacing, mailbox readiness and campaign assignment, suppressions, max
	// and every due time stay as they are (Send now).
	now bool
	// fromRequest reads the Send now request file (leads send --from-request, implies now): a
	// legacy request (a time, or empty) is today's Send now; a manual one sends that batch alone
	// with the overrides of the send_request row it claims (sendRun.claimRequest).
	fromRequest bool
}

// manualSend is a manual batch's Send now as its run claimed and checked it: the batch, the
// send_request row, its overrides and, per mailbox, the numbers outreach.CheckLimits gave at
// the run's start (each send_override row's detail).
type manualSend struct {
	batch  leadstore.Batch
	req    leadstore.SendRequest
	ov     outreach.Overrides
	detail map[int64]*limitDetail
}

// limitDetail is one mailbox's send_override detail: what CheckLimits said about it at the
// run's start, for the sends planned from it, and what the confirm page showed (Shown).
type limitDetail struct {
	MailboxID int64           `json:"mailbox_id"`
	Need      int             `json:"need"`
	Room      int             `json:"room"`         // RoomTodayWith the run's overrides
	RoomPlain int             `json:"room_without"` // RoomToday
	Warnings  []limitWarnJSON `json:"warnings"`
	Stops     []limitStopJSON `json:"stops"`
	// Shown is the warnings with their numbers as the confirm page showed them, from the
	// claimed send_request row (a JSON array).
	Shown json.RawMessage `json:"shown"`
}

type limitWarnJSON struct {
	Code    string `json:"code"`
	Domain  string `json:"domain"`
	Used    int    `json:"used"`
	Allowed int    `json:"allowed"`
	Over    int    `json:"over"`
}

type limitStopJSON struct {
	Reason  string `json:"reason"`
	Domain  string `json:"domain"`
	Used    int    `json:"used"`
	Allowed int    `json:"allowed"`
}

// sendReport is what one send run did; its note is the run row's note.
type sendReport struct {
	live      bool
	sent      []string // the recipient domains, in send order
	refused   []string // "message 7 (tampered)"
	maybe     []int64
	stuck     []int64 // left in sending by an earlier run that broke off
	retry     int
	cancelled int64
	skipped   int
	held      map[outreach.HoldReason]int
	overMax   int
	notes     []string
	// pausedAt is when the operator paused sending, when that stopped this run.
	pausedAt *time.Time
	// stopped: the run ended before planning and said why (no key, no ready mailbox).
	stopped bool
}

func (r *sendReport) note(word string) {
	for _, n := range r.notes {
		if n == word {
			return
		}
	}
	r.notes = append(r.notes, word)
}

// holdOrder is the order the run note and the printout name the held reasons in.
var holdOrder = []outreach.HoldReason{outreach.HoldCap, outreach.HoldHours, outreach.HoldDomain, outreach.HoldNotDue, holdMailbox, holdUnassigned, holdEmailCheck}

// runNote is the run row's note: counts and note words only, never an address or a reply.
func (r *sendReport) runNote(runErr error) string {
	mode := "shadow"
	if r.live {
		mode = "live"
	}
	parts := []string{mode, fmt.Sprintf("sent %d", len(r.sent)), fmt.Sprintf("refused %d", len(r.refused)),
		fmt.Sprintf("maybe_sent %d", len(r.maybe)), fmt.Sprintf("retry %d", r.retry),
		fmt.Sprintf("cancelled %d", r.cancelled), fmt.Sprintf("skipped %d", r.skipped)}
	var held []string
	for _, h := range holdOrder {
		if n := r.held[h]; n > 0 {
			held = append(held, fmt.Sprintf("%s %d", h, n))
		}
	}
	if r.overMax > 0 {
		held = append(held, fmt.Sprintf("max %d", r.overMax))
	}
	if len(held) > 0 {
		parts = append(parts, "held "+strings.Join(held, ", "))
	}
	if len(r.notes) > 0 {
		parts = append(parts, "notes "+strings.Join(r.notes, ", "))
	}
	if runErr != nil {
		parts = append(parts, "error")
	}
	return strings.Join(parts, " · ")
}

// print writes the outcome decision-first: what the operator must check, then what went.
func (r *sendReport) print(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	if len(r.maybe) > 0 {
		p("Check by hand: %s may have gone out (the connection broke after the server took the message; never resent): message %s.\n",
			plural(len(r.maybe), "message", "messages"), idList(r.maybe))
	}
	if len(r.stuck) > 0 {
		p("Check by hand: %s left mid-send by an earlier run may have gone out (never resent; counted toward today's cap): message %s.\n",
			plural(len(r.stuck), "message", "messages"), idList(r.stuck))
	}
	if len(r.refused) > 0 {
		p("Refused %d: %s.\n", len(r.refused), strings.Join(r.refused, "; "))
	}
	switch {
	case len(r.sent) > 0 && r.live:
		p("Sent %d: %s.\n", len(r.sent), strings.Join(r.sent, ", "))
	case len(r.sent) > 0:
		p("Shadow: wrote %d to the outbox (nothing left the machine): %s.\n", len(r.sent), strings.Join(r.sent, ", "))
	}
	if r.retry > 0 {
		p("%s failed for now and will be retried in an hour.\n", plural(r.retry, "message", "messages"))
	}
	if r.cancelled > 0 {
		p("%s cancelled (suppressed, or the lead moved on).\n", plural(int(r.cancelled), "pending message", "pending messages"))
	}
	var held []string
	for _, h := range holdOrder {
		if h == outreach.HoldNotDue {
			continue
		}
		if n := r.held[h]; n > 0 {
			held = append(held, fmt.Sprintf("%s %d", map[outreach.HoldReason]string{
				outreach.HoldCap: "today's cap", outreach.HoldHours: "outside business hours", outreach.HoldDomain: "one per domain per run",
				holdMailbox: "its mailbox cannot send now", holdUnassigned: "mailbox not assigned to the campaign",
				holdEmailCheck: "no passing email check yet",
			}[h], n))
		}
	}
	if r.overMax > 0 {
		held = append(held, fmt.Sprintf("--max %d", r.overMax))
	}
	if len(held) > 0 {
		p("Held for a later run: %s.\n", strings.Join(held, " · "))
	}
	if r.pausedAt != nil {
		p("Sending is paused by the operator (since %s); nothing more is sent until leadhound leads send resume.\n",
			r.pausedAt.Format("2006-01-02 15:04"))
		return
	}
	if len(r.sent)+len(r.refused)+len(r.maybe)+len(r.stuck)+r.retry+len(held) == 0 && r.cancelled == 0 && !r.stopped {
		p("Nothing due to send.\n")
	}
}

func idList(ids []int64) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(out, ", ")
}

// runLeadsSend is `leads send [--max N] [--now | --from-request]`, or `leads send pause [--reason TEXT]` and
// `leads send resume`.
func runLeadsSend(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "pause" || args[0] == "resume") {
		return runLeadsSendPause(ctx, args[0] == "pause", args[1:])
	}
	fs := flag.NewFlagSet("leads send", flag.ContinueOnError)
	maxN := fs.Int("max", 0, "send at most this many messages")
	now := fs.Bool("now", false, "send the due emails now, outside the sending window (this run only; every limit still applies)")
	fromRequest := fs.Bool("from-request", false, "send what the console's Send now request asks (implies --now; the send-now unit's command)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *maxN < 0 {
		return usageError("%s", sendUsage)
	}
	opts := sendOptions{max: *maxN, now: *now || *fromRequest, fromRequest: *fromRequest}
	if opts.fromRequest && opts.max == 0 {
		opts.max = sendRequestMax
	}
	return leadsSendRun(ctx, stdout, opts)
}

const sendUsage = "usage: leadhound leads send [--max N] [--now | --from-request] | leads send pause [--reason TEXT] | leads send resume"

// runLeadsSendPause is `leads send pause [--reason TEXT]` and `leads send resume`: the
// operator's switch that every send run obeys before each message. Setting it to what it
// already is writes nothing.
func runLeadsSendPause(ctx context.Context, pause bool, args []string) error {
	fs := flag.NewFlagSet("leads send pause", flag.ContinueOnError)
	var reason *string
	if pause {
		reason = fs.String("reason", "", "why sending is paused (kept with the pause)")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", sendUsage)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return usageError("LEADHOUND_TZ: %v", err)
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	why := ""
	if reason != nil {
		why = *reason
	}
	// The console's switch runs the same core (consolecore.go).
	sw, err := setSendPause(ctx, leadstore.NewQueries(s.Pool()), pause, why, leadstore.ActorGremy)
	if err != nil {
		return err
	}
	switch {
	case pause && !sw.Changed:
		_, _ = fmt.Fprintf(stdout, "Sending is already paused (since %s).\n", sw.Since.In(loc).Format("2006-01-02 15:04"))
		return nil
	case !pause && !sw.Changed:
		_, _ = fmt.Fprintln(stdout, "Sending is not paused.")
		return nil
	}
	if pause {
		_, _ = fmt.Fprintln(stdout, "Sending is paused: leads send sends nothing until leadhound leads send resume.")
	} else {
		_, _ = fmt.Fprintln(stdout, "Sending is resumed: the next leads send run sends what is due.")
	}
	return nil
}

// startOfDay is midnight of now's day in loc.
func startOfDay(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// sendMailbox is one sending mailbox as a send run sees it: its row and readiness as last
// read, why this run sends nothing more from it, and (live, once opened) its credentials and
// sender.
type sendMailbox struct {
	leadstore.MailboxStatus
	// blocked is why this run sends nothing more from the mailbox, in plain words (never an
	// address or a password); "" while it may send.
	blocked string
	creds   *secretbox.Credentials
	sender  mailSender
}

func (m *sendMailbox) ready() bool { return m.blocked == "" }

// sendRun is one send (or selftest) after its command line is read: its setup, store,
// zone, mailboxes and report.
type sendRun struct {
	w     io.Writer
	s     *store.Store
	lq    leadstore.Queries
	setup leadsSetup
	loc   *time.Location // LEADHOUND_TZ: the day the cap counts, and a selftest's hours
	live  bool
	box   *mailbox.Outbox
	opts  sendOptions
	rep   *sendReport
	runID int64

	// The Send now request file as read before the lock (--from-request): its batch (0 for a
	// legacy request) or why it was refused.
	reqBatch int64
	reqErr   error
	// manual is the claimed manual send; nil for every other run.
	manual *manualSend
	// skipWindow plans outside the sending window; skipHours sends outside the recipient's
	// business hours. A Send now (--now) sets both; a manual send only with its codes.
	skipWindow, skipHours bool
	// gate is a manual send's email check, read again at send time; nil for other runs.
	gate *emailGate

	// key opens the sealed passwords (live only; shadow never opens one).
	key secretbox.PrivateKey
	// mbs is every mailbox by id; order the ids of those not retired, ascending.
	mbs   map[int64]*sendMailbox
	order []int64
	// selftestMB is the mailbox a selftest sends from.
	selftestMB int64
	// accountErr: a mailbox's login or sender address was refused; the run went on with the
	// other mailboxes and exits non-zero so the operator sees it.
	accountErr error

	lastSync time.Time
	// assigned caches, per campaign id, its assigned mailboxes for the planning loop (the
	// claim transaction reads the assignment again).
	assigned map[int64]map[int64]bool
}

// assignedTo reports whether mailbox mailboxID is assigned to campaign campaignID, as read
// once per campaign per run.
func (r *sendRun) assignedTo(ctx context.Context, campaignID, mailboxID int64) (bool, error) {
	set, ok := r.assigned[campaignID]
	if !ok {
		cms, err := r.lq.CampaignMailboxes(ctx, campaignID)
		if err != nil {
			return false, err
		}
		set = make(map[int64]bool, len(cms))
		for _, cm := range cms {
			set[cm.MailboxID] = true
		}
		if r.assigned == nil {
			r.assigned = map[int64]map[int64]bool{}
		}
		r.assigned[campaignID] = set
	}
	return set[mailboxID], nil
}

// leadsSendRun runs one send in its fixed order, any failure stopping it before SMTP: the
// send lock (RA3); the operator's pause; the mailbox key (live); per mailbox its state, its
// latest check and its domain's DNS check passed today, else run now; the reply sync of
// every mailbox (fail-closed per mailbox); PlanMailboxes with each mailbox's room today; per
// message a re-sync when the last is over 10 minutes old, the checks, MarkSending, Send from
// the message's own mailbox and its outcome; the spacing pause.
func leadsSendRun(ctx context.Context, w io.Writer, opts sendOptions) error {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	setup, err := leadsEnv(cfg, leadsForOutreach)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return usageError("LEADHOUND_TZ: %v", err)
	}
	live := setup.env.Live()
	if opts.selftestTo != "" {
		if err := selftestAllowed(setup, opts.selftestTo); err != nil {
			return err
		}
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	// The console's saved settings over leads.yaml: the sending window saved on Controls is
	// the one this run keeps (leadconfig.Config.SendingWindow).
	if setup, err = applyLeadSettings(ctx, lq, setup); err != nil {
		return err
	}
	var selftestMB int64
	if opts.selftestTo != "" {
		if selftestMB, err = selftestMailbox(ctx, lq, opts.selftestMailbox, startOfDay(leadsNow(), loc)); err != nil {
			return err
		}
	}
	// The request file names the batch only; what the run may do comes from the database row it
	// claims under the lock (claimRequest).
	var reqBatch int64
	var reqErr error
	if opts.fromRequest {
		var kind string
		reqBatch, kind, reqErr = readSendNowRequest(setup.dataDir)
		if kind != sendRequestManual {
			reqBatch = 0
		}
	}
	lock, err := leadstore.TryLock(ctx, s.Pool(), leadstore.SendLockKey)
	if errors.Is(err, leadstore.ErrLockHeld) {
		_, _ = fmt.Fprintln(w, "Another send is running; this one did nothing.")
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()

	r := &sendRun{
		w: w, s: s, lq: lq, setup: setup, loc: loc, live: live, opts: opts, selftestMB: selftestMB,
		rep:      &sendReport{live: live, held: map[outreach.HoldReason]int{}},
		reqBatch: reqBatch, reqErr: reqErr, skipWindow: opts.now, skipHours: opts.now,
	}
	command, source := commandLeadsSend, sourceOutreachShadow
	if opts.selftestTo != "" {
		command = commandLeadsSelftest
	}
	if live {
		source = sourceOutreach
	} else {
		r.box = &mailbox.Outbox{Dir: filepath.Join(setup.dataDir, outboxDir), Location: loc, Now: leadsNow}
	}
	runID, err := s.Q().StartRun(ctx, command, source)
	if err != nil {
		return err
	}
	r.runID = runID
	runErr := r.run(ctx)
	if err := s.Q().FinishRun(context.WithoutCancel(ctx), runID,
		store.RunResult{Note: r.rep.runNote(errors.Join(runErr, r.accountErr))}); err != nil && runErr == nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	r.rep.print(w)
	return r.accountErr
}

// selftestMailbox picks the mailbox a selftest sends from: the one named by want (its id or
// its address), or with want empty the only mailbox that is not retired. The address is
// never echoed.
func selftestMailbox(ctx context.Context, lq leadstore.Queries, want string, since time.Time) (int64, error) {
	ms, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, since)
	if err != nil {
		return 0, err
	}
	var open []leadstore.MailboxStatus
	for _, m := range ms {
		if m.State != leadstore.MailboxRetired {
			open = append(open, m)
		}
	}
	want = strings.TrimSpace(want)
	if want == "" {
		switch len(open) {
		case 0:
			return 0, usageError("a selftest sends from a mailbox, and there is none: add one first (Mailboxes in the console, or leads mailboxes add)")
		case 1:
			return open[0].ID, nil
		}
		return 0, usageError("there is more than one mailbox: name the one to test with --mailbox ID or ADDRESS (leads mailboxes list)")
	}
	for _, m := range open {
		if strconv.FormatInt(m.ID, 10) == want || strings.EqualFold(m.Address, want) {
			return m.ID, nil
		}
	}
	return 0, usageError("--mailbox names no mailbox that is not retired (leads mailboxes list)")
}

func (r *sendRun) printf(format string, a ...any) { _, _ = fmt.Fprintf(r.w, format, a...) }

// run is the fixed order after the lock and the run row.
func (r *sendRun) run(ctx context.Context) error {
	err := r.runOrder(ctx)
	if errors.Is(err, errPaused) {
		return nil
	}
	return err
}

// checkPaused returns errPaused, noted in the report, when the operator has paused sending.
func (r *sendRun) checkPaused(ctx context.Context, lq leadstore.Queries) error {
	paused, _, at, err := lq.SendPaused(ctx)
	if err != nil || !paused {
		return err
	}
	at = at.In(r.loc)
	r.rep.pausedAt = &at
	r.rep.note(notePaused)
	return errPaused
}

// refresh reads every mailbox again (its state, check, sends today). A mailbox that is
// retired, paused or (live) without a passed latest check is blocked; a block, once set,
// stays for the run.
func (r *sendRun) refresh(ctx context.Context, now time.Time) error {
	ms, err := r.lq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(now, r.loc))
	if err != nil {
		return err
	}
	if r.mbs == nil {
		r.mbs = map[int64]*sendMailbox{}
	}
	r.order = r.order[:0]
	for _, m := range ms {
		mb := r.mbs[m.ID]
		if mb == nil {
			mb = &sendMailbox{}
			r.mbs[m.ID] = mb
		}
		if mb.sender != nil && sealChanged(mb.Mailbox, m.Mailbox) {
			// The operator changed its login, a server or its password during the run: the
			// next send opens the password again and checks it still matches.
			mb.sender, mb.creds = nil, nil
		}
		mb.MailboxStatus = m
		switch {
		case m.State == leadstore.MailboxRetired:
			mb.blocked = "it is retired"
			continue
		case m.State == leadstore.MailboxPaused:
			r.block(mb, "it is paused; nothing is sent from it until the operator resumes it", "")
		case r.live && (m.LastCheck == nil || m.LastCheck.Outcome != leadstore.CheckPassed):
			r.block(mb, "its latest check has not passed: "+pressCheckNow, "")
		}
		r.order = append(r.order, m.ID)
	}
	return nil
}

// sealChanged reports whether what a mailbox's sealed password is opened and bound by
// (its login, servers, sealed password or sealing key) differs between a and b.
func sealChanged(a, b leadstore.Mailbox) bool {
	return a.Login != b.Login || a.SMTPAddr != b.SMTPAddr || a.IMAPAddr != b.IMAPAddr ||
		a.SealedBy != b.SealedBy || !bytes.Equal(a.PasswordSealed, b.PasswordSealed)
}

// block stops sending from mb for the rest of the run, saying why once; word, when set, is
// the run note's word for it.
func (r *sendRun) block(mb *sendMailbox, why, word string) {
	if word != "" {
		r.rep.note(word)
	}
	if mb.blocked != "" {
		return
	}
	mb.blocked = why
	r.printf("Mailbox %d: nothing is sent from it in this run: %s.\n", mb.ID, why)
}

// anyReady reports whether a mailbox that is not retired may still send.
func (r *sendRun) anyReady() bool {
	for _, id := range r.order {
		if r.mbs[id].ready() {
			return true
		}
	}
	return false
}

// loads is every mailbox as RoomToday reads it: a blocked one has no room, though its sends
// today still count toward its domain.
func (r *sendRun) loads() []outreach.MailboxLoad {
	out := make([]outreach.MailboxLoad, 0, len(r.mbs))
	for _, mb := range r.mbs {
		ml := outreach.MailboxLoad{ID: mb.ID, Domain: mb.Domain, SentToday: mb.SentToday, Ready: mb.ready()}
		if r.setup.leads.Caps.StartDate.IsFirstSend() && mb.FirstLiveSend != nil {
			fs := mb.FirstLiveSend.In(r.loc)
			ml.FirstSend = &fs
		}
		out = append(out, ml)
	}
	return out
}

// gateDNS runs, for each mailbox that may send and has no DNS pass today for its host and
// selector, the check now (once per host and selector); a failing check blocks it.
func (r *sendRun) gateDNS(ctx context.Context, now time.Time) error {
	checked := map[string]mailbox.DNSResult{}
	for _, id := range r.order {
		mb := r.mbs[id]
		if !mb.ready() || mb.DNSPassedToday {
			continue
		}
		k := mb.Host() + " " + mb.DKIMSelector
		res, ok := checked[k]
		if !ok {
			var err error
			if res, err = checkSenderFor(ctx, r.lq, mb.Host(), mb.DKIMSelector, now); err != nil {
				return err
			}
			checked[k] = res
		}
		if !res.OK() {
			r.block(mb, fmt.Sprintf("its domain's DNS check failed (%s); fix the records, then %s", dnsLine(res), pressCheckNow), noteDNS)
		}
	}
	return nil
}

// runOrder is run's body; it ends with errPaused when the operator's pause stops it.
func (r *sendRun) runOrder(ctx context.Context) error {
	now := leadsNow()

	// Send now's mark first, so a run the pause stops still says it was Send now.
	if r.opts.now {
		r.rep.note(noteSendNow)
	}
	// 0. The operator's pause, before anything else (and before every message, sendOne). It
	// comes before the claim, so a paused run never uses up a manual request: once sending is
	// resumed, the request still works within its 15 minutes.
	if err := r.checkPaused(ctx, r.lq); err != nil {
		return err
	}
	// The Send now request: a manual one is claimed once and checked again before anything is
	// planned, so a refused request sends nothing.
	if r.opts.fromRequest {
		if stop, err := r.claimRequest(ctx, now); stop || err != nil {
			return err
		}
	}
	switch {
	case r.manual != nil:
		over := "no warning is overridden"
		if codes := r.manual.ov.Codes(); len(codes) > 0 {
			over = "overriding " + strings.Join(codes, ", ")
		}
		r.printf("Send now: batch %d alone, %s; suppressions, the email check, paused and unassigned mailboxes, the hard daily ceilings, one email per recipient domain per run and due times still apply.\n",
			r.manual.batch.ID, over)
	case r.opts.now:
		r.printf("Send now: this run skips the sending window only; the warm-up and domain limits, the spacing, paused and unassigned mailboxes, suppressions and due times still apply.\n")
	}

	// 1. The mailbox key: live only; shadow never opens a password.
	if r.live {
		key, err := secretbox.LoadPrivateKey(os.Getenv)
		if err != nil {
			r.rep.note(noteKeyMissing)
			r.printf("No sends: the mailbox key is missing (%v).\n", err)
			r.rep.stopped = true
			return nil
		}
		r.key = key
	}

	// 2. The mailboxes, and per mailbox its domain's DNS passed today.
	if err := r.refresh(ctx, now); err != nil {
		return err
	}
	if len(r.order) == 0 {
		r.rep.note(noteNoMailbox)
		r.printf("No sends: there is no mailbox to send from. Add a mailbox first: Mailboxes in the console, or leads mailboxes add.\n")
		r.rep.stopped = true
		return nil
	}
	if r.live {
		if err := r.gateDNS(ctx, now); err != nil {
			return err
		}
		if !r.anyReady() {
			r.printf("No sends: no mailbox is ready to send.\n")
			r.rep.stopped = true
			return nil
		}
		// 3. The reply sync of every mailbox, fail-closed per mailbox.
		if ok, err := r.sync(ctx, true); err != nil || !ok {
			return err
		}
	} else {
		r.printf("Shadow: no DNS check and no reply sync; the outbox is a file, so nothing can reply.\n")
	}

	// A selftest's one message, approved on the command line, from its own mailbox.
	var only int64
	if r.opts.selftestTo != "" {
		mb := r.mbs[r.selftestMB]
		if mb == nil || !mb.ready() {
			r.printf("No selftest: mailbox %d is not ready to send.\n", r.selftestMB)
			r.rep.stopped = true
			return nil
		}
		st, err := r.createSelftest(ctx, now, mb)
		if err != nil {
			return err
		}
		only = st.MessageID
		r.printf("Selftest %d: message %d is approved for your own address.\n", st.N, st.MessageID)
	}

	// Rows an earlier run left in sending: under the send lock none is in flight.
	if r.live {
		stuck, err := r.lq.SendingIDs(ctx)
		if err != nil {
			return err
		}
		if len(stuck) > 0 {
			r.rep.stuck = stuck
			r.rep.note(noteStuck)
		}
	}

	// A sequence a suppression or a wrong number took a call out of moves on here, with the
	// lead's own calendar: the steps after the call fall due as if it had never been drafted.
	if _, err := r.lq.HealSequences(ctx, now, r.leadCalendar); err != nil {
		return err
	}

	// 4. Plan with each mailbox's room today (its own cap, then its domain's): sent,
	// maybe_sent and sending count, shadow_sent never does.
	rows, err := r.lq.SendRows(ctx, now, !r.live)
	if err != nil {
		return err
	}
	var items, plan []outreach.PlanItem
	retired := map[int64]bool{}
	for _, row := range rows {
		if row.Channel != "email" {
			// A call (or any manual step) is never sent or claimed here, whatever the query
			// returned: the operator makes it and records what happened.
			r.rep.skipped++
			continue
		}
		self := row.LeadKind == leadstore.LeadSelftest
		switch {
		case r.manual != nil:
			// A manual send sends its own batch alone, a selftest lead in it included.
			if row.BatchID != r.manual.batch.ID {
				continue
			}
		case (only != 0) != self || (only != 0 && row.ID != only):
			// A selftest goes in its own run only; an ordinary run never sends one.
			continue
		}
		mb := r.mbs[row.MailboxID]
		switch {
		case mb == nil:
			// Drafted before mailboxes: no From to send it from.
			r.rep.held[holdMailbox]++
			r.rep.note(noteNoMailbox)
			continue
		case mb.State == leadstore.MailboxRetired:
			// A thread must come from the same address, and this one is gone.
			retired[mb.ID] = true
			continue
		case !mb.ready():
			r.rep.held[holdMailbox]++
			continue
		}
		// A live message goes only from a mailbox assigned to its batch's campaign; held here,
		// before PlanMailboxes, it takes none of the mailbox's room. A selftest (its own batch,
		// the operator's address) and a shadow batch (drafted onto every active mailbox) never
		// need one.
		if r.live && !self {
			ok, err := r.assignedTo(ctx, row.CampaignID, row.MailboxID)
			if err != nil {
				return err
			}
			if !ok {
				r.rep.held[holdUnassigned]++
				continue
			}
		}
		items = append(items, outreach.PlanItem{
			MessageID: row.ID, BatchID: row.BatchID, LeadID: row.LeadID, Step: row.Step,
			Domain: recipientDomain(row.Recipient()), Loc: r.rowLoc(row.LeadKind, row.OrgCountry),
			DueAt: *row.DueAt, Selftest: self, MailboxID: row.MailboxID,
		})
	}
	for _, id := range slices.Sorted(maps.Keys(retired)) {
		n, err := r.lq.CancelMailboxPending(ctx, id, cancelMailboxRetired)
		if err != nil {
			return err
		}
		r.rep.cancelled += int64(n)
	}
	bh := r.setup.leads.SendingWindow()
	selfOnly := false
	if r.manual != nil {
		var err error
		if selfOnly, err = r.manualSelftest(items); err != nil {
			return err
		}
	}
	if only != 0 || selfOnly {
		// The operator's own address: no cap (it still counts) and no business hours. A manual
		// batch of selftest leads runs as the selftest does.
		plan = items
		if only != 0 {
			defer r.cancelUnsentSelftest(ctx, only)
		}
		if selfOnly {
			for _, it := range items {
				defer r.cancelUnsentSelftest(ctx, it.MessageID)
			}
		}
	} else {
		var held map[outreach.HoldReason]int
		// The overrides lift only the warm-up ramp and the domain's daily limit, never past the
		// hard ceilings, and never for a mailbox that is not ready (RoomTodayWith).
		var ov outreach.Overrides
		loads, day := r.loads(), now.In(r.loc)
		if r.manual != nil {
			ov = r.manual.ov
			r.manual.limits(r.setup.leads, loads, day, items)
		}
		room := outreach.RoomTodayWith(r.setup.leads, loads, day, ov)
		if r.skipWindow {
			plan, held = outreach.PlanMailboxesAnyHour(items, room, now)
		} else {
			plan, held = outreach.PlanMailboxes(items, room, now, bh)
		}
		for k, v := range held {
			if k != outreach.HoldNotDue {
				r.rep.held[k] += v
			}
		}
	}
	if r.opts.max > 0 && len(plan) > r.opts.max {
		r.rep.overMax = len(plan) - r.opts.max
		plan = plan[:r.opts.max]
	}

	// 5. Per message, with 6. the one spacing between any two sends, whichever mailbox.
	for i, it := range plan {
		if err := ctx.Err(); err != nil {
			r.rep.note(noteInterrupted)
			return nil
		}
		if i > 0 && r.live && leadsNow().Sub(r.lastSync) > resyncAfter {
			if ok, err := r.sync(ctx, false); err != nil || !ok {
				return err
			}
		}
		mb := r.mbs[it.MailboxID]
		if !mb.ready() {
			// Blocked during the run: a pause, a refused login, a policy block, a failed sync.
			r.rep.held[holdMailbox]++
			continue
		}
		// Business hours at the moment of sending (RA8): the spacing may have carried this
		// recipient's zone past its window; other zones may still be open.
		if !it.Selftest && !r.skipHours && !outreach.InBusinessHours(it.Loc, bh, leadsNow()) {
			r.rep.held[outreach.HoldHours]++
			continue
		}
		attempted, err := r.sendOne(ctx, it.MessageID, mb)
		if err != nil {
			return err
		}
		if attempted && r.live && i < len(plan)-1 {
			sp := r.setup.leads.Schedule.SpacingMinutes
			var minutes [2]int
			copy(minutes[:], sp)
			d := outreach.Spacing(leadsMail.rand, minutes)
			if r.manual != nil && r.manual.ov.Has(outreach.OverrideSpacingJitter) {
				// The jitter only: the run still waits the minimum between two sends.
				d = outreach.MinSpacing
			}
			// Nothing left would be in its hours after the pause: stop now.
			if !r.skipHours && !anyInHours(plan[i+1:], bh, leadsNow().Add(d)) {
				r.rep.held[outreach.HoldHours] += len(plan) - i - 1
				return nil
			}
			if err := leadsMail.sleep(ctx, d); err != nil {
				r.rep.note(noteInterrupted)
				return nil
			}
		}
	}
	return nil
}

// claimRequest reads what --from-request asks. A legacy request (a time, or an empty file) is
// today's Send now and claims nothing. A manual one takes its batch's newest send_request row
// no older than sendRequestMaxAge and checks it: the batch is there, manual, approved, not
// discarded and of this run's kind (live or shadow); the hash the operator confirmed is its
// content hash, which its rows still hash to; every code is in the closed set. Only a request
// that passes is claimed (marked used), in the same transaction as the checks, so it is claimed
// once and a refused one stays unused: fixed within its 15 minutes, it can still go, and the
// console's Send now again writes a fresh one. stop is true when the run must end with nothing
// sent; a refusal is also an error, so the unit fails where the operator sees it. It runs after
// the pause check, never before.
func (r *sendRun) claimRequest(ctx context.Context, now time.Time) (stop bool, err error) {
	refuse := func(why string) (bool, error) {
		r.rep.note(noteRequestRefused)
		r.rep.stopped = true
		r.printf("No sends: the Send now request is refused: %s.\n", why)
		return true, fmt.Errorf("leads send: send now request refused: %s", why)
	}
	if r.reqErr != nil {
		return refuse(r.reqErr.Error())
	}
	if r.reqBatch == 0 {
		return false, nil
	}
	var (
		req     leadstore.SendRequest
		b       leadstore.Batch
		ov      outreach.Overrides
		gate    *emailGate
		found   bool
		refusal string
	)
	// A refusal rolls the transaction back, so the request is not used.
	errRefused := errors.New("send now request refused")
	no := func(format string, a ...any) error {
		refusal = fmt.Sprintf(format, a...)
		return errRefused
	}
	err = leadsInTx(ctx, r.s.Pool(), func(tq leadstore.Queries) error {
		var err error
		if req, found, err = tq.PendingSendRequest(ctx, r.reqBatch, now.Add(-sendRequestMaxAge)); err != nil || !found {
			return err
		}
		b, err = tq.BatchByID(ctx, req.BatchID)
		switch {
		case errors.Is(err, leadstore.ErrBatchNotFound):
			return no("batch %d is not there", req.BatchID)
		case err != nil:
			return err
		case !b.Manual:
			return no("batch %d is not a manual batch", b.ID)
		case b.DiscardedAt != nil:
			return no("batch %d is discarded", b.ID)
		case b.ApprovedAt == nil:
			return no("batch %d is not approved", b.ID)
		case b.Shadow == r.live:
			return no("batch %d is a %s batch and sending is %s", b.ID, map[bool]string{true: "shadow", false: "live"}[b.Shadow],
				map[bool]string{true: "live", false: "shadow"}[r.live])
		}
		prefix := strings.ToLower(strings.TrimSpace(req.HashPrefix))
		if len(prefix) < leadstore.MinHashPrefix || !strings.HasPrefix(hex.EncodeToString(b.ContentSHA256), prefix) ||
			!bytes.Equal(b.ApprovedHash, b.ContentSHA256) {
			return no("batch %d is not the batch that was confirmed (its hash differs)", b.ID)
		}
		sum, err := tq.BatchContentSHA256(ctx, b.ID)
		if err != nil {
			return err
		}
		if !bytes.Equal(sum, b.ContentSHA256) {
			return no("batch %d changed after it was written (its messages no longer hash to it)", b.ID)
		}
		if ov, err = outreach.ParseOverrides(req.Codes); err != nil {
			return no("%v", err)
		}
		if gate, err = loadEmailGate(ctx, tq, batchSetup{leads: r.setup.leads, shadow: !r.live, loc: r.loc}, now); err != nil {
			return err
		}
		// Every check passed: claim it, last.
		if ok, err := tq.UseSendRequest(ctx, req.ID, now); err != nil || !ok {
			found = false
			return err
		}
		return nil
	})
	switch {
	case errors.Is(err, errRefused):
		return refuse(refusal)
	case err != nil:
		return true, err
	case !found:
		r.rep.note(noteNoRequest)
		r.rep.stopped = true
		r.printf("No sends: batch %d has no confirmed Send now waiting (it was sent already, or confirmed over %d minutes ago); press Send now again on its batch page in the console.\n",
			r.reqBatch, int(sendRequestMaxAge.Minutes()))
		return true, nil
	}
	r.manual = &manualSend{batch: b, req: req, ov: ov, detail: map[int64]*limitDetail{}}
	r.gate = gate
	r.skipWindow = ov.Has(outreach.OverrideWindow) || ov.Has(outreach.OverrideSchedule)
	r.skipHours = ov.Has(outreach.OverrideBusinessHours)
	if codes := ov.Codes(); len(codes) > 0 {
		r.rep.note(noteOverride + strings.Join(codes, ","))
	}
	return false, nil
}

// manualSelftest reports whether a manual batch's due items are its selftest leads alone, which
// then run as the selftest does. A batch mixing selftest and other leads is refused: nothing
// of it is sent.
func (r *sendRun) manualSelftest(items []outreach.PlanItem) (bool, error) {
	self, other := 0, 0
	for _, it := range items {
		if it.Selftest {
			self++
		} else {
			other++
		}
	}
	if self > 0 && other > 0 {
		r.rep.note(noteRequestRefused)
		r.rep.stopped = true
		r.printf("No sends: batch %d mixes selftest and other leads; send a selftest batch alone.\n", r.manual.batch.ID)
		return false, fmt.Errorf("leads send: send now request refused: batch %d mixes selftest and other leads", r.manual.batch.ID)
	}
	return self > 0, nil
}

// limits keeps, per mailbox of items, what outreach.CheckLimits says about sending them all
// now (the reasoning the confirm page showed), for the send_override rows.
func (m *manualSend) limits(cfg leadconfig.Config, loads []outreach.MailboxLoad, day time.Time, items []outreach.PlanItem) {
	need := map[int64]int{}
	for _, it := range items {
		if !it.Selftest {
			need[it.MailboxID]++
		}
	}
	warns, stops := outreach.CheckLimits(cfg, loads, day, need)
	room, plain := outreach.RoomTodayWith(cfg, loads, day, m.ov), outreach.RoomToday(cfg, loads, day)
	out := map[int64]*limitDetail{}
	for id, n := range need {
		out[id] = &limitDetail{MailboxID: id, Need: n, Room: room[id], RoomPlain: plain[id], Warnings: []limitWarnJSON{}, Stops: []limitStopJSON{}}
	}
	for _, w := range warns {
		if d := out[w.MailboxID]; d != nil {
			d.Warnings = append(d.Warnings, limitWarnJSON{Code: string(w.Code), Domain: w.Domain, Used: w.Used, Allowed: w.Allowed, Over: w.Over})
		}
	}
	for _, s := range stops {
		if d := out[s.MailboxID]; d != nil {
			d.Stops = append(d.Stops, limitStopJSON{Reason: s.Reason, Domain: s.Domain, Used: s.Used, Allowed: s.Allowed})
		}
	}
	maps.Copy(m.detail, out)
}

// recordOverride writes the send_override row of a message a manual send with overrides has
// just sent (or, shadow, written to the outbox). It runs after the send is committed, in its own
// statement: a failure stops the run, so nothing more goes without its record, but the message
// already sent stays sent without its send_override row. That gap is accepted: the email went
// and cannot be called back, and the run's note still says "override: <codes>" for the batch.
func (r *sendRun) recordOverride(ctx context.Context, row leadstore.SendRow) error {
	m := r.manual
	if m == nil || len(m.ov.Codes()) == 0 {
		return nil
	}
	d := limitDetail{MailboxID: row.MailboxID, Warnings: []limitWarnJSON{}, Stops: []limitStopJSON{}}
	if got := m.detail[row.MailboxID]; got != nil {
		d = *got
	}
	d.Shown = m.req.Shown
	if len(d.Shown) == 0 {
		d.Shown = json.RawMessage(`[]`)
	}
	detail, err := json.Marshal(d)
	if err != nil {
		return err
	}
	runID, msgID := r.runID, row.ID
	o := leadstore.SendOverride{BatchID: row.BatchID, LeadID: row.LeadID, MessageID: &msgID, Codes: m.ov.Codes(),
		Detail: detail, Operator: m.req.Operator}
	if runID != 0 {
		o.RunID = &runID
	}
	return r.lq.InsertSendOverride(ctx, o)
}

// emailCheck is a manual send's email check at the moment of sending: a fresh check that
// blocks the address refuses the message (live or shadow, whether the check is on or off); while
// the check is on, an address without a fresh passing check is held. ok is false when the
// message must not go now. A selftest (the operator's own address) is never checked.
func (r *sendRun) emailCheck(ctx context.Context, row leadstore.SendRow, to, label string) (ok bool, err error) {
	if r.gate == nil || row.LeadKind == leadstore.LeadSelftest {
		return true, nil
	}
	if err := r.gate.load(ctx, r.lq, to); err != nil {
		return false, err
	}
	switch v := r.gate.verdict(to); {
	case v.Decision == verify.Block:
		r.rep.note(noteEmailCheck)
		r.rep.refused = append(r.rep.refused, label+" (email check: "+v.Status+")")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: email check "+v.Status)
	case v.Decision != verify.Send && r.gate.on:
		r.rep.note(noteEmailCheck)
		r.rep.held[holdEmailCheck]++
		return false, nil
	}
	return true, nil
}

// anyInHours reports whether any of items is inside its business hours at t.
func anyInHours(items []outreach.PlanItem, bh leadconfig.BusinessHours, t time.Time) bool {
	for _, it := range items {
		if it.Selftest || outreach.InBusinessHours(it.Loc, bh, t) {
			return true
		}
	}
	return false
}

// cancelUnsentSelftest cancels a selftest message its run did not send, so no later
// ordinary send picks it up.
func (r *sendRun) cancelUnsentSelftest(ctx context.Context, id int64) {
	row, err := r.lq.SendRowByID(context.WithoutCancel(ctx), id)
	if err != nil || row.State != leadstore.MsgApproved {
		return
	}
	if n, err := r.lq.CancelPending(context.WithoutCancel(ctx), leadstore.CancelTarget{LeadID: row.LeadID}, selftestNotSent); err == nil && n > 0 {
		r.printf("Selftest message %d was not sent; it is cancelled (%s). Run leads selftest again.\n", id, selftestNotSent)
	}
}

// sync runs the reply sync of every mailbox and reports whether sending may go on at all
// (false: the sync lock stayed held, or the sync could not run). A mailbox whose own sync
// failed is blocked for the run; the others go on. Findings are printed (the first sync's
// always, a re-sync's when it saw something), headed by the mailbox when there are several.
func (r *sendRun) sync(ctx context.Context, first bool) (bool, error) {
	res, err := syncAll(ctx, r.s.Pool(), r.key, r.setup.leads, leadsMail.syncWait)
	switch {
	case errors.Is(err, errSyncRunning):
		r.rep.note(noteSyncRunning)
		r.printf("No further sends: a sync is still running after %s; run send again later.\n", leadsMail.syncWait)
		return false, nil
	case err != nil:
		r.rep.note(noteSync)
		r.printf("No further sends: the reply sync failed (%v); nothing is sent until a sync succeeds.\n", err)
		return false, nil
	}
	r.lastSync = leadsNow()
	for _, id := range r.order {
		mb := r.mbs[id]
		ms, ok := res[id]
		switch {
		case !ok:
			r.block(mb, "its reply sync did not run; nothing is sent from it until a sync succeeds", noteSync)
			continue
		case !ms.OK:
			r.block(mb, "the reply sync failed ("+ms.Note+"); nothing is sent from it until a sync succeeds", noteSync)
			continue
		}
		if first || ms.Summary.Messages > 0 {
			if len(res) > 1 {
				r.printf("Mailbox %d:\n", id)
			}
			ms.Summary.print(r.w)
		}
	}
	// A sync can pause a mailbox (its password, or a bounce that tips its health).
	if err := r.refresh(ctx, leadsNow()); err != nil {
		return false, err
	}
	return true, nil
}

// rowLoc is the zone whose business hours a message keeps: its recipient country's, or the
// operator's for a selftest.
func (r *sendRun) rowLoc(kind, country string) *time.Location {
	if kind == leadstore.LeadSelftest {
		return r.loc
	}
	return r.setup.leads.Policy(country).Location()
}

func recipientDomain(addr string) string {
	_, d, _ := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	return d
}

// Why a claim found the message's mailbox unable to send.
var (
	errClaimRetired = errors.New("the mailbox is retired")
	errClaimPaused  = errors.New("the mailbox is paused")
	// errClaimUnassigned: the mailbox was unassigned from the message's campaign during the run.
	errClaimUnassigned = errors.New("the mailbox is not assigned to the campaign")
	// errClaimFirstGone: the lead's first email already went (or is going) in another message.
	errClaimFirstGone = errors.New("the lead's first email already went")
)

// mailboxState reads mailbox id's latest state through tq (the claim's transaction).
func (r *sendRun) mailboxState(ctx context.Context, tq leadstore.Queries, id int64) (string, error) {
	ms, err := tq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(leadsNow(), r.loc))
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if m.ID == id {
			return m.State, nil
		}
	}
	return "", fmt.Errorf("leads send: %w", leadstore.ErrMailboxNotFound)
}

// senderOf opens mb's sealed password, only when it still matches mb's address, login and
// servers (mailboxCreds pauses it otherwise), and builds its SMTP sender. ok is false when
// the password cannot be used: mb is then blocked.
func (r *sendRun) senderOf(ctx context.Context, mb *sendMailbox) (ok bool, err error) {
	if mb.sender != nil {
		return true, nil
	}
	creds, err := mailboxCreds(ctx, r.lq, r.key, mb.Mailbox)
	if errors.Is(err, errMailboxCreds) {
		r.block(mb, "its password cannot be used ("+err.Error()+")", noteMailboxPaused)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	mb.creds = &creds
	mb.sender = leadsMail.smtpFor(creds.SMTPAddr, config.Secret(creds.Login), creds.Password)
	return true, nil
}

// accountReason is the plain reason a mailbox is paused for when the server refused its
// account at stage.
func accountReason(stage string) string {
	switch stage {
	case "auth":
		return "the server refused the login"
	case "mail":
		return "the server refused mail from this address"
	}
	return "the server refused the account at " + stage
}

// policyReason is the reason every mailbox on domain is paused for when a receiving server
// blocked our mail by policy (an enhanced status code 5.7.x at RCPT).
func policyReason(domain string, code [3]int) string {
	return fmt.Sprintf("a receiving server blocked mail from %s (%d.%d.%d)", domain, code[0], code[1], code[2])
}

// sendOne checks message id once more and sends it from mb, its own mailbox. attempted
// reports whether SMTP (or the outbox) was reached. An error is a store failure, or an
// outbox or sender that cannot work; it stops the run.
func (r *sendRun) sendOne(ctx context.Context, id int64, mb *sendMailbox) (attempted bool, err error) {
	row, err := r.lq.SendRowByID(ctx, id)
	if err != nil {
		return false, err
	}
	// Claimed or cancelled since Plan (another sender, a sync), or the other kind of batch
	// (RA1: a shadow send never touches a live batch, nor a live send a shadow one).
	if row.Channel != "email" || row.State != leadstore.MsgApproved || row.Shadow == r.live || row.MailboxID != mb.ID {
		r.rep.skipped++
		return false, nil
	}
	label := fmt.Sprintf("message %d", row.ID)

	// Suppression, the third point.
	set, err := r.lq.SuppressionSet(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return false, err
	}
	// An email_consent step goes to the address they agreed to on the call, and only there.
	if why := row.RecipientRefusal(); why != "" {
		r.rep.note(noteConsent)
		r.rep.refused = append(r.rep.refused, label+" ("+why+")")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: "+why)
	}
	to := row.Recipient()
	if outreach.Suppressed(to, row.OrgDomain, outreach.NewSuppressionSet(set.Addresses, set.Domains)) {
		return false, r.cancel(ctx, row, leadstore.SkipSuppressed)
	}
	// The lead's state: a live batch's lead is batched or contacted, a shadow batch's new.
	canSend := outreach.CanSend(row.LeadState)
	if !r.live {
		canSend = row.LeadState == leadstore.LeadNew
	}
	if !canSend {
		return false, r.cancel(ctx, row, leadstore.SkipLeadState+row.LeadState)
	}
	// A manual send checks the address once more (amendment 6).
	if ok, err := r.emailCheck(ctx, row, to, label); err != nil || !ok {
		return false, err
	}
	// Fixture names never get a live message (RA25).
	if r.live && !leadsMail.allowReserved &&
		(outreach.ReservedDomain(recipientDomain(to)) || outreach.ReservedDomain(row.OrgDomain)) {
		r.rep.note(noteReserved)
		r.rep.refused = append(r.rep.refused, label+" ("+noteReserved+")")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: reserved recipient")
	}
	// What the operator approved, byte for byte (RA2).
	if !bytes.Equal(leadstore.MessageSHA256(row.ToAddress, row.Subject, row.BodyText, row.InReplyTo), row.ApprovedSHA256) {
		r.rep.note(noteTampered)
		r.rep.refused = append(r.rep.refused, label+" ("+noteTampered+")")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: tampered (the message changed after approval)")
	}
	// The password is opened only for a live send, and only when it matches the mailbox.
	if r.live {
		ok, err := r.senderOf(ctx, mb)
		if err != nil {
			return false, err
		}
		if !ok {
			r.rep.held[holdMailbox]++
			return false, nil
		}
	}
	// From the message's own mailbox, under the From name frozen with it as it is: an empty
	// one is no display name, never leads.yaml's name of today.
	from := row.FromAddress
	if from == "" {
		from = mb.Address
	}
	now := leadsNow()
	msg := row.Message
	msg.ToAddress = to // an email_consent step: the address they agreed to (checked above)
	raw, err := mailbox.Build(outboxMessage(msg, row.FromName, from, now))
	if err != nil {
		r.rep.refused = append(r.rep.refused, label+" (not a valid message)")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: "+err.Error())
	}
	var claimed bool
	var firstBatch int64 // the batch the lead's first email already went in
	err = leadsInTx(ctx, r.s.Pool(), func(tq leadstore.Queries) error {
		// The pause is read in the claim's transaction, the last moment before SMTP, and so
		// is the state of the message's mailbox.
		if err := r.checkPaused(ctx, tq); err != nil {
			return err
		}
		switch state, err := r.mailboxState(ctx, tq, mb.ID); {
		case err != nil:
			return err
		case state == leadstore.MailboxRetired:
			return errClaimRetired
		case state == leadstore.MailboxPaused:
			return errClaimPaused
		}
		if r.live && row.LeadKind != leadstore.LeadSelftest {
			switch ok, err := tq.MailboxAssigned(ctx, row.CampaignID, mb.ID); {
			case err != nil:
				return err
			case !ok:
				return errClaimUnassigned
			}
		}
		// One first email per lead, whatever batches it is in: a lead in two batches (a manual
		// one and another) never gets a second, live or shadow. Read in the claim's
		// transaction, under the send lock, so no other send slips between.
		if row.Step == 0 {
			switch b, gone, err := tq.FirstEmailGone(ctx, row.LeadID, row.ID, row.Shadow); {
			case err != nil:
				return err
			case gone:
				firstBatch = b
				return errClaimFirstGone
			}
		}
		var err error
		if claimed, err = tq.MarkSending(ctx, row.ID); err != nil || !claimed {
			return err
		}
		return tq.StampAttempt(ctx, row.ID, now)
	})
	switch {
	case errors.Is(err, errClaimRetired):
		r.block(mb, "it is retired", "")
		n, err := r.lq.CancelMailboxPending(ctx, mb.ID, cancelMailboxRetired)
		r.rep.cancelled += int64(n)
		return false, err
	case errors.Is(err, errClaimUnassigned):
		r.rep.held[holdUnassigned]++
		return false, nil
	case errors.Is(err, errClaimFirstGone):
		why := fmt.Sprintf("its first email already went in batch %d", firstBatch)
		r.rep.note(noteFirstGone)
		r.rep.refused = append(r.rep.refused, label+" ("+why+")")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: "+why)
	case errors.Is(err, errClaimPaused):
		r.block(mb, "it is paused; nothing is sent from it until the operator resumes it", noteMailboxPaused)
		r.rep.held[holdMailbox]++
		return false, nil
	case err != nil || !claimed:
		if err == nil {
			r.rep.skipped++
		}
		return false, err
	}
	domain := recipientDomain(to)
	if row.LeadKind == leadstore.LeadSelftest {
		domain = "your selftest address"
	}

	if !r.live {
		if _, err := r.box.Write(raw); err != nil {
			if rqErr := r.lq.Requeue(context.WithoutCancel(ctx), row.ID, *row.DueAt, ""); rqErr != nil {
				return false, errors.Join(err, rqErr)
			}
			return false, err
		}
		if err := r.lq.MarkShadowSent(ctx, row.ID, now); err != nil {
			return true, err
		}
		r.rep.sent = append(r.rep.sent, domain)
		r.followUp(ctx, row, now)
		return true, r.recordOverride(ctx, row)
	}

	reply, sendErr := mb.sender.Send(ctx, from, to, raw)
	// The outcome is recorded even when ctx ended during the send.
	wctx := context.WithoutCancel(ctx)
	var se *mailbox.SendError
	switch {
	case sendErr == nil:
		// The send is committed on its own: a scheduling failure never undoes it.
		if err := leadsInTx(wctx, r.s.Pool(), func(tq leadstore.Queries) error {
			return tq.MarkSent(wctx, row.ID, reply, now)
		}); err != nil {
			return true, err
		}
		r.rep.sent = append(r.rep.sent, domain)
		r.followUp(wctx, row, now)
		r.appendSent(ctx, mb, raw)
		// After SMTP accepted it: a failure here leaves the email sent without its override row
		// (recordOverride says why that is accepted).
		if err := r.recordOverride(wctx, row); err != nil {
			return true, err
		}
	case mailbox.IsPolicyBlock(sendErr) && errors.As(sendErr, &se):
		// Our domain, not this lead: every mailbox on the domain pauses until the operator
		// resumes it. The message is refused (its address is not suppressed): kept approved,
		// it would be first again after the resume and pause the domain once more.
		r.rep.note(notePolicyBlock)
		r.rep.refused = append(r.rep.refused, label+" (blocked by the receiving server's policy)")
		if err := r.lq.MarkRefused(wctx, row.ID, se.Error()); err != nil {
			return true, err
		}
		if _, err := r.lq.SetDomainState(wctx, mb.Domain, leadstore.MailboxPaused, policyReason(mb.Domain, se.Enhanced), leadstore.ActorSystem); err != nil {
			return true, err
		}
		why := fmt.Sprintf("a receiving server blocked mail from its domain (%d.%d.%d); every mailbox on the domain is paused until the operator resumes it",
			se.Enhanced[0], se.Enhanced[1], se.Enhanced[2])
		for _, id := range r.order {
			if other := r.mbs[id]; strings.EqualFold(other.Domain, mb.Domain) {
				r.block(other, why, noteMailboxPaused)
			}
		}
		r.printf("Message %d is refused: that server blocked it by policy, and nothing is suppressed.\n", row.ID)
	case errors.As(sendErr, &se) && se.Kind == mailbox.Refused:
		r.rep.refused = append(r.rep.refused, label+" (refused by the server)")
		if err := leadsInTx(wctx, r.s.Pool(), func(tq leadstore.Queries) error {
			if err := tq.MarkRefused(wctx, row.ID, se.Error()); err != nil {
				return err
			}
			if se.Bounce {
				if err := tq.MarkHardBounce(wctx, row.ID); err != nil {
					return err
				}
				_, err := tq.Suppress(wctx, leadstore.DefaultAccountID, leadstore.SuppressAddress, to, leadstore.ReasonBounce, "smtp")
				return err
			}
			return nil
		}); err != nil {
			return true, err
		}
		if se.Bounce {
			paused, err := checkBounceHealth(wctx, r.lq, r.setup.leads, mb.ID)
			if err != nil {
				return true, err
			}
			if paused {
				r.block(mb, "it hard-bounced too often (health.bounce_pause) and is paused until the operator resumes it", noteMailboxPaused)
			}
		}
	case errors.As(sendErr, &se) && se.Kind == mailbox.Account:
		// The mailbox, not this message: back to approved, nothing suppressed, the mailbox
		// paused, and the run ends non-zero so the operator sees it.
		r.rep.note(noteAccount)
		if err := r.lq.Requeue(wctx, row.ID, *row.DueAt, se.Error()); err != nil {
			return true, err
		}
		reason := accountReason(se.Stage)
		if err := r.lq.SetMailboxState(wctx, mb.ID, leadstore.MailboxPaused, reason, leadstore.ScopeMailbox, leadstore.ActorSystem); err != nil &&
			!errors.Is(err, leadstore.ErrMailboxRetired) {
			return true, err
		}
		r.block(mb, fmt.Sprintf("%s (%s, %d); it is paused until the operator resumes it, and message %d stays approved",
			reason, se.Stage, se.Code, row.ID), "")
		r.accountErr = errors.Join(r.accountErr, fmt.Errorf("leads send: mailbox %d: %s at %s (%d)", mb.ID, reason, se.Stage, se.Code))
	case errors.As(sendErr, &se) && se.Kind == mailbox.Retryable:
		r.rep.retry++
		if err := r.lq.Requeue(wctx, row.ID, now.Add(retryAfter), se.Error()); err != nil {
			return true, err
		}
	case errors.As(sendErr, &se):
		r.rep.maybe = append(r.rep.maybe, row.ID)
		if err := leadsInTx(wctx, r.s.Pool(), func(tq leadstore.Queries) error {
			if err := tq.MarkMaybeSent(wctx, row.ID, se.Error()); err != nil {
				return err
			}
			return tq.StampAttempt(wctx, row.ID, now)
		}); err != nil {
			return true, err
		}
	default:
		// The sender could not run at all (ErrSMTPConfig): nothing was sent.
		if err := r.lq.Requeue(wctx, row.ID, *row.DueAt, ""); err != nil {
			return false, errors.Join(sendErr, err)
		}
		return false, sendErr
	}
	return true, nil
}

// cancel cancels the lead's pending messages with reason: send found the lead suppressed or
// moved on.
func (r *sendRun) cancel(ctx context.Context, row leadstore.SendRow, reason string) error {
	n, err := r.lq.CancelPending(ctx, leadstore.CancelTarget{LeadID: row.LeadID}, reason)
	r.rep.cancelled += n
	return err
}

// followUp schedules the sequence's next step in its own statement, after the send is
// committed; a failure is the note followup.
func (r *sendRun) followUp(ctx context.Context, row leadstore.SendRow, sentAt time.Time) {
	var err error
	if r.s != nil {
		err = leadsInTx(ctx, r.s.Pool(), func(tq leadstore.Queries) error { return r.scheduleNext(ctx, tq, row, sentAt) })
	} else {
		err = r.scheduleNext(ctx, r.lq, row, sentAt)
	}
	if err != nil {
		r.rep.note(noteFollowup)
	}
}

// scheduleNext moves the lead's sequence on after this send, with tq (run it in one
// transaction: the decision is applied over the rows it was read from). A batch that stores
// its sequence runs campaign.Next (leadstore.SequenceDecision) and applies its due times and
// cancels; a batch from before sequences (none stored) keeps the old rule, legacyNext.
// leadCalendar is the business-day calendar of a lead of kind in country, as scheduleNext
// builds it.
func (r *sendRun) leadCalendar(kind, country string) campaign.Calendar {
	loc, bh := r.rowLoc(kind, country), r.setup.leads.SendingWindow()
	return campaign.CalendarFunc(func(from time.Time, n int) time.Time { return outreach.FollowupDue(from, n, loc, bh) })
}

func (r *sendRun) scheduleNext(ctx context.Context, tq leadstore.Queries, row leadstore.SendRow, sentAt time.Time) error {
	d, legacy, err := tq.SequenceDecision(ctx, row.ID, sentAt, r.leadCalendar(row.LeadKind, row.OrgCountry))
	if err != nil {
		return err
	}
	if legacy {
		return r.legacyNext(ctx, tq, row, sentAt)
	}
	_, err = tq.ApplyDecision(ctx, d)
	return err
}

// legacyNext gives a legacy batch's next step its due time from this send: step 1
// followup_business_days[0] business days after step 0, step 2 [1] days after step 1.
func (r *sendRun) legacyNext(ctx context.Context, tq leadstore.Queries, row leadstore.SendRow, sentAt time.Time) error {
	days := r.setup.leads.Schedule.FollowupBusinessDays
	if row.Step >= len(days) || row.Step >= 2 {
		return nil
	}
	due := outreach.FollowupDue(sentAt, days[row.Step], r.rowLoc(row.LeadKind, row.OrgCountry), r.setup.leads.SendingWindow())
	if due.IsZero() {
		return nil
	}
	_, err := tq.ScheduleFollowup(ctx, row.BatchID, row.LeadID, row.Step+1, due)
	return err
}

// appendSent files a copy of a sent message in the \Sent folder of mb, the mailbox it went
// from, so replies thread in a mail client. A failure is a note only: the message has gone.
func (r *sendRun) appendSent(ctx context.Context, mb *sendMailbox, raw []byte) {
	if mb.creds == nil {
		r.rep.note(noteAppend)
		return
	}
	rd, err := leadsMail.readerFor(ctx, mb.creds.IMAPAddr, config.Secret(mb.creds.Login), mb.creds.Password)
	if err != nil {
		r.rep.note(noteAppend)
		return
	}
	defer func() { _ = rd.Close() }()
	if err := rd.AppendSent(raw); err != nil {
		r.rep.note(noteAppend)
	}
}
