package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// The account and campaign every lead, suppression, mailbox, batch and setting belonged to
// before campaigns existed (migration 00016 seeds them): a zero account or campaign id given
// to a store call that writes means these.
const (
	DefaultAccountID  int64 = 1
	DefaultCampaignID int64 = 1
	// DefaultCampaignSlug is campaign 1's slug, the one a transfer file without a campaign
	// imports into.
	DefaultCampaignSlug = "drupal-eol"
)

// The sources a campaign version can come from (campaign_version_source_check).
const (
	CampaignFromFile    = "file"
	CampaignFromConsole = "console"
)

var (
	// ErrCampaignNotFound is returned when no campaign of the account has that slug.
	ErrCampaignNotFound = errors.New("leadstore: campaign not found")
	// ErrBadCampaign is returned by RegisterCampaign for a slug, name, spec, sha, source or
	// actor the table would refuse.
	ErrBadCampaign = errors.New("leadstore: bad campaign")
)

var (
	campaignSlugRE = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)
	sha256HexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// orAccount is accountID, or DefaultAccountID when it is zero (or less).
func orAccount(accountID int64) int64 {
	if accountID <= 0 {
		return DefaultAccountID
	}
	return accountID
}

// orCampaign is campaignID, or DefaultCampaignID when it is zero (or less).
func orCampaign(campaignID int64) int64 {
	if campaignID <= 0 {
		return DefaultCampaignID
	}
	return campaignID
}

// Campaign is one campaign with its latest spec version (Spec, SpecSHA256 and VersionAt are
// zero when it has none yet).
type Campaign struct {
	ID, AccountID int64
	Slug, Name    string
	RetiredAt     *time.Time
	SpecSHA256    string
	Spec          []byte // JSON
	VersionAt     *time.Time
}

// RegisterCampaign makes sure campaign slug of account accountID exists (created with name
// when missing; an existing campaign keeps its name) and records spec (a JSON object) as its
// latest version, by source (file or console) and actor (operator or system), only when
// specSHA differs from the latest version's: registering the same spec again writes nothing.
// It returns the campaign's id and whether a version was written.
func (q Queries) RegisterCampaign(ctx context.Context, accountID int64, slug, name string, spec []byte, specSHA, source, actor string) (campaignID int64, versionWritten bool, err error) {
	accountID = orAccount(accountID)
	switch {
	case !campaignSlugRE.MatchString(slug):
		return 0, false, fmt.Errorf("leadstore: register campaign: %w: slug %q", ErrBadCampaign, slug)
	case name == "":
		return 0, false, fmt.Errorf("leadstore: register campaign: %w: blank name", ErrBadCampaign)
	case !json.Valid(spec) || !isJSONObject(spec):
		return 0, false, fmt.Errorf("leadstore: register campaign: %w: spec is not a JSON object", ErrBadCampaign)
	case !sha256HexRE.MatchString(specSHA):
		return 0, false, fmt.Errorf("leadstore: register campaign: %w: sha %q", ErrBadCampaign, specSHA)
	case source != CampaignFromFile && source != CampaignFromConsole:
		return 0, false, fmt.Errorf("leadstore: register campaign: %w: source %q", ErrBadCampaign, source)
	case actor != ActorOperator && actor != ActorSystem:
		return 0, false, fmt.Errorf("leadstore: register campaign: %w: %q", ErrUnknownActor, actor)
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO campaign (account_id, slug, name) VALUES ($1, $2, $3)
		 ON CONFLICT (account_id, slug) DO NOTHING`, accountID, slug, name); err != nil {
		return 0, false, fmt.Errorf("leadstore: register campaign: %w", err)
	}
	if err := q.db.QueryRow(ctx, `SELECT id FROM campaign WHERE account_id = $1 AND slug = $2`,
		accountID, slug).Scan(&campaignID); err != nil {
		return 0, false, fmt.Errorf("leadstore: register campaign: %w", err)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO campaign_version (campaign_id, spec, spec_sha256, source, actor)
		 SELECT $1, $2::jsonb, $3, $4, $5
		 WHERE (SELECT v.spec_sha256 FROM campaign_version v WHERE v.campaign_id = $1
		        ORDER BY v.id DESC LIMIT 1) IS DISTINCT FROM $3`,
		campaignID, string(spec), specSHA, source, actor)
	if err != nil {
		return 0, false, fmt.Errorf("leadstore: register campaign: version: %w", err)
	}
	return campaignID, tag.RowsAffected() == 1, nil
}

func isJSONObject(b []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

// campaignSQL reads campaigns (alias c) with their latest version; callers add the WHERE.
const campaignSQL = `SELECT c.id, c.account_id, c.slug, c.name, c.retired_at,
	   coalesce(v.spec_sha256, ''), v.spec::text, v.at
	 FROM campaign c
	 LEFT JOIN LATERAL (
	   SELECT x.spec, x.spec_sha256, x.at FROM campaign_version x
	   WHERE x.campaign_id = c.id ORDER BY x.id DESC LIMIT 1
	 ) v ON true`

func scanCampaign(row pgx.CollectableRow) (Campaign, error) {
	var c Campaign
	var spec *string
	if err := row.Scan(&c.ID, &c.AccountID, &c.Slug, &c.Name, &c.RetiredAt, &c.SpecSHA256, &spec, &c.VersionAt); err != nil {
		return Campaign{}, err
	}
	if spec != nil {
		c.Spec = []byte(*spec)
	}
	return c, nil
}

// Campaigns returns every campaign of account accountID (0: the default account), retired
// ones included, by id, each with its latest spec version.
func (q Queries) Campaigns(ctx context.Context, accountID int64) ([]Campaign, error) {
	rows, err := collect(ctx, q.db, scanCampaign, campaignSQL+` WHERE c.account_id = $1 ORDER BY c.id`,
		orAccount(accountID))
	if err != nil {
		return nil, fmt.Errorf("leadstore: campaigns: %w", err)
	}
	return rows, nil
}

// CampaignBySlug returns campaign slug of account accountID (0: the default account) with its
// latest spec version, or ErrCampaignNotFound.
func (q Queries) CampaignBySlug(ctx context.Context, accountID int64, slug string) (Campaign, error) {
	rows, err := collect(ctx, q.db, scanCampaign, campaignSQL+` WHERE c.account_id = $1 AND c.slug = $2`,
		orAccount(accountID), slug)
	if err != nil {
		return Campaign{}, fmt.Errorf("leadstore: campaign by slug: %w", err)
	}
	if len(rows) == 0 {
		return Campaign{}, fmt.Errorf("leadstore: campaign %q: %w", slug, ErrCampaignNotFound)
	}
	return rows[0], nil
}

// The reasons OpenElsewhere gives.
const (
	ElsewhereReplied = "replied in campaign %s"
	ElsewhereOpen    = "open sequence in campaign %s"
)

// OpenElsewhere returns, for each of orgIDs that is already in a conversation in another
// campaign of account accountID (one business, one conversation), why: a lead of that org in
// another campaign of the account has a reply (its state replied, or a reply in the mailbox)
// or a live message approved, sending or maybe_sent. Shadow batches never count. A reply wins
// over an open sequence; among campaigns the lowest id is named. Orgs not in the map are free.
func (q Queries) OpenElsewhere(ctx context.Context, accountID, campaignID int64, orgIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(orgIDs) == 0 {
		return out, nil
	}
	type row struct {
		OrgID   int64  `db:"org_id"`
		Slug    string `db:"slug"`
		Replied bool   `db:"replied"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT DISTINCT ON (l.org_id) l.org_id, c.slug, r.replied
		 FROM lead l
		 JOIN campaign c ON c.id = l.campaign_id
		 CROSS JOIN LATERAL (
		   SELECT l.state = 'replied'
		     OR EXISTS (SELECT 1 FROM mailbox_event e WHERE e.lead_id = l.id AND e.kind = 'reply') AS replied
		 ) r
		 WHERE l.org_id = ANY($3::bigint[]) AND c.account_id = $1 AND l.campaign_id <> $2
		   AND (r.replied OR EXISTS (
		     SELECT 1 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		     WHERE m.lead_id = l.id AND NOT b.shadow AND m.state IN ('approved', 'sending', 'maybe_sent')))
		 ORDER BY l.org_id, r.replied DESC, c.id`,
		orAccount(accountID), campaignID, orgIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: open elsewhere: %w", err)
	}
	for _, r := range rows {
		if r.Replied {
			out[r.OrgID] = fmt.Sprintf(ElsewhereReplied, r.Slug)
		} else {
			out[r.OrgID] = fmt.Sprintf(ElsewhereOpen, r.Slug)
		}
	}
	return out, nil
}

// leadAccount returns the account lead leadID belongs to, through its campaign.
func (q Queries) leadAccount(ctx context.Context, leadID int64) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`SELECT c.account_id FROM lead l JOIN campaign c ON c.id = l.campaign_id WHERE l.id = $1`, leadID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("lead %d: %w", leadID, ErrLeadNotFound)
	}
	return id, err
}

// SharedLead is a new lead of one campaign whose org also has a lead in another campaign.
type SharedLead struct {
	LeadID  int64  `db:"lead_id"`
	OrgID   int64  `db:"org_id"`
	Domain  string `db:"domain"`
	Kind    string `db:"kind"`
	Channel string `db:"channel"`
}

// SharedNewLeads returns the new leads of campaign campaignID (with a channel) whose org
// also has a lead in another campaign of account accountID (0: DefaultAccountID), by domain
// then id: the leads OpenElsewhere may hold back.
func (q Queries) SharedNewLeads(ctx context.Context, accountID, campaignID int64) ([]SharedLead, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[SharedLead],
		`SELECT l.id AS lead_id, l.org_id, o.domain, l.kind, l.channel FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.campaign_id = $2 AND l.state = 'new' AND l.channel <> 'none'
		   AND EXISTS (SELECT 1 FROM lead x JOIN campaign c ON c.id = x.campaign_id
		               WHERE x.org_id = l.org_id AND x.campaign_id <> $2 AND c.account_id = $1)
		 ORDER BY o.domain, l.id`, orAccount(accountID), campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: shared new leads: %w", err)
	}
	return rows, nil
}
