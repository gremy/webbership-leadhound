//go:build scale

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestScaleConsolePages: the leads page, a batch's full page and a job's page, each read
// whole through the handler as the operator's browser reads it, on the jobs and leads scale
// seeds as the app role, within a second each; the dashboard, which reads the leads card
// live, within its half second with leads set up. The queue and priorities pages are timed
// and logged: their cost is NextUp's, which the leads scale test budgets. Nothing is written.
func TestScaleConsolePages(t *testing.T) {
	info := leadstoretest.ScaleSeeded(t)
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.ScaleDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping scale test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	// The seed lives on reserved names, which only a test lets into the queue.
	saved := leadsMail
	t.Cleanup(func() { leadsMail = saved })
	leadsMail.allowReserved = true
	base, stop := context.WithCancel(ctx)
	b := newBackend(base, s, prof, info.Loc, config.Config{DataDir: examplesDir})
	t.Cleanup(func() { stop(); b.wait() })
	day := info.Day.Add(9 * time.Hour)
	b.now = func() time.Time { return day }

	a, err := console.NewAuth("operator@mail.example", testHash(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	h := console.New(b, a, console.Options{Dev: true, Location: info.Loc})

	var batchID, msgs, jobID int64
	if err := s.Pool().QueryRow(ctx,
		`SELECT batch_id, count(*) FROM outreach_message GROUP BY batch_id HAVING count(*) >= 10
		 ORDER BY count(*), batch_id LIMIT 1`).Scan(&batchID, &msgs); err != nil {
		t.Fatalf("no batch of 10 messages or more in the seed: %v", err)
	}
	if err := s.Pool().QueryRow(ctx, `SELECT max(job_id) FROM job_observation`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		path, want string
		budget     time.Duration // 0: timed and logged only
	}{
		{"/leads", "Batches waiting for approval", time.Second},
		{fmt.Sprintf("/leads/batches/%d", batchID), fmt.Sprintf("Batch %d", batchID), time.Second},
		{fmt.Sprintf("/jobs/%d", jobID), "<dt>Apply link</dt>", time.Second},
		{"/leads/next", "Next leads", 0},
		{"/leads/priorities", "The top 20", 0},
	} {
		r := httptest.NewRequest(http.MethodGet, p.path, nil)
		r.Header.Set("Cookie", "lh_session="+token)
		w := httptest.NewRecorder()
		peak, took := measure(t, func() error {
			h.ServeHTTP(w, r)
			return nil
		})
		if p.budget == 0 {
			t.Logf("%s: %v, peak heap %.1f MB (NextUp's cost; not budgeted here)", p.path, took.Round(time.Millisecond), float64(peak)/(1<<20))
		} else {
			budgets(t, p.path, peak, took, p.budget)
		}
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), p.want) {
			t.Errorf("%s: %d, %d bytes, lacks %q", p.path, w.Code, w.Body.Len(), p.want)
		}
		if strings.HasPrefix(p.path, "/leads/batches/") {
			t.Logf("%s holds %d messages", p.path, msgs)
		}
	}

	// The dashboard with a snapshot in hand: its live parts, the leads card among them.
	b.wait()
	if _, err := b.Dashboard(ctx); err != nil {
		t.Fatal(err)
	}
	b.wait()
	var d console.Dashboard
	peak, took := measure(t, func() error {
		var err error
		d, err = b.Dashboard(ctx)
		return err
	})
	budgets(t, "dashboard with leads", peak, took, 500*time.Millisecond)
	if d.Leads == nil {
		t.Error("the dashboard has no leads card")
	}
}
