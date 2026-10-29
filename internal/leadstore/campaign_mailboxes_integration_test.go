//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestCampaignMailboxAssign: no row is not assigned; assign and unassign each write one row
// and the latest wins; a write that changes nothing writes nothing; both readers agree; a
// mailbox and a campaign of different accounts, a missing side, a retired mailbox and an
// unknown actor are refused.
func TestCampaignMailboxAssign(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	owner := storetest.OwnerConn(t)
	rows := func() int64 { return ownerInt(t, owner, `SELECT count(*) FROM campaign_mailbox`) }

	a := leadstoretest.SeedMailbox(t, q, "a@cm-one.example")
	b := leadstoretest.SeedMailbox(t, q, "b@cm-two.example")
	cafes := register(t, q, leadstore.DefaultAccountID, "cafes", `{"slug": "cafes"}`)
	other := ownerInt(t, owner, `INSERT INTO account (slug, name) VALUES ('second', 'Second') RETURNING id`)
	foreign := register(t, q, other, "foreign", `{"slug": "foreign"}`)

	assignedTo := func(campaign int64) []int64 {
		t.Helper()
		cms, err := q.CampaignMailboxes(ctx, campaign)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, cm := range cms {
			if !cm.Assigned || cm.CampaignID != max(campaign, leadstore.DefaultCampaignID) || cm.At.IsZero() {
				t.Errorf("campaign %d: row %+v", campaign, cm)
			}
			ids = append(ids, cm.MailboxID)
		}
		return ids
	}
	set := func(campaign, mb int64, on bool, wantChanged bool) {
		t.Helper()
		changed, err := q.SetCampaignMailbox(ctx, campaign, mb, on, leadstore.ActorGremy)
		if err != nil || changed != wantChanged {
			t.Fatalf("set %d %d %v: changed %v %v, want %v", campaign, mb, on, changed, err, wantChanged)
		}
	}

	// Cold start: nothing assigned; an unassign of an unassigned pair writes nothing.
	if got := assignedTo(leadstore.DefaultCampaignID); len(got) != 0 {
		t.Fatalf("cold start: %v", got)
	}
	set(leadstore.DefaultCampaignID, a.ID, false, false)
	if n := rows(); n != 0 {
		t.Fatalf("unassign of nothing wrote %d rows", n)
	}

	set(leadstore.DefaultCampaignID, a.ID, true, true)
	set(leadstore.DefaultCampaignID, a.ID, true, false) // unchanged: skipped
	set(cafes, a.ID, true, true)
	set(cafes, b.ID, true, true)
	if n := rows(); n != 3 {
		t.Fatalf("rows %d, want 3", n)
	}
	if got := assignedTo(leadstore.DefaultCampaignID); len(got) != 1 || got[0] != a.ID {
		t.Errorf("drupal-eol: %v, want [%d]", got, a.ID)
	}
	if got := assignedTo(0); len(got) != 1 || got[0] != a.ID {
		t.Errorf("campaign 0 (the default): %v, want [%d]", got, a.ID)
	}
	if got := assignedTo(cafes); len(got) != 2 || got[0] != a.ID || got[1] != b.ID {
		t.Errorf("cafes: %v, want [%d %d]", got, a.ID, b.ID)
	}
	cms, err := q.MailboxCampaigns(ctx, a.ID)
	if err != nil || len(cms) != 2 || cms[0].CampaignID != leadstore.DefaultCampaignID || cms[1].CampaignID != cafes {
		t.Errorf("mailbox a's campaigns: %+v %v", cms, err)
	}

	// Unassign: the latest row wins, the history stays.
	set(cafes, a.ID, false, true)
	set(cafes, a.ID, false, false)
	if got := assignedTo(cafes); len(got) != 1 || got[0] != b.ID {
		t.Errorf("cafes after unassign: %v, want [%d]", got, b.ID)
	}
	for _, c := range []struct {
		campaign, mb int64
		want         bool
	}{{cafes, a.ID, false}, {cafes, b.ID, true}, {leadstore.DefaultCampaignID, a.ID, true}, {leadstore.DefaultCampaignID, b.ID, false}} {
		if got, err := q.MailboxAssigned(ctx, c.campaign, c.mb); err != nil || got != c.want {
			t.Errorf("MailboxAssigned(%d, %d) = %v %v, want %v", c.campaign, c.mb, got, err, c.want)
		}
	}
	if n := rows(); n != 4 {
		t.Fatalf("rows %d, want 4", n)
	}

	// Refusals write nothing.
	for _, c := range []struct {
		name         string
		campaign, mb int64
		actor        string
		want         error
	}{
		{"cross account", foreign, a.ID, leadstore.ActorGremy, leadstore.ErrCrossAccount},
		{"no campaign", 9999, a.ID, leadstore.ActorGremy, leadstore.ErrCampaignNotFound},
		{"no mailbox", cafes, 9999, leadstore.ActorGremy, leadstore.ErrMailboxNotFound},
		{"bad actor", cafes, a.ID, leadstore.ActorOperator, leadstore.ErrUnknownActor},
	} {
		if _, err := q.SetCampaignMailbox(ctx, c.campaign, c.mb, true, c.actor); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if err := q.SetMailboxState(ctx, b.ID, leadstore.MailboxRetired, "gone", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, b.ID, true, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrMailboxRetired) {
		t.Errorf("assign retired: %v, want ErrMailboxRetired", err)
	}
	if n := rows(); n != 4 {
		t.Fatalf("refusals wrote rows: %d, want 4", n)
	}
	// A retired mailbox can still be unassigned.
	set(cafes, b.ID, false, true)
}
