package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// NextUpPage is how many leads NextUp returns when asked for no number.
const NextUpPage = 50

// The hand-steering actions a lead_priority row records (lead_priority_action_check).
const (
	PriorityPin   = "pin"
	PriorityDefer = "defer"
	PriorityClear = "clear"
)

// Reason is one rule that gave a lead points under a profile: Kind names the rule
// (ReasonKind and the others), Label its subject (the lead kind, "7", "GB", "email", the
// band bound), Points what it gave.
type Reason struct {
	Kind, Label string
	Points      int
}

// The rules a Reason can name, in the order ScoreReasons emits them.
const (
	ReasonKind         = "kind"
	ReasonMajor        = "major"
	ReasonExactVersion = "exact_version"
	ReasonCountry      = "country"
	ReasonChannel      = "channel"
	ReasonRankBand     = "rank_band"
)

// scoreSQL is a lead's score under the profile pf.pf (jsonb), over CandidateLeads' aliases:
// l lead, o org, p site_probe, s site. Score is its Go twin; TestPriorityDrift holds the two
// equal.
const scoreSQL = `coalesce((pf.pf->'kinds'->>l.kind)::int, 0)
	   + coalesce((pf.pf->'majors'->>p.major::text)::int, 0)
	   + CASE WHEN coalesce(p.exact_version, '') <> '' THEN coalesce((pf.pf->>'exact_version')::int, 0) ELSE 0 END
	   + coalesce((pf.pf->'countries'->>o.country)::int, 0)
	   + coalesce((pf.pf->'channels'->>l.channel)::int, 0)
	   + coalesce((SELECT (b->>'points')::int FROM jsonb_array_elements(pf.pf->'rank_bands') b
	       WHERE s.list_rank < (b->>'under')::int ORDER BY (b->>'under')::int LIMIT 1), 0)`

// Score is c's score under p and the reasons for it, one per rule that gave points, in
// the order kind, Drupal major, exact version, country, channel, rank band (e.g.
// "Drupal 7 +30", "GB +20", "rank under 10k +15"). It is the oracle of the score
// CandidateLeads computes in SQL; whether c is offered at all is Offered.
func Score(p leadconfig.Profile, c Candidate) (int, []string) {
	var score int
	var why []string
	for _, r := range ScoreReasons(p, c) {
		score += r.Points
		why = append(why, fmt.Sprintf("%s %+d", r.shortLabel(), r.Points))
	}
	return score, why
}

// ScoreReasons is every rule that gave c points under p, in Score's order, with its raw
// subject as Label: the lead kind ("agency_partner"), the major ("7"), the exact version
// ("7.98"), the country ("GB"), the channel ("email") and the band's bound ("10000"). A rule
// that gave 0 points is left out. Score is the sum of their Points.
func ScoreReasons(p leadconfig.Profile, c Candidate) []Reason {
	var out []Reason
	add := func(kind, label string, points int) {
		if points != 0 {
			out = append(out, Reason{Kind: kind, Label: label, Points: points})
		}
	}
	add(ReasonKind, c.Kind, p.Kinds[c.Kind])
	add(ReasonMajor, strconv.Itoa(c.Major), p.Majors[c.Major])
	if c.ExactVersion != "" {
		add(ReasonExactVersion, c.ExactVersion, p.ExactVersion)
	}
	if c.Country != "" {
		add(ReasonCountry, c.Country, p.Countries[c.Country])
	}
	add(ReasonChannel, c.Channel, p.Channels[c.Channel])
	if band, ok := rankBand(p.RankBands, c.ListRank); ok {
		add(ReasonRankBand, strconv.Itoa(band.Under), band.Points)
	}
	return out
}

// shortLabel is r as Score words it: "agency partner", "Drupal 7", "exact version", "GB",
// "email", "rank under 10k".
func (r Reason) shortLabel() string {
	switch r.Kind {
	case ReasonKind:
		return strings.ReplaceAll(r.Label, "_", " ")
	case ReasonMajor:
		return "Drupal " + r.Label
	case ReasonExactVersion:
		return "exact version"
	case ReasonRankBand:
		if n, err := strconv.Atoi(r.Label); err == nil {
			return "rank under " + rankLabel(n)
		}
		return "rank under " + r.Label
	}
	return r.Label
}

// Offered reports whether p lets c be offered at all: its country is not excluded.
func Offered(p leadconfig.Profile, c Candidate) bool {
	return !slices.Contains(p.ExcludeCountries, c.Country)
}

// rankBand is the band with the smallest Under that rank falls below; none for an
// unranked site or one beyond every band.
func rankBand(bands []leadconfig.RankBand, rank *int) (leadconfig.RankBand, bool) {
	var best leadconfig.RankBand
	found := false
	if rank == nil {
		return best, false
	}
	for _, b := range bands {
		if *rank < b.Under && (!found || b.Under < best.Under) {
			best, found = b, true
		}
	}
	return best, found
}

// rankLabel writes a band's bound short: 10k, 1M; other numbers as they are.
func rankLabel(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return strconv.Itoa(n/1_000_000) + "M"
	case n >= 1000 && n%1000 == 0:
		return strconv.Itoa(n/1000) + "k"
	}
	return strconv.Itoa(n)
}

// profileJSON encodes p for the score SQL, with every absent rule as an empty map or list
// so that no path reads a JSON null.
func profileJSON(p leadconfig.Profile) (string, error) {
	p = p.Clone()
	if p.Kinds == nil {
		p.Kinds = map[string]int{}
	}
	if p.Majors == nil {
		p.Majors = map[int]int{}
	}
	if p.Countries == nil {
		p.Countries = map[string]int{}
	}
	if p.Channels == nil {
		p.Channels = map[string]int{}
	}
	if p.RankBands == nil {
		p.RankBands = []leadconfig.RankBand{}
	}
	if p.ExcludeCountries == nil {
		p.ExcludeCountries = []string{}
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// SaveProfile validates p and appends it as the operator's latest priority profile,
// returning its version id. A profile without kinds keeps the file's kind points
// (EffectiveProfile).
func (q Queries) SaveProfile(ctx context.Context, p leadconfig.Profile) (int64, error) {
	if err := p.Validate(); err != nil {
		return 0, fmt.Errorf("leadstore: save profile: %w", err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("leadstore: save profile: %w", err)
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO priority_profile (profile, actor) VALUES ($1::jsonb, $2) RETURNING id`, string(b), ActorGremy).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: save profile: %w", err)
	}
	return id, nil
}

// ResetProfile appends a reset: from now on the file's defaults order the leads again.
func (q Queries) ResetProfile(ctx context.Context) (int64, error) {
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO priority_profile (profile, actor) VALUES (NULL, $1) RETURNING id`, ActorGremy).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: reset profile: %w", err)
	}
	return id, nil
}

// EffectiveProfile is the profile the queue is scored by: the operator's latest saved
// version, its kinds taken from fallback when it has none; or fallback (the file's
// defaults) when no version was saved or the latest is a reset. fromDB says which.
func (q Queries) EffectiveProfile(ctx context.Context, fallback leadconfig.Profile) (p leadconfig.Profile, fromDB bool, err error) {
	var raw []byte
	err = q.db.QueryRow(ctx, `SELECT profile FROM priority_profile ORDER BY id DESC LIMIT 1`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && raw == nil) {
		return fallback, false, nil
	}
	if err != nil {
		return leadconfig.Profile{}, false, fmt.Errorf("leadstore: effective profile: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return leadconfig.Profile{}, false, fmt.Errorf("leadstore: effective profile: %w", err)
	}
	if p.Kinds == nil {
		p.Kinds = fallback.Clone().Kinds
	}
	return p, true, nil
}

// PinLead puts lead leadID at the head of the queue (the newest pin first) until it is
// cleared or deferred.
func (q Queries) PinLead(ctx context.Context, leadID int64) error {
	return q.steer(ctx, leadID, PriorityPin, nil)
}

// DeferLead hides lead leadID from the queue through the day until; it comes back the day
// after.
func (q Queries) DeferLead(ctx context.Context, leadID int64, until time.Time) error {
	day := until.Format(time.DateOnly)
	return q.steer(ctx, leadID, PriorityDefer, &day)
}

// ClearLeadPriority undoes a pin or a defer of lead leadID.
func (q Queries) ClearLeadPriority(ctx context.Context, leadID int64) error {
	return q.steer(ctx, leadID, PriorityClear, nil)
}

// steer appends one lead_priority row by the operator, or ErrLeadNotFound.
func (q Queries) steer(ctx context.Context, leadID int64, action string, until *string) error {
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO lead_priority (lead_id, action, until, actor)
		 SELECT id, $2, $3::date, $4 FROM lead WHERE id = $1 RETURNING id`, leadID, action, until, ActorGremy).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("leadstore: %s lead %d: %w", action, leadID, ErrLeadNotFound)
	}
	if err != nil {
		return fmt.Errorf("leadstore: %s lead %d: %w", action, leadID, err)
	}
	return nil
}

// SkipLead moves lead leadID from new to skipped by the operator, so it is never offered
// again. A lead past new (a batch may just have taken it) does not move: moved is false.
func (q Queries) SkipLead(ctx context.Context, leadID int64) (moved bool, err error) {
	moved, err = q.setLeadStateFrom(ctx, leadID, []string{LeadNew}, LeadSkipped, "skipped from the queue", ActorGremy)
	if err != nil {
		return false, fmt.Errorf("leadstore: skip lead: %w", err)
	}
	if !moved {
		if _, err := q.LeadByID(ctx, leadID); err != nil {
			return false, err
		}
	}
	return moved, nil
}

// NextUp is the console's queue: CandidateLeads in the order a batch drafts from, at most
// limit leads (NextUpPage when limit <= 0).
func (q Queries) NextUp(ctx context.Context, o CandidateOptions, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = NextUpPage
	}
	o.Limit = limit
	return q.CandidateLeads(ctx, o)
}
