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
	YourCall         LeadCall        // the operator's own call: the "Your call" block
	ClaudeSpend      string          // what Claude has cost on this lead in words; "" when nothing
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
	SettingsOrder      = "order"       // the fallback order: the priority profile's points
	SettingsAll        = "all"         // reset only: every section back to the file
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
	Who, KindsOn, KindsOrder, SkipRankOver, BatchesFrom, Rating, LeadsCap, Order SettingSource
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
	"LEADHOUND_OUTREACH", "LEADHOUND_LEADS_TIMERS", "LEADHOUND_LEADS_SEND", "LEADHOUND_PROBE_RATE",
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
	// CallsOff is why no call may be made at all ("" when calls are allowed). In CP3 it is
	// always set: "Calls are off until the call rules are set up". The calls still show, so
	// the flow can be walked in shadow.
	CallsOff string
	// CallsNow are the calls due and inside the lead's calling hours now; CallsOverdue the
	// calls due before today and not yet made.
	CallsNow, CallsOverdue []TodayItem
	// Replies are the replies waiting for the operator, newest first.
	Replies []TodayItem
	// EmailsDue is how many emails the send job sends today; Emails the first few of them.
	EmailsDue int
	Emails    []TodayItem
	// Manual are the contact forms and LinkedIn notes due, to use by hand.
	Manual []TodayItem
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
	Number            string // the number as found ("+44 20 7946 0000")
	// Where the number came from: the page it was read on and when, or FoundByResearch with
	// the URL research cited.
	SourceURL       string
	SourceReadAt    *time.Time
	FoundByResearch bool
	LineKind        string   // "direct" | "decision-maker" | "switchboard" | "" unknown
	TalkingPoints   []string // three, one line each
	// Callable is whether the call may be made now; BlockedReason says why not ("Calls are off
	// until the call rules are set up"). Recording an outcome works either way.
	Callable      bool
	BlockedReason string
	Screening     string     // the number's screening status in words ("not required", ...)
	Latest        *CallLog   // the latest outcome recorded; nil when none
	Shadow        bool       // the call belongs to a shadow batch
	Due           *time.Time // when the call fell due; nil when not scheduled
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
}
