package leadstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The proof library and an org's sector (00029). The closed sector list is leadview.Sectors,
// which imports this package, so here the checks on org_sector and proof hold it; a name not
// on the list comes back as ErrBadSector or ErrBadProof. A proof is fixed at insert and
// proof_retire takes it out of use. Both proof and org_sector are append-only for the app.

// The text limits of a proof, in characters (the proof checks).
const (
	MaxProofTitle  = 80
	MaxProofLine   = 240
	MaxProofURL    = 500
	MaxProofReason = 200
)

// The sources of an org_sector row (org_sector_source_check).
const (
	SectorFromRule     = "rule"
	SectorFromOperator = "operator"
)

// proofsLimitMax is the most rows ProofsFor returns.
const proofsLimitMax = 50

var (
	// ErrBadProof is returned, wrapped with a sentence saying why, for proof input the table
	// would refuse; nothing is written.
	ErrBadProof = errors.New("bad proof")
	// ErrProofNotFound is returned when the account has no proof of that id.
	ErrProofNotFound = errors.New("leadstore: proof not found")
	// ErrBadSector is returned, wrapped, for a sector not on the closed list.
	ErrBadSector = errors.New("bad sector")
)

// Proof is one line of proof.
type Proof struct {
	ID        int64     `db:"id"`
	AccountID int64     `db:"account_id"`
	Title     string    `db:"title"`
	Line      string    `db:"line"` // the sentence a draft may use, verbatim
	URL       string    `db:"url"`  // "" when none; never carried into a draft unless Public
	Sectors   []string  `db:"sectors"`
	Public    bool      `db:"public"`
	Actor     string    `db:"actor"`
	At        time.Time `db:"at"`
}

// proofCheckViolation is true for an error of a table check.
func proofCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// isOutreachLink is true for u (trimmed, not empty) as the proof and proposal link checks want
// it: https, with a host and no login, no white space, at most MaxProofURL characters.
func isOutreachLink(u string) bool {
	p, err := url.Parse(u)
	return err == nil && strings.HasPrefix(u, "https://") && p.Host != "" && p.User == nil &&
		!strings.ContainsFunc(u, unicode.IsSpace) && utf8.RuneCountInString(u) <= MaxProofURL
}

// proofNullText is a pointer to s, or nil for "".
func proofNullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AddProof stores proof p for its account (AccountID 0: DefaultAccountID) by p.Actor (ActorOperator
// or ActorSystem) and returns its id. Title (one line, at most MaxProofTitle characters), Line
// (one line, at most MaxProofLine) and URL ("" or an https:// address, at most MaxProofURL) are
// trimmed. Sectors must be at least one, each on the closed list. ID and At are ignored. Bad
// input is ErrBadProof.
func (q Queries) AddProof(ctx context.Context, p Proof) (int64, error) {
	p.Title, p.Line, p.URL = strings.TrimSpace(p.Title), strings.TrimSpace(p.Line), strings.TrimSpace(p.URL)
	var bad string
	switch {
	case p.Actor != ActorOperator && p.Actor != ActorSystem:
		return 0, fmt.Errorf("leadstore: add proof: %w: %q", ErrUnknownActor, p.Actor)
	case p.Title == "":
		bad = "a proof needs a title"
	case strings.ContainsAny(p.Title, "\r\n"):
		bad = "the title must be one line"
	case utf8.RuneCountInString(p.Title) > MaxProofTitle:
		bad = fmt.Sprintf("the title is over %d characters", MaxProofTitle)
	case p.Line == "":
		bad = "a proof needs a line"
	case strings.ContainsAny(p.Line, "\r\n"):
		bad = "the line must be one line"
	case utf8.RuneCountInString(p.Line) > MaxProofLine:
		bad = fmt.Sprintf("the line is over %d characters", MaxProofLine)
	case p.URL != "" && !isOutreachLink(p.URL):
		bad = "the link must be an https:// address"
	case len(p.Sectors) == 0:
		bad = "a proof needs at least one sector"
	}
	if bad != "" {
		return 0, fmt.Errorf("leadstore: add proof: %w: %s", ErrBadProof, bad)
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO proof (account_id, title, line, url, sectors, public, actor)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		orAccount(p.AccountID), p.Title, p.Line, proofNullText(p.URL), p.Sectors, p.Public, p.Actor).Scan(&id)
	if proofCheckViolation(err) {
		return 0, fmt.Errorf("leadstore: add proof: %w: a sector is not on the list", ErrBadProof)
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: add proof: %w", err)
	}
	return id, nil
}

// RetireProof takes proof proofID of account accountID (0: DefaultAccountID) out of use, with
// reason (trimmed, at most MaxProofReason characters; "" for none). A proof goes once: retiring
// it again changes nothing and keeps the first reason. A long reason is ErrBadProof; a proof the
// account does not have is ErrProofNotFound.
func (q Queries) RetireProof(ctx context.Context, accountID, proofID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > MaxProofReason {
		return fmt.Errorf("leadstore: retire proof %d: %w: the reason is over %d characters", proofID, ErrBadProof, MaxProofReason)
	}
	var found bool
	err := q.db.QueryRow(ctx,
		`WITH p AS (SELECT id FROM proof WHERE id = $1 AND account_id = $2),
		 retired AS (
		   INSERT INTO proof_retire (proof_id, reason) SELECT id, $3 FROM p ON CONFLICT (proof_id) DO NOTHING RETURNING proof_id)
		 SELECT EXISTS (SELECT 1 FROM p)`, proofID, orAccount(accountID), reason).Scan(&found)
	if err != nil {
		return fmt.Errorf("leadstore: retire proof %d: %w", proofID, err)
	}
	if !found {
		return fmt.Errorf("leadstore: retire proof %d: %w", proofID, ErrProofNotFound)
	}
	return nil
}

// proofActiveSQL selects the active proofs of account $1, as a Proof's columns.
const proofActiveSQL = `SELECT p.id, p.account_id, p.title, p.line, coalesce(p.url, '') AS url, p.sectors, p.public, p.actor, p.at
	 FROM proof p
	 WHERE p.account_id = $1 AND NOT EXISTS (SELECT 1 FROM proof_retire r WHERE r.proof_id = p.id)`

// ProofsFor returns the active proofs of account accountID (0: DefaultAccountID) whose sectors
// hold sector, newest first, at most limit (1..50; out of range is clamped). A sector of "" (the
// lead has none) has none.
func (q Queries) ProofsFor(ctx context.Context, accountID int64, sector string, limit int) ([]Proof, error) {
	limit = min(max(limit, 1), proofsLimitMax)
	out, err := collect(ctx, q.db, pgx.RowToStructByName[Proof],
		proofActiveSQL+` AND $2 = ANY(p.sectors) ORDER BY p.id DESC LIMIT $3`, orAccount(accountID), sector, limit)
	if err != nil {
		return nil, fmt.Errorf("leadstore: proofs for %q: %w", sector, err)
	}
	return out, nil
}

// ListProofs returns every active proof of account accountID (0: DefaultAccountID), newest
// first.
func (q Queries) ListProofs(ctx context.Context, accountID int64) ([]Proof, error) {
	out, err := collect(ctx, q.db, pgx.RowToStructByName[Proof], proofActiveSQL+` ORDER BY p.id DESC`, orAccount(accountID))
	if err != nil {
		return nil, fmt.Errorf("leadstore: list proofs: %w", err)
	}
	return out, nil
}

// SetSector appends org orgID's sector, by actor: ActorOperator writes the operator's choice
// and ActorSystem a rule's. A sector not on the closed list is ErrBadSector; a missing org is
// ErrOrgNotFound.
func (q Queries) SetSector(ctx context.Context, orgID int64, sector, actor string) error {
	source := SectorFromRule
	switch actor {
	case ActorOperator:
		source = SectorFromOperator
	case ActorSystem:
	default:
		return fmt.Errorf("leadstore: set sector %d: %w: %q", orgID, ErrUnknownActor, actor)
	}
	_, err := q.db.Exec(ctx, `INSERT INTO org_sector (org_id, sector, source, actor) VALUES ($1, $2, $3, $4)`,
		orgID, sector, source, actor)
	var pgErr *pgconn.PgError
	switch {
	case proofCheckViolation(err):
		return fmt.Errorf("leadstore: set sector %d: %w: %q is not on the list", orgID, ErrBadSector, sector)
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return fmt.Errorf("leadstore: set sector %d: %w", orgID, ErrOrgNotFound)
	case err != nil:
		return fmt.Errorf("leadstore: set sector %d: %w", orgID, err)
	}
	return nil
}

// OperatorSector returns the sector the operator last set on org orgID, or "" when he set none.
// A rule's row does not count: leadview.SectorOf works the rule out on read.
func (q Queries) OperatorSector(ctx context.Context, orgID int64) (string, error) {
	var s string
	err := q.db.QueryRow(ctx,
		`SELECT sector FROM org_sector WHERE org_id = $1 AND source = 'operator' ORDER BY id DESC LIMIT 1`, orgID).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("leadstore: operator sector %d: %w", orgID, err)
	}
	return s, nil
}
