package main

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/verify"
)

// TestReoonDailyCap: unset is the default, a whole number of 0 or more is taken, anything
// else is refused in words naming the setting.
func TestReoonDailyCap(t *testing.T) {
	for v, want := range map[string]int{"": defaultReoonDailyCap, " 50 ": 50, "0": 0} {
		if got, err := reoonDailyCap(envOf(map[string]string{reoonDailyCapEnv: v})); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"-1", "lots", "1.5"} {
		if _, err := reoonDailyCap(envOf(map[string]string{reoonDailyCapEnv: v})); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// TestEmailGateLeavesOtherRoutes: a form lead, a lead with no route and an email lead with
// no address go through the gate as they are, without reading the store.
func TestEmailGateLeavesOtherRoutes(t *testing.T) {
	g := &emailGate{on: true}
	for _, c := range []leadstore.Candidate{
		{Channel: leadconfig.ChannelForm, RouteKind: leadstore.RouteForm, Route: "https://x.example/contact"},
		{Channel: leadconfig.ChannelNone},
		{Channel: leadconfig.ChannelEmail, RouteKind: leadstore.RouteEmail},
	} {
		got, res, err := g.route(context.Background(), leadstore.Queries{}, leadconfig.ChannelEmail, c)
		if err != nil || res.kind != gateSend || res.fell || !reflect.DeepEqual(got, c) {
			t.Errorf("%+v: %+v %+v %v", c, got, res, err)
		}
	}
}

// TestEmailCheckWords: the batch's waiting line, a day's counts, the lead page's line and the
// Controls row's lines.
func TestEmailCheckWords(t *testing.T) {
	if emailWaitingLine(0) != "" || emailWaitingLine(1) != "1 lead waits for its email check" || emailWaitingLine(3) != "3 leads wait for their email check" {
		t.Error("waiting line")
	}
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	rows := []leadstore.EmailCheck{{Status: verify.StatusSafe, CheckedAt: now}, {Status: verify.StatusCatchAll, CheckedAt: now},
		{Status: verify.StatusSpamtrap, CheckedAt: now}, {Status: verify.StatusUnknown, CheckedAt: now}}
	if got := countEmailChecks(rows, leadconfig.CatchAllSend); got != (emailCheckToday{4, 2, 1}) {
		t.Errorf("counts (send) %+v", got)
	}
	if got := countEmailChecks(rows, leadconfig.CatchAllBlock); got != (emailCheckToday{4, 1, 2}) {
		t.Errorf("counts (block) %+v", got)
	}

	for _, tc := range []struct {
		hist []leadstore.EmailCheck
		want string
	}{
		{nil, "Email check: not checked yet"},
		{[]leadstore.EmailCheck{{Status: verify.StatusRole, CheckedAt: now}}, "Email check: role address, may be sent"},
		{[]leadstore.EmailCheck{{Status: verify.StatusInboxFull, CheckedAt: now}}, "Email check: inbox full, not sent"},
		{[]leadstore.EmailCheck{{Status: verify.StatusUnknown, CheckedAt: now}}, "Email check: unknown, checked again the next day"},
		{[]leadstore.EmailCheck{{Status: verify.StatusUnknown, CheckedAt: now}, {Status: verify.StatusUnknown, CheckedAt: day.Add(-time.Hour)}},
			"Email check: unknown, not sent (unknown twice in a row)"},
		{[]leadstore.EmailCheck{{Status: verify.StatusSafe, CheckedAt: now.Add(-verify.FreshFor - time.Hour)}}, "Email check: safe, too old: checked again before sending"},
	} {
		got, at := emailCheckLine(tc.hist, leadconfig.CatchAllSend, now, day)
		if got != tc.want || (at == nil) != (len(tc.hist) == 0) {
			t.Errorf("%+v: %q %v, want %q", tc.hist, got, at, tc.want)
		}
	}

	finished := now.Add(time.Minute)
	run := func(note string) *store.Run { return &store.Run{StartedAt: now, FinishedAt: &finished, Note: note} }
	for _, tc := range []struct {
		on    bool
		today emailCheckToday
		last  *store.Run
		want  []string
	}{
		{true, emailCheckToday{}, nil, []string{"No address checked today."}},
		{false, emailCheckToday{}, nil, []string{emailCheckOffLine, "No address checked today."}},
		{true, emailCheckToday{3, 2, 1}, run("checked 3 · sendable 2 · blocked 1 · waiting 0 · credits left 17 today, 0 one-off"),
			[]string{"Today: 3 addresses checked, 2 may be sent, 1 blocked.", "Credits left at the last run (Tue 27 Oct 09:00): 17 today, 0 one-off."}},
		{true, emailCheckToday{1, 1, 0}, run(noCreditsLine), []string{"Today: 1 address checked, 1 may be sent, 0 blocked.", "Reoon had no credits left at the last run (Tue 27 Oct 09:00)."}},
		{true, emailCheckToday{}, run(noReoonKeyNote), []string{"No address checked today.",
			"The last run (Tue 27 Oct 09:00) checked nothing: the Reoon key is not on the server yet. It goes in /etc/leadhound/reoon.key."}},
	} {
		if got := emailCheckLines(tc.on, tc.today, tc.last, time.UTC); !slices.Equal(got, tc.want) {
			t.Errorf("lines %q\nwant %q", got, tc.want)
		}
	}
	if v := runView(*run(noReoonKeyNote), now); v.Result != "No Reoon key" || !v.Failed {
		t.Errorf("run view %+v", v)
	}
}
