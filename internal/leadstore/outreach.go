package leadstore

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/campaign"
)

// The states an outreach message can be in (outreach_message_state_check).
const (
	MsgDraft      = "draft"
	MsgApproved   = "approved"
	MsgSkipped    = "skipped"
	MsgCancelled  = "cancelled"
	MsgSending    = "sending"
	MsgSent       = "sent"
	MsgShadowSent = "shadow_sent"
	MsgRefused    = "refused"
	MsgMaybeSent  = "maybe_sent"
	MsgDone       = "done"
)

// The cancel and skip reasons the store writes into outreach_message.cancel_reason.
const (
	CancelHandContacted = "hand-contacted"
	SkipByGremy         = "skipped at approval"
	SkipSuppressed      = "suppressed"
	SkipLeadState       = "lead " // + the lead's state, e.g. "lead replied"
)

// MinHashPrefix is the fewest hex digits of content_sha256 ApproveBatch accepts (RA2).
const MinHashPrefix = 12

var (
	// ErrEmptyBatch is returned by CreateBatch for no drafts.
	ErrEmptyBatch = errors.New("leadstore: batch has no drafts")
	// ErrDuplicateDraft is returned by CreateBatch for two drafts of one lead and step.
	ErrDuplicateDraft = errors.New("leadstore: two drafts for one lead and step")
	// ErrNotDraftable is returned by CreateBatch for a draft whose lead is not new or is
	// suppressed; nothing is written.
	ErrNotDraftable = errors.New("leadstore: lead cannot be drafted")
	// ErrBatchNotFound is returned when no outreach_batch row has that id.
	ErrBatchNotFound = errors.New("leadstore: batch not found")
	// ErrHashTooShort is returned by ApproveBatch for a hash prefix under MinHashPrefix hex
	// digits, or one that is not hex.
	ErrHashTooShort = errors.New("leadstore: approve needs at least 12 hex digits of the batch hash")
	// ErrHashMismatch is returned by ApproveBatch when the prefix does not match the
	// batch's content_sha256.
	ErrHashMismatch = errors.New("leadstore: hash does not match the batch")
	// ErrBatchTampered is returned by ApproveBatch when the batch's rows no longer hash to
	// its stored content_sha256 (they were edited after batch new).
	ErrBatchTampered = errors.New("leadstore: batch rows no longer match their content hash")
	// ErrBadSkip is returned by ApproveBatch for a skip number outside the batch's items.
	ErrBadSkip = errors.New("leadstore: no such batch item")
	// ErrMessageState is returned by the Mark* calls when the message is not in a state
	// (or batch kind) the move starts from; nothing changed.
	ErrMessageState = errors.New("leadstore: message not in a state for this move")
	// ErrBatchDiscarded is returned by ApproveBatch for a batch the operator discarded.
	ErrBatchDiscarded = errors.New("leadstore: batch was discarded")
	// ErrBatchApproved is returned by DiscardBatch for an approved batch: what was approved
	// stays, and send decides each message.
	ErrBatchApproved = errors.New("leadstore: batch is approved")
	// ErrBadDraft is returned by CreateBatch for a draft whose sequence fields do not fit: a
	// call row without its phone fact or with an address, a phone fact on a non-call row, an
	// unknown condition, two sequence names for one lead, or a sequence name the batch's
	// sequences do not hold.
	ErrBadDraft = errors.New("leadstore: draft does not fit its sequence")
)

// ChannelPhone is a call step's channel (outreach_message_channel_check): never sent by
// leads send; the operator records its outcome (RecordCallOutcome).
const ChannelPhone = "phone"

// The step conditions a row can carry (outreach_message_condition_check); empty means
// ConditionAlways.
const (
	ConditionAlways           = campaign.WhenAlways
	ConditionHasCallablePhone = campaign.WhenHasCallablePhone
	ConditionCallNotConnected = campaign.WhenCallNotConnected
	ConditionEmailConsent     = campaign.WhenEmailConsent
)

var conditions = []string{ConditionAlways, ConditionHasCallablePhone, ConditionCallNotConnected, ConditionEmailConsent}

// DraftRow is one drafted message for CreateBatch. Its JSON form is what the batch's
// content hash covers.
type DraftRow struct {
	LeadID    int64           `json:"lead_id"`
	Step      int             `json:"step"`
	Channel   string          `json:"channel"` // email | form | linkedin
	To        string          `json:"to"`      // email channel only
	Subject   string          `json:"subject"`
	Body      string          `json:"body"`
	MessageID string          `json:"message_id"`
	InReplyTo string          `json:"in_reply_to"`
	Slots     json.RawMessage `json:"slots"` // slot name → {value, source table, source id}
	// The sending mailbox and the From name frozen with it (decision 6): both are in the
	// content hash, and omitted from its JSON when unset, so a batch drafted before
	// mailboxes hashes as it did. A later step with none inherits its lead's step 0; a draft
	// left with none is refused (ErrBadMailbox).
	MailboxID int64  `json:"mailbox_id,omitempty"`
	FromName  string `json:"from_name,omitempty"`
	// The sequence fields (00017), each omitted from the hash JSON when unset, so an
	// all-email legacy batch hashes as it did. Condition "" and "always" are one value
	// (canonicalDrafts folds "always" to ""). A call row (Channel phone) has no To, its
	// subject "Call <org name>", its body the call card text, and PhoneFactID the org_fact
	// phone it dials; every other row has no PhoneFactID. SequenceName names the batch
	// sequence the lead follows ("" = default), the same on every row of one lead.
	Condition    string `json:"condition,omitempty"`
	PhoneFactID  int64  `json:"phone_fact_id,omitempty"`
	SequenceName string `json:"sequence_name,omitempty"`
}

// canonicalDrafts returns drafts sorted by lead then step, with each Slots re-encoded with
// sorted keys ({} when empty).
func canonicalDrafts(drafts []DraftRow) ([]DraftRow, error) {
	out := slices.Clone(drafts)
	slices.SortStableFunc(out, func(a, b DraftRow) int {
		return cmp.Or(cmp.Compare(a.LeadID, b.LeadID), cmp.Compare(a.Step, b.Step))
	})
	for i := range out {
		if i > 0 && out[i].LeadID == out[i-1].LeadID && out[i].Step == out[i-1].Step {
			return nil, fmt.Errorf("%w: lead %d step %d", ErrDuplicateDraft, out[i].LeadID, out[i].Step)
		}
		if out[i].Condition == ConditionAlways {
			out[i].Condition = ""
		}
		raw := bytes.TrimSpace(out[i].Slots)
		if len(raw) == 0 {
			out[i].Slots = json.RawMessage("{}")
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("leadstore: draft lead %d step %d: slots: %w", out[i].LeadID, out[i].Step, err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("leadstore: draft lead %d step %d: slots: %w", out[i].LeadID, out[i].Step, err)
		}
		out[i].Slots = b
	}
	return out, nil
}

// ContentSHA256 is the content hash of a batch of drafts: sha256 over the canonical JSON
// of the drafts sorted by lead then step, slots keys sorted.
func ContentSHA256(drafts []DraftRow) ([]byte, error) {
	c, err := canonicalDrafts(drafts)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("leadstore: content hash: %w", err)
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// MessageSHA256 is the hash approval binds a message to (RA2): sha256 over to, subject,
// body and in-reply-to, each followed by a NUL byte so no two splits of the text collide.
// send recomputes it from the row and refuses a mismatch.
func MessageSHA256(to, subject, body, inReplyTo string) []byte {
	h := sha256.New()
	for _, s := range []string{to, subject, body, inReplyTo} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// CreateBatch writes one batch of drafts and returns its id and content hash. Every draft's
// lead must be new, not suppressed (address, domain) and, for a site lead, on its site's
// latest probe, else nothing is written and
// ErrNotDraftable names it. A live batch moves its leads to batched; a shadow batch never
// touches lead.state (RA1), so its leads come up again for the first live batch. Messages
// start as drafts, written in draft order (each lead where it first appears in drafts, its
// steps in order), so BatchMessages reads the batch in the order it was drafted; the content
// hash does not depend on that order. templatesSHA is provenance only. The batch belongs to
// its leads' account (every lead must be of one account). Run it in a transaction.
func (q Queries) CreateBatch(ctx context.Context, shadow bool, drafts []DraftRow, templatesSHA []byte) (batchID int64, contentSHA []byte, err error) {
	return q.CreateBatchFor(ctx, BatchMeta{}, shadow, drafts, templatesSHA)
}

// BatchSequences is a batch's frozen sequences (outreach_batch.sequence): sequence name →
// its steps, "default" for the default one.
type BatchSequences map[string][]campaign.Step

// For returns the sequence a row with sequence name name follows ("" = default); nil when
// the batch holds none of that name.
func (b BatchSequences) For(name string) []campaign.Step {
	if name == "" {
		name = campaign.SequenceDefault
	}
	return b[name]
}

// BatchMeta is what a batch stores beside its drafts (00017). The zero value is today's
// legacy batch: the leads' own campaign, no stored sequence (the legacy 3-step rule).
type BatchMeta struct {
	// CampaignID is the campaign the batch drafts for; every lead must be in it. 0: the
	// leads' campaign when they share one, else DefaultCampaignID.
	CampaignID int64
	// Sequences are the campaign's sequences frozen at batch time; nil stores NULL (legacy).
	// Every row's SequenceName must name one of them; with nil, no row may name one.
	Sequences BatchSequences
}

// CreateBatchFor is CreateBatch with the batch's campaign and frozen sequences. A call row
// (Channel phone) must name a phone fact of its lead's org whose number no active phone
// suppression of the account holds (else ErrNotDraftable: the batch writer leaves such a
// lead's call steps out). The content hash covers the call rows like any other.
func (q Queries) CreateBatchFor(ctx context.Context, meta BatchMeta, shadow bool, drafts []DraftRow, templatesSHA []byte) (batchID int64, contentSHA []byte, err error) {
	if len(drafts) == 0 {
		return 0, nil, ErrEmptyBatch
	}
	c, err := canonicalDrafts(drafts)
	if err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
	}
	if err := checkSequenceFields(c, meta.Sequences); err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
	}
	if err := inheritMailbox(c); err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
	}
	contentSHA, err = ContentSHA256(c)
	if err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
	}
	if templatesSHA == nil {
		templatesSHA = []byte{}
	}
	// Draft-time checks (suppression's first point), with the lead rows locked.
	var leadIDs []int64
	for _, d := range c {
		if len(leadIDs) == 0 || leadIDs[len(leadIDs)-1] != d.LeadID {
			leadIDs = append(leadIDs, d.LeadID)
		}
	}
	for _, id := range leadIDs {
		var tos []string
		for _, d := range c {
			if d.LeadID == id && d.To != "" {
				tos = append(tos, d.To)
			}
		}
		st, err := q.leadCheck(ctx, id, tos)
		if err != nil {
			return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
		}
		if st.state != LeadNew || st.suppressed || st.stale {
			why := "state " + st.state
			switch {
			case st.suppressed:
				why = "suppressed"
			case st.stale:
				why = "finding not the site's latest probe; refresh the lead"
			}
			return 0, nil, fmt.Errorf("leadstore: create batch: lead %d (%s): %w", id, why, ErrNotDraftable)
		}
	}
	// The batch is its leads' account's, through their campaigns; one batch never spans two.
	var accounts []int64
	if accounts, err = collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT DISTINCT c.account_id FROM lead l JOIN campaign c ON c.id = l.campaign_id
		 WHERE l.id = ANY($1::bigint[]) ORDER BY 1`, leadIDs); err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: account: %w", err)
	}
	if len(accounts) != 1 {
		return 0, nil, fmt.Errorf("leadstore: create batch: leads of %d accounts: %w", len(accounts), ErrNotDraftable)
	}
	campaignID, err := q.batchCampaign(ctx, meta.CampaignID, leadIDs)
	if err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
	}
	if err := q.checkCallRows(ctx, accounts[0], c); err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
	}
	var seqJSON []byte // nil stores NULL
	if meta.Sequences != nil {
		if seqJSON, err = json.Marshal(meta.Sequences); err != nil {
			return 0, nil, fmt.Errorf("leadstore: create batch: sequences: %w", err)
		}
	}
	if err := q.db.QueryRow(ctx,
		`INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256, account_id, campaign_id, sequence)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb) RETURNING id`,
		shadow, contentSHA, templatesSHA, accounts[0], campaignID, seqJSON).Scan(&batchID); err != nil {
		return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
	}
	for _, d := range draftOrder(drafts, c) {
		cond := d.Condition
		if cond == "" {
			cond = ConditionAlways
		}
		var factID *int64
		if d.PhoneFactID != 0 {
			factID = &d.PhoneFactID
		}
		var seqName *string
		if d.SequenceName != "" {
			seqName = &d.SequenceName
		}
		if _, err := q.db.Exec(ctx,
			`INSERT INTO outreach_message (batch_id, lead_id, step, channel, to_address, subject, body_text,
			   message_id, in_reply_to, slots, mailbox_id, from_name, condition, phone_fact_id, sequence_name)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			batchID, d.LeadID, d.Step, d.Channel, d.To, d.Subject, d.Body, d.MessageID, d.InReplyTo, string(d.Slots),
			d.MailboxID, d.FromName, cond, factID, seqName); err != nil {
			return 0, nil, fmt.Errorf("leadstore: create batch: lead %d step %d: %w", d.LeadID, d.Step, err)
		}
	}
	if !shadow {
		reason := fmt.Sprintf("batch %d", batchID)
		for _, id := range leadIDs {
			if _, err := q.setLeadStateFrom(ctx, id, []string{LeadNew}, LeadBatched, reason, ActorSystem); err != nil {
				return 0, nil, fmt.Errorf("leadstore: create batch: %w", err)
			}
		}
	}
	return batchID, contentSHA, nil
}

// checkSequenceFields checks the sequence fields of c (canonical: by lead, then step) against
// the batch's sequences seqs (nil: a legacy batch).
func checkSequenceFields(c []DraftRow, seqs BatchSequences) error {
	for i, d := range c {
		switch {
		case d.Condition != "" && !slices.Contains(conditions, d.Condition):
			return fmt.Errorf("%w: lead %d step %d: unknown condition %q", ErrBadDraft, d.LeadID, d.Step, d.Condition)
		case d.Channel == ChannelPhone && d.PhoneFactID == 0:
			return fmt.Errorf("%w: lead %d step %d: a call row names no phone fact", ErrBadDraft, d.LeadID, d.Step)
		case d.Channel == ChannelPhone && d.To != "":
			return fmt.Errorf("%w: lead %d step %d: a call row has no address", ErrBadDraft, d.LeadID, d.Step)
		case d.Channel != ChannelPhone && d.PhoneFactID != 0:
			return fmt.Errorf("%w: lead %d step %d: only a call row names a phone fact", ErrBadDraft, d.LeadID, d.Step)
		case i > 0 && c[i-1].LeadID == d.LeadID && c[i-1].SequenceName != d.SequenceName:
			return fmt.Errorf("%w: lead %d: two sequence names in one batch", ErrBadDraft, d.LeadID)
		case seqs == nil && d.SequenceName != "":
			return fmt.Errorf("%w: lead %d: sequence %q in a batch with no sequences", ErrBadDraft, d.LeadID, d.SequenceName)
		case seqs != nil && seqs.For(d.SequenceName) == nil:
			return fmt.Errorf("%w: lead %d: the batch holds no sequence %q", ErrBadDraft, d.LeadID, d.SequenceName)
		}
	}
	return nil
}

// batchCampaign returns the campaign a batch of leadIDs drafts for: want (every lead must be
// in it) or, when want is 0, the leads' campaign when they share one, else DefaultCampaignID.
func (q Queries) batchCampaign(ctx context.Context, want int64, leadIDs []int64) (int64, error) {
	camps, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT DISTINCT campaign_id FROM lead WHERE id = ANY($1::bigint[]) ORDER BY 1`, leadIDs)
	if err != nil {
		return 0, fmt.Errorf("campaign: %w", err)
	}
	if want == 0 {
		if len(camps) == 1 {
			return camps[0], nil
		}
		return DefaultCampaignID, nil
	}
	if len(camps) != 1 || camps[0] != want {
		return 0, fmt.Errorf("leads not all in campaign %d: %w", want, ErrNotDraftable)
	}
	return want, nil
}

// checkCallRows refuses a call row whose phone fact is not a phone of its lead's org, or whose
// number an active phone suppression of account accountID holds.
func (q Queries) checkCallRows(ctx context.Context, accountID int64, c []DraftRow) error {
	for _, d := range c {
		if d.Channel != ChannelPhone {
			continue
		}
		var ok, suppressed bool
		err := q.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM org_fact f JOIN lead l ON l.org_id = f.org_id
			     WHERE f.id = $1 AND l.id = $2 AND f.kind = 'phone'),
			   `+phoneSuppressedSQL("$3::bigint", "$1::bigint"), d.PhoneFactID, d.LeadID, accountID).Scan(&ok, &suppressed)
		switch {
		case err != nil:
			return fmt.Errorf("lead %d step %d: phone fact: %w", d.LeadID, d.Step, err)
		case !ok:
			return fmt.Errorf("%w: lead %d step %d: fact %d is not a phone of its org", ErrBadDraft, d.LeadID, d.Step, d.PhoneFactID)
		case suppressed:
			return fmt.Errorf("lead %d step %d (phone suppressed): %w", d.LeadID, d.Step, ErrNotDraftable)
		}
	}
	return nil
}

// draftOrder returns c (canonical: by lead, then step) with its leads in the order they
// first appear in drafts; each lead's steps stay in step order.
func draftOrder(drafts, c []DraftRow) []DraftRow {
	first := map[int64]int{}
	for i, d := range drafts {
		if _, ok := first[d.LeadID]; !ok {
			first[d.LeadID] = i
		}
	}
	out := slices.Clone(c)
	slices.SortStableFunc(out, func(a, b DraftRow) int { return cmp.Compare(first[a.LeadID], first[b.LeadID]) })
	return out
}

// inheritMailbox gives each later step of a lead (c sorted by lead then step) its step 0's
// mailbox and From name when it has none, and refuses a lead whose steps name two
// mailboxes or two From names: a thread comes from one address. A step left with no
// mailbox, or a From name with a line break (it would be a header injection), is refused.
func inheritMailbox(c []DraftRow) error {
	for i := range c {
		if strings.ContainsAny(c[i].FromName, "\r\n") {
			return fmt.Errorf("%w: lead %d step %d: the From name has a line break", ErrBadMailbox, c[i].LeadID, c[i].Step)
		}
		if i == 0 || c[i].LeadID != c[i-1].LeadID {
			continue
		}
		first := c[i-1] // already settled: the lead's step 0, or a step that inherited it
		if c[i].MailboxID == 0 && c[i].FromName == "" {
			c[i].MailboxID, c[i].FromName = first.MailboxID, first.FromName
		}
		if c[i].MailboxID != first.MailboxID || c[i].FromName != first.FromName {
			return fmt.Errorf("%w: lead %d step %d is not on its first step's mailbox", ErrBadMailbox, c[i].LeadID, c[i].Step)
		}
	}
	for _, d := range c {
		if d.MailboxID == 0 {
			return fmt.Errorf("%w: lead %d step %d has no mailbox", ErrBadMailbox, d.LeadID, d.Step)
		}
	}
	return nil
}

type leadStatus struct {
	state      string
	suppressed bool
	stale      bool // a site lead whose probe is no longer its site's latest
}

// leadCheck locks lead id and returns its state and whether a suppression of its campaign's
// account blocks its route address, its org's domain, or any of tos.
func (q Queries) leadCheck(ctx context.Context, id int64, tos []string) (leadStatus, error) {
	if tos == nil {
		tos = []string{}
	}
	var st leadStatus
	err := q.db.QueryRow(ctx,
		`SELECT l.state,
		   `+suppressedSQL("cg.account_id", "o.domain", "CASE WHEN r.kind = 'email' THEN r.value ELSE '' END")+`
		   OR EXISTS (SELECT 1 FROM unnest($2::text[]) t(addr) WHERE `+suppressedSQL("cg.account_id", "o.domain", "t.addr")+`)
		   , l.probe_id IS NOT NULL AND l.probe_id IS DISTINCT FROM
		       (SELECT s.last_probe_id FROM site_probe p JOIN site s ON s.id = p.site_id WHERE p.id = l.probe_id)
		 FROM lead l JOIN org o ON o.id = l.org_id JOIN campaign cg ON cg.id = l.campaign_id
		 LEFT JOIN contact_route r ON r.id = l.route_id
		 WHERE l.id = $1 FOR UPDATE OF l`, id, tos).Scan(&st.state, &st.suppressed, &st.stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return leadStatus{}, fmt.Errorf("lead %d: %w", id, ErrLeadNotFound)
	}
	return st, err
}

// Message is one outreach_message row.
type Message struct {
	ID             int64           `db:"id"`
	BatchID        int64           `db:"batch_id"`
	LeadID         int64           `db:"lead_id"`
	Step           int             `db:"step"`
	Channel        string          `db:"channel"`
	ToAddress      string          `db:"to_address"`
	Subject        string          `db:"subject"`
	BodyText       string          `db:"body_text"`
	MessageID      string          `db:"message_id"`
	InReplyTo      string          `db:"in_reply_to"`
	Slots          json.RawMessage `db:"slots"`
	ApprovedSHA256 []byte          `db:"approved_sha256"`
	State          string          `db:"state"`
	DueAt          *time.Time      `db:"due_at"`
	SentAt         *time.Time      `db:"sent_at"`
	SMTPReply      string          `db:"smtp_reply"`
	CancelReason   string          `db:"cancel_reason"`
	CreatedAt      time.Time       `db:"created_at"`
	MailboxID      int64           `db:"mailbox_id"`    // rows from before mailboxes name the retired placeholder (00008)
	FromName       string          `db:"from_name"`     // frozen at batch creation
	FromAddress    string          `db:"from_address"`  // the mailbox's address, for reading
	Condition      string          `db:"condition"`     // always | has_callable_phone | call_not_connected | email_consent
	PhoneFactID    *int64          `db:"phone_fact_id"` // the number a call row dials; nil on other rows (or after erasure)
	SequenceName   string          `db:"sequence_name"` // "" on rows from before 00017
}

// messageCols: from_address is the mailbox's address, which is fixed for the app.
const messageCols = `m.id, m.batch_id, m.lead_id, m.step, m.channel, m.to_address, m.subject, m.body_text,
  m.message_id, m.in_reply_to, m.slots, m.approved_sha256, m.state, m.due_at, m.sent_at, m.smtp_reply,
  m.cancel_reason, m.created_at, m.mailbox_id, m.from_name,
  (SELECT fa.address FROM sender_mailbox fa WHERE fa.id = m.mailbox_id) AS from_address,
  m.condition, m.phone_fact_id, coalesce(m.sequence_name, '') AS sequence_name`

// Batch is one outreach_batch row.
type Batch struct {
	ID              int64      `db:"id"`
	Shadow          bool       `db:"shadow"`
	CreatedAt       time.Time  `db:"created_at"`
	ContentSHA256   []byte     `db:"content_sha256"`
	ApprovedAt      *time.Time `db:"approved_at"`
	ApprovedHash    []byte     `db:"approved_hash"`
	TemplatesSHA256 []byte     `db:"templates_sha256"`
	DiscardedAt     *time.Time `db:"discarded_at"`
	CampaignID      int64      `db:"campaign_id"`
	// Sequence is the frozen sequences JSON (BatchSequences); nil for a legacy batch.
	Sequence json.RawMessage `db:"sequence"`
	// Manual is a batch built from chosen leads to send now (00027, MarkBatchManual).
	Manual bool `db:"manual"`
}

// Sequences decodes b.Sequence; nil for a legacy batch (NULL).
func (b Batch) Sequences() (BatchSequences, error) {
	return decodeSequences(b.Sequence)
}

func decodeSequences(raw []byte) (BatchSequences, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, nil
	}
	var s BatchSequences
	if err := json.Unmarshal(t, &s); err != nil {
		return nil, fmt.Errorf("leadstore: batch sequences: %w", err)
	}
	return s, nil
}

// BatchByID returns batch id, or ErrBatchNotFound.
func (q Queries) BatchByID(ctx context.Context, id int64) (Batch, error) {
	b, err := collect(ctx, q.db, pgx.RowToStructByName[Batch],
		`SELECT id, shadow, created_at, content_sha256, approved_at, approved_hash, templates_sha256, discarded_at,
		   campaign_id, sequence, manual
		 FROM outreach_batch WHERE id = $1`, id)
	if err != nil {
		return Batch{}, fmt.Errorf("leadstore: batch by id: %w", err)
	}
	if len(b) == 0 {
		return Batch{}, fmt.Errorf("leadstore: batch %d: %w", id, ErrBatchNotFound)
	}
	return b[0], nil
}

// BatchMessages returns batch id's messages in draft order: by lead, the lead whose first
// message was written first coming first (min(m.id) per lead, D6), then by step. A batch's
// items, the numbers ApproveBatch's skip list names, are its leads in this order, from 1.
func (q Queries) BatchMessages(ctx context.Context, batchID int64) ([]Message, error) {
	ms, err := collect(ctx, q.db, pgx.RowToStructByName[Message],
		`SELECT `+messageCols+` FROM outreach_message m WHERE m.batch_id = $1
		 ORDER BY min(m.id) OVER (PARTITION BY m.lead_id), m.lead_id, m.step`, batchID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: batch messages: %w", err)
	}
	return ms, nil
}

// BatchContentSHA256 recomputes batch batchID's content hash from its stored messages
// (ContentSHA256 over the drafts rebuilt from BatchMessages). For a batch nobody edited it
// equals the stored content_sha256.
func (q Queries) BatchContentSHA256(ctx context.Context, batchID int64) ([]byte, error) {
	ms, err := q.BatchMessages(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("leadstore: batch content hash: batch %d: %w", batchID, ErrBatchNotFound)
	}
	return ContentSHA256(DraftsOf(ms))
}

// DraftsOf rebuilds the drafts the messages were written from.
func DraftsOf(ms []Message) []DraftRow {
	out := make([]DraftRow, 0, len(ms))
	for _, m := range ms {
		d := DraftRow{
			LeadID: m.LeadID, Step: m.Step, Channel: m.Channel, To: m.ToAddress, Subject: m.Subject,
			Body: m.BodyText, MessageID: m.MessageID, InReplyTo: m.InReplyTo, Slots: m.Slots,
			MailboxID: m.MailboxID, FromName: m.FromName, SequenceName: m.SequenceName,
		}
		if m.Condition != ConditionAlways {
			d.Condition = m.Condition
		}
		if m.PhoneFactID != nil {
			d.PhoneFactID = *m.PhoneFactID
		}
		out = append(out, d)
	}
	return out
}

// Approval is what ApproveBatch did.
type Approval struct {
	Already  bool // the batch was approved before; nothing changed
	Approved int  // messages approved
	Skipped  int  // messages skipped (by the skip list, suppression or lead state)
}

// ApproveBatch approves batch id for the operator (RA2). It is refused unless hashPrefix (at least
// MinHashPrefix hex digits) is a prefix of the batch's content_sha256, and unless its rows
// still hash to that (ErrBatchTampered). An approved batch is
// approved again as a no-op (Already). skip lists the item numbers the operator left out, where
// an item is one lead: the batch's distinct leads numbered from 1 in BatchMessages order
// (lead id, then step), so batch show must number them the same way. Their messages are skipped and, in a live batch, the lead moves to skipped. Every
// other item is re-checked (suppression's second point): a suppressed lead, or one no
// longer batched (live) or new (shadow), has its messages skipped. The rest are approved
// with due_at = due(message) (zero: NULL) and approved_sha256 = MessageSHA256 of the row.
// A discarded batch is ErrBatchDiscarded; the row lock it takes is the one DiscardBatch
// takes, so the two never interleave. Run it in a transaction.
func (q Queries) ApproveBatch(ctx context.Context, id int64, hashPrefix string, skip []int, due func(Message) time.Time) (Approval, error) {
	prefix := strings.ToLower(strings.TrimSpace(hashPrefix))
	if _, err := hex.DecodeString(prefix + strings.Repeat("0", len(prefix)%2)); err != nil || len(prefix) < MinHashPrefix {
		return Approval{}, fmt.Errorf("leadstore: approve batch %d: %w", id, ErrHashTooShort)
	}
	var (
		shadow     bool
		content    []byte
		approvedAt *time.Time
		discarded  bool
	)
	err := q.db.QueryRow(ctx,
		`SELECT shadow, content_sha256, approved_at, discarded_at IS NOT NULL FROM outreach_batch WHERE id = $1 FOR UPDATE`, id).
		Scan(&shadow, &content, &approvedAt, &discarded)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, fmt.Errorf("leadstore: approve batch %d: %w", id, ErrBatchNotFound)
	}
	if err != nil {
		return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
	}
	if discarded {
		return Approval{}, fmt.Errorf("leadstore: approve batch %d: %w", id, ErrBatchDiscarded)
	}
	if !strings.HasPrefix(hex.EncodeToString(content), prefix) {
		return Approval{}, fmt.Errorf("leadstore: approve batch %d: %w", id, ErrHashMismatch)
	}
	if approvedAt != nil {
		return Approval{Already: true}, nil
	}
	msgs, err := q.BatchMessages(ctx, id)
	if err != nil {
		return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
	}
	// The rows must still be what the operator was shown (an owner edit after batch new is refused).
	if sum, err := ContentSHA256(DraftsOf(msgs)); err != nil || !bytes.Equal(sum, content) {
		return Approval{}, fmt.Errorf("leadstore: approve batch %d: %w", id, ErrBatchTampered)
	}
	var items [][]Message
	for _, m := range msgs {
		if len(items) == 0 || items[len(items)-1][0].LeadID != m.LeadID {
			items = append(items, nil)
		}
		items[len(items)-1] = append(items[len(items)-1], m)
	}
	skipped := map[int]bool{}
	for _, n := range skip {
		if n < 1 || n > len(items) {
			return Approval{}, fmt.Errorf("leadstore: approve batch %d: item %d of %d: %w", id, n, len(items), ErrBadSkip)
		}
		skipped[n] = true
	}
	want := LeadBatched
	if shadow {
		want = LeadNew
	}
	var res Approval
	for i, item := range items {
		leadID := item[0].LeadID
		var tos []string
		for _, m := range item {
			if m.ToAddress != "" {
				tos = append(tos, m.ToAddress)
			}
		}
		st, err := q.leadCheck(ctx, leadID, tos)
		if err != nil {
			return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
		}
		reason := ""
		switch {
		case skipped[i+1]:
			reason = SkipByGremy
			if !shadow {
				if _, err := q.setLeadStateFrom(ctx, leadID, []string{LeadBatched}, LeadSkipped,
					fmt.Sprintf("skipped at approval of batch %d", id), ActorGremy); err != nil {
					return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
				}
			}
		case st.suppressed:
			reason = SkipSuppressed
			if !shadow {
				if _, err := q.setLeadStateFrom(ctx, leadID, []string{LeadBatched}, LeadSuppressed,
					"suppressed at approval", ActorSystem); err != nil {
					return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
				}
			}
		case st.state != want:
			reason = SkipLeadState + st.state
		}
		for _, m := range item {
			if m.State != MsgDraft {
				continue
			}
			reason := reason
			if reason == "" && m.Channel == ChannelPhone {
				// A number suppressed since batch new: this call is skipped, the rest of the
				// lead's steps go on.
				if reason, err = q.callSuppressed(ctx, leadID, m.PhoneFactID); err != nil {
					return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
				}
			}
			if reason != "" {
				if _, err := q.db.Exec(ctx,
					`UPDATE outreach_message SET state = 'skipped', cancel_reason = $2 WHERE id = $1 AND state = 'draft'`,
					m.ID, reason); err != nil {
					return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
				}
				res.Skipped++
				continue
			}
			var dueAt *time.Time
			if due != nil {
				if t := due(m); !t.IsZero() {
					dueAt = &t
				}
			}
			if _, err := q.db.Exec(ctx,
				`UPDATE outreach_message SET state = 'approved', due_at = $2, approved_sha256 = $3
				 WHERE id = $1 AND state = 'draft'`,
				m.ID, dueAt, MessageSHA256(m.ToAddress, m.Subject, m.BodyText, m.InReplyTo)); err != nil {
				return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
			}
			res.Approved++
		}
	}
	if _, err := q.db.Exec(ctx,
		`UPDATE outreach_batch SET approved_at = now(), approved_hash = content_sha256 WHERE id = $1`, id); err != nil {
		return Approval{}, fmt.Errorf("leadstore: approve batch: %w", err)
	}
	return res, nil
}

// callSuppressed returns SkipSuppressed when an active phone suppression of lead leadID's
// account holds the number of fact factID, else "". A call whose fact is gone (erased) is
// skipped too: there is no number to dial.
func (q Queries) callSuppressed(ctx context.Context, leadID int64, factID *int64) (string, error) {
	if factID == nil {
		return SkipSuppressed, nil
	}
	var suppressed bool
	if err := q.db.QueryRow(ctx,
		`SELECT `+phoneSuppressedSQL("c.account_id", "$2::bigint")+`
		 FROM lead l JOIN campaign c ON c.id = l.campaign_id WHERE l.id = $1`, leadID, *factID).Scan(&suppressed); err != nil {
		return "", err
	}
	if suppressed {
		return SkipSuppressed, nil
	}
	return "", nil
}

// DueMessage is an approved message whose time has come, with what send checks first.
type DueMessage struct {
	Message
	Shadow    bool   `db:"shadow"`
	LeadState string `db:"lead_state"`
	OrgDomain string `db:"org_domain"`
}

// DueMessages returns the approved messages of approved batches of the given kind (shadow
// or live) due at or before now, by due time then id, call rows left out (CallsDue lists
// those). Whatever the lead's state; send
// refuses those whose lead has moved on (a maybe_sent message is never approved again, so
// it never comes back).
func (q Queries) DueMessages(ctx context.Context, now time.Time, shadow bool) ([]DueMessage, error) {
	ms, err := collect(ctx, q.db, pgx.RowToStructByName[DueMessage],
		`SELECT `+messageCols+`, b.shadow, l.state AS lead_state, o.domain AS org_domain
		 FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id
		 JOIN lead l ON l.id = m.lead_id
		 JOIN org o ON o.id = l.org_id
		 WHERE m.state = 'approved' AND b.approved_at IS NOT NULL AND b.shadow = $2 AND m.channel <> 'phone'
		   AND m.due_at IS NOT NULL AND m.due_at <= $1
		 ORDER BY m.due_at, m.id`, now, shadow)
	if err != nil {
		return nil, fmt.Errorf("leadstore: due messages: %w", err)
	}
	return ms, nil
}

// MarkSending claims message id for one sender (RA3): approved → sending. It reports
// whether this call changed it; SMTP proceeds only when it did. A call row (channel phone)
// is never claimed: the operator makes the call and records its outcome.
func (q Queries) MarkSending(ctx context.Context, id int64) (bool, error) {
	tag, err := q.db.Exec(ctx, `UPDATE outreach_message SET state = 'sending'
		WHERE id = $1 AND state = 'approved' AND channel <> 'phone'`, id)
	if err != nil {
		return false, fmt.Errorf("leadstore: mark sending: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkSent records message id (sending, in a live batch) as sent at at with the server's
// reply, and moves its lead from batched to contacted.
func (q Queries) MarkSent(ctx context.Context, id int64, smtpReply string, at time.Time) error {
	var leadID int64
	err := q.db.QueryRow(ctx,
		`UPDATE outreach_message m SET state = 'sent', sent_at = $2, smtp_reply = $3
		 FROM outreach_batch b
		 WHERE m.id = $1 AND m.state = 'sending' AND b.id = m.batch_id AND NOT b.shadow
		 RETURNING m.lead_id`, id, at, smtpReply).Scan(&leadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("leadstore: mark sent: message %d: %w", id, ErrMessageState)
	}
	if err != nil {
		return fmt.Errorf("leadstore: mark sent: %w", err)
	}
	if _, err := q.setLeadStateFrom(ctx, leadID, []string{LeadBatched}, LeadContacted,
		fmt.Sprintf("message %d sent", id), ActorSystem); err != nil {
		return fmt.Errorf("leadstore: mark sent: %w", err)
	}
	return nil
}

// MarkShadowSent records message id of a shadow batch (approved or sending) as written to
// the outbox at at. The lead is never touched (RA1).
func (q Queries) MarkShadowSent(ctx context.Context, id int64, at time.Time) error {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message m SET state = 'shadow_sent', sent_at = $2
		 FROM outreach_batch b
		 WHERE m.id = $1 AND m.state IN ('approved', 'sending') AND m.channel <> 'phone' AND b.id = m.batch_id AND b.shadow`, id, at)
	return markResult("mark shadow sent", id, tag.RowsAffected(), err)
}

// MarkRefused records message id (approved or sending) as refused, by the server or by
// send's own checks; reply says why.
func (q Queries) MarkRefused(ctx context.Context, id int64, reply string) error {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'refused', smtp_reply = $2
		 WHERE id = $1 AND state IN ('approved', 'sending')`, id, reply)
	return markResult("mark refused", id, tag.RowsAffected(), err)
}

// MarkMaybeSent records message id (sending) as maybe sent: the connection broke after
// the server may have taken it. It is never re-queued.
func (q Queries) MarkMaybeSent(ctx context.Context, id int64, reply string) error {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'maybe_sent', smtp_reply = $2 WHERE id = $1 AND state = 'sending'`, id, reply)
	return markResult("mark maybe sent", id, tag.RowsAffected(), err)
}

func markResult(op string, id, rows int64, err error) error {
	if err != nil {
		return fmt.Errorf("leadstore: %s: %w", op, err)
	}
	if rows != 1 {
		return fmt.Errorf("leadstore: %s: message %d: %w", op, id, ErrMessageState)
	}
	return nil
}

// CancelTarget names whose pending messages CancelPending cancels: one lead, or every
// lead on a domain (the org's domain or the message's recipient domain, or a subdomain
// of it). Set exactly one.
type CancelTarget struct {
	LeadID int64
	Domain string
}

// CancelPending cancels the pending (draft or approved) messages of t with reason, every
// channel (call rows included) and every batch, and returns how many it cancelled. A message
// already sending or past it is left alone.
func (q Queries) CancelPending(ctx context.Context, t CancelTarget, reason string) (int64, error) {
	dom := strings.ToLower(strings.TrimSpace(t.Domain))
	if (t.LeadID == 0) == (dom == "") {
		return 0, errors.New("leadstore: cancel pending: set exactly one of lead id and domain")
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message m SET state = 'cancelled', cancel_reason = $3
		 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.id = m.lead_id AND m.state IN ('draft', 'approved')
		   AND (($1::bigint <> 0 AND m.lead_id = $1::bigint)
		     OR ($2::text <> '' AND (o.domain = $2 OR right(o.domain, length($2) + 1) = '.' || $2
		       OR lower(split_part(m.to_address, '@', 2)) = $2
		       OR right(lower(split_part(m.to_address, '@', 2)), length($2) + 1) = '.' || $2)))`,
		t.LeadID, dom, reason)
	if err != nil {
		return 0, fmt.Errorf("leadstore: cancel pending: %w", err)
	}
	return tag.RowsAffected(), nil
}
