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
	RunNow(ctx context.Context, req RunRequest) (RunOutcome, error) // ErrUserFixable for a refusal
	RunNowCard(ctx context.Context) (RunNowCard, error)
	Results(ctx context.Context) (Results, error) // ErrNotFound when there is no preview yet
	LeadsBackend
	MailboxesBackend
}

// The Run now actions. Each name is the request file, the .path unit and the
// route segment alike.
const (
	RunPreview = "preview" // recheck and rank what is collected, show it
	RunFetch   = "fetch"   // a LinkedIn collect and score, then the preview
	RunBrief   = "brief"   // send today's brief now
	RunATS     = "ats"     // the nightly ATS collect, now
)

// RunActions lists the Run now actions in the order the card shows them.
var RunActions = []string{RunPreview, RunFetch, RunBrief, RunATS}

// RunRequest asks for one Run now action. Confirmed is the ATS warning's box.
type RunRequest struct {
	Action    string
	Confirmed bool
}

// RunOutcome is what a RunRequest did, as one sentence for the operator.
type RunOutcome struct {
	Message string
}

// RunNowCard is the dashboard's Run now card: each action's state, when
// today's brief went (nil while unsent) and when the shown results were made
// (nil when there are none).
type RunNowCard struct {
	Actions     []RunNowAction
	BriefSentAt *time.Time
	ResultsAt   *time.Time
}

// RunNowAction is one action's state on the card. State is one of idle,
// waiting, running, done, stuck or unavailable; Line is the plain sentence the
// card prints; Since is when the state began, when known; Pending is true
// while the action waits or runs, so the card keeps polling.
type RunNowAction struct {
	Action, State, Line string
	Since               *time.Time
	Pending             bool
}

// Results is the latest preview: the jobs part of the brief, made now.
type Results struct {
	MadeAt                time.Time
	Day                   time.Time
	RecheckOff            bool
	Health                []string // brief.HealthLines, verbatim
	Apply, YourCall       []ResultJob
	RunnerUp              *ResultJob
	RunnerUpWhy           string // the criterion it failed, and why
	Unreachable, Unscored int
}

// ResultJob is one job of the results, with its live tracking status.
type ResultJob struct {
	JobID                                    int64
	Title, Company, Location, URL, ApplyType string
	Status, ListingStatus                    string
	ApplicantCount                           *int
	PostedAt                                 *time.Time
	Criteria                                 []JobCriterion
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
	RunNow                                       RunNowCard    // read live, outside the snapshot
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

// LeadListQuery is the Leads list's filter as the request gave it: raw values, trimmed and
// with empty ones dropped. The backend reads them, ignores what it does not know and clamps
// the numbers; the console never judges them.
type LeadListQuery struct {
	Kinds, Countries, Channels []string // kind=, country=, channel= (email | form | phone), each repeatable
	Starred                    string   // "" any | "yes" | "no"
	HasPhone                   string   // "" any | "1" only leads with a phone number
	MinRelevance               string   // a whole number, 0 to 100
	Status                     string   // "" (open) | open | contacted | all
	Sort                       string   // "" (relevance) | relevance | newest | order
	Page                       string   // 1 and up
}

// LeadListPage is one page of the Leads list. Total is how many leads match the filter,
// TotalLeads how many there are at all (the two empty states differ). Filter is the query
// as the backend took it, which the form and the pager echo. Notes are plain lines about
// the query, such as a value that was ignored.
type LeadListPage struct {
	Rows          []LeadListRow
	Total         int
	TotalLeads    int
	Page, PerPage int
	Filter        LeadListQuery
	Options       LeadFilterOptions
	Notes         []string
}

// LeadFilterOptions are the choices the filter form offers, each a value and its words.
type LeadFilterOptions struct {
	Kinds, Countries, Channels []FilterOption
	Statuses, Sorts            []FilterOption
}

// FilterOption is one choice of a filter: the value the query carries and its label.
type FilterOption struct{ Value, Label string }

// LeadListRow is one lead of the list, worded by the backend. Kind is the kind's label,
// Finding the finding in one line, Relevance the relevance in words ("" when not rated),
// Position the lead's place in the outreach order ("" when it has none).
type LeadListRow struct {
	LeadID, OrgID                int64
	Name, Domain, Country, Kind  string
	Finding, Relevance, Position string
	State                        string
	HasEmail, HasForm, HasPhone  bool
	Starred, SmallSite           bool
}

// LeadDetailPage is everything stored about one lead, in the order the page shows it: the
// lead and its finding, the finding's signals, the contact routes, the facts found on its
// pages, the agency page, the site, the lead's history, its outreach order, its messages,
// its relevance history and why it ranks where it does. ErrNotFound for no such lead.
type LeadDetailPage struct {
	LeadListRow
	Signals          []LeadSignal
	Routes           []LeadRoute
	Phones           []LeadFact
	Descriptions     []LeadFact
	Social           []LeadFact
	Agency           *LeadAgency
	ListRank         string // "" when unknown
	Origin           string
	CountryPolicy    string // the country's line, e.g. which channel and language it is written in
	Events           []LeadEvent
	Priority         *LeadPriority
	Messages         []LeadDetailMessage
	RelevanceHistory []LeadRelevance // the latest few ratings, newest first
	Why              []string        // why it ranks here, one plain line each
}

// LeadSignal is one piece of the finding's evidence with the fetch it came from. Status is
// the fetch's HTTP status in words; FetchedAt is nil when the fetch is unknown.
type LeadSignal struct {
	Name, Value, FetchURL, FinalURL, Status string
	FetchedAt                               *time.Time
}

// LeadRoute is one way to reach the lead: Kind its label, Note what was checked (the mail
// server, say), PageURL the page it was found on.
type LeadRoute struct {
	Kind, Value, Note, PageURL string
	FoundAt                    *time.Time
}

// LeadFact is one fact found on the lead's own pages: a phone number, a description or a
// social link. Label names it (a social network, say); PageURL is the page it was read on.
// A social link is shown, never fetched.
type LeadFact struct {
	Label, Value, PageURL string
	FoundAt               *time.Time
}

// LeadAgency is the agency page an agency lead was found on.
type LeadAgency struct{ URL, Phrase, FetchURL string }

// LeadEvent is one change of the lead's state.
type LeadEvent struct {
	State, Reason, Actor string
	At                   time.Time
}

// LeadPriority is the latest steer of the lead's place in the queue, in words.
type LeadPriority struct {
	Words, Actor string
	At           time.Time
}

// LeadDetailMessage is one message drafted for the lead.
type LeadDetailMessage struct {
	BatchID                              int64
	Label, Channel, State, Subject, Body string
	Due, Sent                            *time.Time
}

// LeadRelevance is one relevance rating in words.
type LeadRelevance struct {
	Words string
	At    time.Time
}
