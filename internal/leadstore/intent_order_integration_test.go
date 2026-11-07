//go:build integration

package leadstore_test

import (
	"context"
	"encoding/hex"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/intent"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// intentRow appends one derived lead_intent row for lead, as a sync writes it.
func (f outreachFixture) intentRow(t *testing.T, lead int64, class string, score int, reason string, heldUntil *time.Time) {
	t.Helper()
	f.ownerRun(t, `INSERT INTO lead_intent (lead_id, class, overlap, score, reason, signal_at, held_until)
		VALUES ($1, $2, 0, $3, $4, now(), $5)`, lead, class, score, reason, heldUntil)
}

// intentCall is the operator's call on lead (nil class clears it).
func (f outreachFixture) intentCall(t *testing.T, lead int64, class *string) {
	t.Helper()
	if err := f.q.SetIntentCall(context.Background(), lead, class, ""); err != nil {
		t.Fatalf("intent call on %d: %v", lead, err)
	}
}

func timep(t time.Time) *time.Time { return &t }

// intentQueue is one café lead per class (and the hold cases), each ranked so that points
// alone would order them the other way round, plus one starred café lead with no row.
type intentQueue struct {
	outreachFixture
	starred, shopLo, shopHi, paper, sw, curious, coldRow, cold, jcPast, outRoast int64
	jcHeld, outYours, outDerived                                                 int64
}

func newIntentQueue(t *testing.T) intentQueue {
	t.Helper()
	q := intentQueue{outreachFixture: newOutreachFixture(t)}
	rank := 100
	lead := func(name string) int64 {
		rank-- // the later a lead is made, the better its points
		d := name + ".localhost"
		return q.cafeLead(t, d, intp(rank), "info@"+d)
	}
	q.starred = lead("starred")
	q.shopLo, q.shopHi = lead("shop-lo"), lead("shop-hi")
	q.paper, q.sw, q.curious = lead("paper"), lead("switch"), lead("curious")
	q.coldRow, q.cold = lead("cold-row"), lead("cold")
	q.jcPast, q.outRoast = lead("jc-past"), lead("out-roastery")
	q.jcHeld, q.outYours, q.outDerived = lead("jc-held"), lead("out-yours"), lead("out-derived")
	q.star(t, q.starred)
	q.intentRow(t, q.shopLo, intent.Shopping, 1, "follows a vendor", nil)
	q.intentRow(t, q.shopHi, intent.Shopping, 9, "follows two vendors", nil)
	q.intentRow(t, q.paper, intent.Paper, 0, "paper card", nil)
	q.intentRow(t, q.sw, intent.Switch, 0, "on a weak vendor", nil)
	q.intentRow(t, q.curious, intent.Curious, 0, "one touch", nil)
	q.intentRow(t, q.coldRow, intent.Cold, 3, "a cold signal", nil)
	q.intentRow(t, q.jcPast, intent.JustChose, 0, "chose a vendor", timep(time.Now().Add(-time.Hour)))
	q.intentRow(t, q.outRoast, intent.Out, 0, "a roastery", nil)
	q.operatorPlaceType(t, q.outRoast, leadstore.PlaceRoastery)
	q.intentRow(t, q.jcHeld, intent.JustChose, 0, "chose a vendor", timep(time.Now().Add(30*24*time.Hour)))
	q.intentCall(t, q.outYours, strp(intent.Out))
	q.intentRow(t, q.outDerived, intent.Out, 0, "own customer", nil)
	if _, err := q.q.SetRoasteryOutreach(context.Background(), 0, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	return q
}

// held is the leads the intent holds now.
func (q intentQueue) held() []int64 { return []int64{q.jcHeld, q.outYours, q.outDerived} }

// TestQueueIntentOrder: after the starred, the queue goes shopping (score first) > paper >
// switch > curious > cold (score first) > a released just chose > out as a roastery (the
// Roasteries switch lets it in), whatever the points or the relevance say; with HoldIntent a
// held just chose, your Out and a derived Out are left out, and NextUp holds them by itself.
// Without HoldIntent (Send now on chosen leads, the samples) they are offered, after the rest.
func TestQueueIntentOrder(t *testing.T) {
	q := newIntentQueue(t)
	ctx := context.Background()
	want := []int64{q.starred, q.shopHi, q.shopLo, q.paper, q.sw, q.curious, q.coldRow, q.cold, q.jcPast, q.outRoast}
	for _, trusted := range [][]string{nil, {leadstore.TypeCafe}} {
		o := leadstore.CandidateOptions{Profile: testProfile, TrustedTypes: trusted, HoldIntent: true}
		if got := leadIDs(candidates(t, q.q, o)); !reflect.DeepEqual(got, want) {
			t.Errorf("trusted %v: held queue %v, want %v", trusted, got, want)
		}
		o.HoldIntent, o.IncludeReserved = false, true
		next, err := q.q.NextUp(ctx, o, 50)
		if err != nil {
			t.Fatal(err)
		}
		if got := leadIDs(next); !reflect.DeepEqual(got, want) {
			t.Errorf("trusted %v: NextUp %v, want %v (it holds by itself)", trusted, got, want)
		}
		all := leadIDs(candidates(t, q.q, o))
		if !reflect.DeepEqual(all[:8], want[:8]) {
			t.Errorf("trusted %v: unheld queue %v, want it to start %v", trusted, all, want[:8])
		}
		for _, id := range q.held() {
			if i := slices.Index(all, id); i < slices.Index(all, q.cold) {
				t.Errorf("trusted %v: held lead %d at %d of the unheld queue %v, want it after cold", trusted, id, i, all)
			}
		}
	}
	// Relevance sorts only inside an intent tie: the low-scored shopping lead rated best
	// stays behind the high-scored one.
	q.rateUnder(t, q.shopLo, leadstore.QuestionCafe, 100, leadstore.BandStrong)
	q.rateUnder(t, q.shopHi, leadstore.QuestionCafe, 5, leadstore.BandNo)
	o := leadstore.CandidateOptions{Profile: testProfile, TrustedTypes: []string{leadstore.TypeCafe}, HoldIntent: true}
	if got := leadIDs(candidates(t, q.q, o)); !reflect.DeepEqual(got, want) {
		t.Errorf("rated: %v, want %v", got, want)
	}
}

// TestLeadListIntentOrderMatchesQueue: the Leads list's default order is the batch's on the
// same seed (one intentOrderSQL, before relevance), for every lead the queue offers but the
// starred one (the list has no star keys), trusted or not; IntentClass is the effective class.
func TestLeadListIntentOrderMatchesQueue(t *testing.T) {
	q := newIntentQueue(t)
	q.rateUnder(t, q.paper, leadstore.QuestionCafe, 90, leadstore.BandStrong)
	q.rateUnder(t, q.cold, leadstore.QuestionCafe, 80, leadstore.BandStrong)
	for _, trusted := range [][]string{nil, {leadstore.TypeCafe}} {
		queue := leadIDs(candidates(t, q.q, leadstore.CandidateOptions{Profile: testProfile, TrustedTypes: trusted}))
		queue = slices.DeleteFunc(queue, func(id int64) bool { return id == q.starred })
		rows, _ := listRows(t, q.q, leadstore.ListFilter{}, leadstore.ListContext{ProfileJSON: profileJSON(t, testProfile), TrustedTypes: trusted})
		var list []int64
		class := map[int64]string{}
		for _, r := range rows {
			class[r.LeadID] = r.IntentClass
			if slices.Contains(queue, r.LeadID) {
				list = append(list, r.LeadID)
			}
		}
		if !reflect.DeepEqual(list, queue) {
			t.Errorf("trusted %v: list %v, queue %v", trusted, list, queue)
		}
		for id, c := range map[int64]string{q.shopHi: intent.Shopping, q.outYours: intent.Out, q.cold: intent.Cold, q.jcHeld: intent.JustChose} {
			if class[id] != c {
				t.Errorf("lead %d IntentClass %q, want %q", id, class[id], c)
			}
		}
	}
}

// TestHoldIntentJustChoseExpires: a Just chose is held until its held_until, then offered
// again at its rank; your call Out holds until cleared.
func TestHoldIntentJustChoseExpires(t *testing.T) {
	f := newOutreachFixture(t)
	jc := f.cafeLead(t, "jc.localhost", nil, "info@jc.localhost")
	yours := f.cafeLead(t, "yours.localhost", nil, "info@yours.localhost")
	plain := f.cafeLead(t, "plain.localhost", nil, "info@plain.localhost")
	f.intentRow(t, jc, intent.JustChose, 0, "chose a vendor", timep(time.Now().Add(24*time.Hour)))
	f.intentCall(t, yours, strp(intent.Out))
	o := leadstore.CandidateOptions{Profile: testProfile, HoldIntent: true}
	if got := leadIDs(candidates(t, f.q, o)); !reflect.DeepEqual(got, []int64{plain}) {
		t.Errorf("held: %v, want only %d", got, plain)
	}
	f.intentRow(t, jc, intent.JustChose, 0, "chose a vendor", timep(time.Now().Add(-time.Minute)))
	f.intentCall(t, yours, nil)
	if got := leadIDs(candidates(t, f.q, o)); !reflect.DeepEqual(got, []int64{yours, plain, jc}) {
		t.Errorf("released: %v, want %d, %d (cold, by id), %d (just chose, rank 6)", got, yours, plain, jc)
	}
}

// TestHoldIntentNullHeldUntil: a just_chose row with no held_until is not held, and the queue
// agrees with the held-status reads (StarredStatus, LatestIntents): a NULL never hides a lead.
func TestHoldIntentNullHeldUntil(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	jc := f.cafeLead(t, "jc-null.localhost", nil, "info@jc-null.localhost")
	f.intentRow(t, jc, intent.JustChose, 0, "chose a vendor", nil)
	f.star(t, jc)
	o := leadstore.CandidateOptions{Profile: testProfile, HoldIntent: true}
	if got := leadIDs(candidates(t, f.q, o)); !slices.Contains(got, jc) {
		t.Errorf("queue with HoldIntent: %v, want %d (no held_until is not held)", got, jc)
	}
	st, err := f.q.StarredStatus(ctx, leadstore.StarredStatusOptions{CandidateOptions: leadstore.CandidateOptions{Profile: testProfile, HoldIntent: true, IncludeReserved: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.LeadID == jc && s.Reason != leadstore.StarredReady {
			t.Errorf("StarredStatus: %q %q, want ready", s.Reason, s.Detail)
		}
	}
	rows, err := f.q.LatestIntents(ctx, []int64{jc})
	if err != nil {
		t.Fatal(err)
	}
	if r := rows[jc]; r.Class != intent.JustChose || r.Held {
		t.Errorf("LatestIntents: %+v, want just_chose, not held", r)
	}
}

// TestHoldIntentStarred: a hold is a filter, so a starred lead its intent holds is held too,
// and StarredStatus says why (a Just chose with the day it ends, your Out, a derived Out with
// its reason); without HoldIntent it is ready.
func TestHoldIntentStarred(t *testing.T) {
	q := newIntentQueue(t)
	ctx := context.Background()
	for _, id := range q.held() {
		q.star(t, id)
	}
	status := func(hold bool) map[int64]leadstore.StarredState {
		t.Helper()
		st, err := q.q.StarredStatus(ctx, leadstore.StarredStatusOptions{CandidateOptions: leadstore.CandidateOptions{Profile: testProfile, HoldIntent: hold, IncludeReserved: true}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]leadstore.StarredState{}
		for _, s := range st {
			out[s.LeadID] = s
		}
		return out
	}
	got := status(true)
	for id, detail := range map[int64]string{q.jcHeld: "just chose a vendor", q.outYours: "you marked it Out", q.outDerived: "Out: own customer"} {
		if s := got[id]; s.Reason != leadstore.StarredIntentHeld || s.Detail != detail {
			t.Errorf("lead %d: %q %q, want %q %q", id, s.Reason, s.Detail, leadstore.StarredIntentHeld, detail)
		}
	}
	if d := got[q.jcHeld].Date; d == nil || d.Before(time.Now().Add(29*24*time.Hour)) {
		t.Errorf("just chose's date %v, want its held_until", d)
	}
	if s := got[q.starred]; s.Reason != leadstore.StarredReady {
		t.Errorf("the plain starred lead is %q", s.Reason)
	}
	for _, id := range q.held() {
		if s := status(false)[id]; s.Reason != leadstore.StarredReady {
			t.Errorf("without HoldIntent lead %d is %q %q, want ready", id, s.Reason, s.Detail)
		}
	}
}

// TestHoldIntentDrupalUnchanged: a Drupal queue and the Leads list read in exactly the order
// they did with the intent tables empty once café leads hold every class and a call, with
// HoldIntent on or off, trusted or not.
func TestHoldIntentDrupalUnchanged(t *testing.T) {
	f := newOutreachFixture(t)
	ids := variedLeads(t, f)
	for i, id := range ids {
		if i%3 == 0 {
			f.rate(t, id, (i*7)%101, leadstore.BandMaybe)
		}
	}
	p := testProfile.Clone()
	p.ExcludeCountries = nil
	f.star(t, ids[5])
	f.star(t, ids[2])
	type key struct{ hold, trusted bool }
	orders := func() map[key][2][]int64 {
		t.Helper()
		out := map[key][2][]int64{}
		for _, hold := range []bool{false, true} {
			for _, trusted := range []bool{false, true} {
				queue := slices.DeleteFunc(leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: p, TrustedTypes: trustedIf(trusted), HoldIntent: hold})),
					func(id int64) bool { return !slices.Contains(ids, id) })
				list, _ := listIDs(t, f.q, leadstore.ListFilter{}, leadstore.ListContext{ProfileJSON: profileJSON(t, p), TrustedTypes: trustedIf(trusted)})
				list = slices.DeleteFunc(list, func(id int64) bool { return !slices.Contains(ids, id) })
				out[key{hold, trusted}] = [2][]int64{queue, list}
			}
		}
		return out
	}
	before := orders()
	if len(before[key{}][0]) != len(ids) {
		t.Fatalf("%d Drupal candidates, want %d", len(before[key{}][0]), len(ids))
	}
	for _, c := range append(slices.Clone(leadstore.IntentClasses), "call") {
		d := "cafe-" + strings.ReplaceAll(c, "_", "-") + ".localhost"
		id := f.cafeLead(t, d, intp(1), "info@"+d)
		if c == "call" {
			f.intentCall(t, id, strp(intent.Out))
			continue
		}
		f.intentRow(t, id, c, 50, "a reason", timep(time.Now().Add(time.Hour)))
	}
	if after := orders(); !reflect.DeepEqual(after, before) {
		t.Errorf("with café intent rows the Drupal orders changed:\nbefore %v\nafter  %v", before, after)
	}
}

// seqBatchOf drafts and approves one five-step batch for leads (each lead's step 0 due at
// dueBase, each with its own number) and returns each lead's messages.
func (f outreachFixture) seqBatchOf(t *testing.T, leads ...int64) map[int64]seqLead {
	t.Helper()
	ctx := context.Background()
	var drafts []leadstore.DraftRow
	facts := map[int64]int64{}
	for i, lead := range leads {
		facts[lead] = f.phoneFact(t, lead, callNumber[:len(callNumber)-1]+string(rune('2'+i)))
		drafts = append(drafts, f.seqDrafts(t, lead, facts[lead])...)
	}
	id, sum, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}},
		false, drafts, []byte("t"))
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if _, err := f.q.ApproveBatch(ctx, id, hex.EncodeToString(sum), nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return dueBase
		}
		return time.Time{}
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	ms, err := f.q.BatchMessages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]seqLead{}
	for _, lead := range leads {
		out[lead] = seqLead{lead: lead, batch: id, msg: map[int]int64{}, phoneFact: facts[lead]}
	}
	for _, m := range ms {
		out[m.LeadID].msg[m.Step] = m.ID
	}
	return out
}

// TestHoldIntentAfterBatched: a lead held after it was batched. Your Out on a contacted lead
// takes its call out of the call queue, IntentHeldMessages names its unsent messages and
// CancelIntentHeldLead cancels them with SkipIntentHeld, leaving its state and its batch's
// other lead untouched; a sync's just chose on a batched lead with nothing sent sends it
// back to new with the hold's event. A manual batch's messages are never held.
func TestHoldIntentAfterBatched(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	out := f.cafeLead(t, "held-out.localhost", nil, "info@held-out.localhost")
	other := f.cafeLead(t, "other.localhost", nil, "info@other.localhost")
	pair := f.seqBatchOf(t, out, other)
	for _, s := range pair {
		f.send(t, s, false, 0, day(0))
		f.send(t, s, false, 1, day(3))
	}
	calls := func() []int64 {
		t.Helper()
		rows, err := f.q.CallQueue(ctx, day(5).Add(3*time.Hour), time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, r := range rows {
			ids = append(ids, r.LeadID)
		}
		return sortedIDs(ids...)
	}
	if got := calls(); !reflect.DeepEqual(got, sortedIDs(out, other)) {
		t.Fatalf("call queue %v before the hold", got)
	}
	f.intentCall(t, out, strp(intent.Out))
	if got := calls(); !reflect.DeepEqual(got, []int64{other}) {
		t.Errorf("call queue %v, want only %d (your Out does not ring)", got, other)
	}
	var msgs []int64
	for _, s := range pair {
		for _, id := range s.msg {
			msgs = append(msgs, id)
		}
	}
	held, err := f.q.IntentHeldMessages(ctx, msgs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 5 {
		t.Errorf("held messages %v, want the held lead's 5", held)
	}
	for id := range held {
		if !slices.Contains(stepIDs(pair[out].msg), id) {
			t.Errorf("message %d of another lead held", id)
		}
	}
	got, err := f.q.CancelIntentHeldLead(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cancelled != 3 || got.BackToNew {
		t.Errorf("contacted lead: %+v, want 3 cancelled, kept contacted", got)
	}
	for step := 2; step <= 4; step++ {
		if r := f.rowOf(t, pair[out].msg[step]); r.state != leadstore.MsgCancelled || r.reason != leadstore.SkipIntentHeld {
			t.Errorf("held step %d: %+v", step, r)
		}
		if r := f.rowOf(t, pair[other].msg[step]); r.state == leadstore.MsgCancelled {
			t.Errorf("the batch's other lead's step %d was cancelled: %+v", step, r)
		}
	}

	// A sync newly writes just chose on a batched lead with nothing sent: back to new.
	jc := f.cafeLead(t, "held-jc.localhost", nil, "info@held-jc.localhost")
	s := f.seqBatchOf(t, jc)[jc]
	f.intentRow(t, jc, intent.JustChose, 0, "chose a vendor", timep(time.Now().Add(24*time.Hour)))
	if held, err := f.q.IntentHeldMessages(ctx, stepIDs(s.msg), time.Now()); err != nil || len(held) != 5 {
		t.Fatalf("just chose held %v, %v", held, err)
	}
	if got, err := f.q.CancelIntentHeldLead(ctx, jc); err != nil || got.Cancelled != 5 || !got.BackToNew {
		t.Errorf("batched lead: %+v %v, want 5 cancelled and back to new", got, err)
	}
	var state, reason string
	if err := f.owner.QueryRow(ctx, `SELECT l.state, e.reason FROM lead l JOIN lead_event e ON e.lead_id = l.id
		WHERE l.id = $1 ORDER BY e.id DESC LIMIT 1`, jc).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != leadstore.LeadNew || reason != leadstore.SkipIntentHeld {
		t.Errorf("just chose lead %s, last event %q", state, reason)
	}

	// A manual batch (Send now on chosen leads) is the operator's own act: never held.
	man := f.cafeLead(t, "manual.localhost", nil, "info@manual.localhost")
	ms := f.seqBatchOf(t, man)[man]
	f.ownerRun(t, `UPDATE outreach_batch SET manual = true WHERE id = $1`, ms.batch)
	f.intentCall(t, man, strp(intent.Out))
	if held, err := f.q.IntentHeldMessages(ctx, stepIDs(ms.msg), time.Now()); err != nil || len(held) != 0 {
		t.Errorf("manual batch held %v, %v", held, err)
	}
}

// stepIDs is the message ids of m.
func stepIDs(m map[int]int64) []int64 {
	out := make([]int64, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
