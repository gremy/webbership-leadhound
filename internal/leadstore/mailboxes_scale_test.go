//go:build scale

package leadstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
)

// TestScaleMailboxSeed: the v4 scale seed holds ten ready mailboxes over five domains, and
// every seeded message, mailbox event and cursor names a mailbox (what the later NOT NULL
// needs); the per-mailbox reads stay within budget on a year of outreach.
func TestScaleMailboxSeed(t *testing.T) {
	q, s, now, loc := scaleLeads(t)
	if info := leadstoretest.ScaleSeeded(t); info.Leads != "v6" {
		t.Fatalf("leads seed = %q, want v6", info.Leads)
	}
	ctx := context.Background()
	since := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)

	var mbs []leadstore.MailboxStatus
	within(t, "Mailboxes", 200*time.Millisecond, func() (err error) {
		mbs, err = q.Mailboxes(ctx, since)
		return err
	})
	if len(mbs) != 10 {
		t.Fatalf("mailboxes = %d, want 10", len(mbs))
	}
	perDomain := map[string]int{}
	for _, m := range mbs {
		perDomain[m.Domain]++
		if m.State != leadstore.MailboxActive || m.LastCheck == nil || m.LastCheck.Outcome != leadstore.CheckPassed || !m.DNSPassedToday {
			t.Errorf("mailbox %d is not ready: state %s, check %+v, dns today %v", m.ID, m.State, m.LastCheck, m.DNSPassedToday)
		}
		if m.FirstLiveSend == nil {
			t.Errorf("mailbox %d has no first live send in a year of outreach", m.ID)
		}
	}
	if len(perDomain) != 5 {
		t.Errorf("domains = %v, want 5 with two mailboxes each", perDomain)
	}
	for d, n := range perDomain {
		if n != 2 {
			t.Errorf("domain %s has %d mailboxes, want 2", d, n)
		}
	}
	for _, tbl := range []string{"outreach_message", "mailbox_event", "mailbox_cursor"} {
		var missing int
		if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM "+tbl+" WHERE mailbox_id IS NULL").Scan(&missing); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if missing != 0 {
			t.Errorf("%s: %d rows name no mailbox", tbl, missing)
		}
	}
	within(t, "SentSinceByMailbox", 200*time.Millisecond, func() error {
		_, err := q.SentSinceByMailbox(ctx, since)
		return err
	})
	within(t, "WaitingByMailbox", 200*time.Millisecond, func() error {
		_, err := q.WaitingByMailbox(ctx)
		return err
	})
	within(t, "FirstLiveSendByMailbox", 200*time.Millisecond, func() error {
		_, err := q.FirstLiveSendByMailbox(ctx)
		return err
	})
	within(t, "HardBouncesInLastSends", 200*time.Millisecond, func() error {
		_, err := q.HardBouncesInLastSends(ctx, mbs[0].ID, 50)
		return err
	})
}
