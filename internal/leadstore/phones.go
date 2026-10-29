package leadstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/dial"
)

// Phones (CP5, migration 00020): screening numbers against the do-not-call registers,
// whether a stored phone fact works, the research rows of researched numbers, and the
// erasure of an org's phone data.

// Screening results (phone_screening_result_check).
const (
	ScreenClean  = "clean"
	ScreenListed = "listed"
)

// Phone statuses (phone_status_status_check); the latest per fact wins.
const (
	PhoneGood = "good"
	PhoneBad  = "bad"
)

// Why a phone status was recorded (phone_status_reason_check).
const (
	PhoneReasonWrongNumber = "wrong_number" // a call outcome said the number is wrong (bad)
	PhoneReasonBouncedCall = "bounced_call" // the line did not ring through (bad)
	PhoneReasonOperator    = "operator"     // set by hand (good or bad)
	PhoneReasonCorrected   = "corrected"    // a later outcome on the same call replaced a wrong_number (good)
)

// The lines a researched number can be (phone_research_line_kind_check).
const (
	LineDirect        = "direct"
	LineDecisionMaker = "decision_maker"
	LineSwitchboard   = "switchboard"
)

// MaxPhoneRole is the longest role a phone research row may carry (a job title, never a name).
const MaxPhoneRole = 60

// MaxScreenSource is the longest source (a result file's base name) a screening may carry.
const MaxScreenSource = 200

// MaxForgetReason is the longest reason ForgetOrg takes.
const MaxForgetReason = 200

// ScreenRegisters are the register codes phone_screening_register_check allows, sorted. A
// test keeps them equal to the registers of internal/compliance/countries.yaml and to the
// migration's CHECK list.
var ScreenRegisters = []string{"ctps", "dncm", "dncr", "gr_art11", "ndd", "robinson", "rpo", "tps"}

var (
	// ErrBadScreening is returned by RecordScreening for a number, register, result, time,
	// source or actor the table would refuse.
	ErrBadScreening = errors.New("leadstore: bad screening")
	// ErrBadPhoneStatus is returned by RecordPhoneStatus for a status, reason or outcome
	// pairing the table would refuse, or a fact that is not a phone.
	ErrBadPhoneStatus = errors.New("leadstore: bad phone status")
	// ErrBadPhoneResearch is returned by RecordPhoneResearch for a row the table would
	// refuse, or a fact that is not a phone of the row's org.
	ErrBadPhoneResearch = errors.New("leadstore: bad phone research")
	// ErrBadForgetReason is returned by ForgetOrg for a blank reason, one over
	// MaxForgetReason characters, or one with a line break or an @.
	ErrBadForgetReason = errors.New("leadstore: bad forget reason")
)

var e164RE = regexp.MustCompile(`^\+[0-9]{8,15}$`)

// Screening is one phone_screening row.
type Screening struct {
	ID        int64     `db:"id"`
	Number    string    `db:"number"`   // E.164
	Register  string    `db:"register"` // a ScreenRegisters code
	Result    string    `db:"result"`   // ScreenClean or ScreenListed
	CheckedAt time.Time `db:"checked_at"`
	Source    string    `db:"source"`
	Actor     string    `db:"actor"` // operator | system
	At        time.Time `db:"at"`
}

// ScreeningRecorded is what RecordScreening did.
type ScreeningRecorded struct {
	ID         int64 // the phone_screening row
	Suppressed int   // phone suppressions newly written (a listed result only)
}

// storedActor is the actor value the CP5 tables hold for actor (ActorGremy or ActorSystem).
func storedActor(actor string) (string, error) {
	switch actor {
	case ActorGremy:
		return ActorOperator, nil
	case ActorSystem:
		return ActorSystem, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownActor, actor)
}

// inTx runs fn in a transaction over q (a savepoint when q already runs in one), or
// directly when q's db cannot begin one.
func (q Queries) inTx(ctx context.Context, fn func(Queries) error) error {
	b, ok := q.db.(beginner)
	if !ok {
		return fn(q)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(NewQueries(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordScreening appends screening s (Number E.164, Register a ScreenRegisters code,
// Result, CheckedAt) imported from source (the result file's base name; "" for none) by
// actor (ActorGremy or ActorSystem), in one transaction. A listed result also writes the
// phone suppressions of account accountID (0: DefaultAccountID) for the number: one for
// its E.164 form and one for every stored phone fact whose number dials as it (a fact kept
// in national form is matched through dial.Number with its org's country), since the
// batch's phone pick hashes the fact's own normalised value. The suppression's reason is
// the register's code for tps and ctps, and manual (source "screen:<register>") for the
// other registers, whose codes are not suppression reasons. A clean result writes no
// suppression and lifts none: a number once listed stays out until the operator lifts it.
func (q Queries) RecordScreening(ctx context.Context, accountID int64, s Screening, source, actor string) (ScreeningRecorded, error) {
	var res ScreeningRecorded
	who, err := storedActor(actor)
	if err != nil {
		return res, fmt.Errorf("leadstore: record screening: %w", err)
	}
	source = strings.TrimSpace(source)
	switch {
	case !e164RE.MatchString(s.Number):
		return res, fmt.Errorf("leadstore: record screening: %w: the number is not E.164", ErrBadScreening)
	case !slices.Contains(ScreenRegisters, s.Register):
		return res, fmt.Errorf("leadstore: record screening: %w: unknown register %q", ErrBadScreening, s.Register)
	case s.Result != ScreenClean && s.Result != ScreenListed:
		return res, fmt.Errorf("leadstore: record screening: %w: unknown result %q", ErrBadScreening, s.Result)
	case s.CheckedAt.IsZero():
		return res, fmt.Errorf("leadstore: record screening: %w: no check time", ErrBadScreening)
	case utf8.RuneCountInString(source) > MaxScreenSource || strings.ContainsAny(source, "\r\n/\\"):
		return res, fmt.Errorf("leadstore: record screening: %w: the source is a file's base name", ErrBadScreening)
	}
	err = q.inTx(ctx, func(tq Queries) error {
		if err := tq.db.QueryRow(ctx,
			`INSERT INTO phone_screening (number, register, result, checked_at, source, actor)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			s.Number, s.Register, s.Result, s.CheckedAt, source, who).Scan(&res.ID); err != nil {
			return err
		}
		if s.Result != ScreenListed {
			return nil
		}
		reason := ReasonManual
		if s.Register == ReasonTPS || s.Register == ReasonCTPS {
			reason = s.Register
		}
		forms, err := tq.numberForms(ctx, s.Number)
		if err != nil {
			return err
		}
		for _, n := range forms {
			r, err := tq.Suppress(ctx, accountID, SuppressPhone, PhoneSuppressionValue(n), reason,
				fmt.Sprintf("screen:%s:%d", s.Register, res.ID))
			if err != nil {
				return err
			}
			if r.Recorded {
				res.Suppressed++
			}
		}
		return nil
	})
	if err != nil {
		return ScreeningRecorded{}, fmt.Errorf("leadstore: record screening: %w", err)
	}
	return res, nil
}

// numberForms is the E.164 number e164 and every distinct stored phone fact value that dials
// as it (its own normalised value when that differs), sorted.
func (q Queries) numberForms(ctx context.Context, e164 string) ([]string, error) {
	type row struct {
		Normalized string `db:"normalized"`
		Country    string `db:"country"`
	}
	// The last seven digits narrow the scan; dial.Number decides.
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT DISTINCT f.normalized, o.country FROM org_fact f JOIN org o ON o.id = f.org_id
		 WHERE f.kind = 'phone' AND (f.normalized = $1 OR right(f.normalized, 7) = right($1, 7))`, e164)
	if err != nil {
		return nil, err
	}
	forms := []string{e164}
	for _, r := range rows {
		if r.Normalized != e164 && dial.Number(r.Normalized, r.Country) == e164 && !slices.Contains(forms, r.Normalized) {
			forms = append(forms, r.Normalized)
		}
	}
	sort.Strings(forms)
	return forms, nil
}

// LatestScreening is number's (E.164) latest screening against register: the latest
// checked_at, then the latest row. ok is false when it was never screened there.
func (q Queries) LatestScreening(ctx context.Context, number, register string) (s Screening, ok bool, err error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[Screening],
		`SELECT id, number, register, result, checked_at, source, actor, at FROM phone_screening
		 WHERE number = $1 AND register = $2 ORDER BY checked_at DESC, id DESC LIMIT 1`, number, register)
	if err != nil {
		return Screening{}, false, fmt.Errorf("leadstore: latest screening: %w", err)
	}
	if len(rows) == 0 {
		return Screening{}, false, nil
	}
	return rows[0], true, nil
}

// ScreeningsMissingOrOlder lists the stored phone numbers (org_fact.normalized, as stored)
// of country's callable leads of account accountID (0: DefaultAccountID) whose latest
// screening against register is missing or was checked before before: phone facts of orgs
// whose country is country and that have a lead of the account in state new, batched or
// contacted, leaving out a fact whose latest status is bad and a number the account has an
// active phone suppression for. A fact kept in national form is matched to its screenings
// through dial.Number with country. The numbers come sorted, each once.
func (q Queries) ScreeningsMissingOrOlder(ctx context.Context, accountID int64, register, country string, before time.Time) ([]string, error) {
	type row struct {
		Normalized string     `db:"normalized"`
		Latest     *time.Time `db:"latest"`
	}
	// Every candidate fact, with the latest screening of its own value (E.164 facts). $2 is
	// the account (inAccountSQL names it so).
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT DISTINCT f.normalized,
		   (SELECT max(s.checked_at) FROM phone_screening s WHERE s.number = f.normalized AND s.register = $1) AS latest
		 FROM org_fact f JOIN org o ON o.id = f.org_id
		 WHERE f.kind = 'phone' AND o.country = $3
		   AND EXISTS (SELECT 1 FROM lead l WHERE l.org_id = o.id AND l.state IN ('new', 'batched', 'contacted')
		     AND `+inAccountSQL+`)
		   AND NOT `+phoneBadSQL("f.id")+`
		   AND NOT `+phoneSuppressedSQL("$2", "f.id")+`
		 ORDER BY f.normalized`, register, orAccount(accountID), country)
	if err != nil {
		return nil, fmt.Errorf("leadstore: screenings missing: %w", err)
	}
	var out []string
	for _, r := range rows {
		latest := r.Latest
		if e := dial.Number(r.Normalized, country); e != r.Normalized && e164RE.MatchString(e) {
			// A national-form fact: its screenings are under its E.164 form.
			s, ok, err := q.LatestScreening(ctx, e, register)
			if err != nil {
				return nil, err
			}
			latest = nil
			if ok {
				latest = &s.CheckedAt
			}
		}
		if (latest == nil || latest.Before(before)) && !slices.Contains(out, r.Normalized) {
			out = append(out, r.Normalized)
		}
	}
	sort.Strings(out) // byte order, whatever the database's collation
	return out, nil
}

// phoneBadSQL is a SQL condition that is true when the latest phone_status of org_fact id
// factID (an SQL expression) is bad.
func phoneBadSQL(factID string) string {
	return `coalesce((SELECT ps.status = 'bad' FROM phone_status ps WHERE ps.fact_id = ` + factID + `
	  ORDER BY ps.id DESC LIMIT 1), false)`
}

// PhoneStatusRow is one phone_status row.
type PhoneStatusRow struct {
	ID            int64     `db:"id"`
	FactID        int64     `db:"fact_id"`
	Status        string    `db:"status"`
	Reason        string    `db:"reason"`
	CallOutcomeID *int64    `db:"call_outcome_id"`
	Actor         string    `db:"actor"`
	At            time.Time `db:"at"`
}

// RecordPhoneStatus appends status (PhoneGood or PhoneBad) with reason for phone fact
// factID; callOutcomeID names the call outcome for PhoneReasonWrongNumber and
// PhoneReasonCorrected (required there, refused with PhoneReasonOperator). actor is
// ActorGremy or ActorSystem. A fact that is not a phone is refused (ErrBadPhoneStatus). It
// returns the row id. The latest row per fact wins (PhoneStatus). An operator's bad status
// cancels nothing: the call stays on Today and the card, not callable, with "marked wrong",
// and a good status put back makes it callable again.
func (q Queries) RecordPhoneStatus(ctx context.Context, factID int64, status, reason string, callOutcomeID *int64, actor string) (int64, error) {
	who, err := storedActor(actor)
	if err != nil {
		return 0, fmt.Errorf("leadstore: record phone status: %w", err)
	}
	if !validPhoneStatus(status, reason, callOutcomeID) {
		return 0, fmt.Errorf("leadstore: record phone status: %w: %q %q", ErrBadPhoneStatus, status, reason)
	}
	return q.insertPhoneStatus(ctx, factID, status, reason, callOutcomeID, who)
}

func validPhoneStatus(status, reason string, outcome *int64) bool {
	switch reason {
	case PhoneReasonWrongNumber:
		return status == PhoneBad && outcome != nil
	case PhoneReasonCorrected:
		return status == PhoneGood && outcome != nil
	case PhoneReasonBouncedCall:
		return status == PhoneBad
	case PhoneReasonOperator:
		return (status == PhoneGood || status == PhoneBad) && outcome == nil
	}
	return false
}

// insertPhoneStatus writes the row with the stored actor who; a fact that is not a phone
// writes nothing and is ErrBadPhoneStatus.
func (q Queries) insertPhoneStatus(ctx context.Context, factID int64, status, reason string, callOutcomeID *int64, who string) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO phone_status (fact_id, status, reason, call_outcome_id, actor)
		 SELECT f.id, $2, $3, $4, $5 FROM org_fact f WHERE f.id = $1 AND f.kind = 'phone' RETURNING id`,
		factID, status, reason, callOutcomeID, who).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("leadstore: record phone status: %w: fact %d is not a phone", ErrBadPhoneStatus, factID)
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: record phone status: %w", err)
	}
	return id, nil
}

// PhoneStatus is the latest status of phone fact factID; ok is false when it has none.
func (q Queries) PhoneStatus(ctx context.Context, factID int64) (s PhoneStatusRow, ok bool, err error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[PhoneStatusRow],
		`SELECT id, fact_id, status, reason, call_outcome_id, actor, at FROM phone_status
		 WHERE fact_id = $1 ORDER BY id DESC LIMIT 1`, factID)
	if err != nil {
		return PhoneStatusRow{}, false, fmt.Errorf("leadstore: phone status: %w", err)
	}
	if len(rows) == 0 {
		return PhoneStatusRow{}, false, nil
	}
	return rows[0], true, nil
}

// PhoneFactBad reports whether phone fact factID's latest status is bad.
func (q Queries) PhoneFactBad(ctx context.Context, factID int64) (bool, error) {
	var bad bool
	if err := q.db.QueryRow(ctx, `SELECT `+phoneBadSQL("$1::bigint"), factID).Scan(&bad); err != nil {
		return false, fmt.Errorf("leadstore: phone fact bad: %w", err)
	}
	return bad, nil
}

// UsableOrgFacts is OrgFacts without the phone facts whose latest status is bad, in the
// same order: what the batch's phone pick may choose from (a wrong number is never drafted
// again). Facts of the other kinds all come back.
func (q Queries) UsableOrgFacts(ctx context.Context, orgIDs []int64) ([]Fact, error) {
	if len(orgIDs) == 0 {
		return nil, nil
	}
	facts, err := collect(ctx, q.db, pgx.RowToStructByName[Fact],
		`SELECT f.id, f.org_id, f.kind, f.value, f.normalized, coalesce(f.fetch_id, 0) AS fetch_id, f.derived_by, f.found_at
		 FROM org_fact f WHERE f.org_id = ANY($1) AND (f.kind <> 'phone' OR NOT `+phoneBadSQL("f.id")+`)
		 ORDER BY f.org_id, f.kind, f.id`, orgIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: usable org facts: %w", err)
	}
	return facts, nil
}

// PhoneResearchRow is one phone_research row to write.
type PhoneResearchRow struct {
	OrgID      int64
	FactID     *int64 // the verified phone fact (org_fact kind phone of OrgID)
	LineKind   string // LineDirect, LineDecisionMaker, LineSwitchboard or ""
	Role       string // a job title printed beside the number, never a name; at most MaxPhoneRole
	Model      string
	CostMicros *int64 // nil: unknown
}

// RecordPhoneResearch appends r and returns its id. A fact that is not a phone of r.OrgID
// is refused (ErrBadPhoneResearch), as is a line kind, role, model or cost the table would
// refuse.
func (q Queries) RecordPhoneResearch(ctx context.Context, r PhoneResearchRow) (int64, error) {
	r.Role, r.Model = strings.TrimSpace(r.Role), strings.TrimSpace(r.Model)
	switch {
	case !slices.Contains([]string{LineDirect, LineDecisionMaker, LineSwitchboard, ""}, r.LineKind):
		return 0, fmt.Errorf("leadstore: record phone research: %w: line kind %q", ErrBadPhoneResearch, r.LineKind)
	case utf8.RuneCountInString(r.Role) > MaxPhoneRole || strings.ContainsAny(r.Role, "\r\n"):
		return 0, fmt.Errorf("leadstore: record phone research: %w: the role is over %d characters or has a line break", ErrBadPhoneResearch, MaxPhoneRole)
	case r.Model == "" || utf8.RuneCountInString(r.Model) > 100 || strings.ContainsAny(r.Model, "\r\n"):
		return 0, fmt.Errorf("leadstore: record phone research: %w: model", ErrBadPhoneResearch)
	case r.CostMicros != nil && *r.CostMicros < 0:
		return 0, fmt.Errorf("leadstore: record phone research: %w: negative cost", ErrBadPhoneResearch)
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO phone_research (org_id, fact_id, line_kind, role, model, cost_micros)
		 SELECT o.id, $2::bigint, $3, $4, $5, $6 FROM org o WHERE o.id = $1
		   AND ($2::bigint IS NULL OR EXISTS (SELECT 1 FROM org_fact f WHERE f.id = $2 AND f.org_id = o.id AND f.kind = 'phone'))
		 RETURNING id`,
		r.OrgID, r.FactID, r.LineKind, r.Role, r.Model, r.CostMicros).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("leadstore: record phone research: %w: no such org, or the fact is not a phone of it", ErrBadPhoneResearch)
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: record phone research: %w", err)
	}
	return id, nil
}

// markWrongNumber is RecordCallOutcome's phone status step for call message messageID,
// which dials phone fact factID and has just got outcome (row outcomeID) after prev (the
// call's previous latest outcome, "" when none): wrong_number marks the fact bad and cancels
// every other pending call on it (reason wrong_number, the engine re-run); any other
// outcome replacing a wrong_number marks it good again (corrected). who is the stored actor.
func (q Queries) markWrongNumber(ctx context.Context, messageID, factID, outcomeID int64, outcome, prev, who string) error {
	switch {
	case outcome == campaign.OutcomeWrongNumber:
		if _, err := q.insertPhoneStatus(ctx, factID, PhoneBad, PhoneReasonWrongNumber, &outcomeID, who); err != nil {
			return err
		}
		// Every other pending call on the fact (another lead of the org, another campaign)
		// can no longer be made: cancel it, and move its lead's sequence on without it.
		return q.cancelCallsOnFact(ctx, factID, messageID, campaign.CancelWrongNumber)
	case prev == campaign.OutcomeWrongNumber:
		_, err := q.insertPhoneStatus(ctx, factID, PhoneGood, PhoneReasonCorrected, &outcomeID, who)
		return err
	}
	return nil
}

// Forgotten counts what ForgetOrg did; a second run on the same org counts zero everywhere.
type Forgotten struct {
	PhoneFacts      int64 // org_fact phone rows deleted
	PhoneStatuses   int64 // phone_status rows that went with them (ON DELETE CASCADE)
	PhoneResearch   int64 // phone_research rows deleted
	Screenings      int64 // phone_screening rows of the org's numbers deleted
	NotesBlanked    int64 // call_outcome notes blanked (the rows stay)
	PlacesBlanked   int64 // place rows whose map-listing phones and emails were emptied (the rows stay)
	Suppressions    int   // suppressions newly written (domain and hashed phones)
	LeadsSuppressed int   // leads moved to suppressed by them
	Cancelled       int64 // pending messages cancelled by them
	Consents        int64 // email_consent rows of the org deleted (the address they gave is theirs)
	ConsentsBlanked int64 // call outcomes on its leads' calls whose consent and address were cleared (the rows stay)
}

// ForgetOrg erases org orgID's phone data, for `leads forget` (run on the owner's
// connection, like migrate: the app role holds no DELETE and no UPDATE on these tables, so
// under it this fails with insufficient_privilege and changes nothing). In one transaction
// it writes, for every account with a lead of the org (the default account when none), a
// domain suppression of the org's domain and a phone suppression of each of its numbers
// (PhoneSuppressionValue of the stored value and of its E.164 form), all reason manual with
// source "forget: <reason>", so the org is never collected or called again and its pending
// messages are cancelled; then deletes the org's phone research rows, the screenings of
// its numbers (both forms), and its phone facts (their statuses go by ON DELETE CASCADE,
// outreach_message.phone_fact_id is cleared by ON DELETE SET NULL), and blanks the notes of
// the call outcomes on its leads' messages, keeping the rows, deletes its email_consent
// rows and clears the consent and consent address of those call outcomes (the address a
// person gave on a call is theirs), and empties the map-listing phones and emails of its
// places (00021), keeping the rows. A screening of a number
// another org shares goes too: that org must be screened again (the safe side).
//
// reason says why (at most MaxForgetReason characters, no line break, no @: never a
// person's address). It returns counts only, and is idempotent: run again (after a backup
// restore, say), it finds nothing left and counts zero. A missing org is ErrOrgNotFound.
// The org's other facts (descriptions, social profiles), its routes and the fetched pages
// in http_fetch are not touched.
func (q Queries) ForgetOrg(ctx context.Context, orgID int64, reason string) (Forgotten, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxForgetReason || strings.ContainsAny(reason, "\r\n@") {
		return Forgotten{}, fmt.Errorf("leadstore: forget org %d: %w", orgID, ErrBadForgetReason)
	}
	var res Forgotten
	err := q.inTx(ctx, func(tq Queries) error {
		var domain, country string
		err := tq.db.QueryRow(ctx, `SELECT domain, country FROM org WHERE id = $1 FOR UPDATE`, orgID).Scan(&domain, &country)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrgNotFound
		}
		if err != nil {
			return err
		}
		type fact struct {
			ID         int64  `db:"id"`
			Normalized string `db:"normalized"`
		}
		facts, err := collect(ctx, tq.db, pgx.RowToStructByName[fact],
			`SELECT id, normalized FROM org_fact WHERE org_id = $1 AND kind = 'phone' ORDER BY id`, orgID)
		if err != nil {
			return err
		}
		var factIDs []int64
		forms := []string{}
		for _, f := range facts {
			factIDs = append(factIDs, f.ID)
			for _, n := range []string{f.Normalized, dial.Number(f.Normalized, country)} {
				if n != "" && !slices.Contains(forms, n) {
					forms = append(forms, n)
				}
			}
		}
		accounts, err := collect(ctx, tq.db, pgx.RowTo[int64],
			`SELECT DISTINCT c.account_id FROM lead l JOIN campaign c ON c.id = l.campaign_id
			 WHERE l.org_id = $1 ORDER BY 1`, orgID)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			accounts = []int64{DefaultAccountID}
		}
		source := "forget: " + reason
		add := func(s Suppressed) {
			if s.Recorded {
				res.Suppressions++
			}
			res.LeadsSuppressed += s.Moved
			res.Cancelled += s.Cancelled
		}
		// Phones first: their suppression reaches leads through the facts about to go.
		for _, acct := range accounts {
			for _, n := range forms {
				s, err := tq.Suppress(ctx, acct, SuppressPhone, PhoneSuppressionValue(n), ReasonManual, source)
				if err != nil {
					return err
				}
				add(s)
			}
			s, err := tq.Suppress(ctx, acct, SuppressDomain, domain, ReasonManual, source)
			if err != nil {
				return err
			}
			add(s)
		}
		if err := tq.db.QueryRow(ctx, `SELECT count(*) FROM phone_status WHERE fact_id = ANY($1)`, factIDs).
			Scan(&res.PhoneStatuses); err != nil {
			return err
		}
		for _, step := range []struct {
			n    *int64
			sql  string
			args []any
		}{
			{&res.PhoneResearch, `DELETE FROM phone_research WHERE org_id = $1`, []any{orgID}},
			{&res.Screenings, `DELETE FROM phone_screening WHERE number = ANY($1)`, []any{forms}},
			{&res.PhoneFacts, `DELETE FROM org_fact WHERE id = ANY($1)`, []any{factIDs}},
			{&res.NotesBlanked, `UPDATE call_outcome SET note = '' WHERE note <> '' AND message_id IN
			   (SELECT m.id FROM outreach_message m JOIN lead l ON l.id = m.lead_id WHERE l.org_id = $1)`, []any{orgID}},
			// The address a person gave with their consent is theirs: the consent rows go, and
			// each call outcome keeps its outcome without the consent (both columns in one
			// UPDATE, so call_outcome_consent_check holds). The domain suppression above has
			// already cancelled the emails that would have gone to it.
			{&res.Consents, `DELETE FROM email_consent WHERE org_id = $1`, []any{orgID}},
			{&res.ConsentsBlanked, `UPDATE call_outcome SET email_consent = false, consent_address = NULL
			   WHERE (email_consent OR consent_address IS NOT NULL) AND message_id IN
			   (SELECT m.id FROM outreach_message m JOIN lead l ON l.id = m.lead_id WHERE l.org_id = $1)`, []any{orgID}},
			{&res.PlacesBlanked, `UPDATE place SET phones = '[]'::jsonb, emails = '[]'::jsonb
			   WHERE org_id = $1 AND (phones <> '[]'::jsonb OR emails <> '[]'::jsonb)`, []any{orgID}},
		} {
			tag, err := tq.db.Exec(ctx, step.sql, step.args...)
			if err != nil {
				return err
			}
			*step.n = tag.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return Forgotten{}, fmt.Errorf("leadstore: forget org %d: %w", orgID, err)
	}
	return res, nil
}
