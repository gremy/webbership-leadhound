// Package preview is the results file the console's Run now card reads: the jobs part of
// the brief, composed now and written to <data dir>/preview.json instead of being mailed.
//
// The file has its own types, with JSON tags, so its format never silently follows a
// refactor of brief.Input; FromInput is the one place the two meet.
package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gremy/webbership-leadhound/internal/brief"
)

// FileName is the results file's name inside the data directory.
const FileName = "preview.json"

// Version is the file format this package writes and the only one Read accepts.
const Version = 1

// dayLayout is how Day is written: the brief's calendar day, YYYY-MM-DD.
const dayLayout = "2006-01-02"

// ErrNone is returned by Read when no preview has been written yet.
var ErrNone = errors.New("preview: none yet")

// Preview is one composed set of results. Apply and YourCall are already ranked and
// capped, in the brief's order.
type Preview struct {
	Version int       `json:"version"`
	MadeAt  time.Time `json:"made_at"`
	// Day is the brief's calendar day, YYYY-MM-DD, in the day's time zone.
	Day string `json:"day"`
	// RecheckOff says the live recheck was skipped because live fetching is off.
	RecheckOff bool      `json:"recheck_off"`
	Health     []string  `json:"health"`
	Apply      []Job     `json:"apply"`
	YourCall   []Job     `json:"your_call"`
	RunnerUp   *RunnerUp `json:"runner_up,omitempty"`
	// Unreachable and Unscored count the jobs that would be listed but whose listing could
	// not be reached or whose criteria are not all decided.
	Unreachable int `json:"unreachable"`
	Unscored    int `json:"unscored"`
}

// Job is one listed job.
type Job struct {
	ID             int64       `json:"id"`
	Title          string      `json:"title"`
	Company        string      `json:"company"`
	Location       string      `json:"location"`
	URL            string      `json:"url"`
	ApplicantCount *int        `json:"applicant_count,omitempty"`
	PostedAt       *time.Time  `json:"posted_at,omitempty"`
	ApplyType      string      `json:"apply_type"`
	Criteria       []Criterion `json:"criteria"`
	Unclear        []Point     `json:"unclear,omitempty"`
}

// Criterion is one criterion's short verdict.
type Criterion struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Note    string `json:"note,omitempty"`
}

// Point is one unclear or flagged item, with the snippet it rests on when there is one.
type Point struct {
	Name    string `json:"name"`
	Snippet string `json:"snippet,omitempty"`
}

// RunnerUp is the best job left out by exactly one failed criterion.
type RunnerUp struct {
	Job
	FailedCriterion string `json:"failed_criterion"`
	Note            string `json:"note,omitempty"`
}

// FromInput maps a composed brief input to a preview made at madeAt. The leads section,
// the console URL and the storage state are left out: the results are jobs only.
func FromInput(in brief.Input, madeAt time.Time) Preview {
	p := Preview{
		Version:     Version,
		MadeAt:      madeAt,
		RecheckOff:  in.RecheckOff,
		Health:      brief.HealthLines(in),
		Apply:       jobs(in.Apply),
		YourCall:    jobs(in.YourCall),
		Unreachable: in.Unreachable,
		Unscored:    in.Unscored,
	}
	if !in.Day.IsZero() {
		p.Day = in.Day.Format(dayLayout)
	}
	if in.RunnerUp != nil {
		p.RunnerUp = &RunnerUp{
			Job:             job(in.RunnerUp.Job),
			FailedCriterion: in.RunnerUp.FailedCriterion,
			Note:            in.RunnerUp.Note,
		}
	}
	return p
}

func jobs(in []brief.Job) []Job {
	out := make([]Job, 0, len(in))
	for _, j := range in {
		out = append(out, job(j))
	}
	return out
}

func job(j brief.Job) Job {
	out := Job{
		ID:             j.ID,
		Title:          j.Title,
		Company:        j.Company,
		Location:       j.Location,
		URL:            j.URL,
		ApplicantCount: j.ApplicantCount,
		PostedAt:       j.PostedAt,
		ApplyType:      j.ApplyType,
		Criteria:       make([]Criterion, 0, len(j.Criteria)),
	}
	for _, c := range j.Criteria {
		out.Criteria = append(out.Criteria, Criterion{Name: c.Name, Verdict: c.Verdict, Note: c.Note})
	}
	for _, u := range j.Unclear {
		out.Unclear = append(out.Unclear, Point{Name: u.Name, Snippet: u.Snippet})
	}
	return out
}

// Write replaces dir/preview.json with p, atomically: it writes a temporary file in the
// same directory (a rename is atomic only within one file system), syncs it, makes it
// 0600 and renames it into place. On any error the temporary file is removed and the old
// preview.json, if any, is left as it was.
func Write(dir string, p Preview) (err error) {
	f, err := os.CreateTemp(dir, ".preview-*.json")
	if err != nil {
		return fmt.Errorf("preview: create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(p); err != nil {
		return fmt.Errorf("preview: encode: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("preview: sync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("preview: close %s: %w", tmp, err)
	}
	if err = os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("preview: chmod %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("preview: rename into place: %w", err)
	}
	return nil
}

// Read loads dir/preview.json. A missing file is ErrNone; unreadable JSON or a version
// this package does not know is a wrapped error that is not ErrNone.
func Read(dir string) (Preview, error) {
	path := filepath.Join(dir, FileName)
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is the configured data dir plus a fixed name
	if errors.Is(err, os.ErrNotExist) {
		return Preview{}, ErrNone
	}
	if err != nil {
		return Preview{}, fmt.Errorf("preview: read %s: %w", path, err)
	}
	var p Preview
	if err := json.Unmarshal(b, &p); err != nil {
		return Preview{}, fmt.Errorf("preview: decode %s: %w", path, err)
	}
	if p.Version != Version {
		return Preview{}, fmt.Errorf("preview: %s has version %d, want %d", path, p.Version, Version)
	}
	return p, nil
}
