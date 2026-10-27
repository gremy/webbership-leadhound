package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/mail"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

func init() { leadsCommands["sync"] = runLeadsSync }

const (
	// rescanDays is how far back a mailbox is read by date when its cursor cannot be used.
	rescanDays = 30
	// fetchChunk is how many messages one FETCH asks for.
	fetchChunk = 50
)

// errSyncRunning: another sync holds the sync lock.
var errSyncRunning = errors.New("a sync is running")

// syncSummary is what one sync saw.
type syncSummary struct {
	Messages  int
	ByKind    map[mailbox.MessageKind]int
	ToRead    []string // the domains of matched replies and opt-outs, in arrival order
	Cancelled int64
	Junk      bool // the \Junk mailbox was read too
}

func (s *syncSummary) add(kind mailbox.MessageKind, domain string, a leadstore.Applied) {
	if a.Duplicate {
		return
	}
	s.Messages++
	s.ByKind[kind]++
	s.Cancelled += a.Cancelled
	if kind == mailbox.KindReply || kind == mailbox.KindOptOut {
		s.ToRead = append(s.ToRead, domain)
	}
}

// print writes the summary decision-first: what the operator has to read, then what was only
// counted.
func (s syncSummary) print(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	if n := len(s.ToRead); n > 0 {
		p("%s to read in the outreach mailbox: %s\n", plural(n, "reply", "replies"), strings.Join(s.ToRead, ", "))
	}
	if n := s.ByKind[mailbox.KindOptOut]; n > 0 {
		p("Of those, %s, now suppressed.\n", plural(n, "is an opt-out", "are opt-outs"))
	}
	if n := s.ByKind[mailbox.KindBounce]; n > 0 {
		p("%s: the address is suppressed.\n", plural(n, "bounce", "bounces"))
	}
	if n := s.ByKind[mailbox.KindAutoReply]; n > 0 {
		p("%s (recorded only).\n", plural(n, "automatic reply", "automatic replies"))
	}
	if n := s.ByKind[mailbox.KindUnmatched]; n > 0 {
		p("%s in the outreach mailbox.\n", plural(n, "other message", "other messages"))
	}
	if s.Cancelled > 0 {
		p("%s cancelled.\n", plural(int(s.Cancelled), "pending message", "pending messages"))
	}
	if s.Messages == 0 {
		p("Mailbox synced: nothing new.\n")
	}
}

// matcher resolves a classified message to the email of ours it concerns.
type matcher struct {
	byID     map[string]leadstore.SentRef
	byTo     map[string]leadstore.SentRef
	byDomain map[string]leadstore.SentRef // never a selftest: those match by Message-ID only (RA24)
	// ownLines are our own opt-out lines, taken out of a reply before its opt-out words are
	// matched (FO3); set by syncAll from leads.yaml.
	ownLines []string
}

func newMatcher(refs []leadstore.SentRef) matcher {
	m := matcher{byID: map[string]leadstore.SentRef{}, byTo: map[string]leadstore.SentRef{}, byDomain: map[string]leadstore.SentRef{}}
	for _, r := range refs { // oldest first, so the latest email wins each key
		if id := strings.Trim(r.MessageID, "<>"); id != "" {
			m.byID[id] = r
		}
		if r.ToAddress != "" {
			m.byTo[r.ToAddress] = r
		}
		if r.Selftest() {
			continue
		}
		_, toDomain, _ := strings.Cut(r.ToAddress, "@")
		for _, d := range []string{r.OrgDomain, toDomain} {
			if reg, err := sitelist.Registrable(d); err == nil {
				m.byDomain[reg] = r
			}
		}
	}
	return m
}

// optOutLinesOf (FO3) is every line of ours a reply may quote: the opt-out line of each
// language (sorted by language, so the order is fixed), then the selftest sentence and the
// one sent before it (replies to old selftests still quote it).
func optOutLinesOf(cfg leadconfig.Config) []string {
	var lines []string
	for _, lang := range slices.Sorted(maps.Keys(cfg.Sender.OptOutLines)) {
		if line, ok := cfg.Sender.OptOutLine(lang); ok {
			lines = append(lines, line)
		}
	}
	return append(lines, mailbox.SelftestLine, mailbox.OldSelftestLine)
}

func (m matcher) ourID(id string) bool         { _, ok := m.byID[id]; return ok }
func (m matcher) contacted(domain string) bool { _, ok := m.byDomain[domain]; return ok }

// resolve picks the email res concerns: by Message-ID, else a bounce by its final
// recipient, else a reply or opt-out by its sender's domain.
func (m matcher) resolve(res mailbox.Result) *leadstore.SentRef {
	if r, ok := m.byID[res.MatchedMessageID]; ok && res.MatchedMessageID != "" {
		return &r
	}
	switch res.Kind {
	case mailbox.KindBounce:
		if r, ok := m.byTo[res.FinalRecipient]; ok && res.FinalRecipient != "" {
			return &r
		}
	case mailbox.KindReply, mailbox.KindOptOut:
		if r, ok := m.byDomain[res.FromDomain]; ok && res.FromDomain != "" {
			return &r
		}
	}
	return nil
}

// lockWait takes advisory lock key, trying again every leadsMail.lockPoll (real time) for up to wait;
// ErrLockHeld when it is still held after that.
func lockWait(ctx context.Context, pool *pgxpool.Pool, key int64, wait time.Duration) (*leadstore.Lock, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := leadstore.TryLock(ctx, pool, key)
		if !errors.Is(err, leadstore.ErrLockHeld) || !time.Now().Before(deadline) {
			return l, err
		}
		if err := sleepCtx(ctx, min(leadsMail.lockPoll, time.Until(deadline))); err != nil {
			return nil, err
		}
	}
}

// folderOf says whose folder syncFolder reads: sending mailbox mailboxID, with the health
// rules of cfg.
type folderOf struct {
	mailboxID int64
	cfg       leadconfig.Config
}

// syncFolder reads folder name of a sending mailbox (a UIDVALIDITY change, or no cursor yet,
// rereads the last 30 days by date), classifies each message and records it with its
// effects in one transaction per message, moving the mailbox's cursor with it. A bounce
// matched to one of our messages marks that message a hard bounce and checks the health of
// the mailbox it went from.
func syncFolder(ctx context.Context, pool *pgxpool.Pool, r mailbox.Reader, of folderOf, name string, match matcher, sum *syncSummary) error {
	lq := leadstore.NewQueries(pool)
	validity, err := r.Select(name)
	if err != nil {
		return err
	}
	cur, _, err := lq.MailboxCursorFor(ctx, of.mailboxID, name)
	if err != nil {
		return err
	}
	now := leadsNow()
	var uids []uint32
	// A cursor at UID 0 was written by a rescan that found nothing: rescan again rather
	// than read the whole mailbox from its first message.
	if mailbox.NeedsRescan(cur, validity) || cur.LastUID == 0 {
		uids, err = r.SearchSinceDate(now.AddDate(0, 0, -rescanDays))
	} else {
		uids, err = r.SearchSince(cur.LastUID)
	}
	if err != nil {
		return err
	}
	slices.Sort(uids)
	for start := 0; start < len(uids); start += fetchChunk {
		got, err := r.FetchHeaders(uids[start:min(start+fetchChunk, len(uids))])
		if err != nil {
			return err
		}
		slices.SortFunc(got, func(a, b mailbox.Fetched) int { return int(a.UID) - int(b.UID) })
		for _, f := range got {
			in, domain := classifyFetched(f, match)
			in.Mailbox, in.UIDValidity, in.At = name, validity, leadsNow()
			var a leadstore.Applied
			hard := false
			if err := leadsInTx(ctx, pool, func(tq leadstore.Queries) error {
				var err error
				if a, err = tq.ApplyIncomingFor(ctx, of.mailboxID, in); err != nil {
					return err
				}
				if hard = in.Kind == mailbox.KindBounce && in.Matched != nil && !a.Duplicate; hard {
					return tq.MarkHardBounce(ctx, in.Matched.ID)
				}
				return nil
			}); err != nil {
				return err
			}
			if hard && in.Matched.MailboxID != 0 {
				if _, err := checkBounceHealth(ctx, lq, of.cfg, in.Matched.MailboxID); err != nil {
					return err
				}
			}
			sum.add(in.Kind, domain, a)
		}
	}
	last := uint32(0)
	if cur.UIDValidity == validity {
		last = cur.LastUID
	}
	// Records the sync time; a mailbox with no new mail keeps its place.
	return lq.AdvanceCursorFor(ctx, of.mailboxID, name, validity, last, leadsNow())
}

// mailboxSync is how one sending mailbox's sync went.
type mailboxSync struct {
	OK      bool   // every folder was read to the end; when false, its sending waits for a good sync
	Note    string // why it is not OK, in plain words; never a password or an address
	Summary syncSummary
}

// syncAll reads every sending mailbox that is not retired (a paused one still syncs: replies
// keep coming) under the sync lock, taken once and waited for up to wait (errSyncRunning
// after that). Each mailbox logs in with its own sealed password, opened with key; one
// mailbox's failure is its own entry (not OK, with a note) and never stops the others.
// Matching is global: a reply in one mailbox stops a lead written to from another.
func syncAll(ctx context.Context, pool *pgxpool.Pool, key secretbox.PrivateKey, cfg leadconfig.Config, wait time.Duration) (map[int64]mailboxSync, error) {
	lock, err := lockWait(ctx, pool, leadstore.SyncLockKey, wait)
	if errors.Is(err, leadstore.ErrLockHeld) {
		return nil, errSyncRunning
	}
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	lq := leadstore.NewQueries(pool)
	boxes, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, leadsNow())
	if err != nil {
		return nil, fmt.Errorf("leads sync: %w", err)
	}
	refs, err := lq.SentRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("leads sync: %w", err)
	}
	match := newMatcher(refs)
	match.ownLines = optOutLinesOf(cfg)
	out := map[int64]mailboxSync{}
	for _, mb := range boxes {
		if mb.State == leadstore.MailboxRetired {
			continue
		}
		out[mb.ID] = syncSendingMailbox(ctx, pool, key, cfg, mb.Mailbox, match)
		if err := ctx.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// syncSendingMailbox reads INBOX and the \Junk mailbox of mb, each from its cursor.
func syncSendingMailbox(ctx context.Context, pool *pgxpool.Pool, key secretbox.PrivateKey, cfg leadconfig.Config, mb leadstore.Mailbox, match matcher) mailboxSync {
	res := mailboxSync{Summary: syncSummary{ByKind: map[mailbox.MessageKind]int{}}}
	creds, err := mailboxCreds(ctx, leadstore.NewQueries(pool), key, mb)
	if err != nil {
		res.Note = err.Error()
		return res
	}
	r, err := leadsMail.readerFor(ctx, creds.IMAPAddr, config.Secret(creds.Login), creds.Password)
	if err != nil {
		res.Note = "the mailbox could not be read: " + err.Error()
		return res
	}
	defer func() { _ = r.Close() }()
	boxes, err := r.ListMailboxes()
	if err != nil {
		res.Note = "the mailbox's folders could not be listed: " + err.Error()
		return res
	}
	names := []string{cmpOr(boxes.Inbox, "INBOX")}
	if boxes.Junk != "" && boxes.Junk != names[0] {
		names = append(names, boxes.Junk)
		res.Summary.Junk = true
	}
	for _, name := range names {
		if err := syncFolder(ctx, pool, r, folderOf{mailboxID: mb.ID, cfg: cfg}, name, match, &res.Summary); err != nil {
			res.Note = "the mailbox's sync stopped part way: " + err.Error()
			return res
		}
	}
	res.OK = true
	return res
}

// checkBounceHealth pauses mailbox mailboxID (actor system) when health.bounce_pause.bounces
// hard bounces fall within its last health.bounce_pause.within_sends live sends. paused
// reports whether the mailbox is paused after the check: newly by this rule, or already
// (then no second state row is written). A retired mailbox is never paused. Only the
// operator resumes a mailbox.
func checkBounceHealth(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, mailboxID int64) (paused bool, err error) {
	bp := cfg.Health.BouncePause
	bounces := min(max(cmp.Or(bp.Bounces, leadconfig.DefaultBouncePauseBounces), leadconfig.MinBouncePauseBounces), leadconfig.MaxBouncePauseBounces)
	within := min(max(cmp.Or(bp.WithinSends, leadconfig.DefaultBouncePauseWithinSends), leadconfig.MinBouncePauseWithinSends), leadconfig.MaxBouncePauseWithinSends)
	n, err := lq.HardBouncesInLastSends(ctx, mailboxID, within)
	if err != nil {
		return false, err
	}
	if n < bounces {
		return false, nil
	}
	all, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, leadsNow())
	if err != nil {
		return false, err
	}
	for _, mb := range all {
		if mb.ID != mailboxID {
			continue
		}
		switch mb.State {
		case leadstore.MailboxPaused:
			return true, nil
		case leadstore.MailboxRetired:
			return false, nil
		}
	}
	reason := fmt.Sprintf("%d hard bounces in its last %d live sends", n, within)
	err = lq.SetMailboxState(ctx, mailboxID, leadstore.MailboxPaused, reason, leadstore.ScopeMailbox, leadstore.ActorSystem)
	if errors.Is(err, leadstore.ErrMailboxRetired) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// classifyFetched classifies one peeked message and drops its text; it returns the record
// and the domain to name when it is to be read (the lead's own domain when matched).
func classifyFetched(f mailbox.Fetched, match matcher) (leadstore.Incoming, string) {
	in := leadstore.Incoming{UID: f.UID, Kind: mailbox.KindUnmatched}
	h, err := mailbox.ParseHeader(f.Header, f.Report)
	if err != nil {
		h = mailbox.Header{Fields: mail.Header{}}
	}
	res := mailbox.Classify(h, f.FirstText, match.ownLines, match.ourID, match.contacted)
	in.Kind, in.FromDomain = res.Kind, res.FromDomain
	in.Matched = match.resolve(res)
	if in.Matched == nil && res.Kind != mailbox.KindAutoReply {
		// Nothing of ours: a bounce or an answer we cannot place is just another message.
		in.Kind = mailbox.KindUnmatched
	}
	domain := res.FromDomain
	if in.Matched != nil {
		domain = cmpOr(in.Matched.OrgDomain, domain)
		if in.Matched.Selftest() {
			domain = "your selftest"
		}
	}
	return in, domain
}

// runLeadsSync is `leads sync`: it reads every sending mailbox that is not retired, each with
// its own sealed password, applies what came in and prints one line per mailbox, its findings
// under it. A sync already running makes it exit 0 with that said; with no mailbox it syncs
// nothing and says so (no key needed). A missing key, or a mailbox that could not be read
// to the end, fails the command once every other mailbox is synced.
func runLeadsSync(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usageError("usage: leadhound leads sync")
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	boxes, err := leadstore.NewQueries(s.Pool()).Mailboxes(ctx, leadstore.DefaultAccountID, leadsNow())
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(boxes, func(mb leadstore.MailboxStatus) bool { return mb.State != leadstore.MailboxRetired }) {
		_, _ = fmt.Fprintln(stdout, "No mailbox to sync: add one in the console (Mailboxes) or with leads mailboxes add.")
		return nil
	}
	key, err := secretbox.LoadPrivateKey(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "No sync: the mailbox key is missing (%v); nothing was read.\n", err)
		return errors.New("leads sync: the mailbox key is missing")
	}
	res, err := syncAll(ctx, s.Pool(), key, setup.leads, 0)
	switch {
	case errors.Is(err, errSyncRunning):
		_, _ = fmt.Fprintln(stdout, "A sync is running; this one did nothing.")
		return nil
	case err != nil:
		return err
	}
	ids := slices.Sorted(maps.Keys(res))
	failed := 0
	for _, id := range ids {
		ms := res[id]
		switch {
		case !ms.OK:
			failed++
			_, _ = fmt.Fprintf(stdout, "Mailbox %d: not synced: %s\n", id, ms.Note)
		case ms.Summary.Messages == 0:
			_, _ = fmt.Fprintf(stdout, "Mailbox %d: synced, nothing new.\n", id)
		default:
			_, _ = fmt.Fprintf(stdout, "Mailbox %d: synced.\n", id)
			var b strings.Builder
			ms.Summary.print(&b)
			for line := range strings.Lines(b.String()) {
				_, _ = fmt.Fprint(stdout, "  "+line)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("leads sync: %s not synced", plural(failed, "mailbox", "mailboxes"))
	}
	return nil
}
