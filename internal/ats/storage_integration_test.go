//go:build integration

package ats_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/fetch"
)

const diskNote = "database 8.2 GB over its 8 GB budget"

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

// emptyBoards are n Greenhouse boards, each answering an empty list.
func emptyBoards(n int) ([]ats.Board, *stubRT) {
	boards := make([]ats.Board, n)
	for i := range boards {
		boards[i] = ats.Board{ATS: ats.Greenhouse, Board: fmt.Sprintf("board%03d", i), Name: fmt.Sprintf("Board %d", i)}
	}
	return boards, &stubRT{fn: func(*http.Request) (int, string) { return http.StatusOK, `{"jobs": []}` }}
}

// TestCollectStorageOverAtStart: over the storage budget before the first board, nothing
// is requested, and each ATS's run is recorded with stop reason disk_budget and the reason.
func TestCollectStorageOverAtStart(t *testing.T) {
	s := openStore(t)
	boards, rt := emptyBoards(3)
	var asked atomic.Int32
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 100}, &bytes.Buffer{})
	c.Storage = storageAfter(0, &asked)
	res := runBoards(t, c, boards...)
	a := byATS(t, res, ats.Greenhouse)
	if a.StopReason != ats.StopDiskBudget || a.Requests != 0 || rt.requests("") != 0 {
		t.Fatalf("stop %q, %d requests (%d sent); want disk_budget and none", a.StopReason, a.Requests, rt.requests(""))
	}
	if !strings.Contains(a.Note, diskNote) {
		t.Errorf("note %q does not say why", a.Note)
	}
	if got := count(t, `SELECT count(*) FROM run WHERE id = $1 AND stop_reason = 'disk_budget' AND finished_at IS NOT NULL AND note LIKE '%' || $2 || '%'`, a.RunID, diskNote); got != 1 {
		t.Errorf("run row not recorded with disk_budget and the reason")
	}
	if got := count(t, `SELECT count(*) FROM http_fetch`); got != 0 {
		t.Errorf("%d fetches recorded, want 0", got)
	}
}

// TestCollectStorageOverAfterCheckEvery: the budget is asked again every StorageCheckEvery
// boards; over at the second ask, the run stops there and the rest are not requested.
func TestCollectStorageOverAfterCheckEvery(t *testing.T) {
	s := openStore(t)
	boards, rt := emptyBoards(ats.StorageCheckEvery + 10)
	var asked atomic.Int32
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 1000}, &bytes.Buffer{})
	c.Storage = storageAfter(1, &asked)
	a := byATS(t, runBoards(t, c, boards...), ats.Greenhouse)
	if a.StopReason != ats.StopDiskBudget || asked.Load() != 2 {
		t.Fatalf("stop %q after %d asks, want disk_budget after 2", a.StopReason, asked.Load())
	}
	if rt.requests("") != ats.StorageCheckEvery || a.BoardsOK != ats.StorageCheckEvery {
		t.Errorf("%d requests, %d boards ok; want %d", rt.requests(""), a.BoardsOK, ats.StorageCheckEvery)
	}
}

// TestCollectStorageUnder: under the budget every board is collected and the run stops for
// nothing.
func TestCollectStorageUnder(t *testing.T) {
	s := openStore(t)
	boards, rt := emptyBoards(ats.StorageCheckEvery + 1)
	var asked atomic.Int32
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 1000}, &bytes.Buffer{})
	c.Storage = storageAfter(100, &asked)
	a := byATS(t, runBoards(t, c, boards...), ats.Greenhouse)
	if a.StopReason != "" || rt.requests("") != len(boards) || asked.Load() != 2 {
		t.Errorf("stop %q, %d requests, %d asks; want none, %d, 2", a.StopReason, rt.requests(""), asked.Load(), len(boards))
	}
}

// TestCollectStorageCheckError: a budget that cannot be read fails the run like a store
// error; the run row is still finished, with the failure in its note.
func TestCollectStorageCheckError(t *testing.T) {
	s := openStore(t)
	boards, rt := emptyBoards(2)
	boom := errors.New("size unreadable")
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 100}, &bytes.Buffer{})
	c.Storage = func(context.Context) (bool, string, error) { return false, "", boom }
	res, err := c.Run(context.Background(), boards)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the check's error", err)
	}
	if rt.requests("") != 0 {
		t.Errorf("%d requests, want 0", rt.requests(""))
	}
	a := byATS(t, res, ats.Greenhouse)
	if got := count(t, `SELECT count(*) FROM run WHERE id = $1 AND finished_at IS NOT NULL AND note LIKE '%size unreadable%'`, a.RunID); got != 1 {
		t.Errorf("failed run not recorded")
	}
}
