// Package console is the operator's web console: the types the HTTP layer and
// its backend share, the sign-in and session code, and the handlers.
package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
	OutreachBackend
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
	// Planned is a rolling job's next automatic run (set for ats and fetch); nil otherwise.
	Planned *PlannedRun
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
	// Reach is the tab: "" every next action, or a Reach* bucket. A bucket picks its own states,
	// so with Reach set a blank Status means every state: the tabs must not default Status to open.
	Reach string
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
	// Labelled is how many leads of each rating type carry the operator's call against the
	// trust gate's goal, one entry per type the header names; LabelGoal how many the trust
	// gate needs (30).
	Labelled  []TypeCount
	LabelGoal int
	// ReachCounts is the List's tabs' numbers: the rest of the filter, counted by next action.
	ReachCounts ReachCounts
}

// TypeCount is how many leads of one rating type carry the operator's call: Type is the
// type in words ("Drupal leads"), Goal how many the type's trust gate needs.
type TypeCount struct {
	Type           string
	Labelled, Goal int
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
// belongs to. RType is the lead's rating type (leadstore's "drupal" or "cafe"), which picks
// the relevance question, the call help and the trust gate that apply to it.
type LeadListRow struct {
	LeadID, OrgID                int64
	Name, Domain, Country, Kind  string
	RType                        string
	Campaign                     string
	Finding, Relevance, Position string
	State, Call                  string
	HasEmail, HasForm, HasPhone  bool
	Starred, SmallSite           bool
	// StarNote says up front what starring the lead does besides ordering it, when it has
	// no usable way to reach it: StarNoteEmail or StarNotePhone; "" otherwise.
	StarNote string
	// Reach is the lead's next action (a Reach* bucket) and ReachReason why it is a dead end (a
	// Reach* reason, "" for any other bucket). ReachWords and ReachReasonWords say them.
	Reach, ReachReason string
	// Channel is the lead's channel (ChannelEmail, ChannelForm, ChannelLinkedIn, or "none").
	Channel     string
	HasLinkedIn bool
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
// its relevance history and why it ranks where it does. ErrNotFound for no such lead. Its next
// action is LeadListRow.Reach and ReachReason, one definition with the List's tabs.
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
	ShowReport       bool            // the lead's kind has an upgrade-risk report at all (Drupal sites, hand)
	Place            *LeadPlaceView  // the map place a café lead came from; nil for a site lead
	Cafe             *LeadCafeView   // the café finding in words; nil for a lead that is not a café
	OriginWords      string          // where the lead was found, in words ("a map listing (Overture)")
	ReachTitle       string          // the heading of what was read about it ("From their site")
	// SendNowBlock is why Send now cannot send this lead, known from the lead alone (its
	// state, or no channel to email it on); "" when the button is offered. The confirm page
	// still checks the rest (suppression, mailboxes, limits).
	SendNowBlock string
	// Contacts is every contact of the lead's org: routes by kind, then phones, then people;
	// within each, live before retired, the operator's first. It replaces Routes and Phones on
	// the page.
	Contacts []LeadContact
	// Drafts is every message of the lead in sequence order, for every channel and state. It
	// replaces Messages on the page.
	Drafts []LeadDraft
	// DraftRequest is the latest Draft it request, nil when it never had one.
	DraftRequest *LeadDraftRequest
	// DraftItBlock is why Draft it cannot run, in plain words ("It already has a draft", "Already
	// contacted", "Asked for at 09:12"); "" when the button is offered.
	DraftItBlock string
	// DiscardBatchID is the batch Discard throws away: an unapproved batch holding this lead
	// alone; 0 hides the button, so a morning batch of many leads is never discarded from here.
	DiscardBatchID int64
	Proposals      []LeadProposal
	// Sector is the org's sector as its stored name, and SectorWords in words; "" when neither the operator nor
	// the rule gives one; SectorByRule says the rule guessed it ("guessed from the domain").
	Sector, SectorWords string
	SectorByRule        bool
	SectorOptions       []FilterOption // the closed list, each a stored name and its words
	// NoteMax is the longest LinkedIn connection note in characters (sender.linkedin_note_max).
	NoteMax int
	// Posted is a refused form to show again. The console sets it; the backend leaves it zero.
	Posted OutreachPosted
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
	WantEmail    = "email"
	WantPhone    = "phone"
	WantLinkedIn = "linkedin"
)

// LeadResearch is the latest contact research of a lead's org, for its page: the console
// never calls Claude, it only shows what the research job did and asks it to look again.
type LeadResearch struct {
	Status   string     // one of the Research* statuses; "" when never researched
	Want     []string   // what it looks (or looked) for: WantEmail, WantPhone, WantLinkedIn
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

// LeadPlaceView is the map place a café lead came from: its category, its address and its
// own website ("" when it has none).
type LeadPlaceView struct {
	Category, Address, Website string
}

// LeadCafeView is a café lead's finding in words: the loyalty and ordering vendors seen by
// display name (none when nothing was seen), whether an Instagram link was found, the site's
// language, and the loyalty vendor it is compared with and that comparison page's link (""
// when the campaign names none).
type LeadCafeView struct {
	Loyalty, Ordering      []string
	Instagram              bool
	Language               string
	VendorName, VendorLink string
}

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
	ServerCap int // LEADHOUND_JEV_LEADS_DAILY_CALL_CAP: the most a setting may allow a day
	// Trust is the relevance trust gate of each rating type, in display order.
	Trust    []TrustRow
	Notes    []string
	ReadOnly ReadOnlySettings
}

// TrustRow is one rating type's relevance trust gate on the Settings page: Type is the type
// in words ("Drupal leads"), Labelled how many of its leads carry the operator's call, Goal
// how many its gate needs (0 hides the count), Gate whether the list and the queue sort that
// type by relevance yet, and the rule.
type TrustRow struct {
	Type           string
	Labelled, Goal int
	Gate           TrustGate
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
	SeqPlanned   = "planned"   // a form or LinkedIn message that would come: nothing is drafted yet
)

// SequenceStep is one step of a lead's sequence. Step counts from 0; Type is "email" or
// "call"; MessageID is the step's row (0 for SeqNoNumber). Reason is the cancel reason as
// stored (campaign.Cancel* or the store's own, e.g. "opt_out"); Outcome the call outcome for
// SeqDone; Shadow marks a shadow-sent email.
type SequenceStep struct {
	Step      int
	Type      string
	Channel   string // the row's channel; a form or LinkedIn step is worded as that message
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
	// JobRunNow asks for one run of job (one of RunNowJobs) now: the same unit its schedule
	// starts, unchanged. It refuses (ErrUserFixable, with the words to show) when systemd
	// cannot be asked, while the job runs or a press waits, while the job is switched off,
	// while its watcher is not armed or has stopped, while another job holds what this one
	// needs, and until confirmed; otherwise it writes the request file its watcher starts the
	// unit from. The console gains no privilege: systemd runs the job.
	JobRunNow(ctx context.Context, job string, confirmed bool) (ControlOutcome, error)
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

// RunNowJobs are the Controls rows with a Run now block, in ControlJobs order.
var RunNowJobs = []string{JobBatch, JobProbe, JobContacts}

// JobRunNow is a Controls row's Run now block. State is idle, waiting, running, done, bad (the
// asked run did not finish) or stuck; Line says it ("" for idle); Pending is true while a press
// waits or the job runs, so the block refreshes itself; CanAsk is !Pending; Confirm is the
// confirm box's sentence (what the run does and costs).
type JobRunNow struct {
	State   string
	Line    string
	Pending bool
	CanAsk  bool
	Confirm string
}

// PlannedRun is a rolling job's next automatic run: when, the sentence that says it and why
// ("Next run planned at 15:00 tomorrow, 24 hours after the run by hand at 15:02."), and
// whether it is overdue (past its plan by more than half an hour with no start; Line then
// says so and names the journal to read).
type PlannedRun struct {
	At      time.Time
	Line    string
	Overdue bool
}

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
	// RunNow is the row's Run now block; nil for a row without one (only RunNowJobs).
	RunNow *JobRunNow
	// Planned is the job's next automatic run on its rolling schedule; nil for a job on a
	// fixed timer or a disabled one. When set, the page shows it instead of Timer.Next.
	Planned *PlannedRun
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
	// SendNowAgainPlan is the plan of Send now again: a manual batch its confirm approved whose
	// send never ran (the request was refused or expired) and none of whose messages went. It
	// never writes. A batch that went, or is not such a batch, is a refusal.
	SendNowAgainPlan(ctx context.Context, batchID int64) (SendPlan, error)
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
	CanSend    bool          // at least one lead OK and no check running
	Again      bool          // Send now again: the batch is approved, and its confirm only asks for the send
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
	// Again is Send now again's press: the batch was approved by its first confirm and is never
	// approved again; only a fresh send request is written.
	Again bool
}

// PlanChangedError is SendNowConfirm's answer when the plan is no longer the one the page showed;
// Plan is the plan now, which the page shows again for a fresh look.
type PlanChangedError struct {
	Plan SendPlan
}

func (*PlanChangedError) Error() string {
	return "Something changed since you looked; check the warnings again."
}

// CRMBackend is what carries a lead past its first reply: the stage it is at, the operator's
// note and next action, the deal's value, a snooze, and the pages built on them (the replies
// inbox, the pipeline, the call queue and the campaign report). It is optional: New finds it
// on the Backend with a type assertion, and a backend without it shows the CRM pages as not
// on this server yet. Like the leads backend, every method returns ErrLeadsNotSetUp when this
// server has no usable leads settings, ErrNotFound for no such lead or call, and a refusal the
// operator can fix as a *CRMRefused (or any error wrapping ErrUserFixable) with one plain
// sentence; a refused write saves nothing. The console never imports the store: these are its
// own types.
type CRMBackend interface {
	// LeadCRM is one lead's stage, note, next action, deal and snooze. A lead with nothing
	// stored is a zero LeadCRM with its EffectiveStage worked out. It never writes.
	LeadCRM(ctx context.Context, leadID int64) (LeadCRM, error)
	// LeadTimeline is everything that happened to one lead, newest first: lead events, sent
	// messages, replies (with their snippet), calls, email checks and stage changes. It never
	// writes.
	LeadTimeline(ctx context.Context, leadID int64) ([]TimelineItem, error)
	// SetLeadStage sets (or, with Stage "", clears) a lead's stage. It returns the line to
	// show ("" for the console's own).
	SetLeadStage(ctx context.Context, c StageChange) (string, error)
	// SetLeadNote saves a lead's note and next action (each "" to clear).
	SetLeadNote(ctx context.Context, c NoteChange) (string, error)
	// SetLeadDeal saves a lead's deal value and currency (ValueCents nil to clear the value).
	SetLeadDeal(ctx context.Context, c DealChange) (string, error)
	// SnoozeLead hides a lead from the replies inbox and Needs me until until; nil clears the
	// snooze.
	SnoozeLead(ctx context.Context, leadID int64, until *time.Time) (string, error)
	// RepliesInbox is the leads that replied and wait on the operator, newest reply first;
	// snoozed leads are left out. It never writes.
	RepliesInbox(ctx context.Context) ([]ReplyRow, error)
	// Pipeline is every lead with an effective stage, in the order each column shows them.
	// The console groups them by stage and sums them per currency. It never writes.
	Pipeline(ctx context.Context) ([]PipelineCard, error)
	// CallQueue is the calls due on day (a date in the console's zone, at midnight) or before
	// and not made, and the call-backs due on day or before, in the order to make them. It
	// never writes.
	CallQueue(ctx context.Context, day time.Time) ([]CallQueueRow, error)
	// CampaignReport is one row per campaign for what was sent from since on. It never writes.
	CampaignReport(ctx context.Context, since time.Time) ([]CampaignReportRow, error)
	// LeadCallOutcomeOn is CallsBackend.LeadCallOutcome with a call-back date: the console
	// calls it instead whenever a CRM backend is present. Its answers are LeadCallOutcome's.
	LeadCallOutcomeOn(ctx context.Context, req CallOutcomeRequest) (string, error)
}

// The pipeline stages, in the order the pipeline shows them (Stages).
const (
	StageTalking  = "talking"
	StageProposal = "proposal"
	StageWon      = "won"
	StageLost     = "lost"
)

// Stages lists the stages, in the pipeline's order.
var Stages = []string{StageTalking, StageProposal, StageWon, StageLost}

// Currencies are the currencies a deal may be in, the first the default. Sums are kept per
// currency and never converted.
var Currencies = []string{"GBP", "EUR", "RON", "USD"}

// LeadCRM is one lead's CRM fields. Stage is the stored stage ("" when none is stored);
// EffectiveStage is what the pipeline shows: Stage, or StageTalking for a lead that replied
// or had a connected call with no stage stored, or "" for neither.
type LeadCRM struct {
	Stage, EffectiveStage string
	LostReason            string // set only when Stage is StageLost
	Note                  string
	NextAction            string
	NextActionOn          *time.Time // a date (midnight in the console's zone); nil when none
	DealValueCents        *int64     // nil when no value is entered
	Currency              string     // one of Currencies
	TenantURL             string     // the won lead's Webbership tenant, https; "" when none
	SnoozedUntil          *time.Time // nil, or a time in the past, when not snoozed
}

// The kinds of timeline item (TimelineItem.Kind); the console marks each kind's dot.
const (
	TimelineEvent = "event" // a lead event (state change, star, label, research)
	TimelineEmail = "email" // a message sent (or shadow-sent)
	TimelineReply = "reply" // a reply received
	TimelineCall  = "call"  // a call outcome recorded
	TimelineCheck = "check" // an email check
	TimelineStage = "stage" // a pipeline stage change

	TimelineDraft    = "draft"    // a message drafted
	TimelineEdit     = "edit"     // the operator edited a draft
	TimelineDone     = "done"     // a form or LinkedIn message marked sent
	TimelineContact  = "contact"  // a contact added, changed, retired or brought back
	TimelineResearch = "research" // a contact search finished
	TimelineProposal = "proposal" // a proposal added or its status changed
	TimelineSector   = "sector"   // the operator set the sector
)

// TimelineItem is one thing that happened to a lead. Line is one plain sentence with no
// internal codes ("Email sent · step 1"); Detail is optional extra text (a reply's snippet, a
// call note, a lost reason), shown escaped; CostMicros is the Claude spend it carried (0 for
// none).
type TimelineItem struct {
	At         time.Time
	Kind       string
	Line       string
	Detail     string
	CostMicros int64
}

// StageChange is the lead page's stage form or a stage button. LostReason (at most
// MaxLostReason characters) is required with StageLost and empty otherwise; TenantURL (https,
// at most MaxTenantURL characters) is allowed only with StageWon.
type StageChange struct {
	LeadID     int64
	Stage      string // one of Stages, or "" to clear the stored stage
	LostReason string
	TenantURL  string
}

// NoteChange is the lead page's note form. Note is at most MaxNote characters, NextAction at
// most MaxNextAction; NextActionOn needs NextAction.
type NoteChange struct {
	LeadID       int64
	Note         string
	NextAction   string
	NextActionOn *time.Time
}

// DealChange is the lead page's deal form: the value in cents (nil clears it) and its
// currency, one of Currencies.
type DealChange struct {
	LeadID     int64
	ValueCents *int64
	Currency   string
}

// The CRM fields' limits, as the store's checks hold them.
const (
	MaxLostReason = 200
	MaxNote       = 2000
	MaxNextAction = 200
	MaxTenantURL  = 500
	MaxDealCents  = 100_000_000_00 // 100 million in any currency
)

// CRMRefused is a CRM write the backend refused: Field is the form field at fault ("stage",
// "lost_reason", "tenant_url", "note", "next_action", "next_action_on", "value", "currency";
// "" for none) and Sentence the plain words to show. errors.Is(err, ErrUserFixable) holds.
type CRMRefused struct {
	Field, Sentence string
}

func (e *CRMRefused) Error() string { return ErrUserFixable.Error() + ": " + e.Sentence }

// Unwrap makes a CRMRefused an ErrUserFixable.
func (e *CRMRefused) Unwrap() error { return ErrUserFixable }

// ReplyRow is one lead of the replies inbox: its latest reply's first lines (Snippet, at most
// 300 characters, quoted history and signature already stripped; "" when none was kept) and
// when it came.
type ReplyRow struct {
	LeadID         int64
	Name, Domain   string
	Campaign       string // the campaign's name; "" when none
	Snippet        string
	At             time.Time
	EffectiveStage string // as LeadCRM.EffectiveStage
}

// PipelineCard is one lead on the pipeline. ExpectedCents is DealValueCents weighted by the
// stage (the backend's weights; 0 with no value); NextAction and NextActionOn show on open
// cards, LostReason on lost ones, TenantURL on won ones.
type PipelineCard struct {
	LeadID         int64
	Name, Domain   string
	Stage          string // the effective stage, one of Stages
	DealValueCents *int64
	Currency       string
	ExpectedCents  int64
	NextAction     string
	NextActionOn   *time.Time
	LostReason     string
	TenantURL      string
}

// CallQueueRow is one call of the call queue. MessageID is the call's row (its card and the
// outcome form); CallBackOn is set when the row is a call-back the operator asked for. Done
// means an outcome is already recorded and the sequence has moved on (a call-back of a call
// already made): the row links to the lead page and offers no outcome buttons. Callable and
// BlockedReason are CallCard's.
type CallQueueRow struct {
	MessageID, LeadID int64
	Name              string
	Number            string // as found, shown as text
	Dial              string // the tel: number, as CallCard.Dial; "" when none
	Due               *time.Time
	CallBackOn        *time.Time
	Callable          bool
	BlockedReason     string
	Done              bool
	Shadow            bool
}

// Money is an amount in one currency, in cents.
type Money struct {
	Currency string
	Cents    int64
}

// CampaignReportRow is one campaign's funnel and cost over the report's days. Contacted is
// the leads sent a first message; Revenue the won deals' values, one entry per currency.
// ClaudeMicros is the Claude spend on the campaign's leads; CostPerLeadMicros and
// CostPerReplyMicros divide it by Contacted and Replied (0 when the divisor is 0).
// EmailChecks counts the email checks run, with no price put on them.
type CampaignReportRow struct {
	Campaign                              string
	Sent, Contacted, Replied, Positive    int
	Won                                   int
	Revenue                               []Money
	ClaudeMicros                          int64
	CostPerLeadMicros, CostPerReplyMicros int64
	EmailChecks                           int
}

// CallOutcomeRequest is the call outcome form with its optional call-back date: the fields of
// CallsBackend.LeadCallOutcome, plus CallBackOn (a date, today or later; nil for none).
type CallOutcomeRequest struct {
	MessageID      int64
	Outcome, Note  string
	Consent        bool
	ConsentAddress string
	CallBackOn     *time.Time
}

// OutreachBackend is the manual side of a lead: its contacts, its drafts, Draft it, its sector,
// its proposals and the proof library. Like the rest of the leads backend every method returns
// ErrLeadsNotSetUp when this server has no usable leads settings, ErrNotFound for no such lead,
// message, contact, proposal or proof (or one that is not that lead's), and a refusal the
// operator can fix as any error wrapping ErrUserFixable, with one plain sentence; a refused write
// saves nothing. A write returns the line to show ("" for the console's own). The console never
// imports the store: these are its own types.
type OutreachBackend interface {
	// AddContact adds c to the lead's org, or marks the value the org already has as the
	// operator's, bringing it back if it was retired: the line says which ("Already there; now
	// marked as yours."). Contacts belong to the org, so every lead of it shows the contact. A
	// new lead's channel is picked again at once.
	AddContact(ctx context.Context, c NewContact) (string, error)
	// EditContact changes a contact. The same value with a new note, or on a retired contact, is
	// a new mark; a new value retires the old one, which stays on the page and the timeline. A
	// new lead's channel is picked again at once.
	EditContact(ctx context.Context, c ContactChange) (string, error)
	// RetireContact takes a contact out of use, with an optional note; research never brings it
	// back. A new lead's channel is picked again at once.
	RetireContact(ctx context.Context, leadID int64, ref ContactRef, note string) (string, error)
	// SaveDraft saves a new text for one of the lead's messages. A form or LinkedIn text is kept
	// beside the original and moves no batch; an email text moves its unapproved batch's hash. A
	// text that fails the checks is refused with the reasons and nothing is written.
	SaveDraft(ctx context.Context, e DraftEdit) (string, error)
	// MarkSent records that the operator sent a form or LinkedIn message by hand, approving its
	// manual batch at the hash he was shown if it was not yet approved. A hash that is no longer
	// the batch's is refused ("This draft changed; reload.").
	MarkSent(ctx context.Context, m MarkSentRequest) (string, error)
	// RequestDraft queues Draft it for the lead on channel (ChannelEmail, ChannelForm,
	// ChannelLinkedIn; "" is the lead's own) and wakes the drafting service. A request already
	// open is refused, naming when it was asked for.
	RequestDraft(ctx context.Context, leadID int64, channel string) (string, error)
	// DraftStatus is the lead's latest Draft it request, nil when it never had one. It never
	// writes: the page polls it while a request is open.
	DraftStatus(ctx context.Context, leadID int64) (*LeadDraftRequest, error)
	// DiscardDraft discards batchID, LeadDetailPage.DiscardBatchID, and returns the lead to new.
	// A batch that is not that lead's alone, or is approved, is refused.
	DiscardDraft(ctx context.Context, leadID, batchID int64) (string, error)
	// SetSector records the operator's sector for the lead's org, one of SectorOptions.
	SetSector(ctx context.Context, leadID int64, sector string) (string, error)
	// AddProposal adds a proposal to the lead. A sent one moves the stage to proposal when the
	// lead has no stage or is still talking.
	AddProposal(ctx context.Context, p NewProposal) (string, error)
	// SetProposalStatus records a proposal's new status, the latest of which wins. Accepted
	// offers Won; it never sets it.
	SetProposalStatus(ctx context.Context, c ProposalStatusChange) (string, error)
	// Proofs is the proof library: every proof still in use, newest first, and the sectors a
	// proof may name. It never writes.
	Proofs(ctx context.Context) (ProofPage, error)
	// AddProof adds a line of proof.
	AddProof(ctx context.Context, p NewProof) (string, error)
	// RetireProof takes a proof out of use, with an optional reason.
	RetireProof(ctx context.Context, proofID int64, reason string) (string, error)
}

// What the outreach forms take, in characters; the store's checks hold the same limits.
const (
	MaxContactNote   = 200
	MaxContactValue  = 500
	MaxPersonName    = 120
	MaxPersonRole    = 60
	MaxDraftBody     = 5000
	MaxDraftSubject  = 200
	MaxProposalTitle = 120
	MaxProposalNote  = 300
	MaxOutreachURL   = 500 // a proposal's or a proof's link
	MaxProofTitle    = 80
	MaxProofLine     = 240
	MaxProofReason   = 200
)

// A lead's next action (LeadListRow.Reach, LeadListQuery.Reach), the store's buckets: worked out
// from the lead, never stored. ReachOther is skipped, suppressed or closed: it has no tab and
// shows only under Status "all".
const (
	ReachEmail     = "email"     // new or batched, on channel email
	ReachManual    = "manual"    // new or batched, on form or LinkedIn, or on no channel with a phone
	ReachDead      = "dead"      // new, on no channel, with no phone: ReachReason says why
	ReachContacted = "contacted" // someone was reached
	ReachOther     = "other"
)

// ReachTabs are the List's tabs by next action, in the order it shows them.
var ReachTabs = []string{ReachEmail, ReachManual, ReachDead, ReachContacted}

// Why a lead is a dead end (LeadListRow.ReachReason); "" for every other bucket.
const (
	ReachLooking     = "looking"      // a search is queued or running
	ReachNothing     = "nothing"      // the latest search found nothing
	ReachFound       = "found"        // the latest search found a contact: the next build gives it its channel
	ReachNotSearched = "not_searched" // no search has finished
	ReachCountry     = "country"      // its country is off
)

// ReachCounts is how many leads match the rest of the filter in each next action: the List's
// tabs' numbers.
type ReachCounts struct {
	Email, Manual, Dead, Contacted, Other int
}

// All is every lead counted, for the All tab.
func (c ReachCounts) All() int { return c.Email + c.Manual + c.Dead + c.Contacted + c.Other }

// Of is the count of one bucket, for a template ranging over ReachTabs.
func (c ReachCounts) Of(bucket string) int {
	switch bucket {
	case ReachEmail:
		return c.Email
	case ReachManual:
		return c.Manual
	case ReachDead:
		return c.Dead
	case ReachContacted:
		return c.Contacted
	case ReachOther:
		return c.Other
	}
	return 0
}

// ReachWords is a next action as a tab or badge says it.
func ReachWords(bucket string) string {
	switch bucket {
	case ReachEmail:
		return "Email-ready"
	case ReachManual:
		return "Manual"
	case ReachDead:
		return "Dead end"
	case ReachContacted:
		return "Contacted"
	}
	return "Other"
}

// ReachBadge is a next action's badge classes: its colour by bucket, none for Other.
func ReachBadge(bucket string) string {
	switch bucket {
	case ReachEmail:
		return "badge reach-badge badge-good"
	case ReachManual:
		return "badge reach-badge badge-warn"
	case ReachDead:
		return "badge reach-badge badge-bad"
	case ReachContacted:
		return "badge reach-badge badge-accent"
	}
	return "badge reach-badge"
}

// ReachReasonWords is why a lead is a dead end in plain words, the one wording the list and the
// lead's page share; "" for no reason.
func ReachReasonWords(reason string) string {
	switch reason {
	case ReachLooking:
		return "Looking for a contact"
	case ReachNothing:
		return "Nothing found"
	case ReachFound:
		return "A contact was found; the next build picks it up"
	case ReachNotSearched:
		return "Not searched yet"
	case ReachCountry:
		return "This country is off in your settings"
	}
	return ""
}

// The channels a draft is written for (LeadDraft.Channel).
const (
	ChannelEmail    = "email"
	ChannelForm     = "form"
	ChannelLinkedIn = "linkedin"
)

// ChannelWords is a draft's channel as the page says it.
func ChannelWords(channel string) string {
	switch channel {
	case ChannelEmail:
		return "Email"
	case ChannelForm:
		return "Contact form"
	case ChannelLinkedIn:
		return "LinkedIn"
	}
	return channel
}

// Where a draft's batch came from (LeadDraft.Origin).
const (
	OriginBatch   = "batch"    // a morning batch, or a Send now batch
	OriginSendNow = "send_now" // a Send now batch from before origins were kept
	OriginDraftIt = "draft_it" // Draft it, on the lead's page
)

// Who wrote a draft (LeadDraft.Writer). writerWords says them.
const (
	WriterClaude   = "claude"
	WriterTemplate = "template"
)

// LeadDraft is one message of the lead as its card shows it, for every channel and state: a
// message already sent stays on the page with neither Editable nor CanMarkSent.
type LeadDraft struct {
	MessageID int64
	Channel   string // ChannelEmail, ChannelForm or ChannelLinkedIn
	Step      int    // its place in the sequence, from 0
	Label     string // the step in words: "Connection note", "After they accept", "Follow-up 1"
	// Note is true for a LinkedIn connection note: the card counts it against
	// LeadDetailPage.NoteMax with NoteLength.
	Note bool
	// To is an email's address and TargetURL a form's or LinkedIn page, which the card links;
	// the other is "".
	To, TargetURL string
	// Subject and Body are the text as it stands: the operator's latest version when Edited.
	// Subject is "" on a form or LinkedIn message.
	Subject, Body string
	// Edited says the operator changed it; OriginalSubject and OriginalBody then hold the text
	// before his first edit ("Show the original"), else they are "".
	Edited                        bool
	OriginalSubject, OriginalBody string
	// Editable says Edit is offered: a form or LinkedIn message still draft or approved, or an
	// email draft of an unapproved batch; never once the lead replied, bounced or opted out.
	Editable bool
	// CanMarkSent says Mark sent is offered: a form or LinkedIn message still draft or approved,
	// live, on a lead that did not reply, bounce or opt out, whose batch is approved or holds no
	// email. A contacted lead keeps it: the message after they accept follows the note.
	CanMarkSent bool
	// BatchID, BatchHash and Origin are its batch. BatchHash is the start of the batch's hash,
	// as shown; Mark sent posts it back as hash, so a batch that changed since is refused.
	BatchID   int64
	BatchHash string
	Origin    string // an Origin constant
	Shadow    bool   // a shadow batch: nothing is really sent or marked sent
	Writer    string // WriterClaude or WriterTemplate
	// Fallback is why the template wrote it when Claude was meant to, in plain words ("Claude
	// refused twice"); "" otherwise. The card says "from the template, because ...".
	Fallback string
	// State is the message's state in words ("Draft", "Approved", "Marked sent", "Cancelled").
	State string
	// DueNow says it is the operator's to send now; Due is when it falls due, nil when it has no
	// date; Sent is when it was sent or marked sent, nil before that.
	DueNow    bool
	Due, Sent *time.Time
}

// NoteLength is the text's length in characters, as the note limit counts it.
func (d LeadDraft) NoteLength() int { return utf8.RuneCountInString(d.Body) }

// The kinds of a contact (LeadContact.Kind, NewContact.Kind), in the order the Add form offers
// them (ContactKinds). The first three and LinkedIn are ways to reach the org; a person is who
// is there.
const (
	ContactEmail    = "email"
	ContactPhone    = "phone"
	ContactForm     = "form"
	ContactLinkedIn = "linkedin"
	ContactPerson   = "person"
)

// ContactKinds lists the kinds of contact the Add form offers.
var ContactKinds = []string{ContactEmail, ContactPhone, ContactForm, ContactLinkedIn, ContactPerson}

// ContactKindWords is a contact's kind as the page says it.
func ContactKindWords(kind string) string {
	switch kind {
	case ContactEmail:
		return "Email"
	case ContactPhone:
		return "Phone"
	case ContactForm:
		return "Contact form"
	case ContactLinkedIn:
		return "LinkedIn"
	case ContactPerson:
		return "Person"
	}
	return kind
}

// Where a contact came from (LeadContact.Provenance), the store's words. A contact the operator
// has claimed is ProvenanceOperator whatever found it.
const (
	ProvenanceOperator = "operator"
	ProvenanceResearch = "research"
	ProvenanceCrawl    = "crawl"
	ProvenanceOverture = "overture"
	ProvenanceEnrich   = "enrich"
)

// ProvenanceWords is where a contact came from, as its badge says it.
func ProvenanceWords(p string) string {
	switch p {
	case ProvenanceOperator:
		return "added by you"
	case ProvenanceResearch:
		return "found by research"
	case ProvenanceCrawl, ProvenanceEnrich:
		return "from their site"
	case ProvenanceOverture:
		return "from the map"
	}
	return ""
}

// ContactRef names one contact: exactly one of its fields is set. A contact lives in one of three
// tables, each with its own ids, so the {cid} path segment carries which: r12 is route 12 (an
// email, a form or a LinkedIn page), f7 is fact 7 (a phone), p3 is person 3. These strings stay
// in the HTML and the path, never on screen.
type ContactRef struct {
	RouteID, FactID, PersonID int64
}

// String is the ref as a {cid} segment; "" for a ref that names no one contact.
func (r ContactRef) String() string {
	switch {
	case r.RouteID > 0 && r.FactID == 0 && r.PersonID == 0:
		return "r" + strconv.FormatInt(r.RouteID, 10)
	case r.FactID > 0 && r.RouteID == 0 && r.PersonID == 0:
		return "f" + strconv.FormatInt(r.FactID, 10)
	case r.PersonID > 0 && r.RouteID == 0 && r.FactID == 0:
		return "p" + strconv.FormatInt(r.PersonID, 10)
	}
	return ""
}

// ParseContactRef reads a {cid} segment: r, f or p and a whole number above zero, written the way
// String writes it, so r+5, r05 and r0 are not refs.
func ParseContactRef(cid string) (ContactRef, bool) {
	if cid == "" {
		return ContactRef{}, false
	}
	n, err := strconv.ParseInt(cid[1:], 10, 64)
	if err != nil {
		return ContactRef{}, false
	}
	var r ContactRef
	switch cid[0] {
	case 'r':
		r.RouteID = n
	case 'f':
		r.FactID = n
	case 'p':
		r.PersonID = n
	}
	return r, r.String() == cid
}

// LeadContact is one contact of the lead's org on its Contacts card, retired ones included: the
// page marks them, and their value stays visible. The card replaces the old "Ways to reach them"
// list, so it carries what that list showed.
type LeadContact struct {
	CID        string // ContactRef.String: the {cid} of its Edit and Retire forms
	Kind       string // a Contact* kind
	Value      string // the address, number or URL as shown; for a person, "" (see Name and Role)
	Name, Role string // a person's, or the person a route belongs to; "" when none
	PersonID   int64  // the person a route belongs to; 0 for none
	Provenance string // a Provenance constant; ProvenanceWords says it
	FoundAt    time.Time
	Note       string // the operator's own note: the latest mark's; "" for none
	Operator   bool   // the operator's own: added or claimed by him, outranking research
	Retired    bool   // out of use: not picked, not sent to, offered no Retire
	// ReplacedBy is the value that replaced a retired contact; "" for none.
	ReplacedBy string
	// Chosen is true for the route this lead is reached on now.
	Chosen bool
	// Warning is one plain line the card shows, never a block: this manual contact is used
	// although its country is off in leads.yaml, or this email looks personal and the country
	// does not allow emailing individuals; "" for neither.
	Warning string
	// PageURL is the page it was found on; "" for the operator's own. Check is an email's latest
	// email check in words and CheckedAt when, nil when never; MailServer what was checked of its
	// mail server, in words. All "" for any other kind.
	PageURL    string
	Check      string
	CheckedAt  *time.Time
	MailServer string
}

// A Draft it request's status (LeadDraftRequest.Status).
const (
	DraftQueued   = "queued"
	DraftDrafting = "drafting"
	DraftDone     = "done"
	DraftFailed   = "failed"
)

// LeadDraftRequest is the lead's latest Draft it request.
type LeadDraftRequest struct {
	Channel     string // the channel it asked for; "" is the lead's own
	Status      string // a Draft* status
	Reason      string // why it failed, in plain words; "" otherwise
	BatchID     int64  // the batch it made; 0 for none
	RequestedAt time.Time
	FinishedAt  *time.Time
}

// Open reports whether it is queued or drafting: the page keeps polling, and Draft it is refused.
func (r LeadDraftRequest) Open() bool { return r.Status == DraftQueued || r.Status == DraftDrafting }

// The statuses of a proposal (LeadProposal.Status), the order the page offers them.
const (
	ProposalSent      = "sent"
	ProposalAccepted  = "accepted"
	ProposalDeclined  = "declined"
	ProposalWithdrawn = "withdrawn"
)

// ProposalStatuses lists every proposal status.
var ProposalStatuses = []string{ProposalSent, ProposalAccepted, ProposalDeclined, ProposalWithdrawn}

// ProposalStatusWords is a proposal's status as the page says it.
func ProposalStatusWords(status string) string {
	switch status {
	case ProposalSent:
		return "Sent"
	case ProposalAccepted:
		return "Accepted"
	case ProposalDeclined:
		return "Declined"
	case ProposalWithdrawn:
		return "Withdrawn"
	}
	return status
}

// LeadProposal is one proposal of the lead with its latest status.
type LeadProposal struct {
	ID         int64
	Title      string
	URL        string // https; "" when none
	ValueCents *int64 // nil when no value is set
	Currency   string // one of Currencies
	At         time.Time
	Status     string // a Proposal* status
	StatusNote string
	StatusAt   time.Time
}

// OutreachPosted is a refused form of the lead's page: what the operator typed and why it was
// refused, kept in that form when the page is shown again with a 422. The console sets it on
// LeadDetailPage.Posted, never the backend. The zero value is no form.
type OutreachPosted struct {
	// Form names the form: "contact-add", "contact" (ID is its CID), "contact-retire" (ID is its
	// CID), "draft" (ID is its MessageID), "draft-it", "sector", "proposal-add", "proposal" (ID
	// is its ID).
	Form, ID string
	Refusal  string            // one plain sentence
	Values   map[string]string // what was typed, by form field name
}

// On says the refusal belongs to this form, for a template: {{if $p.On "draft" $d.MessageID}}.
func (p OutreachPosted) On(form string, id any) bool {
	return p.Form != "" && p.Form == form && p.ID == fmt.Sprint(id)
}

// Field is what was typed in a form field, or stored when nothing was: a form On is true for
// shows it in place of the stored value.
func (p OutreachPosted) Field(name, stored string) string {
	if v, ok := p.Values[name]; ok {
		return v
	}
	return stored
}

// NewContact is a contact the operator adds. Value is the address, number or URL (a person has
// none); Name and Role are a person's. PersonID ties an email or LinkedIn route to a person
// already on the org; 0 for none.
type NewContact struct {
	LeadID                  int64
	Kind                    string // a Contact* kind
	Value, Name, Role, Note string
	PersonID                int64
}

// ContactChange is an edit of one contact, by the form on its card.
type ContactChange struct {
	LeadID                  int64
	Ref                     ContactRef
	Value, Name, Role, Note string
}

// DraftEdit is a new text for one message. Subject is only an email's: the backend ignores it on
// a form or LinkedIn message.
type DraftEdit struct {
	LeadID, MessageID int64
	Subject, Body     string
}

// MarkSentRequest marks one form or LinkedIn message sent; Hash is LeadDraft.BatchHash as the
// card showed it.
type MarkSentRequest struct {
	LeadID, MessageID int64
	Hash              string
}

// NewProposal is a proposal to add. URL is "" or https; ValueCents nil when no value is set;
// Currency one of Currencies; Status one of ProposalStatuses ("" is ProposalSent).
type NewProposal struct {
	LeadID                             int64
	Title, URL, Currency, Status, Note string
	ValueCents                         *int64
}

// ProposalStatusChange is a proposal's new status.
type ProposalStatusChange struct {
	LeadID, ProposalID int64
	Status, Note       string
}

// ProofPage is the proof library's page: the proofs in use, newest first, and the sectors a proof
// may name, each a stored name and its plain words.
type ProofPage struct {
	Proofs  []Proof
	Sectors []FilterOption
}

// Proof is one line of proof in use. A proof that is not Public is used without its link.
type Proof struct {
	ID      int64
	Title   string
	Line    string         // the sentence a draft may use, word for word
	URL     string         // https; "" when none
	Public  bool           // its link may go in a draft
	Sectors []FilterOption // the sectors it serves, each a stored name and its plain words
	At      time.Time
}

// NewProof is a line of proof to add. Sectors are stored names, at least one.
type NewProof struct {
	Title, Line, URL string
	Sectors          []string
	Public           bool
}
