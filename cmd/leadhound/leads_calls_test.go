package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestCallsOffInCP3: until the call rules exist no call is callable anywhere: the card is
// blocked with the plain words, whatever its state, and the today view carries them over
// every call it lists.
func TestCallsOffInCP3(t *testing.T) {
	due := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	for _, c := range []leadstore.CallRow{
		{MessageID: 1, OrgDomain: "a.example", Phone: "+441632960001", DueAt: &due},
		{MessageID: 2, OrgDomain: "b.example", Shadow: true, DueAt: &due, Outcome: "no_answer"},
		{MessageID: 3, OrgDomain: "c.example"},
	} {
		card := callCard(c)
		if card.Callable || card.BlockedReason != callsOffWords || !strings.Contains(card.Screening, "calls are off") {
			t.Errorf("call %d card = callable %v, blocked %q, screening %q", c.MessageID, card.Callable, card.BlockedReason, card.Screening)
		}
	}
	if callsOffWords != "Calls are off until the call rules are set up" {
		t.Errorf("callsOffWords = %q", callsOffWords)
	}
	v := todayFromStore(leadstore.Today{CallsDueNow: []leadstore.CallRow{{MessageID: 1, OrgDomain: "a.example", DueAt: &due}}},
		leadconfig.Config{}, due, time.UTC, console.TodayView{CallsOff: callsOffWords}, nil)
	if v.CallsOff != callsOffWords || len(v.CallsNow) != 1 {
		t.Errorf("today = %+v", v)
	}
}

// TestTodayTimeOnce: a Today row shows its time once: an email's due time in its line (At
// nil), a call's in At only (its line has no clock), never both.
func TestTodayTimeOnce(t *testing.T) {
	due := time.Date(2026, 10, 28, 11, 30, 0, 0, time.UTC)
	v := todayFromStore(leadstore.Today{
		CallsDueNow:    []leadstore.CallRow{{MessageID: 1, OrgDomain: "a.example", Step: 2, DueAt: &due, Shadow: true}},
		CallsOverdue:   []leadstore.CallRow{{MessageID: 2, OrgDomain: "b.example", Step: 2, DueAt: &due}},
		EmailsDueCount: 1,
		EmailsDue:      []leadstore.TodayItem{{MessageID: 3, OrgDomain: "c.example", Channel: "email", Step: 0, DueAt: due, Shadow: true}},
	}, leadconfig.Config{}, due, time.UTC, console.TodayView{}, nil)
	for _, it := range append(append(v.CallsNow, v.CallsOverdue...), v.Emails...) {
		inLine := strings.Contains(it.Line, "11:30") || strings.Contains(it.Line, "due")
		if inLine && it.At != nil {
			t.Errorf("%s: the time shows twice: %q and At %v", it.OrgName, it.Line, it.At)
		}
		if !inLine && it.At == nil {
			t.Errorf("%s: no time at all: %q", it.OrgName, it.Line)
		}
	}
	if len(v.Emails) != 1 || !strings.Contains(v.Emails[0].Line, "Step 1 · email") {
		t.Errorf("email line: %+v", v.Emails)
	}
}
