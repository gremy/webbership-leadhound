package claude

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// limitLog collects what OnLimit is told.
type limitLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *limitLog) note(_ context.Context, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
}

func (l *limitLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestCappedOnLimit: a refusal by the purpose's daily cap or the total cap tells OnLimit once,
// in one plain line; a normal call, a lead cap and a zero cap tell it nothing; the API's own
// spend-limit and credit answers are told too; a nil OnLimit is safe.
func TestCappedOnLimit(t *testing.T) {
	r := writeReq()
	r.LeadID = 7
	worst, err := WorstWrite(r)
	if err != nil {
		t.Fatal(err)
	}
	used := Usage{Model: ModelSonnet, InputTokens: 300, OutputTokens: 200}
	daily := int64(5_000_000)
	spent := []UsageRow{{AccountID: 1, Purpose: PurposeDrafting, Day: testDay, CostMicros: daily - worst + 1}}

	cases := []struct {
		name  string
		st    *memStore
		f     *Fake
		set   func(*Capped)
		want  string // "" = OnLimit not told
		isCap bool
	}{
		{name: "normal call", st: &memStore{}, f: fakeAt(used)},
		{name: "purpose cap", st: &memStore{rows: spent}, f: fakeAt(used), isCap: true,
			want: fmt.Sprintf("Daily cap reached on drafting: %s spent of $5.00 today", dollars(daily-worst+1))},
		{name: "total cap", st: &memStore{rows: []UsageRow{{AccountID: 1, Purpose: PurposeResearch, Day: testDay, CostMicros: 1_000_000}}},
			f: fakeAt(used), set: func(c *Capped) { c.TotalCapMicros = 1_000_000 }, isCap: true,
			want: "Daily cap across all Claude work reached: $1.00 spent of $1.00 today"},
		{name: "lead cap", st: &memStore{}, f: fakeAt(used), set: func(c *Capped) { c.LeadCapMicros = worst - 1 }, isCap: true},
		{name: "zero cap", st: &memStore{}, f: fakeAt(used), set: func(c *Capped) { c.DailyCapMicros = 0 }, isCap: true},
		{name: "API spend limit", st: &memStore{}, f: &Fake{Err: fmt.Errorf("claude: write: status 400: x: %w: %w", ErrSpendLimit, ErrBadRequest)},
			want: "Anthropic's spend limit is reached: Claude calls are refused until it is raised or resets"},
		{name: "API credit low", st: &memStore{}, f: &Fake{Err: fmt.Errorf("claude: write: status 400: x: %w: %w", ErrCreditLow, ErrBadRequest)},
			want: "Anthropic's credit balance is too low: Claude calls are refused until it is topped up"},
		{name: "other API error", st: &memStore{}, f: &Fake{Err: fmt.Errorf("claude: write: status 400: x: %w", ErrBadRequest)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &limitLog{}
			c := capped(tc.f, tc.st, daily, daily)
			c.OnLimit = log.note
			if tc.set != nil {
				tc.set(c)
			}
			_, _, err := c.Write(context.Background(), r)
			if tc.isCap != errors.Is(err, ai.ErrSpendCap) {
				t.Fatalf("err %v, cap refusal want %t", err, tc.isCap)
			}
			got := log.got()
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("OnLimit told %q, want nothing", got)
			case tc.want != "" && (len(got) != 1 || got[0] != tc.want):
				t.Errorf("OnLimit told %q, want once %q", got, tc.want)
			}
			for _, l := range got {
				if strings.Contains(l, "\n") {
					t.Errorf("line %q spans lines", l)
				}
			}
			// The same call with no OnLimit is safe and ends the same way.
			c.OnLimit = nil
			if _, _, err2 := c.Write(context.Background(), r); (err == nil) != (err2 == nil) {
				t.Errorf("nil OnLimit: err %v, first err %v", err2, err)
			}
		})
	}
}

// TestAccountLimitMapping: a 4xx whose body names the spend limit or a low credit balance
// carries ErrSpendLimit or ErrCreditLow alongside ErrBadRequest; another 400 carries neither.
func TestAccountLimitMapping(t *testing.T) {
	cases := []struct {
		body string
		want error
	}{
		{`{"type":"error","error":{"type":"invalid_request_error","message":"enforced_spend_limit_reached"}}`, ErrSpendLimit},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"You have reached your specified API usage limits."}}`, ErrSpendLimit},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the API."}}`, ErrCreditLow},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`, nil},
	}
	for _, tc := range cases {
		srv, _ := server(t, func(int) (int, string) { return 400, tc.body })
		_, _, err := newClient(t, srv.URL).Write(context.Background(), writeReq())
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", tc.body, err)
		}
		for _, s := range []error{ErrSpendLimit, ErrCreditLow} {
			if errors.Is(err, s) != (s == tc.want) {
				t.Errorf("%s: err %v, carries %v = %t", tc.body, err, s, s != tc.want)
			}
		}
	}
}
