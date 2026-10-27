package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

var (
	// ErrMessageNotFound is returned when no outreach_message row has that id.
	ErrMessageNotFound = errors.New("leadstore: message not found")
	// ErrNotManual is returned by MarkDone for an email message: only a contact-form or
	// LinkedIn item is used by hand.
	ErrNotManual = errors.New("leadstore: message is not a contact-form or LinkedIn item")
)

// BuildOrg is one org with what Classify reads: its site's last probe, when it has one,
// and its first agency finding, when it has one.
type BuildOrg struct {
	ID              int64  `db:"id"`
	Kind            string `db:"kind"`
	Domain          string `db:"domain"`
	Name            string `db:"name"`
	Country         string `db:"country"`
	ProbeID         *int64 `db:"probe_id"`
	IsDrupal        bool   `db:"is_drupal"`
	Major           int    `db:"major"`
	Ambiguous       string `db:"ambiguous"`
	AgencyFindingID *int64 `db:"agency_finding_id"`
	// The org's lead, when it has one: its id, state, kind and channel.
	LeadID      *int64 `db:"lead_id"`
	LeadState   string `db:"lead_state"`
	LeadKind    string `db:"lead_kind"`
	LeadChannel string `db:"lead_channel"`
}

// OrgsForBuild returns every org but selftest ones, by id, each with its site's last probe
// (site.last_probe_id; nil when the org has no site or the site no probe), its first
// agency finding (the lowest id; nil when it has none) and its lead in campaign campaignID
// (0: DefaultCampaignID), when it has one.
func (q Queries) OrgsForBuild(ctx context.Context, campaignID int64) ([]BuildOrg, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[BuildOrg],
		`SELECT o.id, o.kind, o.domain, o.name, o.country,
		   p.id AS probe_id, coalesce(p.is_drupal, false) AS is_drupal, coalesce(p.major, 0) AS major,
		   coalesce(p.ambiguous, '') AS ambiguous,
		   (SELECT min(f.id) FROM agency_finding f WHERE f.org_id = o.id) AS agency_finding_id,
		   l.id AS lead_id, coalesce(l.state, '') AS lead_state, coalesce(l.kind, '') AS lead_kind,
		   coalesce(l.channel, '') AS lead_channel
		 FROM org o
		 LEFT JOIN lead l ON l.org_id = o.id AND l.campaign_id = $1
		 LEFT JOIN site s ON s.id = o.site_id
		 LEFT JOIN site_probe p ON p.id = s.last_probe_id
		 WHERE o.kind <> 'selftest'
		 ORDER BY o.id`, orCampaign(campaignID))
	if err != nil {
		return nil, fmt.Errorf("leadstore: orgs for build: %w", err)
	}
	return rows, nil
}

// EvidenceFetch is the http_fetch row a piece of evidence was read from.
type EvidenceFetch struct {
	ID        int64     `db:"id"`
	URL       string    `db:"url"`
	FinalURL  string    `db:"final_url"` // "" when it equals URL
	FetchedAt time.Time `db:"fetched_at"`
}

// EvidenceSignal is one signal of a site lead's probe, with the fetch it came from (the
// zero EvidenceFetch when that fetch is not on file).
type EvidenceSignal struct {
	Signal
	Fetch EvidenceFetch
}

// Evidence is the finding one lead rests on. A site lead has ProbeID, its probe's final
// host and versions, and its signals; an agency lead has AgencyFindingID, the finding page,
// the matched phrase and the fetch that page was read in. A hand or selftest lead has
// neither.
type Evidence struct {
	LeadID       int64
	ProbeID      *int64
	FinalHost    string
	Major        int
	ExactVersion string
	ProbedAt     time.Time
	Signals      []EvidenceSignal

	AgencyFindingID *int64
	FindingURL      string
	Phrase          string
	FindingFetch    EvidenceFetch
}

// LeadEvidence returns the evidence of each listed lead, keyed by lead id. A lead id with
// no lead row is left out.
func (q Queries) LeadEvidence(ctx context.Context, leadIDs []int64) (map[int64]Evidence, error) {
	if leadIDs == nil {
		leadIDs = []int64{}
	}
	type row struct {
		LeadID          int64      `db:"lead_id"`
		ProbeID         *int64     `db:"probe_id"`
		FinalHost       string     `db:"final_host"`
		Major           int        `db:"major"`
		ExactVersion    string     `db:"exact_version"`
		ProbedAt        *time.Time `db:"probed_at"`
		Signals         []Signal   `db:"signals"`
		AgencyFindingID *int64     `db:"agency_finding_id"`
		FindingURL      string     `db:"finding_url"`
		Phrase          string     `db:"phrase"`
		FindingFetchID  *int64     `db:"finding_fetch_id"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT l.id AS lead_id, l.probe_id, coalesce(p.final_host, '') AS final_host,
		   coalesce(p.major, 0) AS major, coalesce(p.exact_version, '') AS exact_version,
		   p.created_at AS probed_at, coalesce(p.signals, '[]'::jsonb) AS signals,
		   l.agency_finding_id, coalesce(af.url, '') AS finding_url, coalesce(af.phrase, '') AS phrase,
		   af.fetch_id AS finding_fetch_id
		 FROM lead l
		 LEFT JOIN site_probe p ON p.id = l.probe_id
		 LEFT JOIN agency_finding af ON af.id = l.agency_finding_id
		 WHERE l.id = ANY($1)
		 ORDER BY l.id`, leadIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: lead evidence: %w", err)
	}
	fetchIDs := []int64{}
	for _, r := range rows {
		for _, s := range r.Signals {
			fetchIDs = append(fetchIDs, s.FetchID)
		}
		if r.FindingFetchID != nil {
			fetchIDs = append(fetchIDs, *r.FindingFetchID)
		}
	}
	fetches, err := collect(ctx, q.db, pgx.RowToStructByName[EvidenceFetch],
		`SELECT id, url, final_url, fetched_at FROM http_fetch WHERE id = ANY($1)`, fetchIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: lead evidence: %w", err)
	}
	byID := make(map[int64]EvidenceFetch, len(fetches))
	for _, f := range fetches {
		byID[f.ID] = f
	}
	out := make(map[int64]Evidence, len(rows))
	for _, r := range rows {
		e := Evidence{
			LeadID: r.LeadID, ProbeID: r.ProbeID, FinalHost: r.FinalHost, Major: r.Major, ExactVersion: r.ExactVersion,
			AgencyFindingID: r.AgencyFindingID, FindingURL: r.FindingURL, Phrase: r.Phrase,
		}
		if r.ProbedAt != nil {
			e.ProbedAt = *r.ProbedAt
		}
		for _, s := range r.Signals {
			e.Signals = append(e.Signals, EvidenceSignal{Signal: s, Fetch: byID[s.FetchID]})
		}
		if r.FindingFetchID != nil {
			e.FindingFetch = byID[*r.FindingFetchID]
		}
		out[r.LeadID] = e
	}
	return out, nil
}

// Done is what MarkDone did.
type Done struct {
	LeadID    int64
	Shadow    bool   // the message's batch is a shadow batch: the lead was left as it is (RA1)
	Moved     bool   // the lead moved from batched to contacted
	LeadState string // the lead's state afterwards
}

// MarkDone records that the operator used approved contact-form or LinkedIn message id by hand:
// the message becomes done (sent_at now). In a live batch its lead moves from batched to
// contacted with actor; a lead already past batched keeps its state, and a shadow batch
// never touches the lead (RA1). An email message is ErrNotManual, a message that is not
// approved ErrMessageState, a missing one ErrMessageNotFound; nothing changes then. Run it
// in a transaction.
func (q Queries) MarkDone(ctx context.Context, id int64, actor string) (Done, error) {
	if err := checkStateActor(LeadContacted, actor); err != nil {
		return Done{}, fmt.Errorf("leadstore: mark done: %w", err)
	}
	var (
		channel, state string
		d              Done
	)
	err := q.db.QueryRow(ctx,
		`SELECT m.channel, m.state, m.lead_id, b.shadow
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE m.id = $1 FOR UPDATE OF m`, id).Scan(&channel, &state, &d.LeadID, &d.Shadow)
	if errors.Is(err, pgx.ErrNoRows) {
		return Done{}, fmt.Errorf("leadstore: mark done: message %d: %w", id, ErrMessageNotFound)
	}
	if err != nil {
		return Done{}, fmt.Errorf("leadstore: mark done: %w", err)
	}
	switch {
	case channel != "form" && channel != "linkedin":
		return Done{}, fmt.Errorf("leadstore: mark done: message %d (%s): %w", id, channel, ErrNotManual)
	case state != MsgApproved:
		return Done{}, fmt.Errorf("leadstore: mark done: message %d is %s: %w", id, state, ErrMessageState)
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'done', sent_at = now() WHERE id = $1 AND state = 'approved'`, id)
	if err := markResult("mark done", id, tag.RowsAffected(), err); err != nil {
		return Done{}, err
	}
	if !d.Shadow {
		moved, err := q.setLeadStateFrom(ctx, d.LeadID, []string{LeadBatched}, LeadContacted,
			fmt.Sprintf("message %d done by hand", id), actor)
		if err != nil {
			return Done{}, fmt.Errorf("leadstore: mark done: %w", err)
		}
		d.Moved = moved
	}
	if err := q.db.QueryRow(ctx, `SELECT state FROM lead WHERE id = $1`, d.LeadID).Scan(&d.LeadState); err != nil {
		return Done{}, fmt.Errorf("leadstore: mark done: %w", err)
	}
	return d, nil
}

// SuppressionCount is how many active suppression rows have one kind and reason.
type SuppressionCount struct {
	Kind   string `db:"kind"`
	Reason string `db:"reason"`
	N      int    `db:"n"`
}

// SuppressionCounts counts the active suppression rows (none lifted, FO6) of account
// accountID (0: DefaultAccountID) by kind, then reason.
func (q Queries) SuppressionCounts(ctx context.Context, accountID int64) ([]SuppressionCount, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[SuppressionCount],
		`SELECT kind, reason, count(*)::int AS n FROM suppression_active WHERE account_id = $1
		 GROUP BY kind, reason ORDER BY kind, reason`, orAccount(accountID))
	if err != nil {
		return nil, fmt.Errorf("leadstore: suppression counts: %w", err)
	}
	return rows, nil
}

// LiftedCount is how many suppressions have been lifted (FO6).
func (q Queries) LiftedCount(ctx context.Context) (int, error) {
	var n int
	if err := q.db.QueryRow(ctx, `SELECT count(*)::int FROM suppression_lift`).Scan(&n); err != nil {
		return 0, fmt.Errorf("leadstore: lifted count: %w", err)
	}
	return n, nil
}

// batchNewLockKey is the transaction-level advisory lock one batch new holds from its guard
// to its CreateBatch, so two at once never both pass the guard.
const batchNewLockKey int64 = 0x4c48_4241_5443 // "LHBATC"

// LockBatchNew waits for the batch-new lock and holds it until the transaction ends. Run it
// in a transaction, first.
func (q Queries) LockBatchNew(ctx context.Context) error {
	if _, err := q.db.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, batchNewLockKey); err != nil {
		return fmt.Errorf("leadstore: lock batch new: %w", err)
	}
	return nil
}

// UnapprovedBatch returns the oldest batch of the given kind (shadow or live) that is
// neither approved nor discarded, of account accountID (0: DefaultAccountID): the one a new
// batch waits for. ok is false when none waits.
func (q Queries) UnapprovedBatch(ctx context.Context, accountID int64, shadow bool) (id int64, ok bool, err error) {
	err = q.db.QueryRow(ctx,
		`SELECT id FROM outreach_batch WHERE shadow = $1 AND approved_at IS NULL AND discarded_at IS NULL
		   AND account_id = $2
		 ORDER BY id LIMIT 1`, shadow, orAccount(accountID)).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("leadstore: unapproved batch: %w", err)
	}
	return id, true, nil
}

// WaitingEmails counts the live emails that may still go, follow-ups included (with a due
// time or not): approved, and toCheck, those maybe sent or left sending, which go again if
// the operator finds them not sent. Selftests never count. A new batch's emails come after
// these. A toCheck row attempted today also counts in today's sends (SentSince), so the
// two together err on the side of drafting less.
func (q Queries) WaitingEmails(ctx context.Context) (approved, toCheck int, err error) {
	if err := q.db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE m.state = 'approved'), count(*) FILTER (WHERE m.state IN ('sending', 'maybe_sent'))
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE NOT b.shadow AND b.approved_at IS NOT NULL AND m.channel = 'email' AND l.kind <> 'selftest'
		   AND m.state IN ('approved', 'sending', 'maybe_sent')`).Scan(&approved, &toCheck); err != nil {
		return 0, 0, fmt.Errorf("leadstore: waiting emails: %w", err)
	}
	return approved, toCheck, nil
}

// CancelDiscarded is the cancel_reason of a discarded batch's messages.
const CancelDiscarded = "batch discarded"

// Discard is what DiscardBatch did.
type Discard struct {
	Already   bool  // the batch was discarded before; nothing changed
	Shadow    bool  // a shadow batch: its leads never left new
	Cancelled int64 // the batch's pending messages cancelled
	LeadsBack int   // leads moved from batched back to new
}

// DiscardBatch throws away batch id before approval: with the batch row locked (as
// ApproveBatch locks it), its own draft and approved messages are cancelled (CancelDiscarded),
// its leads go from batched back to new, so the next batch may draft them, and discarded_at
// is set. An approved batch is ErrBatchApproved, a missing one ErrBatchNotFound; nothing
// changes then. Run it in a transaction.
func (q Queries) DiscardBatch(ctx context.Context, id int64) (Discard, error) {
	var (
		d         Discard
		approved  bool
		discarded bool
	)
	err := q.db.QueryRow(ctx,
		`SELECT shadow, approved_at IS NOT NULL, discarded_at IS NOT NULL FROM outreach_batch WHERE id = $1 FOR UPDATE`, id).
		Scan(&d.Shadow, &approved, &discarded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Discard{}, fmt.Errorf("leadstore: discard batch %d: %w", id, ErrBatchNotFound)
	case err != nil:
		return Discard{}, fmt.Errorf("leadstore: discard batch: %w", err)
	case approved:
		return Discard{}, fmt.Errorf("leadstore: discard batch %d: %w", id, ErrBatchApproved)
	case discarded:
		return Discard{Already: true, Shadow: d.Shadow}, nil
	}
	leadIDs, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT DISTINCT lead_id FROM outreach_message WHERE batch_id = $1 ORDER BY lead_id`, id)
	if err != nil {
		return Discard{}, fmt.Errorf("leadstore: discard batch: %w", err)
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'cancelled', cancel_reason = $2
		 WHERE batch_id = $1 AND state IN ('draft', 'approved')`, id, CancelDiscarded)
	if err != nil {
		return Discard{}, fmt.Errorf("leadstore: discard batch: %w", err)
	}
	d.Cancelled = tag.RowsAffected()
	if !d.Shadow {
		reason := fmt.Sprintf("batch %d discarded", id)
		for _, l := range leadIDs {
			moved, err := q.setLeadStateFrom(ctx, l, []string{LeadBatched}, LeadNew, reason, ActorGremy)
			if err != nil {
				return Discard{}, fmt.Errorf("leadstore: discard batch: %w", err)
			}
			if moved {
				d.LeadsBack++
			}
		}
	}
	if _, err := q.db.Exec(ctx, `UPDATE outreach_batch SET discarded_at = now() WHERE id = $1`, id); err != nil {
		return Discard{}, fmt.Errorf("leadstore: discard batch: %w", err)
	}
	return d, nil
}

// LatestSenderCheckSelector is the latest check of the domain whose hash is domainSHA with
// DKIM selector selector made at or after since; ok is false when there is none. A row with
// no selector (the single-mailbox check) never counts, as in SenderCheckPassedSinceSelector.
func (q Queries) LatestSenderCheckSelector(ctx context.Context, domainSHA []byte, selector string, since time.Time) (r mailbox.DNSResult, ok bool, err error) {
	err = q.db.QueryRow(ctx,
		`SELECT spf, dkim, dmarc, note FROM sender_check
		 WHERE domain_sha256 = $1 AND selector = $2 AND selector <> '' AND checked_at >= $3
		 ORDER BY checked_at DESC, id DESC LIMIT 1`,
		domainSHA, strings.TrimSpace(selector), since).Scan(&r.SPF, &r.DKIM, &r.DMARC, &r.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return mailbox.DNSResult{}, false, nil
	}
	if err != nil {
		return mailbox.DNSResult{}, false, fmt.Errorf("leadstore: latest sender check: %w", err)
	}
	return r, true, nil
}
