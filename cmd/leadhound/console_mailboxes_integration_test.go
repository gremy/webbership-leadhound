//go:build integration

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// TestConsoleLeadsSendingFromMailboxes: the Leads page's sending summary through the real
// backend is read from the stored mailboxes: with zero, one and two active mailboxes, today's
// cap is the caps of the mailboxes ready for new leads summed (each domain at most
// caps.domain_per_day), and the records show as passed only when every active domain and
// selector passed today.
func TestConsoleLeadsSendingFromMailboxes(t *testing.T) {
	ctx := context.Background()
	// scene is leadsScene (mailbox 1, testUser on sel1, passed today) live at perDay a day.
	scene := func(t *testing.T, perDay string) (*backend, leadstore.Queries) {
		t.Helper()
		dir := leadsScene(t)
		setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: "+perDay+"}]")
		t.Setenv("LEADHOUND_OUTREACH", "live")
		return leadsBackend(t), leadstoretest.Q(t)
	}
	sending := func(t *testing.T, b *backend) console.Sending {
		t.Helper()
		p, err := b.Leads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return p.Sending
	}
	passed := func(t *testing.T, s console.Sending) {
		t.Helper()
		if d := s.DNS; d == nil || !d.SPF || !d.DKIM || !d.DMARC || !d.At.Equal(leadsTuesday) {
			t.Errorf("records %+v, want passed at %s", d, leadsTuesday)
		}
	}

	t.Run("zero", func(t *testing.T) {
		b, lq := scene(t, "10")
		if err := lq.SetMailboxState(ctx, 1, leadstore.MailboxRetired, "gone", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
		s := sending(t, b)
		if !s.SetUp || s.CapToday != 0 || s.DNS != nil {
			t.Errorf("no active mailbox: set up %v, cap %d, records %+v; want true, 0, none", s.SetUp, s.CapToday, s.DNS)
		}
	})

	t.Run("one", func(t *testing.T) {
		b, _ := scene(t, "10")
		s := sending(t, b)
		if !s.SetUp || s.CapToday != 10 {
			t.Errorf("one mailbox: set up %v, cap %d; want true, 10", s.SetUp, s.CapToday)
		}
		passed(t, s)
	})

	t.Run("shadow", func(t *testing.T) {
		b, _ := scene(t, "10")
		t.Setenv("LEADHOUND_OUTREACH", "")
		if s := sending(t, b); s.SetUp {
			t.Error("shadow outreach shows sending set up")
		}
	})

	t.Run("two", func(t *testing.T) {
		b, lq := scene(t, "10")
		desk := seedMailbox(t, lq, "desk@other.example", withSelector("s2"), withDNSPass())
		s := sending(t, b)
		if s.CapToday != 20 {
			t.Errorf("two ready mailboxes: cap %d, want 20", s.CapToday)
		}
		passed(t, s)
		h, c := jobsHandler(t, b)
		r := httptest.NewRequest(http.MethodGet, "/leads/outreach", nil)
		r.Header.Set("Cookie", c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if page := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(page, "<dd>0 of 20</dd>") || !strings.Contains(page, "Sending is on.") ||
			!strings.Contains(page, "SPF ok · DKIM ok · DMARC ok") {
			t.Errorf("leads page %d:\n%s", w.Code, page)
		}

		// A failed check today for the second domain: it is no longer ready, and the
		// records show what is missing.
		if err := lq.RecordSenderCheckSelector(ctx, leadstore.SenderDomainSHA256(desk.Domain), desk.DKIMSelector,
			mailbox.DNSResult{SPF: true, DMARC: true}, leadsNow().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		s = sending(t, b)
		if s.CapToday != 20 {
			t.Errorf("a pass then a fail today still counts as passed today: cap %d, want 20", s.CapToday)
		}
		if d := s.DNS; d == nil || d.DKIM || !d.SPF || !d.DMARC {
			t.Errorf("records %+v, want today's failed check (DKIM missing)", d)
		}
	})

	t.Run("two, one not checked today", func(t *testing.T) {
		b, lq := scene(t, "10")
		seedMailbox(t, lq, "desk@other.example", withSelector("s2"))
		s := sending(t, b)
		if s.CapToday != 10 || s.DNS != nil {
			t.Errorf("one ready, one unchecked today: cap %d, records %+v; want 10, none", s.CapToday, s.DNS)
		}
	})

	t.Run("two, one paused", func(t *testing.T) {
		b, lq := scene(t, "10")
		desk := seedMailbox(t, lq, "desk@other.example", withSelector("s2"))
		if err := lq.SetMailboxState(ctx, desk.ID, leadstore.MailboxPaused, "by hand", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
		s := sending(t, b)
		if s.CapToday != 10 {
			t.Errorf("cap %d, want 10", s.CapToday)
		}
		passed(t, s)
	})

	t.Run("two on one domain", func(t *testing.T) {
		b, lq := scene(t, "30")
		seedMailbox(t, lq, "desk@"+testSenderDomain, withDNSPass())
		if s := sending(t, b); s.CapToday != 50 {
			t.Errorf("two mailboxes at 30 on one domain: cap %d, want caps.domain_per_day 50", s.CapToday)
		}
	})
}
