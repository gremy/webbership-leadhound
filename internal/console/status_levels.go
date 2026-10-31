package console

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// statusLevel is how an area or a row stands. The order is the severity: a larger level is
// worse, and grey (not checked yet) is never worse than warn.
type statusLevel int

const (
	levelGood statusLevel = iota
	levelGrey
	levelWarn
	levelBad
)

// Class is the level's suffix in the board's classes: dot-good, state-warn, is-bad.
func (l statusLevel) Class() string {
	switch l {
	case levelGrey:
		return "grey"
	case levelWarn:
		return "warn"
	case levelBad:
		return "bad"
	}
	return "good"
}

// Chip is the level in words, as the Status page's legend says it.
func (l statusLevel) Chip() string {
	switch l {
	case levelGrey:
		return "Not checked yet"
	case levelWarn:
		return "Look soon"
	case levelBad:
		return "Act now"
	}
	return "All clear"
}

// statusThresholds are every number the Status colours turn on. They live in one value so
// the Status page and the dashboard's status card cannot judge the same figure two ways.
// The rules without a number: any paused mailbox is amber and every live one paused red; a
// failed part of a mailbox's last check is amber; a unit whose last run failed is red; free
// disk under the server's floor is red; Jev at or over a cap is amber; Claude's month to
// date has no cap and never colours. An area never read is grey; one whose last error is
// newer than its last good reading is unknown, amber, with the error.
type statusThresholds struct {
	SyncWarn, SyncBad time.Duration // a mailbox's reply sync older than this is amber, red
	ReoonWarnUnder    int           // Reoon daily credits under this are amber; none left is red
	ClaudeWarnShare   float64       // Claude today over this share of a daily cap is amber; at or over the cap red
	DBWarnShare       float64       // the database over this share of its max is amber; over the max red
}

// thresholds is the one threshold table.
var thresholds = statusThresholds{
	SyncWarn:        45 * time.Minute,
	SyncBad:         2 * time.Hour,
	ReoonWarnUnder:  20,
	ClaudeWarnShare: 0.8,
	DBWarnShare:     0.8,
}

// statusFinding is one judged figure: its level and the plain line that says why.
type statusFinding struct {
	Level statusLevel
	Line  string
}

// worst is the first finding of the highest level in fs; good with line ok when fs is empty.
func worst(fs []statusFinding, ok string) statusFinding {
	out := statusFinding{Level: levelGood, Line: ok}
	for _, f := range fs {
		if f.Level > out.Level {
			out = f
		}
	}
	return out
}

// statusArea is one area as the dashboard's status card and the Status page show it: its
// level, one plain line, the anchor of its Status page section, and when it was last read.
type statusArea struct {
	ID, Name  string
	Level     statusLevel
	Line      string
	UpdatedAt *time.Time
}

// The areas, in the order they show when they stand the same.
const (
	areaMailboxes = "mailboxes"
	areaServer    = "server"
	areaClaude    = "claude"
	areaReoon     = "reoon"
	areaJev       = "jev"
)

// notChecked is the grey line.
const notChecked = "Not checked yet"

// statusAreas judges every area of st at now, worst first (bad, warn, grey, good; areas
// that stand the same keep the order Mailboxes, Server, Claude, Reoon, Jev). It is the
// only place a Status figure gets a colour.
func statusAreas(st Status, now time.Time) []statusArea {
	areas := []statusArea{
		mailboxesArea(st.Mailboxes, now),
		serverArea(st.Server),
		claudeArea(st.Claude),
		reoonArea(st.Reoon),
		jevArea(st.Jev),
	}
	slices.SortStableFunc(areas, func(a, b statusArea) int { return cmp.Compare(b.Level, a.Level) })
	return areas
}

// newer reports whether an error at errAt is newer than the last good reading at good
// (nil: never), which makes the figures unknown.
func newer(errAt, good *time.Time) bool {
	return errAt != nil && (good == nil || errAt.After(*good))
}

// mailboxFindings judges one mailbox. A retired mailbox is never judged.
func mailboxFindings(m MailboxStatus, now time.Time) []statusFinding {
	if m.State == StatusMailboxRetired {
		return nil
	}
	name := cmp.Or(m.Name, m.Address)
	var fs []statusFinding
	if m.State == StatusMailboxPaused {
		line := name + " paused"
		if m.Reason != "" {
			line += ": " + m.Reason
		}
		fs = append(fs, statusFinding{levelWarn, line})
	}
	var failed []string
	for _, c := range m.Checks {
		if !c.OK {
			failed = append(failed, c.Name)
		}
	}
	if len(failed) > 0 {
		fs = append(fs, statusFinding{levelWarn, name + " failed " + strings.Join(failed, ", ")})
	}
	switch age := syncAge(m, now); {
	case m.SyncedAt == nil:
		fs = append(fs, statusFinding{levelGrey, name + " replies not synced yet"})
	case age > thresholds.SyncBad:
		fs = append(fs, statusFinding{levelBad, name + " replies last synced " + ago(*m.SyncedAt, now)})
	case age > thresholds.SyncWarn:
		fs = append(fs, statusFinding{levelWarn, name + " replies last synced " + ago(*m.SyncedAt, now)})
	}
	return fs
}

// syncAge is how long ago m's replies were synced; 0 when never.
func syncAge(m MailboxStatus, now time.Time) time.Duration {
	if m.SyncedAt == nil {
		return 0
	}
	return now.Sub(*m.SyncedAt)
}

// mailboxLevel is one mailbox's row on the Status page.
func mailboxLevel(m MailboxStatus, now time.Time) statusFinding {
	if m.State == StatusMailboxRetired {
		return statusFinding{levelGrey, "Retired"}
	}
	return worst(mailboxFindings(m, now), "OK")
}

func mailboxesArea(ms MailboxesStatus, now time.Time) statusArea {
	a := statusArea{ID: areaMailboxes, Name: "Mailboxes", UpdatedAt: ms.UpdatedAt}
	var fs []statusFinding
	live, paused := 0, 0
	for _, m := range ms.Rows {
		if m.State == StatusMailboxRetired {
			continue
		}
		live++
		if m.State == StatusMailboxPaused {
			paused++
		}
		fs = append(fs, mailboxFindings(m, now)...)
	}
	switch {
	case live == 0:
		a.Level, a.Line = levelGrey, "No mailboxes yet"
		return a
	case paused == live:
		fs = append([]statusFinding{{levelBad, "All mailboxes paused"}}, fs...)
	}
	if ms.UpdatedAt == nil {
		fs = append(fs, statusFinding{levelGrey, notChecked})
	}
	ok := "All " + strconv.Itoa(live) + " mailboxes OK"
	if live == 1 {
		ok = "The mailbox is OK"
	}
	f := worst(fs, ok)
	a.Level, a.Line = f.Level, f.Line
	return a
}

// unitLevel is one unit's row on the Status page.
func unitLevel(u UnitStatus) statusLevel {
	switch u.Result {
	case UnitFailed:
		return levelBad
	case UnitNever, "":
		return levelGrey
	}
	return levelGood
}

func serverArea(sv ServerStatus) statusArea {
	a := statusArea{ID: areaServer, Name: "Server", UpdatedAt: sv.UpdatedAt}
	if sv.UpdatedAt == nil {
		a.Level, a.Line = levelGrey, notChecked
		return a
	}
	var fs []statusFinding
	for _, u := range sv.Units {
		if unitLevel(u) == levelBad {
			fs = append(fs, statusFinding{levelBad, u.Name + " failed"})
		}
	}
	if l := diskLevel(sv); l == levelBad {
		fs = append(fs, statusFinding{levelBad, "Free disk " + pct(diskFreePct(sv)) + ", under the " + pct(sv.DiskFloorPct) + " floor"})
	}
	switch l := dbLevel(sv); l {
	case levelBad:
		fs = append(fs, statusFinding{levelBad, "Database " + bytesWords(sv.DBBytes) + ", over its " + bytesWords(sv.DBMaxBytes)})
	case levelWarn:
		fs = append(fs, statusFinding{levelWarn, "Database " + bytesWords(sv.DBBytes) + " of " + bytesWords(sv.DBMaxBytes)})
	}
	ok := "All units ran clean"
	if len(sv.Units) == 0 {
		ok = "No units ran yet"
	}
	f := worst(fs, ok)
	a.Level, a.Line = f.Level, f.Line
	return a
}

// diskFreePct is the free share of the disk in percent; 0 when not read.
func diskFreePct(sv ServerStatus) float64 {
	if sv.DiskTotalBytes <= 0 {
		return 0
	}
	return float64(sv.DiskFreeBytes) * 100 / float64(sv.DiskTotalBytes)
}

// diskLevel is the free disk's level: red under the floor, good otherwise, grey unread.
func diskLevel(sv ServerStatus) statusLevel {
	switch {
	case sv.DiskTotalBytes <= 0:
		return levelGrey
	case diskFreePct(sv) < sv.DiskFloorPct:
		return levelBad
	}
	return levelGood
}

// dbLevel is the database's level against its max: red over it, amber over the warn share,
// grey with no max set.
func dbLevel(sv ServerStatus) statusLevel {
	switch {
	case sv.DBMaxBytes <= 0:
		return levelGrey
	case sv.DBBytes > sv.DBMaxBytes:
		return levelBad
	case float64(sv.DBBytes) > thresholds.DBWarnShare*float64(sv.DBMaxBytes):
		return levelWarn
	}
	return levelGood
}

// spendLevel is a spend against its daily cap: red at or over it, amber over the warn share,
// good under it or with no cap.
func spendLevel(micros, capMicros int64) statusLevel {
	switch {
	case capMicros <= 0:
		return levelGood
	case micros >= capMicros:
		return levelBad
	case float64(micros) > thresholds.ClaudeWarnShare*float64(capMicros):
		return levelWarn
	}
	return levelGood
}

func claudeArea(c ClaudeStatus) statusArea {
	a := statusArea{ID: areaClaude, Name: "Claude", UpdatedAt: c.UpdatedAt}
	switch {
	case newer(c.LastErrorAt, c.UpdatedAt):
		a.Level, a.Line = levelWarn, "Unknown: "+cmp.Or(c.LastError, "the last call failed")
		return a
	case c.UpdatedAt == nil:
		a.Level, a.Line = levelGrey, notChecked
		return a
	}
	line := USD(c.TodayMicros) + " today"
	if c.DailyCapMicros > 0 {
		line = SpendOf(c.TodayMicros, c.DailyCapMicros) + " today"
	}
	fs := []statusFinding{{spendLevel(c.TodayMicros, c.DailyCapMicros), line}}
	for _, p := range c.ByPurpose {
		if l := spendLevel(p.Micros, p.CapMicros); l > levelGood {
			fs = append(fs, statusFinding{l, purposeWords(p.Purpose) + " " + SpendOf(p.Micros, p.CapMicros) + " today"})
		}
	}
	f := worst(fs, line)
	a.Level, a.Line = f.Level, f.Line
	return a
}

// purposeWords is a Claude purpose as the page says it: "drafts_eval" is "Drafts eval".
func purposeWords(p string) string {
	p = strings.ReplaceAll(p, "_", " ")
	if p == "" {
		return p
	}
	return strings.ToUpper(p[:1]) + p[1:]
}

// reoonLevel is the daily credits' level: red at none, amber under the warn line.
func reoonLevel(credits int) statusLevel {
	switch {
	case credits <= 0:
		return levelBad
	case credits < thresholds.ReoonWarnUnder:
		return levelWarn
	}
	return levelGood
}

func reoonArea(r ReoonStatus) statusArea {
	a := statusArea{ID: areaReoon, Name: "Reoon", UpdatedAt: r.FetchedAt}
	switch {
	case newer(r.LastErrorAt, r.FetchedAt):
		a.Level, a.Line = levelWarn, "Unknown: "+cmp.Or(r.LastError, "the last reading failed")
		if r.FetchedAt == nil {
			a.UpdatedAt = r.LastErrorAt
		}
		return a
	case r.FetchedAt == nil:
		a.Level, a.Line = levelGrey, notChecked
		return a
	}
	a.Level = reoonLevel(r.DailyCredits)
	a.Line = thousands(r.DailyCredits) + " daily credits left"
	if r.DailyCredits <= 0 {
		a.Line = "No daily credits left"
	}
	return a
}

// jevLevel is one purpose's calls against its cap: amber at or over it.
func jevLevel(calls, limit int) statusLevel {
	if limit > 0 && calls >= limit {
		return levelWarn
	}
	return levelGood
}

func jevArea(j JevStatus) statusArea {
	a := statusArea{ID: areaJev, Name: "Jev", UpdatedAt: j.UpdatedAt}
	if j.UpdatedAt == nil {
		a.Level, a.Line = levelGrey, notChecked
		return a
	}
	var fs []statusFinding
	if jevLevel(j.JobsCalls, j.JobsCap) > levelGood {
		fs = append(fs, statusFinding{levelWarn, "Jobs at the cap: " + thousands(j.JobsCalls) + " of " + thousands(j.JobsCap) + " calls"})
	}
	if jevLevel(j.LeadsCalls, j.LeadsCap) > levelGood {
		fs = append(fs, statusFinding{levelWarn, "Leads at the cap: " + thousands(j.LeadsCalls) + " of " + thousands(j.LeadsCap) + " calls"})
	}
	f := worst(fs, callsOf(j.JobsCalls, j.JobsCap)+" for jobs · "+callsOf(j.LeadsCalls, j.LeadsCap)+" for leads")
	a.Level, a.Line = f.Level, f.Line
	return a
}

// callsOf words calls against a cap, "120 of 300"; "120" with no cap.
func callsOf(n, limit int) string {
	if limit > 0 {
		return thousands(n) + " of " + thousands(limit)
	}
	return thousands(n)
}

// pct words a percentage with no decimals, "12%".
func pct(p float64) string { return strconv.FormatFloat(p, 'f', 0, 64) + "%" }
