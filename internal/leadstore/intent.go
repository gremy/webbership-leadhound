package leadstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/intent"
)

// A café lead's loyalty intent (migration 00033, rules intent@1 in internal/intent). The class
// is derived here from the store, never asked of a model: IntentInputs gathers every signal of
// a café lead in a few bulk reads, intent.Classify decides, and SyncIntent appends a lead_intent
// row only where the derived values changed. The operator's own call (lead_intent_call) is
// stored apart and wins on every read (intentJoinSQL), so a derived row is still written under
// a call and reads again the moment the call is cleared.

// intentSyncLockKey is the transaction-level advisory lock a sync holds from its first read to
// its commit, so two syncs at once never both see the same latest row and write it twice.
const intentSyncLockKey int64 = 0x4c48_494e_544e // "LHINTN"

// intentMaxAge is how far back a sync reads touches: older ones are ignored by intent@1.
const intentMaxAge = 365 * 24 * time.Hour

// intentCallHold is how long an operator's just_chose call holds the lead (D6).
const intentCallHold = "60 days"

// IntentMaxNote is the longest note a call may carry (lead_intent_call_note_check).
const IntentMaxNote = 200

// intentRoasteryReason is the reason intent@1's rule 4 gives a roastery's out, and only that
// rule gives it. lead_intent has no rule column, so the hold reads this reason to tell a
// roastery out (never held by intent: the roastery hold and its Controls switch decide) from an
// out by rules 2-3 (held). Reading the reason, not the place type, keeps a roastery that is
// already a customer (rule 2 fires first) held, and needs no join on the place type in the hot
// queue fragment. TestIntentOwnVendorAndRoastery pins the words against intent.Classify.
const intentRoasteryReason = "a roastery"

var (
	// ErrUnknownIntentClass is returned by SetIntentCall for a class outside intent@1's seven.
	ErrUnknownIntentClass = errors.New("leadstore: not an intent class")
	// ErrBadIntentNote is returned by SetIntentCall for a note over IntentMaxNote characters.
	ErrBadIntentNote = errors.New("leadstore: intent note too long")
	// ErrBadIntentConfig is returned for an IntentConfig missing one of its parts.
	ErrBadIntentConfig = errors.New("leadstore: intent config incomplete")
)

// IntentClasses are intent@1's classes in rank order (shopping first, out last).
var IntentClasses = []string{intent.Shopping, intent.Paper, intent.Switch, intent.Curious, intent.Cold, intent.JustChose, intent.Out}

// IntentConfig is what a sync reads beside the store. igsignal imports leadstore, so the
// Instagram rules arrive as functions the caller builds from igsignal:
//
//	m := igsignal.NewMatcher(ccfg, nil)
//	IntentConfig{Cafe: ccfg,
//	  VendorAccount: func(h string) string { return m.MatchFollow(igsignal.Follow{Handle: h}).VendorAccount },
//	  RuleVendor: func(p IGProfile) string { id, _, _ := igsignal.Vendor(<p as igsignal.Profile>, ccfg); return id },
//	  Prepaid: func(p IGProfile) bool { return igsignal.Prepaid(<p as igsignal.Profile>, ccfg) }}
//
// Cafe must be the signal list WITH the own vendor (Config.WithOwnVendor, as the importer
// builds it): the bare list never sees Webbership, so rule 2 could never fire.
type IntentConfig struct {
	Cafe *cafe.Config
	// VendorAccount is the vendor id whose own Instagram account handle is, "" for none.
	VendorAccount func(handle string) string
	// RuleVendor is the loyalty vendor a profile shows by rule, "" for none.
	RuleVendor func(p IGProfile) string
	// Prepaid reports whether a profile shows a prepaid offer.
	Prepaid func(p IGProfile) bool
}

func (c IntentConfig) check() error {
	if c.Cafe == nil || c.VendorAccount == nil || c.RuleVendor == nil || c.Prepaid == nil {
		return ErrBadIntentConfig
	}
	return nil
}

// loyal reports whether id is a loyalty vendor of the list (the own one included).
func (c IntentConfig) loyal(id string) bool {
	v := c.Cafe.Vendor(id)
	return v != nil && v.Kind == cafe.SigLoyalty
}

// own is the own vendor's id, "" when the list has none.
func (c IntentConfig) own() string {
	for _, v := range c.Cafe.Vendors {
		if v.Own {
			return v.ID
		}
	}
	return ""
}

// intentLeadsSQL is every café lead a sync classifies: never a selftest org and never one
// whose domain is suppressed (an erased org is never read again), as igLeadKeySQL; $1 is the
// campaign, 0 for every campaign.
var intentLeadsSQL = `SELECT l.id AS lead_id, ` + effectivePlaceTypeSQL + ` AS place_type
	 FROM lead l JOIN org o ON o.id = l.org_id
	 WHERE l.kind IN (` + cafeKindsSQL() + `) AND o.kind <> 'selftest' AND NOT ` + orgDomainSuppressedSQL("o") + `
	   AND ($1::bigint = 0 OR l.campaign_id = $1::bigint)
	 ORDER BY l.id`

// intentHandlesSQL is every handle whose latest match is a strong or operator one naming a lead
// of $1, as (lead_id, handle): igLeadHandleSQL's rule without its LIMIT 1, so a café holding two
// accounts reads both.
const intentHandlesSQL = `SELECT x.lead_id, x.handle FROM (
	   SELECT DISTINCT ON (handle) handle, coalesce(lead_id, 0) AS lead_id, tier FROM ig_match
	   WHERE handle IN (SELECT handle FROM ig_match WHERE lead_id = ANY($1::bigint[])) ORDER BY handle, id DESC) x
	 WHERE x.lead_id = ANY($1::bigint[]) AND x.tier IN ('` + IGTierHandle + `', '` + IGTierDomain + `', '` + IGTierOperator + `')
	 ORDER BY x.lead_id, x.handle`

// IntentInputs gathers intent@1's inputs for every café lead of campaign campaignID (0: every
// campaign), keyed by lead id, in a fixed number of bulk reads. The call is filled from the
// latest lead_intent_call (a cleared call is none); SyncIntent drops it before it derives.
func (q Queries) IntentInputs(ctx context.Context, cfg IntentConfig, campaignID int64, now time.Time) (map[int64]intent.Inputs, error) {
	if err := cfg.check(); err != nil {
		return nil, fmt.Errorf("leadstore: intent inputs: %w", err)
	}
	type leadRow struct {
		LeadID    int64  `db:"lead_id"`
		PlaceType string `db:"place_type"`
	}
	leads, err := collect(ctx, q.db, pgx.RowToStructByName[leadRow], intentLeadsSQL, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: intent inputs: leads: %w", err)
	}
	ownVendor := cfg.own()
	name := func(id string) string {
		if v := cfg.Cafe.Vendor(id); v != nil {
			return v.Name
		}
		return id
	}
	out := make(map[int64]intent.Inputs, len(leads))
	ids := make([]int64, 0, len(leads))
	for _, l := range leads {
		ids = append(ids, l.LeadID)
		out[l.LeadID] = intent.Inputs{Now: now, PlaceType: l.PlaceType, OwnVendor: ownVendor,
			QuestionCues: cfg.Cafe.Instagram.QuestionCues, VendorName: name}
	}
	if len(ids) == 0 {
		return out, nil
	}

	// Handles, every strong or operator one per lead.
	type handleRow struct {
		LeadID int64  `db:"lead_id"`
		Handle string `db:"handle"`
	}
	hrows, err := collect(ctx, q.db, pgx.RowToStructByName[handleRow], intentHandlesSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("leadstore: intent inputs: handles: %w", err)
	}
	byLead := map[int64][]string{}
	handles := []string{}
	for _, h := range hrows {
		byLead[h.LeadID] = append(byLead[h.LeadID], h.Handle)
		if !slices.Contains(handles, h.Handle) {
			handles = append(handles, h.Handle)
		}
	}

	var (
		followers map[string][]IGFollowerOf
		touches   map[string][]IGTouch
		hints     map[string][]IGHint
		profiles  map[string]IGProfile
		jev       map[string]map[string]string
	)
	if len(handles) > 0 {
		if followers, err = q.IGFollowerVendors(ctx, handles); err != nil {
			return nil, fmt.Errorf("leadstore: intent inputs: %w", err)
		}
		if touches, err = q.IGTouchesFor(ctx, handles, now.Add(-intentMaxAge)); err != nil {
			return nil, fmt.Errorf("leadstore: intent inputs: %w", err)
		}
		if hints, err = q.intentHints(ctx, handles); err != nil {
			return nil, fmt.Errorf("leadstore: intent inputs: hints: %w", err)
		}
		if profiles, err = q.intentProfiles(ctx, handles); err != nil {
			return nil, fmt.Errorf("leadstore: intent inputs: profiles: %w", err)
		}
		if jev, err = q.intentJev(ctx, handles); err != nil {
			return nil, fmt.Errorf("leadstore: intent inputs: judgments: %w", err)
		}
	}
	site, err := q.intentSiteLoyalty(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("leadstore: intent inputs: website: %w", err)
	}
	calls, err := q.intentCalls(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("leadstore: intent inputs: calls: %w", err)
	}

	for _, id := range ids {
		in := out[id]
		var places, loyalties, vendors []string
		for _, h := range byLead[id] {
			for _, f := range followers[h] {
				if v := cfg.VendorAccount(f.Owner); v != "" && cfg.loyal(v) {
					in.Follows = append(in.Follows, intent.Dated{Vendor: v, At: f.CapturedAt})
				}
			}
			for _, hint := range hints[h] {
				if cfg.loyal(hint.Value) {
					in.FollowedBy = append(in.FollowedBy, intent.Dated{Vendor: hint.Value, At: hint.CapturedAt})
				}
			}
			for _, t := range touches[h] {
				v := cfg.VendorAccount(t.VendorHandle)
				if v == "" || !cfg.loyal(v) {
					continue
				}
				tc := intent.Touch{Vendor: v, Kind: t.Kind, At: t.At}
				if t.Kind == IGTouchComment {
					tc.Text = t.Text
				}
				in.Touches = append(in.Touches, tc)
			}
			if p, ok := profiles[h]; ok {
				if rv := cfg.RuleVendor(p); rv != "" && (in.RuleVendor == "" || (ownVendor != "" && rv == ownVendor)) {
					in.RuleVendor = rv // the first handle's vendor, the own vendor over any
				}
				in.Prepaid = in.Prepaid || cfg.Prepaid(p)
			}
			j := jev[h]
			places, loyalties, vendors = append(places, j[IGPlace]), append(loyalties, j[IGLoyalty]), append(vendors, j[IGVendor])
		}
		in.JevPlace, in.JevLoyalty, in.JevVendor = mergeJevPlace(places), mergeJevLoyalty(loyalties), mergeJevVendor(vendors)
		s := site[id]
		for _, v := range s.vendors {
			if cfg.loyal(v) && !slices.Contains(in.SiteVendors, v) {
				in.SiteVendors = append(in.SiteVendors, v)
			}
		}
		in.SiteWord = s.word
		if c, ok := calls[id]; ok && c.Class != "" {
			in.Call, in.CallNote, in.CallHeldUntil = c.Class, c.Note, c.HeldUntil
		}
		out[id] = in
	}
	return out, nil
}

// mergeJevPlace is the lead's ig_place over its handles: not_coffee only when no handle reads
// as a coffee place, so a café's second account never outs it; "" otherwise.
func mergeJevPlace(cs []string) string {
	if slices.Contains(cs, IGPlaceCafeSeating) || slices.Contains(cs, IGPlaceRoasteryOrOnline) {
		return ""
	}
	if slices.Contains(cs, "not_coffee") {
		return "not_coffee"
	}
	return ""
}

// mergeJevLoyalty is runs_scheme when any handle runs one, then none_seen, else "".
func mergeJevLoyalty(cs []string) string {
	for _, want := range []string{"runs_scheme", "none_seen"} {
		if slices.Contains(cs, want) {
			return want
		}
	}
	return ""
}

// mergeJevVendor is the first named vendor over the handles, then paper_card, then
// unnamed_app_or_qr, then none; unclear and no answer are "".
func mergeJevVendor(cs []string) string {
	for _, c := range cs {
		switch c {
		case "", "unclear", "none", "paper_card", "unnamed_app_or_qr":
		default:
			return c
		}
	}
	for _, want := range []string{"paper_card", "unnamed_app_or_qr", "none"} {
		if slices.Contains(cs, want) {
			return want
		}
	}
	return ""
}

// intentHints are the followed_by_vendor hints of handles, by handle.
func (q Queries) intentHints(ctx context.Context, handles []string) (map[string][]IGHint, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[IGHint],
		`SELECT handle, kind, value, captured_at FROM ig_signal_hint
		 WHERE kind = '`+IGHintFollowedByVendor+`' AND handle = ANY($1::text[]) ORDER BY handle, id`, handles)
	if err != nil {
		return nil, err
	}
	out := map[string][]IGHint{}
	for _, r := range rows {
		out[r.Handle] = append(out[r.Handle], r)
	}
	return out, nil
}

// intentProfiles is the latest ok capture of each handle (a missing capture is no text).
func (q Queries) intentProfiles(ctx context.Context, handles []string) (map[string]IGProfile, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[IGProfile],
		`SELECT DISTINCT ON (p.handle) `+igProfileColumns+`
		 FROM ig_profile p WHERE p.handle = ANY($1::text[]) AND p.outcome = '`+IGOutcomeOK+`'
		 ORDER BY p.handle, p.id DESC`, handles)
	if err != nil {
		return nil, err
	}
	out := make(map[string]IGProfile, len(rows))
	for _, r := range rows {
		out[r.Handle] = r
	}
	return out, nil
}

// intentJev is each handle's standing real Jev answer per question, from the newest capture
// holding one: the fake decider's answers are left out in SQL (never a fact about a café), and
// rule rows too (RuleVendor recomputes the rule now, so an old rule's view never sticks).
// "unclear" reads as no answer.
func (q Queries) intentJev(ctx context.Context, handles []string) (map[string]map[string]string, error) {
	type row struct {
		Handle   string `db:"handle"`
		Question string `db:"question"`
		Choice   string `db:"choice"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT DISTINCT ON (p.handle, j.question) p.handle, j.question, j.choice
		 FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id
		 WHERE p.handle = ANY($1::text[]) AND j.question IN ('`+IGPlace+`', '`+IGLoyalty+`', '`+IGVendor+`')
		   AND j.source = '`+IGFromJev+`' AND j.model IS DISTINCT FROM '`+IGFakeModel+`'
		 ORDER BY p.handle, j.question, j.profile_id DESC, j.id DESC`, handles)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for _, r := range rows {
		if r.Choice == "unclear" {
			continue
		}
		if out[r.Handle] == nil {
			out[r.Handle] = map[string]string{}
		}
		out[r.Handle][r.Question] = r.Choice
	}
	return out, nil
}

type intentSite struct {
	vendors []string
	word    bool
}

// intentSiteLoyalty is the café website's loyalty verdict of each lead, from its finding's
// evidence items {name: loyalty, value: loyalty:<vendor>:<via>:<match>}: a named vendor, or
// a loyalty phrase with no vendor (via word).
func (q Queries) intentSiteLoyalty(ctx context.Context, ids []int64) (map[int64]intentSite, error) {
	type row struct {
		LeadID int64  `db:"lead_id"`
		Vendor string `db:"vendor"`
		Via    string `db:"via"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT l.id AS lead_id, split_part(e.v->>'value', ':', 2) AS vendor, split_part(e.v->>'value', ':', 3) AS via
		 FROM lead l JOIN finding f ON f.id = l.finding_id
		 CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(f.evidence) = 'array' THEN f.evidence ELSE '[]'::jsonb END) e(v)
		 WHERE l.id = ANY($1::bigint[]) AND e.v->>'name' = 'loyalty'
		 ORDER BY l.id, e.v->>'value'`, ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]intentSite{}
	for _, r := range rows {
		s := out[r.LeadID]
		if r.Via == cafe.ViaWord {
			s.word = true
		} else if r.Vendor != "" {
			s.vendors = append(s.vendors, r.Vendor)
		}
		out[r.LeadID] = s
	}
	return out, nil
}

type intentCall struct {
	LeadID    int64      `db:"lead_id"`
	Class     string     `db:"class"` // "" for a cleared call
	HeldUntil *time.Time `db:"held_until"`
	Note      string     `db:"note"`
}

// intentCalls is the latest call row of each lead of ids (a cleared one has Class "").
func (q Queries) intentCalls(ctx context.Context, ids []int64) (map[int64]intentCall, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[intentCall],
		`SELECT DISTINCT ON (lead_id) lead_id, coalesce(class, '') AS class, held_until, note
		 FROM lead_intent_call WHERE lead_id = ANY($1::bigint[]) ORDER BY lead_id, id DESC`, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]intentCall, len(rows))
	for _, r := range rows {
		out[r.LeadID] = r
	}
	return out, nil
}

// IntentSync is what one SyncIntent wrote, or in a dry run would write.
type IntentSync struct {
	// Leads is how many café leads the sync classified.
	Leads int
	// Classes counts those leads by their derived class after the sync (every class a key).
	Classes map[string]int
	// Changes are the leads a lead_intent row was written for, by lead id.
	Changes []IntentChange
}

// IntentChange is one lead whose derived intent a sync changed.
type IntentChange struct {
	LeadID int64
	From   string // the derived class before; intent.Cold for a lead with no row
	intent.Result
}

// intentLatest is a lead's latest lead_intent row; no row reads as cold, 0, 0, "", nil, nil.
type intentLatest struct {
	LeadID    int64      `db:"lead_id"`
	Class     string     `db:"class"`
	Overlap   int        `db:"overlap"`
	Score     int        `db:"score"`
	Reason    string     `db:"reason"`
	SignalAt  *time.Time `db:"signal_at"`
	HeldUntil *time.Time `db:"held_until"`
}

// SyncIntent derives every café lead's intent@1 class of campaign campaignID (0: every café
// campaign) at now and appends a lead_intent row where class, overlap, score, reason,
// signal_at or held_until differ from the lead's latest row. The derived row never carries the
// operator's call: the call is read beside it (intentJoinSQL), so clearing it shows the
// derived class at once. It runs in one transaction under intentSyncLockKey, so it is
// idempotent even when two run at once; a dry run does the same writes and rolls them back.
// No model is called and nothing is read from Instagram.
func (q Queries) SyncIntent(ctx context.Context, cfg IntentConfig, campaignID int64, now time.Time, dryRun bool) (IntentSync, error) {
	b, ok := q.db.(beginner)
	if !ok {
		return IntentSync{}, fmt.Errorf("leadstore: sync intent: %T cannot begin a transaction", q.db)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return IntentSync{}, fmt.Errorf("leadstore: sync intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	out, err := NewQueries(tx).syncIntent(ctx, cfg, campaignID, now)
	if err != nil {
		return IntentSync{}, fmt.Errorf("leadstore: sync intent: %w", err)
	}
	if dryRun {
		if err := tx.Rollback(ctx); err != nil {
			return IntentSync{}, fmt.Errorf("leadstore: sync intent: dry run rollback: %w", err)
		}
		return out, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return IntentSync{}, fmt.Errorf("leadstore: sync intent: commit: %w", err)
	}
	return out, nil
}

// syncIntent is SyncIntent inside its transaction.
func (q Queries) syncIntent(ctx context.Context, cfg IntentConfig, campaignID int64, now time.Time) (IntentSync, error) {
	if _, err := q.db.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, intentSyncLockKey); err != nil {
		return IntentSync{}, fmt.Errorf("lock: %w", err)
	}
	inputs, err := q.IntentInputs(ctx, cfg, campaignID, now)
	if err != nil {
		return IntentSync{}, err
	}
	ids := make([]int64, 0, len(inputs))
	for id := range inputs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := IntentSync{Leads: len(ids), Classes: map[string]int{}}
	for _, c := range IntentClasses {
		out.Classes[c] = 0
	}
	if len(ids) == 0 {
		return out, nil
	}
	latestRows, err := collect(ctx, q.db, pgx.RowToStructByName[intentLatest],
		`SELECT DISTINCT ON (lead_id) lead_id, class, overlap, score, reason, signal_at, held_until
		 FROM lead_intent WHERE lead_id = ANY($1::bigint[]) ORDER BY lead_id, id DESC`, ids)
	if err != nil {
		return IntentSync{}, fmt.Errorf("latest: %w", err)
	}
	latest := make(map[int64]intentLatest, len(latestRows))
	for _, r := range latestRows {
		latest[r.LeadID] = r
	}

	var (
		wIDs              []int64
		wClass, wReason   []string
		wOverlap, wScore  []int32
		wSignal, wHeldTil []*time.Time
	)
	for _, id := range ids {
		in := inputs[id]
		in.Call, in.CallNote, in.CallHeldUntil = "", "", nil // the derived row never carries the call
		r := intent.Classify(in)
		r.SignalAt, r.HeldUntil = micro(r.SignalAt), micro(r.HeldUntil)
		out.Classes[r.Class]++
		prev, ok := latest[id]
		if !ok {
			// No row reads as cold, 0, 0, nil, nil, and cold's own words ("no loyalty signal
			// yet") are no change: a first sync writes only the leads that are not plain cold.
			prev = intentLatest{Class: intent.Cold}
			if r.Class == intent.Cold {
				prev.Reason = r.Reason
			}
		}
		if prev.Class == r.Class && prev.Overlap == r.Overlap && prev.Score == r.Score && prev.Reason == r.Reason &&
			sameTime(prev.SignalAt, r.SignalAt) && sameTime(prev.HeldUntil, r.HeldUntil) {
			continue
		}
		out.Changes = append(out.Changes, IntentChange{LeadID: id, From: prev.Class, Result: r})
		wIDs = append(wIDs, id)
		wClass, wReason = append(wClass, r.Class), append(wReason, r.Reason)
		wOverlap, wScore = append(wOverlap, int32(r.Overlap)), append(wScore, int32(r.Score))
		wSignal, wHeldTil = append(wSignal, r.SignalAt), append(wHeldTil, r.HeldUntil)
	}
	if len(wIDs) == 0 {
		return out, nil
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO lead_intent (lead_id, class, overlap, score, reason, signal_at, held_until, rules)
		 SELECT u.id, u.c, u.o, u.s, u.r, u.sa, u.hu, $8
		 FROM unnest($1::bigint[], $2::text[], $3::int[], $4::int[], $5::text[], $6::timestamptz[], $7::timestamptz[])
		   AS u(id, c, o, s, r, sa, hu)`,
		wIDs, wClass, wOverlap, wScore, wReason, wSignal, wHeldTil, intent.Rules)
	if err != nil {
		return IntentSync{}, fmt.Errorf("write: %w", err)
	}
	if tag.RowsAffected() != int64(len(wIDs)) {
		return IntentSync{}, fmt.Errorf("write: %d rows for %d changes", tag.RowsAffected(), len(wIDs))
	}
	return out, nil
}

// micro is t at the store's precision (microseconds), so a value read back compares equal.
func micro(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Truncate(time.Microsecond)
	return &v
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// SetIntentCall appends the operator's call on lead leadID's class: class nil clears it (a
// clear with no standing call writes nothing). A just_chose call holds the lead for 60 days
// from now (held_until stored on the call). The note is folded to one line. It refuses a class
// outside intent@1's seven (ErrUnknownIntentClass), a note over IntentMaxNote characters
// (ErrBadIntentNote), an id with no lead (ErrLeadNotFound) and a lead whose rating type is not
// café or whose org is a selftest one (ErrNotCafeLead).
func (q Queries) SetIntentCall(ctx context.Context, leadID int64, class *string, note string) error {
	if class != nil && !slices.Contains(IntentClasses, *class) {
		return fmt.Errorf("leadstore: set intent call %d: %w: %q", leadID, ErrUnknownIntentClass, *class)
	}
	note = strings.Join(strings.Fields(note), " ")
	if utf8.RuneCountInString(note) > IntentMaxNote {
		return fmt.Errorf("leadstore: set intent call %d: %w", leadID, ErrBadIntentNote)
	}
	if class == nil {
		note = ""
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO lead_intent_call (lead_id, class, held_until, note, actor)
		 SELECT l.id, $2::text, CASE WHEN $2::text = '`+intent.JustChose+`' THEN now() + interval '`+intentCallHold+`' END, $3, '`+ActorGremy+`'
		 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.id = $1 AND l.kind IN (`+cafeKindsSQL()+`) AND o.kind <> 'selftest'
		   AND ($2::text IS NOT NULL OR (SELECT c.class FROM lead_intent_call c WHERE c.lead_id = l.id ORDER BY c.id DESC LIMIT 1) IS NOT NULL)`,
		leadID, class, note)
	if err != nil {
		return fmt.Errorf("leadstore: set intent call %d: %w", leadID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists, isCafe bool
	if err := q.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM lead WHERE id = $1),
		        EXISTS (SELECT 1 FROM lead l JOIN org o ON o.id = l.org_id
		                WHERE l.id = $1 AND l.kind IN (`+cafeKindsSQL()+`) AND o.kind <> 'selftest')`, leadID).Scan(&exists, &isCafe); err != nil {
		return fmt.Errorf("leadstore: set intent call %d: %w", leadID, err)
	}
	switch {
	case !exists:
		return fmt.Errorf("leadstore: set intent call %d: %w", leadID, ErrLeadNotFound)
	case !isCafe:
		return fmt.Errorf("leadstore: set intent call %d: %w", leadID, ErrNotCafeLead)
	}
	return nil // a clear with no standing call
}

// intentJoinSQL is the lead l's intent as one row, for a LATERAL join beside a lead (it reads
// l.id): the latest lead_intent row plus the latest lead_intent_call, the call's class winning
// (a cleared call is none). It always returns one row; a lead with neither reads cold, 0, 0.
//
//	intent_class      the effective class
//	intent_derived    the latest derived class, '' with no row
//	intent_overlap, intent_score, intent_reason, intent_signal_at   the derived row's
//	intent_held_until the call's held_until under a call, else the derived row's
//	intent_yours      true under a call; intent_note its note ('' without)
//	intent_synced_at  the derived row's created_at, NULL with none
const intentJoinSQL = `SELECT coalesce(lic.class, lid.class, '` + intent.Cold + `') AS intent_class,
	   coalesce(lid.class, '') AS intent_derived,
	   coalesce(lid.overlap, 0) AS intent_overlap, coalesce(lid.score, 0) AS intent_score,
	   coalesce(lid.reason, '') AS intent_reason, lid.signal_at AS intent_signal_at,
	   CASE WHEN lic.class IS NOT NULL THEN lic.held_until ELSE lid.held_until END AS intent_held_until,
	   (lic.class IS NOT NULL) AS intent_yours,
	   CASE WHEN lic.class IS NOT NULL THEN lic.note ELSE '' END AS intent_note,
	   lid.created_at AS intent_synced_at
	 FROM (SELECT 1) intent_one
	 LEFT JOIN LATERAL (SELECT li.class, li.overlap, li.score, li.reason, li.signal_at, li.held_until, li.created_at
	   FROM lead_intent li WHERE li.lead_id = l.id ORDER BY li.id DESC LIMIT 1) lid ON true
	 LEFT JOIN LATERAL (SELECT lc.class, lc.held_until, lc.note
	   FROM lead_intent_call lc WHERE lc.lead_id = l.id ORDER BY lc.id DESC LIMIT 1) lic ON true`

// intentRankSQL is the order rank of the intentJoinSQL row aliased x: shopping 1, paper 2,
// switch 3, curious 4, cold 5, just_chose 6, out 7 (intent.Rank); anything else 5.
func intentRankSQL(x string) string {
	var b strings.Builder
	b.WriteString(`(CASE ` + x + `.intent_class`)
	for _, c := range IntentClasses {
		fmt.Fprintf(&b, ` WHEN %s THEN %d`, sqlLiteral(c), intent.Rank(c))
	}
	b.WriteString(` ELSE 5 END)`)
	return b.String()
}

// intentHeldExprSQL is true when the intentJoinSQL row x holds its lead from sends at now (an
// SQL expression): just_chose while held_until > now, an out from the operator's call, and a
// derived out by rules 2-3 (every derived out but the roastery's, intentRoasteryReason). A NULL
// (a just_chose with no held_until) reads false, so the queue, StarredStatus and intentHeldSQL
// agree it is not held (TestHoldIntentNullHeldUntil).
func intentHeldExprSQL(x, now string) string {
	return `coalesce((` + x + `.intent_class = '` + intent.JustChose + `' AND ` + x + `.intent_held_until > ` + now + `)
	   OR (` + x + `.intent_class = '` + intent.Out + `' AND (` + x + `.intent_yours OR ` + x + `.intent_reason <> ` + sqlLiteral(intentRoasteryReason) + `)), false)`
}

// intentHeldSQL is the SQL condition that is true when the lead l is held from sends by its
// loyalty intent at now (a parameter or expression, e.g. "$3" or "now()"), standalone like
// roasteryHeldSQL so it fits any WHERE beside a lead l: see intentHeldExprSQL. A lead with no
// row and no call is never held.
func intentHeldSQL(now string) string {
	return `coalesce((SELECT ` + intentHeldExprSQL("ih", now) + ` FROM (` + intentJoinSQL + `) ih), false)`
}

// IntentRow is a lead's loyalty intent as the pages and reports read it.
type IntentRow struct {
	LeadID int64 `db:"lead_id"`
	// Class is the effective class: the operator's call when there is one, else the derived
	// one; cold with neither.
	Class string `db:"intent_class"`
	// Derived is the latest derived class; "" for a lead never synced to a row (reads cold).
	Derived   string     `db:"intent_derived"`
	Overlap   int        `db:"intent_overlap"`
	Score     int        `db:"intent_score"`
	Reason    string     `db:"intent_reason"` // the derived reason, even under a call
	SignalAt  *time.Time `db:"intent_signal_at"`
	HeldUntil *time.Time `db:"intent_held_until"` // the call's under a call
	Yours     bool       `db:"intent_yours"`
	Note      string     `db:"intent_note"` // the call's note; "" without a call
	SyncedAt  *time.Time `db:"intent_synced_at"`
	// Held is whether the class holds the lead from sends now (intentHeldSQL).
	Held bool `db:"intent_held"`
	// Rank is the class's order rank (intent.Rank).
	Rank int `db:"intent_rank"`
}

// LatestIntents reads the intent of each lead of ids in one query, keyed by lead id; an id
// with no lead is not in the map, and a lead with no row and no call reads cold.
func (q Queries) LatestIntents(ctx context.Context, ids []int64) (map[int64]IntentRow, error) {
	out := map[int64]IntentRow{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[IntentRow],
		`SELECT l.id AS lead_id, x.*, `+intentHeldSQL("now()")+` AS intent_held,
		        `+intentRankSQL("x")+` AS intent_rank
		 FROM lead l CROSS JOIN LATERAL (`+intentJoinSQL+`) x
		 WHERE l.id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("leadstore: latest intents: %w", err)
	}
	for _, r := range rows {
		out[r.LeadID] = r
	}
	return out, nil
}
