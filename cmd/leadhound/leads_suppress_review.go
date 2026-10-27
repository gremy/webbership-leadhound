package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"os"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// liftReasonQuoted is the reason the review's lift lines carry.
const liftReasonQuoted = "reply quoted our opt-out line"

// reviewed is one recorded opt-out as the review re-read it: where (OptOutIn's answer) when
// it was re-read, else why it could not be.
type reviewed struct {
	ev       leadstore.OptOutEvent
	read     bool
	where    mailbox.OptOutWhere
	cannot   string // why it could not be re-read, when !read
	label    string // "your selftest" or its from_domain
	addrs    []string
	domains  []string
	stillOpt bool // re-read and still an opt-out (subject or own words)
}

// runSuppressReview is `leads suppress review` (FO6, FO8): it re-reads every recorded opt-out
// from its mailbox, classifies it again with today's rules (OptOutIn, our own lines out) and
// lists the ones that now read as replies, each with the exact lift line for every active
// suppression it wrote, unless another opt-out leans on that row. It lifts nothing, writes
// nothing (no run row, no event, no cursor move, no mailbox pause) and takes no lock: a sync
// running beside it is harmless. Addresses are always masked.
func runSuppressReview(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usageError(leadsSuppressUsage)
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	events, err := lq.OptOutEvents(ctx)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		_, _ = fmt.Fprintln(stdout, "No opt-outs to review.")
		return nil
	}
	key, err := secretbox.LoadPrivateKey(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "No review: the mailbox key is missing (%v); nothing was read.\n", err)
		return errors.New("leads suppress review: the mailbox key is missing")
	}
	boxes, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, leadsNow())
	if err != nil {
		return err
	}
	refs, err := lq.SentRefs(ctx)
	if err != nil {
		return err
	}
	revs := reviewEvents(ctx, events, refs, boxes, key, optOutLinesOf(setup.leads))
	printReview(revs)
	return nil
}

// reviewEvents re-reads every event, one mailbox login at a time, and fills in what each
// lead reaches (for FO8).
func reviewEvents(ctx context.Context, events []leadstore.OptOutEvent, refs []leadstore.SentRef,
	boxes []leadstore.MailboxStatus, key secretbox.PrivateKey, ownLines []string) []*reviewed {
	byLead := map[int64][]leadstore.SentRef{}
	for _, r := range refs {
		byLead[r.LeadID] = append(byLead[r.LeadID], r)
	}
	byBox := map[int64]leadstore.MailboxStatus{}
	for _, b := range boxes {
		byBox[b.ID] = b
	}
	revs := make([]*reviewed, len(events))
	groups := map[int64][]*reviewed{}
	for i, ev := range events {
		rv := &reviewed{ev: ev, label: ev.FromDomain}
		if rv.label == "" {
			rv.label = "an unknown sender"
		}
		rv.addrs, rv.domains = reach(ev, byLead)
		if ev.LeadID != nil && slices.ContainsFunc(byLead[*ev.LeadID], leadstore.SentRef.Selftest) {
			rv.label = "your selftest"
		}
		revs[i] = rv
		groups[ev.MailboxID] = append(groups[ev.MailboxID], rv)
	}
	for _, id := range slices.Sorted(maps.Keys(groups)) {
		rereadMailbox(ctx, byBox, id, key, ownLines, groups[id])
	}
	return revs
}

// reach is every address and domain an event's opt-out reaches (FO8): the address our
// message went to and the lead's other sent addresses; the sender's domain, the registrable
// domain of each of those addresses and the lead's org domain.
func reach(ev leadstore.OptOutEvent, byLead map[int64][]leadstore.SentRef) (addrs, domains []string) {
	addAddr := func(a string) {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || slices.Contains(addrs, a) {
			return
		}
		addrs = append(addrs, a)
	}
	addDomain := func(d string) {
		r, err := sitelist.Registrable(strings.ToLower(strings.TrimSpace(d)))
		if err != nil || slices.Contains(domains, r) {
			return
		}
		domains = append(domains, r)
	}
	addAddr(ev.ToAddress)
	addDomain(ev.FromDomain)
	if ev.LeadID != nil {
		for _, r := range byLead[*ev.LeadID] {
			addAddr(r.ToAddress)
			addDomain(r.OrgDomain)
		}
	}
	for _, a := range addrs {
		_, d, _ := strings.Cut(a, "@")
		addDomain(d)
	}
	return addrs, domains
}

// rereadMailbox re-reads the events of mailbox id: a gone, retired or password-less mailbox,
// or one whose password cannot be opened, leaves each event not re-read with the reason.
// Each folder is selected read-only; a changed UIDVALIDITY or a missing UID is a reason too.
// Each message's text is dropped as soon as it is classified.
func rereadMailbox(ctx context.Context, byBox map[int64]leadstore.MailboxStatus, id int64,
	key secretbox.PrivateKey, ownLines []string, evs []*reviewed) {
	fail := func(rs []*reviewed, why string) {
		for _, r := range rs {
			r.cannot = why
		}
	}
	mb, ok := byBox[id]
	switch {
	case !ok:
		fail(evs, fmt.Sprintf("mailbox %d is gone", id))
		return
	case mb.State == leadstore.MailboxRetired:
		fail(evs, fmt.Sprintf("mailbox %d is retired", id))
		return
	case len(mb.PasswordSealed) == 0:
		fail(evs, fmt.Sprintf("mailbox %d has no password", id))
		return
	}
	creds, why := openCreds(key, mb.Mailbox)
	if why != "" {
		fail(evs, fmt.Sprintf("mailbox %d: %s", id, why))
		return
	}
	r, err := leadsMail.readerFor(ctx, creds.IMAPAddr, config.Secret(creds.Login), creds.Password)
	if err != nil {
		fail(evs, "the mailbox could not be read: "+err.Error())
		return
	}
	defer func() { _ = r.Close() }()
	byFolder := map[string][]*reviewed{}
	for _, e := range evs {
		byFolder[e.ev.Folder] = append(byFolder[e.ev.Folder], e)
	}
	for _, folder := range slices.Sorted(maps.Keys(byFolder)) {
		rereadFolder(r, folder, ownLines, byFolder[folder])
	}
}

// rereadFolder selects folder (read-only, Reader.Select) and classifies each event's message.
func rereadFolder(r mailbox.Reader, folder string, ownLines []string, evs []*reviewed) {
	validity, err := r.Select(folder)
	if err != nil {
		for _, e := range evs {
			e.cannot = "the folder " + folder + " could not be opened: " + err.Error()
		}
		return
	}
	byUID := map[uint32]*reviewed{}
	var uids []uint32
	for _, e := range evs {
		if e.ev.UIDValidity != validity {
			e.cannot = "the folder was renumbered"
			continue
		}
		// Two events of one UID (a legacy source) both read the same message.
		if _, seen := byUID[e.ev.UID]; !seen {
			uids = append(uids, e.ev.UID)
		}
		byUID[e.ev.UID] = e
	}
	slices.Sort(uids)
	got := map[uint32]mailbox.OptOutWhere{}
	for start := 0; start < len(uids); start += fetchChunk {
		fetched, err := r.FetchHeaders(uids[start:min(start+fetchChunk, len(uids))])
		if err != nil {
			for _, e := range evs {
				if e.cannot == "" && slices.Contains(uids[start:], e.ev.UID) {
					e.cannot = "the folder " + folder + " could not be read: " + err.Error()
				}
			}
			return
		}
		for i := range fetched {
			got[fetched[i].UID] = classifyAgain(fetched[i], ownLines)
			fetched[i].FirstText, fetched[i].Header, fetched[i].Report = nil, nil, nil
		}
	}
	for _, e := range evs {
		if e.cannot != "" {
			continue
		}
		where, ok := got[e.ev.UID]
		if !ok {
			e.cannot = "the message is no longer in " + folder
			continue
		}
		e.read, e.where = true, where
		e.stillOpt = where == mailbox.OptOutSubject || where == mailbox.OptOutText
	}
}

// classifyAgain is OptOutIn over one fetched message, with our own lines taken out (FO3).
func classifyAgain(f mailbox.Fetched, ownLines []string) mailbox.OptOutWhere {
	h, err := mailbox.ParseHeader(f.Header, f.Report)
	if err != nil {
		h = mailbox.Header{Fields: mail.Header{}}
	}
	return mailbox.OptOutIn(h.Fields, f.FirstText, ownLines)
}

// covers reports whether event o's opt-out reaches suppression row s (FO8).
func covers(o *reviewed, s leadstore.SuppressionRow) bool {
	if s.Kind == leadstore.SuppressAddress {
		return slices.Contains(o.addrs, s.Value)
	}
	return slices.Contains(o.domains, s.Value)
}

// eventLine is "Event 7 · mailbox 1 INBOX uid 3 · your selftest · 2026-10-26".
func eventLine(r *reviewed) string {
	return fmt.Sprintf("Event %d · mailbox %d %s uid %d · %s · %s", r.ev.EventID, r.ev.MailboxID, r.ev.Folder, r.ev.UID,
		r.label, leadsDay(r.ev.SeenAt))
}

// printReview prints the review decision-first: the opt-outs that now read as replies, each
// with its lift lines (FO8 applied first), then the counts of the rest.
func printReview(revs []*reviewed) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(stdout, format, a...) }
	var replies, still, cannot []*reviewed
	for _, r := range revs {
		switch {
		case !r.read:
			cannot = append(cannot, r)
		case r.stillOpt:
			still = append(still, r)
		default:
			replies = append(replies, r)
		}
	}
	if len(replies) == 0 {
		p("No recorded opt-out reads as a reply now.\n")
	}
	for _, r := range replies {
		why := "the opt-out word was only in quoted text"
		if r.where == mailbox.OptOutNone {
			why = "no opt-out word in it now"
		}
		p("%s: now a reply (%s).\n", eventLine(r), why)
		if r.ev.Ambiguous {
			p("  ambiguous: re-check by hand\n")
			continue
		}
		if len(r.ev.Suppressions) == 0 {
			p("  No suppression of its own to lift.\n")
		}
		for _, s := range r.ev.Suppressions {
			if s.LiftedAt != nil {
				p("  #%d already lifted\n", s.ID)
				continue
			}
			blocked := false
			for _, o := range revs {
				if o == r || !covers(o, s) || (o.read && !o.stillOpt) {
					continue
				}
				what := "still an opt-out"
				if !o.read {
					what = "could not be re-read"
				}
				p("  #%d also covers event %d (%s): do not lift.\n", s.ID, o.ev.EventID, what)
				blocked = true
			}
			if !blocked {
				p("  To lift: leads suppress lift %d --domain %s --reason %q\n", s.ID, s.Domain(), liftReasonQuoted)
			}
		}
	}
	p("%d still read as opt-outs (subject or their own words): no change.\n", len(still))
	if len(cannot) == 0 {
		p("0 could not be re-read.\n")
	} else {
		p("%d could not be re-read:\n", len(cannot))
		for _, r := range cannot {
			p("  %s: can't re-read: %s\n", eventLine(r), r.cannot)
		}
	}
	p("Nothing was lifted: review and lift by hand.\n")
}
