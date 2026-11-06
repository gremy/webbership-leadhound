package leadstore

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Edits to a draft and Mark sent (00029). A batch's content hash covers every message, so there
// are two roads. A form, LinkedIn or Instagram edit never touches outreach_message: the latest message_edit
// is the effective text and no batch hash moves, so an approved batch carrying live emails is
// never called tampered. An email edit is allowed only while its batch is unapproved: it moves
// the message and re-hashes the batch in one transaction, and the operator approves the hash he
// is shown.

// EditCheck is the check a saved text must pass; each string is one reason to refuse. The caller
// brings it (CheckIn, CheckText and the note limit), so the store stays free of outreach rules.
type EditCheck func(subject, body string) []string

var (
	// ErrEditRefused is returned for a text the check refused; nothing was written. The error
	// is an *EditRefusedError.
	ErrEditRefused = errors.New("leadstore: the text did not pass the checks")
	// ErrLeadAnswered is returned by the edits and MarkManualSent for a lead that replied,
	// bounced or opted out: its pending drafts are no longer the operator's to use.
	ErrLeadAnswered = errors.New("leadstore: the lead has replied, bounced or opted out")
	// ErrApproveBatchFirst is returned by MarkManualSent for an unapproved batch it may not
	// approve itself: one that is not manual, or one holding an email, which needs the send
	// time only its own approval gives.
	ErrApproveBatchFirst = errors.New("leadstore: approve the batch first")
	// ErrShadowMarkSent is returned by MarkManualSent for a message of a shadow batch.
	ErrShadowMarkSent = errors.New("leadstore: shadow mode; nothing is marked sent")
	// ErrDraftChanged is returned by MarkManualSent when the hash the operator was shown is no
	// longer the batch's.
	ErrDraftChanged = errors.New("leadstore: this draft changed; reload")
)

// EditRefusedError is ErrEditRefused with the reasons, in plain words.
type EditRefusedError struct {
	Reasons []string
}

func (e *EditRefusedError) Error() string {
	return fmt.Sprintf("%s: %s", ErrEditRefused, strings.Join(e.Reasons, "; "))
}

// Is makes errors.Is(err, ErrEditRefused) true.
func (e *EditRefusedError) Is(target error) bool { return target == ErrEditRefused }

// runEditCheck refuses a blank text and whatever check refuses. A nil check refuses too: no text
// is saved unchecked.
func runEditCheck(check EditCheck, subject, body string) error {
	if check == nil {
		return errors.New("no check given")
	}
	if strings.TrimSpace(body) == "" {
		return &EditRefusedError{Reasons: []string{"The text is empty."}}
	}
	if reasons := check(subject, body); len(reasons) > 0 {
		return &EditRefusedError{Reasons: reasons}
	}
	return nil
}

// leadAnswered is the read-side rule of who may still be written to by hand. A contacted lead may:
// the LinkedIn message after they accept is used after the note moved the lead.
func leadAnswered(state string) bool {
	return state == LeadReplied || state == LeadBounced || state == LeadOptedOut
}

// draftTarget is what an edit or Mark sent reads about one message.
type draftTarget struct {
	batchID, leadID                     int64
	channel, state, subject, body       string
	leadState                           string
	content                             []byte
	approved, discarded, shadow, manual bool
}

// Which row a draftTarget read locks: an email edit and Mark sent take the batch, as approval
// does; a manual edit takes only the message, so it never waits on a batch.
const (
	lockTargetBatch   = "FOR UPDATE OF b"
	lockTargetMessage = "FOR UPDATE OF m"
)

func (q Queries) draftTarget(ctx context.Context, id int64, lock string) (draftTarget, error) {
	var t draftTarget
	err := q.db.QueryRow(ctx,
		`SELECT m.batch_id, m.lead_id, m.channel, m.state, m.subject, m.body_text, l.state, b.content_sha256,
		   b.approved_at IS NOT NULL, b.discarded_at IS NOT NULL, b.shadow, b.manual
		 FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id
		 JOIN lead l ON l.id = m.lead_id
		 WHERE m.id = $1 `+lock, id).Scan(&t.batchID, &t.leadID, &t.channel, &t.state, &t.subject, &t.body,
		&t.leadState, &t.content, &t.approved, &t.discarded, &t.shadow, &t.manual)
	if errors.Is(err, pgx.ErrNoRows) {
		return draftTarget{}, fmt.Errorf("message %d: %w", id, ErrMessageNotFound)
	}
	return t, err
}

// insertMessageEdit appends one edit and returns its id.
func (q Queries) insertMessageEdit(ctx context.Context, id int64, subjectBefore, bodyBefore, subject, body string) (int64, error) {
	var editID int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO message_edit (message_id, subject_before, body_before, subject_after, body_after, actor)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		id, subjectBefore, bodyBefore, subject, body, ActorOperator).Scan(&editID)
	return editID, err
}

// SaveMessageEdit saves a new text for message id by the road of its channel: SaveEmailEdit for
// an email, SaveManualEdit otherwise. It returns what they return. Run it in a transaction.
func (q Queries) SaveMessageEdit(ctx context.Context, id int64, subject, body string, check EditCheck) (int64, error) {
	var channel string
	err := q.db.QueryRow(ctx, `SELECT channel FROM outreach_message WHERE id = $1`, id).Scan(&channel)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("leadstore: save edit: message %d: %w", id, ErrMessageNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: save edit: %w", err)
	}
	if channel == "email" {
		return q.SaveEmailEdit(ctx, id, subject, body, check)
	}
	return q.SaveManualEdit(ctx, id, subject, body, check)
}

// SaveManualEdit saves a new text for form, LinkedIn or Instagram message id as one message_edit row and
// returns its id, 0 when the text is what it already was (nothing is written). The message row and
// the batch hash stay as they are. The message must be draft or approved and its lead must not have
// replied, bounced or opted out (ErrLeadAnswered); a state refused is ErrMessageState, an email or
// call ErrNotManual, a text refused by check an *EditRefusedError. Nothing is written then. Run it
// in a transaction.
func (q Queries) SaveManualEdit(ctx context.Context, id int64, subject, body string, check EditCheck) (int64, error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: save manual edit: %w", err) }
	t, err := q.draftTarget(ctx, id, lockTargetMessage)
	if err != nil {
		return 0, wrap(err)
	}
	switch {
	case !manualChannel(t.channel):
		return 0, wrap(fmt.Errorf("message %d (%s): %w", id, t.channel, ErrNotManual))
	case t.state != MsgDraft && t.state != MsgApproved:
		return 0, wrap(fmt.Errorf("message %d is %s: %w", id, t.state, ErrMessageState))
	case leadAnswered(t.leadState):
		return 0, wrap(fmt.Errorf("lead %d is %s: %w", t.leadID, t.leadState, ErrLeadAnswered))
	}
	if err := runEditCheck(check, subject, body); err != nil {
		return 0, wrap(err)
	}
	before, err := q.EffectiveText(ctx, id)
	if err != nil {
		return 0, err
	}
	if before.Subject == subject && before.Body == body {
		return 0, nil
	}
	editID, err := q.insertMessageEdit(ctx, id, before.Subject, before.Body, subject, body)
	if err != nil {
		return 0, wrap(err)
	}
	return editID, nil
}

// SaveEmailEdit saves a new subject and body for email message id, returning the message_edit row's
// id, 0 when the text is what it already was (nothing is written). With the batch row locked, the
// way ApproveBatch locks it, the message moves, the batch is re-hashed with the code approval
// recomputes it with, and the edit is recorded. The message must be a draft of a batch neither
// approved (ErrBatchApproved) nor discarded (ErrBatchDiscarded) whose rows still hash to its stored
// hash (ErrBatchTampered: a re-hash must never launder an edit made elsewhere), and its lead must
// not have replied, bounced or opted out (ErrLeadAnswered). A text refused by check is an
// *EditRefusedError. The writes carry the unapproved guard themselves, so an approval that got past
// every check here still finds the message unmoved. Run it in a transaction.
func (q Queries) SaveEmailEdit(ctx context.Context, id int64, subject, body string, check EditCheck) (int64, error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: save email edit: %w", err) }
	t, err := q.draftTarget(ctx, id, lockTargetBatch)
	if err != nil {
		return 0, wrap(err)
	}
	switch {
	case t.channel != "email":
		return 0, wrap(fmt.Errorf("message %d is %s, not email: %w", id, t.channel, ErrMessageState))
	case t.discarded:
		return 0, wrap(fmt.Errorf("batch %d: %w", t.batchID, ErrBatchDiscarded))
	case t.approved:
		return 0, wrap(fmt.Errorf("batch %d: %w", t.batchID, ErrBatchApproved))
	case t.state != MsgDraft:
		return 0, wrap(fmt.Errorf("message %d is %s: %w", id, t.state, ErrMessageState))
	case leadAnswered(t.leadState):
		return 0, wrap(fmt.Errorf("lead %d is %s: %w", t.leadID, t.leadState, ErrLeadAnswered))
	}
	sum, err := q.BatchContentSHA256(ctx, t.batchID)
	if err != nil {
		return 0, wrap(err)
	}
	if !bytes.Equal(sum, t.content) {
		return 0, wrap(fmt.Errorf("batch %d: %w", t.batchID, ErrBatchTampered))
	}
	if err := runEditCheck(check, subject, body); err != nil {
		return 0, wrap(err)
	}
	if t.subject == subject && t.body == body {
		return 0, nil
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message m SET subject = $2, body_text = $3
		 FROM outreach_batch b
		 WHERE m.id = $1 AND m.state = 'draft' AND b.id = m.batch_id
		   AND b.approved_at IS NULL AND b.discarded_at IS NULL`, id, subject, body)
	if err != nil {
		return 0, wrap(err)
	}
	if tag.RowsAffected() != 1 {
		return 0, wrap(fmt.Errorf("batch %d: %w", t.batchID, ErrBatchApproved))
	}
	if sum, err = q.BatchContentSHA256(ctx, t.batchID); err != nil {
		return 0, wrap(err)
	}
	tag, err = q.db.Exec(ctx,
		`UPDATE outreach_batch SET content_sha256 = $2 WHERE id = $1 AND approved_at IS NULL AND discarded_at IS NULL`,
		t.batchID, sum)
	if err != nil {
		return 0, wrap(err)
	}
	if tag.RowsAffected() != 1 {
		return 0, wrap(fmt.Errorf("batch %d: %w", t.batchID, ErrBatchApproved))
	}
	editID, err := q.insertMessageEdit(ctx, id, t.subject, t.body, subject, body)
	if err != nil {
		return 0, wrap(err)
	}
	return editID, nil
}

// EffectiveDraft is the text of a message as it stands, and the text it started from.
type EffectiveDraft struct {
	Subject string
	Body    string
	// Edited says the operator changed it; the Original fields then hold the text before his
	// first edit (else they are empty).
	Edited          bool
	OriginalSubject string
	OriginalBody    string
}

// EffectiveText returns message id's text: for a form, LinkedIn or Instagram message the latest edit, else the
// row; for an email the row, which an edit moves. An edit blanked by erasure counts as no edit.
// A missing message is ErrMessageNotFound.
func (q Queries) EffectiveText(ctx context.Context, id int64) (EffectiveDraft, error) {
	var (
		channel                 string
		d                       EffectiveDraft
		lastSubject, lastBody   *string
		firstSubject, firstBody *string
	)
	err := q.db.QueryRow(ctx,
		`SELECT m.channel, m.subject, m.body_text, newest.subject_after, newest.body_after,
		   oldest.subject_before, oldest.body_before
		 FROM outreach_message m
		 LEFT JOIN LATERAL (SELECT subject_after, body_after FROM message_edit
		   WHERE message_id = m.id AND body_after <> '' ORDER BY id DESC LIMIT 1) newest ON true
		 LEFT JOIN LATERAL (SELECT subject_before, body_before FROM message_edit
		   WHERE message_id = m.id AND body_after <> '' ORDER BY id LIMIT 1) oldest ON true
		 WHERE m.id = $1`, id).Scan(&channel, &d.Subject, &d.Body, &lastSubject, &lastBody, &firstSubject, &firstBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return EffectiveDraft{}, fmt.Errorf("leadstore: effective text: message %d: %w", id, ErrMessageNotFound)
	}
	if err != nil {
		return EffectiveDraft{}, fmt.Errorf("leadstore: effective text: %w", err)
	}
	if lastBody == nil {
		return d, nil
	}
	d.Edited, d.OriginalSubject, d.OriginalBody = true, *firstSubject, *firstBody
	if channel != "email" {
		d.Subject, d.Body = *lastSubject, *lastBody
	}
	return d, nil
}

// OperatorVersion is one text the operator saved, scrubbed of its lead's names, for the writer to
// match.
type OperatorVersion struct {
	MessageID int64
	LeadID    int64
	Step      int
	Subject   string
	Body      string
}

// OperatorVersions returns the operator's latest saved text per lead on channel of campaign
// campaignID, newest first, at most limit (limit <= 0: all): the org's domain and name become
// "<org>" and any person of the org "<name>", so a version of one lead cannot leak into another's
// draft. An edit blanked by erasure is left out.
func (q Queries) OperatorVersions(ctx context.Context, campaignID int64, channel string, limit int) ([]OperatorVersion, error) {
	lim := any(nil)
	if limit > 0 {
		lim = limit
	}
	rows, err := q.db.Query(ctx,
		`SELECT message_id, lead_id, step, subject_after, body_after, org_name, domain, names FROM (
		   SELECT DISTINCT ON (m.lead_id) e.id, e.message_id, m.lead_id, m.step, e.subject_after, e.body_after,
		     o.name AS org_name, o.domain,
		     ARRAY(SELECT p.name FROM org_person p WHERE p.org_id = o.id AND p.name IS NOT NULL) AS names
		   FROM message_edit e
		   JOIN outreach_message m ON m.id = e.message_id
		   JOIN outreach_batch b ON b.id = m.batch_id
		   JOIN lead l ON l.id = m.lead_id
		   JOIN org o ON o.id = l.org_id
		   WHERE b.campaign_id = $1 AND m.channel = $2 AND e.body_after <> ''
		   ORDER BY m.lead_id, e.id DESC) v
		 ORDER BY id DESC LIMIT $3`, campaignID, channel, lim)
	if err != nil {
		return nil, fmt.Errorf("leadstore: operator versions: %w", err)
	}
	defer rows.Close()
	var out []OperatorVersion
	for rows.Next() {
		var (
			v           OperatorVersion
			org, domain string
			names       []string
		)
		if err := rows.Scan(&v.MessageID, &v.LeadID, &v.Step, &v.Subject, &v.Body, &org, &domain, &names); err != nil {
			return nil, fmt.Errorf("leadstore: operator versions: %w", err)
		}
		v.Subject = scrubVersion(v.Subject, org, domain, names)
		v.Body = scrubVersion(v.Body, org, domain, names)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leadstore: operator versions: %w", err)
	}
	return out, nil
}

// scrubVersion replaces domain and org with "<org>" and each of names, whole and word by word, with
// "<name>". The longest goes first, so a full name is never left half replaced.
func scrubVersion(text, org, domain string, names []string) string {
	text = replaceWords(text, domain, "<org>")
	text = replaceWords(text, org, "<org>")
	var words []string
	for _, name := range names {
		words = append(words, name)
		for _, w := range strings.Fields(name) {
			if utf8.RuneCountInString(strings.Trim(w, ".,")) > 1 {
				words = append(words, strings.Trim(w, ".,"))
			}
		}
	}
	slices.SortStableFunc(words, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, w := range words {
		text = replaceWords(text, w, "<name>")
	}
	return text
}

// replaceWords replaces word in text, ignoring case, wherever it stands as whole words: "Art" goes
// from "Art House" but not from "Part".
func replaceWords(text, word, with string) string {
	word = strings.TrimSpace(word)
	if word == "" {
		return text
	}
	re, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(word))
	if err != nil {
		return text
	}
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringIndex(text, -1) {
		before, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
		after, _ := utf8.DecodeRuneInString(text[loc[1]:])
		if loc[0] > 0 && isWordRune(before) || loc[1] < len(text) && isWordRune(after) {
			continue
		}
		b.WriteString(text[last:loc[0]])
		b.WriteString(with)
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// MarkManualSent records that the operator used form, LinkedIn or Instagram message id by hand: his approval
// and his sending in one act. With the batch row locked: an unapproved manual batch is approved at
// hashPrefix, the hash he was shown (a hash no longer the batch's, or rows no longer hashing to it,
// is ErrDraftChanged); then MarkDone, the lead moving from batched to contacted, recorded as
// ActorGremy. An approved batch is not re-checked: a manual edit never moves its hash.
//
// Refused, with nothing changed: an email or call (ErrNotManual); a shadow batch (ErrShadowMarkSent);
// a message neither draft nor approved (ErrMessageState); a lead that replied, bounced or opted out
// (ErrLeadAnswered, while a contacted lead still may: the message after they accept); an unapproved
// batch that is not manual or holds an email (ErrApproveBatchFirst). Run it in a transaction: the
// approval and the mark stand or fall together.
func (q Queries) MarkManualSent(ctx context.Context, id int64, hashPrefix string) (Done, error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: mark manual sent: %w", err) }
	t, err := q.draftTarget(ctx, id, lockTargetBatch)
	if err != nil {
		return Done{}, wrap(err)
	}
	switch {
	case !manualChannel(t.channel):
		return Done{}, wrap(fmt.Errorf("message %d (%s): %w", id, t.channel, ErrNotManual))
	case t.shadow:
		return Done{}, wrap(fmt.Errorf("message %d: %w", id, ErrShadowMarkSent))
	case t.state != MsgDraft && t.state != MsgApproved:
		return Done{}, wrap(fmt.Errorf("message %d is %s: %w", id, t.state, ErrMessageState))
	case leadAnswered(t.leadState):
		return Done{}, wrap(fmt.Errorf("lead %d is %s: %w", t.leadID, t.leadState, ErrLeadAnswered))
	}
	if !t.approved {
		var hasEmail bool
		if err := q.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM outreach_message WHERE batch_id = $1 AND channel = 'email')`,
			t.batchID).Scan(&hasEmail); err != nil {
			return Done{}, wrap(err)
		}
		if !t.manual || hasEmail {
			return Done{}, wrap(fmt.Errorf("batch %d: %w", t.batchID, ErrApproveBatchFirst))
		}
		_, err := q.ApproveBatch(ctx, t.batchID, hashPrefix, nil, nil)
		if errors.Is(err, ErrHashMismatch) || errors.Is(err, ErrBatchTampered) {
			return Done{}, wrap(fmt.Errorf("batch %d: %w: %w", t.batchID, ErrDraftChanged, err))
		}
		if err != nil {
			return Done{}, wrap(err)
		}
	}
	d, err := q.MarkDone(ctx, id, ActorGremy)
	if err != nil {
		return Done{}, wrap(err)
	}
	return d, nil
}
