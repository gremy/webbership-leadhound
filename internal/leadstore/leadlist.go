package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// The Leads list: every lead with its own finding, its latest relevance and star, and what
// is known about its org, filtered, sorted and paged in one query; and one lead in full.

// The status filters a list can take (ListFilter.Status).
//
//   - open: state new, the leads not yet taken into outreach (the default).
//   - contacted: the states a batch or a send has reached, batched, contacted, replied,
//     bounced and opted_out; the same set CandidateLeads treats as a reached domain. A lead
//     reaches them only through a batch (batched, then contacted on send), the mailbox
//     (replied, bounced, opted_out) or MarkContacted (contacted by hand).
//   - all: every state, so also suppressed (a suppression caught it while new or batched,
//     before any send), skipped (by the operator, or from a batch before a send) and
//     closed.
const (
	ListOpen      = "open"
	ListContacted = "contacted"
	ListAll       = "all"
)

// The sorts a list can take (ListFilter.Sort).
const (
	SortRelevance = "relevance" // the default
	SortNewest    = "newest"
	SortOrder     = "order" // starred leads only, in the operator's order
)

// The channels a list can be filtered by (ListFilter.Channels).
const (
	ListChannelEmail = "email"
	ListChannelForm  = "form"
	ListChannelPhone = "phone"
)

// The page sizes of a list (ListFilter.PerPage), and the highest page LeadList reads: a
// larger ListFilter.Page reads this one, so the offset never overflows.
const (
	ListPerPageDefault = 50
	ListPerPageMax     = 100
	ListPageMax        = 1_000_000
)

// contactedStates are the states ListContacted shows.
var contactedStates = []string{LeadBatched, LeadContacted, LeadReplied, LeadBounced, LeadOptedOut}

// ErrBadListFilter is returned by LeadList for a status, sort or channel it does not know.
var ErrBadListFilter = errors.New("leadstore: unknown list filter value")

// FindingSubject is what a lead's own finding says, from its subject: a site finding's
// version, or an agency finding's page and words. Every field is empty for a lead without
// a finding (a hand lead).
type FindingSubject struct {
	Kind         string // lead_kind.name
	Detector     string
	Major        int // site findings; 0 when unknown
	ExactVersion string
	Ambiguous    string
	URL          string // agency findings
	Phrase       string
	// A café finding (cafe@1): the Decision's plain reason and the language source.
	Reason      string
	Language    string // ISO 639-1; "" when unknown
	LanguageVia string // html-lang | meta | text
}

// ListFilter is one page of the list the operator asked for.
type ListFilter struct {
	Kinds, Countries, Channels []string // channels: email | form | phone
	Starred                    *bool
	HasPhone                   bool
	MinRelevance               int    // 0..100
	Status                     string // open (default, state new) | contacted | all
	Sort                       string // relevance (default) | newest | order
	Page, PerPage              int    // PerPage default 50, max 100
	// LeadIDs, when not nil, keeps only these leads (the To label filter); an empty, non-nil
	// slice matches no lead.
	LeadIDs []int64
	// CampaignID keeps only the leads of that campaign; 0 lists every campaign's.
	CampaignID int64
}

// ListContext is what the list is weighed by.
type ListContext struct {
	ProfileJSON     string // the effective priority profile, for points
	SkipRankOver    int
	SortByRelevance bool // the trust gate passed (D9)
	// CampaignID keeps LeadsToRate to that campaign's leads; 0 is every campaign's.
	CampaignID int64
}

// ListRow is one lead of the list.
type ListRow struct {
	LeadID, OrgID               int64
	CampaignID                  int64
	CampaignSlug, CampaignName  string
	Domain, Name                string // Name: site_name, else org.name, else domain
	Country, Kind, State        string
	Channel                     string // the lead's channel (email, form, linkedin or none)
	HasEmail, HasForm, HasPhone bool
	Relevance                   *int
	Band                        string
	RatedAt                     *time.Time
	Points                      int
	Starred                     bool
	Position                    *int64
	SmallSite                   bool
	Finding                     FindingSubject
	CreatedAt                   time.Time
}

// Detail is one lead in full: its list row and everything behind it.
type Detail struct {
	ListRow
	Signals          []DetailSignal // name, value, fetch id, URL, final URL, status, fetched at
	Routes           []DetailRoute  // kind, value, mx_ok, page fetch id + final URL, found at
	Facts            []DetailFact   // kind, value, page fetch id + final URL, found at
	Agency           *DetailAgency  // url, phrase, fetch id
	ListRank         *int
	Origin           string
	Events           []DetailEvent     // state, reason, actor, at
	Priority         *DetailPriority   // latest lead_priority row
	Messages         []DetailMessage   // batch, step, channel, state, due, sent, subject, body
	RelevanceHistory []DetailRelevance // last 5
	Reasons          []Reason

	// score is what Weigh scores the lead by: its probe's version, as CandidateLeads reads it.
	score Candidate
}

// DetailSignal is one piece of the finding's evidence and the fetch it was read from; the
// fetch fields are zero when the evidence names no fetch.
type DetailSignal struct {
	Name      string
	Value     string
	FetchID   *int64
	URL       string
	FinalURL  string // after redirects; "" when the same as URL
	Status    int
	FetchedAt *time.Time
}

// DetailRoute is one contact route of the lead's org and the page it was read from.
type DetailRoute struct {
	Kind     string // email | form
	Value    string
	MXOK     bool
	FetchID  int64
	FinalURL string // the page's address after redirects, else the URL asked for
	FoundAt  time.Time
}

// DetailFact is one fact of the lead's org (enrichment) and the page it was read from.
type DetailFact struct {
	Kind     string // phone | description | social
	Value    string
	FetchID  int64
	FinalURL string // the page's address after redirects, else the URL asked for
	FoundAt  time.Time
}

// DetailAgency is an agency lead's finding page.
type DetailAgency struct {
	URL     string
	Phrase  string
	FetchID int64
}

// DetailEvent is one state change of the lead.
type DetailEvent struct {
	State  string
	Reason string
	Actor  string
	At     time.Time
}

// DetailPriority is the lead's latest lead_priority row.
type DetailPriority struct {
	Action string     // pin | defer | clear
	Until  *time.Time // defer only: hidden through this day
	Actor  string
	At     time.Time
}

// DetailMessage is one outreach message drafted for the lead, its body included.
type DetailMessage struct {
	ID      int64
	BatchID int64
	Shadow  bool // a shadow batch never goes live
	Step    int
	Channel string
	State   string
	DueAt   *time.Time
	SentAt  *time.Time
	Subject string
	Body    string
}

// DetailRelevance is one rating of the lead.
type DetailRelevance struct {
	Score     int
	Band      string
	Question  string
	Decider   string
	Model     string
	CreatedAt time.Time
}

// ListProfileJSON encodes p as ListContext.ProfileJSON, the form the points SQL reads.
func ListProfileJSON(p leadconfig.Profile) (string, error) {
	s, err := profileJSON(p)
	if err != nil {
		return "", fmt.Errorf("leadstore: list profile: %w", err)
	}
	return s, nil
}

// listRow is one row of listRowsSQL.
type listRow struct {
	LeadID          int64      `db:"lead_id"`
	OrgID           int64      `db:"org_id"`
	CampaignID      int64      `db:"campaign_id"`
	CampaignSlug    string     `db:"campaign_slug"`
	CampaignName    string     `db:"campaign_name"`
	Domain          string     `db:"domain"`
	Name            string     `db:"name"`
	Country         string     `db:"country"`
	Kind            string     `db:"kind"`
	State           string     `db:"state"`
	Channel         string     `db:"channel"`
	ProbeID         *int64     `db:"probe_id"`
	AgencyFindingID *int64     `db:"agency_finding_id"`
	HasEmail        bool       `db:"has_email"`
	HasForm         bool       `db:"has_form"`
	HasPhone        bool       `db:"has_phone"`
	Relevance       *int       `db:"relevance"`
	Band            string     `db:"band"`
	RatedAt         *time.Time `db:"rated_at"`
	Points          int        `db:"points"`
	Starred         bool       `db:"starred"`
	Position        *int64     `db:"position"`
	SmallSite       bool       `db:"small_site"`
	FindingKind     string     `db:"f_kind"`
	Detector        string     `db:"f_detector"`
	Major           int        `db:"f_major"`
	ExactVersion    string     `db:"f_exact_version"`
	Ambiguous       string     `db:"f_ambiguous"`
	URL             string     `db:"f_url"`
	Phrase          string     `db:"f_phrase"`
	Reason          string     `db:"f_reason"`
	Language        string     `db:"f_language"`
	LanguageVia     string     `db:"f_language_via"`
	ListRank        *int       `db:"list_rank"`
	Origin          string     `db:"origin"`
	ProbeMajor      int        `db:"probe_major"`
	ProbeExact      string     `db:"probe_exact"`
	CreatedAt       time.Time  `db:"created_at"`
	Total           int        `db:"total"`
}

func (r listRow) row() ListRow {
	return ListRow{
		LeadID: r.LeadID, OrgID: r.OrgID, Domain: r.Domain, Name: r.Name,
		CampaignID: r.CampaignID, CampaignSlug: r.CampaignSlug, CampaignName: r.CampaignName,
		Country: r.Country, Kind: r.Kind, State: r.State, Channel: r.Channel,
		HasEmail: r.HasEmail, HasForm: r.HasForm, HasPhone: r.HasPhone,
		Relevance: r.Relevance, Band: r.Band, RatedAt: r.RatedAt, Points: r.Points,
		Starred: r.Starred, Position: r.Position, SmallSite: r.SmallSite,
		Finding: FindingSubject{
			Kind: r.FindingKind, Detector: r.Detector, Major: r.Major, ExactVersion: r.ExactVersion,
			Ambiguous: r.Ambiguous, URL: r.URL, Phrase: r.Phrase,
			Reason: r.Reason, Language: r.Language, LanguageVia: r.LanguageVia,
		},
		CreatedAt: r.CreatedAt,
	}
}

// listRowsSQL is every lead but the selftest ones as listRow's columns but total, over the
// aliases scoreSQL reads (l lead, o org, p site_probe, s site, pf the profile): $1 the
// profile (jsonb), $2 SkipRankOver. Points are scoreSQL over the lead's own probe, as
// CandidateLeads scores it; unlike the queue, a lead whose probe is no longer its site's
// latest is listed with its own finding. A site lead is one with a probe. No message body
// is read.
const listRowsSQL = `SELECT l.id AS lead_id, l.org_id, l.campaign_id, cg.slug AS campaign_slug,
	   cg.name AS campaign_name, o.domain,
	   coalesce(nullif(p.site_name, ''), nullif(o.name, ''), o.domain) AS name,
	   o.country, l.kind, l.state, l.channel, l.probe_id, l.agency_finding_id, l.created_at,
	   EXISTS (SELECT 1 FROM contact_route r WHERE r.org_id = l.org_id AND r.kind = 'email') AS has_email,
	   EXISTS (SELECT 1 FROM contact_route r WHERE r.org_id = l.org_id AND r.kind = 'form') AS has_form,
	   EXISTS (SELECT 1 FROM org_fact x WHERE x.org_id = l.org_id AND x.kind = 'phone') AS has_phone,
	   rv.score AS relevance, coalesce(rv.band, '') AS band, rv.created_at AS rated_at,
	   sc.score AS points,
	   coalesce(st.action <> 'unstar', false) AS starred,
	   CASE WHEN st.action <> 'unstar' THEN st.position END AS position,
	   (l.probe_id IS NOT NULL AND $2::int > 0 AND (s.list_rank IS NULL OR s.list_rank > $2::int)) AS small_site,
	   coalesce(f.kind, '') AS f_kind, coalesce(f.detector, '') AS f_detector,
	   CASE WHEN jsonb_typeof(f.subject->'major') = 'number' THEN (f.subject->>'major')::int ELSE 0 END AS f_major,
	   coalesce(f.subject->>'exact_version', '') AS f_exact_version,
	   coalesce(f.subject->>'ambiguous', '') AS f_ambiguous,
	   coalesce(f.subject->>'url', '') AS f_url,
	   coalesce(f.subject->>'phrase', '') AS f_phrase,
	   coalesce(f.subject->>'reason', '') AS f_reason, coalesce(f.subject->>'language', '') AS f_language,
	   coalesce(f.subject->>'language_via', '') AS f_language_via,
	   s.list_rank, coalesce(s.origin, o.source) AS origin,
	   coalesce(p.major, 0) AS probe_major, coalesce(p.exact_version, '') AS probe_exact
	 FROM lead l
	 CROSS JOIN (SELECT $1::jsonb AS pf) pf
	 JOIN org o ON o.id = l.org_id
	 JOIN campaign cg ON cg.id = l.campaign_id
	 LEFT JOIN site_probe p ON p.id = l.probe_id
	 LEFT JOIN site s ON s.id = coalesce(p.site_id, o.site_id)
	 LEFT JOIN finding f ON f.id = l.finding_id
	 LEFT JOIN LATERAL (
	   SELECT r.score, r.band, r.created_at FROM lead_relevance r
	   WHERE r.lead_id = l.id ORDER BY r.id DESC LIMIT 1
	 ) rv ON true
	 LEFT JOIN LATERAL (
	   SELECT x.action, x.position FROM lead_star x
	   WHERE x.lead_id = l.id ORDER BY x.id DESC LIMIT 1
	 ) st ON true
	 CROSS JOIN LATERAL (SELECT ` + scoreSQL + ` AS score) sc
	 WHERE l.kind <> 'selftest' AND o.kind <> 'selftest'`

// LeadList returns page f.Page of the leads f asks for, weighed by c, and how many leads
// match in all. Filters combine with AND; Kinds, Countries and Channels each match any of
// their values (a lead with an email route, a form route or a phone fact for its
// channels). MinRelevance leaves out the leads never rated. The sorts:
//   - relevance, c.SortByRelevance: relevance (unrated last), then points, then list rank
//     (unranked last), then id;
//   - relevance, not trusted: points, then list rank, then id;
//   - newest: created, newest first, then id, highest first;
//   - order: the starred leads only, by position, then id.
//
// A page past the last returns no rows and the true total. An unknown status, sort or
// channel is ErrBadListFilter.
func (q Queries) LeadList(ctx context.Context, f ListFilter, c ListContext) ([]ListRow, int, error) {
	var conds []string
	args := []any{profileOrEmpty(c.ProfileJSON), c.SkipRankOver}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if len(f.Kinds) > 0 {
		conds = append(conds, "m.kind = ANY("+arg(f.Kinds)+"::text[])")
	}
	if len(f.Countries) > 0 {
		conds = append(conds, "m.country = ANY("+arg(f.Countries)+"::text[])")
	}
	if len(f.Channels) > 0 {
		var ors []string
		for _, ch := range f.Channels {
			switch ch {
			case ListChannelEmail:
				ors = append(ors, "m.has_email")
			case ListChannelForm:
				ors = append(ors, "m.has_form")
			case ListChannelPhone:
				ors = append(ors, "m.has_phone")
			default:
				return nil, 0, fmt.Errorf("leadstore: lead list: channel %q: %w", ch, ErrBadListFilter)
			}
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	if f.Starred != nil {
		conds = append(conds, "m.starred = "+arg(*f.Starred))
	}
	if f.HasPhone {
		conds = append(conds, "m.has_phone")
	}
	if f.CampaignID > 0 {
		conds = append(conds, "m.campaign_id = "+arg(f.CampaignID))
	}
	if f.LeadIDs != nil {
		conds = append(conds, "m.lead_id = ANY("+arg(f.LeadIDs)+"::bigint[])")
	}
	if f.MinRelevance > 0 {
		conds = append(conds, "m.relevance >= "+arg(min(f.MinRelevance, 100)))
	}
	switch f.Status {
	case "", ListOpen:
		conds = append(conds, "m.state = 'new'")
	case ListContacted:
		conds = append(conds, "m.state = ANY("+arg(contactedStates)+"::text[])")
	case ListAll:
	default:
		return nil, 0, fmt.Errorf("leadstore: lead list: status %q: %w", f.Status, ErrBadListFilter)
	}
	var order string
	switch f.Sort {
	case "", SortRelevance:
		order = "m.points DESC, m.list_rank NULLS LAST, m.lead_id"
		if c.SortByRelevance {
			order = "m.relevance DESC NULLS LAST, " + order
		}
	case SortNewest:
		order = "m.created_at DESC, m.lead_id DESC"
	case SortOrder:
		conds = append(conds, "m.starred")
		order = "m.position, m.lead_id"
	default:
		return nil, 0, fmt.Errorf("leadstore: lead list: sort %q: %w", f.Sort, ErrBadListFilter)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	per := f.PerPage
	switch {
	case per <= 0:
		per = ListPerPageDefault
	case per > ListPerPageMax:
		per = ListPerPageMax
	}
	page := min(max(f.Page, 1), ListPageMax)
	countArgs := len(args)
	sql := `SELECT m.*, count(*) OVER ()::int AS total FROM (` + listRowsSQL + `) m` + where +
		` ORDER BY ` + order + ` LIMIT ` + arg(per) + ` OFFSET ` + arg((page-1)*per)
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[listRow], sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("leadstore: lead list: %w", err)
	}
	out := make([]ListRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.row())
	}
	if len(rows) > 0 {
		return out, rows[0].Total, nil
	}
	if page == 1 {
		return out, 0, nil
	}
	var total int
	if err := q.db.QueryRow(ctx, `SELECT count(*)::int FROM (`+listRowsSQL+`) m`+where, args[:countArgs]...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("leadstore: lead list: count: %w", err)
	}
	return out, total, nil
}

// profileOrEmpty is s, or an empty profile (no points) when s is blank.
func profileOrEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// LeadDetail returns lead leadID in full, its message bodies included; a selftest lead or
// a missing one is ErrLeadNotFound. Its list row carries no profile: Points, Reasons and
// SmallSite are zero until Weigh fills them.
func (q Queries) LeadDetail(ctx context.Context, leadID int64) (Detail, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[listRow],
		`SELECT m.*, 1 AS total FROM (`+listRowsSQL+`) m WHERE m.lead_id = $3`, "{}", 0, leadID)
	if err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: %w", err)
	}
	if len(rows) == 0 {
		return Detail{}, fmt.Errorf("leadstore: lead detail %d: %w", leadID, ErrLeadNotFound)
	}
	r := rows[0]
	d := Detail{
		ListRow: r.row(), ListRank: r.ListRank, Origin: r.Origin,
		score: Candidate{
			LeadID: r.LeadID, OrgID: r.OrgID, Domain: r.Domain, Country: r.Country, Kind: r.Kind,
			Channel: r.Channel, ProbeID: r.ProbeID, AgencyFindingID: r.AgencyFindingID,
			ListRank: r.ListRank, Major: r.ProbeMajor, ExactVersion: r.ProbeExact,
		},
	}
	if d.Signals, err = collect(ctx, q.db, pgx.RowToStructByPos[DetailSignal],
		`SELECT coalesce(e.v->>'name', ''), coalesce(e.v->>'value', ''), h.id, coalesce(h.url, ''),
		   coalesce(h.final_url, ''), coalesce(h.status, 0), h.fetched_at
		 FROM lead l JOIN finding f ON f.id = l.finding_id
		 CROSS JOIN LATERAL jsonb_array_elements(f.evidence) WITH ORDINALITY AS e(v, i)
		 LEFT JOIN http_fetch h ON jsonb_typeof(e.v->'fetch_id') = 'number' AND h.id = (e.v->>'fetch_id')::bigint
		 WHERE l.id = $1 ORDER BY e.i`, leadID); err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: signals: %w", err)
	}
	if d.Routes, err = collect(ctx, q.db, pgx.RowToStructByPos[DetailRoute],
		`SELECT r.kind, r.value, r.mx_ok, r.fetch_id, coalesce(nullif(h.final_url, ''), h.url), r.found_at
		 FROM contact_route r JOIN http_fetch h ON h.id = r.fetch_id
		 WHERE r.org_id = $1 ORDER BY r.kind, r.id`, r.OrgID); err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: routes: %w", err)
	}
	if d.Facts, err = collect(ctx, q.db, pgx.RowToStructByPos[DetailFact],
		`SELECT x.kind, x.value, x.fetch_id, coalesce(nullif(h.final_url, ''), h.url), x.found_at
		 FROM org_fact x JOIN http_fetch h ON h.id = x.fetch_id
		 WHERE x.org_id = $1 ORDER BY x.kind, x.id`, r.OrgID); err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: facts: %w", err)
	}
	if r.AgencyFindingID != nil {
		ag, err := collect(ctx, q.db, pgx.RowToStructByPos[DetailAgency],
			`SELECT url, phrase, fetch_id FROM agency_finding WHERE id = $1`, *r.AgencyFindingID)
		if err != nil {
			return Detail{}, fmt.Errorf("leadstore: lead detail: agency: %w", err)
		}
		if len(ag) > 0 {
			d.Agency = &ag[0]
		}
	}
	if d.Events, err = collect(ctx, q.db, pgx.RowToStructByPos[DetailEvent],
		`SELECT state, reason, actor, at FROM lead_event WHERE lead_id = $1 ORDER BY id`, leadID); err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: events: %w", err)
	}
	pr, err := collect(ctx, q.db, pgx.RowToStructByPos[DetailPriority],
		`SELECT action, until, actor, at FROM lead_priority WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, leadID)
	if err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: priority: %w", err)
	}
	if len(pr) > 0 {
		d.Priority = &pr[0]
	}
	if d.Messages, err = collect(ctx, q.db, pgx.RowToStructByPos[DetailMessage],
		`SELECT m.id, m.batch_id, b.shadow, m.step, m.channel, m.state, m.due_at, m.sent_at, m.subject, m.body_text
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE m.lead_id = $1 ORDER BY m.id`, leadID); err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: messages: %w", err)
	}
	if d.RelevanceHistory, err = collect(ctx, q.db, pgx.RowToStructByPos[DetailRelevance],
		`SELECT score, band, question, decider, model, created_at FROM lead_relevance
		 WHERE lead_id = $1 ORDER BY id DESC LIMIT 5`, leadID); err != nil {
		return Detail{}, fmt.Errorf("leadstore: lead detail: relevance: %w", err)
	}
	return d, nil
}

// Weigh fills d's Reasons and Points from p (ScoreReasons over the lead's probe, the same
// as LeadList's points under p) and SmallSite from skipRankOver (0: off), as LeadList does.
func (d *Detail) Weigh(p leadconfig.Profile, skipRankOver int) {
	d.Reasons = ScoreReasons(p, d.score)
	d.Points = 0
	for _, r := range d.Reasons {
		d.Points += r.Points
	}
	d.SmallSite = d.score.ProbeID != nil && skipRankOver > 0 && (d.ListRank == nil || *d.ListRank > skipRankOver)
}
