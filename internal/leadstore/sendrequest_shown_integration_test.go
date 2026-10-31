//go:build integration

package leadstore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestSendRequestShown: the warnings the confirm page showed round-trip through the claim; none
// stores []; anything but a JSON array is refused by the table.
func TestSendRequestShown(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	batch, _ := f.sendNowBatch(t, f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost"))
	notBefore := sendNow.AddDate(-100, 0, 0) // created_at is the database's own now

	if _, err := f.q.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: batch, HashPrefix: "0123456789ab", Operator: "op"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := f.q.ClaimSendRequest(ctx, batch, notBefore, sendNow)
	if err != nil || !ok || string(got.Shown) != "[]" {
		t.Fatalf("no shown: %s %v %v, want []", got.Shown, ok, err)
	}
	shown := json.RawMessage(`[{"code": "ramp", "over": 2, "used": 0, "allowed": 1}]`)
	if _, err := f.q.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: batch, HashPrefix: "0123456789ab", Operator: "op", Shown: shown}); err != nil {
		t.Fatal(err)
	}
	got, ok, err = f.q.ClaimSendRequest(ctx, batch, notBefore, sendNow)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	var a, b any
	_ = json.Unmarshal(got.Shown, &a)
	_ = json.Unmarshal(shown, &b)
	if !jsonEqual(a, b) {
		t.Errorf("shown %s, want %s", got.Shown, shown)
	}
	for _, bad := range []string{`{"code":"ramp"}`, `"ramp"`, `3`} {
		if _, err := f.q.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: batch, HashPrefix: "0123456789ab", Operator: "op",
			Shown: json.RawMessage(bad)}); sqlState(err) != "23514" {
			t.Errorf("shown %s: %v, want a check violation", bad, err)
		}
	}
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// TestFirstEmailGone: a lead's first email has gone when another step-0 email of a live batch
// is sending, sent or maybe sent; a shadow one counts only for a shadow send; a draft, approved,
// refused or cancelled one never does, nor the message itself.
func TestFirstEmailGone(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	live, _ := f.batch(t, false, lead)
	var first int64
	if err := owner.QueryRow(ctx, `SELECT id FROM outreach_message WHERE batch_id = $1 AND step = 0`, live).Scan(&first); err != nil {
		t.Fatal(err)
	}
	set := func(id int64, state string) {
		t.Helper()
		if _, err := owner.Exec(ctx, `UPDATE outreach_message SET state = $2 WHERE id = $1`, id, state); err != nil {
			t.Fatal(err)
		}
	}
	const other = int64(1 << 40) // a message id that is not first
	for _, tc := range []struct {
		state        string
		live, shadow bool // gone for a live send, for a shadow send
	}{
		{"draft", false, false}, {"approved", false, false}, {"refused", false, false}, {"cancelled", false, false},
		{"sending", true, true}, {"sent", true, true}, {"maybe_sent", true, true},
	} {
		set(first, tc.state)
		for _, shadow := range []bool{false, true} {
			b, gone, err := f.q.FirstEmailGone(ctx, lead, other, shadow)
			want := tc.live
			if shadow {
				want = tc.shadow
			}
			if err != nil || gone != want || (gone && b != live) {
				t.Errorf("%s, shadow send %v: %d %v %v, want %v", tc.state, shadow, b, gone, err, want)
			}
		}
		if _, gone, err := f.q.FirstEmailGone(ctx, lead, first, false); err != nil || gone {
			t.Errorf("%s: the message itself counted (%v %v)", tc.state, gone, err)
		}
	}
	// A shadow batch's first email written to the outbox: gone for a shadow send only.
	set(first, "cancelled")
	if _, err := owner.Exec(ctx, `UPDATE outreach_batch SET shadow = true WHERE id = $1`, live); err != nil {
		t.Fatal(err)
	}
	set(first, "shadow_sent")
	if _, gone, err := f.q.FirstEmailGone(ctx, lead, other, false); err != nil || gone {
		t.Errorf("a shadow first email stops a live send (%v %v)", gone, err)
	}
	if b, gone, err := f.q.FirstEmailGone(ctx, lead, other, true); err != nil || !gone || b != live {
		t.Errorf("shadow send: %d %v %v, want gone in %d", b, gone, err, live)
	}
}
