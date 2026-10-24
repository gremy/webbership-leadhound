package leadstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
// agency finding (the lowest id; nil when it has none) and its lead, when it has one.
func (q Queries) OrgsForBuild(ctx context.Context) ([]BuildOrg, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[BuildOrg],
		`SELECT o.id, o.kind, o.domain, o.name, o.country,
		   p.id AS probe_id, coalesce(p.is_drupal, false) AS is_drupal, coalesce(p.major, 0) AS major,
		   coalesce(p.ambiguous, '') AS ambiguous,
		   (SELECT min(f.id) FROM agency_finding f WHERE f.org_id = o.id) AS agency_finding_id,
		   l.id AS lead_id, coalesce(l.state, '') AS lead_state, coalesce(l.kind, '') AS lead_kind,
		   coalesce(l.channel, '') AS lead_channel
		 FROM org o
		 LEFT JOIN lead l ON l.org_id = o.id
		 LEFT JOIN site s ON s.id = o.site_id
		 LEFT JOIN site_probe p ON p.id = s.last_probe_id
		 WHERE o.kind <> 'selftest'
		 ORDER BY o.id`)
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

// MarkDone records that Gremy used approved contact-form or LinkedIn message id by hand:
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

// SuppressionCount is how many suppression rows have one kind and reason.
type SuppressionCount struct {
	Kind   string `db:"kind"`
	Reason string `db:"reason"`
	N      int    `db:"n"`
}

// SuppressionCounts counts the suppression rows by kind, then reason.
func (q Queries) SuppressionCounts(ctx context.Context) ([]SuppressionCount, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[SuppressionCount],
		`SELECT kind, reason, count(*)::int AS n FROM suppression GROUP BY kind, reason ORDER BY kind, reason`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: suppression counts: %w", err)
	}
	return rows, nil
}
