package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

func init() { leadsCommands["sync"] = runLeadsSync }

const (
	// rescanDays is how far back a mailbox is read by date when its cursor cannot be used.
	rescanDays = 30
	// fetchChunk is how many messages one FETCH asks for.
	fetchChunk = 50
)

var (
	// errSyncRunning: another sync holds the sync lock.
	errSyncRunning = errors.New("a sync is running")
	// errNoIMAP: the IMAP variables are not all set, so there is no mailbox to read.
	errNoIMAP = errors.New("no IMAP settings")
)

// imapReady reports whether the IMAP variables a sync needs are all set.
func imapReady(env leadconfig.Env) bool {
	return env.IMAPAddr != "" && env.User.Reveal() != "" && env.Password.Reveal() != ""
}

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

// print writes the summary decision-first: what Gremy has to read, then what was only
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

// syncMailbox reads INBOX and the \Junk mailbox from their cursors (a UIDVALIDITY change,
// or no cursor yet, rereads the last 30 days by date), classifies each message and records
// it with its effects in one transaction per message, moving the cursor with it. It holds
// the sync lock throughout, waiting up to wait for it (errSyncRunning after that).
func syncMailbox(ctx context.Context, pool *pgxpool.Pool, env leadconfig.Env, wait time.Duration) (syncSummary, error) {
	sum := syncSummary{ByKind: map[mailbox.MessageKind]int{}}
	if !imapReady(env) {
		return sum, errNoIMAP
	}
	lock, err := lockWait(ctx, pool, leadstore.SyncLockKey, wait)
	if errors.Is(err, leadstore.ErrLockHeld) {
		return sum, errSyncRunning
	}
	if err != nil {
		return sum, err
	}
	defer lock.Release()

	lq := leadstore.NewQueries(pool)
	refs, err := lq.SentRefs(ctx)
	if err != nil {
		return sum, err
	}
	match := newMatcher(refs)
	r, err := leadsMail.openReader(ctx, env)
	if err != nil {
		return sum, fmt.Errorf("leads sync: %w", err)
	}
	defer func() { _ = r.Close() }()
	boxes, err := r.ListMailboxes()
	if err != nil {
		return sum, fmt.Errorf("leads sync: %w", err)
	}
	names := []string{cmpOr(boxes.Inbox, "INBOX")}
	if boxes.Junk != "" && boxes.Junk != names[0] {
		names = append(names, boxes.Junk)
		sum.Junk = true
	}
	for _, name := range names {
		if err := syncOne(ctx, pool, r, name, match, &sum); err != nil {
			return sum, fmt.Errorf("leads sync: %w", err)
		}
	}
	return sum, nil
}

// syncOne reads one mailbox.
func syncOne(ctx context.Context, pool *pgxpool.Pool, r mailbox.Reader, name string, match matcher, sum *syncSummary) error {
	lq := leadstore.NewQueries(pool)
	validity, err := r.Select(name)
	if err != nil {
		return err
	}
	cur, _, err := lq.MailboxCursor(ctx, name)
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
			if err := leadsInTx(ctx, pool, func(tq leadstore.Queries) error {
				var err error
				a, err = tq.ApplyIncoming(ctx, in)
				return err
			}); err != nil {
				return err
			}
			sum.add(in.Kind, domain, a)
		}
	}
	last := uint32(0)
	if cur.UIDValidity == validity {
		last = cur.LastUID
	}
	// Records the sync time; a mailbox with no new mail keeps its place.
	return lq.AdvanceCursor(ctx, name, validity, last, leadsNow())
}

// classifyFetched classifies one peeked message and drops its text; it returns the record
// and the domain to name when it is to be read (the lead's own domain when matched).
func classifyFetched(f mailbox.Fetched, match matcher) (leadstore.Incoming, string) {
	in := leadstore.Incoming{UID: f.UID, Kind: mailbox.KindUnmatched}
	h, err := mailbox.ParseHeader(f.Header, f.Report)
	if err != nil {
		h = mailbox.Header{Fields: mail.Header{}}
	}
	res := mailbox.Classify(h, f.FirstText, match.ourID, match.contacted)
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

// runLeadsSync is `leads sync`: it reads the outreach mailbox and applies what came in. A
// sync already running makes it exit 0 with that said; with no IMAP settings it syncs
// nothing and says so (shadow sends need none).
func runLeadsSync(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usageError("usage: leadhound leads sync")
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	sum, err := syncMailbox(ctx, s.Pool(), setup.env, 0)
	switch {
	case errors.Is(err, errNoIMAP):
		_, _ = fmt.Fprintln(stdout, "No IMAP settings (LEADHOUND_IMAP_ADDR, LEADHOUND_OUTREACH_USER, LEADHOUND_OUTREACH_PASSWORD): nothing synced.")
		return nil
	case errors.Is(err, errSyncRunning):
		_, _ = fmt.Fprintln(stdout, "A sync is running; this one did nothing.")
		return nil
	case err != nil:
		return err
	}
	sum.print(stdout)
	return nil
}
