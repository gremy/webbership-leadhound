package ats

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// timeoutErr is a network error that timed out, as a dial or read deadline gives.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// TestFailedFetch: an answer with a bad status is a miss; no answer at all (too large, a
// timeout, a network error) is no verdict; the breaker stops the ATS; a done context and
// the live gate stop the command.
func TestFailedFetch(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	ok := context.Background()
	wrap := func(err error) error { return fmt.Errorf("fetch: board x: %w", err) }
	for _, tc := range []struct {
		name          string
		ctx           context.Context
		status        int
		err           error
		outcome, stop string
		fatal         bool
	}{
		{"5xx", ok, 503, wrap(fetch.ErrStatus), store.CheckError, "", false},
		{"403", ok, 403, wrap(fetch.ErrStatus), store.CheckError, "", false},
		{"too large", ok, 200, wrap(fetch.ErrBodyTooLarge), store.CheckNoVerdict, "", false},
		{"client timeout", ok, 0, wrap(context.DeadlineExceeded), store.CheckNoVerdict, "", false},
		{"net timeout", ok, 0, wrap(timeoutErr{}), store.CheckNoVerdict, "", false},
		{"network error", ok, 0, wrap(errors.New("connection refused")), store.CheckNoVerdict, "", false},
		{"breaker", ok, 429, &fetch.OpenError{Reason: fetch.StopThrottled}, "", string(fetch.StopThrottled), false},
		{"caller done", done, 0, wrap(context.Canceled), "", "", true},
		{"caller done mid-status", done, 503, wrap(fetch.ErrStatus), "", "", true},
		{"live off", ok, 0, wrap(fetch.ErrLiveOff), "", "", true},
		{"other", ok, 200, errors.New("store: record fetch: boom"), "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, stop, fatal := FailedFetch(tc.ctx, fetch.Response{Status: tc.status}, tc.err)
			if outcome != tc.outcome || stop != tc.stop || (fatal != nil) != tc.fatal {
				t.Errorf("= %q, %q, %v; want %q, %q, fatal %v", outcome, stop, fatal, tc.outcome, tc.stop, tc.fatal)
			}
		})
	}
}
