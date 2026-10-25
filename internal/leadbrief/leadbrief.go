// Package leadbrief reads the leads section of the morning brief: what is waiting on the
// member (replies to read, a batch to approve, manual items, emails to check by hand) and
// the outreach health of yesterday. Its types are plain values the brief renders; it never
// carries an address or a person's name, only registrable domains and counts.
//
// Fixture data stays out (RA25): every count leaves out selftest orgs and leads, and orgs,
// sites and senders on a reserved TLD (leadstore.ReservedTLDs).
package leadbrief

import (
	"context"
	"errors"
	"fmt"
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
// not, the brief shows only its health line.
func (s Section) Todo() bool {
	return len(s.Replies) > 0 || s.Other > 0 || s.Paused != "" || s.OperatorPaused != nil || s.Batch != nil ||
		s.ManualWaiting > 0 || s.CheckByHand > 0
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
// noteAccount).
const (
	noteDNS     = "dns"
	noteSync    = "sync"
	noteAccount = "smtp-account"
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

	var s Section
	steps := []func() error{
		func() error { return s.loadReplies(ctx, q, re, yesterday, tomorrow) },
		func() error { return s.loadOther(ctx, q, re, yesterday, tomorrow) },
		func() error { return s.loadBatch(ctx, q, re) },
		func() error { return s.loadOutreach(ctx, q, re, yesterday, today) },
		func() error { return s.loadPaused(ctx, q, yesterday) },
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

// loadPaused reads why the latest finished send run since from stopped sending, if it did.
// A DNS stop names the records the latest sender check found missing, and a sender check
// that has passed since clears it.
func (s *Section) loadPaused(ctx context.Context, q store.DBTX, from time.Time) error {
	var note string
	err := q.QueryRow(ctx,
		`SELECT note FROM run WHERE command = $1 AND finished_at IS NOT NULL AND started_at >= $2
		 ORDER BY id DESC LIMIT 1`, sendCommand, from).Scan(&note)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("send run: %w", err)
	}
	words := NoteWords(note)
	switch {
	case words[noteDNS]:
		var spf, dkim, dmarc bool
		err := q.QueryRow(ctx,
			`SELECT spf, dkim, dmarc FROM sender_check ORDER BY checked_at DESC, id DESC LIMIT 1`).Scan(&spf, &dkim, &dmarc)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			s.Paused = PausedDNS
		case err != nil:
			return fmt.Errorf("sender check: %w", err)
		default:
			s.Paused = DNSPaused(spf, dkim, dmarc)
		}
	case words[noteSync]:
		s.Paused = PausedSync
	case words[noteAccount]:
		s.Paused = PausedAccount
	}
	return nil
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
