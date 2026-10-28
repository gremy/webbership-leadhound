package claude

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/config"
)

// hangingClient talks to a server that answers reply(n) for n < hangFrom and never answers
// from request hangFrom on, with a short timeout and no retries, so a request that was sent
// gets no answer.
func hangingClient(t *testing.T, hangFrom int, reply func(n int) (int, string)) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if n >= hangFrom {
			select {
			case <-r.Context().Done():
			case <-done:
			}
			return
		}
		status, body := reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	c, err := New(config.Secret(testKey), WithBaseURL(srv.URL), WithTimeout(200*time.Millisecond), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

// A request that was sent but got no answer (the client timed out) may still have been
// billed: Capped records it at the request's worst case, marked provisional (no tokens, a
// cost), and the next call's cap check counts it.
func TestCappedNoAnswerRecordsWorstCase(t *testing.T) {
	r := writeReq()
	r.LeadID = 7
	worst, err := WorstWrite(r)
	if err != nil {
		t.Fatal(err)
	}
	c, calls := hangingClient(t, 1, nil)
	st := &memStore{}
	cp := &Capped{Client: c, Store: st, AccountID: 1, Purpose: PurposeDrafting,
		DailyCapMicros: 2*worst - 1, LeadCapMicros: 10 * worst, Loc: testLoc, Now: testNow}

	_, u, err := cp.Write(context.Background(), r)
	if !errors.Is(err, ErrNoAnswer) || !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("err %v, want ErrNoAnswer and ai.ErrUnavailable", err)
	}
	if u.billed() {
		t.Errorf("usage %+v, want none reported", u)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows %d, want 1 provisional row", len(st.rows))
	}
	got := st.rows[0]
	if !got.Provisional() || got.CostMicros != worst || got.Model != ModelSonnet || got.LeadID != 7 ||
		got.Purpose != PurposeDrafting || !got.Day.Equal(testDay) {
		t.Errorf("row %+v, want provisional at worst %d", got, worst)
	}

	// The next call sees the provisional spend: worst already counted + worst > the cap.
	_, _, err = cp.Write(context.Background(), r)
	if !errors.Is(err, ai.ErrSpendCap) {
		t.Fatalf("second call: err %v, want ErrSpendCap from the provisional spend", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server calls %d, want 1", calls.Load())
	}
}

// A 4xx the API rejected is not billed: nothing is recorded.
func TestCappedBadRequestRecordsNothing(t *testing.T) {
	srv, _ := server(t, func(int) (int, string) {
		return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`
	})
	r := writeReq()
	worst, _ := WorstWrite(r)
	st := &memStore{}
	cp := &Capped{Client: newClient(t, srv.URL), Store: st, AccountID: 1, Purpose: PurposeDrafting,
		DailyCapMicros: worst, LeadCapMicros: worst, Loc: testLoc, Now: testNow}
	_, _, err := cp.Write(context.Background(), r)
	if !errors.Is(err, ErrBadRequest) || errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err %v, want ErrBadRequest only", err)
	}
	if len(st.rows) != 0 {
		t.Fatalf("rows %+v, want none", st.rows)
	}
}

// A 429/5xx answer after retries is an answer: unbilled, not recorded, not ErrNoAnswer.
func TestCappedOverloadedRecordsNothing(t *testing.T) {
	srv, _ := server(t, func(int) (int, string) {
		return 529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`
	})
	r := writeReq()
	worst, _ := WorstWrite(r)
	st := &memStore{}
	c, err := New(config.Secret(testKey), WithBaseURL(srv.URL), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	cp := &Capped{Client: c, Store: st, AccountID: 1, Purpose: PurposeDrafting,
		DailyCapMicros: worst, LeadCapMicros: worst, Loc: testLoc, Now: testNow}
	_, _, err = cp.Write(context.Background(), r)
	if !errors.Is(err, ai.ErrUnavailable) || errors.Is(err, ErrNoAnswer) || len(st.rows) != 0 {
		t.Fatalf("err %v rows %+v, want ErrUnavailable, no ErrNoAnswer, no rows", err, st.rows)
	}
}

// A context already done before the call never sends a request: nothing is recorded.
func TestCappedDoneContextRecordsNothing(t *testing.T) {
	c, calls := hangingClient(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := c.Write(ctx, writeReq())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoAnswer) || calls.Load() != 0 {
		t.Fatalf("err %v calls %d, want context.Canceled before sending", err, calls.Load())
	}
}

// A research continuation sent with no answer: the billed first leg is recorded as billed,
// the lost leg provisionally at its own worst case.
func TestCappedResearchLostLegRecordsWorstCase(t *testing.T) {
	c, _ := hangingClient(t, 2, func(int) (int, string) { return 200, message("pause_turn", "", 1000, 100, 1) })
	r := researchReq()
	r.LeadID = 9
	w1, _ := WorstResearchLeg(r, 1)
	st := &memStore{}
	cp := &Capped{Client: c, Store: st, AccountID: 1, Purpose: PurposeResearch,
		DailyCapMicros: 10 * w1, LeadCapMicros: 10 * w1, Loc: testLoc, Now: testNow}
	_, u, err := cp.Research(context.Background(), r)
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err %v, want ErrNoAnswer", err)
	}
	if u.InputTokens != 1000 {
		t.Errorf("usage %+v, want leg 0's", u)
	}
	leg0, _ := Cost(Usage{Model: ModelHaiku, InputTokens: 1000, OutputTokens: 100, WebSearches: 1})
	if len(st.rows) != 2 || st.rows[0].Provisional() || st.rows[0].CostMicros != leg0 ||
		!st.rows[1].Provisional() || st.rows[1].CostMicros != w1 || st.rows[1].LeadID != 9 {
		t.Fatalf("rows %+v, want leg 0 billed (%d) then leg 1 provisional (%d)", st.rows, leg0, w1)
	}
}

// Each leg is its own request and re-sends the whole turn so far as input, so a continuation
// reserves that leg's full worst case on top of what earlier legs actually cost: allowed at
// exactly the cap, refused one micro-dollar under it.
func TestCappedResearchLegArithmetic(t *testing.T) {
	r := researchReq()
	leg0Used := Usage{Model: ModelHaiku, InputTokens: 1000, OutputTokens: 100, WebSearches: 1}
	leg0, _ := Cost(leg0Used)
	w1, _ := WorstResearchLeg(r, 1)
	const prior = 5000 // already spent today on research
	reply := func(n int) (int, string) {
		if n == 1 {
			return 200, message("pause_turn", "", 1000, 100, 1)
		}
		return 200, message("end_turn", "done", 10, 1, 0)
	}
	for _, tc := range []struct {
		name string
		cap  int64
		ok   bool
	}{
		{"at the cap", prior + leg0 + w1, true},
		{"one under", prior + leg0 + w1 - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := server(t, reply)
			st := &memStore{rows: []UsageRow{{AccountID: 1, Purpose: PurposeResearch, Day: testDay, CostMicros: prior}}}
			cp := &Capped{Client: newClient(t, srv.URL), Store: st, AccountID: 1, Purpose: PurposeResearch,
				DailyCapMicros: tc.cap, LeadCapMicros: 1, Loc: testLoc, Now: testNow}
			_, _, err := cp.Research(context.Background(), r)
			if tc.ok {
				if err != nil || got.calls.Load() != 2 {
					t.Fatalf("err %v calls %d, want the continuation sent", err, got.calls.Load())
				}
				return
			}
			if err == nil || got.calls.Load() != 1 {
				t.Fatalf("err %v calls %d, want the continuation refused", err, got.calls.Load())
			}
		})
	}
}
