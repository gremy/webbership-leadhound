package leadstore

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Instagram as a source of a café lead's place type (migration 00032). What Instagram says is
// computed here from the store, never asked of a model: the lead's handle (igLeadHandleSQL, the
// same one its card shows), that handle's standing ig_place answer (IGStandingJudgments), and a
// map of that answer to a type. The result is written as an instagram row only where it differs
// from the lead's latest instagram row, so a sync is cheap to run after every import and every
// operator match, and a second run writes nothing.
//
// Why a written row and not a join at read time: every queue and send road reads
// effectivePlaceTypeSQL, and folding the judgment ordering (a rule before Jev, a real answer
// before the fake's) into that hot fragment would copy IGStandingJudgments into SQL. One sync
// keeps one copy of that logic, and every change of type is a row with its reason.

// igPlaceSyncLockKey is the transaction-level advisory lock a sync holds from its first read
// to its commit, so two syncs at once (an import and an operator's match) never both see the
// same latest row and write it twice: the second waits, then reads what the first committed.
const igPlaceSyncLockKey int64 = 0x4c48_4947_504c // "LHIGPL"

// igNoStrongMatch is the detail of a silent row written because the lead has no strong match
// any more (an unmatch, a re-match to another lead, a tie).
const igNoStrongMatch = "no strong match"

// igLatestPlaceSQL is the place type of the lead l's latest instagram row, NULL with none.
const igLatestPlaceSQL = `(SELECT t.place_type FROM lead_place_type t
	   WHERE t.lead_id = l.id AND t.source = '` + PlaceFromInstagram + `' ORDER BY t.id DESC LIMIT 1)`

// igPlaceLeadsSQL is every café lead a sync looks at, with its handle (empty with none) and its
// latest instagram row's type (empty with none): a lead some ig_match row names, or one that
// already holds an instagram row, so an unmatch reaches the lead it leaves. Never a selftest
// org and never an org whose domain is suppressed, as igLeadKeySQL: an erased org is never
// read again.
var igPlaceLeadsSQL = `SELECT l.id AS lead_id, coalesce(h.handle, '') AS handle,
	   coalesce(` + igLatestPlaceSQL + `, '') AS latest
	 FROM lead l
	 JOIN org o ON o.id = l.org_id
	 LEFT JOIN LATERAL (` + igLeadHandleSQL + `) h ON true
	 WHERE l.kind IN (` + cafeKindsSQL() + `) AND o.kind <> 'selftest' AND NOT ` + orgDomainSuppressedSQL("o") + `
	   AND (EXISTS (SELECT 1 FROM ig_match m WHERE m.lead_id = l.id)
	        OR EXISTS (SELECT 1 FROM lead_place_type t WHERE t.lead_id = l.id AND t.source = '` + PlaceFromInstagram + `'))
	 ORDER BY l.id`

// igPlaceInsertSQL appends Instagram's place type $2 with detail $3 on lead $1, through the
// same café and selftest join as SetPlaceType, and only while the lead's latest instagram row
// says otherwise. No row reads as unclear, so a lead Instagram never typed gets no silent row.
var igPlaceInsertSQL = `INSERT INTO lead_place_type (lead_id, place_type, source, detail, input_sha256, actor)
	 SELECT l.id, $2::text, '` + PlaceFromInstagram + `', $3, NULL, '` + ActorSystem + `'
	 FROM lead l JOIN org o ON o.id = l.org_id
	 WHERE l.id = $1 AND l.kind IN (` + cafeKindsSQL() + `) AND o.kind <> 'selftest'
	   AND coalesce(` + igLatestPlaceSQL + `, '` + PlaceUnclear + `') IS DISTINCT FROM $2::text`

// IGPlaceSync is what one SyncIGPlaceTypes wrote, or in a dry run would write.
type IGPlaceSync struct {
	// Roastery, Cafe and Silent count the instagram rows written by type (Silent: unclear).
	Roastery, Cafe, Silent int
	// Released are the leads whose effective type went from roastery to café, by lead id: the
	// one road that can put a roastery back into café outreach, so callers list it first.
	Released []IGPlaceChange
	// Held are the leads whose effective type went from café to roastery, by lead id.
	Held []IGPlaceChange
}

// IGPlaceChange is one lead whose effective place type a sync changed.
type IGPlaceChange struct {
	LeadID int64
	Handle string // the lead's handle, stored form without the @; empty with no strong match
	Choice string // the handle's standing ig_place choice; empty with none
	// Before is the effective type before the sync, with the source and detail that decided
	// it (empty source: the café default); After is the effective type after it.
	Before, After PlaceTypeRow
}

// SyncIGPlaceTypes writes Instagram's current place call as each café lead's instagram place
// type row, where it differs from the lead's latest one. Instagram says roastery for a
// roastery_or_online call and café for a cafe_seating call on the lead's handle; anything
// else (unclear, not_coffee, no call, the fake decider's answer, no strong match) is silence,
// an unclear row, so the next source decides. A handle whose latest capture is missing keeps
// its last standing answer, as the lead card shows it.
//
// It runs in one transaction under igPlaceSyncLockKey, with one conditional insert per lead,
// so it is idempotent even when two run at once. A dry run does the same writes and rolls them
// back, so its counts and lists are exactly what the real run would write. No model is called
// and nothing is read from Instagram.
func (q Queries) SyncIGPlaceTypes(ctx context.Context, dryRun bool) (IGPlaceSync, error) {
	b, ok := q.db.(beginner)
	if !ok {
		return IGPlaceSync{}, fmt.Errorf("leadstore: sync ig place types: %T cannot begin a transaction", q.db)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return IGPlaceSync{}, fmt.Errorf("leadstore: sync ig place types: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	out, err := NewQueries(tx).syncIGPlaceTypes(ctx)
	if err != nil {
		return IGPlaceSync{}, fmt.Errorf("leadstore: sync ig place types: %w", err)
	}
	if dryRun {
		if err := tx.Rollback(ctx); err != nil {
			return IGPlaceSync{}, fmt.Errorf("leadstore: sync ig place types: dry run rollback: %w", err)
		}
		return out, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return IGPlaceSync{}, fmt.Errorf("leadstore: sync ig place types: commit: %w", err)
	}
	return out, nil
}

// syncIGPlaceTypes is SyncIGPlaceTypes inside its transaction.
func (q Queries) syncIGPlaceTypes(ctx context.Context) (IGPlaceSync, error) {
	if _, err := q.db.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, igPlaceSyncLockKey); err != nil {
		return IGPlaceSync{}, fmt.Errorf("lock: %w", err)
	}
	type lead struct {
		LeadID int64  `db:"lead_id"`
		Handle string `db:"handle"`
		Latest string `db:"latest"`
	}
	leads, err := collect(ctx, q.db, pgx.RowToStructByName[lead], igPlaceLeadsSQL)
	if err != nil {
		return IGPlaceSync{}, fmt.Errorf("leads: %w", err)
	}
	handles := []string{}
	for _, l := range leads {
		if l.Handle != "" {
			handles = append(handles, l.Handle)
		}
	}
	judgments := map[string]map[string]IGJudgment{}
	if len(handles) > 0 {
		if judgments, err = q.IGStandingJudgments(ctx, handles); err != nil {
			return IGPlaceSync{}, fmt.Errorf("judgments: %w", err)
		}
	}

	type write struct {
		id                                int64
		handle, choice, placeType, detail string
	}
	var writes []write
	ids := []int64{}
	for _, l := range leads {
		choice := ""
		// The fake decider never decides a real lead's type. IGStandingJudgments already puts a
		// real answer over a fake one, but a handle with only fake answers still returns them.
		if j, ok := judgments[l.Handle][IGPlace]; ok && (j.Source != IGFromJev || j.Model != IGFakeModel) {
			choice = j.Choice
		}
		want := igPlaceTypeOf(choice)
		latest := l.Latest
		if latest == "" {
			latest = PlaceUnclear
		}
		if latest == want {
			continue
		}
		writes = append(writes, write{id: l.LeadID, handle: l.Handle, choice: choice, placeType: want,
			detail: igPlaceDetail(l.Handle, choice)})
		ids = append(ids, l.LeadID)
	}
	out := IGPlaceSync{}
	if len(writes) == 0 {
		return out, nil
	}
	before, err := q.PlaceTypes(ctx, ids)
	if err != nil {
		return IGPlaceSync{}, fmt.Errorf("before: %w", err)
	}
	written := make([]write, 0, len(writes))
	for _, w := range writes {
		tag, err := q.db.Exec(ctx, igPlaceInsertSQL, w.id, w.placeType, w.detail)
		if err != nil {
			return IGPlaceSync{}, fmt.Errorf("write lead %d: %w", w.id, err)
		}
		if tag.RowsAffected() != 1 {
			continue
		}
		switch w.placeType {
		case PlaceRoastery:
			out.Roastery++
		case PlaceCafe:
			out.Cafe++
		default:
			out.Silent++
		}
		written = append(written, w)
	}
	if len(written) == 0 {
		return out, nil
	}
	ids = ids[:0]
	for _, w := range written {
		ids = append(ids, w.id)
	}
	after, err := q.PlaceTypes(ctx, ids)
	if err != nil {
		return IGPlaceSync{}, fmt.Errorf("after: %w", err)
	}
	for _, w := range written {
		c := IGPlaceChange{LeadID: w.id, Handle: w.handle, Choice: w.choice, Before: before[w.id], After: after[w.id]}
		switch {
		case c.Before.Type == PlaceRoastery && c.After.Type == PlaceCafe:
			out.Released = append(out.Released, c)
		case c.Before.Type == PlaceCafe && c.After.Type == PlaceRoastery:
			out.Held = append(out.Held, c)
		}
	}
	return out, nil
}

// igPlaceTypeOf maps an ig_place choice to the place type Instagram says: only a café with
// seating and a roastery or online store decide; every other choice, and none, is unclear.
func igPlaceTypeOf(choice string) string {
	switch choice {
	case IGPlaceCafeSeating:
		return PlaceCafe
	case IGPlaceRoasteryOrOnline:
		return PlaceRoastery
	}
	return PlaceUnclear
}

// igPlaceDetail is an instagram row's detail: "@handle: choice", "@handle: no place call" for a
// handle with no standing call, or igNoStrongMatch with no handle. It fits
// lead_place_type_detail_check: one line, at most 300 characters.
func igPlaceDetail(handle, choice string) string {
	var d string
	switch {
	case handle == "":
		d = igNoStrongMatch
	case choice == "":
		d = "@" + handle + ": no place call"
	default:
		d = "@" + handle + ": " + choice
	}
	d = strings.NewReplacer("\r", " ", "\n", " ").Replace(d)
	for utf8.RuneCountInString(d) > 300 {
		_, size := utf8.DecodeLastRuneInString(d)
		d = d[:len(d)-size]
	}
	return d
}
