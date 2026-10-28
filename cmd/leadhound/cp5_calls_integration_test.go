//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// cp5World is an approved batch from the example campaign (GB calls on, IE calls off) with a
// GB lead (tailspin.example) and an IE lead (northwind.example), each with a call step; the
// call of the GB lead is due now, its earlier steps out of the way.
type cp5World struct {
	dir                string
	now                time.Time
	gbCall, ieCall     int64
	gbLead, gbOrg      int64
	gbFact             int64
	gbNumber, ieNumber string
	b                  *backend
}

const (
	cp5GBNumber = "+441632960002"
	cp5IENumber = "+353215550100"
)

func newCP5World(t *testing.T) *cp5World {
	t.Helper()
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	w := &cp5World{dir: dir, now: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC), gbNumber: cp5GBNumber, ieNumber: cp5IENumber} // a Tuesday
	saved := leadsNow
	leadsNow = func() time.Time { return w.now }
	t.Cleanup(func() { leadsNow = saved })

	cfgPath := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	ie := strings.Replace(string(raw), "  DE: {channel: form,", "  IE: {channel: email, lang: en, tz: Europe/Dublin, footer: footer-en}\n  DE: {channel: form,", 1)
	if err := os.WriteFile(cfgPath, []byte(ie), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(dir, campaign.Dir), os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}
	batchFixture(t)
	owner := storetest.OwnerConn(t)
	if _, err := owner.Exec(ctx, `UPDATE org SET country = 'IE' WHERE domain = 'northwind.example'`); err != nil {
		t.Fatal(err)
	}
	orgPhone(t, "northwind.example", w.ieNumber)
	orgPhone(t, "tailspin.example", w.gbNumber)
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	mustRun(t, "leads", "batch", "new")
	mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))
	// The batch was approved by the database's clock; the test's clock is earlier.
	if _, err := owner.Exec(ctx, `UPDATE outreach_batch SET approved_at = $1`, w.now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	callOf := func(domain string) (call, lead, org int64) {
		t.Helper()
		if err := owner.QueryRow(ctx,
			`SELECT m.id, l.id, o.id FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
			 WHERE o.domain = $1 AND m.channel = 'phone' ORDER BY m.step LIMIT 1`, domain).Scan(&call, &lead, &org); err != nil {
			t.Fatalf("%s call: %v", domain, err)
		}
		return
	}
	w.gbCall, w.gbLead, w.gbOrg = callOf("tailspin.example")
	w.ieCall, _, _ = callOf("northwind.example")
	if err := owner.QueryRow(ctx, `SELECT phone_fact_id FROM outreach_message WHERE id = $1`, w.gbCall).Scan(&w.gbFact); err != nil {
		t.Fatal(err)
	}
	// The earlier steps are done with; the call is due now.
	if _, err := owner.Exec(ctx,
		`UPDATE outreach_message m SET state = 'cancelled', cancel_reason = 'condition'
		 FROM outreach_message c WHERE c.id = $1 AND m.lead_id = c.lead_id AND m.batch_id = c.batch_id AND m.step < c.step`, w.gbCall); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE outreach_message SET due_at = $2 WHERE id = $1`, w.gbCall, w.now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	w.b = testBackend(t)
	w.b.cfg.DataDir = dir
	w.b.now = func() time.Time { return w.now }
	return w
}

// screen records a screening of number against register at at, with no suppression (the
// screening rule alone).
func (w *cp5World) screen(t *testing.T, number, register, result string, at time.Time) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`INSERT INTO phone_screening (number, register, result, checked_at, actor) VALUES ($1, $2, $3, $4, 'operator')`,
		number, register, result, at); err != nil {
		t.Fatal(err)
	}
}

// reset puts the GB call's org, number and screenings back as the fixture made them.
func (w *cp5World) reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	for _, q := range []string{
		`DELETE FROM phone_screening`,
		`DELETE FROM phone_status`,
		`DELETE FROM suppression WHERE kind = 'phone' AND source = 'cp5-sweep'`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Exec(ctx, `UPDATE org SET country = 'GB' WHERE id = $1`, w.gbOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE org_fact SET value = $2, normalized = $2 WHERE id = $1`, w.gbFact, w.gbNumber); err != nil {
		t.Fatal(err)
	}
	w.now = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
}

// surfaces is whether the GB call shows as callable on each surface: the card, Today's
// "Calls to make now", and `leads calls list --due`; with the card's blocked reason and
// warning, and the list line ("" when the call is not listed at all, as once a suppression
// has cancelled it).
type surfaces struct {
	card, today, list bool
	reason, warn      string
	line              string
	todayLine         string
}

func (w *cp5World) surfaces(t *testing.T) surfaces {
	t.Helper()
	ctx := context.Background()
	var s surfaces
	card, err := w.b.LeadCall(ctx, w.gbCall)
	if err != nil {
		t.Fatal(err)
	}
	s.card, s.reason, s.warn = card.Callable, card.BlockedReason, card.CallsOff
	today, err := w.b.LeadsToday(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range today.CallsNow {
		if it.MessageID == w.gbCall {
			s.today, s.todayLine = true, it.Line
		}
	}
	for _, it := range append(today.CallsDue, today.CallsOverdue...) {
		if it.MessageID == w.gbCall {
			s.todayLine = it.Line
		}
	}
	listOut := mustRun(t, "leads", "calls", "list", "--due")
	for _, l := range strings.Split(listOut, "\n") {
		if strings.HasPrefix(l, fmt.Sprintf("message %d ", w.gbCall)) {
			s.line = l
			s.list = strings.Contains(l, "· callable now")
		}
	}
	return s
}

// TestCallableInvariantSweep: across the rules a call must pass, a call failing any one of
// them is callable nowhere: not on its card, not under Today's "Calls to make now", not in
// calls list. Each case changes one thing about the same due GB call: the country off by law
// (no override), a do-not-call suppression, a number marked wrong, unscreened, screened 29
// days ago, listed, outside the calling hours, a US lead with no region, an Irish mobile. A
// country whose calls the campaign's own rules have off is callable with the warning on every
// surface (warn and choose); a GB call screened clean yesterday in hours is callable
// everywhere, so the sweep is not vacuous.
func TestCallableInvariantSweep(t *testing.T) {
	ctx := context.Background()
	w := newCP5World(t)
	owner := storetest.OwnerConn(t)
	day := 24 * time.Hour
	clean := func(t *testing.T, ago time.Duration) {
		w.screen(t, w.gbNumber, "tps", "clean", w.now.Add(-ago))
		w.screen(t, w.gbNumber, "ctps", "clean", w.now.Add(-ago))
	}
	cases := []struct {
		name     string
		set      func(t *testing.T)
		callable bool
		reason   string // in the card's blocked reason and the lines
		warn     bool
	}{
		{"screened clean yesterday, in hours", func(t *testing.T) { clean(t, day) }, true, "", false},
		{"country off by law (AT)", func(t *testing.T) {
			clean(t, day)
			if _, err := owner.Exec(ctx, `UPDATE org SET country = 'AT' WHERE id = $1`, w.gbOrg); err != nil {
				t.Fatal(err)
			}
		}, false, "off by law", false},
		{"suppressed", func(t *testing.T) {
			clean(t, day)
			if _, err := owner.Exec(ctx, `INSERT INTO suppression (account_id, kind, value, reason, source) VALUES ($1, 'phone', $2, 'do_not_call', 'cp5-sweep')`,
				leadstore.DefaultAccountID, leadstore.PhoneSuppressionValue(w.gbNumber)); err != nil {
				t.Fatal(err)
			}
		}, false, "do-not-call list", false},
		{"number marked wrong", func(t *testing.T) {
			clean(t, day)
			if _, err := leadstoretest.Q(t).RecordPhoneStatus(ctx, w.gbFact, leadstore.PhoneBad, leadstore.PhoneReasonOperator, nil, leadstore.ActorGremy); err != nil {
				t.Fatal(err)
			}
		}, false, "marked wrong", false},
		{"GB unscreened", func(*testing.T) {}, false, "Not screened against", false},
		{"GB screened 29 days ago", func(t *testing.T) { clean(t, 29*day) }, false, "older than 28 days", false},
		{"listed on TPS", func(t *testing.T) {
			clean(t, 2*day)
			w.screen(t, w.gbNumber, "tps", "listed", w.now.Add(-day))
		}, false, "Listed on", false},
		{"outside the calling hours", func(t *testing.T) {
			clean(t, day)
			w.now = time.Date(2026, 10, 27, 21, 30, 0, 0, time.UTC)
		}, false, "Outside the calling hours", false},
		{"US, region unknown", func(t *testing.T) {
			if _, err := owner.Exec(ctx, `UPDATE org SET country = 'US' WHERE id = $1`, w.gbOrg); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Exec(ctx, `UPDATE org_fact SET value = '+12125550100', normalized = '+12125550100' WHERE id = $1`, w.gbFact); err != nil {
				t.Fatal(err)
			}
		}, false, "region unknown", false},
		{"IE mobile", func(t *testing.T) {
			if _, err := owner.Exec(ctx, `UPDATE org SET country = 'IE' WHERE id = $1`, w.gbOrg); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Exec(ctx, `UPDATE org_fact SET value = '+353871234567', normalized = '+353871234567' WHERE id = $1`, w.gbFact); err != nil {
				t.Fatal(err)
			}
			w.screen(t, "+353871234567", "ndd", "clean", w.now.Add(-time.Hour))
		}, false, "landlines only", false},
		{"IE landline, calls off in the campaign: warn and choose", func(t *testing.T) {
			if _, err := owner.Exec(ctx, `UPDATE org SET country = 'IE' WHERE id = $1`, w.gbOrg); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Exec(ctx, `UPDATE org_fact SET value = '+353215550101', normalized = '+353215550101' WHERE id = $1`, w.gbFact); err != nil {
				t.Fatal(err)
			}
			w.screen(t, "+353215550101", "ndd", "clean", w.now.Add(-time.Hour))
		}, true, "", true},
		{"IE landline screened before the batch was approved", func(t *testing.T) {
			if _, err := owner.Exec(ctx, `UPDATE org SET country = 'IE' WHERE id = $1`, w.gbOrg); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Exec(ctx, `UPDATE org_fact SET value = '+353215550101', normalized = '+353215550101' WHERE id = $1`, w.gbFact); err != nil {
				t.Fatal(err)
			}
			w.screen(t, "+353215550101", "ndd", "clean", w.now.Add(-3*time.Hour))
		}, false, "before this campaign started", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w.reset(t)
			c.set(t)
			s := w.surfaces(t)
			if s.card != c.callable || s.today != c.callable || s.list != c.callable {
				t.Fatalf("callable: card %v, Today %v, list %v; want all %v\n card reason %q\n today %q\n list %q",
					s.card, s.today, s.list, c.callable, s.reason, s.todayLine, s.line)
			}
			if !c.callable {
				// the card states the reason as a sentence; after "not callable:" it reads lower-case
				after := console.AfterColon(c.reason)
				if !strings.Contains(s.reason, c.reason) || !strings.Contains(s.todayLine, after) || !strings.Contains(s.line, after) {
					t.Errorf("reason %q missing: card %q, today %q, list %q", c.reason, s.reason, s.todayLine, s.line)
				}
			} else if s.reason != "" {
				t.Errorf("a callable card carries a blocked reason %q", s.reason)
			}
			if got := s.warn != ""; got != c.warn {
				t.Errorf("campaign warning on the card = %q, want shown %v", s.warn, c.warn)
			}
			if c.warn && c.callable && (!strings.Contains(s.todayLine, "your call") || !strings.Contains(s.line, "your call")) {
				t.Errorf("the warning is not on every surface: today %q, list %q", s.todayLine, s.line)
			}
		})
	}
	w.reset(t)
}

// TestCallsBadNumberStopsOtherCalls: a number marked wrong on one call stops every other
// pending call on the same phone fact, and a later batch never drafts it again.
func TestCallsBadNumberStopsOtherCalls(t *testing.T) {
	ctx := context.Background()
	w := newCP5World(t)
	w.screen(t, w.gbNumber, "tps", "clean", w.now.Add(-time.Hour))
	w.screen(t, w.gbNumber, "ctps", "clean", w.now.Add(-time.Hour))
	if s := w.surfaces(t); !s.card {
		t.Fatalf("baseline not callable: %+v", s)
	}
	if _, err := leadstoretest.Q(t).RecordPhoneStatus(ctx, w.gbFact, leadstore.PhoneBad, leadstore.PhoneReasonOperator, nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if s := w.surfaces(t); s.card || s.today || s.list {
		t.Errorf("a wrong number is still callable: %+v", s)
	}
	facts, err := leadstoretest.Q(t).UsableOrgFacts(ctx, []int64{w.gbOrg})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, why := pickPhone(facts, nil); ok || why != callNoNumber {
		t.Errorf("the batch would draft the wrong number again (ok %v, %q)", ok, why)
	}
}
