package campaign

import (
	"testing"
	"time"
)

// TestSequenceCallPassedOver: a call row that was skipped at approval (its number suppressed
// between batch new and approval) or cancelled by a phone suppression or a wrong number found
// on another lead is passed over the way an absent call row is: step 3 ("tried to reach you")
// has no call to speak of and is cancelled, step 4 counts from step 1's send (the CP3 rule).
// An email row skipped or cancelled by the store still stops the walk.
func TestSequenceCallPassedOver(t *testing.T) {
	seq := fiveSteps(t)
	call := func(state, reason string) Row {
		return Row{MessageID: 12, Step: 2, State: state, CancelReason: reason}
	}
	passed := []struct {
		name string
		row  Row
	}{
		{"skipped at approval: suppressed", call("skipped", "suppressed")},
		{"cancelled by a phone suppression (do_not_call)", call(StateCancelled, "suppressed: do_not_call")},
		{"cancelled by a phone suppression (tps)", call(StateCancelled, "suppressed: tps")},
		{"cancelled by a phone suppression (manual)", call(StateCancelled, "suppressed: manual")},
		{"cancelled: wrong number on another lead", call(StateCancelled, CancelWrongNumber)},
	}
	for _, tc := range passed {
		t.Run(tc.name, func(t *testing.T) {
			rows := []Row{sent(10, 0, t0), sent(11, 1, t1), tc.row, pending(13, 3), pending(14, 4)}
			checkDecision(t, Next(seq, rows, nil, t3, dayCal),
				map[int64]time.Time{14: t1.AddDate(0, 0, 5)}, map[int64]string{13: CancelCondition})
		})
	}
	stops := []struct {
		name string
		rows []Row
	}{
		{"an email row cancelled by a suppression stops the walk",
			[]Row{sent(10, 0, t0), {MessageID: 11, Step: 1, State: StateCancelled, CancelReason: "suppressed: opt_out"}, pending(14, 4)}},
		{"a call skipped by the operator stops the walk",
			[]Row{sent(10, 0, t0), sent(11, 1, t1), call("skipped", "skipped at approval"), pending(13, 3), pending(14, 4)}},
	}
	for _, tc := range stops {
		t.Run(tc.name, func(t *testing.T) {
			checkDecision(t, Next(seq, tc.rows, nil, t3, dayCal), nil, nil)
		})
	}
}
