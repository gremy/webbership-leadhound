//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// triageInput is the JSON object a call on a fixture lead stores as its input.
var triageInput = []byte(`{"name":"Cafea Ficta"}`)

// triageFixture is an outreach fixture with the setters and readers the grid cells share.
type triageFixture struct {
	outreachFixture
	seq *int
}

func newTriageFixture(t *testing.T) triageFixture {
	t.Helper()
	return triageFixture{outreachFixture: newOutreachFixture(t), seq: new(int)}
}

// cafe adds a café lead on a domain of its own.
func (f triageFixture) cafe(t *testing.T) int64 {
	t.Helper()
	*f.seq++
	return f.cafeLead(t, fmt.Sprintf("triage-cafe-%d.localhost", *f.seq), nil, "")
}

// drupal adds a Drupal lead on a domain of its own.
func (f triageFixture) drupal(t *testing.T) int64 {
	t.Helper()
	*f.seq++
	return f.siteLead(t, fmt.Sprintf("triage-drupal-%d.localhost", *f.seq), nil, 7, "")
}

// selftest adds the selftest lead and returns its id.
func (f triageFixture) selftest(t *testing.T) int64 {
	t.Helper()
	if _, err := f.q.CreateSelftest(context.Background(), leadstore.SelftestDraft{To: "operator" + "@" + "sender.localhost",
		Subject: "selftest", Body: "selftest", MessageID: "<triage@sender.localhost>", Due: time.Now(), MailboxID: f.mailbox(t)}); err != nil {
		t.Fatal(err)
	}
	return ownerInt(t, f.owner, `SELECT id FROM lead WHERE kind = 'selftest'`)
}

// set moves lead to state by actor, as any road other than the triage would.
func (f triageFixture) set(t *testing.T, lead int64, state, actor string) {
	t.Helper()
	if err := f.q.SetLeadState(context.Background(), lead, state, "fixture", actor); err != nil {
		t.Fatalf("set %d to %s: %v", lead, state, err)
	}
}

// call appends a call on lead by hand, with band "" meaning a cleared call.
func (f triageFixture) call(t *testing.T, lead int64, band string) {
	t.Helper()
	var err error
	if band == "" {
		_, err = f.q.SetLeadLabel(context.Background(), lead, nil, nil, leadstore.ActorOperator)
	} else {
		_, err = f.q.SetLeadLabel(context.Background(), lead, &band, triageInput, leadstore.ActorOperator)
	}
	if err != nil {
		t.Fatalf("call %q on %d: %v", band, lead, err)
	}
}

// skip is the operator's skip of lead with its input, which must move it.
func (f triageFixture) skip(t *testing.T, lead int64, input []byte) {
	t.Helper()
	r, err := f.q.TriageSkip(context.Background(), lead, input)
	if err != nil || !r.Moved {
		t.Fatalf("skip %d: %+v, %v", lead, r, err)
	}
}

// calls is every lead_label row of lead in order, each as "band/actor" with a cleared call as
// "cleared/actor".
func (f triageFixture) calls(t *testing.T, lead int64) []string {
	t.Helper()
	rows, err := f.owner.Query(context.Background(),
		`SELECT coalesce(band, 'cleared') || '/' || actor FROM lead_label WHERE lead_id = $1 ORDER BY id`, lead)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// events is how many lead_events lead has.
func (f triageFixture) events(t *testing.T, lead int64) int64 {
	t.Helper()
	return ownerInt(t, f.owner, `SELECT count(*) FROM lead_event WHERE lead_id = $1`, lead)
}

// wantLead checks lead's state, its calls and how many events it gained since before.
func (f triageFixture) wantLead(t *testing.T, lead int64, state string, calls []string, before, gained int64) {
	t.Helper()
	if got := f.leadOf(t, lead).State; got != state {
		t.Errorf("state %s, want %s", got, state)
	}
	if got := f.calls(t, lead); !slices.Equal(got, calls) {
		t.Errorf("calls %v, want %v", got, calls)
	}
	if got := f.events(t, lead) - before; got != gained {
		t.Errorf("%d events gained, want %d", got, gained)
	}
}

const (
	callNo      = "no/operator"
	callLikely  = "likely/operator"
	callStrong  = "strong/operator"
	callCleared = "cleared/operator"
)

// TestTriageSkipGrid is the grid's Skip column: only a lead in new moves, a café lead with no
// current call gains the call "no", and every other cell writes nothing.
func TestTriageSkipGrid(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name      string
		make      func(*testing.T) int64
		input     []byte
		moved     bool
		written   bool
		state     string
		calls     []string
		newEvents int64
	}{
		{name: "new, no call", make: f.cafe, input: triageInput, moved: true, written: true, state: leadstore.LeadSkipped, calls: []string{callNo}, newEvents: 1},
		{name: "new, call strong is kept", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.call(t, l, leadstore.BandStrong)
			return l
		}, input: triageInput, moved: true, state: leadstore.LeadSkipped, calls: []string{callStrong}, newEvents: 1},
		{name: "new, a cleared call is no call", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.call(t, l, leadstore.BandStrong)
			f.call(t, l, "")
			return l
		}, input: triageInput, moved: true, written: true, state: leadstore.LeadSkipped, calls: []string{callStrong, callCleared, callNo}, newEvents: 1},
		{name: "new, a café lead with no input writes no call", make: f.cafe, moved: true, state: leadstore.LeadSkipped, calls: []string{}, newEvents: 1},
		{name: "a Drupal lead writes no call", make: f.drupal, moved: true, state: leadstore.LeadSkipped, calls: []string{}, newEvents: 1},
		{name: "a Drupal lead handed an input writes no call", make: f.drupal, input: triageInput, moved: true, state: leadstore.LeadSkipped, calls: []string{}, newEvents: 1},
		{name: "batched does not move", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, leadstore.LeadBatched, leadstore.ActorSystem)
			return l
		}, input: triageInput, state: leadstore.LeadBatched, calls: []string{}},
		{name: "contacted does not move", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, leadstore.LeadContacted, leadstore.ActorSystem)
			return l
		}, input: triageInput, state: leadstore.LeadContacted, calls: []string{}},
		{name: "replied does not move", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, leadstore.LeadReplied, leadstore.ActorSystem)
			return l
		}, input: triageInput, state: leadstore.LeadReplied, calls: []string{}},
		{name: "skipped by gremy does not move again", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			return l
		}, input: triageInput, state: leadstore.LeadSkipped, calls: []string{callNo}},
		{name: "skipped by gremy, call changed since, does not move again", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			f.call(t, l, leadstore.BandMaybe)
			return l
		}, input: triageInput, state: leadstore.LeadSkipped, calls: []string{callNo, "maybe/operator"}},
		{name: "skipped by the system does not move and writes no call", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, leadstore.LeadSkipped, leadstore.ActorSystem)
			return l
		}, input: triageInput, state: leadstore.LeadSkipped, calls: []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			lead := c.make(t)
			before := f.events(t, lead)
			r, err := f.q.TriageSkip(ctx, lead, c.input)
			if err != nil {
				t.Fatal(err)
			}
			if r.Moved != c.moved || r.CallWritten != c.written || r.State != c.state {
				t.Errorf("result %+v, want moved %v, written %v, state %s", r, c.moved, c.written, c.state)
			}
			f.wantLead(t, lead, c.state, c.calls, before, c.newEvents)
		})
	}
}

// TestTriageSkipEvent: the skip's event names the operator, the reason, and the call it wrote,
// and names no call when it wrote none.
func TestTriageSkipEvent(t *testing.T) {
	f := newTriageFixture(t)
	wrote := f.cafe(t)
	f.skip(t, wrote, triageInput)
	label := ownerInt(t, f.owner, `SELECT id FROM lead_label WHERE lead_id = $1`, wrote)
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_event
		WHERE lead_id = $1 AND state = 'skipped' AND actor = 'gremy' AND reason = 'skipped from the list' AND skip_label_id = $2`, wrote, label); n != 1 {
		t.Errorf("%d matching events of a skip that wrote call %d", n, label)
	}
	drupal := f.drupal(t)
	f.skip(t, drupal, nil)
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_event WHERE lead_id = $1 AND state = 'skipped' AND skip_label_id IS NULL`, drupal); n != 1 {
		t.Errorf("%d events without a call on a Drupal skip", n)
	}
}

// TestTriageSkipRefusals: a missing or selftest lead is not found, and an input that is not a
// JSON object writes nothing.
func TestTriageSkipRefusals(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	if _, err := f.q.TriageSkip(ctx, 999_999, triageInput); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v, want ErrLeadNotFound", err)
	}
	if _, err := f.q.TriageSkip(ctx, f.selftest(t), nil); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("selftest lead: %v, want ErrLeadNotFound", err)
	}
	lead := f.cafe(t)
	before := f.events(t, lead)
	for _, bad := range [][]byte{{}, []byte(`[1]`), []byte(`{oops`)} {
		if _, err := f.q.TriageSkip(ctx, lead, bad); !errors.Is(err, leadstore.ErrBadLabelInput) {
			t.Errorf("input %q: %v, want ErrBadLabelInput", bad, err)
		}
	}
	f.wantLead(t, lead, leadstore.LeadNew, []string{}, before, 0)
}

// TestTriageSkipTwoTabs: two skips of one lead at the same moment, on two connections, move it
// once and write one call and one event.
func TestTriageSkipTwoTabs(t *testing.T) {
	f := newTriageFixture(t)
	lead := f.cafe(t)
	before := f.events(t, lead)
	var (
		wg      sync.WaitGroup
		results [2]leadstore.TriageSkipResult
		errs    [2]error
	)
	for i := range results {
		q := leadstore.NewQueries(storetest.AppConn(t))
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = q.TriageSkip(context.Background(), lead, triageInput)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("tab %d: %v", i, err)
		}
		if results[i].State != leadstore.LeadSkipped {
			t.Errorf("tab %d: state %s", i, results[i].State)
		}
	}
	if results[0].Moved == results[1].Moved {
		t.Errorf("results %+v, want exactly one move", results)
	}
	f.wantLead(t, lead, leadstore.LeadSkipped, []string{callNo}, before, 1)
}

// TestTriageUndoGrid is the grid's Undo column.
func TestTriageUndoGrid(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name      string
		make      func(*testing.T) int64
		moved     bool
		cleared   bool
		state     string
		calls     []string
		newEvents int64
	}{
		{name: "the skip wrote the call: new, call cleared", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			return l
		}, moved: true, cleared: true, state: leadstore.LeadNew, calls: []string{callNo, callCleared}, newEvents: 1},
		{name: "the skip found a call: new, call kept", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.call(t, l, leadstore.BandStrong)
			f.skip(t, l, triageInput)
			return l
		}, moved: true, state: leadstore.LeadNew, calls: []string{callStrong}, newEvents: 1},
		{name: "a hand no since the skip: new, call kept", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			f.call(t, l, leadstore.BandNo)
			return l
		}, moved: true, state: leadstore.LeadNew, calls: []string{callNo, callNo}, newEvents: 1},
		{name: "a Good since the skip: new, call kept", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			if _, err := f.q.TriageGood(ctx, l, triageInput); err != nil {
				t.Fatal(err)
			}
			return l
		}, moved: true, state: leadstore.LeadNew, calls: []string{callNo, callLikely}, newEvents: 1},
		{name: "cleared by hand since the skip: no second cleared row", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			f.call(t, l, "")
			return l
		}, moved: true, state: leadstore.LeadNew, calls: []string{callNo, callCleared}, newEvents: 1},
		{name: "a Drupal lead: new, no call written", make: func(t *testing.T) int64 {
			l := f.drupal(t)
			f.skip(t, l, nil)
			return l
		}, moved: true, state: leadstore.LeadNew, calls: []string{}, newEvents: 1},
		{name: "a state skipped by gremy some other way: new", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, leadstore.LeadSkipped, leadstore.ActorGremy)
			return l
		}, moved: true, state: leadstore.LeadNew, calls: []string{}, newEvents: 1},
		{name: "new does not move", make: f.cafe, state: leadstore.LeadNew, calls: []string{}},
		{name: "undone already does not move", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			if r, err := f.q.TriageUndoSkip(ctx, l); err != nil || !r.Moved {
				t.Fatalf("first undo: %+v, %v", r, err)
			}
			return l
		}, state: leadstore.LeadNew, calls: []string{callNo, callCleared}},
		{name: "batched does not move", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, leadstore.LeadBatched, leadstore.ActorGremy)
			return l
		}, state: leadstore.LeadBatched, calls: []string{}},
		{name: "contacted does not move", make: func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, leadstore.LeadContacted, leadstore.ActorGremy)
			return l
		}, state: leadstore.LeadContacted, calls: []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			lead := c.make(t)
			before := f.events(t, lead)
			r, err := f.q.TriageUndoSkip(ctx, lead)
			if err != nil {
				t.Fatal(err)
			}
			if r.Moved != c.moved || r.CallCleared != c.cleared || r.State != c.state {
				t.Errorf("result %+v, want moved %v, cleared %v, state %s", r, c.moved, c.cleared, c.state)
			}
			f.wantLead(t, lead, c.state, c.calls, before, c.newEvents)
		})
	}
}

// TestTriageUndoEvent: an undo writes its own event by the operator, naming no call.
func TestTriageUndoEvent(t *testing.T) {
	f := newTriageFixture(t)
	lead := f.cafe(t)
	f.skip(t, lead, triageInput)
	if _, err := f.q.TriageUndoSkip(context.Background(), lead); err != nil {
		t.Fatal(err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_event
		WHERE lead_id = $1 AND state = 'new' AND actor = 'gremy' AND reason = 'skip undone from the list' AND skip_label_id IS NULL`, lead); n != 1 {
		t.Errorf("%d matching undo events", n)
	}
}

// TestTriageUndoSystemSkip: a skip by the system is refused with its own error and changes
// nothing, including a system skip that follows an operator's undone skip.
func TestTriageUndoSystemSkip(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	plain := f.cafe(t)
	f.set(t, plain, leadstore.LeadSkipped, leadstore.ActorSystem)
	later := f.cafe(t)
	f.skip(t, later, triageInput)
	if _, err := f.q.TriageUndoSkip(ctx, later); err != nil {
		t.Fatal(err)
	}
	if moved, err := f.q.SkipNewLead(ctx, later, "a duplicate"); err != nil || !moved {
		t.Fatalf("system skip: %v, %v", moved, err)
	}
	for lead, calls := range map[int64][]string{plain: {}, later: {callNo, callCleared}} {
		before := f.events(t, lead)
		r, err := f.q.TriageUndoSkip(ctx, lead)
		if !errors.Is(err, leadstore.ErrSystemSkip) {
			t.Errorf("lead %d: %v, want ErrSystemSkip", lead, err)
		}
		if r.Moved || r.CallCleared || r.State != leadstore.LeadSkipped {
			t.Errorf("lead %d: result %+v", lead, r)
		}
		f.wantLead(t, lead, leadstore.LeadSkipped, calls, before, 0)
	}
}

// TestTriageUndoRefusals: a missing or selftest lead is not found.
func TestTriageUndoRefusals(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	if _, err := f.q.TriageUndoSkip(ctx, 999_999); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v, want ErrLeadNotFound", err)
	}
	if _, err := f.q.TriageUndoSkip(ctx, f.selftest(t)); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("selftest lead: %v, want ErrLeadNotFound", err)
	}
}

// TestTriageSkipUndoRoundTrip: a second skip after an undo writes a fresh call, because the
// cleared call left none, and undoing it clears that one in turn. The current call is read
// back through the answer key, which leaves a cleared lead out.
func TestTriageSkipUndoRoundTrip(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	lead := f.cafe(t)
	for range 2 {
		f.skip(t, lead, triageInput)
		if n, err := f.q.LabelCount(ctx, leadstore.TypeCafe); err != nil || n != 1 {
			t.Fatalf("after a skip: %d labelled, %v", n, err)
		}
		if r, err := f.q.TriageUndoSkip(ctx, lead); err != nil || !r.CallCleared {
			t.Fatalf("undo: %+v, %v", r, err)
		}
		if n, err := f.q.LabelCount(ctx, leadstore.TypeCafe); err != nil || n != 0 {
			t.Fatalf("after an undo: %d labelled, %v", n, err)
		}
	}
	if got, want := f.calls(t, lead), []string{callNo, callCleared, callNo, callCleared}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

// TestTriageGoodGrid is the grid's Good column: a call only, in any state, never moving the
// lead; a strong stays and a likely writes no second row.
func TestTriageGoodGrid(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	inState := func(state, actor string) func(*testing.T) int64 {
		return func(t *testing.T) int64 {
			l := f.cafe(t)
			f.set(t, l, state, actor)
			return l
		}
	}
	withCall := func(band string) func(*testing.T) int64 {
		return func(t *testing.T) int64 {
			l := f.cafe(t)
			f.call(t, l, band)
			return l
		}
	}
	for _, c := range []struct {
		name    string
		make    func(*testing.T) int64
		band    string
		written bool
		state   string
		calls   []string
	}{
		{"new, no call", f.cafe, leadstore.BandLikely, true, leadstore.LeadNew, []string{callLikely}},
		{"new, call strong stays", withCall(leadstore.BandStrong), leadstore.BandStrong, false, leadstore.LeadNew, []string{callStrong}},
		{"new, call likely writes no row", withCall(leadstore.BandLikely), leadstore.BandLikely, false, leadstore.LeadNew, []string{callLikely}},
		{"new, call no becomes likely", withCall(leadstore.BandNo), leadstore.BandLikely, true, leadstore.LeadNew, []string{callNo, callLikely}},
		{"new, a cleared call is no call", func(t *testing.T) int64 {
			l := withCall(leadstore.BandStrong)(t)
			f.call(t, l, "")
			return l
		}, leadstore.BandLikely, true, leadstore.LeadNew, []string{callStrong, callCleared, callLikely}},
		{"batched: call only", inState(leadstore.LeadBatched, leadstore.ActorSystem), leadstore.BandLikely, true, leadstore.LeadBatched, []string{callLikely}},
		{"contacted: call only", inState(leadstore.LeadContacted, leadstore.ActorSystem), leadstore.BandLikely, true, leadstore.LeadContacted, []string{callLikely}},
		{"skipped by gremy, skip wrote the call: call only", func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			return l
		}, leadstore.BandLikely, true, leadstore.LeadSkipped, []string{callNo, callLikely}},
		{"skipped by gremy, a hand no since: call only", func(t *testing.T) int64 {
			l := f.cafe(t)
			f.skip(t, l, triageInput)
			f.call(t, l, leadstore.BandNo)
			return l
		}, leadstore.BandLikely, true, leadstore.LeadSkipped, []string{callNo, callNo, callLikely}},
		{"skipped by the system: call only", inState(leadstore.LeadSkipped, leadstore.ActorSystem), leadstore.BandLikely, true, leadstore.LeadSkipped, []string{callLikely}},
	} {
		t.Run(c.name, func(t *testing.T) {
			lead := c.make(t)
			before := f.events(t, lead)
			r, err := f.q.TriageGood(ctx, lead, triageInput)
			if err != nil {
				t.Fatal(err)
			}
			if r.Band != c.band || r.Written != c.written {
				t.Errorf("result %+v, want band %s, written %v", r, c.band, c.written)
			}
			f.wantLead(t, lead, c.state, c.calls, before, 0)
		})
	}
}

// TestTriageGoodRefusals: a Drupal lead is refused with or without an input, a missing or
// selftest lead is not found, and a café lead needs an input that is a JSON object.
func TestTriageGoodRefusals(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	drupal := f.drupal(t)
	for _, input := range [][]byte{nil, triageInput} {
		if _, err := f.q.TriageGood(ctx, drupal, input); !errors.Is(err, leadstore.ErrNotCafeLead) {
			t.Errorf("Drupal lead, input %q: %v, want ErrNotCafeLead", input, err)
		}
	}
	if got := f.calls(t, drupal); len(got) != 0 {
		t.Errorf("Drupal lead gained calls %v", got)
	}
	if _, err := f.q.TriageGood(ctx, 999_999, triageInput); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v, want ErrLeadNotFound", err)
	}
	if _, err := f.q.TriageGood(ctx, f.selftest(t), triageInput); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("selftest lead: %v, want ErrLeadNotFound", err)
	}
	cafeLead := f.cafe(t)
	for _, bad := range [][]byte{nil, {}, []byte(`[1]`), []byte(`{oops`)} {
		if _, err := f.q.TriageGood(ctx, cafeLead, bad); !errors.Is(err, leadstore.ErrBadLabelInput) {
			t.Errorf("input %q: %v, want ErrBadLabelInput", bad, err)
		}
	}
	if got := f.calls(t, cafeLead); len(got) != 0 {
		t.Errorf("café lead gained calls %v", got)
	}
}

// TestTriageCampaignProgress: every kind of lead counts where the plan says, a selftest lead
// and another campaign's lead count nowhere, and an empty campaign is 0 of 0.
func TestTriageCampaignProgress(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	campaign := f.leadOf(t, f.cafe(t)).CampaignID // one new lead: in the total only
	f.cafe(t)                                     // and another

	skipped := f.cafe(t)
	f.skip(t, skipped, triageInput) // reviewed
	drupalSkipped := f.drupal(t)
	f.skip(t, drupalSkipped, nil) // reviewed, with no call
	called := f.cafe(t)
	f.call(t, called, leadstore.BandMaybe) // reviewed
	batchedCalled := f.cafe(t)
	f.call(t, batchedCalled, leadstore.BandLikely)
	f.set(t, batchedCalled, leadstore.LeadBatched, leadstore.ActorSystem) // reviewed
	systemCalled := f.cafe(t)
	f.call(t, systemCalled, leadstore.BandNo)
	f.set(t, systemCalled, leadstore.LeadSkipped, leadstore.ActorSystem) // reviewed, by its call

	duplicate := f.cafe(t)
	f.set(t, duplicate, leadstore.LeadSkipped, leadstore.ActorSystem) // in neither
	batched := f.cafe(t)
	f.set(t, batched, leadstore.LeadBatched, leadstore.ActorSystem) // in neither
	cleared := f.cafe(t)
	f.call(t, cleared, leadstore.BandNo)
	f.call(t, cleared, "")
	f.set(t, cleared, leadstore.LeadContacted, leadstore.ActorSystem) // in neither: no current call
	undone := f.cafe(t)
	f.skip(t, undone, triageInput)
	if _, err := f.q.TriageUndoSkip(ctx, undone); err != nil { // new again: in the total only
		t.Fatal(err)
	}
	f.selftest(t)
	f.ownerRun(t, `UPDATE lead SET campaign_id = $1 WHERE kind = 'selftest'`, campaign)

	other := register(t, f.q, 0, "second-campaign", `{"slug": "second-campaign"}`)
	elsewhere := f.cafe(t)
	f.call(t, elsewhere, leadstore.BandNo)
	f.ownerRun(t, `UPDATE lead SET campaign_id = $1 WHERE id = $2`, other, elsewhere)

	if reviewed, total, err := f.q.CampaignProgress(ctx, campaign); err != nil || reviewed != 5 || total != 8 {
		t.Errorf("campaign: %d of %d, %v, want 5 of 8", reviewed, total, err)
	}
	if reviewed, total, err := f.q.CampaignProgress(ctx, other); err != nil || reviewed != 1 || total != 1 {
		t.Errorf("second campaign: %d of %d, %v, want 1 of 1", reviewed, total, err)
	}
	empty := register(t, f.q, 0, "empty-campaign", `{"slug": "empty-campaign"}`)
	if reviewed, total, err := f.q.CampaignProgress(ctx, empty); err != nil || reviewed != 0 || total != 0 {
		t.Errorf("empty campaign: %d of %d, %v, want 0 of 0", reviewed, total, err)
	}
}
