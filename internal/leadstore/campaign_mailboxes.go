package leadstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The mailboxes assigned per campaign (migration 00024): campaign_mailbox is append-only,
// the latest row (by id) of a campaign and mailbox pair is the assignment, and no row means
// not assigned. A live batch of a campaign is drafted only onto its assigned mailboxes, and
// a live send holds a message whose mailbox is not (or no longer) assigned to its batch's
// campaign.

// ErrCrossAccount is returned by SetCampaignMailbox for a mailbox and a campaign of
// different accounts.
var ErrCrossAccount = errors.New("leadstore: the mailbox and the campaign belong to different accounts")

// CampaignMailbox is the latest campaign_mailbox row of one campaign and mailbox pair.
type CampaignMailbox struct {
	CampaignID int64     `db:"campaign_id"`
	MailboxID  int64     `db:"mailbox_id"`
	Assigned   bool      `db:"assigned"`
	Actor      string    `db:"actor"`
	At         time.Time `db:"at"`
}

// latestAssignmentSQL is the latest row of every pair; callers filter on cm.
const latestAssignmentSQL = `SELECT cm.campaign_id, cm.mailbox_id, cm.assigned, cm.actor, cm.at
	 FROM (SELECT DISTINCT ON (x.campaign_id, x.mailbox_id) x.campaign_id, x.mailbox_id, x.assigned, x.actor, x.at
	       FROM campaign_mailbox x %s ORDER BY x.campaign_id, x.mailbox_id, x.id DESC) cm
	 WHERE cm.assigned`

// CampaignMailboxes returns the mailboxes assigned to campaign campaignID (0: the default
// campaign), by mailbox id. Retired or paused mailboxes are included: assignment is not
// readiness.
func (q Queries) CampaignMailboxes(ctx context.Context, campaignID int64) ([]CampaignMailbox, error) {
	rows, err := collect(ctx, q.db, rowToCampaignMailbox,
		fmt.Sprintf(latestAssignmentSQL, `WHERE x.campaign_id = $1`)+` ORDER BY cm.mailbox_id`, orCampaign(campaignID))
	if err != nil {
		return nil, fmt.Errorf("leadstore: campaign mailboxes: %w", err)
	}
	return rows, nil
}

// MailboxCampaigns returns the campaigns mailbox mailboxID is assigned to, by campaign id.
func (q Queries) MailboxCampaigns(ctx context.Context, mailboxID int64) ([]CampaignMailbox, error) {
	rows, err := collect(ctx, q.db, rowToCampaignMailbox,
		fmt.Sprintf(latestAssignmentSQL, `WHERE x.mailbox_id = $1`)+` ORDER BY cm.campaign_id`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: mailbox campaigns: %w", err)
	}
	return rows, nil
}

// MailboxAssigned reports whether mailbox mailboxID is assigned to campaign campaignID (0:
// the default campaign) now. It is one indexed read, for the send's claim transaction to
// re-check the assignment at the last moment before SMTP (run it on the transaction's
// Queries).
func (q Queries) MailboxAssigned(ctx context.Context, campaignID, mailboxID int64) (bool, error) {
	var assigned bool
	if err := q.db.QueryRow(ctx,
		`SELECT coalesce((SELECT x.assigned FROM campaign_mailbox x
		   WHERE x.campaign_id = $1 AND x.mailbox_id = $2 ORDER BY x.id DESC LIMIT 1), false)`,
		orCampaign(campaignID), mailboxID).Scan(&assigned); err != nil {
		return false, fmt.Errorf("leadstore: mailbox assigned: %w", err)
	}
	return assigned, nil
}

// SetCampaignMailbox assigns (assigned true) or unassigns mailbox mailboxID to campaign
// campaignID (0: the default campaign) by actor (ActorGremy or ActorSystem), and reports
// whether it wrote a row: a pair already in that state writes nothing. A missing campaign is
// ErrCampaignNotFound, a missing mailbox ErrMailboxNotFound, a mailbox and campaign of
// different accounts ErrCrossAccount, and assigning a retired mailbox ErrMailboxRetired
// (unassigning one is allowed). The mailbox row is locked for the statement, so two writers
// of one pair take turns.
func (q Queries) SetCampaignMailbox(ctx context.Context, campaignID, mailboxID int64, assigned bool, actor string) (changed bool, err error) {
	if actor != ActorGremy && actor != ActorSystem {
		return false, fmt.Errorf("leadstore: set campaign mailbox: %w: %q", ErrUnknownActor, actor)
	}
	campaignID = orCampaign(campaignID)
	var campaignAccount, mailboxAccount *int64
	var retired bool
	var inserted *int64
	err = q.db.QueryRow(ctx,
		`WITH mb AS (SELECT id, account_id FROM sender_mailbox WHERE id = $2 FOR UPDATE),
		 c AS (SELECT id, account_id FROM campaign WHERE id = $1),
		 st AS (SELECT s.state FROM mailbox_state s WHERE s.mailbox_id = $2 ORDER BY s.id DESC LIMIT 1),
		 cur AS (SELECT x.assigned FROM campaign_mailbox x WHERE x.campaign_id = $1 AND x.mailbox_id = $2 ORDER BY x.id DESC LIMIT 1),
		 ins AS (
		   INSERT INTO campaign_mailbox (campaign_id, mailbox_id, assigned, actor)
		   SELECT c.id, mb.id, $3, $4 FROM mb JOIN c ON c.account_id = mb.account_id
		   WHERE coalesce((SELECT assigned FROM cur), false) IS DISTINCT FROM $3
		     AND NOT ($3 AND EXISTS (SELECT 1 FROM st WHERE state = 'retired'))
		   RETURNING id
		 )
		 SELECT (SELECT account_id FROM c), (SELECT account_id FROM mb),
		   EXISTS (SELECT 1 FROM st WHERE state = 'retired'), (SELECT id FROM ins)`,
		campaignID, mailboxID, assigned, actor).Scan(&campaignAccount, &mailboxAccount, &retired, &inserted)
	switch {
	case err != nil:
		return false, fmt.Errorf("leadstore: set campaign mailbox: %w", err)
	case campaignAccount == nil:
		return false, fmt.Errorf("leadstore: set campaign mailbox: campaign %d: %w", campaignID, ErrCampaignNotFound)
	case mailboxAccount == nil:
		return false, fmt.Errorf("leadstore: set campaign mailbox: mailbox %d: %w", mailboxID, ErrMailboxNotFound)
	case *campaignAccount != *mailboxAccount:
		return false, fmt.Errorf("leadstore: set campaign mailbox: mailbox %d, campaign %d: %w", mailboxID, campaignID, ErrCrossAccount)
	case assigned && retired:
		return false, fmt.Errorf("leadstore: set campaign mailbox: mailbox %d: %w", mailboxID, ErrMailboxRetired)
	}
	return inserted != nil, nil
}

func rowToCampaignMailbox(row pgx.CollectableRow) (CampaignMailbox, error) {
	var c CampaignMailbox
	err := row.Scan(&c.CampaignID, &c.MailboxID, &c.Assigned, &c.Actor, &c.At)
	return c, err
}
