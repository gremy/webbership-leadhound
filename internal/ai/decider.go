// Package ai asks a model typed questions about a job posting and gets typed answers back.
//
// A Decider never writes text: every question is a choice among named options, and every
// answer is the chosen option with its probabilities. Jev is the TypeSafe client, Fake the
// scripted stand-in for tests and keyless runs, and Capped the daily spend cap around either.
package ai

import (
	"context"
	"errors"
	"fmt"
)

// Decider makes typed decisions about one state; it never writes text.
type Decider interface {
	Name() string // e.g. "jev-1.13.0"
	Decide(ctx context.Context, state string, qs []Question) (map[string]Answer, Usage, error)
}

// Question is a "choice" question: the model picks one of Options.
type Question struct {
	Name, Instructions string
	Options            []Option
}

// Option is one named choice and what it means.
type Option struct{ Name, Description string }

// Answer is the chosen option, the model's confidence in it, and the probability of every
// option by name.
type Answer struct {
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

// Usage is what one call cost. Only input tokens are billed.
type Usage struct{ InputTokens int }

var (
	// ErrTooLong means the state and questions are longer than the model's context.
	ErrTooLong = errors.New("ai: request longer than the model's context")
	// ErrUnavailable means the provider stayed rate-limited or overloaded through every try.
	ErrUnavailable = errors.New("ai: provider unavailable")
	// ErrSpendCap means today's call cap is reached; no call was made.
	ErrSpendCap = errors.New("ai: daily call cap reached")
	// ErrNotScripted means a Fake has no answer for a question.
	ErrNotScripted = errors.New("ai: no scripted answer")
	// ErrBadAnswer means the provider answered (and billed) the call, but the answer could
	// not be used: it did not decode, missed a question or named an unknown option. The
	// Usage returned with it is what the call cost, so the cap still counts it.
	ErrBadAnswer = errors.New("ai: unusable answer to a billed call")
	// ErrUsageStore means the cap could not read or record usage. A caller must stop: going
	// on would make calls the cap cannot count.
	ErrUsageStore = errors.New("ai: usage store")
)

// checkQuestions rejects a batch the provider would refuse: no questions, a question with
// no name or no options, and duplicate question or option names.
func checkQuestions(qs []Question) error {
	if len(qs) == 0 {
		return errors.New("ai: no questions")
	}
	seen := make(map[string]bool, len(qs))
	for i, q := range qs {
		if q.Name == "" {
			return fmt.Errorf("ai: question %d has no name", i)
		}
		if seen[q.Name] {
			return fmt.Errorf("ai: question %q appears twice", q.Name)
		}
		seen[q.Name] = true
		if len(q.Options) == 0 || len(q.Options) > 255 {
			return fmt.Errorf("ai: question %q has %d options, want 1 to 255", q.Name, len(q.Options))
		}
		opts := make(map[string]bool, len(q.Options))
		for _, o := range q.Options {
			if o.Name == "" {
				return fmt.Errorf("ai: question %q has an option with no name", q.Name)
			}
			if opts[o.Name] {
				return fmt.Errorf("ai: question %q has option %q twice", q.Name, o.Name)
			}
			opts[o.Name] = true
		}
	}
	return nil
}

// hasOption reports whether q offers an option named name.
func hasOption(q Question, name string) bool {
	for _, o := range q.Options {
		if o.Name == name {
			return true
		}
	}
	return false
}
