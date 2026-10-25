package probe

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// stopAfter is a Deps.Stop that reports over once it has been asked more than calls times.
func stopAfter(calls int32, asked *atomic.Int32) func(context.Context) (bool, string, error) {
	return func(context.Context) (bool, string, error) {
		if asked.Add(1) > calls {
			return true, "disk 13% free, under the 15% floor", nil
		}
		return false, "", nil
	}
}

// Over the storage budget before the first site: nothing is requested or stored, and the
// run stops with disk_budget and the reason.
func TestRunStorageOverAtStart(t *testing.T) {
	rt := &scripted{}
	var asked atomic.Int32
	saved := 0
	sum, err := Run(context.Background(), Deps{NewClient: scriptedFactory(rt), Recorder: &fakeRecorder{},
		Save: func(context.Context, Write) error { saved++; return nil },
		Stop: stopAfter(0, &asked)}, manySites(5, "example"), Options{Concurrency: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.StopReason != StopDiskBudget || sum.StopNote != "disk 13% free, under the 15% floor" {
		t.Errorf("stop %q (%q), want disk_budget with the floor note", sum.StopReason, sum.StopNote)
	}
	if rt.total() != 0 || saved != 0 || sum.Sites != 0 {
		t.Errorf("requests %d, saved %d, sites %d; want nothing", rt.total(), saved, sum.Sites)
	}
}

// The budget is asked again every StorageCheckEvery sites: over at the second ask, the run
// stores exactly the sites handed out before it, whatever the concurrency.
func TestRunStorageOverAfterCheckEvery(t *testing.T) {
	for _, conc := range []int{1, 8} {
		t.Run(strconv.Itoa(conc), func(t *testing.T) {
			rt := &scripted{}
			var asked atomic.Int32
			sum, w := run2(t, Deps{NewClient: scriptedFactory(rt), Stop: stopAfter(1, &asked)},
				manySites(StorageCheckEvery+50, "example"), conc)
			if sum.StopReason != StopDiskBudget || asked.Load() != 2 {
				t.Fatalf("stop %q after %d asks, want disk_budget after 2", sum.StopReason, asked.Load())
			}
			if sum.Sites != StorageCheckEvery || len(w) != StorageCheckEvery || sum.Interrupted != 0 {
				t.Errorf("stored %d (%d writes, %d cut short), want %d", sum.Sites, len(w), sum.Interrupted, StorageCheckEvery)
			}
		})
	}
}

// Under the budget the whole list is probed, the budget asked once per StorageCheckEvery.
func TestRunStorageUnder(t *testing.T) {
	var asked atomic.Int32
	sum, w := run2(t, Deps{NewClient: scriptedFactory(&scripted{}), Stop: stopAfter(1000, &asked)},
		manySites(2*StorageCheckEvery+1, "example"), 4)
	if sum.StopReason != "" || sum.StopNote != "" || len(w) != 2*StorageCheckEvery+1 || asked.Load() != 3 {
		t.Errorf("stop %q, stored %d, asked %d; want none, %d, 3", sum.StopReason, len(w), asked.Load(), 2*StorageCheckEvery+1)
	}
}

// A budget that cannot be read fails the run like a store error, before anything is asked.
func TestRunStorageCheckError(t *testing.T) {
	rt := &scripted{}
	boom := errors.New("size unreadable")
	saved := 0
	_, err := Run(context.Background(), Deps{NewClient: scriptedFactory(rt), Recorder: &fakeRecorder{},
		Save: func(context.Context, Write) error { saved++; return nil },
		Stop: func(context.Context) (bool, string, error) { return false, "", boom }},
		manySites(5, "example"), Options{Concurrency: 2})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the check's error", err)
	}
	if rt.total() != 0 || saved != 0 {
		t.Errorf("requests %d, saved %d; want none", rt.total(), saved)
	}
}

// run2 is run with Deps given: the recorder and Save are filled in.
func run2(t *testing.T, d Deps, list []leadstore.Site, conc int) (Summary, map[int64]Write) {
	t.Helper()
	writes := map[int64]Write{}
	d.Recorder = &fakeRecorder{}
	d.Save = func(_ context.Context, w Write) error { writes[w.SiteID] = w; return nil }
	sum, err := Run(context.Background(), d, list, Options{Concurrency: conc})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum, writes
}
