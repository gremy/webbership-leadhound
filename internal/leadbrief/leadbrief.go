// Package leadbrief reads the leads section of the morning brief: what is waiting on the
// member (replies to read, a batch to approve, manual items, emails to check by hand) and
// the outreach health of yesterday. Its types are plain values the brief renders; it never
// carries an address or a person's name, only registrable domains and counts.
//
// Fixture data stays out (RA25): every count leaves out selftest orgs and leads, and orgs,
// sites and senders on a reserved TLD (leadstore.ReservedTLDs).
package leadbrief

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Section is the brief's leads block.
type Section struct {
	// Replies are the registrable domains of the replies and opt-outs to read (RA5) seen
	// since the start of yesterday, in arrival order, each once.
	Replies []string
	// Other counts the unmatched messages in the outreach mailbox since yesterday (RA9).
	Other int
	// Batch is the newest batch awaiting approval, nil when there is none.
	Batch *BatchReady
	// ManualWaiting counts the approved contact forms and LinkedIn notes of live batches not
	// yet marked done; a shadow batch's are never real outreach.
	ManualWaiting int
	// Sent and ShadowSent count yesterday's live and shadow emails; Bounced and OptedOut
	// yesterday's bounces and opt-outs. Yesterday is read in the brief's location.
	Sent, ShadowSent, Bounced, OptedOut int
	// Paused says why sending stopped on the latest send run since yesterday, or "".
	Paused string
	// OperatorPaused is when the operator paused sending, in the brief's location, nil while
	// sending is not paused. The pause's reason stays out: it is free text.
	OperatorPaused *time.Time
	// CheckByHand counts every live email that may have gone out, whatever its date:
	// maybe_sent, and those left in sending by a run that broke off. Nothing resolves
	// them yet, so the count never lapses.
	CheckByHand int
	// Probe is yesterday's site probe numbers, nil when nothing was probed.
	Probe *ProbeLine
	// Mailboxes is the sending mailboxes' health, for its one line (MailboxHealth.Line).
	Mailboxes MailboxHealth
}

// MailboxHealth counts the sending mailboxes that are not retired, by readiness, as of the
// start of the brief's day; mailboxes on a reserved TLD stay out.
type MailboxHealth struct {
	// Ready may be given new leads: active, its latest check passed, and its host and
	// selector passed today's DNS check. Paused are paused, by the operator or the system;
	// NeedsCheck are active but not ready.
	Ready, Paused, NeedsCheck int
	// PausedWhy are the paused ones' reasons in fixed words (pausedWhy), each once, in
	// mailbox order. The operator's own words never reach it: they are free text.
	PausedWhy []string
	// KeyMissing says the latest send run since yesterday found no mailbox key and sent
	// nothing.
	KeyMissing bool
}

// Line is the brief's mailbox line, decision-first: "Mailboxes: 2 ready · 1 paused (2 hard
// bounces) · 1 needs a check".
func (m MailboxHealth) Line() string {
	if m.KeyMissing {
		return "Mailboxes: the mailbox key is missing, nothing was sent"
	}
	if m.Ready+m.Paused+m.NeedsCheck == 0 {
		return "Mailboxes: none yet — add one in the console (Mailboxes)"
	}
	parts := []string{fmt.Sprintf("%d ready", m.Ready)}
	if m.Paused > 0 {
		p := fmt.Sprintf("%d paused", m.Paused)
		if len(m.PausedWhy) > 0 {
			p += " (" + strings.Join(m.PausedWhy, ", ") + ")"
		}
		parts = append(parts, p)
	}
	switch {
	case m.NeedsCheck == 1:
		parts = append(parts, "1 needs a check")
	case m.NeedsCheck > 1:
		parts = append(parts, fmt.Sprintf("%d need a check", m.NeedsCheck))
	}
	return "Mailboxes: " + strings.Join(parts, " · ")
}

// bouncesReason is the reason the bounce health rule writes (cmd/leadhound's
// checkBounceHealth); its count is the one number a pause shows.
var bouncesReason = regexp.MustCompile(`^(\d+) hard bounces in its last \d+ live sends$`)

// pausedWhy is a paused mailbox's reason in fixed words. The system's reasons have known
// shapes (cmd/leadhound's checkBounceHealth, policyReason, mailboxCreds); anything else is
// the operator's own text, which never reaches the brief.
func pausedWhy(scope, reason string) string {
	switch {
	case bouncesReason.MatchString(reason):
		return bouncesReason.FindStringSubmatch(reason)[1] + " hard bounces"
	case strings.HasPrefix(reason, "a receiving server blocked mail from "):
		return "blocked by a receiving server"
	case strings.HasPrefix(reason, "re-enter the password"):
		return "password to re-enter"
	case scope == leadstore.ScopeDomain:
		return "its domain paused by the operator"
	default:
		return "by the operator"
	}
}

// BatchReady is a batch waiting for approval, counted by item channel: first emails (a
// follow-up is not an item), contact forms and LinkedIn notes.
type BatchReady struct {
	ID                      int64
	Shadow                  bool // a shadow batch: nothing it holds is ever sent
	Emails, Forms, LinkedIn int
}

// ProbeLine is one day's probe numbers: sites checked, those on Drupal, and those by
// Drupal major (0 = major unknown).
type ProbeLine struct {
	Checked, Drupal int
	ByMajor         map[int]int
}

// Todo reports whether the section has anything for the member to act on; when it has
// not, the brief shows only its health line. A missing mailbox key stops sending as a DNS
// stop does, so it counts; the other mailbox counts are health.
func (s Section) Todo() bool {
	return len(s.Replies) > 0 || s.Other > 0 || s.Paused != "" || s.OperatorPaused != nil || s.Batch != nil ||
		s.ManualWaiting > 0 || s.CheckByHand > 0 || s.Mailboxes.KeyMissing
}

// The Paused values.
const (
	PausedSync    = "sync failed"
	PausedAccount = "smtp account refused"
	PausedDNS     = "dns check failed" // no sender check on file names the failing record
)

// sendCommand is the run command the send writes (cmd/leadhound's commandLeadsSend); a
// selftest run has its own command and never counts here.
const sendCommand = "leads-send"

// The send run's note words that stop sending (cmd/leadhound's noteDNS, noteSync,
// noteAccount), and the one that says the key was missing (noteKeyMissing).
const (
	noteDNS        = "dns"
	noteSync       = "sync"
	noteAccount    = "smtp-account"
	noteKeyMissing = "mailbox key missing"
)

// Load reads the section for the brief of day, yesterday being the calendar day before it
// in loc. Orgs, sites and senders on leadstore.ReservedTLDs are left out.
func Load(ctx context.Context, q store.DBTX, day time.Time, loc *time.Location) (Section, error) {
	return loadExcluding(ctx, q, day, loc, leadstore.ReservedTLDs)
}

// loadExcluding is Load with reserved as the TLDs left out; an empty list means
// leadstore.ReservedTLDs. Only tests pass another list (export_test.go), to see fixture
// rows counted at all, since fixture data lives on reserved TLDs only.
func loadExcluding(ctx context.Context, q store.DBTX, day time.Time, loc *time.Location, reserved []string) (Section, error) {
	if len(reserved) == 0 {
		reserved = leadstore.ReservedTLDs
	}
	re := `\.(` + strings.Join(reserved, "|") + `)$`
	y, m, d := day.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)
	yesterday, tomorrow := today.AddDate(0, 0, -1), today.AddDate(0, 0, 1)

	var (
		s     Section
		note  string
		boxes []leadstore.MailboxStatus
	)
	steps := []func() error{
		func() error { return s.loadReplies(ctx, q, re, yesterday, tomorrow) },
		func() error { return s.loadOther(ctx, q, re, yesterday, tomorrow) },
		func() error { return s.loadBatch(ctx, q, re) },
		func() error { return s.loadOutreach(ctx, q, re, yesterday, today) },
		func() (err error) { note, err = sendNote(ctx, q, yesterday); return err },
		func() (err error) { boxes, err = keptMailboxes(ctx, q, re, today); return err },
		func() error { return s.loadPaused(ctx, q, note, boxes) },
		func() error { s.loadMailboxes(note, boxes); return nil },
		func() error { return s.loadOperatorPaused(ctx, q, loc) },
		func() error { return s.loadProbe(ctx, q, re, yesterday, today) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return Section{}, fmt.Errorf("leadbrief: %w", err)
		}
	}
	return s, nil
}

// keptLead is the SQL condition on lead l and its org o that keeps fixture data out: no
// selftest, no org on a reserved TLD ($1 is the reserved-TLD expression).
const keptLead = `l.kind <> 'selftest' AND o.kind <> 'selftest' AND o.domain !~ $1`

func (s *Section) loadReplies(ctx context.Context, q store.DBTX, re string, from, to time.Time) error {
	rows, err := q.Query(ctx,
		`SELECT o.domain FROM mailbox_event e
		 JOIN lead l ON l.id = e.lead_id JOIN org o ON o.id = l.org_id
		 WHERE e.kind IN ('reply', 'opt_out') AND e.seen_at >= $2 AND e.seen_at < $3 AND `+keptLead+`
		 GROUP BY o.domain ORDER BY min(e.seen_at), o.domain`, re, from, to)
	if err != nil {
		return fmt.Errorf("replies: %w", err)
	}
	s.Replies, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("replies: %w", err)
	}
	return nil
}

func (s *Section) loadOther(ctx context.Context, q store.DBTX, re string, from, to time.Time) error {
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM mailbox_event e
		 WHERE e.kind = 'unmatched' AND e.seen_at >= $2 AND e.seen_at < $3 AND e.from_domain !~ $1`,
		re, from, to).Scan(&s.Other); err != nil {
		return fmt.Errorf("other messages: %w", err)
	}
	return nil
}

func (s *Section) loadBatch(ctx context.Context, q store.DBTX, re string) error {
	var b BatchReady
	err := q.QueryRow(ctx,
		`SELECT b.id, b.shadow,
		   count(*) FILTER (WHERE m.channel = 'email' AND m.step = 0),
		   count(*) FILTER (WHERE m.channel = 'form'),
		   count(*) FILTER (WHERE m.channel = 'linkedin')
		 FROM outreach_batch b JOIN outreach_message m ON m.batch_id = b.id
		 JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 WHERE b.approved_at IS NULL AND m.state = 'draft' AND `+keptLead+`
		 GROUP BY b.id, b.shadow
		 HAVING count(*) FILTER (WHERE m.channel IN ('form', 'linkedin') OR m.step = 0) > 0
		 ORDER BY b.id DESC LIMIT 1`, re).Scan(&b.ID, &b.Shadow, &b.Emails, &b.Forms, &b.LinkedIn)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("batch: %w", err)
	}
	s.Batch = &b
	return nil
}

// loadOutreach counts the manual items waiting, yesterday's sends, bounces and opt-outs,
// and the emails to check by hand.
func (s *Section) loadOutreach(ctx context.Context, q store.DBTX, re string, from, to time.Time) error {
	if err := q.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE NOT b.shadow AND b.approved_at IS NOT NULL AND m.state = 'approved' AND m.channel IN ('form', 'linkedin')),
		   count(*) FILTER (WHERE NOT b.shadow AND m.state = 'sent' AND m.sent_at >= $2 AND m.sent_at < $3),
		   count(*) FILTER (WHERE b.shadow AND m.state = 'shadow_sent' AND m.sent_at >= $2 AND m.sent_at < $3),
		   count(*) FILTER (WHERE NOT b.shadow AND m.state IN ('maybe_sent', 'sending'))
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 WHERE `+keptLead, re, from, to).Scan(&s.ManualWaiting, &s.Sent, &s.ShadowSent, &s.CheckByHand); err != nil {
		return fmt.Errorf("outreach: %w", err)
	}
	if err := q.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE e.kind = 'bounce'), count(*) FILTER (WHERE e.kind = 'opt_out')
		 FROM mailbox_event e JOIN lead l ON l.id = e.lead_id JOIN org o ON o.id = l.org_id
		 WHERE e.kind IN ('bounce', 'opt_out') AND e.seen_at >= $2 AND e.seen_at < $3 AND `+keptLead,
		re, from, to).Scan(&s.Bounced, &s.OptedOut); err != nil {
		return fmt.Errorf("bounces: %w", err)
	}
	return nil
}

// sendNote is the note of the latest finished send run since from, "" when none.
func sendNote(ctx context.Context, q store.DBTX, from time.Time) (string, error) {
	var note string
	err := q.QueryRow(ctx,
		`SELECT note FROM run WHERE command = $1 AND finished_at IS NOT NULL AND started_at >= $2
		 ORDER BY id DESC LIMIT 1`, sendCommand, from).Scan(&note)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("send run: %w", err)
	}
	return note, nil
}

// keptMailboxes are the mailboxes not retired, by id, as of since (the start of the brief's
// day), leaving out those on a reserved TLD (re, a regular expression Go and Postgres read
// alike).
func keptMailboxes(ctx context.Context, q store.DBTX, re string, since time.Time) ([]leadstore.MailboxStatus, error) {
	all, err := leadstore.NewQueries(q).Mailboxes(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("mailboxes: %w", err)
	}
	reserved, err := regexp.Compile(re)
	if err != nil {
		return nil, fmt.Errorf("mailboxes: %w", err)
	}
	var out []leadstore.MailboxStatus
	for _, m := range all {
		if m.State != leadstore.MailboxRetired && !reserved.MatchString(m.Domain) && !reserved.MatchString(m.Host()) {
			out = append(out, m)
		}
	}
	return out, nil
}

// loadMailboxes counts boxes by readiness and reads the key-missing note of the send run.
func (s *Section) loadMailboxes(note string, boxes []leadstore.MailboxStatus) {
	h := MailboxHealth{KeyMissing: NoteWords(note)[noteKeyMissing]}
	for _, m := range boxes {
		switch {
		case m.State == leadstore.MailboxPaused:
			h.Paused++
			if why := pausedWhy(m.Scope, m.Reason); !slices.Contains(h.PausedWhy, why) {
				h.PausedWhy = append(h.PausedWhy, why)
			}
		case m.LastCheck != nil && m.LastCheck.Outcome == leadstore.CheckPassed && m.DNSPassedToday:
			h.Ready++
		default:
			h.NeedsCheck++
		}
	}
	s.Mailboxes = h
}

// loadPaused reads why the send run's note says sending stopped, if it did. A DNS stop
// names, per host and DKIM selector of boxes, the records its latest sender check found
// missing; a sender check that has passed since clears that one.
func (s *Section) loadPaused(ctx context.Context, q store.DBTX, note string, boxes []leadstore.MailboxStatus) error {
	words := NoteWords(note)
	switch {
	case words[noteDNS]:
		p, err := dnsPaused(ctx, q, boxes)
		if err != nil {
			return err
		}
		s.Paused = p
	case words[noteSync]:
		s.Paused = PausedSync
	case words[noteAccount]:
		s.Paused = PausedAccount
	}
	return nil
}

// dnsPaused is the DNS Paused value: one result per host and selector of boxes, from its
// latest sender check ("dns: mail.alpha.example dmarc missing; beta.example not checked
// yet"); a host with more than one selector names it ("beta.example (s2) dkim missing").
// When no host has a check on file it is PausedDNS; when every one passed, "".
func dnsPaused(ctx context.Context, q store.DBTX, boxes []leadstore.MailboxStatus) (string, error) {
	type pair struct{ host, selector string }
	var pairs []pair
	selectors := map[string]int{}
	for _, m := range boxes {
		p := pair{m.Host(), strings.TrimSpace(m.DKIMSelector)}
		if !slices.Contains(pairs, p) {
			pairs = append(pairs, p)
			selectors[p.host]++
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(cmp.Compare(a.host, b.host), cmp.Compare(a.selector, b.selector))
	})
	var results []string
	checked := false
	for _, p := range pairs {
		name := p.host
		if selectors[p.host] > 1 {
			name += " (" + p.selector + ")"
		}
		var spf, dkim, dmarc bool
		err := q.QueryRow(ctx,
			`SELECT spf, dkim, dmarc FROM sender_check WHERE domain_sha256 = $1 AND selector = $2 AND selector <> ''
			 ORDER BY checked_at DESC, id DESC LIMIT 1`, leadstore.SenderDomainSHA256(p.host), p.selector).Scan(&spf, &dkim, &dmarc)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			results = append(results, name+" not checked yet")
		case err != nil:
			return "", fmt.Errorf("sender check: %w", err)
		default:
			checked = true
			if r := DNSPaused(spf, dkim, dmarc); r != "" {
				results = append(results, name+" "+strings.TrimPrefix(r, "dns: "))
			}
		}
	}
	switch {
	case !checked:
		return PausedDNS, nil
	case len(results) == 0:
		return "", nil
	}
	return "dns: " + strings.Join(results, "; "), nil
}

// loadOperatorPaused reads the operator's pause switch (leadstore.SendPaused).
func (s *Section) loadOperatorPaused(ctx context.Context, q store.DBTX, loc *time.Location) error {
	paused, _, at, err := leadstore.NewQueries(q).SendPaused(ctx)
	if err != nil {
		return fmt.Errorf("send pause: %w", err)
	}
	if paused {
		at = at.In(loc)
		s.OperatorPaused = &at
	}
	return nil
}

// NoteWords are the words of a send run note's "notes a, b" part.
func NoteWords(note string) map[string]bool {
	out := map[string]bool{}
	for part := range strings.SplitSeq(note, " · ") {
		list, ok := strings.CutPrefix(part, "notes ")
		if !ok {
			continue
		}
		for w := range strings.SplitSeq(list, ", ") {
			if w = strings.TrimSpace(w); w != "" {
				out[w] = true
			}
		}
	}
	return out
}

// DNSPaused is the Paused value for a sender check's results: "dns: dmarc missing", or ""
// when every record passed.
func DNSPaused(spf, dkim, dmarc bool) string {
	var missing []string
	for _, r := range []struct {
		name string
		ok   bool
	}{{"spf", spf}, {"dkim", dkim}, {"dmarc", dmarc}} {
		if !r.ok {
			missing = append(missing, r.name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "dns: " + strings.Join(missing, ", ") + " missing"
}

// loadProbe reads the day's probes first (site_probe's created_at index) and only then
// joins their sites by key for the reserved-name test. OFFSET 0 keeps the planner from
// flattening the subquery back into the join: flattened, it scans every site with the regex
// (about 1 s on a million sites).
func (s *Section) loadProbe(ctx context.Context, q store.DBTX, re string, from, to time.Time) error {
	rows, err := q.Query(ctx,
		`SELECT p.is_drupal, p.major, count(*)::int
		 FROM (SELECT site_id, is_drupal, major FROM site_probe
		       WHERE created_at >= $2 AND created_at < $3 OFFSET 0) p
		 JOIN site si ON si.id = p.site_id
		 WHERE si.domain !~ $1
		 GROUP BY p.is_drupal, p.major`, re, from, to)
	if err != nil {
		return fmt.Errorf("probes: %w", err)
	}
	p := ProbeLine{ByMajor: map[int]int{}}
	var (
		drupal bool
		major  int
		n      int
	)
	if _, err := pgx.ForEachRow(rows, []any{&drupal, &major, &n}, func() error {
		p.Checked += n
		if drupal {
			p.Drupal += n
			p.ByMajor[major] += n
		}
		return nil
	}); err != nil {
		return fmt.Errorf("probes: %w", err)
	}
	if p.Checked > 0 {
		s.Probe = &p
	}
	return nil
}
