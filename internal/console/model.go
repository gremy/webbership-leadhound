// Package console is the operator's web console: the types the HTTP layer and
// its backend share, the sign-in and session code, and the handlers.
package console

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Backend is everything the console reads and writes. The command that serves
// the console implements it over the store; the console never touches the
// store itself.
type Backend interface {
	Dashboard(ctx context.Context) (Dashboard, error)
	BriefJobs(ctx context.Context, day string) (BriefJobs, error)    // day's sent brief (YYYY-MM-DD), or the latest when "", numbered
	Tracked(ctx context.Context, status string) (TrackedList, error) // status "" | "all" | one of store.JobStatuses
	Runs(ctx context.Context, limit int) ([]RunRow, error)
	Rules(ctx context.Context) ([]RuleRow, error) // every rule, disabled included
	Track(ctx context.Context, req TrackRequest) (TrackOutcome, error)
	Job(ctx context.Context, id int64) (JobPage, error) // ErrNotFound for no such job
	AddRule(ctx context.Context, req RuleRequest) (string, error)
	DisableRule(ctx context.Context, id int64, reason string) (string, error)
	LeadsBackend
}

// ErrUserFixable wraps every refusal the operator can fix; the outcome's
// Message is shown.
var ErrUserFixable = errors.New("console: fix the request")

// ErrNotFound is what a read returns for a job, batch or lead that does not exist.
var ErrNotFound = errors.New("console: not found")

// Options configures the handler New returns.
type Options struct {
	Dev      bool
	Location *time.Location
	Logger   *slog.Logger
}

// Dashboard is the state the dashboard page shows. The health lines, the tiers
// and the funnel come from a snapshot built in the background: AsOf is when it
// was built, zero when none has been yet; Updating is a newer one being built,
// Building the first one; LastBuildFailedAt is when the last build failed, nil
// once one succeeds. The rest is read live.
type Dashboard struct {
	AsOf                                         time.Time
	Updating, Building                           bool
	LastBuildFailedAt                            *time.Time
	Day                                          time.Time
	Health                                       []string // brief.HealthLines, verbatim
	ApplyNow, YourCallNow, Unscored, Unreachable int
	Funnel                                       Funnel // copy of store.Funnel's fields (no store import here)
	AICalls, AIInputTokens                       int
	AICostUSD                                    float64
	CallCap                                      int
	Boards                                       []BoardCount  // ats, state, n
	LastRuns                                     []RunRow      // the latest 8
	LastBrief                                    *BriefStamp   // day, sent at, listed count; nil when none sent
	Leads                                        *LeadsSummary // nil when leads are not set up on this server
}

// Funnel counts the day's jobs at each step, as store.Funnel does.
type Funnel struct {
	NewBySource               map[string]int
	Gated, GateFailed, Scored int
}

// BoardCount is the number of boards of one ATS in one state.
type BoardCount struct {
	ATS, State string
	N          int
}

// BriefStamp describes the last sent brief.
type BriefStamp struct {
	Day    time.Time
	SentAt time.Time
	Listed int
	Health []string // the sent brief's stored "Health: " lines
}

// BriefJobs is a sent brief's jobs, in the order they were numbered. No brief is
// the zero BriefJobs.
type BriefJobs struct {
	Day    time.Time
	SentAt *time.Time
	Jobs   []BriefJob
}

// BriefJob is one numbered job of a brief.
type BriefJob struct {
	N                                          int
	JobID                                      int64
	Title, Company, Status, ListingStatus, URL string
}

// TrackedList is the Tracked page's jobs, capped, and how many match in all.
type TrackedList struct {
	Rows  []Tracked
	Total int
}

// Tracked is one job with a tracking status.
type Tracked struct {
	JobID                          int64
	Title, Company, Status, Reason string
	Since                          time.Time
}

// RunRow is one collect, score or brief run.
type RunRow struct {
	ID                 int64
	Command, Source    string
	StartedAt          time.Time
	FinishedAt         *time.Time
	Requests, Failures int
	StopReason, Note   string
}

// RuleRow is one skip rule.
type RuleRow struct {
	ID                           int64
	Kind, Field, Pattern, Reason string
	FromJobID                    *int64
	CreatedAt                    time.Time
	DisabledAt                   *time.Time
	DisabledReason               string
}

// TrackRequest marks one job. Ref is "N" (a number in the brief of Day, or in the
// latest sent brief when Day is "") or "job:ID". Day is YYYY-MM-DD.
type TrackRequest struct {
	Ref, Day                               string
	Action, Reason, RuleField, RulePattern string
}

// TrackOutcome is what a TrackRequest did.
type TrackOutcome struct {
	Job         BriefJob
	From, To    string
	RuleID      int64
	NoOp, Noted bool
	Message     string
}

// RuleRequest adds one rule: skip or flag the jobs whose Field matches Pattern.
type RuleRequest struct {
	Kind, Field, Pattern, Reason string
}

// JobPage is one job's whole record, for reading before applying: its facts, the full
// current description, each criterion's verdict with the keywords it matched and the
// lines that back it, what is unclear, and its status history, newest first.
type JobPage struct {
	JobID                            int64
	Title, Company, Location         string
	URL, ApplyURL                    string // the posting and, when it has its own, the apply link
	ApplicantCount                   *int
	PostedAt                         *time.Time
	ApplyType, ListingStatus, Status string
	Description                      string // "" when the job has no observation yet
	Criteria                         []JobCriterion
	Unclear                          []JobPoint
	History                          []StatusLine
}

// JobCriterion is one criterion's verdict, the keywords it matched and the lines that
// back it.
type JobCriterion struct {
	Name, Verdict, Keywords string
	Evidence                []string
}

// JobPoint is one flagged gate or unclear criterion, with its snippet when it has one.
type JobPoint struct{ Name, Snippet string }

// StatusLine is one entry of a job's status history.
type StatusLine struct {
	Status, Reason, Actor string
	At                    time.Time
}
