//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// forgetShared is a batch shared by a lead whose org is about to be erased (its emails go to a
// person's address) and a lead that stays.
type forgetShared struct {
	f          outreachFixture
	gone, kept int64
	batch      int64
	hash       string
}

func newForgetShared(t *testing.T) forgetShared {
	t.Helper()
	ctx := context.Background()
	f := newOutreachFixture(t)
	gone := f.siteLead(t, "gone.localhost", nil, 7, "info@gone.localhost")
	kept := f.siteLead(t, "kept.localhost", nil, 7, "info@kept.localhost")
	org := f.orgOf(t, gone)
	person, err := f.q.AddPerson(ctx, leadstore.PersonRow{OrgID: org, Role: "Head of IT"})
	if err != nil {
		t.Fatal(err)
	}
	const personal = "pat@gone.localhost"
	if _, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: personal, PersonID: person}); err != nil {
		t.Fatal(err)
	}
	drafts := f.drafts(t, gone, kept)
	for i := range drafts {
		if drafts[i].LeadID == gone {
			drafts[i].To = personal
		}
	}
	id, sum, err := f.q.CreateBatch(ctx, false, drafts, []byte("templates"))
	if err != nil {
		t.Fatal(err)
	}
	return forgetShared{f: f, gone: gone, kept: kept, batch: id, hash: hex.EncodeToString(sum)}
}

func (s forgetShared) forget(t *testing.T) leadstore.Forgotten {
	t.Helper()
	got, err := leadstore.NewQueries(s.f.owner).ForgetOrg(context.Background(), s.f.orgOf(t, s.gone), "erasure request")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	return got
}

func (s forgetShared) row(t *testing.T) leadstore.Batch {
	t.Helper()
	b, err := s.f.q.BatchByID(context.Background(), s.batch)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sendKept sends the kept lead's first email the way a run does: it is due, its row still hashes
// to what was approved, and it can be claimed and marked sent.
func (s forgetShared) sendKept(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	rows, err := s.f.q.SendRows(ctx, dueBase.Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.LeadID == s.gone {
			t.Errorf("message %d of the erased lead is still due", r.ID)
		}
		if r.LeadID != s.kept || r.Step != 0 {
			continue
		}
		if !bytes.Equal(leadstore.MessageSHA256(r.ToAddress, r.Subject, r.BodyText, r.InReplyTo), r.ApprovedSHA256) {
			t.Fatalf("message %d no longer hashes to what was approved", r.ID)
		}
		if ok, err := s.f.q.MarkSending(ctx, r.ID); err != nil || !ok {
			t.Fatalf("mark sending: %v %v", ok, err)
		}
		if err := s.f.q.MarkSent(ctx, r.ID, "250 ok", dueBase); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("the kept lead's first email is not due: %d rows", len(rows))
}

// TestForgetOrgUnapprovedBatch: erasing one lead's org leaves a shared unapproved batch
// approvable: the operator reloads and approves the new hash, the old one is refused, only the
// erased org's rows changed, and the other lead's email sends.
func TestForgetOrgUnapprovedBatch(t *testing.T) {
	ctx := context.Background()
	s := newForgetShared(t)
	before, err := s.f.q.BatchMessages(ctx, s.batch)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.forget(t); got.BatchesRehashed != 1 {
		t.Errorf("re-hashed %d batches, want 1", got.BatchesRehashed)
	}
	b := s.row(t)
	sum, err := s.f.q.BatchContentSHA256(ctx, s.batch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sum, b.ContentSHA256) {
		t.Fatal("the stored hash does not match the rows after the erasure")
	}
	if hex.EncodeToString(b.ContentSHA256) == s.hash {
		t.Fatal("the rows changed but the hash did not, so this proves nothing")
	}
	after, err := s.f.q.BatchMessages(ctx, s.batch)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range after {
		was := before[i]
		same := m.ToAddress == was.ToAddress && m.Subject == was.Subject && m.BodyText == was.BodyText
		if m.LeadID == s.kept && !same {
			t.Errorf("the kept lead's message %d changed", m.ID)
		}
		if m.LeadID == s.gone && m.ToAddress != "" {
			t.Errorf("the erased lead's message %d keeps its target", m.ID)
		}
	}
	if _, err := s.f.q.ApproveBatch(ctx, s.batch, s.hash[:12], nil, dueAt); !errors.Is(err, leadstore.ErrHashMismatch) {
		t.Errorf("approve with the old hash: %v, want a mismatch", err)
	}
	a, err := s.f.q.ApproveBatch(ctx, s.batch, hex.EncodeToString(b.ContentSHA256)[:12], nil, dueAt)
	if err != nil {
		t.Fatalf("approve with the new hash: %v", err)
	}
	if a.Approved != 2 {
		t.Errorf("approval %+v, want the kept lead's 2 messages", a)
	}
	s.sendKept(t)
	if got := s.forget(t); got != (leadstore.Forgotten{}) {
		t.Errorf("a second run: %+v, want nothing", got)
	}
}

// TestForgetOrgApprovedBatch: erasing one lead's org from an approved batch keeps the batch
// consistent, so the other lead's approved emails keep going, on the per-message hash that was
// never touched; that hash still refuses any later change.
func TestForgetOrgApprovedBatch(t *testing.T) {
	ctx := context.Background()
	s := newForgetShared(t)
	s.f.approve(t, s.batch, s.hash)
	if got := s.forget(t); got.BatchesRehashed != 1 {
		t.Errorf("re-hashed %d batches, want 1", got.BatchesRehashed)
	}
	b := s.row(t)
	sum, err := s.f.q.BatchContentSHA256(ctx, s.batch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sum, b.ContentSHA256) || !bytes.Equal(b.ApprovedHash, b.ContentSHA256) {
		t.Fatalf("after the erasure rows %x, content %x, approved %x: the send's checks would refuse", sum, b.ContentSHA256, b.ApprovedHash)
	}
	if b.ApprovedAt == nil {
		t.Fatal("the erasure un-approved the batch")
	}
	s.sendKept(t)

	// The rest of the kept lead's approved messages stay bound to what was approved.
	second := s.f.msgID(t, s.batch, s.kept, 1)
	s.f.ownerRun(t, `UPDATE outreach_message SET body_text = 'changed by hand' WHERE id = $1`, second)
	row, err := s.f.q.SendRowByID(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(leadstore.MessageSHA256(row.ToAddress, row.Subject, row.BodyText, row.InReplyTo), row.ApprovedSHA256) {
		t.Error("a message changed after the erasure still matches its approved hash")
	}
}

// TestForgetOrgNeverLaunders: a batch that already failed its hash before the erasure stays
// refused, approved or not, and is not counted: the erasure repairs only its own change.
func TestForgetOrgNeverLaunders(t *testing.T) {
	ctx := context.Background()
	for _, approved := range []bool{false, true} {
		s := newForgetShared(t)
		if approved {
			s.f.approve(t, s.batch, s.hash)
		}
		s.f.ownerRun(t, `UPDATE outreach_message SET body_text = 'changed by hand' WHERE id = $1`, s.f.msgID(t, s.batch, s.kept, 0))
		was := s.row(t)
		if got := s.forget(t); got.BatchesRehashed != 0 {
			t.Errorf("approved %v: re-hashed %d, want 0", approved, got.BatchesRehashed)
		}
		b := s.row(t)
		if !bytes.Equal(b.ContentSHA256, was.ContentSHA256) || !bytes.Equal(b.ApprovedHash, was.ApprovedHash) {
			t.Errorf("approved %v: the hashes of a tampered batch moved", approved)
		}
		sum, err := s.f.q.BatchContentSHA256(ctx, s.batch)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(sum, b.ContentSHA256) {
			t.Errorf("approved %v: the tampered batch now matches its hash", approved)
		}
		if !approved {
			if _, err := s.f.q.ApproveBatch(ctx, s.batch, s.hash[:12], nil, dueAt); !errors.Is(err, leadstore.ErrBatchTampered) {
				t.Errorf("approve: %v, want tampered", err)
			}
		}
	}
}

// TestForgetOrgLeavesOtherBatches: a batch with no row of the erased org is not touched.
func TestForgetOrgLeavesOtherBatches(t *testing.T) {
	s := newForgetShared(t)
	other := s.f.siteLead(t, "other.localhost", nil, 7, "info@other.localhost")
	id, hash := s.f.batch(t, false, other)
	if got := s.forget(t); got.BatchesRehashed != 1 {
		t.Errorf("re-hashed %d, want the shared batch alone", got.BatchesRehashed)
	}
	b, err := s.f.q.BatchByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(b.ContentSHA256) != hash {
		t.Error("a batch without the org changed its hash")
	}
}

// TestForgetOrgCallBatch: deleting a phone fact clears the call's reference to it, which the hash
// covers, so an approved batch holding a call of the org is re-hashed too.
func TestForgetOrgCallBatch(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "calls.localhost", nil, 7, "info@calls.localhost")
	f.setCountry(t, lead, "GB")
	s := f.seqBatch(t, false, lead)
	got, err := leadstore.NewQueries(f.owner).ForgetOrg(ctx, f.orgOf(t, lead), "erasure request")
	if err != nil {
		t.Fatal(err)
	}
	if got.BatchesRehashed != 1 {
		t.Errorf("re-hashed %d, want 1", got.BatchesRehashed)
	}
	b, err := f.q.BatchByID(ctx, s.batch)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := f.q.BatchContentSHA256(ctx, s.batch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sum, b.ContentSHA256) || !bytes.Equal(b.ApprovedHash, b.ContentSHA256) {
		t.Errorf("rows %x, content %x, approved %x", sum, b.ContentSHA256, b.ApprovedHash)
	}
}
