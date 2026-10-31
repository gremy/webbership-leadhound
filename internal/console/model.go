// Package console is the operator's web console: the types the HTTP layer and
// its backend share, the sign-in and session code, and the handlers.
package console

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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
	CallsBackend
	DraftsBackend
	MailboxesBackend
	ControlsBackend
	StatusBackend
	ManualSendBackend
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
	// Operator is the operator's sign-in email, shown in the gear menu and on Account.
	Operator string
	// Now is the clock the Status page and the dashboard's status card judge ages by; nil
	// is time.Now.
	Now func() time.Time
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
	// JobsCalls and LeadsCalls are today's Jev calls for scoring jobs and rating leads, each
	// against its own daily cap (JobsCallCap, LeadsCallCap; 0 when unknown).
	JobsCalls, JobsCallCap, LeadsCalls, LeadsCallCap int
	AIInputTokens                                    int     // both purposes together
	AICostUSD                                        float64 // both purposes together
	// ClaudeMicros is the account's Claude spend today in micro-dollars, ClaudeCapMicros the
	// LEADHOUND_CLAUDE_DAILY_USD_CAP ceiling, ClaudeByPurpose the spend per purpose (only
	// purposes that spent, drafting, research and drafts_eval first).
	ClaudeMicros, ClaudeCapMicros int64
	ClaudeByPurpose               []PurposeSpend
	Boards                        []BoardCount  // ats, state, n
	LastRuns                      []RunRow      // the latest 8
	LastBrief                     *BriefStamp   // day, sent at, listed count; nil when none sent
	Leads                         *LeadsSummary // nil when leads are not set up on this server
	RunNow                        RunNowCard    // read live, outside the snapshot
	// NeedsMe are the leads waiting on the operator (a reply with nothing scheduled, a call
	// with no next step), the latest first and capped; NeedsMeTotal is how many there are in
	// all, more than len(NeedsMe) when the list is capped.
	NeedsMe      []NeedsMeRow
	NeedsMeTotal int
	// CampaignFunnel is each campaign's emails of the last 30 days: sent, replied, positive.
	CampaignFunnel []CampaignFunnelRow
	// ClaudeMonthMicros is the account's Claude spend this calendar month, in micro-dollars.
	ClaudeMonthMicros int64
	// ReoonChecksToday is today's email checks; ReoonDailyLimit the daily cap on them, 0 when
	// unknown.
	ReoonChecksToday, ReoonDailyLimit int
}

// NeedsMeRow is one lead waiting on the operator: what happened, and when.
type NeedsMeRow struct {
	LeadID int64
	Name   string // the lead's name, or its domain
	What   string // "Replied · nothing scheduled", "Connected on a call · no next step"
	At     time.Time
}

// CampaignFunnelRow is one campaign's emails of the last 30 days.
type CampaignFunnelRow struct {
	Campaign                string // the campaign's name
	Sent, Replied, Positive int
}

// PurposeSpend is one Claude purpose's spend in micro-dollars.
type PurposeSpend struct {
	Purpose string
	Micros  int64
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
	ToLabel                    string   // "" any | "1" the leads to label: the spread sample and the leads with a call
	Campaign                   string   // "" every campaign | one campaign's slug
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
	// Labelled is how many leads carry the operator's call; LabelGoal how many the trust gate
	// needs (30).
	Labelled, LabelGoal int
}

// LeadFilterOptions are the choices the filter form offers, each a value and its words.
type LeadFilterOptions struct {
	Kinds, Countries, Channels []FilterOption
	Statuses, Sorts            []FilterOption
	Campaigns                  []FilterOption // each campaign's slug and name
}

// FilterOption is one choice of a filter: the value the query carries and its label.
type FilterOption struct{ Value, Label string }

// LeadListRow is one lead of the list, worded by the backend. Kind is the kind's label,
// Finding the finding in one line, Relevance the relevance in words ("" when not rated),
// Position the lead's place in the outreach order ("" when it has none), Call the operator's
// own band for it ("" when he has made no call), Campaign the name of the campaign the lead
// belongs to.
type LeadListRow struct {
	LeadID, OrgID                int64
	Name, Domain, Country, Kind  string
	Campaign                     string
	Finding, Relevance, Position string
	State, Call                  string
	HasEmail, HasForm, HasPhone  bool
	Starred, SmallSite           bool
	// StarNote says up front what starring the lead does besides ordering it, when it has
	// no usable way to reach it: StarNoteEmail or StarNotePhone; "" otherwise.
	StarNote string
}

// The star control's notes (LeadListRow.StarNote): a lead with no usable route, in a country
// whose rule lets it be emailed, or in a country that is written to through forms only.
const (
	StarNoteEmail = "No email yet: starring it looks for one"
	StarNotePhone = "Calls-first country: starring it looks for a business phone"
)

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
	YourCall         LeadCall        // the operator's own call: the "Your call" block
	ClaudeSpend      string          // what Claude has cost on this lead in words; "" when nothing
	Research         LeadResearch    // the contact research of the lead's org: the "Contact research" line
	Report           ReportLink      // the upgrade-risk report: offered, or why not
}

// The contact research statuses LeadResearch.Status names (the store's own).
const (
	ResearchQueued  = "queued"
	ResearchLooking = "looking"
	ResearchFound   = "found"
	ResearchNothing = "nothing"
	ResearchCapped  = "capped"
	ResearchError   = "error"
)

// What a research attempt looks for (LeadResearch.Want).
const (
	WantEmail = "email"
	WantPhone = "phone"
)

// LeadResearch is the latest contact research of a lead's org, for its page: the console
// never calls Claude, it only shows what the research job did and asks it to look again.
type LeadResearch struct {
	Status   string     // one of the Research* statuses; "" when never researched
	Want     []string   // what it looks (or looked) for: WantEmail, WantPhone
	Verified int        // contacts found and checked on their page (found)
	At       *time.Time // queued: when asked; looking: when it began; else when it finished
	// NextAt is the first time another attempt may run, when one ran within the last 90
	// days; nil when one may run now.
	NextAt *time.Time
	// CanAsk is true when "Look again" may be offered at all: the lead is new and its
	// country's rule gives something to look for.
	CanAsk bool
}

// Open reports whether an attempt is queued or looking: "Look again" is hidden then.
func (r LeadResearch) Open() bool { return r.Status == ResearchQueued || r.Status == ResearchLooking }

// ReportLink is the lead page's upgrade-risk report: Ready when it can be downloaded, else
// Why in plain words ("" hides the line: nothing to say).
type ReportLink struct {
	Ready bool
	Why   string
}

// LeadReport is the upgrade-risk report of one lead, as the Report link serves it: the
// self-contained HTML page and the file name to save it under.
type LeadReport struct {
	Filename string
	HTML     []byte
}

// LeadCall is the operator's own call on a lead, as its page shows it: the band he chose
// (LeadListRow.Call) and when, the bands he may choose with their plain meaning, and how
// many leads carry a call against the trust gate's goal.
type LeadCall struct {
	At                  *time.Time // when the current call was made; nil with no call
	Bands               []CallBand // best first
	Labelled, LabelGoal int
}

// CallBand is one band the operator may choose: the value the form posts, the button's
// words, and what the band means.
type CallBand struct{ Value, Label, Help string }

// The values the "Your call" form posts as band: one of LabelBands, or LabelClear.
const LabelClear = "clear"

// LabelBands are the bands a call may name, best first: leadrate's bands, which the backend
// checks again.
var LabelBands = []string{"strong", "likely", "maybe", "unlikely", "no"}

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
	// Check is an email route's latest email check in words ("Email check: safe, may be
	// sent"), "" for a form; CheckedAt is when, nil when never checked.
	Check     string
	CheckedAt *time.Time
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

// The sections of the Leads Settings page. Each editable one posts to
// /leads/settings/{section} and is the anchor of its part of the page; SettingsAll takes only
// a reset, of every section at once.
const (
	SettingsWho        = "who"         // who to contact: countries wanted, never-contact countries
	SettingsKinds      = "kinds"       // lead kinds on or off, and their order
	SettingsSmallSites = "small-sites" // skip very small sites
	SettingsBatches    = "batches"     // where a new batch draws its leads from
	SettingsDates      = "dates"       // the Drupal end-of-life dates
	SettingsRelevance  = "relevance"   // Jev relevance: rating on or off, the leads daily cap
	SettingsEmailCheck = "email-check" // the email check before sending: what a catch-all address does
	SettingsOrder      = "order"       // the fallback order: the priority profile's points
	SettingsWindow     = "window"      // the sending window: saved like the others, changed on the Controls page
	SettingsAll        = "all"         // reset only: every section back to the file
)

// The values LeadSettingsValues.CatchAll takes, as leads.yaml's verify.catch_all does.
const (
	CatchAllSend  = "send"  // a catch-all domain's address may enter a live batch
	CatchAllBlock = "block" // it may not
)

// The values LeadSettingsValues.BatchesFrom takes, as leads.yaml's batches_from does.
const (
	BatchesStarred         = "starred"           // starred leads only
	BatchesStarredThenRest = "starred_then_rest" // starred leads first, then the rest of the queue
)

// LeadSettingsValues are the settings the console may change (plan D11), as the Settings
// page's forms hold them: text stays text, so the backend checks it with the same rules
// leads.yaml passes and words every problem. A save hands the backend the values of one
// section; the backend reads only that section's fields.
type LeadSettingsValues struct {
	// Who to contact. Countries are the countries leads.yaml names, in the page's order;
	// Wanted false puts one on the never-contact list. NeverContact is the rest of that list,
	// codes leads.yaml does not name, as typed ("FR, IT"). Both go to the priority profile's
	// exclude_countries.
	Countries    []CountryToggle
	NeverContact string
	// Lead kinds, in the order batches draw them, first first (kinds_priority); On false
	// turns a kind off (kinds_off).
	Kinds []KindToggle
	// SkipRankOver is skip_rank_over as typed: a whole number, "0" off.
	SkipRankOver string
	// BatchesFrom is batches_from: BatchesStarred or BatchesStarredThenRest.
	BatchesFrom string
	// Dates are the end-of-life dates, facts.d6_eol to facts.d10_eol, YYYY-MM-DD as typed.
	Dates []DateSetting
	// Rating is relevance.rating; LeadsCap relevance.leads_daily_calls as typed ("0" is the
	// server's cap, LEADHOUND_JEV_LEADS_DAILY_CALL_CAP).
	Rating   bool
	LeadsCap string
	// CatchAll is verify.catch_all: CatchAllSend or CatchAllBlock.
	CatchAll string
	// The sending window (schedule.business_hours), as the Controls page's form holds it:
	// start and end HH:MM as typed, and the days ticked (Mon … Sun).
	WindowStart, WindowEnd string
	WindowDays             []string
	// Order is the fallback order: the priority profile's points, exact version, countries
	// and rank bands. Its Exclude is never read or written here: never-contact countries are
	// Who's.
	Order PriorityForm
}

// CountryToggle is one country of Who to contact: its ISO code, its words, and whether it
// is contacted.
type CountryToggle struct {
	Code, Label string
	Wanted      bool
}

// KindToggle is one lead kind: its name (lead_kind.name), its words, and whether it is on.
type KindToggle struct {
	Name, Label string
	On          bool
}

// DateSetting is one end-of-life date. Name is the form field and the setting both
// (d6_eol … d10_eol); Label names the Drupal version; Source is where the date comes from.
type DateSetting struct {
	Name, Label, Value string
	Source             SettingSource
}

// SettingSource is where a value comes from: the file (leads.yaml), or the console, saved At.
type SettingSource struct {
	Console bool
	At      time.Time // when it was changed in the console; zero for the file
}

// SettingsSources say where each setting of the page comes from. A date carries its own.
type SettingsSources struct {
	Who, KindsOn, KindsOrder, SkipRankOver, BatchesFrom, Rating, LeadsCap, CatchAll, Order SettingSource
}

// LeadSettingsPage is the Settings page: the values in force and where each comes from,
// the words of the relevance trust gate, and what the console shows but never changes.
// Notes are plain lines about the settings as a whole, such as a saved change leads.yaml no
// longer accepts, which is ignored.
type LeadSettingsPage struct {
	Values    LeadSettingsValues
	Sources   SettingsSources
	ServerCap int       // LEADHOUND_JEV_LEADS_DAILY_CALL_CAP: the most a setting may allow a day
	TrustGate TrustGate // whether the list and the queue sort by relevance yet, and the rule
	// Labelled is how many leads carry the operator's call, LabelGoal how many the trust gate
	// needs; the leads to label are at ToLabelPath.
	Labelled, LabelGoal int
	Notes               []string
	ReadOnly            ReadOnlySettings
}

// TrustGate is the relevance trust gate in words: Status says whether the list and the queue
// sort by relevance yet and what the latest check read, one line each (none when the gate
// says nothing); Rule is the rule in plain words, and Exact its thresholds for the fold.
type TrustGate struct {
	Status      []string
	Rule, Exact string
}

// ReadOnlySettings are the settings the Settings page shows and never changes: they are
// read outside the settings door (plan D11), so they are edited in leads.yaml or set in
// .env on the server. Never put a secret here (operator addresses, a DSN, the password hash,
// a key or a mailbox password); the page also shows an env name only when it is on
// EnvAllowlist.
type ReadOnlySettings struct {
	Countries       []CountryPolicy // each country's channel, language, time zone and footer
	Pace            []string        // sending pace and the warm-up ramp, one plain line each
	SenderName      string
	IdentityLine    string
	SiteURL         string
	OptOutLanguages []string     // the languages that have an opt-out line; the lines are not shown
	Templates       []string     // the message templates in use, by name only (kind, language, step); never a body
	Env             []EnvSetting // the allowlisted environment values in force
}

// CountryPolicy is one country's policy from leads.yaml, in words.
type CountryPolicy struct {
	Code, Label, Channel, Language, Timezone, Footer string
}

// EnvSetting is one environment value the page shows: its name and its value ("" for
// unset, the default in force).
type EnvSetting struct{ Name, Value string }

// EnvAllowlist are the only environment names the Settings page shows (plan D11); the page
// drops any other the backend hands it.
var EnvAllowlist = []string{
	"LEADHOUND_OUTREACH", "LEADHOUND_LEADS_TIMERS", "LEADHOUND_PROBE_RATE",
	"LEADHOUND_PROBE_CONCURRENCY", "LEADHOUND_TZ", "LEADHOUND_JEV_MODEL", "LEADHOUND_JEV_DAILY_CALL_CAP",
	"LEADHOUND_JEV_LEADS_DAILY_CALL_CAP",
}

// SettingsRefused is a settings save the backend refused, with every problem in plain words
// (the same words leads.yaml's checks use). errors.Is(err, ErrUserFixable) holds for it; a
// plain ErrUserFixable refusal works too, shown as one problem.
type SettingsRefused struct {
	Problems []string
}

func (e *SettingsRefused) Error() string {
	return ErrUserFixable.Error() + ": " + strings.Join(e.Problems, " ")
}

// Unwrap makes a SettingsRefused an ErrUserFixable.
func (e *SettingsRefused) Unwrap() error { return ErrUserFixable }

// CallsBackend is what the Today view, the call card and a lead's Sequence section read
// and write. Every read and write loads the leads settings afresh, as LeadsBackend's do,
// and returns ErrLeadsNotSetUp when they are missing or broken.
type CallsBackend interface {
	// LeadsToday is the day's work, decision-first, for one campaign (its slug, as the
	// list's campaign filter takes it) or every campaign (""). It never writes.
	LeadsToday(ctx context.Context, campaign string) (TodayView, error)
	// LeadCall is one call row's card; ErrNotFound for no such message or one that is not
	// a call. It never writes.
	LeadCall(ctx context.Context, messageID int64) (CallCard, error)
	// LeadCallOutcome records what happened on a call: outcome is one of campaign.Outcomes,
	// note at most 500 characters ("" for none); consent and consentAddress only for
	// connected (they agreed to get an email, at that address; "" for the lead's own). It
	// returns the line to show the operator ("" for the console's own). A refusal the
	// operator can read ("not due yet", "They replied: the sequence has ended") wraps
	// ErrUserFixable with its words; ErrNotFound for no such call.
	LeadCallOutcome(ctx context.Context, messageID int64, outcome, note string, consent bool, consentAddress string) (string, error)
	// LeadSequence is one lead's steps, in order, across its latest batch; none before a
	// batch drafted anything for it. ErrNotFound for no such lead. It never writes.
	LeadSequence(ctx context.Context, leadID int64) ([]SequenceStep, error)
}

// TodayView is the Today tab: what to do today, in the order to do it. Campaign is the
// campaign slug the backend took ("" every campaign), echoed by the filter; Campaigns are
// the filter's choices, as LeadFilterOptions.Campaigns.
type TodayView struct {
	Campaign  string
	Campaigns []FilterOption
	// CallsNow are the calls due today that the call rules let be made now (CallCard's
	// Callable: a usable number, not suppressed, the country's law allowing it, screened
	// where needed, inside the lead's calling hours). CallsDue are the calls due today that
	// cannot be made now, each line saying why.
	// CallsOverdue are the calls due before today and not yet made.
	CallsNow, CallsDue, CallsOverdue []TodayItem
	// CallsWaiting are the first few calls of approved batches that are not due yet, each
	// line saying what it waits on; CallsWaitingTotal counts them all.
	CallsWaiting      []TodayItem
	CallsWaitingTotal int
	// Replies are the replies waiting for the operator, newest first.
	Replies []TodayItem
	// EmailsDue is how many emails the send job sends today; Emails the first few of them.
	EmailsDue int
	Emails    []TodayItem
	// Manual are the contact forms and LinkedIn notes due, to use by hand.
	Manual []TodayItem
	// ShadowManual counts the approved contact forms and LinkedIn notes of shadow batches
	// that are not listed under Manual: shadow batches are practice, so the empty state says
	// so rather than that none exist.
	ShadowManual int
}

// TodayItem is one line of the Today tab: the lead, the message it is about (0 when none),
// the organisation's name, one plain line about it (no internal codes), and its time (due,
// or received for a reply; nil when none).
type TodayItem struct {
	LeadID, MessageID int64
	OrgName           string
	Line              string
	At                *time.Time
}

// CallCard is one call row's card: who to call, on which number, where the number came
// from, what to say, whether calls are allowed, and what happened last time.
type CallCard struct {
	MessageID, LeadID int64
	OrgName           string
	Number            string // the number as found ("+44 (0)20 7946 0000"), shown as text
	// Dial is the international number the tel: link dials ("+442079460000"), built from the
	// stored normalised number and the lead's country (internal/dial); "" when there is none
	// (an erased fact, or a national number of an unknown country), and then the card shows
	// the number as text with no link.
	Dial string
	// Where the number came from: the page it was read on and when, or FoundByResearch with
	// the URL research cited.
	SourceURL       string
	SourceReadAt    *time.Time
	FoundByResearch bool
	LineKind        string   // "direct" | "decision-maker" | "switchboard" | "" unknown
	TalkingPoints   []string // three, one line each
	// Callable is whether the call may be made now under the call rules (number,
	// suppression, wrong number, the country's law, screening, hours); BlockedReason says why
	// not, in plain words with no closing full stop ("outside calling hours now (…)"). The
	// outcome of a call that is due can be recorded either way.
	Callable      bool
	BlockedReason string
	Screening     string     // the number's screening status in words ("not required", ...)
	Latest        *CallLog   // the latest outcome recorded; nil when none
	Shadow        bool       // the call belongs to a shadow batch
	Due           *time.Time // when the call fell due; nil when not scheduled
	// NotDue says why an outcome cannot be recorded yet ("Not due yet: this call waits on the
	// email before it."); "" when the call is due. The outcome buttons are disabled while set.
	NotDue string
	// Done is set once an outcome is recorded and the sequence has moved on after it: the
	// store refuses a second outcome then, so the card shows "Done" with Latest and disables
	// the outcome buttons.
	Done bool
	// CallsOff warns that calls to the lead's country are off in the campaign's rules: the
	// operator decides whether to ring. "" when they are on.
	CallsOff string
}

// CallLog is one recorded call outcome: Outcome one of campaign.Outcomes, the note, and for
// connected whether they agreed to get an email.
type CallLog struct {
	Outcome, Note string
	EmailConsent  bool
	At            time.Time
}

// The states of a sequence step (SequenceStep.State). The console words each one.
const (
	SeqDraft     = "draft"     // drafted, the batch not approved yet
	SeqDue       = "due"       // approved and due at At
	SeqWaiting   = "waiting"   // approved, waiting on the step before it (a call, say)
	SeqSent      = "sent"      // an email sent (or shadow-sent, Shadow) at At
	SeqDone      = "done"      // a call made, Outcome recorded at At
	SeqCancelled = "cancelled" // cancelled for Reason
	SeqNoNumber  = "no_number" // a call step with no row: there was no number we may call
	SeqCheck     = "check"     // an email that may have gone out: check it by hand
)

// SequenceStep is one step of a lead's sequence. Step counts from 0; Type is "email" or
// "call"; MessageID is the step's row (0 for SeqNoNumber). Reason is the cancel reason as
// stored (campaign.Cancel* or the store's own, e.g. "opt_out"); Outcome the call outcome for
// SeqDone; Shadow marks a shadow-sent email.
type SequenceStep struct {
	Step      int
	Type      string
	MessageID int64
	State     string
	At        *time.Time
	Reason    string
	Outcome   string
	Shadow    bool
	Note      string // a short note shown after the state ("calls off for IE in the campaign rules")
	CallNote  string // a call's latest outcome note, the operator's own words; "" when none
}

// DraftsBackend is what the Drafts tab and the Campaigns tab's gate line read and write: the
// sampled Claude drafts of a campaign and the operator's review of each. The console never
// calls Claude; `leads drafts sample` writes the samples.
type DraftsBackend interface {
	// DraftSamples is campaign's sampled drafts (its slug; "" the account's first campaign),
	// newest first, at most limit (<= 0: all), with the campaign's gate and the campaigns the
	// filter offers. An unknown slug wraps ErrUserFixable with the words to show. It never
	// writes.
	DraftSamples(ctx context.Context, campaign string, limit int) (DraftsPage, error)
	// ReviewDraft records the operator's verdict on sampled draft generationID: VerdictApprove
	// (no reasons), or VerdictReject with at least one of ReviewReasons ("other" needs a
	// note). The latest review of a draft wins. A draft the check refused, an unknown draft or
	// bad input wraps ErrUserFixable with the words to show.
	ReviewDraft(ctx context.Context, generationID int64, verdict string, reasons []string, note string) error
	// DraftGates is each campaign's Claude-draft gate, by slug. It never writes.
	DraftGates(ctx context.Context) (map[string]DraftGate, error)
}

// A review's verdict, as the store takes it.
const (
	VerdictApprove = "approve"
	VerdictReject  = "reject"
)

// ReviewReasons are the reasons a reject may name, in the form's order, as the store takes
// them; ReasonOther needs a note.
var ReviewReasons = []string{"wrong_tone", "too_long", "unclear", "factual", "other"}

// ReasonOther is the reject reason that needs a note.
const ReasonOther = "other"

// DraftGate is a campaign's Claude-draft trust gate: whether batches may draft with Claude,
// and the gate in one line ("Claude drafts: not trusted yet: 14 of 20 reviewed, 86% approved").
type DraftGate struct {
	Trusted bool
	Summary string
}

// DraftsPage is one campaign's sampled drafts, newest first, with its gate. Campaign is the
// slug the backend took; Campaigns are the filter's choices ("" when there is no campaign).
type DraftsPage struct {
	Campaign  string
	Campaigns []FilterOption
	Gate      DraftGate
	Samples   []DraftSample
}

// DraftSample is one sampled draft: its lead, the facts it rests on, what it says with each
// claim's cites resolved to the fact and its source row, the check's verdict and the latest
// review. A draft the check refused shows why and cannot be reviewed (Reviewable false).
type DraftSample struct {
	GenerationID  int64
	LeadID        int64
	Domain        string
	At            time.Time
	Attempt       int  // 1, or 2 for the retry after a refused first try
	Fake          bool // the fake decider wrote it: a test draft that never counts toward the gate
	CheckOK       bool
	Reviewable    bool
	Failures      []DraftFailure
	Facts         []DraftFact
	Steps         []DraftStep
	TalkingPoints []DraftParagraph
	Review        *DraftReview // nil: not reviewed yet
}

// DraftFailure is one reason the check refused a draft: where in plain words ("email 1,
// paragraph 2", "the talking points"), the message, and the rule's code for the details.
type DraftFailure struct {
	Where, Msg, Code string
}

// DraftFact is one fact of a draft's fact sheet: its id on the sheet (F2), label, value, the
// group it belongs to ("lead" | "sender" | "offer"), and the row it came from. ContextOnly
// marks the org's own text, which a draft may never quote.
type DraftFact struct {
	ID, Label, Value, Group string
	Table                   string
	RowID                   int64
	ContextOnly             bool
}

// DraftStep is one email of a draft. Step counts from 0.
type DraftStep struct {
	Step       int
	Subject    string
	Paragraphs []DraftParagraph
}

// DraftParagraph is one paragraph or talking point with its cites resolved.
type DraftParagraph struct {
	Text   string
	PainID string
	Cites  []DraftCite
}

// DraftCite is one cited fact: its id on the sheet, label, value and source row. Missing
// marks a cite that names no fact on the sheet (the check refuses it).
type DraftCite struct {
	ID, Label, Value, Table string
	RowID                   int64
	Missing                 bool
}

// DraftReview is the latest review of a draft: VerdictApprove or VerdictReject with its
// reasons (ReviewReasons) and note.
type DraftReview struct {
	Verdict string
	Reasons []string
	Note    string
	At      time.Time
}

// ClaudeWriting is how a message Claude wrote rests on stored facts, for the batch page:
// each paragraph (each talking point, for a call) with its cited facts. Paragraphs carry no
// text when the generation's own text could not be read; the cites still show.
type ClaudeWriting struct {
	GenerationID int64
	Paragraphs   []DraftParagraph
}

// ControlsBackend is what the Controls page, a campaign's mailboxes and a mailbox's sending
// card read and write: every on/off that governs leads, read live (the timers from systemd),
// and the mailboxes each campaign sends from. Like the leads backend, it returns
// ErrLeadsNotSetUp when this server has no usable leads settings.
type ControlsBackend interface {
	// Controls is the Controls page: why nothing is going out (or when it next does), the
	// sending switch and each job's switch with its schedule, timer and last run, and the
	// guards set on the server. It never writes.
	Controls(ctx context.Context) (ControlsPage, error)
	// SendingWhy is the one sentence about sending the Controls page leads with, for the
	// Leads page and the dashboard. It never writes.
	SendingWhy(ctx context.Context) (SendingWhy, error)
	// ControlAct runs one write: a job switch (ControlSwitchOn, ControlSwitchOff) or a
	// campaign's mailbox (ControlAssign, ControlUnassign). The sending switch is not one: it
	// stays LeadAct's LeadPause and LeadResume. A refusal the operator can fix wraps
	// ErrUserFixable with the words to show.
	ControlAct(ctx context.Context, a ControlAction) (ControlOutcome, error)
	// CampaignMailboxes is every campaign of the operator's account, in the Campaigns tab's
	// order, with the mailboxes it may be assigned and where each one's domain lands. It
	// never writes.
	CampaignMailboxes(ctx context.Context) ([]CampaignMailboxes, error)
	// MailboxSending is mailbox id's campaigns, where its domain lands, and why nothing goes
	// out from it now (or when it next does). ErrNotFound for no such mailbox. It never
	// writes.
	MailboxSending(ctx context.Context, id int64) (MailboxSendingView, error)
	// SendNow asks for one send that skips the sending window and nothing else: it refuses
	// (ErrUserFixable, with the words to show) while a send runs or one is already asked,
	// in shadow mode, while sending is off, with nothing due now and with today's warm-up
	// limit used, and otherwise writes the request file its watcher starts the Send now unit
	// from. The console gains no privilege: systemd runs the send.
	SendNow(ctx context.Context) (ControlOutcome, error)
}

// The writes ControlAction.Do names.
const (
	ControlSwitchOn  = "on"       // Job, Reason
	ControlSwitchOff = "off"      // Job, Reason
	ControlAssign    = "assign"   // Campaign, MailboxID
	ControlUnassign  = "unassign" // Campaign, MailboxID
)

// The jobs a switch turns on and off, as the store names them; the route segment of a
// switch's form is the name.
const (
	JobBatch    = "batch"    // the morning batch
	JobSync     = "sync"     // the inbox sync
	JobProbe    = "probe"    // the site probe
	JobContacts = "contacts" // contacts, enrich and rate
	JobResearch = "research" // contact research
	JobVerify   = "verify"   // the email check (Reoon)
)

// ControlJobs lists the switched jobs in the order the Controls page shows them.
var ControlJobs = []string{JobBatch, JobSync, JobProbe, JobContacts, JobResearch, JobVerify}

// ControlReasonMax is the longest reason a job switch keeps, on one line.
const ControlReasonMax = 300

// ControlAction is one write from the Controls page or a campaign's mailboxes.
type ControlAction struct {
	Do        string
	Job       string // one of ControlJobs
	Reason    string // why the job is switched; "" for none
	Campaign  string // the campaign's slug
	MailboxID int64
}

// ControlOutcome is what a write did: the line to show.
type ControlOutcome struct {
	Message string
}

// SendingWhy is why nothing is going out, in one plain sentence, or when the waiting emails
// next go. Blocked is true while something stops them (the page shows it amber), false when
// they go at the next run (green). Next is when the sentence says they next go; nil when it
// names no time.
type SendingWhy struct {
	Blocked  bool
	Sentence string
	Next     *time.Time
	// SendNow is true when the page offers Send now beside the sentence: the waiting emails
	// go in the next sending window, or at the next run.
	SendNow bool
	// SendNowLine is the state of the latest Send now request ("Asked at 21:03, waiting to
	// start."), "" with none in the last day.
	SendNowLine string
	// SendNowPending is true while that request waits or runs.
	SendNowPending bool
}

// SendNowWords say what Send now skips and what it keeps, under its button.
const SendNowWords = "Send now skips the sending window for one run only. The warm-up and domain limits, the spacing, paused and unassigned mailboxes, opt-outs and the sending switch still apply."

// SendingWindowView is the sending window on the Controls page: start and end (HH:MM, in each
// recipient's local time), the days it opens, every day the form offers, and where it comes
// from (set on Controls, or the leads settings file).
type SendingWindowView struct {
	Start, End string
	Days       []string
	AllDays    []string
	Source     SettingSource
}

// ControlsPage is the Controls page: the sending sentence, the sending switch, each job's
// switch, the guards set on the server and where the rating setting is.
type ControlsPage struct {
	Why     SendingWhy
	Sending ControlRow   // the sending switch (Job ""): it posts LeadPause or LeadResume
	Jobs    []ControlRow // one per ControlJobs entry, in its order
	// TimersRead is true when the timers' state was read from systemd. False: systemd could
	// not be asked, so each timer shows what the server's settings make install.sh do, and
	// TimersNote says so in a plain sentence ("" when read).
	TimersRead bool
	TimersNote string
	// Guards are the switches set on the server only, read-only, each with the line that
	// changes it; GuardsHow says where that line goes.
	Guards    []ServerGuard
	GuardsHow string
	// RatingOn is whether the contacts run rates leads with Jev; RatingPath is the setting on
	// the Settings page.
	RatingOn   bool
	RatingPath string
	// Window is the sending window in force, which the page changes and resets.
	Window SendingWindowView
	// SendSchedule says when the hourly send runs, under the window: a time the window allows
	// outside those runs is reached only by Send now.
	SendSchedule string
	// WindowProblems are why the window last submitted was refused, in plain words; the form
	// then holds what was submitted.
	WindowProblems []string
}

// RatingSettingsPath is the rating setting on the Settings page.
const RatingSettingsPath = "/leads/settings#" + SettingsRelevance

// ControlRow is one switch of the Controls page: sending or a job. On is the switch (for
// sending: not paused); Since is when it was last switched, nil when never; Reason why;
// BySystem is true when the server switched it itself (the one-time conversion of the old
// sending line), not the operator. Schedule says in plain words when it runs; Timer is its
// unit's state; LastRun is its latest run, nil when it never ran.
type ControlRow struct {
	Job      string // one of ControlJobs; "" for sending
	Name     string // "Morning batch"
	What     string // one plain line on what it does
	On       bool
	Since    *time.Time
	Reason   string
	BySystem bool
	Schedule string
	Timer    TimerState
	LastRun  *ControlRun
	// Lines are the row's own plain lines beyond those (the Email check's: whether
	// addresses go unchecked, today's checks, the credits left).
	Lines []string
}

// TimerState is a job's unit as systemd reports it (or, when systemd cannot be asked, as the
// server's settings make install.sh leave it). Unit is "" for a job with no unit of its own
// yet; Next is the timer's next run, nil when none is planned or for a request watcher.
type TimerState struct {
	Unit    string
	Enabled bool
	Next    *time.Time
}

// ControlRun is a job's latest run: when it started and finished (nil while running or when
// it never finished), its result in plain words ("Finished", "Switched off", ...) and
// whether it failed.
type ControlRun struct {
	StartedAt  time.Time
	FinishedAt *time.Time
	Result     string
	Failed     bool
}

// ServerGuard is one switch set on the server only: its environment name, its value ("" for
// unset), what that value means in words, and the line that changes it.
type ServerGuard struct {
	Name, Value, State, Line string
}

// CampaignMailboxes is one campaign's mailboxes for its page. SiteURL is the campaign's
// product.site_url and SiteHost its host ("" when the spec names none); Rows is every mailbox
// not retired, and any retired one still assigned, by id; Assigned counts the assigned rows.
type CampaignMailboxes struct {
	Slug, Name        string
	SiteURL, SiteHost string
	Rows              []CampaignMailboxRow
	Assigned          int
}

// CampaignMailboxRow is one mailbox on a campaign's page: its address, the host after its @
// and its registrable domain, its state (one of the Mailbox* states, Reason for a pause),
// whether it is assigned to the campaign, and where its host's website lands.
type CampaignMailboxRow struct {
	MailboxID             int64
	Address, Host, Domain string
	State, Reason         string
	Assigned              bool
	Landing               LandingView
}

// The states LandingView.State takes.
const (
	LandingNotChecked = "not_checked" // no check has read it yet
	LandingNoAnswer   = "no_answer"   // the latest check found the domain's website didn't answer
	LandingFound      = "found"       // URL is where it lands
)

// LandingView is where https://<mailbox host>/ sent a visitor at the mailbox's latest finished
// check: State one of the Landing* states, URL and Host the final page and its host (Found
// only), At the check. OffSite is true, on a campaign's page, when Host is not the campaign's
// SiteHost (a leading "www." is ignored on both).
type LandingView struct {
	State     string
	URL, Host string
	At        *time.Time
	OffSite   bool
}

// MailboxSendingView is a mailbox's sending card: the campaigns it is assigned to (slug and
// name), where its domain lands, how many approved live emails wait on it, and why none of
// them goes out now (or when they next go). With Waiting 0, Why.Sentence says no approved
// email uses it.
type MailboxSendingView struct {
	MailboxID int64
	Campaigns []FilterOption
	Landing   LandingView
	Waiting   int
	Why       SendingWhy
}

// ManualSendBackend is the console's manual Send now: a batch of chosen leads, previewed, then confirmed.
// The console shows the plan, posts back exactly what it showed (the hash and the warning codes) and
// the operator who pressed; the backend re-checks everything, approves the batch and asks the send
// unit for the run. Hard stops are never codes: no input from the console lifts one. Like the leads
// backend, every method returns ErrLeadsNotSetUp when this server has no usable leads settings, and
// a refusal the operator can fix wraps ErrUserFixable with one plain sentence that carries its number.
type ManualSendBackend interface {
	// SendNowPreview builds (or reuses) a manual batch for the chosen leads or an existing batch and returns its plan.
	// ErrNotFound when the batch or a lead does not exist.
	SendNowPreview(ctx context.Context, req ManualSendRequest) (SendPlan, error)
	// SendNowPlan re-reads the plan of a manual batch (the confirm page's GET and its htmx poll). It
	// never writes. ErrNotFound for no such manual batch.
	SendNowPlan(ctx context.Context, batchID int64) (SendPlan, error)
	// SendNowConfirm approves and asks for the send; a plan that changed since the page was shown returns *PlanChangedError.
	// Changed means: a different hash, a warning that is not in Codes, or a hard stop that is new.
	SendNowConfirm(ctx context.Context, c ManualConfirm) (ControlOutcome, error)
	// SendNowCheckEmail asks the email check to run now for the batch's unchecked addresses.
	SendNowCheckEmail(ctx context.Context, batchID int64) (ControlOutcome, error)
}

// ManualSendRequest is what a Send now button asks for: chosen leads (the lead page, the list) or an
// existing batch (the batch page). Exactly one is set; the console refuses the rest.
type ManualSendRequest struct {
	LeadIDs []int64 // each positive, no repeats, in the order chosen
	BatchID int64
}

// SendPlan is a manual batch as the confirm page shows it.
type SendPlan struct {
	BatchID    int64
	HashPrefix string // the batch's content hash as shown; confirm posts it back
	Leads      []SendPlanLead
	Warnings   []SendWarning // overridable, each with numbers
	Notes      []string      // e.g. two leads share a domain so one goes in the next run; more than 25 must be split
	Waiting    string        // non-empty while drafts are still being written (page polls)
	CanSend    bool          // at least one lead OK and nothing waiting
}

// SendPlanLead is one chosen lead and what happens to it. At most one of OK, Stop and Waiting holds.
type SendPlanLead struct {
	LeadID              int64
	Name, Domain, Email string
	OK                  bool   // goes in this send
	Stop                string // hard stop reason, never overridable; the lead is left out
	Waiting             string // e.g. "Email not checked yet"; the lead is not in this send
}

// SendWarning is one overridable warning: its code (one of SendWarningCodes) and the sentence with
// its numbers, e.g. "Mailbox 1 has sent 18 of 18 today (warm-up). This goes 1 over."
type SendWarning struct {
	Code     string
	Sentence string
}

// The warning codes, a closed set. Anything else is never an override.
const (
	WarnWindow        = "window"         // outside the sending window
	WarnBusinessHours = "business_hours" // a recipient outside business hours
	WarnRamp          = "ramp"           // a mailbox's warm-up room is used (the 40 ceiling stays hard)
	WarnDomainDaily   = "domain_daily"   // a sending domain's 50 a day (the 100 ceiling stays hard)
	WarnSpacingJitter = "spacing_jitter" // no jitter between sends (the 60 s minimum stays)
	WarnSchedule      = "schedule"       // sent before its due time or outside the batch schedule
)

// SendWarningCodes lists every warning code, in the order the confirm page shows them.
var SendWarningCodes = []string{WarnWindow, WarnBusinessHours, WarnRamp, WarnDomainDaily, WarnSpacingJitter, WarnSchedule}

// ManualConfirm is the confirm page's press: the batch, the hash and the warning codes it showed, and
// the operator's sign-in email (Options.Operator). Codes are deduplicated and each is in
// SendWarningCodes; the console has checked that the operator ticked "I have read" when Codes is not
// empty.
type ManualConfirm struct {
	BatchID    int64
	HashPrefix string
	Codes      []string
	Operator   string
}

// PlanChangedError is SendNowConfirm's answer when the plan is no longer the one the page showed;
// Plan is the plan now, which the page shows again for a fresh look.
type PlanChangedError struct {
	Plan SendPlan
}

func (*PlanChangedError) Error() string {
	return "Something changed since you looked; check the warnings again."
}
