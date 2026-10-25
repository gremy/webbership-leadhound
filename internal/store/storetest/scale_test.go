//go:build scale

package storetest_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestScaleSeeded seeds the scale database (or reuses it) within the seed's budget, and
// checks the seed's shape: the job count, one observation each, the known jobs.
func TestScaleSeeded(t *testing.T) {
	start := time.Now()
	info := storetest.Seeded(t)
	took := time.Since(start)
	t.Logf("Seeded: %s, %d jobs, day %s", took.Round(time.Millisecond), info.Jobs, info.Day.Format(time.DateOnly))
	if took > 4*time.Minute {
		t.Errorf("Seeded took %s, want under 4m", took)
	}
	if info.Jobs != storetest.ScaleJobs() || info.Day.IsZero() || !info.WideDay.Equal(info.Day.AddDate(0, 0, -1)) {
		t.Fatalf("info = %+v", info)
	}
	for name, id := range map[string]int64{
		"all-pass": info.AllPassJobID, "runner-up": info.RunnerUpJobID,
		"seen today": info.SeenTodayJobID, "not seen": info.NotSeenJobID,
	} {
		if id == 0 {
			t.Errorf("no known %s job", name)
		}
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, storetest.ScaleDSN())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var jobs, obs, unfailed int
	if err := conn.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM job), (SELECT count(*) FROM job_observation),
		        (SELECT count(*) FROM job_observation o
		         WHERE NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.verdict = 'fail'))`).
		Scan(&jobs, &obs, &unfailed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if jobs != info.Jobs || obs != info.Jobs {
		t.Errorf("jobs %d, observations %d, want %d each", jobs, obs, info.Jobs)
	}
	if share := float64(unfailed) / float64(jobs); share < 0.09 || share > 0.13 {
		t.Errorf("%.3f of jobs fail no gate, want about 0.11", share)
	}

	// A second call in the same process returns the same seed.
	if again := storetest.Seeded(t); again != info {
		t.Errorf("second call = %+v, want %+v", again, info)
	}
}
