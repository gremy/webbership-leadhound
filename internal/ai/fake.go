package ai

import (
	"context"
	"fmt"
	"sync"
)

// Fake answers from a script, for tests and for running without a key. ByState answers win
// over Answers for that state; a question with no scripted answer is ErrNotScripted. A Fake
// is safe for concurrent use.
type Fake struct {
	Answers map[string]Answer            // by question name
	ByState map[string]map[string]Answer // state → question name → answer
	Usage   Usage                        // reported on every successful call
	Err     error                        // when set, every call returns it

	mu    sync.Mutex
	calls int
}

// FakeName is the Fake's name and provider, as decision.decider and ai_usage store it.
const FakeName = "fake"

// Name is FakeName.
func (*Fake) Name() string { return FakeName }

// Provider is FakeName, so fake calls never count against a real provider's cap.
func (*Fake) Provider() string { return FakeName }

// Calls returns how many times Decide has been called.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Decide returns the scripted answer for each question.
func (f *Fake) Decide(_ context.Context, state string, qs []Question) (map[string]Answer, Usage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.Err != nil {
		return nil, Usage{}, f.Err
	}
	if err := checkQuestions(qs); err != nil {
		return nil, Usage{}, err
	}
	out := make(map[string]Answer, len(qs))
	for _, q := range qs {
		a, ok := f.ByState[state][q.Name]
		if !ok {
			a, ok = f.Answers[q.Name]
		}
		if !ok {
			return nil, Usage{}, fmt.Errorf("fake: question %q: %w", q.Name, ErrNotScripted)
		}
		if !hasOption(q, a.Choice) {
			return nil, Usage{}, fmt.Errorf("fake: question %q: scripted choice %q is not an option", q.Name, a.Choice)
		}
		out[q.Name] = a
	}
	return out, f.Usage, nil
}
