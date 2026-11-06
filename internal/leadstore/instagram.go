package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/enrich"
)

// Instagram signals on café leads (migration 00031): what a café's public profile shows, read
// into a capture file by the operator's own browser and imported here. Every table is
// append-only; the latest row of a handle decides its match and its review status. Handles are
// stored lower case without their @ (enrich.InstagramHandle). Nothing here writes a lead or an
// org: signals inform, they never move a lead. The one exception is the place type, a separate
// table: SyncIGPlaceTypes (place_type_ig.go) writes Instagram's place call on a strongly matched
// profile as the lead's instagram place type row, and nothing else here writes one.

// The outcomes of a profile capture (ig_profile_outcome_check).
const (
	IGOutcomeOK      = "ok"
	IGOutcomeMissing = "missing" // the handle no longer exists
)

// How a profile was reached (ig_profile_source_check).
const (
	IGSourceLead      = "lead"
	IGSourceFollowing = "following"
	IGSourceSearch    = "search"
	IGSourceManual    = "manual"
)

// The match tiers (ig_match_tier_check). Handle and domain are strong: signals attach. Name is
// weak: the profile waits in the review list. Operator is the operator's own call, and none
// matches nothing.
const (
	IGTierHandle   = "handle"
	IGTierDomain   = "domain"
	IGTierName     = "name"
	IGTierOperator = "operator"
	IGTierNone     = "none"
)

// The questions a profile is judged on (ig_judgment_question_check).
const (
	IGPlace   = "ig_place"
	IGLoyalty = "ig_loyalty"
	IGVendor  = "ig_vendor"
)

// The ig_place choices that decide a lead's place type (SyncIGPlaceTypes); every other choice
// is silence. They live here, not in leadrate, which asks the question: leadrate imports
// leadstore, never the other way round, and leadrate's own constants are defined from these.
const (
	IGPlaceCafeSeating      = "cafe_seating"
	IGPlaceRoasteryOrOnline = "roastery_or_online"
)

// The sources of a judgment (ig_judgment_source_check). A rule row is deterministic (a vendor
// in a link or a tag, or no text to judge) and outranks Jev's on the same profile.
const (
	IGFromJev  = "jev"
	IGFromRule = "rule"
)

// IGFakeModel is the model of a Jev-source judgment the keyless fake decider wrote (ai.FakeName):
// a test or an unattended run's stand-in, never an answer about a café. A real Jev answer on any
// capture of a handle outranks every fake one (igRealJudgedSQL), and the importer never asks the
// fake about a handle Jev has answered.
const IGFakeModel = "fake"

// igFakeRowSQL is true for a judgment j the fake decider wrote.
const igFakeRowSQL = `(j.source = '` + IGFromJev + `' AND j.model IS NOT DISTINCT FROM '` + IGFakeModel + `')`

// igRealJudgedSQL is true when some capture of handle (a column or parameter) holds a real Jev
// answer: a Jev-source judgment of a model other than the fake's.
func igRealJudgedSQL(handle string) string {
	return `EXISTS (SELECT 1 FROM ig_judgment rj JOIN ig_profile rp ON rp.id = rj.profile_id
	   WHERE rp.handle = ` + handle + ` AND rj.source = '` + IGFromJev + `' AND rj.model IS DISTINCT FROM '` + IGFakeModel + `')`
}

// The review list's statuses (ig_review_status_status_check).
const (
	IGReviewNew       = "new"
	IGReviewDismissed = "dismissed"
	IGReviewMatched   = "matched"
	IGReviewWanted    = "wanted" // the operator wants it as a lead; making the lead is a later step
)

// IGHintFollowedByVendor is a handle a loyalty vendor's own account follows; the hint's value
// is the vendor id. It never counts as running a scheme.
const IGHintFollowedByVendor = "followed_by_vendor"

// IGMaxCaptions is the most captions one capture holds (ig_profile_captions_check).
const IGMaxCaptions = 12

// ErrBadIG is returned for an Instagram row outside its lists: a value that is not a handle,
// an unknown source, tier, question, status or kind, a hash that is not 32 bytes, too many
// captions, or a match whose lead and candidates do not fit its tier.
var ErrBadIG = errors.New("leadstore: not a valid Instagram row")

// igHandle is h as stored, or ErrBadIG.
func igHandle(h string) (string, error) {
	out, ok := enrich.InstagramHandle(h)
	if !ok {
		return "", fmt.Errorf("%w: %q is not a handle", ErrBadIG, h)
	}
	return out, nil
}

// igHandles is every handle in hs as stored, once each; a value that is not a handle is left out.
func igHandles(hs []string) []string {
	out := []string{}
	for _, h := range hs {
		if n, ok := enrich.InstagramHandle(h); ok && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// IGProfile is one capture of one profile (an ig_profile row). Text the capture did not give
// is empty, a count it did not give nil.
type IGProfile struct {
	ID           int64     `db:"id"`
	Handle       string    `db:"handle"`
	Outcome      string    `db:"outcome"` // IGOutcomeOK or IGOutcomeMissing; empty writes ok
	IGUserID     string    `db:"ig_user_id"`
	FullName     string    `db:"full_name"`
	Category     string    `db:"category"`
	Bio          string    `db:"bio"`
	ExternalURL  string    `db:"external_url"`
	Followers    *int      `db:"followers"`
	Following    *int      `db:"following"`
	Posts        *int      `db:"posts"`
	IsPrivate    *bool     `db:"is_private"`
	Captions     []string  `db:"captions"`
	Source       string    `db:"source"`         // IGSourceLead, IGSourceFollowing, IGSourceSearch or IGSourceManual
	SourceOf     string    `db:"source_of"`      // the list owner's handle for IGSourceFollowing, else empty
	SourceLeadID int64     `db:"source_lead_id"` // the capture's lead hint, for the report only; 0 for none
	CaptureSHA   []byte    `db:"capture_sha256"` // SHA-256 of the capture line's canonical JSON
	CapturedAt   time.Time `db:"captured_at"`
	ImportedAt   time.Time `db:"imported_at"`
}

const igProfileColumns = `p.id, p.handle, p.outcome, coalesce(p.ig_user_id, '') AS ig_user_id,
	coalesce(p.full_name, '') AS full_name, coalesce(p.category, '') AS category, coalesce(p.bio, '') AS bio,
	coalesce(p.external_url, '') AS external_url, p.followers, p.following, p.posts, p.is_private, p.captions,
	p.source, coalesce(p.source_of, '') AS source_of, coalesce(p.source_lead_id, 0) AS source_lead_id,
	p.capture_sha256, p.captured_at, p.imported_at`

// InsertIGProfile stores one capture of a profile and returns its id. A capture already stored
// (the same handle and CaptureSHA) is not stored twice: its id comes back with isNew false.
// ID and ImportedAt are ignored.
func (q Queries) InsertIGProfile(ctx context.Context, p IGProfile) (id int64, isNew bool, err error) {
	handle, err := igHandle(p.Handle)
	if err != nil {
		return 0, false, fmt.Errorf("leadstore: insert ig profile: %w", err)
	}
	outcome := p.Outcome
	if outcome == "" {
		outcome = IGOutcomeOK
	}
	sourceOf := ""
	if p.SourceOf != "" {
		if sourceOf, err = igHandle(p.SourceOf); err != nil {
			return 0, false, fmt.Errorf("leadstore: insert ig profile %s: source of: %w", handle, err)
		}
	}
	switch {
	case outcome != IGOutcomeOK && outcome != IGOutcomeMissing,
		!slices.Contains([]string{IGSourceLead, IGSourceFollowing, IGSourceSearch, IGSourceManual}, p.Source),
		(p.Source == IGSourceFollowing) != (sourceOf != ""),
		len(p.CaptureSHA) != 32, len(p.Captions) > IGMaxCaptions, p.CapturedAt.IsZero():
		return 0, false, fmt.Errorf("leadstore: insert ig profile %s: %w: outcome %q, source %q of %q, %d-byte hash, %d captions",
			handle, ErrBadIG, outcome, p.Source, sourceOf, len(p.CaptureSHA), len(p.Captions))
	}
	captions := p.Captions
	if captions == nil {
		captions = []string{}
	}
	cj, err := json.Marshal(captions)
	if err != nil {
		return 0, false, fmt.Errorf("leadstore: insert ig profile %s: captions: %w", handle, err)
	}
	// A conflict leaves ins empty; the second branch then reads the row already there (the
	// statement's snapshot never sees ins's own row).
	err = q.db.QueryRow(ctx,
		`WITH ins AS (
		   INSERT INTO ig_profile (handle, outcome, ig_user_id, full_name, category, bio, external_url, followers,
		     following, posts, is_private, captions, source, source_of, source_lead_id, capture_sha256, captured_at)
		   VALUES ($1, $2, nullif($3, ''), nullif($4, ''), nullif($5, ''), nullif($6, ''), nullif($7, ''), $8, $9, $10,
		     $11, $12::jsonb, $13, nullif($14, ''), nullif($15::bigint, 0), $16, $17)
		   ON CONFLICT (handle, capture_sha256) DO NOTHING
		   RETURNING id)
		 SELECT id, true FROM ins
		 UNION ALL
		 SELECT id, false FROM ig_profile WHERE handle = $1 AND capture_sha256 = $16 AND NOT EXISTS (SELECT 1 FROM ins)`,
		handle, outcome, p.IGUserID, p.FullName, p.Category, p.Bio, p.ExternalURL, p.Followers, p.Following, p.Posts,
		p.IsPrivate, string(cj), p.Source, sourceOf, p.SourceLeadID, p.CaptureSHA, p.CapturedAt).Scan(&id, &isNew)
	if err != nil {
		return 0, false, fmt.Errorf("leadstore: insert ig profile %s: %w", handle, err)
	}
	return id, isNew, nil
}

// The directions of a follow list (ig_follow_direction_check, 00033): IGFollowing is the
// list owner following each handle (every row before 00033), IGFollowers each handle following
// the list owner.
const (
	IGFollowing = "following"
	IGFollowers = "followers"
)

// IGFollowRow is one account kept from a following or followers list. FullName is written only for an
// account that is matched or lands in the review list (Decision 9); leave it empty otherwise.
type IGFollowRow struct {
	Handle   string
	FullName string
}

// InsertIGFollows stores one capture (sha, capturedAt) of listOwner's list in direction
// (IGFollowing: the accounts listOwner follows; IGFollowers: the accounts following it), and
// returns how many rows were new: a re-import of the same capture stores nothing twice, and
// one account on both of an owner's lists is a row in each. A row whose handle is not a
// handle, or a direction outside the two, fails the whole call with ErrBadIG.
func (q Queries) InsertIGFollows(ctx context.Context, listOwner, direction string, capturedAt time.Time, sha []byte, rows []IGFollowRow) (int64, error) {
	owner, err := igHandle(listOwner)
	if err != nil {
		return 0, fmt.Errorf("leadstore: insert ig follows: %w", err)
	}
	if len(sha) != 32 || capturedAt.IsZero() || (direction != IGFollowing && direction != IGFollowers) {
		return 0, fmt.Errorf("leadstore: insert ig follows of %s: %w: %d-byte hash, direction %q", owner, ErrBadIG, len(sha), direction)
	}
	handles := make([]string, len(rows))
	names := make([]string, len(rows))
	for i, r := range rows {
		if handles[i], err = igHandle(r.Handle); err != nil {
			return 0, fmt.Errorf("leadstore: insert ig follows of %s: %w", owner, err)
		}
		names[i] = r.FullName
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO ig_follow (list_owner, direction, handle, full_name, captured_at, capture_sha256)
		 SELECT $1, $6, u.h, nullif(u.n, ''), $2, $3 FROM unnest($4::text[], $5::text[]) AS u(h, n)
		 ON CONFLICT (list_owner, direction, handle, capture_sha256) DO NOTHING`,
		owner, capturedAt, sha, handles, names, direction)
	if err != nil {
		return 0, fmt.Errorf("leadstore: insert ig follows of %s: %w", owner, err)
	}
	return tag.RowsAffected(), nil
}

// IGMatch is what a handle matches (an ig_match row). LeadID 0 is no lead: tier none, an
// operator's unmatch, or a tie at the best tier, whose leads are then in Candidates (two or
// more). A single weak (name) candidate is LeadID itself.
type IGMatch struct {
	ID         int64     `db:"id"`
	Handle     string    `db:"handle"`
	LeadID     int64     `db:"lead_id"`
	Tier       string    `db:"tier"`
	Candidates []int64   `db:"candidate_ids"`
	Detail     string    `db:"detail"`
	Actor      string    `db:"actor"`
	CreatedAt  time.Time `db:"created_at"`
}

// Strong reports whether m attaches a profile's signals to its lead: a handle, domain or
// operator match that names one.
func (m IGMatch) Strong() bool {
	return m.LeadID != 0 && (m.Tier == IGTierHandle || m.Tier == IGTierDomain || m.Tier == IGTierOperator)
}

const igMatchColumns = `m.id, m.handle, coalesce(m.lead_id, 0) AS lead_id, m.tier, m.candidate_ids, m.detail, m.actor, m.created_at`

// SetIGMatch appends m as its handle's match, unless the handle's latest match already says
// the same (the same lead, tier and candidates): changed reports whether a row was written.
// Candidates are stored sorted, once each. IGTierOperator is the operator's (actor gremy) alone.
func (q Queries) SetIGMatch(ctx context.Context, m IGMatch) (changed bool, err error) {
	handle, err := igHandle(m.Handle)
	if err != nil {
		return false, fmt.Errorf("leadstore: set ig match: %w", err)
	}
	cands := slices.Clone(m.Candidates)
	slices.Sort(cands)
	cands = slices.Compact(cands)
	if cands == nil {
		cands = []int64{}
	}
	switch {
	case m.Actor != ActorGremy && m.Actor != ActorSystem:
		return false, fmt.Errorf("leadstore: set ig match %s: %w: %q", handle, ErrUnknownActor, m.Actor)
	case !slices.Contains([]string{IGTierHandle, IGTierDomain, IGTierName, IGTierOperator, IGTierNone}, m.Tier),
		m.Tier == IGTierOperator && m.Actor != ActorGremy,
		m.Tier == IGTierNone && (m.LeadID != 0 || len(cands) > 0),
		m.Tier == IGTierOperator && len(cands) > 0,
		m.Tier != IGTierNone && m.Tier != IGTierOperator && (m.LeadID != 0) == (len(cands) >= 2),
		len(cands) == 1, slices.Contains(cands, 0), m.LeadID < 0:
		return false, fmt.Errorf("leadstore: set ig match %s: %w: tier %q by %q, lead %d, %d candidates",
			handle, ErrBadIG, m.Tier, m.Actor, m.LeadID, len(cands))
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO ig_match (handle, lead_id, tier, candidate_ids, detail, actor)
		 SELECT $1, nullif($2::bigint, 0), $3, $4::bigint[], $5, $6
		 WHERE NOT EXISTS (
		   SELECT 1 FROM (SELECT lead_id, tier, candidate_ids FROM ig_match WHERE handle = $1 ORDER BY id DESC LIMIT 1) x
		   WHERE x.lead_id IS NOT DISTINCT FROM nullif($2::bigint, 0) AND x.tier = $3 AND x.candidate_ids = $4::bigint[])`,
		handle, m.LeadID, m.Tier, cands, m.Detail, m.Actor)
	if err != nil {
		return false, fmt.Errorf("leadstore: set ig match %s: %w", handle, err)
	}
	return tag.RowsAffected() == 1, nil
}

// IGMatches reads the latest match of each handle in handles, keyed by stored handle; a handle
// never matched is not in the map, nor a value that is not a handle.
func (q Queries) IGMatches(ctx context.Context, handles []string) (map[string]IGMatch, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[IGMatch],
		`SELECT DISTINCT ON (m.handle) `+igMatchColumns+`
		 FROM ig_match m WHERE m.handle = ANY($1::text[]) ORDER BY m.handle, m.id DESC`, igHandles(handles))
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig matches: %w", err)
	}
	out := make(map[string]IGMatch, len(rows))
	for _, r := range rows {
		out[r.Handle] = r
	}
	return out, nil
}

// IGJudgment is one answer on one profile (an ig_judgment row). Choice is the effective
// choice; RawChoice and Confidence are what Jev gave (empty and nil for a rule).
type IGJudgment struct {
	ID         int64     `db:"id"`
	ProfileID  int64     `db:"profile_id"`
	Question   string    `db:"question"`
	Choice     string    `db:"choice"`
	RawChoice  string    `db:"raw_choice"`
	Confidence *float64  `db:"confidence"`
	Source     string    `db:"source"`       // IGFromJev or IGFromRule
	Model      string    `db:"model"`        // the model Jev ran; empty for a rule
	Detail     string    `db:"detail"`       // for a rule, how it was seen
	InputSHA   []byte    `db:"input_sha256"` // what Jev was asked; nil for a rule
	CreatedAt  time.Time `db:"created_at"`
}

const igJudgmentColumns = `j.id, j.profile_id, j.question, j.choice, coalesce(j.raw_choice, '') AS raw_choice,
	j.confidence::float8 AS confidence, j.source, coalesce(j.model, '') AS model, j.detail, j.input_sha256, j.created_at`

// AddIGJudgment appends j and returns its id. A Jev row needs its RawChoice and the 32-byte
// SHA-256 of its input; a rule row has neither, nor a confidence.
func (q Queries) AddIGJudgment(ctx context.Context, j IGJudgment) (int64, error) {
	switch {
	case !slices.Contains([]string{IGPlace, IGLoyalty, IGVendor}, j.Question),
		j.Source != IGFromJev && j.Source != IGFromRule,
		j.Source == IGFromJev && (len(j.InputSHA) != 32 || j.RawChoice == ""),
		j.Source == IGFromRule && (len(j.InputSHA) != 0 || j.RawChoice != "" || j.Confidence != nil):
		return 0, fmt.Errorf("leadstore: add ig judgment on profile %d: %w: %q from %q with a %d-byte hash",
			j.ProfileID, ErrBadIG, j.Question, j.Source, len(j.InputSHA))
	}
	var sha []byte
	if len(j.InputSHA) > 0 {
		sha = j.InputSHA
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO ig_judgment (profile_id, question, choice, raw_choice, confidence, source, model, detail, input_sha256)
		 VALUES ($1, $2, $3, nullif($4, ''), $5, $6, nullif($7, ''), $8, $9) RETURNING id`,
		j.ProfileID, j.Question, j.Choice, j.RawChoice, j.Confidence, j.Source, j.Model, j.Detail, sha).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: add ig judgment on profile %d: %w", j.ProfileID, err)
	}
	return id, nil
}

// IGJudgmentHash is the input hash of the latest Jev-source answer on question for any capture
// of handle, nil when none: of the fake decider's answers when fake, else of real Jev's only, so a
// fake run in between never makes a real run ask again. It is read by handle, never by profile:
// each capture is a new profile row, so a recapture with unchanged text compares equal and costs
// no call.
func (q Queries) IGJudgmentHash(ctx context.Context, handle, question string, fake bool) ([]byte, error) {
	h, err := igHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig judgment hash: %w", err)
	}
	var sha []byte
	err = q.db.QueryRow(ctx,
		`SELECT j.input_sha256 FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id
		 WHERE p.handle = $1 AND j.question = $2 AND j.source = '`+IGFromJev+`' AND `+igFakeRowSQL+` = $3
		 ORDER BY j.id DESC LIMIT 1`, h, question, fake).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig judgment hash %s %s: %w", h, question, err)
	}
	return sha, nil
}

// IGRealJudged reports whether any capture of handle holds a real Jev answer (not the fake's).
func (q Queries) IGRealJudged(ctx context.Context, handle string) (bool, error) {
	h, err := igHandle(handle)
	if err != nil {
		return false, fmt.Errorf("leadstore: ig real judged: %w", err)
	}
	var ok bool
	if err := q.db.QueryRow(ctx, `SELECT `+igRealJudgedSQL("$1"), h).Scan(&ok); err != nil {
		return false, fmt.Errorf("leadstore: ig real judged %s: %w", h, err)
	}
	return ok, nil
}

// IGStandingJudgments reads the standing judgment of each handle in handles on each question:
// from the newest capture holding any answer on that question, a rule row before Jev's, then
// the latest. A recapture Jev was not asked about (equal hash) leaves the older answer standing;
// a rule is written against each new capture, so it never outlives the text it read. The fake
// decider's answers never stand for a handle real Jev has answered. The lead card, the review
// list and the importer's place calls all read it.
func (q Queries) IGStandingJudgments(ctx context.Context, handles []string) (map[string]map[string]IGJudgment, error) {
	type row struct {
		Handle string `db:"handle"`
		IGJudgment
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT DISTINCT ON (p.handle, j.question) p.handle, `+igJudgmentColumns+`
		 FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id
		 WHERE p.handle = ANY($1::text[]) AND NOT (`+igFakeRowSQL+` AND `+igRealJudgedSQL("p.handle")+`)
		 ORDER BY p.handle, j.question, j.profile_id DESC, (j.source = '`+IGFromRule+`') DESC, j.id DESC`, handles)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]IGJudgment{}
	for _, r := range rows {
		if out[r.Handle] == nil {
			out[r.Handle] = map[string]IGJudgment{}
		}
		out[r.Handle][r.Question] = r.IGJudgment
	}
	return out, nil
}

// igLatestProfiles reads the latest capture of each handle in handles.
func (q Queries) igLatestProfiles(ctx context.Context, handles []string) (map[string]IGProfile, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[IGProfile],
		`SELECT DISTINCT ON (p.handle) `+igProfileColumns+`
		 FROM ig_profile p WHERE p.handle = ANY($1::text[]) ORDER BY p.handle, p.id DESC`, handles)
	if err != nil {
		return nil, err
	}
	out := make(map[string]IGProfile, len(rows))
	for _, r := range rows {
		out[r.Handle] = r
	}
	return out, nil
}

// IGHint is one weak signal about a handle (an ig_signal_hint row).
type IGHint struct {
	Handle     string    `db:"handle"`
	Kind       string    `db:"kind"`  // IGHintFollowedByVendor
	Value      string    `db:"value"` // the vendor id
	CapturedAt time.Time `db:"captured_at"`
}

// AddIGHint records a hint about handle, once per handle, kind and value: inserted reports
// whether it was new.
func (q Queries) AddIGHint(ctx context.Context, h IGHint) (inserted bool, err error) {
	handle, err := igHandle(h.Handle)
	if err != nil {
		return false, fmt.Errorf("leadstore: add ig hint: %w", err)
	}
	if h.Kind != IGHintFollowedByVendor || h.CapturedAt.IsZero() {
		return false, fmt.Errorf("leadstore: add ig hint %s: %w: kind %q", handle, ErrBadIG, h.Kind)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO ig_signal_hint (handle, kind, value, captured_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (handle, kind, value) DO NOTHING`, handle, h.Kind, h.Value, h.CapturedAt)
	if err != nil {
		return false, fmt.Errorf("leadstore: add ig hint %s: %w", handle, err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetIGReviewStatus appends status for handle, unless its latest status is already status:
// changed reports whether a row was written. note is the operator's words, if any.
func (q Queries) SetIGReviewStatus(ctx context.Context, handle, status, note, actor string) (changed bool, err error) {
	h, err := igHandle(handle)
	if err != nil {
		return false, fmt.Errorf("leadstore: set ig review status: %w", err)
	}
	switch {
	case actor != ActorGremy && actor != ActorSystem:
		return false, fmt.Errorf("leadstore: set ig review status %s: %w: %q", h, ErrUnknownActor, actor)
	case !slices.Contains([]string{IGReviewNew, IGReviewDismissed, IGReviewMatched, IGReviewWanted}, status):
		return false, fmt.Errorf("leadstore: set ig review status %s: %w: %q", h, ErrBadIG, status)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO ig_review_status (handle, status, note, actor)
		 SELECT $1, $2, $3, $4
		 WHERE (SELECT status FROM ig_review_status WHERE handle = $1 ORDER BY id DESC LIMIT 1) IS DISTINCT FROM $2`,
		h, status, strings.TrimSpace(note), actor)
	if err != nil {
		return false, fmt.Errorf("leadstore: set ig review status %s: %w", h, err)
	}
	return tag.RowsAffected() == 1, nil
}

// QueueIGReview puts handle on the review list as new, by the system, only when it has never
// had a review status: a handle the operator dismissed, matched or wanted keeps his call, and
// a new one is never queued twice. queued reports whether a row was written.
func (q Queries) QueueIGReview(ctx context.Context, handle string) (queued bool, err error) {
	h, err := igHandle(handle)
	if err != nil {
		return false, fmt.Errorf("leadstore: queue ig review: %w", err)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO ig_review_status (handle, status, actor)
		 SELECT $1, '`+IGReviewNew+`', '`+ActorSystem+`'
		 WHERE NOT EXISTS (SELECT 1 FROM ig_review_status WHERE handle = $1)`, h)
	if err != nil {
		return false, fmt.Errorf("leadstore: queue ig review %s: %w", h, err)
	}
	return tag.RowsAffected() == 1, nil
}

// IGReviewItem is one handle on the review list.
type IGReviewItem struct {
	Handle   string
	Status   string // the latest status
	Note     string
	StatusAt time.Time
	// Profile is the latest capture; nil for a following-list account never read.
	Profile *IGProfile
	// FollowName is the name its latest following-list row kept; empty when none did.
	FollowName string
	// Match is the latest match; nil when never matched.
	Match *IGMatch
	// Candidates are the leads it may be: the weak match's lead, or the tied candidates.
	Candidates []int64
	// Place, Loyalty and Vendor are the standing ig_place, ig_loyalty and ig_vendor choices
	// (IGStandingJudgments, as the lead card reads them); empty when never judged.
	Place, Loyalty, Vendor string
}

// IGReview reads the handles whose latest review status is status (empty: every status), by
// handle, each with its latest capture, kept name, match, candidates and place call.
func (q Queries) IGReview(ctx context.Context, status string) ([]IGReviewItem, error) {
	type st struct {
		Handle    string    `db:"handle"`
		Status    string    `db:"status"`
		Note      string    `db:"note"`
		CreatedAt time.Time `db:"created_at"`
	}
	sts, err := collect(ctx, q.db, pgx.RowToStructByName[st],
		`SELECT s.handle, s.status, s.note, s.created_at FROM (
		   SELECT DISTINCT ON (handle) handle, status, note, created_at FROM ig_review_status ORDER BY handle, id DESC) s
		 WHERE $1 = '' OR s.status = $1 ORDER BY s.handle`, status)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig review: %w", err)
	}
	handles := make([]string, len(sts))
	for i, s := range sts {
		handles[i] = s.Handle
	}
	profiles, err := q.igLatestProfiles(ctx, handles)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig review: profiles: %w", err)
	}
	matches, err := q.IGMatches(ctx, handles)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig review: %w", err)
	}
	judgments, err := q.IGStandingJudgments(ctx, handles)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig review: judgments: %w", err)
	}
	type fn struct {
		Handle   string `db:"handle"`
		FullName string `db:"full_name"`
	}
	names, err := collect(ctx, q.db, pgx.RowToStructByName[fn],
		`SELECT DISTINCT ON (handle) handle, full_name FROM ig_follow
		 WHERE handle = ANY($1::text[]) AND full_name IS NOT NULL ORDER BY handle, id DESC`, handles)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig review: names: %w", err)
	}
	nameOf := make(map[string]string, len(names))
	for _, n := range names {
		nameOf[n.Handle] = n.FullName
	}
	out := make([]IGReviewItem, 0, len(sts))
	for _, s := range sts {
		it := IGReviewItem{Handle: s.Handle, Status: s.Status, Note: s.Note, StatusAt: s.CreatedAt,
			FollowName: nameOf[s.Handle], Place: judgments[s.Handle][IGPlace].Choice,
			Loyalty: judgments[s.Handle][IGLoyalty].Choice, Vendor: judgments[s.Handle][IGVendor].Choice}
		if p, ok := profiles[s.Handle]; ok {
			it.Profile = &p
		}
		if m, ok := matches[s.Handle]; ok {
			it.Match = &m
			it.Candidates = m.Candidates
			if m.LeadID != 0 {
				it.Candidates = []int64{m.LeadID}
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// IGLead is what Instagram says about one lead.
type IGLead struct {
	Match IGMatch // the strong or operator match naming the lead
	// Profile is the handle's latest capture; nil for a handle never read (an operator's match
	// of a following-list account).
	Profile *IGProfile
	// Judgments are the standing answers by question (IGPlace, IGLoyalty, IGVendor), a rule's
	// before Jev's.
	Judgments map[string]IGJudgment
	Hints     []IGHint
}

// igLeadHandleSQL is the ig_match row that gives the lead l its Instagram handle, with no row
// when there is none: of the handles whose latest match is a strong or operator one naming l
// (IGMatch.Strong), the most recently matched. It reads l.id, so it runs as a LATERAL subquery
// beside a lead. IGForLead (the lead card) and SyncIGPlaceTypes (the place type) both read it,
// so the card and the type always name the same handle.
const igLeadHandleSQL = `SELECT x.* FROM (
	   SELECT DISTINCT ON (handle) * FROM ig_match
	   WHERE handle IN (SELECT handle FROM ig_match WHERE lead_id = l.id) ORDER BY handle, id DESC) x
	 WHERE x.lead_id = l.id AND x.tier IN ('` + IGTierHandle + `', '` + IGTierDomain + `', '` + IGTierOperator + `')
	 ORDER BY x.id DESC LIMIT 1`

// IGForLead reads what Instagram says about lead leadID: of the handles whose latest match is
// a strong or operator one naming it, the most recently matched (igLeadHandleSQL). nil when
// there is none, so a weak match never shows its signals.
func (q Queries) IGForLead(ctx context.Context, leadID int64) (*IGLead, error) {
	ms, err := collect(ctx, q.db, pgx.RowToStructByName[IGMatch],
		`SELECT `+igMatchColumns+` FROM (SELECT $1::bigint AS id) l CROSS JOIN LATERAL (`+igLeadHandleSQL+`) m`, leadID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig for lead %d: %w", leadID, err)
	}
	if len(ms) == 0 {
		return nil, nil
	}
	m := ms[0]
	out := &IGLead{Match: m, Judgments: map[string]IGJudgment{}}
	handles := []string{m.Handle}
	profiles, err := q.igLatestProfiles(ctx, handles)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig for lead %d: profile: %w", leadID, err)
	}
	if p, ok := profiles[m.Handle]; ok {
		out.Profile = &p
	}
	judgments, err := q.IGStandingJudgments(ctx, handles)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig for lead %d: judgments: %w", leadID, err)
	}
	if j := judgments[m.Handle]; j != nil {
		out.Judgments = j
	}
	if out.Hints, err = collect(ctx, q.db, pgx.RowToStructByName[IGHint],
		`SELECT handle, kind, value, captured_at FROM ig_signal_hint WHERE handle = $1 ORDER BY id`, m.Handle); err != nil {
		return nil, fmt.Errorf("leadstore: ig for lead %d: hints: %w", leadID, err)
	}
	return out, nil
}

// IGLeadKey is a café lead as the Instagram matcher reads it.
type IGLeadKey struct {
	LeadID   int64  `db:"lead_id"`
	OrgID    int64  `db:"org_id"`
	OrgName  string `db:"org_name"`
	SiteName string `db:"site_name"` // the site's own name from the lead's probe; empty without one
	// PlaceName and Town are the org's latest map listing's name and locality; empty without one.
	PlaceName string `db:"place_name"`
	Town      string `db:"town"`
	// Domain is the org's own website's registrable domain; empty for an org with no website
	// of its own (a map listing's PlaceOrgPrefix key), which never matches by domain.
	Domain string `db:"domain"`
	// Handles are the Instagram handles the lead already holds, sorted, once each: its org's
	// live instagram routes and social facts, the instagram signals of the lead's probe and its
	// places' socials, all through enrich.InstagramHandle. A loyalty or ordering vendor's own
	// account (signals.yaml) is never one: a café linking to its vendor does not hold it.
	Handles []string `db:"-"`
	// TrustedHandles are those of Handles the lead holds from the operator (a route he typed or
	// claimed) or its map listing (a place's socials, a route or fact read from a place), not
	// only from a website: a site can link someone else's account (a theme author's, another
	// café's), so a handle a site alone gave needs more to attach (the importer's name check).
	TrustedHandles []string `db:"-"`
	// PlaceType is the lead's effective place type (effectivePlaceTypeSQL).
	PlaceType string `db:"place_type"`
	// JevPlaceType is the place type of the lead's latest Jev row (unclear included); empty
	// when Jev was never asked.
	JevPlaceType string `db:"jev_place_type"`

	RawHandles []string `db:"raw_handles"` // the values Handles is read from
	RawTrusted []string `db:"raw_trusted"` // the values TrustedHandles is read from
}

// igLeadKeySQL is every café lead (any state, never a selftest one) whose org's domain is not
// suppressed (an erased org is never matched again), as IGLeadKey's columns. A retired route
// or fact is not a handle the lead holds.
var igLeadKeySQL = `SELECT l.id AS lead_id, o.id AS org_id, coalesce(o.name, '') AS org_name,
	   coalesce(p.site_name, '') AS site_name, coalesce(pl.name, '') AS place_name, coalesce(pl.locality, '') AS town,
	   CASE WHEN o.domain LIKE ` + sqlLiteral(PlaceOrgPrefix+"%") + ` THEN '' ELSE o.domain END AS domain,
	   ` + effectivePlaceTypeSQL + ` AS place_type,
	   coalesce((SELECT t.place_type FROM lead_place_type t WHERE t.lead_id = l.id AND t.source = ` + sqlLiteral(PlaceFromJev) + `
	             ORDER BY t.id DESC LIMIT 1), '') AS jev_place_type,
	   ARRAY(
	     SELECT r.value FROM contact_route r
	     WHERE r.org_id = o.id AND r.kind = ` + sqlLiteral(RouteInstagram) + `
	       AND ` + contactLatestMarkSQL("route_id", "r.id") + ` IS DISTINCT FROM 'retire'
	     UNION
	     SELECT f.normalized FROM org_fact f
	     WHERE f.org_id = o.id AND f.kind = 'social' AND f.normalized LIKE 'https://instagram.com/%'
	       AND ` + contactLatestMarkSQL("fact_id", "f.id") + ` IS DISTINCT FROM 'retire'
	     UNION
	     SELECT regexp_replace(e.v->>'value', '^[^:]*:[^:]*:[^:]*:', '')
	     FROM jsonb_array_elements(p.signals) AS e(v) WHERE e.v->>'signal' = ` + sqlLiteral(cafe.SigInstagram) + `
	     UNION
	     SELECT s.v FROM place pa CROSS JOIN LATERAL jsonb_array_elements_text(pa.socials) AS s(v) WHERE pa.org_id = o.id
	   ) AS raw_handles,
	   ARRAY(
	     SELECT r.value FROM contact_route r
	     CROSS JOIN LATERAL (SELECT ` + contactLatestMarkSQL("route_id", "r.id") + ` AS mark) m
	     WHERE r.org_id = o.id AND r.kind = ` + sqlLiteral(RouteInstagram) + ` AND m.mark IS DISTINCT FROM 'retire'
	       AND (m.mark = 'claim' OR r.derived_by = ` + sqlLiteral(DerivedByOperator) + ` OR r.place_id IS NOT NULL)
	     UNION
	     SELECT f.normalized FROM org_fact f
	     WHERE f.org_id = o.id AND f.kind = 'social' AND f.normalized LIKE 'https://instagram.com/%' AND f.place_id IS NOT NULL
	       AND ` + contactLatestMarkSQL("fact_id", "f.id") + ` IS DISTINCT FROM 'retire'
	     UNION
	     SELECT s.v FROM place pa CROSS JOIN LATERAL jsonb_array_elements_text(pa.socials) AS s(v) WHERE pa.org_id = o.id
	   ) AS raw_trusted
	 FROM lead l
	 JOIN org o ON o.id = l.org_id
	 LEFT JOIN site_probe p ON p.id = l.probe_id
	 LEFT JOIN LATERAL (SELECT pa.name, pa.locality FROM place pa WHERE pa.org_id = o.id ORDER BY pa.id DESC LIMIT 1) pl ON true
	 WHERE l.kind IN (` + cafeKindsSQL() + `) AND o.kind <> 'selftest' AND NOT ` + orgDomainSuppressedSQL("o")

// IGLeadKeys reads every café lead of campaign campaignID (0: every campaign's) the matcher may
// match, by lead id; see igLeadKeySQL for which.
func (q Queries) IGLeadKeys(ctx context.Context, campaignID int64) ([]IGLeadKey, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[IGLeadKey],
		igLeadKeySQL+` AND ($1::bigint = 0 OR l.campaign_id = $1::bigint) ORDER BY l.id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig lead keys: %w", err)
	}
	vendors := vendorIGHandles()
	own := func(raw []string) []string {
		hs := []string{}
		for _, h := range igHandles(raw) {
			if !vendors[h] {
				hs = append(hs, h)
			}
		}
		slices.Sort(hs)
		return hs
	}
	for i := range rows {
		rows[i].Handles, rows[i].TrustedHandles = own(rows[i].RawHandles), own(rows[i].RawTrusted)
		rows[i].RawHandles, rows[i].RawTrusted = nil, nil
	}
	return rows, nil
}

// IsVendorIGHandle reports whether v (a handle or a profile URL) is a loyalty or ordering
// vendor's own Instagram account (signals.yaml): never a café's route, never its channel.
func IsVendorIGHandle(v string) bool {
	h, ok := enrich.InstagramHandle(v)
	return ok && vendorIGHandles()[h]
}

// IGImportRoutes are the live instagram routes of handle that an Instagram import wrote
// (DerivedByInstagram) and nobody has marked since: the ones an import may take back. A route
// the operator typed or claimed, or one already retired, is never among them.
func (q Queries) IGImportRoutes(ctx context.Context, handle string) ([]Route, error) {
	h, err := igHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig import routes: %w", err)
	}
	routes, err := collect(ctx, q.db, pgx.RowToStructByName[Route],
		`SELECT r.id, r.org_id, r.kind, r.value, coalesce(r.fetch_id, 0) AS fetch_id, r.mx_ok, r.derived_by, r.found_at,
		   coalesce(r.person_id, 0) AS person_id, false AS operator
		 FROM contact_route r
		 WHERE r.kind = `+sqlLiteral(RouteInstagram)+` AND r.value = $1 AND r.derived_by = `+sqlLiteral(DerivedByInstagram)+`
		   AND `+contactLatestMarkSQL("route_id", "r.id")+` IS NULL
		 ORDER BY r.id`, enrich.InstagramURL(h))
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig import routes %s: %w", h, err)
	}
	return routes, nil
}

// vendorIGHandles are the handles of the vendors' own Instagram accounts in signals.yaml
// (a match entry on Instagram's host).
var vendorIGHandles = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	for _, v := range cafe.DefaultConfig().Vendors {
		if v.Kind == cafe.SigInstagram {
			continue
		}
		for _, m := range v.Match {
			if !strings.Contains(m, "/") {
				continue // a bare host is a site, not an account
			}
			if h, ok := enrich.InstagramHandle(m); ok {
				out[h] = true
			}
		}
	}
	return out
})
