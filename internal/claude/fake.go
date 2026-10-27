package claude

import (
	"context"
	"fmt"
	"sync"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// FakeModel is the Fake's model name. It is priced at zero and Capped never records it, so
// fake calls never count against a real cap.
const FakeModel = "fake"

// Fake answers from a script, for tests and for running without a key. ByLead answers win
// over WriteAnswer for that lead; a Write with neither is ai.ErrNotScripted. A Fake is safe
// for concurrent use.
type Fake struct {
	WriteAnswer    WriteResult
	ByLead         map[int64]WriteResult
	ResearchAnswer ResearchResult
	Usage          Usage // reported on every call; Model "" reads as FakeModel
	Err            error // when set, every call returns it with Usage

	mu    sync.Mutex
	calls int
}

var _ Caller = (*Fake)(nil)

// Calls returns how many times Write or Research has been called.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *Fake) usage() Usage {
	u := f.Usage
	if u.Model == "" {
		u.Model = FakeModel
	}
	return u
}

// Write returns the scripted answer for r.LeadID, else WriteAnswer.
func (f *Fake) Write(_ context.Context, r WriteRequest) (WriteResult, Usage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.Err != nil {
		return WriteResult{}, f.usage(), f.Err
	}
	a, ok := f.ByLead[r.LeadID]
	if !ok {
		a = f.WriteAnswer
	}
	if a.JSON == nil {
		return WriteResult{}, Usage{}, fmt.Errorf("claude fake: lead %d: %w", r.LeadID, ai.ErrNotScripted)
	}
	return a, f.usage(), nil
}

// Research returns ResearchAnswer.
func (f *Fake) Research(_ context.Context, _ ResearchRequest) (ResearchResult, Usage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.Err != nil {
		return ResearchResult{}, f.usage(), f.Err
	}
	return f.ResearchAnswer, f.usage(), nil
}
