package campaign

import (
	"slices"
	"time"
)

// The sequence engine: given one lead's sequence, its drafted rows, the call outcomes recorded
// on them and a business-day calendar, Next says which pending row falls due when and which
// pending rows are cancelled and why. It is pure: no store, no clock (now is a parameter).
//
// The engine walks the steps in order, keeping the anchor (the time of the last step sent or
// completed) and the latest call outcome:
//   - a step with no row is absent (no callable phone at draft time) and is passed over;
//   - a row sent or shadow-sent (email) or with a call outcome (call) is completed: it moves
//     the anchor and, for a call, the outcome the later conditions read;
//   - a row the engine itself cancelled (a reason from EngineReasons) is passed over, so it
//     never moves the anchor: step 4 counts from step 1's send, or from the call outcome;
//   - the first pending row (approved) is decided: its condition fails → cancelled with the
//     reason and the walk goes on; it holds → it falls due after_business_days after the anchor (unless it already
//     has a due time) and the walk stops, since a later step waits for this one to be sent;
//   - any other row (draft, sending, maybe_sent, refused, skipped, a call marked done with no
//     outcome, a row cancelled for any other reason) stops the walk: nothing after it is
//     scheduled, as the legacy rule never chains past a step that was not sent.
//
// The first step is never scheduled here: its due time is set at approval.

// The message states the engine reads (outreach_message_state_check).
const (
	StateApproved   = "approved"
	StateSent       = "sent"
	StateShadowSent = "shadow_sent"
	StateDone       = "done"
	StateCancelled  = "cancelled"
)

// Call outcomes (call_outcome.outcome).
const (
	OutcomeConnected   = "connected"
	OutcomeNoAnswer    = "no_answer"
	OutcomeVoicemail   = "voicemail"
	OutcomeWrongNumber = "wrong_number"
	OutcomeDoNotCall   = "do_not_call"
	OutcomeSkipped     = "skipped"
)

// Outcomes lists every call outcome, in the order the call card shows them.
var Outcomes = []string{OutcomeConnected, OutcomeNoAnswer, OutcomeVoicemail, OutcomeWrongNumber, OutcomeDoNotCall, OutcomeSkipped}

// The reasons the engine cancels a row with (outreach_message.cancel_reason). A reply,
// opt-out, bounce or suppression cancels every pending row of the lead in the store, with the
// store's own reasons; the engine never writes those.
const (
	CancelCondition     = "condition"      // the step's condition fails (e.g. no call was ever connected)
	CancelCallConnected = "call_connected" // the call connected: the sequence ends
	CancelWrongNumber   = "wrong_number"   // the number was wrong: no "tried to reach you", no more calls
	CancelCallSkipped   = "call_skipped"   // the operator skipped the call
	CancelDoNotCall     = "do_not_call"    // asked not to be called: the sequence ends
	CancelNoConsent     = "no_consent"     // connected, but no agreement to get an email
)

// EngineReasons are the cancel reasons Next writes; a row cancelled with one of them is
// passed over on the next walk, any other cancelled row stops it.
var EngineReasons = []string{CancelCondition, CancelCallConnected, CancelWrongNumber, CancelCallSkipped, CancelDoNotCall, CancelNoConsent}

// Row is one drafted step of a lead's sequence, as the store holds it.
type Row struct {
	MessageID    int64
	Step         int
	State        string     // outreach_message.state
	CancelReason string     // outreach_message.cancel_reason, for a cancelled row
	SentAt       *time.Time // when an email was sent or shadow-sent
	DueAt        *time.Time // nil until scheduled
}

// Outcome is one recorded call outcome; the latest per message wins.
type Outcome struct {
	MessageID    int64
	Outcome      string
	EmailConsent bool // connected, and they agreed to get an email
	At           time.Time
}

// Decision is what the store applies: due times (set only on a row with none yet) and
// cancels with their reason.
type Decision struct {
	Due    map[int64]time.Time
	Cancel map[int64]string
}

// Calendar turns an anchor and a count of business days into a due time; the zero time means
// no slot was found and the row stays unscheduled. The caller builds it from the lead's
// location and the business hours (outreach.FollowupDue).
type Calendar interface {
	After(from time.Time, businessDays int) time.Time
}

// CalendarFunc adapts a function to Calendar.
type CalendarFunc func(from time.Time, businessDays int) time.Time

// After calls f.
func (f CalendarFunc) After(from time.Time, businessDays int) time.Time { return f(from, businessDays) }

// LegacySequence is today's three-step all-email sequence: step 1 followupBusinessDays[0]
// business days after step 0, step 2 [1] after step 1. New batches of the legacy campaign
// store it explicitly, so they run through Next.
func LegacySequence(followupBusinessDays []int) []Step {
	seq := make([]Step, 3)
	for i := range seq {
		seq[i] = Step{Step: i, Type: StepEmail, When: WhenAlways}
		if i > 0 && i-1 < len(followupBusinessDays) {
			seq[i].AfterBusinessDays = followupBusinessDays[i-1]
		}
	}
	return seq
}

// callState is what the walk knows about the calls made so far.
type callState struct {
	last    string // the latest call's outcome; "" before any call completed
	consent bool   // a connected call came with an agreement to get an email
}

// Next decides one lead's sequence. now anchors a completed step whose time is missing.
func Next(seq []Step, rows []Row, outcomes []Outcome, now time.Time, cal Calendar) Decision {
	d := Decision{Due: map[int64]time.Time{}, Cancel: map[int64]string{}}

	byStep := make(map[int]Row, len(rows))
	for _, r := range rows {
		byStep[r.Step] = r
	}
	latest := map[int64]Outcome{}
	for _, o := range outcomes {
		if cur, ok := latest[o.MessageID]; !ok || !o.At.Before(cur.At) {
			latest[o.MessageID] = o
		}
	}

	var (
		anchor    time.Time
		anchored  bool
		calls     callState
		completed = func(t time.Time) {
			if t.IsZero() {
				t = now
			}
			anchor, anchored = t, true
		}
	)

	for _, st := range seq {
		row, ok := byStep[st.Step]
		if !ok {
			continue // absent: no callable phone at draft time
		}
		o, hasOutcome := latest[row.MessageID]

		switch {
		case st.Type == StepCall && hasOutcome && row.State != StateCancelled:
			completed(o.At)
			calls.last = o.Outcome
			if o.Outcome == OutcomeConnected && o.EmailConsent {
				calls.consent = true
			}
			continue
		case st.Type == StepEmail && (row.State == StateSent || row.State == StateShadowSent):
			var t time.Time
			if row.SentAt != nil {
				t = *row.SentAt
			}
			completed(t)
			continue
		case row.State == StateCancelled && slices.Contains(EngineReasons, row.CancelReason):
			continue
		case row.State != StateApproved:
			return d // in flight, refused, skipped or cancelled by the store: stop here
		}

		// A pending row: decide it.
		if cancel, reason := decide(st, calls); cancel {
			d.Cancel[row.MessageID] = reason
			continue
		}
		if !anchored || row.DueAt != nil {
			return d // the first step (due set at approval), or already scheduled
		}
		if due := cal.After(anchor, st.AfterBusinessDays); !due.IsZero() {
			d.Due[row.MessageID] = due
		}
		return d
	}
	return d
}

// decide reads a pending step against the calls made so far. A call that ended the sequence
// (connected, do_not_call) cancels every later step bar an email_consent one, which follows
// the consent; a bad number (wrong_number, do_not_call) cancels every later call.
func decide(st Step, calls callState) (cancel bool, reason string) {
	switch calls.last {
	case OutcomeDoNotCall:
		return true, CancelDoNotCall
	case OutcomeConnected:
		if st.When == WhenEmailConsent {
			if calls.consent {
				return false, ""
			}
			return true, CancelNoConsent
		}
		return true, CancelCallConnected
	case OutcomeWrongNumber:
		if st.Type == StepCall || st.When == WhenCallNotConnected {
			return true, CancelWrongNumber
		}
	case OutcomeSkipped:
		if st.When == WhenCallNotConnected {
			return true, CancelCallSkipped
		}
	}

	switch st.When {
	case WhenCallNotConnected:
		switch calls.last {
		case OutcomeNoAnswer, OutcomeVoicemail:
			return false, ""
		default:
			// No call completed before it (the call row was passed over), so there is
			// nothing to say "tried to reach you" about.
			return true, CancelCondition
		}
	case WhenEmailConsent:
		if calls.consent {
			return false, ""
		}
		// Never connected (no answer twice, voicemail, skipped, wrong number, no call):
		// no consent was given, so the email steps end.
		return true, CancelCondition
	}
	// always, has_callable_phone (decided at draft time: the row exists), or empty.
	return false, ""
}
