package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// The longest person's name and role (org_person_name_check, org_person_role_check).
const (
	MaxPersonNameRunes = 120
	MaxPersonRoleRunes = 60
)

// PersonRow is one org_person row to write: the operator's when FetchID is 0, else read from
// that page.
type PersonRow struct {
	OrgID     int64
	Name      string // only if printed beside the link, or typed
	Role      string
	Note      string // the operator's words; "" for none
	FetchID   int64  // the page it was read from; 0 for the operator's
	DerivedBy string // read from a page: e.g. "claude-research@1"
}

// Person is one org_person row.
type Person struct {
	ID        int64     `db:"id"`
	OrgID     int64     `db:"org_id"`
	Name      string    `db:"name"` // "" when no name was printed or typed
	Role      string    `db:"role"`
	DerivedBy string    `db:"derived_by"`
	FetchID   int64     `db:"fetch_id"` // 0 for the operator's
	At        time.Time `db:"at"`
}

// AddPerson writes p and returns its id. The operator's person (no FetchID) is derived_by
// operator@1 with a claim carrying the note; one read from a page has no mark. A person needs a
// name or a role.
func (q Queries) AddPerson(ctx context.Context, p PersonRow) (int64, error) {
	name, role, err := personText(p.Name, p.Role)
	if err != nil {
		return 0, fmt.Errorf("leadstore: add person: %w", err)
	}
	note, err := contactNote(p.Note)
	if err != nil {
		return 0, fmt.Errorf("leadstore: add person: %w", err)
	}
	var id int64
	err = q.inTx(ctx, func(tq Queries) error {
		id, err = tq.insertPerson(ctx, p, name, role, note)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("leadstore: add person: %w", err)
	}
	return id, nil
}

func (q Queries) insertPerson(ctx context.Context, p PersonRow, name, role string, note *string) (int64, error) {
	derivedBy, actor := p.DerivedBy, "system"
	if p.FetchID == 0 {
		derivedBy, actor = DerivedByOperator, "operator"
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO org_person (org_id, name, role, derived_by, fetch_id, actor)
		 VALUES ($1, nullif($2, ''), $3, $4, nullif($5::bigint, 0), $6) RETURNING id`,
		p.OrgID, name, role, derivedBy, p.FetchID, actor).Scan(&id); err != nil {
		return 0, err
	}
	if p.FetchID != 0 {
		return id, nil
	}
	return id, q.writeContactMark(ctx, Mark{PersonID: id, Mark: MarkClaim}, note)
}

// PeopleFor returns the people of org orgID in the order found, without the retired ones.
func (q Queries) PeopleFor(ctx context.Context, orgID int64) ([]Person, error) {
	people, err := collect(ctx, q.db, pgx.RowToStructByName[Person],
		`SELECT p.id, p.org_id, coalesce(p.name, '') AS name, p.role, p.derived_by, coalesce(p.fetch_id, 0) AS fetch_id, p.at
		 FROM org_person p
		 WHERE p.org_id = $1 AND `+contactLatestMarkSQL("person_id", "p.id")+` IS DISTINCT FROM 'retire'
		 ORDER BY p.id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: people for org %d: %w", orgID, err)
	}
	return people, nil
}

// editPerson is EditOperatorContact for a person: the same name and role is a claim carrying the
// note; a change is a new person of the operator's and a retire on the old one (a mark names
// no replacement person: contact_mark_replaced_check).
func (q Queries) editPerson(ctx context.Context, e ContactEdit, note *string) (ContactAdded, error) {
	name, role, err := personText(e.Name, e.Role)
	if err != nil {
		return ContactAdded{}, err
	}
	var oldName, oldRole string
	var retired bool
	err = q.db.QueryRow(ctx,
		`SELECT coalesce(p.name, ''), p.role, `+contactLatestMarkSQL("person_id", "p.id")+` IS NOT DISTINCT FROM 'retire'
		 FROM org_person p WHERE p.id = $1 AND p.org_id = $2`, e.PersonID, e.OrgID).Scan(&oldName, &oldRole, &retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContactAdded{}, ErrContactNotFound
	}
	if err != nil {
		return ContactAdded{}, err
	}
	if name == oldName && role == oldRole {
		return ContactAdded{ID: e.PersonID, WasRetired: retired}, q.writeContactMark(ctx, Mark{PersonID: e.PersonID, Mark: MarkClaim}, note)
	}
	id, err := q.insertPerson(ctx, PersonRow{OrgID: e.OrgID}, name, role, note)
	if err != nil {
		return ContactAdded{}, err
	}
	return ContactAdded{ID: id, Added: true, Replaced: true}, q.writeContactMark(ctx, Mark{PersonID: e.PersonID, Mark: MarkRetire}, nil)
}

// checkLivePerson is nil when person id is the org's and not retired.
func (q Queries) checkLivePerson(ctx context.Context, orgID, id int64) error {
	var ok bool
	if err := q.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM org_person p WHERE p.id = $1 AND p.org_id = $2
		   AND `+contactLatestMarkSQL("person_id", "p.id")+` IS DISTINCT FROM 'retire')`, id, orgID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrContactNotFound
	}
	return nil
}

// personText is a person's name and role trimmed, or ErrBadContact.
func personText(name, role string) (string, string, error) {
	name, role = strings.TrimSpace(name), strings.TrimSpace(role)
	switch {
	case name == "" && role == "":
		return "", "", fmt.Errorf("%w: a person needs a name or a role", ErrBadContact)
	case utf8.RuneCountInString(name) > MaxPersonNameRunes:
		return "", "", fmt.Errorf("%w: a name is at most %d characters", ErrBadContact, MaxPersonNameRunes)
	case utf8.RuneCountInString(role) > MaxPersonRoleRunes:
		return "", "", fmt.Errorf("%w: a role is at most %d characters", ErrBadContact, MaxPersonRoleRunes)
	case strings.ContainsAny(name+role, "\r\n"):
		return "", "", fmt.Errorf("%w: a name or a role is one line", ErrBadContact)
	}
	return name, role, nil
}
