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
)

// Proposals (00029): a proposal sent to a lead and its statuses, both append-only, latest
// status wins. Every bad input here is ErrBadCRM, like the rest of the CRM's.

// The statuses of a proposal (lead_proposal_status_status_check).
const (
	ProposalSent      = "sent"
	ProposalAccepted  = "accepted"
	ProposalDeclined  = "declined"
	ProposalWithdrawn = "withdrawn"
)

// ProposalStatuses lists every proposal status.
var ProposalStatuses = []string{ProposalSent, ProposalAccepted, ProposalDeclined, ProposalWithdrawn}

// The proposal text limits, in characters (the lead_proposal and lead_proposal_status checks).
const (
	MaxProposalTitle = 120
	MaxProposalNote  = 300
)

// ErrProposalNotFound is returned when no proposal has that id.
var ErrProposalNotFound = errors.New("leadstore: proposal not found")

// NewProposal is a proposal to add.
type NewProposal struct {
	LeadID     int64
	Title      string // one line, at most MaxProposalTitle characters
	URL        string // "" or an https:// address, at most MaxProofURL characters
	ValueCents *int64 // nil when no value is set; never below 0
	Currency   string // one of Currencies; "" is DefaultCurrency
	Status     string // one of ProposalStatuses; "" is ProposalSent
	Note       string // on its first status, at most MaxProposalNote characters
	Actor      string // ActorOperator or ActorSystem
}

// Proposal is one proposal with its latest status.
type Proposal struct {
	ID         int64     `db:"id"`
	LeadID     int64     `db:"lead_id"`
	Title      string    `db:"title"`
	URL        string    `db:"url"` // "" when none
	ValueCents *int64    `db:"value_cents"`
	Currency   string    `db:"currency"`
	Actor      string    `db:"actor"`
	At         time.Time `db:"at"`
	Status     string    `db:"status"`
	StatusNote string    `db:"status_note"`
	StatusAt   time.Time `db:"status_at"`
}

// checkProposalStatus checks status is known and note (trimmed) short enough.
func checkProposalStatus(status, note string) error {
	switch {
	case !slices.Contains(ProposalStatuses, status):
		return fmt.Errorf("%w: unknown proposal status %q", ErrBadCRM, status)
	case utf8.RuneCountInString(note) > MaxProposalNote:
		return fmt.Errorf("%w: the note is over %d characters", ErrBadCRM, MaxProposalNote)
	}
	return nil
}

// checkProposalActor checks actor may write a proposal or its status.
func checkProposalActor(actor string) error {
	if actor != ActorOperator && actor != ActorSystem {
		return fmt.Errorf("%w: %q", ErrUnknownActor, actor)
	}
	return nil
}

// lockProposalLead locks lead leadID until the transaction ends, so two writers of its stage
// take turns; a missing lead is ErrLeadNotFound.
func (q Queries) lockProposalLead(ctx context.Context, leadID int64) error {
	var id int64
	err := q.db.QueryRow(ctx, `SELECT id FROM lead WHERE id = $1 FOR UPDATE`, leadID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeadNotFound
	}
	return err
}

// stageForSentProposal moves locked lead leadID to the proposal stage through SetLeadStage
// when its effective stage is none or talking; a lead further on keeps its stage. The stage is
// read in its own statement after the lock, so it sees whatever the lock waited for.
func (q Queries) stageForSentProposal(ctx context.Context, leadID int64, now time.Time) error {
	var stage string
	if err := q.db.QueryRow(ctx,
		`SELECT `+effectiveStageSQL+` FROM lead l LEFT JOIN lead_crm k ON k.lead_id = l.id WHERE l.id = $1`, leadID).Scan(&stage); err != nil {
		return err
	}
	if stage != "" && stage != StageTalking {
		return nil
	}
	return q.SetLeadStage(ctx, leadID, StageProposal, "", "", now)
}

// insertProposalStatus appends a status to a proposal at now.
func (q Queries) insertProposalStatus(ctx context.Context, proposalID int64, status, note, actor string, now time.Time) error {
	_, err := q.db.Exec(ctx,
		`INSERT INTO lead_proposal_status (proposal_id, status, note, actor, at) VALUES ($1, $2, $3, $4, $5)`,
		proposalID, status, note, actor, now)
	return err
}

// AddProposal stores proposal p and its first status at now and returns its id, in one
// transaction. Added as sent, it also moves the lead to the proposal stage when its effective
// stage is none or talking; no other status moves a stage (accepted only lets the console offer
// won). Bad input is ErrBadCRM and writes nothing; a missing lead is ErrLeadNotFound.
func (q Queries) AddProposal(ctx context.Context, p NewProposal, now time.Time) (int64, error) {
	p.Title, p.URL, p.Note = strings.TrimSpace(p.Title), strings.TrimSpace(p.URL), strings.TrimSpace(p.Note)
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if p.Currency == "" {
		p.Currency = DefaultCurrency
	}
	if p.Status == "" {
		p.Status = ProposalSent
	}
	var err error
	switch {
	case p.Title == "":
		err = fmt.Errorf("%w: a proposal needs a title", ErrBadCRM)
	case p.URL != "" && !isOutreachLink(p.URL):
		err = fmt.Errorf("%w: the proposal link must be an https:// address", ErrBadCRM)
	case p.ValueCents != nil && *p.ValueCents < 0:
		err = fmt.Errorf("%w: the proposal value cannot be below zero", ErrBadCRM)
	case !slices.Contains(Currencies, p.Currency):
		err = fmt.Errorf("%w: unknown currency %q", ErrBadCRM, p.Currency)
	default:
		err = errors.Join(oneLine(p.Title, "proposal title", MaxProposalTitle),
			checkProposalStatus(p.Status, p.Note), checkProposalActor(p.Actor))
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: add proposal: lead %d: %w", p.LeadID, err)
	}
	var id int64
	err = q.inTx(ctx, func(tq Queries) error {
		if err := tq.lockProposalLead(ctx, p.LeadID); err != nil {
			return err
		}
		if err := tq.db.QueryRow(ctx,
			`INSERT INTO lead_proposal (lead_id, title, url, value_cents, currency, actor, at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			p.LeadID, p.Title, proofNullText(p.URL), p.ValueCents, p.Currency, p.Actor, now).Scan(&id); err != nil {
			return err
		}
		if err := tq.insertProposalStatus(ctx, id, p.Status, p.Note, p.Actor, now); err != nil {
			return err
		}
		if p.Status != ProposalSent {
			return nil
		}
		return tq.stageForSentProposal(ctx, p.LeadID, now)
	})
	if err != nil {
		return 0, fmt.Errorf("leadstore: add proposal: lead %d: %w", p.LeadID, err)
	}
	return id, nil
}

// SetProposalStatus appends proposal proposalID's status (one of ProposalStatuses) with note
// (trimmed, at most MaxProposalNote characters) by actor at now, in one transaction; the latest
// wins. Sent again moves the stage as AddProposal does; no other status moves it. Bad input is
// ErrBadCRM and writes nothing; a missing proposal is ErrProposalNotFound.
func (q Queries) SetProposalStatus(ctx context.Context, proposalID int64, status, note, actor string, now time.Time) error {
	note = strings.TrimSpace(note)
	if err := errors.Join(checkProposalStatus(status, note), checkProposalActor(actor)); err != nil {
		return fmt.Errorf("leadstore: set proposal status %d: %w", proposalID, err)
	}
	err := q.inTx(ctx, func(tq Queries) error {
		var leadID int64
		err := tq.db.QueryRow(ctx, `SELECT lead_id FROM lead_proposal WHERE id = $1`, proposalID).Scan(&leadID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProposalNotFound
		}
		if err != nil {
			return err
		}
		if err := tq.lockProposalLead(ctx, leadID); err != nil {
			return err
		}
		if err := tq.insertProposalStatus(ctx, proposalID, status, note, actor, now); err != nil {
			return err
		}
		if status != ProposalSent {
			return nil
		}
		return tq.stageForSentProposal(ctx, leadID, now)
	})
	if err != nil {
		return fmt.Errorf("leadstore: set proposal status %d: %w", proposalID, err)
	}
	return nil
}

// ProposalsFor returns lead leadID's proposals, newest first, each with its latest status
// (empty, never nil, for none).
func (q Queries) ProposalsFor(ctx context.Context, leadID int64) ([]Proposal, error) {
	out, err := collect(ctx, q.db, pgx.RowToStructByName[Proposal],
		`SELECT p.id, p.lead_id, p.title, coalesce(p.url, '') AS url, p.value_cents, p.currency, p.actor, p.at,
		   s.status, s.note AS status_note, s.at AS status_at
		 FROM lead_proposal p
		 JOIN LATERAL (SELECT status, note, at FROM lead_proposal_status
		   WHERE proposal_id = p.id ORDER BY id DESC LIMIT 1) s ON true
		 WHERE p.lead_id = $1
		 ORDER BY p.id DESC`, leadID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: proposals for %d: %w", leadID, err)
	}
	return out, nil
}
