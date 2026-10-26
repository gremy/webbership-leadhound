package main

import (
	"bytes"
	"context"
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

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/store"
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
)

// holdMailbox holds a due message whose mailbox may not send in this run: paused, not
// checked, its DNS or sync failed, its login refused, or no mailbox at all.
const holdMailbox outreach.HoldReason = "mailbox"

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
	for _, h := range []outreach.HoldReason{outreach.HoldCap, outreach.HoldHours, outreach.HoldDomain, outreach.HoldNotDue, holdMailbox} {
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
	for _, h := range []outreach.HoldReason{outreach.HoldCap, outreach.HoldHours, outreach.HoldDomain, holdMailbox} {
		if n := r.held[h]; n > 0 {
			held = append(held, fmt.Sprintf("%s %d", map[outreach.HoldReason]string{
				outreach.HoldCap: "today's cap", outreach.HoldHours: "outside business hours", outreach.HoldDomain: "one per domain per run",
				holdMailbox: "its mailbox cannot send now",
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

// runLeadsSend is `leads send [--max N]`, or `leads send pause [--reason TEXT]` and
// `leads send resume`.
func runLeadsSend(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "pause" || args[0] == "resume") {
		return runLeadsSendPause(ctx, args[0] == "pause", args[1:])
	}
	fs := flag.NewFlagSet("leads send", flag.ContinueOnError)
	maxN := fs.Int("max", 0, "send at most this many messages")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *maxN < 0 {
		return usageError("%s", sendUsage)
	}
	return leadsSendRun(ctx, stdout, sendOptions{max: *maxN})
}

const sendUsage = "usage: leadhound leads send [--max N] | leads send pause [--reason TEXT] | leads send resume"

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
	sw, err := setSendPause(ctx, leadstore.NewQueries(s.Pool()), pause, why)
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
	var selftestMB int64
	if opts.selftestTo != "" {
		if selftestMB, err = selftestMailbox(ctx, lq, opts.selftestMailbox, startOfDay(leadsNow(), loc)); err != nil {
			return err
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
		rep: &sendReport{live: live, held: map[outreach.HoldReason]int{}},
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
	ms, err := lq.Mailboxes(ctx, since)
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
	ms, err := r.lq.Mailboxes(ctx, startOfDay(now, r.loc))
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
		mb.MailboxStatus = m
		switch {
		case m.State == leadstore.MailboxRetired:
			mb.blocked = "it is retired"
			continue
		case m.State == leadstore.MailboxPaused:
			r.block(mb, "it is paused; nothing is sent from it until the operator resumes it", "")
		case r.live && (m.LastCheck == nil || m.LastCheck.Outcome != leadstore.CheckPassed):
			r.block(mb, "its latest check has not passed: press Check on its mailbox", "")
		}
		r.order = append(r.order, m.ID)
	}
	return nil
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

// gateDNS runs, for each mailbox that may send and has no DNS pass today for its domain and
// selector, the check now (once per domain and selector); a failing check blocks it.
func (r *sendRun) gateDNS(ctx context.Context, now time.Time) error {
	checked := map[string]mailbox.DNSResult{}
	for _, id := range r.order {
		mb := r.mbs[id]
		if !mb.ready() || mb.DNSPassedToday {
			continue
		}
		k := strings.ToLower(mb.Domain) + " " + mb.DKIMSelector
		res, ok := checked[k]
		if !ok {
			var err error
			if res, err = checkSenderFor(ctx, r.lq, mb.Domain, mb.DKIMSelector, now); err != nil {
				return err
			}
			checked[k] = res
		}
		if !res.OK() {
			r.block(mb, fmt.Sprintf("its domain's DNS check failed (%s); fix the records, then press Check on its mailbox", dnsLine(res)), noteDNS)
		}
	}
	return nil
}

// runOrder is run's body; it ends with errPaused when the operator's pause stops it.
func (r *sendRun) runOrder(ctx context.Context) error {
	now := leadsNow()

	// 0. The operator's pause, before anything else (and before every message, sendOne).
	if err := r.checkPaused(ctx, r.lq); err != nil {
		return err
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

	// 4. Plan with each mailbox's room today (its own cap, then its domain's): sent,
	// maybe_sent and sending count, shadow_sent never does.
	rows, err := r.lq.SendRows(ctx, now, !r.live)
	if err != nil {
		return err
	}
	var items, plan []outreach.PlanItem
	retired := map[int64]bool{}
	for _, row := range rows {
		self := row.LeadKind == leadstore.LeadSelftest
		// A selftest goes in its own run only; an ordinary run never sends one.
		if (only != 0) != self || (only != 0 && row.ID != only) {
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
		items = append(items, outreach.PlanItem{
			MessageID: row.ID, BatchID: row.BatchID, LeadID: row.LeadID, Step: row.Step,
			Domain: recipientDomain(row.ToAddress), Loc: r.rowLoc(row.LeadKind, row.OrgCountry),
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
	bh := r.setup.leads.Schedule.BusinessHours
	if only != 0 {
		// The operator's own address: no cap (it still counts) and no business hours.
		plan = items
		defer r.cancelUnsentSelftest(ctx, only)
	} else {
		var held map[outreach.HoldReason]int
		plan, held = outreach.PlanMailboxes(items, outreach.RoomToday(r.setup.leads, r.loads(), now.In(r.loc)), now, bh)
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
		if !it.Selftest && !outreach.InBusinessHours(it.Loc, bh, leadsNow()) {
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
			// Nothing left would be in its hours after the pause: stop now.
			if !anyInHours(plan[i+1:], bh, leadsNow().Add(d)) {
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
)

// mailboxState reads mailbox id's latest state through tq (the claim's transaction).
func (r *sendRun) mailboxState(ctx context.Context, tq leadstore.Queries, id int64) (string, error) {
	ms, err := tq.Mailboxes(ctx, startOfDay(leadsNow(), r.loc))
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
	if row.State != leadstore.MsgApproved || row.Shadow == r.live || row.MailboxID != mb.ID {
		r.rep.skipped++
		return false, nil
	}
	label := fmt.Sprintf("message %d", row.ID)

	// Suppression, the third point.
	set, err := r.lq.SuppressionSet(ctx)
	if err != nil {
		return false, err
	}
	if outreach.Suppressed(row.ToAddress, row.OrgDomain, outreach.NewSuppressionSet(set.Addresses, set.Domains)) {
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
	// Fixture names never get a live message (RA25).
	if r.live && !leadsMail.allowReserved &&
		(outreach.ReservedDomain(recipientDomain(row.ToAddress)) || outreach.ReservedDomain(row.OrgDomain)) {
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
	// From the message's own mailbox, under the From name frozen with it.
	from := row.FromAddress
	if from == "" {
		from = mb.Address
	}
	now := leadsNow()
	raw, err := mailbox.Build(outboxMessage(row.Message, cmpOr(row.FromName, r.setup.leads.Sender.Name), from, now))
	if err != nil {
		r.rep.refused = append(r.rep.refused, label+" (not a valid message)")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: "+err.Error())
	}
	var claimed bool
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
	domain := recipientDomain(row.ToAddress)
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
		return true, nil
	}

	reply, sendErr := mb.sender.Send(ctx, from, row.ToAddress, raw)
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
	case mailbox.IsPolicyBlock(sendErr) && errors.As(sendErr, &se):
		// Our domain, not this lead: the message waits, and every mailbox on the domain
		// pauses until the operator resumes it.
		r.rep.note(notePolicyBlock)
		if err := r.lq.Requeue(wctx, row.ID, *row.DueAt, se.Error()); err != nil {
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
		r.printf("Message %d stays approved and goes once its mailbox is resumed.\n", row.ID)
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
				_, err := tq.Suppress(wctx, leadstore.SuppressAddress, row.ToAddress, leadstore.ReasonBounce, "smtp")
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
	if err := r.scheduleNext(ctx, r.lq, row, sentAt); err != nil {
		r.rep.note(noteFollowup)
	}
}

// scheduleNext gives the sequence's next step its due time from this send: step 1
// followup_business_days[0] business days after step 0, step 2 [1] days after step 1.
func (r *sendRun) scheduleNext(ctx context.Context, tq leadstore.Queries, row leadstore.SendRow, sentAt time.Time) error {
	days := r.setup.leads.Schedule.FollowupBusinessDays
	if row.Step >= len(days) || row.Step >= 2 {
		return nil
	}
	due := outreach.FollowupDue(sentAt, days[row.Step], r.rowLoc(row.LeadKind, row.OrgCountry), r.setup.leads.Schedule.BusinessHours)
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
