//go:build integration

package leadstore_test

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// CP4a: Instagram is a manual channel like a contact form or LinkedIn, in every store-side
// whitelist. Every handle here is invented (privacycheck's Instagram allow list).

// igChannelLead is a café lead on domain whose channel is Instagram: its org holds the
// import's route to handle, and the lead points at it.
func (f outreachFixture) igChannelLead(t *testing.T, domain, handle string) int64 {
	t.Helper()
	lead := f.cafeLead(t, domain, nil, "")
	org := f.leadOf(t, lead).OrgID
	if _, err := f.q.InsertRoute(context.Background(), leadstore.RouteRow{OrgID: org, Kind: leadstore.RouteInstagram,
		Value: handle, DerivedBy: leadstore.DerivedByInstagram}); err != nil {
		t.Fatal(err)
	}
	route := ownerInt(t, f.owner, `SELECT id FROM contact_route WHERE org_id = $1 AND kind = 'instagram'`, org)
	f.ownerRun(t, `UPDATE lead SET channel = 'instagram', route_id = $2 WHERE id = $1`, lead, route)
	return lead
}

// TestInstagramIsManual: an Instagram lead is on the Manual tab, carries the flag and the
// filter on the list, and its one DM is asked for, edited, marked sent (the lead moves to
// contacted) and shown on the timeline like any manual item.
func TestInstagramIsManual(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.igChannelLead(t, "dm.example", "dm_cafe.example")
	other := f.cafeLead(t, "mail.example", nil, "info@mail.example")
	const url = "https://www.instagram.com/dm_cafe.example/"

	if r, err := f.q.ReachOf(ctx, lead, nil); err != nil || r.Bucket != leadstore.ReachManual {
		t.Errorf("reach = %+v %v, want manual", r, err)
	}
	rows, total, err := f.q.LeadList(ctx, leadstore.ListFilter{Channels: []string{leadstore.ListChannelInstagram}}, leadstore.ListContext{})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].LeadID != lead || !rows[0].HasInstagram || rows[0].HasLinkedIn {
		t.Fatalf("instagram filter: total %d, rows %+v, %v", total, rows, err)
	}
	extras, err := f.q.ListExtras(ctx, []int64{lead, other})
	if err != nil || !slices.Equal(extras[lead].Instagrams, []string{url}) || extras[other].Instagrams == nil || len(extras[other].Instagrams) != 0 {
		t.Fatalf("extras: %+v %v", extras, err)
	}

	if _, err := f.q.RequestDraft(ctx, lead, leadstore.RouteInstagram); err != nil {
		t.Fatalf("request an instagram draft: %v", err)
	}
	if _, err := f.q.RequestDraft(ctx, other, "tiktok"); !errors.Is(err, leadstore.ErrBadDraftRequest) {
		t.Errorf("an unknown channel: %v, want ErrBadDraftRequest", err)
	}
	f.ownerRun(t, `DELETE FROM draft_request WHERE lead_id = $1`, lead) // CreateBatch does not read it

	batch, hash, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Origin: leadstore.BatchOriginDraftIt}, false, []leadstore.DraftRow{
		{LeadID: lead, Step: 0, Channel: leadstore.RouteInstagram, To: url, Body: "dm text", MailboxID: f.mailbox(t)},
	}, nil)
	if err != nil {
		t.Fatalf("instagram batch: %v", err)
	}
	ms, err := f.q.BatchMessages(ctx, batch)
	if err != nil || len(ms) != 1 {
		t.Fatalf("messages %+v %v", ms, err)
	}
	msg := ms[0].ID
	if _, err := f.q.SaveManualEdit(ctx, msg, "", "edited dm", t13Pass); err != nil {
		t.Fatalf("edit the dm: %v", err)
	}
	if got, err := f.q.EffectiveText(ctx, msg); err != nil || got.Body != "edited dm" {
		t.Errorf("effective text %+v %v", got, err)
	}
	steps, err := f.q.SequenceOf(ctx, lead)
	if err != nil || len(steps) != 1 || steps[0].Channel != leadstore.RouteInstagram {
		t.Errorf("sequence %+v %v, want its one row as it is", steps, err)
	}
	d, err := f.q.MarkManualSent(ctx, msg, hex.EncodeToString(hash)[:12])
	if err != nil || !d.Moved || d.LeadState != leadstore.LeadContacted {
		t.Fatalf("mark sent: %+v %v", d, err)
	}
	if r, err := f.q.ReachOf(ctx, lead, nil); err != nil || r.Bucket != leadstore.ReachContacted {
		t.Errorf("after mark sent: reach %+v %v, want contacted", r, err)
	}
	items, err := f.q.LeadTimeline(ctx, lead, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(items, func(i leadstore.TimelineItem) bool {
		return i.Kind == leadstore.TimelineDone && i.Channel == leadstore.RouteInstagram && i.MessageID == msg
	}) {
		t.Errorf("timeline %+v has no done item for the dm", items)
	}
}

// TestOperatorInstagramRoute: the operator may add an Instagram account in any form a handle
// is read in; it is stored in its one canonical form, never as a person's, and anything that
// names no profile is refused.
func TestOperatorInstagramRoute(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.cafeLead(t, "typed.example", nil, "")
	org := f.leadOf(t, lead).OrgID

	added, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteInstagram, Value: "@Typed.Example"})
	if err != nil || !added.Added {
		t.Fatalf("add: %+v %v", added, err)
	}
	again, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteInstagram, Value: "https://instagram.com/typed.example?hl=ro"})
	if err != nil || again.Added || again.ID != added.ID {
		t.Errorf("the same account in another form: %+v %v, want the same row", again, err)
	}
	if v := ownerString(t, f.owner, `SELECT value FROM contact_route WHERE id = $1`, added.ID); v != "https://www.instagram.com/typed.example/" {
		t.Errorf("stored %q", v)
	}
	for _, bad := range []string{"https://www.instagram.com/p/AbC/", "https://instagram.com/", "not a handle!", "https://typed.example/insta"} {
		if _, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteInstagram, Value: bad}); !errors.Is(err, leadstore.ErrBadContact) {
			t.Errorf("%q: %v, want ErrBadContact", bad, err)
		}
	}
	person := ownerInt(t, f.owner, `INSERT INTO org_person (org_id, name, role, derived_by, actor) VALUES ($1, 'Ana Exemplu', 'Owner', 'operator@1', 'operator') RETURNING id`, org)
	if _, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteInstagram, Value: "ana.example", PersonID: person}); !errors.Is(err, leadstore.ErrBadContact) {
		t.Errorf("a person's instagram: %v, want ErrBadContact", err)
	}
}
