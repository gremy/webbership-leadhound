//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Edits to a draft, Mark sent, batch origin and the channel of a generation (00029).

// t13Pass is a check that refuses nothing.
func t13Pass(_, _ string) []string { return nil }

// t13ManualBatch writes a form text (step 0) and a LinkedIn message (step 1) for lead, unapproved,
// and returns the batch, its hash and its messages.
func (f outreachFixture) t13ManualBatch(t *testing.T, meta leadstore.BatchMeta, shadow bool, lead int64) (int64, string, []leadstore.Message) {
	t.Helper()
	ctx := context.Background()
	id, sum, err := f.q.CreateBatchFor(ctx, meta, shadow, []leadstore.DraftRow{
		{LeadID: lead, Step: 0, Channel: "form", Body: "form text", MailboxID: f.mailbox(t)},
		{LeadID: lead, Step: 1, Channel: "linkedin", Body: "after they accept"},
	}, nil)
	if err != nil {
		t.Fatalf("manual batch: %v", err)
	}
	ms, err := f.q.BatchMessages(ctx, id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("manual batch messages: %v %v", ms, err)
	}
	return id, hex.EncodeToString(sum), ms
}

// t13DraftIt is t13ManualBatch for a Draft it batch.
func (f outreachFixture) t13DraftIt(t *testing.T, lead int64) (int64, string, []leadstore.Message) {
	t.Helper()
	return f.t13ManualBatch(t, leadstore.BatchMeta{Origin: leadstore.BatchOriginDraftIt}, false, lead)
}

// t13MixedBatch writes a morning batch: lead's email at step 0, a form text at step 1, approved.
func (f outreachFixture) t13MixedBatch(t *testing.T, lead int64, to string) (int64, string, []leadstore.Message) {
	t.Helper()
	ctx := context.Background()
	id, sum, err := f.q.CreateBatch(ctx, false, []leadstore.DraftRow{
		{LeadID: lead, Step: 0, Channel: "email", To: to, Subject: "Hello", Body: "email text", MailboxID: f.mailbox(t)},
		{LeadID: lead, Step: 1, Channel: "form", Body: "form text"},
	}, nil)
	if err != nil {
		t.Fatalf("mixed batch: %v", err)
	}
	hash := hex.EncodeToString(sum)
	f.approve(t, id, hash)
	ms, err := f.q.BatchMessages(ctx, id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("mixed batch messages: %v %v", ms, err)
	}
	return id, hash, ms
}

// t13Message reads message id back as the owner sees it.
func (f outreachFixture) t13Message(t *testing.T, id int64) (state, subject, body string) {
	t.Helper()
	if err := f.owner.QueryRow(context.Background(),
		`SELECT state, subject, body_text FROM outreach_message WHERE id = $1`, id).Scan(&state, &subject, &body); err != nil {
		t.Fatalf("message %d: %v", id, err)
	}
	return state, subject, body
}

// t13EditCount counts message id's message_edit rows.
func (f outreachFixture) t13EditCount(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), `SELECT count(*) FROM message_edit WHERE message_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("edits of message %d: %v", id, err)
	}
	return n
}

// t13Stored returns batch id's stored content hash and whether it is approved.
func (f outreachFixture) t13Stored(t *testing.T, id int64) (string, bool) {
	t.Helper()
	var sum []byte
	var approved bool
	if err := f.owner.QueryRow(context.Background(),
		`SELECT content_sha256, approved_at IS NOT NULL FROM outreach_batch WHERE id = $1`, id).Scan(&sum, &approved); err != nil {
		t.Fatalf("batch %d: %v", id, err)
	}
	return hex.EncodeToString(sum), approved
}

// TestEmailEditNewHashApprovesOldHashFails is Decision 12's first two tests: an edit moves the
// message and the batch hash in one go; the new hash approves, the old one does not; approval binds
// the new text.
func TestEmailEditNewHashApprovesOldHashFails(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "edit.localhost", intp(1), 7, "info@edit.localhost")
	batch, oldHash := f.batch(t, false, lead)
	msg := f.msgID(t, batch, lead, 0)
	_, subject0, body0 := f.t13Message(t, msg)

	editID, err := f.q.SaveEmailEdit(ctx, msg, "New subject", "New body", t13Pass)
	if err != nil || editID == 0 {
		t.Fatalf("save: %d %v", editID, err)
	}
	if _, subject, body := f.t13Message(t, msg); subject != "New subject" || body != "New body" {
		t.Errorf("row after edit: %q %q", subject, body)
	}
	var sb, bb, sa, ba, actor string
	if err := f.owner.QueryRow(ctx, `SELECT subject_before, body_before, subject_after, body_after, actor
		FROM message_edit WHERE id = $1`, editID).Scan(&sb, &bb, &sa, &ba, &actor); err != nil {
		t.Fatal(err)
	}
	if sb != subject0 || bb != body0 || sa != "New subject" || ba != "New body" || actor != "operator" {
		t.Errorf("edit row: %q %q %q %q %q", sb, bb, sa, ba, actor)
	}
	newHash, _ := f.t13Stored(t, batch)
	if newHash == oldHash {
		t.Fatal("the batch hash did not move")
	}
	sum, err := f.q.BatchContentSHA256(ctx, batch)
	if err != nil || hex.EncodeToString(sum) != newHash {
		t.Errorf("stored hash %s is not what the rows hash to (%x, %v)", newHash, sum, err)
	}
	if _, err := f.q.ApproveBatch(ctx, batch, oldHash[:12], nil, dueAt); !errors.Is(err, leadstore.ErrHashMismatch) {
		t.Errorf("approve with the old hash: %v, want ErrHashMismatch", err)
	}
	if a := f.approve(t, batch, newHash); a.Approved != 2 {
		t.Errorf("approve with the new hash: %+v", a)
	}
	ms, err := f.q.BatchMessages(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ms[0].ApprovedSHA256, leadstore.MessageSHA256(ms[0].ToAddress, "New subject", "New body", ms[0].InReplyTo)) {
		t.Error("approval did not bind the edited text")
	}
}

// TestEmailEditRefusedBySQLAfterApproval is Decision 12's third test. The check runs after every Go
// test of the batch's state, on a connection with no transaction, so an approval inside it is an
// approval nothing in Go saw: only the guards in the UPDATEs stand in the way.
func TestEmailEditRefusedBySQLAfterApproval(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "guard.localhost", intp(1), 7, "info@guard.localhost")
	batch, hash := f.batch(t, false, lead)
	msg := f.msgID(t, batch, lead, 0)
	_, subject0, body0 := f.t13Message(t, msg)

	approved := false
	_, err := f.q.SaveEmailEdit(ctx, msg, "Late", "Late", func(_, _ string) []string {
		f.approve(t, batch, hash)
		approved = true
		return nil
	})
	if !approved {
		t.Fatal("the check never ran")
	}
	if !errors.Is(err, leadstore.ErrBatchApproved) {
		t.Errorf("save past the Go check: %v, want ErrBatchApproved", err)
	}
	if _, subject, body := f.t13Message(t, msg); subject != subject0 || body != body0 {
		t.Errorf("SQL let the row move: %q %q", subject, body)
	}
	if got, _ := f.t13Stored(t, batch); got != hash {
		t.Error("the approved batch was re-hashed")
	}
	if n := f.t13EditCount(t, msg); n != 0 {
		t.Errorf("%d edit rows", n)
	}
	// And with the Go check in the way, as every real save has it.
	if _, err := f.q.SaveEmailEdit(ctx, msg, "Late", "Late", t13Pass); !errors.Is(err, leadstore.ErrBatchApproved) {
		t.Errorf("save after approval: %v, want ErrBatchApproved", err)
	}
}

// TestEmailEditRefusedBySQLOnADraftOfAnApprovedBatch: with the message forced back to draft, its
// state no longer stops the UPDATE, so approved_at alone must.
func TestEmailEditRefusedBySQLOnADraftOfAnApprovedBatch(t *testing.T) {
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "guard2.localhost", intp(1), 7, "info@guard2.localhost")
	batch, hash := f.batch(t, false, lead)
	msg := f.msgID(t, batch, lead, 0)
	_, subject0, body0 := f.t13Message(t, msg)
	_, err := f.q.SaveEmailEdit(context.Background(), msg, "Late", "Late", func(_, _ string) []string {
		f.approve(t, batch, hash)
		f.ownerRun(t, `UPDATE outreach_message SET state = 'draft', due_at = NULL, approved_sha256 = NULL WHERE id = $1`, msg)
		return nil
	})
	if !errors.Is(err, leadstore.ErrBatchApproved) {
		t.Errorf("save: %v, want ErrBatchApproved", err)
	}
	if _, subject, body := f.t13Message(t, msg); subject != subject0 || body != body0 {
		t.Errorf("SQL let the row move: %q %q", subject, body)
	}
}

// TestEmailEditNeverLaundersATamperedBatch: rows already changed behind the hash stay refused; a
// re-hash would otherwise make them approvable.
func TestEmailEditNeverLaundersATamperedBatch(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "tamper.localhost", intp(1), 7, "info@tamper.localhost")
	batch, hash := f.batch(t, false, lead)
	msg := f.msgID(t, batch, lead, 0)
	f.ownerRun(t, `UPDATE outreach_message SET body_text = 'changed by hand' WHERE id = $1`, f.msgID(t, batch, lead, 1))
	if _, err := f.q.SaveEmailEdit(ctx, msg, "S", "B", t13Pass); !errors.Is(err, leadstore.ErrBatchTampered) {
		t.Errorf("save: %v, want ErrBatchTampered", err)
	}
	if got, _ := f.t13Stored(t, batch); got != hash {
		t.Error("the hash moved")
	}
}

// TestEditCheckRefusalWritesNothing: for every channel a refused, blank or unchecked text writes
// nothing, and an unchanged text writes no edit.
func TestEditCheckRefusalWritesNothing(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	emailLead := f.siteLead(t, "mail.localhost", intp(1), 7, "info@mail.localhost")
	emailBatch, emailHash := f.batch(t, false, emailLead)
	formBatch, formHash, ms := f.t13DraftIt(t, f.formLead(t, "form.localhost"))

	refuse := func(_, _ string) []string { return []string{"Banned word.", "Too long."} }
	for name, msg := range map[string]int64{
		"email": f.msgID(t, emailBatch, emailLead, 0), "form": ms[0].ID, "linkedin": ms[1].ID,
	} {
		t.Run(name, func(t *testing.T) {
			_, subject0, body0 := f.t13Message(t, msg)
			_, err := f.q.SaveMessageEdit(ctx, msg, "S", "B", refuse)
			var refused *leadstore.EditRefusedError
			if !errors.Is(err, leadstore.ErrEditRefused) || !errors.As(err, &refused) ||
				!reflect.DeepEqual(refused.Reasons, []string{"Banned word.", "Too long."}) {
				t.Errorf("refused: %v", err)
			}
			if _, err := f.q.SaveMessageEdit(ctx, msg, "S", "  \n", t13Pass); !errors.Is(err, leadstore.ErrEditRefused) {
				t.Errorf("blank: %v, want ErrEditRefused", err)
			}
			if _, err := f.q.SaveMessageEdit(ctx, msg, "S", "B", nil); err == nil {
				t.Error("no check: saved")
			}
			if id, err := f.q.SaveMessageEdit(ctx, msg, subject0, body0, t13Pass); err != nil || id != 0 {
				t.Errorf("unchanged: %d %v, want no edit", id, err)
			}
			if _, subject, body := f.t13Message(t, msg); subject != subject0 || body != body0 {
				t.Errorf("row moved: %q %q", subject, body)
			}
			if n := f.t13EditCount(t, msg); n != 0 {
				t.Errorf("%d edit rows", n)
			}
		})
	}
	if got, _ := f.t13Stored(t, emailBatch); got != emailHash {
		t.Error("email batch hash moved")
	}
	if got, _ := f.t13Stored(t, formBatch); got != formHash {
		t.Error("form batch hash moved")
	}
	if _, err := f.q.SaveMessageEdit(ctx, 99999, "S", "B", t13Pass); !errors.Is(err, leadstore.ErrMessageNotFound) {
		t.Errorf("missing message: %v", err)
	}
}

// TestManualEditLeavesRowAndHash: a manual edit on an approved morning batch moves nothing the
// batch hash or the send path reads; the latest edit is the effective text.
func TestManualEditLeavesRowAndHash(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "mixed.localhost", intp(1), 7, "info@mixed.localhost")
	batch, hash, ms := f.t13MixedBatch(t, lead, "info@mixed.localhost")
	email, form := ms[0], ms[1]

	// First edit on a template draft, no generation row: the cold start.
	before, err := f.q.EffectiveText(ctx, form.ID)
	if err != nil || before.Edited || before.Body != "form text" {
		t.Fatalf("before any edit: %+v %v", before, err)
	}
	if _, err := f.q.SaveManualEdit(ctx, form.ID, "", "first edit", t13Pass); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.SaveManualEdit(ctx, form.ID, "", "second edit", t13Pass); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.EffectiveText(ctx, form.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (leadstore.EffectiveDraft{Body: "second edit", Edited: true, OriginalBody: "form text"}); got != want {
		t.Errorf("effective: %+v, want %+v", got, want)
	}
	var secondBefore string
	if err := f.owner.QueryRow(ctx, `SELECT body_before FROM message_edit WHERE message_id = $1 ORDER BY id DESC LIMIT 1`,
		form.ID).Scan(&secondBefore); err != nil || secondBefore != "first edit" {
		t.Errorf("the second edit's before: %q %v", secondBefore, err)
	}
	if _, _, body := f.t13Message(t, form.ID); body != "form text" {
		t.Errorf("message row moved: %q", body)
	}
	stored, approved := f.t13Stored(t, batch)
	if stored != hash || !approved {
		t.Errorf("batch hash %s (approved %v), want %s", stored, approved, hash)
	}
	sum, err := f.q.BatchContentSHA256(ctx, batch)
	if err != nil || hex.EncodeToString(sum) != hash {
		t.Errorf("the batch reads as changed: %x %v", sum, err)
	}
	after, err := f.q.BatchMessages(ctx, batch)
	if err != nil || !bytes.Equal(after[0].ApprovedSHA256, email.ApprovedSHA256) {
		t.Errorf("the email's approval moved: %v", err)
	}
	if _, err := f.q.SaveManualEdit(ctx, email.ID, "S", "B", t13Pass); !errors.Is(err, leadstore.ErrNotManual) {
		t.Errorf("manual road on an email: %v, want ErrNotManual", err)
	}
	if _, err := f.q.SaveEmailEdit(ctx, form.ID, "S", "B", t13Pass); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("email road on a form text: %v, want ErrMessageState", err)
	}
	if _, err := f.q.EffectiveText(ctx, 99999); !errors.Is(err, leadstore.ErrMessageNotFound) {
		t.Errorf("missing: %v", err)
	}
}

// TestEditEffectiveTextOfEmailAndErased: an email's text is its row; an edit blanked by erasure
// counts as none.
func TestEditEffectiveTextOfEmailAndErased(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "eff.localhost", intp(1), 7, "info@eff.localhost")
	batch, _ := f.batch(t, false, lead)
	msg := f.msgID(t, batch, lead, 0)
	_, subject0, body0 := f.t13Message(t, msg)
	if _, err := f.q.SaveEmailEdit(ctx, msg, "S2", "B2", t13Pass); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.EffectiveText(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if want := (leadstore.EffectiveDraft{Subject: "S2", Body: "B2", Edited: true, OriginalSubject: subject0, OriginalBody: body0}); got != want {
		t.Errorf("email: %+v, want %+v", got, want)
	}

	_, _, ms := f.t13DraftIt(t, f.formLead(t, "erased.localhost"))
	if _, err := f.q.SaveManualEdit(ctx, ms[0].ID, "", "a name typed in", t13Pass); err != nil {
		t.Fatal(err)
	}
	f.ownerRun(t, `UPDATE message_edit SET body_before = '', body_after = '' WHERE message_id = $1`, ms[0].ID)
	got, err = f.q.EffectiveText(ctx, ms[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (leadstore.EffectiveDraft{Body: "form text"}); got != want {
		t.Errorf("erased: %+v, want %+v", got, want)
	}
}

// TestEditAndMarkSentGrid walks the Status x operation grid of the plan's Testing Strategy, the
// Edit and Mark sent columns.
func TestEditAndMarkSentGrid(t *testing.T) {
	ctx := context.Background()
	const shownNothing = "000000000000" // an approved batch is not re-checked

	// edit tries a save by the message's road and reports the error; markSent likewise.
	edit := func(f outreachFixture, id int64) error {
		_, err := f.q.SaveMessageEdit(ctx, id, "", "edited text", t13Pass)
		return err
	}

	t.Run("email, batch unapproved", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.siteLead(t, "a.localhost", intp(1), 7, "info@a.localhost")
		batch, hash := f.batch(t, false, lead)
		msg := f.msgID(t, batch, lead, 0)
		if _, err := f.q.MarkManualSent(ctx, msg, hash[:12]); !errors.Is(err, leadstore.ErrNotManual) {
			t.Errorf("mark sent: %v, want ErrNotManual", err)
		}
		if err := edit(f, msg); err != nil {
			t.Errorf("edit: %v", err)
		}
		if got, _ := f.t13Stored(t, batch); got == hash {
			t.Error("edit did not re-hash the batch")
		}
	})

	t.Run("form in an approved morning batch with live emails", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.siteLead(t, "b.localhost", intp(1), 7, "info@b.localhost")
		batch, hash, ms := f.t13MixedBatch(t, lead, "info@b.localhost")
		if err := edit(f, ms[1].ID); err != nil {
			t.Errorf("edit: %v", err)
		}
		if got, _ := f.t13Stored(t, batch); got != hash {
			t.Error("the batch hash moved")
		}
		d, err := f.q.MarkManualSent(ctx, ms[1].ID, shownNothing)
		if err != nil || !d.Moved || d.LeadState != leadstore.LeadContacted {
			t.Errorf("mark sent: %+v %v", d, err)
		}
		if state, _, _ := f.t13Message(t, ms[0].ID); state != leadstore.MsgApproved {
			t.Errorf("the email went %s", state)
		}
	})

	for _, state := range []string{leadstore.MsgApproved, leadstore.MsgSending, leadstore.MsgSent} {
		t.Run("email, "+state, func(t *testing.T) {
			f := newOutreachFixture(t)
			lead := f.siteLead(t, "c.localhost", intp(1), 7, "info@c.localhost")
			batch, hash := f.batch(t, false, lead)
			f.approve(t, batch, hash)
			msg := f.msgID(t, batch, lead, 0)
			f.ownerRun(t, `UPDATE outreach_message SET state = $2, sent_at = CASE WHEN $2 = 'sent' THEN now() END WHERE id = $1`, msg, state)
			if err := edit(f, msg); !errors.Is(err, leadstore.ErrBatchApproved) {
				t.Errorf("edit: %v, want ErrBatchApproved", err)
			}
			if _, err := f.q.MarkManualSent(ctx, msg, hash[:12]); !errors.Is(err, leadstore.ErrNotManual) {
				t.Errorf("mark sent: %v, want ErrNotManual", err)
			}
		})
	}

	t.Run("form and LinkedIn, draft, unapproved Draft it", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.formLead(t, "d.localhost")
		batch, hash, ms := f.t13DraftIt(t, lead)
		for _, m := range ms {
			if err := edit(f, m.ID); err != nil {
				t.Errorf("edit %s: %v", m.Channel, err)
			}
		}
		if got, approved := f.t13Stored(t, batch); got != hash || approved {
			t.Errorf("an edit moved the batch: %s %v", got, approved)
		}
		d, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12])
		if err != nil || !d.Moved || d.LeadState != leadstore.LeadContacted {
			t.Errorf("mark sent: %+v %v", d, err)
		}
		if _, approved := f.t13Stored(t, batch); !approved {
			t.Error("the batch was not approved")
		}
		if state, _, _ := f.t13Message(t, ms[0].ID); state != leadstore.MsgDone {
			t.Errorf("message is %s", state)
		}
		if state, _, _ := f.t13Message(t, ms[1].ID); state != leadstore.MsgApproved {
			t.Errorf("the message after they accept is %s", state)
		}
		var actor string
		if err := f.owner.QueryRow(ctx, `SELECT actor FROM lead_event WHERE lead_id = $1 AND state = 'contacted'`, lead).Scan(&actor); err != nil || actor != leadstore.ActorGremy {
			t.Errorf("lead event actor %q %v", actor, err)
		}
	})

	t.Run("form, approved morning batch, then done", func(t *testing.T) {
		f := newOutreachFixture(t)
		ms := f.manualBatch(t, false, f.formLead(t, "e.localhost"))
		if err := edit(f, ms[0].ID); err != nil {
			t.Errorf("edit: %v", err)
		}
		if _, err := f.q.MarkManualSent(ctx, ms[0].ID, shownNothing); err != nil {
			t.Errorf("mark sent: %v", err)
		}
		if err := edit(f, ms[0].ID); !errors.Is(err, leadstore.ErrMessageState) {
			t.Errorf("edit a done message: %v, want ErrMessageState", err)
		}
		if _, err := f.q.MarkManualSent(ctx, ms[0].ID, shownNothing); !errors.Is(err, leadstore.ErrMessageState) {
			t.Errorf("mark sent again: %v, want ErrMessageState", err)
		}
	})

	t.Run("cancelled and skipped refuse the edit", func(t *testing.T) {
		f := newOutreachFixture(t)
		ms := f.manualBatch(t, false, f.formLead(t, "f.localhost"))
		f.ownerRun(t, `UPDATE outreach_message SET state = 'cancelled' WHERE id = $1`, ms[0].ID)
		f.ownerRun(t, `UPDATE outreach_message SET state = 'skipped' WHERE id = $1`, ms[1].ID)
		for _, m := range ms {
			if err := edit(f, m.ID); !errors.Is(err, leadstore.ErrMessageState) {
				t.Errorf("edit: %v, want ErrMessageState", err)
			}
		}
	})

	t.Run("shadow batch", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.formLead(t, "g.localhost")
		batch, hash, ms := f.t13ManualBatch(t, leadstore.BatchMeta{}, true, lead)
		if err := edit(f, ms[0].ID); err != nil {
			t.Errorf("edit: %v", err)
		}
		_, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12])
		if !errors.Is(err, leadstore.ErrShadowMarkSent) || !strings.Contains(err.Error(), "shadow mode; nothing is marked sent") {
			t.Errorf("mark sent: %v, want ErrShadowMarkSent", err)
		}
		if _, approved := f.t13Stored(t, batch); approved {
			t.Error("the shadow batch was approved")
		}
		if got := f.leadState(t, lead); got != leadstore.LeadNew {
			t.Errorf("lead is %s", got)
		}
		email := f.siteLead(t, "g2.localhost", intp(2), 7, "info@g2.localhost")
		shadowEmail, _ := f.batch(t, true, email)
		if err := edit(f, f.msgID(t, shadowEmail, email, 0)); err != nil {
			t.Errorf("edit a shadow email: %v", err)
		}
	})

	t.Run("lead contacted, the follow-up still works (C3)", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.formLead(t, "h.localhost")
		_, hash, ms := f.t13DraftIt(t, lead)
		if _, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12]); err != nil {
			t.Fatal(err)
		}
		if got := f.leadState(t, lead); got != leadstore.LeadContacted {
			t.Fatalf("lead is %s", got)
		}
		if err := edit(f, ms[1].ID); err != nil {
			t.Errorf("edit: %v", err)
		}
		d, err := f.q.MarkManualSent(ctx, ms[1].ID, hash[:12])
		if err != nil || d.Moved || d.LeadState != leadstore.LeadContacted {
			t.Errorf("mark sent: %+v %v", d, err)
		}
	})

	for _, state := range []string{leadstore.LeadReplied, leadstore.LeadBounced, leadstore.LeadOptedOut} {
		t.Run("lead "+state, func(t *testing.T) {
			f := newOutreachFixture(t)
			lead := f.formLead(t, "i.localhost")
			batch, hash, ms := f.t13DraftIt(t, lead)
			f.ownerRun(t, `UPDATE lead SET state = $2 WHERE id = $1`, lead, state)
			if err := edit(f, ms[0].ID); !errors.Is(err, leadstore.ErrLeadAnswered) {
				t.Errorf("edit: %v, want ErrLeadAnswered", err)
			}
			if _, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12]); !errors.Is(err, leadstore.ErrLeadAnswered) {
				t.Errorf("mark sent: %v, want ErrLeadAnswered", err)
			}
			if _, approved := f.t13Stored(t, batch); approved {
				t.Error("the batch was approved")
			}
			emailLead := f.siteLead(t, "i2.localhost", intp(2), 7, "info@i2.localhost")
			emailBatch, _ := f.batch(t, false, emailLead)
			f.ownerRun(t, `UPDATE lead SET state = $2 WHERE id = $1`, emailLead, state)
			if err := edit(f, f.msgID(t, emailBatch, emailLead, 0)); !errors.Is(err, leadstore.ErrLeadAnswered) {
				t.Errorf("edit an email: %v, want ErrLeadAnswered", err)
			}
		})
	}

	t.Run("discard returns the lead to new", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.formLead(t, "j.localhost")
		batch, hash, ms := f.t13DraftIt(t, lead)
		if d, err := f.q.DiscardBatch(ctx, batch); err != nil || d.LeadsBack != 1 {
			t.Fatalf("discard: %+v %v", d, err)
		}
		if got := f.leadState(t, lead); got != leadstore.LeadNew {
			t.Errorf("lead is %s", got)
		}
		if err := edit(f, ms[0].ID); !errors.Is(err, leadstore.ErrMessageState) {
			t.Errorf("edit after discard: %v, want ErrMessageState", err)
		}
		if _, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12]); !errors.Is(err, leadstore.ErrMessageState) {
			t.Errorf("mark sent after discard: %v, want ErrMessageState", err)
		}
	})
}

// TestMarkManualSentApproveRules: who Mark sent may approve, and what a stale hash does.
func TestMarkManualSentApproveRules(t *testing.T) {
	ctx := context.Background()

	t.Run("an unapproved batch that is not manual", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.formLead(t, "a.localhost")
		batch, hash, ms := f.t13ManualBatch(t, leadstore.BatchMeta{}, false, lead)
		_, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12])
		if !errors.Is(err, leadstore.ErrApproveBatchFirst) || !strings.Contains(err.Error(), "approve the batch first") {
			t.Errorf("mark sent: %v, want ErrApproveBatchFirst", err)
		}
		if _, approved := f.t13Stored(t, batch); approved {
			t.Error("the batch was approved")
		}
	})

	t.Run("a Send now batch flipped manual is approved like Draft it", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.formLead(t, "b.localhost")
		batch, hash, ms := f.t13ManualBatch(t, leadstore.BatchMeta{}, false, lead)
		if err := f.q.MarkBatchManual(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if _, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12]); err != nil {
			t.Errorf("mark sent: %v", err)
		}
	})

	t.Run("a manual batch holding an email keeps its own approval", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.siteLead(t, "c.localhost", intp(1), 7, "info@c.localhost")
		batch, sum, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Origin: leadstore.BatchOriginDraftIt}, false, []leadstore.DraftRow{
			{LeadID: lead, Step: 0, Channel: "email", To: "info@c.localhost", Subject: "S", Body: "email text", MailboxID: f.mailbox(t)},
			{LeadID: lead, Step: 1, Channel: "linkedin", Body: "note"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.q.MarkManualSent(ctx, f.msgID(t, batch, lead, 1), hex.EncodeToString(sum)[:12]); !errors.Is(err, leadstore.ErrApproveBatchFirst) {
			t.Errorf("mark sent: %v, want ErrApproveBatchFirst", err)
		}
		if _, approved := f.t13Stored(t, batch); approved {
			t.Error("the email was approved with no send time")
		}
	})

	t.Run("a stale hash", func(t *testing.T) {
		f := newOutreachFixture(t)
		batch, hash, ms := f.t13DraftIt(t, f.formLead(t, "d.localhost"))
		wrong := strings.Repeat("0", 12)
		if hash[:12] == wrong {
			t.Skip("the hash happens to be zeros")
		}
		_, err := f.q.MarkManualSent(ctx, ms[0].ID, wrong)
		if !errors.Is(err, leadstore.ErrDraftChanged) || !strings.Contains(err.Error(), "this draft changed; reload") {
			t.Errorf("mark sent: %v, want ErrDraftChanged", err)
		}
		if state, _, _ := f.t13Message(t, ms[0].ID); state != leadstore.MsgDraft {
			t.Errorf("message is %s", state)
		}
		f.ownerRun(t, `UPDATE outreach_message SET body_text = 'changed by hand' WHERE id = $1`, ms[1].ID)
		if _, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12]); !errors.Is(err, leadstore.ErrDraftChanged) {
			t.Errorf("rows changed behind the hash: %v, want ErrDraftChanged", err)
		}
		if _, approved := f.t13Stored(t, batch); approved {
			t.Error("the batch was approved")
		}
	})

	t.Run("a manual edit before Mark sent keeps the shown hash good", func(t *testing.T) {
		f := newOutreachFixture(t)
		_, hash, ms := f.t13DraftIt(t, f.formLead(t, "e.localhost"))
		if _, err := f.q.SaveManualEdit(ctx, ms[0].ID, "", "edited", t13Pass); err != nil {
			t.Fatal(err)
		}
		if _, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12]); err != nil {
			t.Errorf("mark sent: %v", err)
		}
	})

	t.Run("approval and mark stand or fall together", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.formLead(t, "f.localhost")
		batch, hash, ms := f.t13DraftIt(t, lead)
		// A lead no longer batched: approval skips its messages, so the mark then fails.
		f.ownerRun(t, `UPDATE lead SET state = 'skipped' WHERE id = $1`, lead)
		conn := storetest.AppConn(t)
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			_, err := leadstore.NewQueries(tx).MarkManualSent(ctx, ms[0].ID, hash[:12])
			return err
		})
		if !errors.Is(err, leadstore.ErrMessageState) {
			t.Errorf("mark sent: %v, want ErrMessageState", err)
		}
		if _, approved := f.t13Stored(t, batch); approved {
			t.Error("the approval outlived the failed mark")
		}
		if state, _, _ := f.t13Message(t, ms[0].ID); state != leadstore.MsgDraft {
			t.Errorf("message is %s", state)
		}
	})

	t.Run("missing message", func(t *testing.T) {
		f := newOutreachFixture(t)
		if _, err := f.q.MarkManualSent(ctx, 99999, strings.Repeat("0", 12)); !errors.Is(err, leadstore.ErrMessageNotFound) {
			t.Errorf("mark sent: %v, want ErrMessageNotFound", err)
		}
	})
}

// TestBatchOriginFixedAtInsert: an origin is stored as given, empty is batch, a Draft it batch is
// manual and live and moves its lead, and it never blocks batch new.
func TestBatchOriginFixedAtInsert(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	plain, _, _ := f.t13ManualBatch(t, leadstore.BatchMeta{}, false, f.formLead(t, "plain.localhost"))
	got, err := f.q.BatchByID(ctx, plain)
	if err != nil || got.Origin != leadstore.BatchOriginBatch || got.Manual {
		t.Errorf("plain batch: %+v %v", got, err)
	}
	if id, ok, err := f.q.UnapprovedBatch(ctx, 0, false); err != nil || !ok || id != plain {
		t.Errorf("plain batch waits: %d %v %v", id, ok, err)
	}
	if _, err := f.q.DiscardBatch(ctx, plain); err != nil {
		t.Fatal(err)
	}

	lead := f.formLead(t, "drafted.localhost")
	drafted, _, _ := f.t13DraftIt(t, lead)
	got, err = f.q.BatchByID(ctx, drafted)
	if err != nil || got.Origin != leadstore.BatchOriginDraftIt || !got.Manual || got.Shadow {
		t.Errorf("Draft it batch: %+v %v", got, err)
	}
	if st := f.leadState(t, lead); st != leadstore.LeadBatched {
		t.Errorf("lead is %s", st)
	}
	if id, ok, err := f.q.UnapprovedBatch(ctx, 0, false); err != nil || ok {
		t.Errorf("a Draft it batch blocks batch new: %d %v %v", id, ok, err)
	}

	bad := f.formLead(t, "bad.localhost")
	if _, _, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Origin: "somewhere"}, false, []leadstore.DraftRow{
		{LeadID: bad, Step: 0, Channel: "form", Body: "x", MailboxID: f.mailbox(t)},
	}, nil); err == nil {
		t.Error("an unknown origin was stored")
	}
}

// TestStaleManualSweepKeepsDraftIt is Decision 8: a 31-minute-old Draft it batch survives the sweep
// a Send now press runs, an older Send now batch beside it does not, and Mark sent still works on
// what survived.
func TestStaleManualSweepKeepsDraftIt(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-31 * time.Minute)

	draftLead := f.formLead(t, "draftit.localhost")
	drafted, hash, ms := f.t13DraftIt(t, draftLead)
	staleLead := f.siteLead(t, "sendnow.localhost", intp(1), 7, "info@sendnow.localhost")
	stale, _ := f.sendNowBatch(t, staleLead)
	f.ownerRun(t, `UPDATE outreach_batch SET created_at = $2 WHERE id = ANY($1)`, []int64{drafted, stale}, old)

	n, err := f.q.DiscardStaleManualBatches(ctx, now.Add(-30*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v, want 1", n, err)
	}
	if b, err := f.q.BatchByID(ctx, drafted); err != nil || b.DiscardedAt != nil {
		t.Errorf("the Draft it batch: %+v %v", b, err)
	}
	if st := f.leadState(t, draftLead); st != leadstore.LeadBatched {
		t.Errorf("Draft it lead is %s", st)
	}
	if b, err := f.q.BatchByID(ctx, stale); err != nil || b.DiscardedAt == nil {
		t.Errorf("the Send now batch was not discarded: %+v %v", b, err)
	}
	if st := f.leadState(t, staleLead); st != leadstore.LeadNew {
		t.Errorf("Send now lead is %s", st)
	}
	// A pre-00029 row, backfilled to send_now, is swept as before.
	oldLead := f.siteLead(t, "backfill.localhost", intp(2), 7, "info@backfill.localhost")
	backfilled, _ := f.sendNowBatch(t, oldLead)
	f.ownerRun(t, `UPDATE outreach_batch SET origin = 'send_now', created_at = $2 WHERE id = $1`, backfilled, old)
	if n, err := f.q.DiscardStaleManualBatches(ctx, now.Add(-30*time.Minute)); err != nil || n != 1 {
		t.Errorf("sweep of a backfilled row: %d %v, want 1", n, err)
	}
	if _, err := f.q.MarkManualSent(ctx, ms[0].ID, hash[:12]); err != nil {
		t.Errorf("mark sent on the surviving Draft it batch: %v", err)
	}
}

// TestEditOperatorVersions: the latest saved text per lead, newest first, on one channel of one
// campaign, with the lead's names gone; an erased edit is out.
func TestEditOperatorVersions(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	if got, err := f.q.OperatorVersions(ctx, 1, "form", 2); err != nil || len(got) != 0 {
		t.Fatalf("cold start: %v %v", got, err)
	}
	orgOf := func(lead int64) int64 {
		var org int64
		if err := f.owner.QueryRow(ctx, `SELECT org_id FROM lead WHERE id = $1`, lead).Scan(&org); err != nil {
			t.Fatal(err)
		}
		return org
	}
	a := f.formLead(t, "harbourlane.localhost")
	f.ownerRun(t, `UPDATE org SET name = 'Harbour Lane Studio' WHERE id = $1`, orgOf(a))
	f.ownerRun(t, `INSERT INTO org_person (org_id, name, role, derived_by, actor) VALUES ($1, 'Jane Doe', 'Web lead', 'operator@1', 'operator')`, orgOf(a))
	_, _, ams := f.t13DraftIt(t, a)
	b := f.formLead(t, "second.localhost")
	_, _, bms := f.t13DraftIt(t, b)
	c := f.formLead(t, "third.localhost")
	_, _, cms := f.t13DraftIt(t, c)

	save := func(id int64, body string) {
		t.Helper()
		if _, err := f.q.SaveManualEdit(ctx, id, "", body, t13Pass); err != nil {
			t.Fatal(err)
		}
	}
	save(ams[0].ID, "an older text")
	save(bms[0].ID, "Text of the second lead.")
	save(ams[0].ID, "Hi Jane, Jane Doe at HARBOUR LANE STUDIO (harbourlane.localhost) and Janet the partisan.")
	save(ams[1].ID, "linkedin text")
	save(cms[0].ID, "erased text")
	f.ownerRun(t, `UPDATE message_edit SET body_before = '', body_after = '' WHERE message_id = $1`, cms[0].ID)

	got, err := f.q.OperatorVersions(ctx, 1, "form", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].LeadID != a || got[1].LeadID != b {
		t.Fatalf("versions: %+v", got)
	}
	if want := "Hi <name>, <name> at <org> (<org>) and Janet the partisan."; got[0].Body != want {
		t.Errorf("scrubbed: %q, want %q", got[0].Body, want)
	}
	if got[0].MessageID != ams[0].ID || got[0].Step != 0 {
		t.Errorf("version: %+v", got[0])
	}
	if one, err := f.q.OperatorVersions(ctx, 1, "form", 1); err != nil || len(one) != 1 || one[0].LeadID != a {
		t.Errorf("limit 1: %+v %v", one, err)
	}
	if li, err := f.q.OperatorVersions(ctx, 1, "linkedin", 5); err != nil || len(li) != 1 || li[0].Body != "linkedin text" {
		t.Errorf("linkedin: %+v %v", li, err)
	}
	if other, err := f.q.OperatorVersions(ctx, 2, "form", 5); err != nil || len(other) != 0 {
		t.Errorf("another campaign: %+v %v", other, err)
	}
}

// TestGenerationChannelKeepsManualOutOfTheGate is Decision 9a: a manual generation is stored under
// its channel, and none of the email readers sees it.
func TestGenerationChannelKeepsManualOutOfTheGate(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "channels.localhost", intp(1), 7, "info@channels.localhost")
	sheet := sheetFor("7")
	record := func(channel, purpose string) int64 {
		t.Helper()
		id, err := f.q.RecordGeneration(ctx, leadstore.GenerationRow{LeadID: lead, CampaignID: 1, Attempt: 1,
			Model: "fake", Facts: sheet, Output: []byte(goodOutput), CheckOK: true, Purpose: purpose, Channel: channel})
		if err != nil {
			t.Fatalf("record %q: %v", channel, err)
		}
		return id
	}
	email := record("", leadstore.PurposeSample)
	form := record("form", leadstore.PurposeSample)
	linkedin := record("linkedin", leadstore.PurposeBatch)
	instagram := record("instagram", leadstore.PurposeBatch)

	if g, err := f.q.GenerationByID(ctx, email); err != nil || g.Channel != "email" {
		t.Errorf("empty channel stored as %q: %v", g.Channel, err)
	}
	if g, err := f.q.GenerationByID(ctx, form); err != nil || g.Channel != "form" {
		t.Errorf("form stored as %q: %v", g.Channel, err)
	}
	for channel, want := range map[string]int64{"email": email, "form": form, "linkedin": linkedin, "instagram": instagram} {
		gens, err := f.q.GenerationsForLead(ctx, lead, 1, channel)
		if err != nil || len(gens) != 1 || gens[0].ID != want {
			t.Errorf("generations on %s: %+v %v, want %d", channel, gens, err, want)
		}
		g, ok, err := f.q.LatestGenerationForLead(ctx, lead, 1, channel, sheet.SHA256())
		if err != nil || !ok || g.ID != want {
			t.Errorf("latest on %s: %d %v %v, want %d", channel, g.ID, ok, err, want)
		}
	}
	for _, id := range []int64{email, form} {
		if _, err := f.q.RecordReview(ctx, id, leadstore.VerdictApprove, nil, "", leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	reviews, err := f.q.ReviewsForCampaign(ctx, 1, "email", 0)
	if err != nil || len(reviews) != 1 || reviews[0].GenerationID != email {
		t.Errorf("the gate's reviews: %+v %v", reviews, err)
	}
	if reviews, err = f.q.ReviewsForCampaign(ctx, 1, "form", 0); err != nil || len(reviews) != 1 || reviews[0].GenerationID != form {
		t.Errorf("form reviews: %+v %v", reviews, err)
	}
	samples, err := f.q.SampleGenerations(ctx, 1, 0)
	if err != nil || len(samples) != 1 || samples[0].ID != email {
		t.Errorf("samples: %+v %v", samples, err)
	}

	// A lead reviewed only on a manual draft is no lead eval regenerates.
	other := f.formLead(t, "manualonly.localhost")
	id, err := f.q.RecordGeneration(ctx, leadstore.GenerationRow{LeadID: other, CampaignID: 1, Attempt: 1, Model: "fake",
		Facts: sheet, Output: []byte(goodOutput), CheckOK: true, Purpose: leadstore.PurposeBatch, Channel: "form"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.RecordReview(ctx, id, leadstore.VerdictApprove, nil, "", leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if leads, err := f.q.ReviewedLeads(ctx, 1); err != nil || !reflect.DeepEqual(leads, []int64{lead}) {
		t.Errorf("reviewed leads: %v %v", leads, err)
	}

	if _, err := f.q.RecordGeneration(ctx, leadstore.GenerationRow{LeadID: lead, CampaignID: 1, Attempt: 1, Model: "fake",
		Facts: outreach.FactSheet{}, Output: []byte(goodOutput), CheckOK: true, Purpose: leadstore.PurposeBatch, Channel: "phone"}); !errors.Is(err, leadstore.ErrBadGeneration) {
		t.Errorf("channel phone: %v, want ErrBadGeneration", err)
	}
}
