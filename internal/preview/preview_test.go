package preview

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/brief"
)

func sample() Preview {
	n := 42
	posted := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	return Preview{
		Version:    Version,
		MadeAt:     time.Date(2026, 10, 26, 10, 30, 0, 0, time.UTC),
		Day:        "2026-10-26",
		RecheckOff: true,
		Health:     []string{"Health: LinkedIn ok"},
		Apply: []Job{{
			ID: 1, Title: "Go engineer", Company: "Acme", Location: "Remote", URL: "https://example.test/1",
			ApplicantCount: &n, PostedAt: &posted, ApplyType: "simple",
			Criteria: []Criterion{{Name: "stack", Verdict: "pass", Note: "Go"}},
		}},
		YourCall: []Job{{
			ID: 2, Title: "Backend", ApplyType: "ats",
			Criteria: []Criterion{{Name: "seniority", Verdict: "unclear"}},
			Unclear:  []Point{{Name: "seniority", Snippet: "mid or senior"}},
		}},
		RunnerUp:    &RunnerUp{Job: Job{ID: 3, Title: "Associate", Criteria: []Criterion{}}, FailedCriterion: "seniority", Note: "associate"},
		Unreachable: 1,
		Unscored:    2,
	}
}

// tempFiles lists the leftover temporary files Write may create in dir.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".preview-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sample()
	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.MadeAt.Equal(want.MadeAt) {
		t.Errorf("MadeAt = %v, want %v", got.MadeAt, want.MadeAt)
	}
	got.MadeAt = want.MadeAt
	if !got.Apply[0].PostedAt.Equal(*want.Apply[0].PostedAt) {
		t.Errorf("PostedAt = %v, want %v", got.Apply[0].PostedAt, want.Apply[0].PostedAt)
	}
	got.Apply[0].PostedAt = want.Apply[0].PostedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestWriteReplaces(t *testing.T) {
	dir := t.TempDir()
	first := sample()
	if err := Write(dir, first); err != nil {
		t.Fatal(err)
	}
	second := sample()
	second.MadeAt = first.MadeAt.Add(time.Hour)
	second.Apply = nil
	if err := Write(dir, second); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.MadeAt.Equal(second.MadeAt) || len(got.Apply) != 0 {
		t.Errorf("second write not in place: MadeAt %v, %d apply", got.MadeAt, len(got.Apply))
	}
}

func TestReadMissingIsErrNone(t *testing.T) {
	_, err := Read(t.TempDir())
	if !errors.Is(err, ErrNone) {
		t.Fatalf("Read of empty dir = %v, want ErrNone", err)
	}
}

func TestReadCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(dir)
	if err == nil || errors.Is(err, ErrNone) {
		t.Fatalf("Read of corrupt file = %v, want an error that is not ErrNone", err)
	}
}

func TestReadUnknownVersion(t *testing.T) {
	for _, v := range []int{0, Version + 1} {
		dir := t.TempDir()
		p := sample()
		p.Version = v
		if err := Write(dir, p); err != nil {
			t.Fatal(err)
		}
		_, err := Read(dir)
		if err == nil || errors.Is(err, ErrNone) || !strings.Contains(err.Error(), "version") {
			t.Errorf("version %d: Read = %v, want a version error", v, err)
		}
	}
}

func TestFailedWriteKeepsOld(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	old := sample()
	if err := Write(dir, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: a directory made read-only to force a failed write
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: restores the test directory

	next := sample()
	next.MadeAt = old.MadeAt.Add(time.Hour)
	if err := Write(dir, next); err == nil {
		t.Fatal("Write into a read-only dir succeeded")
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("old preview unreadable after a failed write: %v", err)
	}
	if !got.MadeAt.Equal(old.MadeAt) {
		t.Errorf("MadeAt = %v, want the old %v", got.MadeAt, old.MadeAt)
	}
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestFailedRenameLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory where preview.json belongs makes the final rename fail,
	// after the temp file was written.
	if err := os.MkdirAll(filepath.Join(dir, FileName, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, sample()); err == nil {
		t.Fatal("Write over a directory succeeded")
	}
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestWriteMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if err := Write(dir, sample()); err == nil {
		t.Fatal("Write into a missing dir succeeded")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Write created the missing dir: %v", err)
	}
}

func TestFromInputMatchesBriefOrder(t *testing.T) {
	loc := time.FixedZone("test", 2*60*60)
	n := 7
	in := brief.Input{
		Day: time.Date(2026, 10, 26, 0, 0, 0, 0, loc),
		Apply: []brief.Job{
			{ID: 11, Title: "A", ApplicantCount: &n, Criteria: []brief.Criterion{{Name: "stack", Verdict: "pass", Note: "Go"}}},
			{ID: 7, Title: "B"},
		},
		YourCall: []brief.Job{
			{ID: 30, Title: "C", Unclear: []brief.Point{{Name: "remote", Snippet: "hybrid?"}}},
		},
		RunnerUp:    &brief.RunnerUp{Job: brief.Job{ID: 99, Title: "D"}, FailedCriterion: "seniority", Note: "associate"},
		RecheckOff:  true,
		Unreachable: 3,
		Unscored:    4,
		ConsoleURL:  "https://console.example.test",
	}
	madeAt := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	p := FromInput(in, madeAt)

	var got []int64
	for _, j := range p.Apply {
		got = append(got, j.ID)
	}
	for _, j := range p.YourCall {
		got = append(got, j.ID)
	}
	if p.RunnerUp == nil {
		t.Fatal("runner-up dropped")
	}
	got = append(got, p.RunnerUp.ID)
	want := append(in.JobIDs(), in.RunnerUp.ID)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("job ids = %v, want %v", got, want)
	}

	if p.Version != Version || !p.MadeAt.Equal(madeAt) || p.Day != "2026-10-26" {
		t.Errorf("header = version %d, made %v, day %q", p.Version, p.MadeAt, p.Day)
	}
	if !p.RecheckOff || p.Unreachable != 3 || p.Unscored != 4 {
		t.Errorf("counts = recheckOff %v, unreachable %d, unscored %d", p.RecheckOff, p.Unreachable, p.Unscored)
	}
	if !reflect.DeepEqual(p.Health, brief.HealthLines(in)) {
		t.Errorf("Health = %v, want brief.HealthLines", p.Health)
	}
	if p.RunnerUp.FailedCriterion != "seniority" || p.RunnerUp.Note != "associate" {
		t.Errorf("runner-up = %+v", *p.RunnerUp)
	}
	if c := p.Apply[0].Criteria; len(c) != 1 || c[0] != (Criterion{Name: "stack", Verdict: "pass", Note: "Go"}) {
		t.Errorf("criteria = %+v", c)
	}
	if p.Apply[0].ApplicantCount == nil || *p.Apply[0].ApplicantCount != 7 {
		t.Errorf("applicant count = %v", p.Apply[0].ApplicantCount)
	}
	if u := p.YourCall[0].Unclear; len(u) != 1 || u[0] != (Point{Name: "remote", Snippet: "hybrid?"}) {
		t.Errorf("unclear = %+v", u)
	}

	if q := FromInput(brief.Input{}, madeAt); q.RunnerUp != nil || q.Apply == nil || q.YourCall == nil {
		t.Errorf("empty input: runner-up %v, apply %v, your call %v", q.RunnerUp, q.Apply, q.YourCall)
	}
}
