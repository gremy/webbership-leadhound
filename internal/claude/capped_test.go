package claude

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// memStore is an in-memory UsageStore.
type memStore struct {
	mu      sync.Mutex
	rows    []UsageRow
	readErr error
	recErr  error
}

func (s *memStore) ClaudeSpendOn(_ context.Context, account int64, purpose string, day time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return 0, s.readErr
	}
	var sum int64
	for _, r := range s.rows {
		if r.AccountID == account && (purpose == "" || r.Purpose == purpose) && r.Day.Equal(day) {
			sum += r.CostMicros
		}
	}
	return sum, nil
}

func (s *memStore) ClaudeSpendForLead(_ context.Context, lead int64, purpose string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return 0, s.readErr
	}
	var sum int64
	for _, r := range s.rows {
		if r.LeadID == lead && r.Purpose == purpose {
			sum += r.CostMicros
		}
	}
	return sum, nil
}

func (s *memStore) RecordClaudeUsage(_ context.Context, r UsageRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recErr != nil {
		return s.recErr
	}
	s.rows = append(s.rows, r)
	return nil
}

func (s *memStore) total() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, r := range s.rows {
		sum += r.CostMicros
	}
	return sum
}

// lockedStore adds the cross-process lock a database store holds.
type lockedStore struct {
	*memStore
	lock sync.Mutex
}

func (l *lockedStore) WithClaudeUsageLock(_ context.Context, fn func(UsageStore) error) error {
	l.lock.Lock()
	defer l.lock.Unlock()
	return fn(l.memStore)
}

var (
	testLoc = time.FixedZone("test", 3600)
	testNow = func() time.Time { return time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC) }
	testDay = time.Date(2026, 10, 28, 0, 0, 0, 0, testLoc)
)

func capped(c Caller, st UsageStore, daily, lead int64) *Capped {
	return &Capped{Client: c, Store: st, AccountID: 1, Purpose: PurposeDrafting,
		DailyCapMicros: daily, LeadCapMicros: lead, Loc: testLoc, Now: testNow}
}

// worstUsage is a usage exactly at r's worst-case bound.
func worstUsage(r WriteRequest) Usage {
	b, _ := json.Marshal(r.Schema)
	return Usage{Model: r.Model, CacheWriteTokens: len(r.System) + len(r.Prompt) + len(b) + inputOverheadTokens, OutputTokens: r.MaxTokens}
}

func fakeAt(u Usage) *Fake {
	return &Fake{WriteAnswer: WriteResult{JSON: json.RawMessage(`{"subject":"hi"}`)}, Usage: u}
}

func TestCappedGrid(t *testing.T) {
	r := writeReq()
	r.LeadID = 7
	worst, err := WorstWrite(r)
	if err != nil {
		t.Fatal(err)
	}
	used := Usage{Model: ModelSonnet, InputTokens: 300, OutputTokens: 200}
	billedCost, _ := Cost(used)

	t.Run("under cap calls and records", func(t *testing.T) {
		st := &memStore{}
		f := fakeAt(used)
		res, u, err := capped(f, st, worst, worst).Write(context.Background(), r)
		if err != nil || string(res.JSON) != `{"subject":"hi"}` || u != used {
			t.Fatalf("res %s usage %+v err %v", res.JSON, u, err)
		}
		if len(st.rows) != 1 {
			t.Fatalf("rows %d, want 1", len(st.rows))
		}
		got := st.rows[0]
		want := UsageRow{AccountID: 1, LeadID: 7, Purpose: PurposeDrafting, Model: ModelSonnet, Day: testDay,
			InputTokens: 300, OutputTokens: 200, CostMicros: billedCost}
		if !got.Day.Equal(want.Day) || got.Day.Location() != testLoc {
			t.Errorf("day %v, want %v", got.Day, want.Day)
		}
		got.Day = want.Day
		if got != want {
			t.Errorf("row %+v, want %+v", got, want)
		}
	})
	t.Run("worst over purpose cap refuses without calling", func(t *testing.T) {
		st := &memStore{rows: []UsageRow{{AccountID: 1, Purpose: PurposeDrafting, Day: testDay, CostMicros: 2}}}
		f := fakeAt(used)
		_, _, err := capped(f, st, worst+1, worst*10).Write(context.Background(), r)
		if !errors.Is(err, ai.ErrSpendCap) || f.Calls() != 0 || len(st.rows) != 1 {
			t.Fatalf("err %v calls %d rows %d", err, f.Calls(), len(st.rows))
		}
	})
	t.Run("other purposes and days do not count", func(t *testing.T) {
		st := &memStore{rows: []UsageRow{
			{AccountID: 1, Purpose: PurposeResearch, Day: testDay, CostMicros: worst},
			{AccountID: 1, Purpose: PurposeDrafting, Day: testDay.AddDate(0, 0, -1), CostMicros: worst},
			{AccountID: 2, Purpose: PurposeDrafting, Day: testDay, CostMicros: worst},
		}}
		if _, _, err := capped(fakeAt(used), st, worst, worst*10).Write(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("total ceiling across purposes", func(t *testing.T) {
		st := &memStore{rows: []UsageRow{{AccountID: 1, Purpose: PurposeResearch, Day: testDay, CostMicros: 5}}}
		c := capped(fakeAt(used), st, worst*10, worst*10)
		c.TotalCapMicros = worst + 4
		if _, _, err := c.Write(context.Background(), r); !errors.Is(err, ai.ErrSpendCap) {
			t.Fatalf("err %v, want ErrSpendCap", err)
		}
	})
	t.Run("worst over lead cap refuses", func(t *testing.T) {
		st := &memStore{rows: []UsageRow{{AccountID: 1, LeadID: 7, Purpose: PurposeDrafting, Day: testDay.AddDate(0, 0, -3), CostMicros: 1}}}
		f := fakeAt(used)
		_, _, err := capped(f, st, worst*10, worst).Write(context.Background(), r)
		if !errors.Is(err, ai.ErrSpendCap) || f.Calls() != 0 {
			t.Fatalf("err %v calls %d", err, f.Calls())
		}
		r0 := r
		r0.LeadID = 0 // no lead: the lead cap does not apply
		if _, _, err := capped(f, st, worst*10, 0).Write(context.Background(), r0); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("zero daily cap refuses", func(t *testing.T) {
		f := fakeAt(used)
		if _, _, err := capped(f, &memStore{}, 0, worst).Write(context.Background(), r); !errors.Is(err, ai.ErrSpendCap) || f.Calls() != 0 {
			t.Fatalf("err %v calls %d", err, f.Calls())
		}
	})
	t.Run("store read failure stops", func(t *testing.T) {
		f := fakeAt(used)
		_, _, err := capped(f, &memStore{readErr: errors.New("db down")}, worst, worst).Write(context.Background(), r)
		if !errors.Is(err, ai.ErrUsageStore) || f.Calls() != 0 {
			t.Fatalf("err %v calls %d", err, f.Calls())
		}
	})
	t.Run("store record failure stops", func(t *testing.T) {
		_, _, err := capped(fakeAt(used), &memStore{recErr: errors.New("db down")}, worst, worst).Write(context.Background(), r)
		if !errors.Is(err, ai.ErrUsageStore) {
			t.Fatalf("err %v, want ErrUsageStore", err)
		}
	})
	t.Run("billed refusal is recorded", func(t *testing.T) {
		st := &memStore{}
		f := fakeAt(used)
		f.Err = ErrRefused
		_, _, err := capped(f, st, worst, worst).Write(context.Background(), r)
		if !errors.Is(err, ErrRefused) || len(st.rows) != 1 {
			t.Fatalf("err %v rows %d", err, len(st.rows))
		}
	})
	t.Run("unbilled failure is not recorded", func(t *testing.T) {
		st := &memStore{}
		f := &Fake{Err: ai.ErrUnavailable, Usage: Usage{Model: ModelSonnet}}
		_, _, err := capped(f, st, worst, worst).Write(context.Background(), r)
		if !errors.Is(err, ai.ErrUnavailable) || len(st.rows) != 0 {
			t.Fatalf("err %v rows %d", err, len(st.rows))
		}
	})
	t.Run("fake is never recorded", func(t *testing.T) {
		st := &memStore{}
		f := fakeAt(Usage{InputTokens: 1_000_000})
		rf := r
		rf.Model = FakeModel
		if _, _, err := capped(f, st, 1, 1).Write(context.Background(), rf); err != nil || len(st.rows) != 0 {
			t.Fatalf("err %v rows %d", err, len(st.rows))
		}
	})
	t.Run("unknown purpose refused", func(t *testing.T) {
		c := capped(fakeAt(used), &memStore{}, worst, worst)
		c.Purpose = "jobs"
		if _, _, err := c.Write(context.Background(), r); err == nil {
			t.Fatal("purpose jobs accepted")
		}
	})
}

// Two callers in separate processes (separate Capped values) at the edge of the cap: the
// lock lets only one through.
func TestCappedConcurrentEdge(t *testing.T) {
	r := writeReq()
	worst, _ := WorstWrite(r)
	st := &lockedStore{memStore: &memStore{}}
	f := fakeAt(worstUsage(r))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = capped(f, st, worst+worst/2, worst*10).Write(context.Background(), r)
		}()
	}
	wg.Wait()
	var ok, refused int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ai.ErrSpendCap):
			refused++
		default:
			t.Fatalf("unexpected err %v", err)
		}
	}
	if ok != 1 || refused != 1 || f.Calls() != 1 {
		t.Fatalf("ok %d refused %d calls %d, want 1 1 1", ok, refused, f.Calls())
	}
}

// A call that spends everything its request allows still lands within the cap the worst
// case was checked against.
func TestCappedOvershootWithinWorstCase(t *testing.T) {
	r := writeReq()
	worst, _ := WorstWrite(r)
	if c, _ := Cost(worstUsage(r)); c != worst {
		t.Fatalf("worst-case usage costs %d, WorstWrite says %d", c, worst)
	}
	// Every usage the request can produce: input ≤ bytes (+overhead), output ≤ MaxTokens,
	// split any way between input, cache read and cache write.
	wu := worstUsage(r)
	in := wu.CacheWriteTokens
	for _, u := range []Usage{
		{Model: ModelSonnet, InputTokens: in, OutputTokens: r.MaxTokens},
		{Model: ModelSonnet, CacheReadTokens: in, OutputTokens: r.MaxTokens},
		{Model: ModelSonnet, InputTokens: in / 2, CacheWriteTokens: in / 2, OutputTokens: r.MaxTokens},
		wu,
	} {
		if c, _ := Cost(u); c > worst {
			t.Errorf("usage %+v costs %d > worst %d", u, c, worst)
		}
	}
	const daily = 1_000_000
	st := &memStore{rows: []UsageRow{{AccountID: 1, Purpose: PurposeDrafting, Day: testDay, CostMicros: daily - worst}}}
	if _, _, err := capped(fakeAt(wu), st, daily, daily).Write(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got := st.total(); got > daily {
		t.Errorf("spend %d passed the cap %d", got, daily)
	}
}

// A pause_turn continuation is checked again: when the first leg's used cost plus the next
// leg's worst case passes the cap, the turn stops there and what was billed is recorded.
func TestCappedResearchContinuationChecked(t *testing.T) {
	srv, got := server(t, func(int) (int, string) { return 200, message("pause_turn", "", 1000, 100, 1) })
	r := researchReq()
	r.LeadID = 9
	w0, _ := WorstResearchLeg(r, 0)
	w1, _ := WorstResearchLeg(r, 1)
	if w1 <= w0 {
		t.Fatalf("leg 1 worst %d not above leg 0 %d", w1, w0)
	}
	st := &memStore{}
	c := &Capped{Client: newClient(t, srv.URL), Store: st, AccountID: 1, Purpose: PurposeResearch,
		DailyCapMicros: w0, LeadCapMicros: 1_000_000, Loc: testLoc, Now: testNow}
	_, u, err := c.Research(context.Background(), r)
	if !errors.Is(err, ai.ErrSpendCap) {
		t.Fatalf("err %v, want ErrSpendCap", err)
	}
	if got.calls.Load() != 1 {
		t.Errorf("calls %d, want 1", got.calls.Load())
	}
	if len(st.rows) != 1 || st.rows[0].InputTokens != 1000 || st.rows[0].WebSearches != 1 || u.InputTokens != 1000 {
		t.Errorf("rows %+v usage %+v", st.rows, u)
	}
	if st.total() > w0 {
		t.Errorf("leg 0 cost %d over its worst case %d", st.total(), w0)
	}
}
