package console

import (
	"context"
	"time"
)

// statusFake is the Status part of fakeBackend.
type statusFake struct {
	status     Status
	statusErr  error // returned by Status
	refreshOut ControlOutcome
	refreshErr error // returned by RefreshStatus
	topupOut   ControlOutcome
	topupErr   error // returned by AddClaudeTopup
	topups     []topupCall
}

// topupCall is one AddClaudeTopup call.
type topupCall struct {
	Cents int64
	At    time.Time
}

func (f *fakeBackend) Status(context.Context) (Status, error) {
	f.count("Status")
	return f.status, f.statusErr
}

func (f *fakeBackend) RefreshStatus(context.Context) (ControlOutcome, error) {
	f.count("RefreshStatus")
	return f.refreshOut, f.refreshErr
}

func (f *fakeBackend) AddClaudeTopup(_ context.Context, cents int64, at time.Time) (ControlOutcome, error) {
	f.count("AddClaudeTopup")
	f.mu.Lock()
	f.topups = append(f.topups, topupCall{cents, at})
	f.mu.Unlock()
	return f.topupOut, f.topupErr
}
