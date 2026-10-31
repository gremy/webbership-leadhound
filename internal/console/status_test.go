package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// statusNow is the clock every harness judges Status ages by.
var statusNow = time.Date(2026, 10, 24, 14, 32, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// minsAgo is statusNow less m minutes.
func minsAgo(m int) *time.Time { return ptr(statusNow.Add(-time.Duration(m) * time.Minute)) }

// okChecks is a mailbox check that passed every part.
func okChecks() []CheckPart {
	return []CheckPart{{"SMTP", true}, {"IMAP", true}, {"folders", true}, {"SPF", true}, {"DKIM", true}, {"DMARC", true}}
}

// richStatus is a Status with every area read and every area good, its words set to text.
func richStatus(text string) Status {
	return Status{
		Mailboxes: MailboxesStatus{UpdatedAt: minsAgo(2), Rows: []MailboxStatus{
			{ID: 1, Name: text, Address: "sam@send-one.example", State: StatusMailboxActive, CheckedAt: minsAgo(60), Checks: okChecks(),
				SentToday: 18, RampToday: 18, Ready: true, HardBounces7d: 1, Sent7d: 160, SyncedAt: minsAgo(9)},
			{ID: 2, Name: "Mailbox 2", Address: "ana@send-two.example", State: StatusMailboxActive, CheckedAt: minsAgo(60), Checks: okChecks(),
				SentToday: 4, RampToday: 20, Ready: true, SyncedAt: minsAgo(9)},
			{ID: 3, Name: "Mailbox 3", Address: "kit@send-two.example", State: StatusMailboxRetired},
		}},
		Server: ServerStatus{UpdatedAt: minsAgo(1),
			Units: []UnitStatus{
				{Name: "leads-send", LastRun: minsAgo(12), Result: UnitOK, Note: text, Next: ptr(statusNow.Add(8 * time.Minute)), Switch: SwitchOn},
				{Name: "jobs-search", LastRun: minsAgo(400), Result: UnitOK, Switch: SwitchOff},
				{Name: "leads-verify-balance", Result: UnitNever, Switch: SwitchUnknown},
			},
			DBBytes: 1200 << 20, DBMaxBytes: 4 << 30, DiskFreeBytes: 31 << 30, DiskTotalBytes: 40 << 30, DiskFloorPct: 10,
			Load: ptr(0.21), MemoryPct: ptr(38.0), Started: statusNow.Add(-73 * time.Hour), Version: "4f2c9e1"},
		Claude: ClaudeStatus{UpdatedAt: minsAgo(0), TodayMicros: 1_840_000, DailyCapMicros: 5_000_000,
			ByPurpose:   []PurposeCap{{"drafting", 1_100_000, 3_000_000}, {"research", 740_000, 2_000_000}},
			MonthMicros: 23_400_000, Topup: &ClaudeTopup{AmountCents: 5000, At: testDay}, SinceTopupMicros: 23_400_000},
		Reoon: ReoonStatus{FetchedAt: minsAgo(32), APIStatus: "active", DailyCredits: 140, InstantCredits: 480,
			ChecksToday: ReoonChecks{Valid: 31, Invalid: 3, Other: 2}},
		Jev: JevStatus{UpdatedAt: minsAgo(30), JobsCalls: 120, JobsCap: 300, LeadsCalls: 1400, LeadsCap: 10000},
	}
}

// areaOf is the area id's entry of statusAreas(st, statusNow).
func areaOf(t *testing.T, st Status, id string) statusArea {
	t.Helper()
	for _, a := range statusAreas(st, statusNow) {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no area %s", id)
	return statusArea{}
}

// TestStatusThresholds: every rule of the threshold table on both sides of its boundary.
func TestStatusThresholds(t *testing.T) {
	mb := func(f func(*MailboxStatus)) Status {
		st := richStatus("Mailbox 1")
		f(&st.Mailboxes.Rows[0])
		return st
	}
	tests := []struct {
		name string
		st   Status
		area string
		want statusLevel
		line string // a part of the area's line; "" not checked
	}{
		{"all good", richStatus("Mailbox 1"), areaMailboxes, levelGood, "All 2 mailboxes OK"},

		{"sync 45 min is fine", mb(func(m *MailboxStatus) { m.SyncedAt = minsAgo(45) }), areaMailboxes, levelGood, ""},
		{"sync 46 min is amber", mb(func(m *MailboxStatus) { m.SyncedAt = minsAgo(46) }), areaMailboxes, levelWarn, "Mailbox 1 replies last synced 46 min ago"},
		{"sync 2 h is amber", mb(func(m *MailboxStatus) { m.SyncedAt = minsAgo(120) }), areaMailboxes, levelWarn, ""},
		{"sync 2 h 1 min is red", mb(func(m *MailboxStatus) { m.SyncedAt = minsAgo(121) }), areaMailboxes, levelBad, "Mailbox 1 replies last synced 2 h ago"},
		{"never synced is grey", mb(func(m *MailboxStatus) { m.SyncedAt = nil }), areaMailboxes, levelGrey, "Mailbox 1 replies not synced yet"},
		{"one paused is amber", mb(func(m *MailboxStatus) { m.State, m.Reason = StatusMailboxPaused, "3 bounces in 24 h" }), areaMailboxes, levelWarn, "Mailbox 1 paused: 3 bounces in 24 h"},
		{"every live one paused is red", func() Status {
			st := richStatus("Mailbox 1")
			st.Mailboxes.Rows[0].State, st.Mailboxes.Rows[1].State = StatusMailboxPaused, StatusMailboxPaused
			return st
		}(), areaMailboxes, levelBad, "All mailboxes paused"},
		{"a failed check is amber", mb(func(m *MailboxStatus) { m.Checks[4].OK = false }), areaMailboxes, levelWarn, "Mailbox 1 failed DKIM"},
		{"a retired mailbox is never judged", mb(func(m *MailboxStatus) { m.State, m.SyncedAt = StatusMailboxRetired, minsAgo(500) }), areaMailboxes, levelGood, "The mailbox is OK"},

		{"credits 20 is fine", func() Status { st := richStatus("x"); st.Reoon.DailyCredits = 20; return st }(), areaReoon, levelGood, "20 daily credits left"},
		{"credits 19 is amber", func() Status { st := richStatus("x"); st.Reoon.DailyCredits = 19; return st }(), areaReoon, levelWarn, "19 daily credits left"},
		{"credits 1 is amber", func() Status { st := richStatus("x"); st.Reoon.DailyCredits = 1; return st }(), areaReoon, levelWarn, ""},
		{"credits 0 is red", func() Status { st := richStatus("x"); st.Reoon.DailyCredits = 0; return st }(), areaReoon, levelBad, "No daily credits left"},

		{"Claude 80% is fine", func() Status { st := richStatus("x"); st.Claude.TodayMicros = 4_000_000; return st }(), areaClaude, levelGood, "$4.00 of $5.00 today"},
		{"Claude over 80% is amber", func() Status { st := richStatus("x"); st.Claude.TodayMicros = 4_000_001; return st }(), areaClaude, levelWarn, "of $5.00 today"},
		{"Claude under the cap is amber", func() Status { st := richStatus("x"); st.Claude.TodayMicros = 4_999_999; return st }(), areaClaude, levelWarn, ""},
		{"Claude at the cap is red", func() Status { st := richStatus("x"); st.Claude.TodayMicros = 5_000_000; return st }(), areaClaude, levelBad, "$5.00 of $5.00 today"},
		{"a purpose over 80% is amber", func() Status {
			st := richStatus("x")
			st.Claude.ByPurpose[1].Micros = 1_700_000
			return st
		}(), areaClaude, levelWarn, "Research $1.70 of $2.00 today"},
		{"the month never colours", func() Status { st := richStatus("x"); st.Claude.MonthMicros = 1e12; return st }(), areaClaude, levelGood, ""},
		{"no daily cap never colours", func() Status {
			st := richStatus("x")
			st.Claude.DailyCapMicros, st.Claude.TodayMicros = 0, 9_000_000
			return st
		}(), areaClaude, levelGood, "$9.00 today"},

		{"disk at the floor is fine", func() Status { st := richStatus("x"); st.Server.DiskFreeBytes = 4 << 30; return st }(), areaServer, levelGood, "All units ran clean"},
		{"disk under the floor is red", func() Status { st := richStatus("x"); st.Server.DiskFreeBytes = 4<<30 - 1; return st }(), areaServer, levelBad, "Free disk 10%, under the 10% floor"},
		{"DB at 80% is fine", func() Status {
			st := richStatus("x")
			st.Server.DBBytes, st.Server.DBMaxBytes = 800, 1000
			return st
		}(), areaServer, levelGood, ""},
		{"DB over 80% is amber", func() Status {
			st := richStatus("x")
			st.Server.DBBytes, st.Server.DBMaxBytes = 801, 1000
			return st
		}(), areaServer, levelWarn, "Database"},
		{"DB at its max is amber", func() Status {
			st := richStatus("x")
			st.Server.DBBytes, st.Server.DBMaxBytes = 1000, 1000
			return st
		}(), areaServer, levelWarn, ""},
		{"DB over its max is red", func() Status {
			st := richStatus("x")
			st.Server.DBBytes, st.Server.DBMaxBytes = 1001, 1000
			return st
		}(), areaServer, levelBad, "over its"},
		{"a failed unit is red", func() Status { st := richStatus("x"); st.Server.Units[1].Result = UnitFailed; return st }(), areaServer, levelBad, "jobs-search failed"},
		{"a running unit is fine", func() Status { st := richStatus("x"); st.Server.Units[1].Result = UnitRunning; return st }(), areaServer, levelGood, ""},

		{"Jev under the cap is fine", func() Status { st := richStatus("x"); st.Jev.JobsCalls = 299; return st }(), areaJev, levelGood, "299 of 300 for jobs"},
		{"Jev at the jobs cap is amber", func() Status { st := richStatus("x"); st.Jev.JobsCalls = 300; return st }(), areaJev, levelWarn, "Jobs at the cap: 300 of 300 calls"},
		{"Jev over the leads cap is amber", func() Status { st := richStatus("x"); st.Jev.LeadsCalls = 10_001; return st }(), areaJev, levelWarn, "Leads at the cap"},
		{"Jev with no cap is fine", func() Status {
			st := richStatus("x")
			st.Jev.JobsCap, st.Jev.JobsCalls = 0, 5000
			return st
		}(), areaJev, levelGood, "5,000 for jobs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := areaOf(t, tt.st, tt.area)
			if a.Level != tt.want || !strings.Contains(a.Line, tt.line) {
				t.Errorf("%s: %s %q, want %s with %q", tt.area, a.Level.Class(), a.Line, tt.want.Class(), tt.line)
			}
		})
	}
}

// TestStatusColdStart: with nothing read, every area is grey and says so; nothing is red.
func TestStatusColdStart(t *testing.T) {
	areas := statusAreas(Status{}, statusNow)
	if len(areas) != 5 {
		t.Fatalf("%d areas", len(areas))
	}
	for _, a := range areas {
		if a.Level != levelGrey || a.Line == "" {
			t.Errorf("%s: %s %q, want grey", a.ID, a.Level.Class(), a.Line)
		}
	}
	// Mailboxes on the record but never checked or synced are grey too.
	st := Status{Mailboxes: MailboxesStatus{Rows: []MailboxStatus{{Name: "Mailbox 1", State: StatusMailboxActive}}}}
	if a := areaOf(t, st, areaMailboxes); a.Level != levelGrey {
		t.Errorf("never-checked mailboxes: %s %q", a.Level.Class(), a.Line)
	}
}

// TestStatusUnknownIsNotGrey: an error newer than the last good reading is unknown, amber
// with the error; an older error leaves the reading's own colour; an error with no reading
// at all is unknown, never grey.
func TestStatusUnknownIsNotGrey(t *testing.T) {
	st := richStatus("x")
	st.Reoon.LastError, st.Reoon.LastErrorAt = "HTTP 503", minsAgo(5)
	if a := areaOf(t, st, areaReoon); a.Level != levelWarn || a.Line != "Unknown: HTTP 503" {
		t.Errorf("newer error: %s %q", a.Level.Class(), a.Line)
	}
	st.Reoon.LastErrorAt = minsAgo(60)
	if a := areaOf(t, st, areaReoon); a.Level != levelGood {
		t.Errorf("older error: %s %q", a.Level.Class(), a.Line)
	}
	st.Reoon = ReoonStatus{LastError: "invalid key", LastErrorAt: minsAgo(5)}
	if a := areaOf(t, st, areaReoon); a.Level != levelWarn || !strings.Contains(a.Line, "invalid key") {
		t.Errorf("error, never read: %s %q", a.Level.Class(), a.Line)
	}
	st.Claude.LastError, st.Claude.LastErrorAt = "credit balance too low", minsAgo(-1)
	if a := areaOf(t, st, areaClaude); a.Level != levelWarn || a.Line != "Unknown: credit balance too low" {
		t.Errorf("Claude newer error: %s %q", a.Level.Class(), a.Line)
	}
}

// TestStatusWorstFirst: the areas come worst first, grey after amber, and areas that stand
// the same keep their order.
func TestStatusWorstFirst(t *testing.T) {
	st := richStatus("x")
	st.Jev = JevStatus{}                        // grey
	st.Reoon.DailyCredits = 14                  // amber
	st.Server.Units[0].Result = UnitFailed      // red
	st.Mailboxes.Rows[0].SyncedAt = minsAgo(50) // amber
	var got []string
	for _, a := range statusAreas(st, statusNow) {
		got = append(got, a.ID+"="+a.Level.Class())
	}
	want := "server=bad mailboxes=warn reoon=warn jev=grey claude=good"
	if strings.Join(got, " ") != want {
		t.Errorf("order %v, want %s", got, want)
	}
}

// statusDots reads, per area anchor, the dot the dashboard's status card shows and the dot
// each Status page section's heading shows.
var (
	dashDot    = regexp.MustCompile(`<a href="/status#(\w+)"><span class="dot dot-(\w+)"></span>`)
	sectionDot = regexp.MustCompile(`<h2 id="(\w+)-h"><span class="dot dot-(\w+)"></span>`)
)

func dots(re *regexp.Regexp, body string) map[string]string {
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// TestStatusPageAndDashboardAgree: for every kind of Status, the dashboard's status card and
// the Status page show each area at the same level, the one statusAreas gives, in the same
// order.
func TestStatusPageAndDashboardAgree(t *testing.T) {
	bad := richStatus("x")
	bad.Server.Units[0].Result = UnitFailed
	bad.Reoon.DailyCredits = 3
	bad.Mailboxes.Rows[1].SyncedAt = minsAgo(300)
	unknown := richStatus("x")
	unknown.Reoon.LastError, unknown.Reoon.LastErrorAt = "timeout", minsAgo(1)
	for name, st := range map[string]Status{"cold": {}, "good": richStatus("x"), "bad": bad, "unknown": unknown} {
		t.Run(name, func(t *testing.T) {
			b := richBackend("x")
			b.status = st
			h := newHarness(t, b, false)
			c := h.session(t)
			dw, sw := h.do(get("/", c)), h.do(get("/status", c))
			if dw.Code != http.StatusOK || sw.Code != http.StatusOK {
				t.Fatalf("dashboard %d, status %d", dw.Code, sw.Code)
			}
			dash, page := dots(dashDot, dw.Body.String()), dots(sectionDot, sw.Body.String())
			for _, a := range statusAreas(st, statusNow) {
				if dash[a.ID] != a.Level.Class() || page[a.ID] != a.Level.Class() {
					t.Errorf("%s: dashboard %q, page %q, statusAreas %q", a.ID, dash[a.ID], page[a.ID], a.Level.Class())
				}
				if !strings.Contains(dw.Body.String(), `<span class="what">`+a.Line+`</span>`) && !strings.Contains(a.Line, "'") {
					t.Errorf("%s: the dashboard lacks the line %q", a.ID, a.Line)
				}
			}
			var dOrder, pOrder []string
			for _, m := range dashDot.FindAllStringSubmatch(dw.Body.String(), -1) {
				dOrder = append(dOrder, m[1])
			}
			for _, m := range sectionDot.FindAllStringSubmatch(sw.Body.String(), -1) {
				pOrder = append(pOrder, m[1])
			}
			if fmt.Sprint(dOrder) != fmt.Sprint(pOrder) || len(dOrder) != 5 {
				t.Errorf("dashboard order %v, page order %v", dOrder, pOrder)
			}
		})
	}
}

// TestStatusPageColdStart: with nothing read the page answers 200 with every section grey,
// "Last updated never" and a real empty state, and asks for a top-up.
func TestStatusPageColdStart(t *testing.T) {
	h := newHarness(t, &fakeBackend{}, false)
	w := h.do(get("/status", h.session(t)))
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	if n := strings.Count(body, "Last updated never"); n != 5 {
		t.Errorf("%d sections say never, want 5", n)
	}
	for _, want := range []string{"No mailboxes yet", "No balance read yet", "No Claude spend yet", "No Jev calls yet",
		"The units, the database and the disk show here", "Remaining credit needs your last top-up", `id="topup" method="post" action="/status/claude-topup#topup"`,
		`action="/status/refresh#reoon"`, `href="https://console.anthropic.com/settings/billing"`} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	for id, c := range dots(sectionDot, body) {
		if c != "grey" {
			t.Errorf("%s is %s on a cold start", id, c)
		}
	}
	if strings.Contains(body, "state-bad") || strings.Contains(body, "state-warn") || strings.Contains(body, "is-bad") || strings.Contains(body, "is-warn") {
		t.Error("a cold start shows amber or red")
	}
}

// TestStatusPageFull: a full Status shows every section's figures.
func TestStatusPageFull(t *testing.T) {
	h := newHarness(t, richBackend("Mailbox 1"), false)
	body := h.do(get("/status", h.session(t))).Body.String()
	for _, want := range []string{
		"Last updated 14:30 (2 min ago)", "sam@send-one.example", "All 6 checks OK", "18 / 18", "1 · 0.6%", "9 min ago", "Retired",
		"leads-send", "OK · Mailbox 1", "jobs-search", "Never ran", "1.2 GB</strong> of 4.0 GB", "31.0 GB</strong> of 40.0 GB · floor 10%",
		"Load 0.21 · memory 38% · console up 3 days · version 4f2c9e1",
		"$1.84 <small>of $5.00</small>", "$23.40", "$26.60", "estimate · top-up $50.00 on 24 Oct", "Drafting", "$1.10</strong> of $3.00",
		"140", "480", "31 valid · 3 invalid · 2 other",
		"Calls for jobs", "<strong>120</strong> of 300", "Calls for leads",
		`<input class="input" id="topup-date" name="date" type="date" value="2026-10-24" max="2026-10-24" required>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, "Remaining credit needs your last top-up") {
		t.Error("asks for a top-up with one recorded")
	}
}

// TestDashboardWidgets: the dashboard shows each widget's figures, and their empty states on
// a cold start.
func TestDashboardWidgets(t *testing.T) {
	b := richBackend("Willow Lane Physio")
	b.dash.Leads = &LeadsSummary{DueEmails: 12, SentToday: 22, CapToday: 48, Replies: 2, CallsDue: 3}
	h := newHarness(t, b, false)
	body := h.do(get("/", h.session(t))).Body.String()
	for _, want := range []string{
		"Good afternoon", "Saturday 24 October, 14:32",
		"<dt>Due emails</dt><dd>12</dd>", "22 <small>of 48</small>", "<dt>Calls due</dt><dd>3</dd>",
		`<span class="badge badge-warn">3</span>`, `<a class="list-title" href="/leads/5">Willow Lane Physio</a>`, "26 h ago", "2 more leads wait on Today",
		`<span class="bar-fill w-100"></span></span><span class="num">212</span>`, `<span class="bar-fill w-40"></span></span><span class="num">84</span>`,
		`<span class="bar-fill w-5"></span></span><span class="num">6</span>`, `<span class="bar-fill w-0"></span></span><span class="num">0</span>`,
		"$23.40", "Claude, since 1 Oct", "<strong>36</strong> of 50", "4 <small>new today</small>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}

	if strings.Contains(body, "No room today") || strings.Contains(body, `<p class="hint mt-1 mb-0">`) {
		t.Error("a no-room note shows with room today")
	}
	b.dash.Leads = &LeadsSummary{DueEmails: 12, CapToday: 0, NoRoom: "Today's domain check has not run for mail.northwind.example yet."}
	body = h.do(get("/", h.session(t))).Body.String()
	if want := `0 <small>of 0</small></dd></div>`; !strings.Contains(body, want) {
		t.Errorf("dashboard lacks %q", want)
	}
	tally := strings.Index(body, "<dt>Calls due</dt>")
	note := strings.Index(body, `<p class="hint mt-1 mb-0">Today&#39;s domain check has not run for mail.northwind.example yet.</p>`)
	if note < tally || tally < 0 {
		t.Errorf("the no-room note is not under the tally: tally %d, note %d", tally, note)
	}
	// A note asking for Check now links the one mailbox it is about, else the list; the card
	// is no longer one link, so the note's link does not sit inside another.
	for _, tt := range []struct {
		mailbox int64
		want    string
	}{{3, `<a href="/mailboxes/3">Open its mailbox</a></p>`}, {0, `<a href="/mailboxes">Open the mailboxes</a></p>`}} {
		b.dash.Leads = &LeadsSummary{NoRoom: "Today's domain check has not run.", NoRoomCheck: true, NoRoomMailbox: tt.mailbox}
		body = h.do(get("/", h.session(t))).Body.String()
		if !strings.Contains(body, tt.want) || strings.Contains(body, `<a class="card span-4" href="/leads/home"`) ||
			!strings.Contains(body, `<a class="card-link" href="/leads/home">`) {
			t.Errorf("mailbox %d: the note lacks %q or the card is still a link", tt.mailbox, tt.want)
		}
	}

	h = newHarness(t, &fakeBackend{}, false)
	w := h.do(get("/", h.session(t)))
	body = w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("cold dashboard: %d", w.Code)
	}
	for _, want := range []string{"Leads are not set up on this server yet", "Nothing needs you", "No emails sent in the last 30 days",
		"The day's numbers are on their way.", noBriefYet, `<span class="what">Not checked yet</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("cold dashboard lacks %q", want)
		}
	}
	if strings.Contains(body, "badge-warn") {
		t.Error("an empty Needs me shows a count")
	}
}

// TestDashboardStatusUnavailable: a failed Status read leaves the dashboard up, its status
// card grey, and is logged; Leads and Jobs home never read Status.
func TestDashboardStatusUnavailable(t *testing.T) {
	b := richBackend("x")
	b.statusErr = errors.New("db gone")
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(get("/", c))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `<span class="dot dot-grey"></span><span class="area">Status</span><span class="what">Status unavailable</span>`) {
		t.Errorf("status %d, want the grey card", w.Code)
	}
	if strings.Contains(body, "db gone") || !strings.Contains(h.logs.String(), "read=status") {
		t.Error("the error is shown, or not logged")
	}
	before := b.n("Status")
	for _, p := range []string{"/leads/home", "/jobs/home"} {
		if w := h.do(get(p, c)); w.Code != http.StatusOK {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if b.n("Status") != before {
		t.Error("Leads or Jobs home read Status")
	}
	if w := h.do(get("/status", c)); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "db gone") {
		t.Errorf("/status with the read failing: %d", w.Code)
	}
}

func TestParseDollars(t *testing.T) {
	for _, tt := range []struct {
		in    string
		cents int64
		ok    bool
	}{
		{"50", 5000, true}, {"50.00", 5000, true}, {"50.5", 5050, true}, {" 12.34 ", 1234, true}, {"$20", 2000, true},
		{"0.01", 1, true}, {"100000", 100_000_00, true}, {"100000.00", 100_000_00, true},
		{"", 0, false}, {"0", 0, false}, {"0.00", 0, false}, {"-5", 0, false}, {"5.", 0, false}, {".5", 0, false},
		{"5.001", 0, false}, {"1e3", 0, false}, {"50,00", 0, false}, {"fifty", 0, false}, {"100000.01", 0, false},
		{"99999999", 0, false}, {"9223372036854775807", 0, false}, {"1 000", 0, false}, {"+5", 0, false},
	} {
		cents, refusal := parseDollars(tt.in)
		if ok := refusal == ""; ok != tt.ok || cents != tt.cents {
			t.Errorf("parseDollars(%q) = %d %q, want %d ok=%v", tt.in, cents, refusal, tt.cents, tt.ok)
		}
		if !tt.ok && (!strings.HasSuffix(refusal, ".") || strings.Contains(refusal, "\n")) {
			t.Errorf("parseDollars(%q): refusal %q is not one sentence", tt.in, refusal)
		}
	}
}

// TestClaudeTopup: a good amount and day reach the backend in cents at midnight of that day,
// and the outcome comes back as a flash on /status; a bad amount or a day after today is
// refused on the page and nothing is recorded; the backend's own refusal shows too.
func TestClaudeTopup(t *testing.T) {
	b := richBackend("x")
	b.topupOut = ControlOutcome{Message: "Top-up of $50.00 recorded."}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/status/claude-topup", url.Values{"amount": {"50"}, "date": {"2026-10-20"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/status#claude" {
		t.Fatalf("got %d to %q", w.Code, w.Header().Get("Location"))
	}
	if len(b.topups) != 1 || b.topups[0] != (topupCall{5000, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}) {
		t.Errorf("topups %+v", b.topups)
	}
	if body := h.do(get("/status", c, cookieNamed(w, "__Host-lh_flash"))).Body.String(); !strings.Contains(body, "Top-up of $50.00 recorded.") {
		t.Error("the outcome is not shown")
	}
	if w := h.do(post("/status/claude-topup", url.Values{"amount": {"12.5"}, "date": {""}}, c)); w.Code != http.StatusSeeOther ||
		b.topups[1] != (topupCall{1250, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}) {
		t.Errorf("no date is today: %d %+v", w.Code, b.topups)
	}

	for _, tt := range []struct {
		f     url.Values
		field string
	}{
		{url.Values{"amount": {"abc"}, "date": {"2026-10-20"}}, "topup-amount"},
		{url.Values{"amount": {"5.001"}, "date": {"2026-10-20"}}, "topup-amount"},
		{url.Values{"amount": {"0"}, "date": {"2026-10-20"}}, "topup-amount"},
		{url.Values{"amount": {"50"}, "date": {"2026-10-25"}}, "topup-date"},
		{url.Values{"amount": {"50"}, "date": {"20/10/2026"}}, "topup-date"},
	} {
		w := h.do(post("/status/claude-topup", tt.f, c))
		body := w.Body.String()
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d, want 422", tt.f, w.Code)
			continue
		}
		topupRefusalIn(t, body, tt.f.Get("amount"), tt.f.Get("date"), tt.field)
	}
	if len(b.topups) != 2 {
		t.Errorf("a refused form reached the backend: %+v", b.topups)
	}
	b.topupErr = fmt.Errorf("%w: That top-up is already recorded.", ErrUserFixable)
	w = h.do(post("/status/claude-topup", url.Values{"amount": {"50"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "That top-up is already recorded.") {
		t.Errorf("backend refusal: %d", w.Code)
	}
	topupRefusalIn(t, w.Body.String(), "50", "", "")
	if logs := h.logs.String(); strings.Contains(logs, "5000") || !strings.Contains(logs, "action=topup") {
		t.Errorf("log %q", logs)
	}
}

// topupRefusalIn checks a refused top-up's page: the refusal line sits inside the Claude
// section, after its heading and before the fields, as an alert; the typed amount (and date)
// are back in the fields; the field the line is about (field, "" for none) is marked invalid
// and described by it, and no other is; nothing shows at the top of the page.
func topupRefusalIn(t *testing.T, body, amount, date, field string) {
	t.Helper()
	head := strings.Index(body, `id="claude-h"`)
	alert := strings.Index(body, `<div class="alert alert-stop" role="alert">`)
	line := strings.Index(body, `id="topup-error"`)
	form := strings.Index(body, `id="topup" method="post" action="/status/claude-topup#topup"`)
	fields := strings.Index(body, `id="topup-amount"`)
	if head < 0 || alert < head || form < head || alert < form || line < alert || fields < line {
		t.Errorf("refusal not between the Claude heading (%d), the form (%d) and its fields (%d): alert %d, line %d", head, form, fields, alert, line)
	}
	if strings.Contains(body, `notice notice-error`) {
		t.Error("the refusal also shows at the top of the page")
	}
	if !strings.Contains(body, `name="amount" value="`+amount+`"`) {
		t.Errorf("the amount %q typed is not back in its field", amount)
	}
	if date != "" && !strings.Contains(body, `name="date" type="date" value="`+date+`"`) {
		t.Errorf("the date %q typed is not back in its field", date)
	}
	for _, id := range []string{"topup-amount", "topup-date"} {
		i := strings.Index(body, `id="`+id+`"`)
		tag := body[i : i+strings.Index(body[i:], ">")]
		marked := strings.Contains(tag, `aria-invalid="true" aria-describedby="topup-error"`)
		if marked != (id == field) {
			t.Errorf("%s marked invalid %v, want %v: %s", id, marked, id == field, tag)
		}
	}
}

// TestRefreshStatus: Refresh asks the backend and shows its line; a refusal shows on the
// page; a failure is a plain 500.
func TestRefreshStatus(t *testing.T) {
	b := richBackend("x")
	b.refreshOut = ControlOutcome{Message: "Asked for a fresh balance; it shows within a minute."}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/status/refresh", nil, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/status#reoon" || b.n("RefreshStatus") != 1 {
		t.Fatalf("got %d to %q", w.Code, w.Header().Get("Location"))
	}
	if body := h.do(get("/status", c, cookieNamed(w, "__Host-lh_flash"))).Body.String(); !strings.Contains(body, "Asked for a fresh balance; it shows within a minute.") {
		t.Error("the outcome is not shown")
	}
	b.refreshErr = fmt.Errorf("%w: A refresh is already asked.", ErrUserFixable)
	w = h.do(post("/status/refresh", nil, c))
	body := w.Body.String()
	start := strings.Index(body, `id="reoon-h"`)
	section := ""
	if start >= 0 {
		section = body[start:]
		section = section[:strings.Index(section, "</section>")]
	}
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(section, `<div class="alert alert-stop" role="alert">`) ||
		!strings.Contains(section, "A refresh is already asked.") || strings.Contains(body, "notice notice-error") {
		t.Errorf("refusal %d not inside the Reoon section as an alert:\n%s", w.Code, body)
	}
	// While a refresh waits, its "already asked" refusal is a notice, not a stop, and it
	// replaces the "Refresh asked at" line rather than repeating it; with no refusal the line
	// shows, and an empty balance says one is asked for rather than "Refresh asks for one now".
	asked := time.Date(2026, 10, 24, 9, 41, 0, 0, time.UTC)
	b.status.Reoon = ReoonStatus{RefreshAskedAt: &asked}
	b.refreshErr = fmt.Errorf("%w: A fresh Reoon balance was already asked at 09:41.", ErrUserFixable)
	w = h.do(post("/status/refresh", nil, c))
	body = w.Body.String()
	section = body[strings.Index(body, `id="reoon-h"`):]
	section = section[:strings.Index(section, "</section>")]
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(section, `<div class="alert alert-info" role="status">`) ||
		strings.Contains(section, "alert-stop") || !strings.Contains(section, "already asked at 09:41.") ||
		strings.Contains(section, "Refresh asked at") || strings.Contains(section, "Refresh asks for one now") ||
		!strings.Contains(section, "One has been asked for and shows here once the check runs.") {
		t.Errorf("already-asked refusal %d not one notice:\n%s", w.Code, section)
	}
	body = h.do(get("/status", c)).Body.String()
	section = body[strings.Index(body, `id="reoon-h"`):]
	section = section[:strings.Index(section, "</section>")]
	if strings.Count(section, "asked") != 2 || !strings.Contains(section, "Refresh asked at 09:41") || strings.Contains(section, "alert") {
		t.Errorf("a waiting refresh shows other than its line and the empty state:\n%s", section)
	}
	b.refreshErr = errors.New("disk full")
	if w := h.do(post("/status/refresh", nil, c)); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "disk full") {
		t.Errorf("failure: %d", w.Code)
	}
	if b.n("RefreshStatus") != 4 {
		t.Errorf("RefreshStatus called %d times", b.n("RefreshStatus"))
	}
}

func TestWidthClass(t *testing.T) {
	for _, tt := range []struct {
		n, total int64
		want     string
	}{
		{0, 100, "w-0"}, {5, 0, "w-0"}, {-1, 10, "w-0"}, {1, 1000, "w-5"}, {19, 212, "w-10"}, {6, 212, "w-5"},
		{84, 212, "w-40"}, {212, 212, "w-100"}, {300, 212, "w-100"}, {12, 100, "w-10"}, {13, 100, "w-15"},
	} {
		if got := widthClass(tt.n, tt.total); got != tt.want {
			t.Errorf("widthClass(%d, %d) = %s, want %s", tt.n, tt.total, got, tt.want)
		}
	}
}

// TestNewTemplatesUseKnownClasses: every class the Status page and the dashboard name, the
// level suffixes included, has a rule in console.css.
func TestNewTemplatesUseKnownClasses(t *testing.T) {
	css, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([a-zA-Z][\w-]*)`).FindAllStringSubmatch(string(css), -1) {
		have[m[1]] = true
	}
	// Classes the console styles by element or context, not by a rule of their own.
	// text-grey never renders: no figure shown in colour (credits, spend) is ever grey.
	plain := map[string]bool{"num": true, "area": true, "what": true, "text-grey": true}
	for _, name := range []string{"templates/status.html", "templates/dashboard.html"} {
		b, err := templateFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(string(b), -1) {
			v := m[1]
			for _, l := range []string{"good", "warn", "bad", "grey"} {
				v = strings.NewReplacer("{{.Level.Class}}", l, "{{$l.Level.Class}}", l, "{{$l.Class}}", l, "{{$pl.Class}}", l).Replace(v)
				for _, c := range strings.Fields(regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(v, " ")) {
					if !have[c] && !plain[c] {
						t.Errorf("%s: class %q has no rule in console.css", name, c)
					}
				}
				v = m[1]
			}
		}
	}
}
