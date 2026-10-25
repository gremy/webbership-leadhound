package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/outreach"
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
)

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
	for _, h := range []outreach.HoldReason{outreach.HoldCap, outreach.HoldHours, outreach.HoldDomain, outreach.HoldNotDue} {
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

// print writes the outcome decision-first: what Gremy must check, then what went.
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
	for _, h := range []outreach.HoldReason{outreach.HoldCap, outreach.HoldHours, outreach.HoldDomain} {
		if n := r.held[h]; n > 0 {
			held = append(held, fmt.Sprintf("%s %d", map[outreach.HoldReason]string{
				outreach.HoldCap: "today's cap", outreach.HoldHours: "outside business hours", outreach.HoldDomain: "one per domain per run",
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
	if len(r.sent)+len(r.refused)+len(r.maybe)+len(r.stuck)+r.retry+len(held) == 0 && r.cancelled == 0 {
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

// liveMissing names the variables a live send needs that are not set.
func liveMissing(setup leadsSetup) []string {
	var missing []string
	for _, v := range []struct {
		name string
		set  bool
	}{
		{"LEADHOUND_SMTP_ADDR", setup.env.SMTPAddr != ""},
		{"LEADHOUND_IMAP_ADDR", setup.env.IMAPAddr != ""},
		{"LEADHOUND_OUTREACH_USER (the mailbox address, local@domain)", validUser(setup.env)},
		{"LEADHOUND_OUTREACH_PASSWORD", setup.env.Password.Reveal() != ""},
	} {
		if !v.set {
			missing = append(missing, v.name)
		}
	}
	return missing
}

// validUser reports whether LEADHOUND_OUTREACH_USER is a bare local@domain address: the
// sending domain the DNS gate checks comes from it.
func validUser(env leadconfig.Env) bool {
	u := env.User.Reveal()
	if _, ok := sendingDomain(env); !ok {
		return false
	}
	a, err := mail.ParseAddress(u)
	return err == nil && a.Address == u && a.Name == ""
}

// startOfDay is midnight of now's day in loc.
func startOfDay(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// sendRun is one send (or selftest) after its command line is read: its setup, store,
// zone and report.
type sendRun struct {
	w      io.Writer
	s      *store.Store
	lq     leadstore.Queries
	setup  leadsSetup
	loc    *time.Location // LEADHOUND_TZ: the day the cap counts, and a selftest's hours
	live   bool
	from   string
	sender mailSender
	box    *mailbox.Outbox
	opts   sendOptions
	rep    *sendReport

	syncing  bool
	lastSync time.Time
}

// leadsSendRun runs one send in its fixed order (Task 5.3), any failure stopping it before
// SMTP: the send lock (RA3); the sending domain's DNS check passed today, else run now; a
// reply sync (fail-closed); Plan with today's cap; per message a re-sync when the last is
// over 10 minutes old, the checks, MarkSending, Send and its outcome; the spacing pause.
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
	if live {
		if missing := liveMissing(setup); len(missing) > 0 {
			return usageError("LEADHOUND_OUTREACH is live, so sending needs %s", strings.Join(missing, ", "))
		}
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
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
		w: w, s: s, lq: leadstore.NewQueries(s.Pool()), setup: setup, loc: loc, live: live, opts: opts,
		rep: &sendReport{live: live, held: map[outreach.HoldReason]int{}},
	}
	command, source := commandLeadsSend, sourceOutreachShadow
	if opts.selftestTo != "" {
		command = commandLeadsSelftest
	}
	if live {
		source = sourceOutreach
		r.from = setup.env.User.Reveal()
		r.sender = leadsMail.smtp(setup.env)
	} else {
		r.from = "outreach@" + outreach.MessageIDDomain(setup.env.User.Reveal())
		r.box = &mailbox.Outbox{Dir: filepath.Join(setup.dataDir, outboxDir), Location: loc, Now: leadsNow}
	}
	runID, err := s.Q().StartRun(ctx, command, source)
	if err != nil {
		return err
	}
	runErr := r.run(ctx)
	if err := s.Q().FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{Note: r.rep.runNote(runErr)}); err != nil && runErr == nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	r.rep.print(w)
	return nil
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

// runOrder is run's body; it ends with errPaused when the operator's pause stops it.
func (r *sendRun) runOrder(ctx context.Context) error {
	env := r.setup.env
	now := leadsNow()

	// 0. The operator's pause, before anything else (and before every message, sendOne).
	if err := r.checkPaused(ctx, r.lq); err != nil {
		return err
	}

	// 1. The sending domain's DNS, passed today.
	if domain, ok := sendingDomain(env); ok {
		sha := leadstore.SenderDomainSHA256(domain)
		passed, err := r.lq.SenderCheckPassedSince(ctx, sha, startOfDay(now, r.loc))
		if err != nil {
			return err
		}
		if !passed {
			res, err := checkSender(ctx, r.lq, domain, dkimSelector(r.setup.leads), now)
			if err != nil {
				return err
			}
			if !res.OK() {
				r.rep.note(noteDNS)
				r.printf("No sends: the sending domain's DNS check failed (%s). Fix the records, then run leadhound leads senders check.\n", dnsLine(res))
				return nil
			}
		}
	} else if r.live {
		// leadsSendRun refuses this before the run starts; never send without the DNS gate.
		return errors.New("leads send: live without a sending domain")
	} else {
		r.printf("Shadow without LEADHOUND_OUTREACH_USER: no sending domain, so no DNS check.\n")
	}

	// 2. The reply sync, fail-closed. Shadow without IMAP settings has nothing to read.
	r.syncing = r.live || imapReady(env)
	if r.syncing {
		if ok := r.sync(ctx, true); !ok {
			return nil
		}
	} else {
		r.printf("No IMAP settings: nothing synced; shadow sends go ahead (nothing can reply to a file).\n")
	}

	// A selftest's one message, approved on the command line.
	var only int64
	if r.opts.selftestTo != "" {
		st, err := r.createSelftest(ctx, now)
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

	// 3. Plan with today's cap: sent, maybe_sent and sending count, shadow_sent never does.
	rows, err := r.lq.SendRows(ctx, now, !r.live)
	if err != nil {
		return err
	}
	sentToday, err := r.lq.SentSince(ctx, startOfDay(now, r.loc))
	if err != nil {
		return err
	}
	// The ramp's day 0: a fixed date, or with start_date first-send the first live send.
	var firstSend *time.Time
	if r.setup.leads.Caps.StartDate.IsFirstSend() {
		if firstSend, err = r.lq.FirstLiveSend(ctx); err != nil {
			return err
		}
		if firstSend != nil {
			fs := firstSend.In(r.loc)
			firstSend = &fs
		}
	}
	capToday := outreach.CapOn(r.setup.leads, firstSend, now.In(r.loc))
	var items, plan []outreach.PlanItem
	for _, row := range rows {
		self := row.LeadKind == leadstore.LeadSelftest
		// A selftest goes in its own run only; an ordinary run never sends one.
		if (only != 0) != self || (only != 0 && row.ID != only) {
			continue
		}
		items = append(items, outreach.PlanItem{
			MessageID: row.ID, BatchID: row.BatchID, LeadID: row.LeadID, Step: row.Step,
			Domain: recipientDomain(row.ToAddress), Loc: r.rowLoc(row.LeadKind, row.OrgCountry),
			DueAt: *row.DueAt, Selftest: self,
		})
	}
	bh := r.setup.leads.Schedule.BusinessHours
	if only != 0 {
		// The operator's own address: no cap (it still counts) and no business hours.
		plan = items
		defer r.cancelUnsentSelftest(ctx, only)
	} else {
		var held map[outreach.HoldReason]int
		plan, held = outreach.Plan(items, sentToday, capToday, now, bh)
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

	// 4. Per message, with 5. the spacing between two sends.
	for i, it := range plan {
		if err := ctx.Err(); err != nil {
			r.rep.note(noteInterrupted)
			return nil
		}
		if i > 0 && r.syncing && leadsNow().Sub(r.lastSync) > resyncAfter {
			if ok := r.sync(ctx, false); !ok {
				return nil
			}
		}
		// Business hours at the moment of sending (RA8): the spacing may have carried this
		// recipient's zone past its window; other zones may still be open.
		if !it.Selftest && !outreach.InBusinessHours(it.Loc, bh, leadsNow()) {
			r.rep.held[outreach.HoldHours]++
			continue
		}
		attempted, err := r.sendOne(ctx, it.MessageID)
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

// sync runs one reply sync and reports whether sending may go on. Its findings are printed
// (the first always, a re-sync when it saw something).
func (r *sendRun) sync(ctx context.Context, first bool) bool {
	sum, err := syncMailbox(ctx, r.s.Pool(), r.setup.env, leadsMail.syncWait)
	switch {
	case errors.Is(err, errSyncRunning):
		r.rep.note(noteSyncRunning)
		r.printf("No further sends: a sync is still running after %s; run send again later.\n", leadsMail.syncWait)
		return false
	case err != nil:
		r.rep.note(noteSync)
		r.printf("No further sends: the reply sync failed (%v); nothing is sent until a sync succeeds.\n", err)
		return false
	}
	r.lastSync = leadsNow()
	if first || sum.Messages > 0 {
		sum.print(r.w)
	}
	return true
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

// sendOne checks message id once more and sends it. attempted reports whether SMTP (or the
// outbox) was reached. An error is a store failure, or an outbox or sender that cannot work;
// it stops the run.
func (r *sendRun) sendOne(ctx context.Context, id int64) (attempted bool, err error) {
	row, err := r.lq.SendRowByID(ctx, id)
	if err != nil {
		return false, err
	}
	// Claimed or cancelled since Plan (another sender, a sync), or the other kind of batch
	// (RA1: a shadow send never touches a live batch, nor a live send a shadow one).
	if row.State != leadstore.MsgApproved || row.Shadow == r.live {
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
	// What Gremy approved, byte for byte (RA2).
	if !bytes.Equal(leadstore.MessageSHA256(row.ToAddress, row.Subject, row.BodyText, row.InReplyTo), row.ApprovedSHA256) {
		r.rep.note(noteTampered)
		r.rep.refused = append(r.rep.refused, label+" ("+noteTampered+")")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: tampered (the message changed after approval)")
	}
	now := leadsNow()
	raw, err := mailbox.Build(outboxMessage(row.Message, r.setup.leads.Sender.Name, r.from, now))
	if err != nil {
		r.rep.refused = append(r.rep.refused, label+" (not a valid message)")
		return false, r.lq.MarkRefused(ctx, row.ID, "refused: "+err.Error())
	}
	var claimed bool
	err = leadsInTx(ctx, r.s.Pool(), func(tq leadstore.Queries) error {
		// The pause is read in the claim's transaction, the last moment before SMTP.
		if err := r.checkPaused(ctx, tq); err != nil {
			return err
		}
		var err error
		if claimed, err = tq.MarkSending(ctx, row.ID); err != nil || !claimed {
			return err
		}
		return tq.StampAttempt(ctx, row.ID, now)
	})
	if err != nil || !claimed {
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

	reply, sendErr := r.sender.Send(ctx, r.from, row.ToAddress, raw)
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
		r.appendSent(ctx, raw)
	case errors.As(sendErr, &se) && se.Kind == mailbox.Refused:
		r.rep.refused = append(r.rep.refused, label+" (refused by the server)")
		if err := leadsInTx(wctx, r.s.Pool(), func(tq leadstore.Queries) error {
			if err := tq.MarkRefused(wctx, row.ID, se.Error()); err != nil {
				return err
			}
			if se.Bounce {
				_, err := tq.Suppress(wctx, leadstore.SuppressAddress, row.ToAddress, leadstore.ReasonBounce, "smtp")
				return err
			}
			return nil
		}); err != nil {
			return true, err
		}
	case errors.As(sendErr, &se) && se.Kind == mailbox.Account:
		// The mailbox, not this message: back to approved, nothing suppressed, and the run
		// stops so the operator sees it.
		r.rep.note(noteAccount)
		if err := r.lq.Requeue(wctx, row.ID, *row.DueAt, se.Error()); err != nil {
			return true, err
		}
		r.printf("Stopped: the outreach mailbox account was refused at %s (%d). Nothing more is sent until it is fixed; message %d stays approved.\n",
			se.Stage, se.Code, row.ID)
		return true, fmt.Errorf("leads send: the outreach mailbox account was refused at %s (%d)", se.Stage, se.Code)
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

// appendSent files a copy of a sent message in the mailbox's \Sent folder, so replies thread
// in a mail client. A failure is a note only: the message has gone.
func (r *sendRun) appendSent(ctx context.Context, raw []byte) {
	rd, err := leadsMail.openReader(ctx, r.setup.env)
	if err != nil {
		r.rep.note(noteAppend)
		return
	}
	defer func() { _ = rd.Close() }()
	if err := rd.AppendSent(raw); err != nil {
		r.rep.note(noteAppend)
	}
}
