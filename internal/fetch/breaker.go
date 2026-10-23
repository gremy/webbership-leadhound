package fetch

import (
	"fmt"
	"sync"
)

// StopReason says why a Breaker opened. The values match run.stop_reason.
type StopReason string

// The reasons a Breaker opens.
const (
	StopThrottled   StopReason = "throttled"    // two 429s in a row
	StopBlocked     StopReason = "blocked"      // a status or redirect the Profile names as blocked
	StopFailureRate StopReason = "failure_rate" // at least minRequests, over half of them failed
	StopBudget      StopReason = "budget"       // a kind's budget is spent
)

const (
	throttleAfter = 2 // consecutive 429s that open the breaker
	minRequests   = 5 // requests before the failure rate is judged
)

// Budgets caps the requests per run for each kind. A kind not in the map is unlimited;
// a kind mapped to 0 may not be fetched at all. Retries count, since each one is a request.
type Budgets map[Kind]int

// OpenError is returned once a Breaker is open. It matches ErrBreakerOpen with errors.Is,
// and also wraps the error of the request that opened it, when there was one.
type OpenError struct {
	Reason StopReason
	Kind   Kind  // the kind whose budget is spent; set only for StopBudget
	Err    error // what the opening request returned; nil on later calls
}

func (e *OpenError) Error() string {
	msg := fmt.Sprintf("%s: %s", ErrBreakerOpen, e.Reason)
	if e.Kind != "" {
		msg += " (" + string(e.Kind) + ")"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *OpenError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrBreakerOpen}
	}
	return []error{ErrBreakerOpen, e.Err}
}

// Breaker decides when a run stops fetching. Share one per run between every Client that
// fetches from the same source. Throttled, blocked and failure_rate stop every kind; a spent
// budget stops only its own kind, so a large search matrix cannot starve detail fetches.
// It is safe for concurrent use.
type Breaker struct {
	mu          sync.Mutex
	reason      StopReason
	budgets     Budgets
	used        map[Kind]int
	spent       map[Kind]bool
	requests    int
	failures    int
	consecutive int // 429s in a row
}

// NewBreaker returns a closed Breaker with the given budgets (nil: unlimited).
func NewBreaker(budgets Budgets) *Breaker {
	b := &Breaker{budgets: Budgets{}, used: map[Kind]int{}, spent: map[Kind]bool{}}
	for k, n := range budgets {
		b.budgets[k] = n
	}
	return b
}

// Reason is why the breaker opened: throttled, blocked or failure_rate when every kind is
// stopped, else budget when any kind's budget is spent, else "".
func (b *Breaker) Reason() StopReason {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reason != "" {
		return b.reason
	}
	if len(b.spent) > 0 {
		return StopBudget
	}
	return ""
}

// Requests is how many requests went out (run.requests).
func (b *Breaker) Requests() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests
}

// Failures is how many of them were transport errors or 5xx responses (run.failures).
func (b *Breaker) Failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

// Used is how much of kind's budget is spent.
func (b *Breaker) Used(kind Kind) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used[kind]
}

// take reserves one request of kind, or says why none may go out.
func (b *Breaker) take(kind Kind) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reason != "" {
		return &OpenError{Reason: b.reason}
	}
	if limit, ok := b.budgets[kind]; ok && b.used[kind] >= limit {
		b.spent[kind] = true
		return &OpenError{Reason: StopBudget, Kind: kind}
	}
	b.used[kind]++
	return nil
}

// open opens the breaker for reason unless it is already open, and returns the reason it is
// open for.
func (b *Breaker) open(reason StopReason) StopReason {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reason == "" {
		b.reason = reason
	}
	return b.reason
}

// outcome is how one request ended, for the breaker's counts.
type outcome int

const (
	outcomeOK       outcome = iota // any response the breaker does not count against the run
	outcomeNeutral                 // no verdict: the caller cancelled, or the live gate refused a redirect
	outcomeFailure                 // a transport error or a 5xx
	outcomeThrottle                // a 429
	outcomeBlocked                 // a status or final path the Profile names as blocked
)

// observe counts one request that went out and returns the reason the breaker opened on
// it, or "" when it did not open now.
func (b *Breaker) observe(o outcome) StopReason {
	b.mu.Lock()
	defer b.mu.Unlock()
	if o == outcomeNeutral {
		return ""
	}
	b.requests++
	if o == outcomeThrottle {
		b.consecutive++
	} else {
		b.consecutive = 0
	}
	if o == outcomeFailure {
		b.failures++
	}
	if b.reason != "" {
		return ""
	}
	switch {
	case o == outcomeBlocked:
		b.reason = StopBlocked
	case b.consecutive >= throttleAfter:
		b.reason = StopThrottled
	case b.requests >= minRequests && b.failures*2 > b.requests:
		b.reason = StopFailureRate
	default:
		return ""
	}
	return b.reason
}
