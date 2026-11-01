package leadstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/dial"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/jackc/pgx/v5"
)

// The operator's own contacts (00029). Routes, facts and people are fixed at insert and a value
// exists once per org, so what he does to one is a contact_mark on its row: the latest mark
// decides. A claim makes it his and carries his note; a retire takes it out of use. A changed
// value is a new or claimed row plus a retire on the old one, naming its replacement.

// DerivedByOperator marks a row the operator typed in: it came from no page and no place.
const DerivedByOperator = "operator@1"

// The marks a contact can carry (contact_mark_mark_check).
const (
	MarkClaim  = "claim"
	MarkRetire = "retire"
)

// ContactPhone and ContactPerson are the Kind of a Contact that is not a route.
const (
	ContactPhone  = "phone"
	ContactPerson = "person"
)

// Where a contact came from (Contact.Provenance).
const (
	ProvenanceOperator = "operator"
	ProvenanceResearch = "research"
	ProvenanceCrawl    = "crawl"
	ProvenanceOverture = "overture"
	ProvenanceEnrich   = "enrich"
)

// MaxContactNoteRunes is the longest note on a contact (contact_mark_note_check).
const MaxContactNoteRunes = 200

// maxContactURLRunes is the longest form page URL the operator may type.
const maxContactURLRunes = 500

var (
	// ErrBadContact is a contact or mark that cannot be stored; the wrapped text says why, in
	// words the operator can read.
	ErrBadContact = errors.New("leadstore: bad contact")
	// ErrContactNotFound is a route, phone or person that is not the org's.
	ErrContactNotFound = errors.New("leadstore: contact not found")
)

var operatorEmailRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// contactLatestMarkSQL is a SQL expression for the latest mark of a contact: column is
// route_id, fact_id or person_id, and id an SQL expression. It is NULL when there is no mark.
func contactLatestMarkSQL(column, id string) string {
	return `(SELECT cm.mark FROM contact_mark cm WHERE cm.` + column + ` = ` + id + ` ORDER BY cm.id DESC LIMIT 1)`
}

// MessageTargetRetiredSQL is a SQL condition true when message alias msg was written to a route
// of org (an SQL expression) that has since been retired. An approved message keeps the target it
// was approved with, so the queues that send or show it read the route's latest mark.
func MessageTargetRetiredSQL(msg, org string) string {
	return `EXISTS (SELECT 1 FROM contact_route tr WHERE tr.org_id = ` + org + ` AND tr.kind = ` + msg + `.channel
	   AND lower(tr.value) = lower(` + msg + `.to_address) AND ` + contactLatestMarkSQL("route_id", "tr.id") + ` IS NOT DISTINCT FROM 'retire')`
}

// OperatorRoute is a route the operator adds.
type OperatorRoute struct {
	OrgID    int64
	Kind     string // RouteEmail, RouteForm or RouteLinkedIn
	Value    string // the address, the form page URL or the LinkedIn URL
	Note     string // his words; "" for none
	PersonID int64  // the person it belongs to (email or LinkedIn only); 0 for none
}

// OperatorPhone is a phone number the operator adds.
type OperatorPhone struct {
	OrgID int64
	Value string // as typed; a national number takes the org's country code
	Note  string
}

// ContactAdded says what adding or editing a contact did.
type ContactAdded struct {
	ID         int64 // the row now holding the value
	Added      bool  // a new row was written; false: already there, and now marked as his
	WasRetired bool  // the row was retired until this claim brought it back
	Replaced   bool  // EditOperatorContact: another row was retired in its favour
}

// AddOperatorRoute writes r as the operator's own route (derived_by operator@1, no fetch, mx_ok
// false) with a claim, or claims the row the org already has for that value, un-retiring it.
// An existing row keeps the person it was written with. His email skips the role-address filter,
// and the email check before any send stays the real gate.
func (q Queries) AddOperatorRoute(ctx context.Context, r OperatorRoute) (ContactAdded, error) {
	value, err := operatorRouteValue(r.Kind, r.Value)
	if err != nil {
		return ContactAdded{}, fmt.Errorf("leadstore: add route: %w", err)
	}
	note, err := contactNote(r.Note)
	if err != nil {
		return ContactAdded{}, fmt.Errorf("leadstore: add route: %w", err)
	}
	if r.PersonID != 0 && r.Kind == RouteForm {
		return ContactAdded{}, fmt.Errorf("leadstore: add route: %w: a form cannot belong to a person", ErrBadContact)
	}
	var res ContactAdded
	err = q.inTx(ctx, func(tq Queries) error {
		if r.PersonID != 0 {
			if err := tq.checkLivePerson(ctx, r.OrgID, r.PersonID); err != nil {
				return err
			}
		}
		res, err = tq.addOrClaimRoute(ctx, r.OrgID, r.Kind, value, note, r.PersonID)
		return err
	})
	if err != nil {
		return ContactAdded{}, fmt.Errorf("leadstore: add route: %w", err)
	}
	return res, nil
}

// AddOperatorPhone writes p as the operator's own phone fact with a claim, or claims the fact
// the org already has for that number, in E.164 or national form, un-retiring it.
func (q Queries) AddOperatorPhone(ctx context.Context, p OperatorPhone) (ContactAdded, error) {
	note, err := contactNote(p.Note)
	if err != nil {
		return ContactAdded{}, fmt.Errorf("leadstore: add phone: %w", err)
	}
	var res ContactAdded
	err = q.inTx(ctx, func(tq Queries) error {
		res, err = tq.addOrClaimPhone(ctx, p.OrgID, p.Value, note)
		return err
	})
	if err != nil {
		return ContactAdded{}, fmt.Errorf("leadstore: add phone: %w", err)
	}
	return res, nil
}

// Mark is one mark on the org's route, phone or person: exactly one of RouteID, FactID and
// PersonID is set.
type Mark struct {
	OrgID             int64
	RouteID           int64
	FactID            int64
	PersonID          int64
	Mark              string // MarkClaim or MarkRetire
	Note              string
	ReplacedByRouteID int64 // a retired route's replacement; 0 for none
	ReplacedByFactID  int64 // a retired phone's replacement; 0 for none
}

// MarkContact appends m. A target or a replacement that is not the org's is ErrContactNotFound.
func (q Queries) MarkContact(ctx context.Context, m Mark) error {
	note, err := contactNote(m.Note)
	if err != nil {
		return fmt.Errorf("leadstore: mark contact: %w", err)
	}
	targets := 0
	for _, id := range []int64{m.RouteID, m.FactID, m.PersonID} {
		if id != 0 {
			targets++
		}
	}
	switch {
	case targets != 1:
		return fmt.Errorf("leadstore: mark contact: %w: it needs one route, phone or person", ErrBadContact)
	case m.Mark != MarkClaim && m.Mark != MarkRetire:
		return fmt.Errorf("leadstore: mark contact: %w: unknown mark %q", ErrBadContact, m.Mark)
	case m.ReplacedByRouteID != 0 && (m.Mark != MarkRetire || m.RouteID == 0 || m.ReplacedByRouteID == m.RouteID),
		m.ReplacedByFactID != 0 && (m.Mark != MarkRetire || m.FactID == 0 || m.ReplacedByFactID == m.FactID):
		return fmt.Errorf("leadstore: mark contact: %w: a replacement is another row of the same sort, on a retire", ErrBadContact)
	}
	err = q.inTx(ctx, func(tq Queries) error {
		for _, c := range []struct {
			id    int64
			table string
		}{
			{m.RouteID, `contact_route x WHERE true`},
			{m.FactID, `org_fact x WHERE x.kind = 'phone'`},
			{m.PersonID, `org_person x WHERE true`},
			{m.ReplacedByRouteID, `contact_route x WHERE true`},
			{m.ReplacedByFactID, `org_fact x WHERE x.kind = 'phone'`},
		} {
			if c.id == 0 {
				continue
			}
			var ok bool
			if err := tq.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+c.table+` AND x.id = $1 AND x.org_id = $2)`,
				c.id, m.OrgID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrContactNotFound
			}
		}
		return tq.writeContactMark(ctx, m, note)
	})
	if err != nil {
		return fmt.Errorf("leadstore: mark contact: %w", err)
	}
	return nil
}

// ContactEdit changes one of the org's contacts: exactly one of RouteID, FactID and PersonID is
// set. Value is a route's or a phone's new value; Name and Role a person's.
type ContactEdit struct {
	OrgID    int64
	RouteID  int64
	FactID   int64
	PersonID int64
	Value    string
	Name     string
	Role     string
	Note     string
}

// EditOperatorContact changes e's contact in one transaction. The same value (after
// normalising) is a new claim carrying the note, nothing else. A new value is added or claimed
// like AddOperatorRoute or AddOperatorPhone, and the old row is retired naming it; the kind, and
// a route's person, carry over. A person's changed name or role is a new person and a retire on
// the old one: a route written for the old person keeps pointing at it, because a route's
// person is fixed at insert.
func (q Queries) EditOperatorContact(ctx context.Context, e ContactEdit) (ContactAdded, error) {
	note, err := contactNote(e.Note)
	if err != nil {
		return ContactAdded{}, fmt.Errorf("leadstore: edit contact: %w", err)
	}
	var res ContactAdded
	err = q.inTx(ctx, func(tq Queries) error {
		var err error
		switch {
		case e.RouteID != 0 && e.FactID == 0 && e.PersonID == 0:
			res, err = tq.editRoute(ctx, e, note)
		case e.FactID != 0 && e.RouteID == 0 && e.PersonID == 0:
			res, err = tq.editPhone(ctx, e, note)
		case e.PersonID != 0 && e.RouteID == 0 && e.FactID == 0:
			res, err = tq.editPerson(ctx, e, note)
		default:
			err = fmt.Errorf("%w: it needs one route, phone or person", ErrBadContact)
		}
		return err
	})
	if err != nil {
		return ContactAdded{}, fmt.Errorf("leadstore: edit contact: %w", err)
	}
	return res, nil
}

func (q Queries) editRoute(ctx context.Context, e ContactEdit, note *string) (ContactAdded, error) {
	var kind string
	var personID int64
	err := q.db.QueryRow(ctx, `SELECT kind, coalesce(person_id, 0) FROM contact_route WHERE id = $1 AND org_id = $2`,
		e.RouteID, e.OrgID).Scan(&kind, &personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContactAdded{}, ErrContactNotFound
	}
	if err != nil {
		return ContactAdded{}, err
	}
	value, err := operatorRouteValue(kind, e.Value)
	if err != nil {
		return ContactAdded{}, err
	}
	res, err := q.addOrClaimRoute(ctx, e.OrgID, kind, value, note, personID)
	if err != nil {
		return ContactAdded{}, err
	}
	if res.ID != e.RouteID {
		res.Replaced = true
		err = q.writeContactMark(ctx, Mark{RouteID: e.RouteID, Mark: MarkRetire, ReplacedByRouteID: res.ID}, nil)
	}
	return res, err
}

func (q Queries) editPhone(ctx context.Context, e ContactEdit, note *string) (ContactAdded, error) {
	var ok bool
	if err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_fact WHERE id = $1 AND org_id = $2 AND kind = 'phone')`,
		e.FactID, e.OrgID).Scan(&ok); err != nil {
		return ContactAdded{}, err
	}
	if !ok {
		return ContactAdded{}, ErrContactNotFound
	}
	res, err := q.addOrClaimPhone(ctx, e.OrgID, e.Value, note)
	if err != nil {
		return ContactAdded{}, err
	}
	if res.ID != e.FactID {
		res.Replaced = true
		err = q.writeContactMark(ctx, Mark{FactID: e.FactID, Mark: MarkRetire, ReplacedByFactID: res.ID}, nil)
	}
	return res, err
}

// addOrClaimRoute writes the row if the org has no such value, then the claim. value is
// already normalised and person already checked.
func (q Queries) addOrClaimRoute(ctx context.Context, orgID int64, kind, value string, note *string, personID int64) (ContactAdded, error) {
	res := ContactAdded{Added: true}
	err := q.db.QueryRow(ctx,
		`INSERT INTO contact_route (org_id, kind, value, mx_ok, derived_by, person_id)
		 VALUES ($1, $2, $3, false, $4, nullif($5::bigint, 0))
		 ON CONFLICT (org_id, kind, value) DO NOTHING RETURNING id`,
		orgID, kind, value, DerivedByOperator, personID).Scan(&res.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		res.Added = false
		err = q.db.QueryRow(ctx,
			`SELECT r.id, `+contactLatestMarkSQL("route_id", "r.id")+` IS NOT DISTINCT FROM 'retire'
			 FROM contact_route r WHERE r.org_id = $1 AND r.kind = $2 AND r.value = $3`,
			orgID, kind, value).Scan(&res.ID, &res.WasRetired)
	}
	if err != nil {
		return ContactAdded{}, err
	}
	return res, q.writeContactMark(ctx, Mark{RouteID: res.ID, Mark: MarkClaim}, note)
}

// addOrClaimPhone turns raw into its E.164 form the way phones.E164 does (that package imports
// this one), finds the org's fact for that number, whichever form it was stored in, or writes
// one, then the claim.
func (q Queries) addOrClaimPhone(ctx context.Context, orgID int64, raw string, note *string) (ContactAdded, error) {
	var country string
	err := q.db.QueryRow(ctx, `SELECT coalesce(country, '') FROM org WHERE id = $1`, orgID).Scan(&country)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContactAdded{}, ErrOrgNotFound
	}
	if err != nil {
		return ContactAdded{}, err
	}
	shown, norm, ok := enrich.NormalizePhone(raw)
	number := ""
	if ok {
		number = dial.Number(norm, country)
	}
	if n := len(number) - 1; n < 8 || n > 15 || utf8.RuneCountInString(shown) > MaxFactRunes {
		return ContactAdded{}, fmt.Errorf("%w: that does not look like a phone number; try it with the country code", ErrBadContact)
	}

	find := func() (ContactAdded, bool, error) {
		type row struct {
			ID         int64  `db:"id"`
			Normalized string `db:"normalized"`
			Retired    bool   `db:"retired"`
		}
		rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
			`SELECT f.id, f.normalized, `+contactLatestMarkSQL("fact_id", "f.id")+` IS NOT DISTINCT FROM 'retire' AS retired
			 FROM org_fact f WHERE f.org_id = $1 AND f.kind = 'phone' ORDER BY f.id`, orgID)
		if err != nil {
			return ContactAdded{}, false, err
		}
		for _, r := range rows {
			if r.Normalized == number || dial.Number(r.Normalized, country) == number {
				return ContactAdded{ID: r.ID, WasRetired: r.Retired}, true, nil
			}
		}
		return ContactAdded{}, false, nil
	}
	res, found, err := find()
	if err != nil {
		return ContactAdded{}, err
	}
	if !found {
		res.Added = true
		err = q.db.QueryRow(ctx,
			`INSERT INTO org_fact (org_id, kind, value, normalized, derived_by) VALUES ($1, 'phone', $2, $3, $4)
			 ON CONFLICT (org_id, kind, normalized) DO NOTHING RETURNING id`,
			orgID, shown, number, DerivedByOperator).Scan(&res.ID)
		if errors.Is(err, pgx.ErrNoRows) { // written by someone else since the look above
			res, _, err = find()
		}
		if err != nil {
			return ContactAdded{}, err
		}
	}
	return res, q.writeContactMark(ctx, Mark{FactID: res.ID, Mark: MarkClaim}, note)
}

// writeContactMark appends m with note as is; its target and replacement are already checked.
func (q Queries) writeContactMark(ctx context.Context, m Mark, note *string) error {
	if m.Mark == MarkRetire && (m.RouteID != 0 || m.FactID != 0) {
		if err := q.cancelForRetired(ctx, m.RouteID, m.FactID); err != nil {
			return err
		}
	}
	_, err := q.db.Exec(ctx,
		`INSERT INTO contact_mark (route_id, fact_id, person_id, mark, note, replaced_by_route_id, replaced_by_fact_id, actor)
		 VALUES (nullif($1::bigint, 0), nullif($2::bigint, 0), nullif($3::bigint, 0), $4, $5,
		   nullif($6::bigint, 0), nullif($7::bigint, 0), 'operator')`,
		m.RouteID, m.FactID, m.PersonID, m.Mark, note, m.ReplacedByRouteID, m.ReplacedByFactID)
	return err
}

// CancelContactRetired is the cancel_reason of a message whose route or phone was retired.
const CancelContactRetired = "contact retired"

// cancelForRetired cancels every unsent (draft or approved) message aimed at route routeID or
// phone fact factID, shadow or live, whatever its batch: a call on the fact, or an email, form or
// LinkedIn message of the route's org whose target is the route's value. A claim does not revive
// them; the operator drafts again. Sent, done and maybe_sent rows are left, and a batch's hashes
// read no message state, so its other messages still send. The engine then runs for each lead
// touched, and a batched lead left with nothing live to do goes back to new, as a discarded
// batch's does, off the retired route if it was on it. Run it in a transaction; it locks each
// lead, then its messages, as HealSequences does.
func (q Queries) cancelForRetired(ctx context.Context, routeID, factID int64) error {
	const aimed = `m.state IN ('draft', 'approved') AND (
	     (m.channel = 'phone' AND m.phone_fact_id = $2::bigint)
	     OR (m.channel <> 'phone' AND EXISTS (SELECT 1 FROM contact_route r JOIN lead l ON l.org_id = r.org_id
	       WHERE r.id = $1::bigint AND l.id = m.lead_id AND r.kind = m.channel AND lower(r.value) = lower(m.to_address))))`
	ids, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT DISTINCT m.lead_id FROM outreach_message m WHERE `+aimed+` ORDER BY 1`, routeID, factID)
	if err != nil || len(ids) == 0 {
		return err
	}
	if _, err := q.db.Exec(ctx, `SELECT 1 FROM lead WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids); err != nil {
		return err
	}
	if _, err := q.db.Exec(ctx, `SELECT 1 FROM outreach_message WHERE lead_id = ANY($1) ORDER BY lead_id, id FOR UPDATE`, ids); err != nil {
		return err
	}
	if factID != 0 {
		if err := q.cancelCallsOnFact(ctx, factID, 0, CancelContactRetired); err != nil {
			return err
		}
	} else {
		if _, err := q.db.Exec(ctx, `UPDATE outreach_message m SET state = 'cancelled', cancel_reason = $3 WHERE `+aimed,
			routeID, factID, CancelContactRetired); err != nil {
			return err
		}
		if _, err := q.resequence(ctx, ids, time.Now(), nil); err != nil {
			return err
		}
	}
	idle, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT l.id FROM lead l WHERE l.id = ANY($1) AND l.state = 'batched' AND NOT EXISTS (
		   SELECT 1 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		   WHERE m.lead_id = l.id AND NOT b.shadow AND b.discarded_at IS NULL
		     AND m.state NOT IN ('cancelled', 'skipped', 'refused')) ORDER BY l.id`, ids)
	if err != nil {
		return err
	}
	for _, id := range idle {
		if _, err := q.setLeadStateFrom(ctx, id, []string{LeadBatched}, LeadNew, "contact retired, nothing left to send", ActorSystem); err != nil {
			return err
		}
		if _, err := q.db.Exec(ctx, `UPDATE lead SET channel = 'none', route_id = NULL WHERE id = $1 AND state = 'new' AND route_id = $2`, id, routeID); err != nil {
			return err
		}
	}
	return nil
}

// operatorRouteValue is what the operator typed as the stored value of a route of kind, or
// ErrBadContact.
func operatorRouteValue(kind, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	switch kind {
	case RouteEmail:
		value = strings.ToLower(value)
		if !operatorEmailRE.MatchString(value) {
			return "", fmt.Errorf("%w: that does not look like an email address", ErrBadContact)
		}
	case RouteForm:
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.ContainsAny(value, " \t\r\n") ||
			utf8.RuneCountInString(value) > maxContactURLRunes {
			return "", fmt.Errorf("%w: a form needs its full page address, starting https://", ErrBadContact)
		}
	case RouteLinkedIn:
		canonical, _, _, ok := enrich.LinkedInURL(value)
		if !ok {
			return "", fmt.Errorf("%w: that is not a LinkedIn company, school or profile address", ErrBadContact)
		}
		value = canonical
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownRouteKind, kind)
	}
	return value, nil
}

// contactNote is note trimmed, nil when blank, or ErrBadContact.
func contactNote(note string) (*string, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(note) > MaxContactNoteRunes {
		return nil, fmt.Errorf("%w: a note is at most %d characters", ErrBadContact, MaxContactNoteRunes)
	}
	return &note, nil
}

// Contact is one of an org's routes, phones or people, for the lead page.
type Contact struct {
	Kind       string    `db:"kind"`  // RouteEmail, RouteForm, RouteLinkedIn, ContactPhone or ContactPerson
	ID         int64     `db:"id"`    // the row: contact_route, org_fact or org_person, by Kind
	Value      string    `db:"value"` // the route's value, the phone as shown, a person's name
	PersonID   int64     `db:"person_id"`
	Name       string    `db:"name"` // the person's, for a person or a route of one
	Role       string    `db:"role"`
	DerivedBy  string    `db:"derived_by"`
	FoundAt    time.Time `db:"found_at"`
	Operator   bool      `db:"operator"`    // the latest mark is a claim
	Retired    bool      `db:"retired"`     // the latest mark is a retire
	Note       string    `db:"note"`        // the latest mark's note
	ReplacedBy int64     `db:"replaced_by"` // a retired route's or phone's replacement; 0 for none
	Provenance string    `db:"-"`           // a Provenance constant
}

// ContactsFor returns every route, phone and person of org orgID, retired ones included and
// flagged: routes by kind, then phones, then people; within each, live before retired, the
// operator's first, then in the order found.
func (q Queries) ContactsFor(ctx context.Context, orgID int64) ([]Contact, error) {
	latest := func(column, id string) string {
		return `LEFT JOIN LATERAL (SELECT cm.mark, cm.note, coalesce(cm.replaced_by_route_id, cm.replaced_by_fact_id) AS replaced_by
		   FROM contact_mark cm WHERE cm.` + column + ` = ` + id + ` ORDER BY cm.id DESC LIMIT 1) m ON true`
	}
	const marks = `coalesce(m.mark = 'claim', false) AS operator, coalesce(m.mark = 'retire', false) AS retired,
		   coalesce(m.note, '') AS note, coalesce(m.replaced_by, 0) AS replaced_by`
	contacts, err := collect(ctx, q.db, pgx.RowToStructByName[Contact],
		`SELECT kind, id, value, person_id, name, role, derived_by, found_at, operator, retired, note, replaced_by FROM (
		   SELECT 0 AS sort, r.kind, r.id, r.value, coalesce(r.person_id, 0) AS person_id, coalesce(p.name, '') AS name,
		     coalesce(p.role, '') AS role, r.derived_by, r.found_at, `+marks+`
		   FROM contact_route r LEFT JOIN org_person p ON p.id = r.person_id `+latest("route_id", "r.id")+`
		   WHERE r.org_id = $1
		   UNION ALL
		   SELECT 1, 'phone', f.id, f.value, 0::bigint, '', '', f.derived_by, f.found_at, `+marks+`
		   FROM org_fact f `+latest("fact_id", "f.id")+`
		   WHERE f.org_id = $1 AND f.kind = 'phone'
		   UNION ALL
		   SELECT 2, 'person', p.id, coalesce(p.name, ''), p.id, coalesce(p.name, ''), p.role, p.derived_by, p.at, `+marks+`
		   FROM org_person p `+latest("person_id", "p.id")+`
		   WHERE p.org_id = $1
		 ) c ORDER BY sort, kind, retired, operator DESC, id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: contacts for org %d: %w", orgID, err)
	}
	for i := range contacts {
		contacts[i].Provenance = contactProvenance(contacts[i].DerivedBy, contacts[i].Operator)
	}
	return contacts, nil
}

// contactProvenance is where a contact came from: its derived_by, overridden by the operator's
// claim. Any other derived_by was read from one of the org's pages.
func contactProvenance(derivedBy string, claimed bool) string {
	if claimed {
		return ProvenanceOperator
	}
	name, _, _ := strings.Cut(derivedBy, "@")
	switch name {
	case "operator":
		return ProvenanceOperator
	case "claude-research":
		return ProvenanceResearch
	case "overture":
		return ProvenanceOverture
	case "enrich":
		return ProvenanceEnrich
	}
	return ProvenanceCrawl
}
