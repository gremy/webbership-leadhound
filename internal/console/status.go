package console

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StatusBackend is the Status page's backend: one read of every area's raw figures, and the
// two writes the page offers. The figures carry no colours; statusAreas colours them.
type StatusBackend interface {
	// Status is every area's figures as they stand. It never writes.
	Status(ctx context.Context) (Status, error)
	// RefreshStatus asks for a fresh Reoon balance: it writes the request file its watcher
	// starts the balance unit from. A refusal the operator can fix (one already asked, the
	// email check not set up) wraps ErrUserFixable with the words to show.
	RefreshStatus(ctx context.Context) (ControlOutcome, error)
	// AddClaudeTopup records a top-up of the Claude account: amountCents in US cents, made
	// at at (the day the operator gave, at midnight in Options.Location). A refusal wraps
	// ErrUserFixable with the words to show.
	AddClaudeTopup(ctx context.Context, amountCents int64, at time.Time) (ControlOutcome, error)
}

// Status is the Status page's figures, one section per area. Each section's UpdatedAt (or
// FetchedAt) is when its figures were last read; nil means never, which the page shows grey
// as "Not checked yet", never as a failure.
type Status struct {
	Mailboxes MailboxesStatus
	Server    ServerStatus
	Claude    ClaudeStatus
	Reoon     ReoonStatus
	Jev       JevStatus
}

// The states a mailbox has on the Status page.
const (
	StatusMailboxActive  = "active"
	StatusMailboxPaused  = "paused" // Reason says why
	StatusMailboxRetired = "retired"
)

// MailboxesStatus is every mailbox, in the Mailboxes page's order. UpdatedAt is the latest
// check or reply sync of any of them; nil when none was ever checked or synced.
type MailboxesStatus struct {
	UpdatedAt *time.Time
	Rows      []MailboxStatus
}

// MailboxStatus is one mailbox's health.
type MailboxStatus struct {
	ID            int64
	Name, Address string
	State         string // a StatusMailbox* state
	Reason        string // why it is paused; "" for none
	// CheckedAt is its latest check; nil when never checked. Checks is that check's result
	// per part, in the order SMTP, IMAP, folders, SPF, DKIM, DMARC.
	CheckedAt *time.Time
	Checks    []CheckPart
	// SentToday is today's sends against RampToday, today's warm-up limit. Ready is true
	// when the next send run may send from it.
	SentToday, RampToday int
	Ready                bool
	// HardBounces7d and Sent7d are its hard bounces and sends over the last 7 days.
	HardBounces7d, Sent7d int
	// SyncedAt is its latest reply sync; nil when never synced.
	SyncedAt *time.Time
}

// CheckPart is one part of a mailbox check and whether it passed.
type CheckPart struct {
	Name string // "SMTP", "IMAP", "folders", "SPF", "DKIM", "DMARC"
	OK   bool
}

// The results a unit's last run has.
const (
	UnitOK      = "ok"
	UnitFailed  = "failed"
	UnitRunning = "running"
	UnitNever   = "never"
)

// The states a unit's switch has.
const (
	SwitchOn      = "on"
	SwitchOff     = "off"
	SwitchUnknown = "unknown" // the unit has no switch, or it could not be read
)

// ServerStatus is the server: each unit's last run and next timer, the database and the
// disk, and the machine's figures. UpdatedAt is when they were read; nil when never.
type ServerStatus struct {
	UpdatedAt *time.Time
	Units     []UnitStatus
	// DBBytes is the database's size on disk against DBMaxBytes, the size it should stay
	// under (0 when not set).
	DBBytes, DBMaxBytes int64
	// DiskFreeBytes of DiskTotalBytes are free on the database's disk (DiskTotalBytes 0 when
	// not read); DiskFloorPct is the share of the disk that should stay free.
	DiskFreeBytes, DiskTotalBytes int64
	DiskFloorPct                  float64
	// Load is the one-minute load average and MemoryPct the share of memory in use; nil
	// when not available on this machine.
	Load, MemoryPct *float64
	// Started is when the console started; zero when unknown.
	Started time.Time
	// Version is the build's commit; "" when unknown.
	Version string
}

// UnitStatus is one systemd unit: its last run, its next timer and its switch.
type UnitStatus struct {
	Name    string     // "leads-send"
	LastRun *time.Time // nil when it never ran
	Result  string     // a Unit* result
	Note    string     // a short note on the last run ("41 s", "2 sent"); "" for none
	Next    *time.Time // the next timer; nil when it has none
	Switch  string     // a Switch* state
}

// ClaudeStatus is the Claude account's spend. UpdatedAt is its latest recorded spend; nil
// when it never spent.
type ClaudeStatus struct {
	UpdatedAt *time.Time
	// TodayMicros is today's spend against DailyCapMicros, the daily ceiling.
	TodayMicros, DailyCapMicros int64
	// ByPurpose is today's spend per purpose, each against its own daily cap.
	ByPurpose []PurposeCap
	// MonthMicros is this calendar month's spend. It has no cap.
	MonthMicros int64
	// Topup is the last top-up recorded; nil when none. SinceTopupMicros is the spend since
	// it.
	Topup            *ClaudeTopup
	SinceTopupMicros int64
	// LastError is the last API error's text, at LastErrorAt; nil when none.
	LastError   string
	LastErrorAt *time.Time
}

// PurposeCap is one Claude purpose's spend today against its daily cap (0 when it has none).
type PurposeCap struct {
	Purpose           string
	Micros, CapMicros int64
}

// ClaudeTopup is one top-up of the Claude account.
type ClaudeTopup struct {
	AmountCents int64
	At          time.Time
}

// ReoonStatus is the email check's (Reoon's) account. FetchedAt is the last good balance
// reading; nil when none.
type ReoonStatus struct {
	FetchedAt                    *time.Time
	APIStatus                    string // as Reoon words it, "active"
	DailyCredits, InstantCredits int
	// LastError is the last failed reading's text, at LastErrorAt; nil when none.
	LastError   string
	LastErrorAt *time.Time
	// RefreshAskedAt is when a refresh was asked and not yet done; nil when none waits.
	RefreshAskedAt *time.Time
	// ChecksToday is today's email checks by outcome.
	ChecksToday ReoonChecks
}

// ReoonChecks counts email checks by outcome.
type ReoonChecks struct {
	Valid, Invalid, Other int
}

// Total is every check counted.
func (c ReoonChecks) Total() int { return c.Valid + c.Invalid + c.Other }

// JevStatus is today's Jev calls, for jobs and for leads, each against its own daily cap (0
// when unknown). UpdatedAt is the latest call; nil when Jev was never called.
type JevStatus struct {
	UpdatedAt            *time.Time
	JobsCalls, JobsCap   int
	LeadsCalls, LeadsCap int
}

const (
	statusPath = "/status"
	// anthropicBilling is where the exact Claude balance is; the API has no balance endpoint.
	anthropicBilling = "https://console.anthropic.com/settings/billing"
	// maxTopupCents is the largest top-up the form takes, $100,000: anything above is a typo.
	maxTopupCents = 100_000_00
)

// statusView is the Status page: the figures, each area's colour from statusAreas, and the
// top-up form's values.
type statusView struct {
	Status  Status
	Order   []statusArea // worst first
	Now     time.Time
	Billing string
	Today   string // YYYY-MM-DD in Options.Location: the top-up date's default and its max
	Form    statusForm
}

// statusForm is what a refused Status page write brings back: the values typed, so nothing is
// lost, and the refusal line, shown inside its own section rather than at the top of the page.
type statusForm struct {
	Amount, Date string // the top-up form's values as typed; Date "" shows today
	// TopupRefusal is the top-up's refusal line; TopupField is the field it is about
	// ("amount", "date"), "" when it is about neither.
	TopupRefusal, TopupField string
	// RefreshRefusal is the Reoon Refresh's refusal line.
	RefreshRefusal string
}

// status is GET /status. It reads only.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	s.renderStatus(w, r, http.StatusOK, s.takeFlash(w, r), statusForm{})
}

// renderStatus shows the Status page with msg, a success flash, at the top, and form's
// values and refusal lines in their sections.
func (s *server) renderStatus(w http.ResponseWriter, r *http.Request, code int, msg string, form statusForm) {
	st, err := s.b.Status(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := s.now()
	today := now.In(s.opt.Location).Format(time.DateOnly)
	s.render(w, r, code, "status", page{Title: "Status", Active: statusPath, SignedIn: true, Message: msg,
		Data: statusView{Status: st, Order: statusAreas(st, now), Now: now, Billing: anthropicBilling, Today: today, Form: form}})
}

// statusLog writes one line for a Status page action and its outcome, never an amount.
func (s *server) statusLog(action, outcome string) {
	s.log.Info("console status", "action", action, "outcome", outcome)
}

// statusAct answers a Status page write: on success a redirect to the page at anchor with
// the outcome in the one-time cookie; on a refusal the page (422) with refused's form, which
// carries the refusal line into the action's own section.
func (s *server) statusAct(w http.ResponseWriter, r *http.Request, action, anchor string, out ControlOutcome, err error, refused func(line string) statusForm) {
	switch {
	case errors.Is(err, ErrUserFixable):
		s.statusLog(action, "refused")
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "", refused(refusalWords(err)))
		return
	case err != nil:
		s.statusLog(action, "failed")
		s.fail(w, r, err)
		return
	}
	s.statusLog(action, "ok")
	s.setFlash(w, out.Message)
	http.Redirect(w, r, statusPath+anchor, http.StatusSeeOther)
}

// refreshStatus is POST /status/refresh: ask for a fresh Reoon balance.
func (s *server) refreshStatus(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	out, err := s.b.RefreshStatus(r.Context())
	s.statusAct(w, r, "refresh", "#reoon", out, err, func(line string) statusForm { return statusForm{RefreshRefusal: line} })
}

// claudeTopup is POST /status/claude-topup: amount in dollars ("50" or "50.00") and the day
// it was made (YYYY-MM-DD, not after today; "" is today). A refusal shows the page with one
// plain line, and nothing is recorded.
func (s *server) claudeTopup(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	form := statusForm{Amount: r.PostForm.Get("amount"), Date: r.PostForm.Get("date")}
	cents, refusal := parseDollars(form.Amount)
	field := "amount"
	var at time.Time
	if refusal == "" {
		at, refusal = s.topupDay(form.Date)
		field = "date"
	}
	if refusal != "" {
		s.statusLog("topup", "refused")
		form.TopupRefusal, form.TopupField = refusal, field
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "", form)
		return
	}
	out, err := s.b.AddClaudeTopup(r.Context(), cents, at)
	s.statusAct(w, r, "topup", "#claude", out, err, func(line string) statusForm {
		form.TopupRefusal = line
		return form
	})
}

// parseDollars reads a top-up amount in dollars, "50", "50.5" or "50.00", into cents. It
// takes digits with at most two decimals, above zero and at most maxTopupCents; anything
// else is a refusal, a sentence to show.
func parseDollars(v string) (cents int64, refusal string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "$")
	if v == "" {
		return 0, "Enter the top-up amount in dollars, like 50 or 50.00."
	}
	whole, frac, dot := strings.Cut(v, ".")
	digits := func(s string) bool {
		for _, c := range s {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	if whole == "" || !digits(whole) || !digits(frac) || dot && frac == "" {
		return 0, "The amount is not a number of dollars, like 50 or 50.00."
	}
	if len(frac) > 2 {
		return 0, "The amount has more than two decimals; give it in dollars and cents."
	}
	if len(whole) > 7 {
		return 0, "The amount is over $100,000.00; check it and try again."
	}
	w, _ := strconv.ParseInt(whole, 10, 64)
	frac += strings.Repeat("0", 2-len(frac))
	f, _ := strconv.ParseInt(frac, 10, 64)
	cents = w*100 + f
	switch {
	case cents <= 0:
		return 0, "The amount must be more than $0.00."
	case cents > maxTopupCents:
		return 0, "The amount is over $100,000.00; check it and try again."
	}
	return cents, ""
}

// topupDay reads the top-up's day, YYYY-MM-DD in Options.Location; "" is today. A day after
// today is refused.
func (s *server) topupDay(v string) (day time.Time, refusal string) {
	now := s.now().In(s.opt.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.opt.Location)
	v = strings.TrimSpace(v)
	if v == "" {
		return today, ""
	}
	d, err := time.ParseInLocation(time.DateOnly, v, s.opt.Location)
	if err != nil {
		return time.Time{}, "The date is not a day like 2026-10-20."
	}
	if d.After(today) {
		return time.Time{}, "The date is after today; give the day the top-up was made."
	}
	return d, ""
}

// now is the time the Status figures are judged at.
func (s *server) now() time.Time {
	if s.opt.Now != nil {
		return s.opt.Now()
	}
	return time.Now()
}

// ago words how long before now t was: "just now", "4 min ago", "3 h ago", "2 days ago".
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " days ago"
}

// widthClass maps n of total to the nearest w-NN class (multiples of 5, w-0 to w-100), so a
// bar or meter has its width without an inline style. A share above zero never rounds to
// w-0, so a small number still shows.
func widthClass(n, total int64) string {
	if total <= 0 || n <= 0 {
		return "w-0"
	}
	pct := math.Round(float64(n) * 20 / float64(total))
	if pct < 1 {
		pct = 1
	}
	return "w-" + strconv.Itoa(int(math.Min(pct, 20))*5)
}

// bytesWords words a size in bytes: "640 MB", "1.2 GB".
func bytesWords(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.Itoa(int(n>>20)) + " MB"
	case n >= 1<<10:
		return strconv.Itoa(int(n>>10)) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

// centsUSD words cents as dollars, "$50.00".
func centsUSD(c int64) string { return USD(c * 10_000) }

// remainingMicros is the estimate of what is left of the last top-up: the top-up minus the
// spend since it, never below zero.
func remainingMicros(c ClaudeStatus) int64 {
	if c.Topup == nil {
		return 0
	}
	return max(c.Topup.AmountCents*10_000-c.SinceTopupMicros, 0)
}

// statusSection is one area's section of the Status page: its colour and the page's
// figures.
type statusSection struct {
	statusArea
	V statusView
}

// Sections are the page's sections, worst first, the order the dashboard's card lists them.
func (v statusView) Sections() []statusSection {
	out := make([]statusSection, len(v.Order))
	for i, a := range v.Order {
		out[i] = statusSection{statusArea: a, V: v}
	}
	return out
}

// Remaining is the estimate of the Claude credit left: the last top-up minus the spend
// since it.
func (v statusView) Remaining() int64 { return remainingMicros(v.Status.Claude) }

// mailboxStateWords is a mailbox's state as its row says it: "Active", "Active · not ready",
// "Paused · 3 bounces in 24 h", "Retired".
func mailboxStateWords(m MailboxStatus) string {
	switch m.State {
	case StatusMailboxPaused:
		if m.Reason != "" {
			return "Paused · " + m.Reason
		}
		return "Paused"
	case StatusMailboxRetired:
		return "Retired"
	}
	if !m.Ready {
		return "Active · not ready"
	}
	return "Active"
}

// checkWords is a mailbox's last check in a few words: "All 6 checks OK", "DKIM failed ·
// DMARC failed", "Never checked".
func checkWords(m MailboxStatus) string {
	if m.CheckedAt == nil {
		return "Never checked"
	}
	var failed []string
	for _, c := range m.Checks {
		if !c.OK {
			failed = append(failed, c.Name+" failed")
		}
	}
	if len(failed) > 0 {
		return strings.Join(failed, " · ")
	}
	if len(m.Checks) == 0 {
		return "Checked"
	}
	return "All " + strconv.Itoa(len(m.Checks)) + " checks OK"
}

// bounceWords is a mailbox's hard bounces over 7 days with their rate, "4 · 1.9%"; just the
// count with nothing sent.
func bounceWords(m MailboxStatus) string {
	if m.Sent7d <= 0 {
		return strconv.Itoa(m.HardBounces7d)
	}
	return strconv.Itoa(m.HardBounces7d) + " · " + strconv.FormatFloat(float64(m.HardBounces7d)*100/float64(m.Sent7d), 'f', 1, 64) + "%"
}

// optional words a figure that may be missing with decimals places and suffix; "n/a" when
// nil.
func optional(v *float64, decimals int, suffix string) string {
	if v == nil {
		return "n/a"
	}
	return strconv.FormatFloat(*v, 'f', decimals, 64) + suffix
}

// uptime words how long the console has run, "3 days", "5 h", "12 min"; "unknown" when its
// start is not known.
func uptime(started, now time.Time) string {
	if started.IsZero() {
		return "unknown"
	}
	d := now.Sub(started)
	switch {
	case d >= 48*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + " days"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	}
	return strconv.Itoa(int(d/time.Minute)) + " min"
}

// int64Of is a template's whole number as an int64; 0 for anything else.
func int64Of(v any) int64 {
	switch v := v.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// ReoonUnknown reports whether the last Reoon reading failed after the last good one.
func (v statusView) ReoonUnknown() bool {
	return newer(v.Status.Reoon.LastErrorAt, v.Status.Reoon.FetchedAt)
}

// ClaudeUnknown reports whether the last Claude call failed after the last recorded spend.
func (v statusView) ClaudeUnknown() bool {
	return newer(v.Status.Claude.LastErrorAt, v.Status.Claude.UpdatedAt)
}

// jevMeterView is one Jev purpose's calls today against its cap (0 when unknown).
type jevMeterView struct {
	Label  string
	N, Cap int
}
