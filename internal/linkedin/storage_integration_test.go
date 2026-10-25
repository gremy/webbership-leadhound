//go:build integration

package linkedin_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/linkedin"
)

const diskNote = "disk 13% free, under the 15% floor"

// storageAfter is a storage check that reports over once it has been asked more than calls
// times.
func storageAfter(calls int32, asked *atomic.Int32) func(context.Context) (bool, string, error) {
	return func(context.Context) (bool, string, error) {
		if asked.Add(1) > calls {
			return true, diskNote, nil
		}
		return false, "", nil
	}
}

// TestCollectStorageOverAtStart: over the storage budget before the first search page,
// nothing is requested, and the run is recorded with stop reason disk_budget and the reason.
func TestCollectStorageOverAtStart(t *testing.T) {
	s := openStore(t)
	ft := fixtures(t, testdata)
	var asked atomic.Int32
	c := collector(t, s, ft, time.Now, &bytes.Buffer{})
	c.Storage = storageAfter(0, &asked)
	res, err := c.Run(context.Background(), demo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != linkedin.StopDiskBudget || res.Requests != 0 || ft.HitsFor("") != 0 {
		t.Fatalf("result %+v, %d hits; want disk_budget and no request", res, ft.HitsFor(""))
	}
	r := latestRun(t, linkedin.CommandCollect)
	if r.ID != res.RunID || r.StopReason != "disk_budget" || !strings.Contains(r.Note, diskNote) {
		t.Errorf("run row %+v, want disk_budget with the reason", r)
	}
	if got := count(t, `SELECT count(*) FROM job`); got != 0 {
		t.Errorf("jobs = %d, want 0", got)
	}
}

// TestCollectStorageOverBeforeASearchPage: the budget is asked before each search page;
// over at the second ask, the first page's cards are kept and nothing more is requested,
// details included.
func TestCollectStorageOverBeforeASearchPage(t *testing.T) {
	s := openStore(t)
	ft := fixtures(t, testdata)
	var asked atomic.Int32
	c := collector(t, s, ft, time.Now, &bytes.Buffer{})
	c.Storage = storageAfter(1, &asked)
	res, err := c.Run(context.Background(), demo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != linkedin.StopDiskBudget || asked.Load() != 2 {
		t.Fatalf("stop %q after %d asks, want disk_budget after 2", res.StopReason, asked.Load())
	}
	if len(requestedURLs(t, "search")) != 1 || ft.HitsFor("jobPosting") != 0 || res.Searches != 1 {
		t.Errorf("searches %d (%d parsed), details %d; want 1, 1, 0", len(requestedURLs(t, "search")), res.Searches, ft.HitsFor("jobPosting"))
	}
	if got := count(t, `SELECT count(*) FROM job`); got == 0 {
		t.Errorf("the first page's cards were not kept")
	}
}

// TestCollectStorageCheckError: a budget that cannot be read fails the run like a store
// error; the run row is still finished, with the failure in its note.
func TestCollectStorageCheckError(t *testing.T) {
	s := openStore(t)
	ft := fixtures(t, testdata)
	boom := errors.New("size unreadable")
	c := collector(t, s, ft, time.Now, &bytes.Buffer{})
	c.Storage = func(context.Context) (bool, string, error) { return false, "", boom }
	_, err := c.Run(context.Background(), demo)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the check's error", err)
	}
	if ft.HitsFor("") != 0 {
		t.Errorf("%d requests, want 0", ft.HitsFor(""))
	}
	if r := latestRun(t, linkedin.CommandCollect); !strings.Contains(r.Note, "size unreadable") {
		t.Errorf("run note %q, want the failure", r.Note)
	}
}
