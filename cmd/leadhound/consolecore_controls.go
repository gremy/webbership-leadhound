package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The console's Controls: every on/off that governs leads, read live. The switches are rows
// in the database (job_switch, send_pause); the timers are read from systemd, which the
// console may ask without any privilege (systemctl show over its socket), and only when that
// fails are they worked out from the server's settings as install.sh applies them; the guards
// set on the server are shown, never changed. whyNotSending is the one sentence about
// sending the Controls page, the Leads page and a mailbox's card all show.

// The leads units, as deploy/ installs them.
const (
	unitBatch    = "leadhound-leads-batch.timer"
	unitSync     = "leadhound-leads-sync.timer"
	unitProbe    = "leadhound-leads-probe.timer"
	unitContacts = "leadhound-leads-contacts.timer"
	unitSend     = "leadhound-leads-send.timer"
	unitResearch = "leadhound-leads-research.path"
)

// controlUnits are the units Controls asks systemd about, in one call.
var controlUnits = []string{unitBatch, unitSync, unitProbe, unitContacts, unitSend, unitResearch}

// controlRow is what Controls shows of one switch beyond its state: its words, its unit and
// the run commands whose latest run is its last run.
type controlRow struct {
	job      string // a leadstore.Job*, "" for sending
	what     string
	schedule string
	unit     string // "" when the job has no unit of its own yet
	commands []string
}

// controlSending is the sending switch's row.
var controlSending = controlRow{
	what:     "Sends the approved emails that are due, from their own mailbox, within the warm-up limits.",
	schedule: "Every hour from 10:00 to 19:00, Monday to Friday (Bucharest time).",
	unit:     unitSend, commands: []string{commandLeadsSend},
}

// controlJobs are the switched jobs' rows, in console.ControlJobs order.
var controlJobs = []controlRow{
	{job: leadstore.JobBatch, what: "Drafts the morning batch of emails for you to approve.",
		schedule: "Every day at 06:15 (Bucharest time).", unit: unitBatch, commands: []string{commandLeadsBatch}},
	{job: leadstore.JobSync, what: "Reads the mailboxes for replies, bounces and opt-outs.",
		schedule: "Every 15 minutes.", unit: unitSync, commands: []string{commandLeadsSync}},
	{job: leadstore.JobProbe, what: "Checks the listed sites for the Drupal version they run.",
		schedule: "Every day at 13:00 (Bucharest time).", unit: unitProbe, commands: []string{commandLeadsSitesProbe}},
	{job: leadstore.JobContacts, what: "Finds contact routes on the sites, reads their pages and rates the leads.",
		schedule: "Every day at 10:00 (Bucharest time).", unit: unitContacts,
		commands: []string{commandLeadsContacts, commandLeadsEnrich, commandLeadsRate}},
	{job: leadstore.JobResearch, what: "Looks for a contact for a starred lead that has none, with Claude.",
		schedule: "When you star a lead with no contact, or press Look again.", unit: unitResearch,
		commands: []string{commandLeadsResearch}},
	{job: leadstore.JobVerify, what: "Checks each email address before it can enter a batch.",
		schedule: "Not running yet: it comes with the email check."},
}

// unitState is one unit as systemctl show reports it.
type unitState struct {
	Loaded  bool       // LoadState loaded
	Enabled bool       // UnitFileState enabled (or enabled-runtime)
	Active  bool       // ActiveState active
	Next    *time.Time // a timer's next run; nil when none is planned
}

// timerRead is every control unit's state, from systemd (fromSystemd) or, when it could not
// be asked, worked out from the server's settings, with note saying so.
type timerRead struct {
	units       map[string]unitState
	fromSystemd bool
	note        string
}

// timersNotRead is the Controls line when systemd could not be asked.
const timersNotRead = "The timers could not be read from the server, so each one shows what the server's settings make the last deploy do."

// systemctlShow runs systemctl show for units, one block of properties each; tests swap it,
// so no test asks the machine's systemd. --timestamp=unix prints the next run as @seconds.
var systemctlShow = func(ctx context.Context, units []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, systemctlWait)
	defer cancel()
	args := append([]string{"show", "--timestamp=unix", "-p", "Id,LoadState,UnitFileState,ActiveState,NextElapseUSecRealtime"}, units...)
	return exec.CommandContext(ctx, "systemctl", args...).Output() // #nosec G204 -- fixed arguments and the fixed unit names
}

// systemctlWait is how long a page waits for systemd before it shows the timers inferred:
// systemctl answers in milliseconds, and a hung systemd must not hold the dashboard.
const systemctlWait = 2 * time.Second

// timersLogged makes a failed systemctl a single log line per process: on a machine without
// systemd (the local console) every page would log it again.
var timersLogged atomic.Bool

// controlsGetenv reads the server's environment for the guards and the timers' fallback;
// tests swap it.
var controlsGetenv = os.Getenv

// readTimers is the state of every control unit: from systemd, else inferred from the
// server's settings as install.sh's leads_timers applies them.
func readTimers(ctx context.Context, getenv func(string) string) timerRead {
	out, err := systemctlShow(ctx, controlUnits)
	var units map[string]unitState
	if err == nil {
		units, err = parseSystemctlShow(out, controlUnits)
	}
	if err == nil {
		return timerRead{units: units, fromSystemd: true}
	}
	if timersLogged.CompareAndSwap(false, true) {
		slog.Default().Warn("console timers: systemctl show failed; timers are inferred from the environment from now on until it answers", "err", err.Error())
	}
	inferred := map[string]unitState{}
	for unit, on := range inferredTimers(getenv) {
		inferred[unit] = unitState{Loaded: true, Enabled: on}
	}
	return timerRead{units: inferred, note: timersNotRead}
}

// inferredTimers is which leads units install.sh's leads_timers leaves enabled for the
// server's settings: probe and contacts with LEADHOUND_LEADS_TIMERS=on; batch, send and sync
// with that and LEADHOUND_OUTREACH=live. The research watcher is always enabled.
func inferredTimers(getenv func(string) string) map[string]bool {
	finding := strings.TrimSpace(getenv("LEADHOUND_LEADS_TIMERS")) == "on"
	sending := finding && strings.TrimSpace(getenv("LEADHOUND_OUTREACH")) == leadconfig.OutreachLive
	return map[string]bool{
		unitProbe: finding, unitContacts: finding,
		unitBatch: sending, unitSend: sending, unitSync: sending,
		unitResearch: true,
	}
}

// parseSystemctlShow reads systemctl show's output: one block of Key=Value lines per unit,
// blocks apart by a blank line, in the order units were asked (Id names each). A unit systemd
// does not know is loaded false. It fails when the output does not hold one block per unit.
func parseSystemctlShow(out []byte, units []string) (map[string]unitState, error) {
	var blocks []map[string]string
	cur := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			if len(cur) > 0 {
				blocks = append(blocks, cur)
				cur = map[string]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("systemctl show: a line with no =: %q", line)
		}
		cur[k] = v
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(blocks) != len(units) {
		return nil, fmt.Errorf("systemctl show: %d blocks for %d units", len(blocks), len(units))
	}
	res := make(map[string]unitState, len(units))
	for i, b := range blocks {
		id := cmp.Or(b["Id"], units[i])
		if !slices.Contains(units, id) {
			return nil, fmt.Errorf("systemctl show: unit %q was not asked for", id)
		}
		st := unitState{
			Loaded:  b["LoadState"] == "loaded",
			Enabled: b["UnitFileState"] == "enabled" || b["UnitFileState"] == "enabled-runtime",
			Active:  b["ActiveState"] == "active",
		}
		next, err := parseSystemdTime(b["NextElapseUSecRealtime"])
		if err != nil {
			return nil, err
		}
		st.Next = next
		res[id] = st
	}
	return res, nil
}

// systemdLayout is systemctl's own timestamp, as printed without --timestamp=unix.
const systemdLayout = "Mon 2006-01-02 15:04:05 MST"

// parseSystemdTime reads a timestamp systemctl show prints: @seconds (--timestamp=unix),
// microseconds, or its own layout in the machine's zone. Empty, n/a, 0 and infinity are no
// time (nil).
func parseSystemdTime(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "", "n/a", "0", "infinity":
		return nil, nil
	}
	if s, ok := strings.CutPrefix(v, "@"); ok {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("systemctl show: time %q: %w", v, err)
		}
		t := time.Unix(n, 0)
		return &t, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		t := time.UnixMicro(n)
		return &t, nil
	}
	t, err := time.ParseInLocation(systemdLayout, v, time.Local)
	if err != nil {
		return nil, fmt.Errorf("systemctl show: time %q: %w", v, err)
	}
	return &t, nil
}

// The reasons whyNotSending gives, in the order it checks them; the first that applies wins.
const (
	whyShadow     = iota + 1 // LEADHOUND_OUTREACH is not live
	whyPaused                // the sending switch is off
	whyNoTimer               // the send timer is not enabled
	whyNothing               // no approved live email waits
	whyMailbox               // a due email's mailbox is paused, retired or not checked
	whyUnassigned            // a due email's mailbox is not assigned to its campaign
	whyCap                   // today's warm-up limit is used
	whyHours                 // outside the sending window
	whyGoes                  // nothing stops them: they go at the next run
	whyNotDue                // approved emails wait, none due yet (a turn of whyNothing)
)

// sendWhy is whyNotSending's answer: Reason one of the why* reasons, Blocked whether
// something stops the waiting emails, the sentence, and when they next go (zero: no time).
type sendWhy struct {
	Reason   int
	Blocked  bool
	Sentence string
	Next     time.Time
}

// view is w as the console takes it.
func (w sendWhy) view() console.SendingWhy {
	v := console.SendingWhy{Blocked: w.Blocked, Sentence: w.Sentence}
	if !w.Next.IsZero() {
		next := w.Next
		v.Next = &next
	}
	return v
}

// sendFacts is everything whyNot reads, so the reasons are worked out in one place over
// stored rows; loadSendFacts reads them.
type sendFacts struct {
	live          bool
	paused        bool
	pausedAt      time.Time
	pauseReason   string
	sendTimer     bool       // the send timer is enabled
	sendNext      *time.Time // its next run; nil when unknown
	rows          []leadstore.SendRow
	mailboxes     map[int64]leadstore.MailboxStatus
	assigned      map[[2]int64]bool // (campaign, mailbox) pairs assigned
	campaigns     map[int64]string  // campaign names by id
	cfg           leadconfig.Config
	loc           *time.Location // the operator's zone
	now           time.Time
	mailboxScoped bool // rows are one mailbox's only
	firstSend     bool // caps.start_date first-send
}

// nextRoomDays is how many days ahead nextRoom looks for a day with room.
const nextRoomDays = 60

// whenWords is t as the Controls sentences say it: "Tue 27 Oct 10:00", in loc.
func whenWords(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon 2 Jan 15:04")
}

// emailsWait is "N approved emails wait" with its verb agreeing.
func emailsWait(n int) string {
	if n == 1 {
		return "1 approved email waits"
	}
	return fmt.Sprintf("%d approved emails wait", n)
}

// emailsGo is "N approved emails go" with its verb agreeing.
func emailsGo(n int) string {
	if n == 1 {
		return "1 approved email goes"
	}
	return fmt.Sprintf("%d approved emails go", n)
}

// sendReady reports whether mailbox m may send now, as a live send run's refresh decides:
// active and its latest check passed. A DNS check missing today is not a block: the send
// run makes it itself.
func sendReady(m leadstore.MailboxStatus) bool {
	return m.State == leadstore.MailboxActive && m.LastCheck != nil && m.LastCheck.Outcome == leadstore.CheckPassed
}

// mailboxBlockWords is why mailbox m sends nothing, in words: "paused (reason)", "retired",
// "a check is running", "its latest check has not passed".
func mailboxBlockWords(m leadstore.MailboxStatus) string {
	switch {
	case m.State == leadstore.MailboxRetired:
		return "it is retired"
	case m.State == leadstore.MailboxPaused && m.Reason != "":
		return "it is paused (" + m.Reason + ")"
	case m.State == leadstore.MailboxPaused:
		return "it is paused"
	case m.LastCheck != nil && m.LastCheck.Outcome == leadstore.CheckRequested:
		return "its check is still running"
	case m.LastCheck == nil:
		return "it has not been checked yet"
	}
	return "its latest check has not passed"
}

// rowLoc is the zone whose sending window a waiting email keeps: its recipient country's.
func (f sendFacts) rowLoc(r leadstore.SendRow) *time.Location {
	return f.cfg.Policy(r.OrgCountry).Location()
}

// loads is every mailbox as a send run's room reads it: one not ready to send (a retired one
// included) has no room, though its sends today still count toward its domain.
func (f sendFacts) loads() []outreach.MailboxLoad {
	out := make([]outreach.MailboxLoad, 0, len(f.mailboxes))
	for _, id := range slices.Sorted(maps.Keys(f.mailboxes)) {
		m := f.mailboxes[id]
		ml := outreach.MailboxLoad{ID: m.ID, Domain: m.Domain, SentToday: m.SentToday, Ready: sendReady(m)}
		if f.firstSend && m.FirstLiveSend != nil {
			fs := m.FirstLiveSend.In(f.loc)
			ml.FirstSend = &fs
		}
		out = append(out, ml)
	}
	return out
}

// whyNot is why nothing is going out (or when it next does), over facts read once: the first
// of the reasons that applies, in the order of the why* constants. It never reads the store.
//
// Reasons 5 and 6 name the mailbox (and campaign) holding the most due emails, the lowest id
// first on a tie: any due email held there is a block, even while others go out.
func whyNot(f sendFacts) sendWhy {
	loc := f.loc
	switch {
	case !f.live:
		return sendWhy{Reason: whyShadow, Blocked: true,
			Sentence: "Shadow mode: LEADHOUND_OUTREACH is not live on the server, so nothing is sent for real."}
	case f.paused && f.pauseReason != "":
		return sendWhy{Reason: whyPaused, Blocked: true,
			Sentence: fmt.Sprintf("Sending is off since %s: %s.", whenWords(f.pausedAt, loc), strings.TrimRight(f.pauseReason, "."))}
	case f.paused:
		return sendWhy{Reason: whyPaused, Blocked: true, Sentence: fmt.Sprintf("Sending is off since %s.", whenWords(f.pausedAt, loc))}
	case !f.sendTimer:
		return sendWhy{Reason: whyNoTimer, Blocked: true, Sentence: "The send timer isn't enabled on the server."}
	case len(f.rows) == 0 && f.mailboxScoped:
		return sendWhy{Reason: whyNothing, Blocked: true, Sentence: "No approved email uses this mailbox."}
	case len(f.rows) == 0:
		return sendWhy{Reason: whyNothing, Blocked: true, Sentence: "Nothing approved is waiting to go out."}
	}

	var due []leadstore.SendRow
	var nextDue time.Time
	for _, r := range f.rows {
		if !r.DueAt.After(f.now) {
			due = append(due, r)
		} else if nextDue.IsZero() || r.DueAt.Before(nextDue) {
			nextDue = *r.DueAt
		}
	}
	if len(due) == 0 {
		return sendWhy{Reason: whyNotDue, Blocked: true, Next: nextDue,
			Sentence: fmt.Sprintf("Nothing approved is due yet; the next email falls due %s.", whenWords(nextDue, loc))}
	}

	// 5: the due emails whose mailbox may not send.
	blocked := map[int64]int{}
	var ok []leadstore.SendRow
	for _, r := range due {
		if m, found := f.mailboxes[r.MailboxID]; !found || !sendReady(m) {
			blocked[r.MailboxID]++
			continue
		}
		ok = append(ok, r)
	}
	if id, n := mostOf(blocked); n > 0 {
		m, found := f.mailboxes[id]
		if !found {
			return sendWhy{Reason: whyMailbox, Blocked: true,
				Sentence: fmt.Sprintf("%s with no mailbox to send from: they were drafted before mailboxes.", emailsWait(n))}
		}
		return sendWhy{Reason: whyMailbox, Blocked: true,
			Sentence: fmt.Sprintf("%s on %s: %s.", emailsWait(n), m.Address, mailboxBlockWords(m))}
	}

	// 6: the due emails whose mailbox is not assigned to their campaign.
	unassigned := map[[2]int64]int{}
	var sendable []leadstore.SendRow
	for _, r := range ok {
		if !f.assigned[[2]int64{r.CampaignID, r.MailboxID}] {
			unassigned[[2]int64{r.MailboxID, r.CampaignID}]++
			continue
		}
		sendable = append(sendable, r)
	}
	if len(unassigned) > 0 {
		pairs := slices.SortedFunc(maps.Keys(unassigned), func(a, b [2]int64) int {
			return cmp.Or(cmp.Compare(unassigned[b], unassigned[a]), cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
		})
		p := pairs[0]
		name := cmp.Or(f.campaigns[p[1]], fmt.Sprintf("campaign %d", p[1]))
		return sendWhy{Reason: whyUnassigned, Blocked: true,
			Sentence: fmt.Sprintf("%s: %s is not assigned to %s.", emailsWait(unassigned[p]), f.mailboxes[p[0]].Address, name)}
	}

	// 7: today's warm-up limit, over the mailboxes the due emails use.
	bh := f.cfg.Schedule.BusinessHours
	local := f.now.In(loc)
	loads := f.loads()
	room := outreach.RoomToday(f.cfg, loads, local)
	used := map[int64]bool{}
	for _, r := range sendable {
		used[r.MailboxID] = true
	}
	left := 0
	for id := range used {
		left += room[id]
	}
	items := make([]outreach.PlanItem, 0, len(sendable))
	for _, r := range sendable {
		items = append(items, outreach.PlanItem{MessageID: r.ID, BatchID: r.BatchID, LeadID: r.LeadID, Step: r.Step,
			Domain: recipientDomain(r.Recipient()), Loc: f.rowLoc(r), DueAt: *r.DueAt, MailboxID: r.MailboxID})
	}
	plan, held := outreach.PlanMailboxes(items, room, f.now, bh)
	if left == 0 || len(plan) == 0 && held[outreach.HoldCap] > 0 && held[outreach.HoldHours] == 0 {
		return f.capWhy(used, loads, local, f.nextRoom(sendable, used, loads, local))
	}

	// 8: outside the sending window of every recipient.
	if len(plan) == 0 {
		var next time.Time
		for _, r := range sendable {
			if s := outreach.NextSlot(f.rowLoc(r), bh, f.now); !s.IsZero() && (next.IsZero() || s.Before(next)) {
				next = s
			}
		}
		if next.IsZero() {
			return sendWhy{Reason: whyHours, Blocked: true,
				Sentence: fmt.Sprintf("%s for the next sending window.", emailsWait(len(sendable)))}
		}
		return sendWhy{Reason: whyHours, Blocked: true, Next: next,
			Sentence: fmt.Sprintf("%s out in the next sending window, %s.", emailsGo(len(sendable)), whenWords(next, loc))}
	}

	// 9: nothing stops them.
	if f.sendNext == nil {
		return sendWhy{Reason: whyGoes, Sentence: fmt.Sprintf("%s out at the next send run.", emailsGo(len(plan)))}
	}
	return sendWhy{Reason: whyGoes, Next: *f.sendNext,
		Sentence: fmt.Sprintf("%s out at the next run, %s.", emailsGo(len(plan)), whenWords(*f.sendNext, loc))}
}

// mostOf is the key with the largest count, the lowest key on a tie; 0, 0 for an empty map.
func mostOf(counts map[int64]int) (int64, int) {
	var best int64
	n := 0
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		if counts[k] > n {
			best, n = k, counts[k]
		}
	}
	return best, n
}

// capWhy is reason 7 in the words of the limit that is used, the next send (next, zero when
// none is found) after it: a warm-up that has not started (caps.start_date still ahead) says
// when it starts; a sending domain whose daily limit is used while its mailboxes' own warm-up
// limits are not names the domain and its numbers (every mailbox on it counted, as the send
// counts them); else the warm-up limit of the mailboxes used, today's sends of their caps.
func (f sendFacts) capWhy(used map[int64]bool, loads []outreach.MailboxLoad, local, next time.Time) sendWhy {
	w := sendWhy{Reason: whyCap, Blocked: true, Next: next}
	end := "."
	if !next.IsZero() {
		end = "; the next send is " + whenWords(next, f.loc) + "."
	}
	date := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	if sd := f.cfg.Caps.StartDate; !sd.IsFirstSend() {
		switch {
		case sd.IsZero():
			w.Sentence = "The warm-up has no start date, so nothing is sent."
			return w
		case date(local).Before(date(sd.Time)):
			w.Sentence = "The warm-up starts on " + sd.Format("Mon 2 Jan") + "."
			return w
		}
	}
	ownSent, ownCap := 0, 0
	domainSent := map[string]int{}
	var domains []string
	for _, ml := range loads {
		d := strings.ToLower(strings.TrimSpace(ml.Domain))
		domainSent[d] += ml.SentToday
		if !used[ml.ID] {
			continue
		}
		ownSent += ml.SentToday
		ownCap += outreach.MailboxCap(f.cfg, ml, local)
		if !slices.Contains(domains, d) {
			domains = append(domains, d)
		}
	}
	if ownSent >= ownCap {
		w.Sentence = fmt.Sprintf("Today's warm-up limit is used (%d of %d)%s", ownSent, ownCap, end)
		return w
	}
	slices.Sort(domains)
	for _, d := range domains {
		if domainSent[d] >= outreach.DomainCap(f.cfg) {
			w.Sentence = fmt.Sprintf("Today's sending limit for %s is used (%d of %d)%s", d, domainSent[d], outreach.DomainCap(f.cfg), end)
			return w
		}
	}
	w.Sentence = "Today's sending limit is used" + end
	return w
}

// nextRoom is the first moment after today when one of the used mailboxes has room again and
// one of rows is inside its sending window: the first business day whose caps give room, at
// the earliest window start of rows' recipients. Zero when none falls within the look-ahead.
func (f sendFacts) nextRoom(rows []leadstore.SendRow, used map[int64]bool, loads []outreach.MailboxLoad, local time.Time) time.Time {
	bh := f.cfg.Schedule.BusinessHours
	fresh := make([]outreach.MailboxLoad, 0, len(loads))
	for _, ml := range loads {
		ml.SentToday = 0
		fresh = append(fresh, ml)
	}
	for i := 1; i <= nextRoomDays; i++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+i, 0, 0, 0, 0, local.Location())
		room := outreach.RoomToday(f.cfg, fresh, day.Add(12*time.Hour))
		left := 0
		for id := range used {
			left += room[id]
		}
		if left == 0 {
			continue
		}
		var next time.Time
		for _, r := range rows {
			s := outreach.NextSlot(f.rowLoc(r), bh, day)
			if !s.IsZero() && (next.IsZero() || s.Before(next)) {
				next = s
			}
		}
		// A slot past this day's end belongs to a later day whose room is checked there.
		if !next.IsZero() && next.Before(day.AddDate(0, 0, 1)) {
			return next
		}
	}
	return time.Time{}
}

// loadSendFacts reads what whyNot needs, as of now in the operator's zone: the sending switch,
// the send timer (from timers), every approved live email that is not a selftest (only
// mailbox mailboxID's when it is not 0), every mailbox, the assignments of the emails'
// campaigns and the campaigns' names.
func (b *backend) loadSendFacts(ctx context.Context, setup leadsSetup, timers timerRead, mailboxID int64) (sendFacts, error) {
	now := b.now()
	lq := b.lq()
	f := sendFacts{live: setup.env.Live(), cfg: setup.leads, loc: b.loc, now: now, mailboxScoped: mailboxID != 0,
		firstSend: setup.leads.Caps.StartDate.IsFirstSend()}
	var err error
	if f.paused, f.pauseReason, f.pausedAt, err = lq.SendPaused(ctx); err != nil {
		return sendFacts{}, err
	}
	send := timers.units[unitSend]
	f.sendTimer, f.sendNext = send.Enabled, send.Next
	// Every approved live email with a due time: SendRows up to far ahead (an approved
	// follow-up with no due time yet waits on the step before it, so it is not waiting here).
	rows, err := lq.SendRows(ctx, now.AddDate(10, 0, 0), false)
	if err != nil {
		return sendFacts{}, err
	}
	ms, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(now, b.loc))
	if err != nil {
		return sendFacts{}, err
	}
	f.mailboxes = make(map[int64]leadstore.MailboxStatus, len(ms))
	for _, m := range ms {
		f.mailboxes[m.ID] = m
	}
	for _, r := range rows {
		if r.LeadKind == leadstore.LeadSelftest || r.DueAt == nil || (mailboxID != 0 && r.MailboxID != mailboxID) {
			continue
		}
		// What the send run cancels never waits: an email whose lead may no longer be written
		// to (outreach.CanSend, as sendOne checks it), and one on a retired mailbox (a thread
		// must come from the same address, so the run cancels the mailbox's pending emails).
		if !outreach.CanSend(r.LeadState) {
			continue
		}
		if m, ok := f.mailboxes[r.MailboxID]; ok && m.State == leadstore.MailboxRetired {
			continue
		}
		f.rows = append(f.rows, r)
	}
	f.assigned = map[[2]int64]bool{}
	seen := map[int64]bool{}
	for _, r := range f.rows {
		if seen[r.CampaignID] {
			continue
		}
		seen[r.CampaignID] = true
		cms, err := lq.CampaignMailboxes(ctx, r.CampaignID)
		if err != nil {
			return sendFacts{}, err
		}
		for _, cm := range cms {
			f.assigned[[2]int64{cm.CampaignID, cm.MailboxID}] = true
		}
	}
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return sendFacts{}, err
	}
	f.campaigns = make(map[int64]string, len(cs))
	for _, c := range cs {
		f.campaigns[c.ID] = c.Name
	}
	return f, nil
}

// whyNotSending is the one sentence about sending: why nothing is going out, or when the
// waiting emails next go. The Controls page, the Leads page and the dashboard show it.
func (b *backend) whyNotSending(ctx context.Context) (sendWhy, error) {
	return b.whyNotSendingFrom(ctx, 0)
}

// whyNotSendingFrom is whyNotSending over the emails of mailbox mailboxID only (0: every
// mailbox), for a mailbox's card.
func (b *backend) whyNotSendingFrom(ctx context.Context, mailboxID int64) (sendWhy, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return sendWhy{}, err
	}
	f, err := b.loadSendFacts(ctx, setup, readTimers(ctx, controlsGetenv), mailboxID)
	if err != nil {
		return sendWhy{}, err
	}
	return whyNot(f), nil
}

// SendingWhy is whyNotSending as the console takes it.
func (b *backend) SendingWhy(ctx context.Context) (console.SendingWhy, error) {
	w, err := b.whyNotSending(ctx)
	if err != nil {
		return console.SendingWhy{}, err
	}
	return w.view(), nil
}

// Controls is the Controls page, read live: the sending sentence, the sending switch and each
// job's switch with its timer and last run, the guards set on the server and the rating
// setting. The timers are asked of systemd once for the whole page.
func (b *backend) Controls(ctx context.Context) (console.ControlsPage, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return console.ControlsPage{}, err
	}
	lq := b.lq()
	timers := readTimers(ctx, controlsGetenv)
	f, err := b.loadSendFacts(ctx, setup, timers, 0)
	if err != nil {
		return console.ControlsPage{}, err
	}
	p := console.ControlsPage{Why: whyNot(f).view(), TimersRead: timers.fromSystemd, TimersNote: timers.note,
		Guards: serverGuards(controlsGetenv), GuardsHow: guardsHow,
		RatingOn: setup.leads.Relevance.RatingOn(), RatingPath: console.RatingSettingsPath}
	var commands []string
	for _, r := range append([]controlRow{controlSending}, controlJobs...) {
		commands = append(commands, r.commands...)
	}
	last, err := latestRuns(ctx, b.s.Pool(), commands)
	if err != nil {
		return console.ControlsPage{}, err
	}
	pausedBy, err := sendPauseActor(ctx, b.s.Pool())
	if err != nil {
		return console.ControlsPage{}, err
	}
	p.Sending = controlView(controlSending, "Sending", !f.paused, f.pausedAt, f.pauseReason, pausedBy, timers, last, b.now())
	sws, err := lq.JobSwitches(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return console.ControlsPage{}, err
	}
	byJob := map[string]leadstore.JobSwitch{}
	for _, s := range sws {
		byJob[s.Job] = s
	}
	for _, r := range controlJobs {
		s := byJob[r.job]
		p.Jobs = append(p.Jobs, controlView(r, jobName(r.job), s.On, s.At, s.Reason, s.Actor, timers, last, b.now()))
	}
	return p, nil
}

// controlView is one switch's row: on, since at (zero: never), reason and actor, its unit's
// state and the latest run of its commands.
func controlView(r controlRow, name string, on bool, at time.Time, reason, actor string, timers timerRead, last map[string]store.Run, now time.Time) console.ControlRow {
	v := console.ControlRow{Job: r.job, Name: name, What: r.what, On: on, Reason: reason, BySystem: actor == leadstore.ActorSystem,
		Schedule: r.schedule}
	if !at.IsZero() {
		v.Since = &at
	}
	if r.unit != "" {
		st := timers.units[r.unit]
		v.Timer = console.TimerState{Unit: r.unit, Enabled: st.Enabled, Next: st.Next}
	}
	var latest *store.Run
	for _, c := range r.commands {
		if run, ok := last[c]; ok && (latest == nil || run.StartedAt.After(latest.StartedAt)) {
			latest = &run
		}
	}
	if latest != nil {
		v.LastRun = runView(*latest, now)
	}
	return v
}

// runStuckAfter is how long a run may go unfinished before its row says it did not finish.
const runStuckAfter = 24 * time.Hour

// stopWords are the run stop reasons in words.
var stopWords = map[string]string{
	"throttled":    "Stopped: the sites asked it to slow down",
	"blocked":      "Stopped: it was blocked",
	"failure_rate": "Stopped: too many requests failed",
	"budget":       "Stopped: it used its request budget",
	"parser_broke": "Stopped: a page could not be read",
	"spend_cap":    "Stopped: today's spending limit was reached",
	"disk_budget":  "Stopped: the disk budget was used",
}

// runView is a run's result in words: switched off, still running (or not finished after a
// day), stopped and why, finished (with its failures), or sending paused.
func runView(r store.Run, now time.Time) *console.ControlRun {
	v := &console.ControlRun{StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
	switch {
	case r.FinishedAt == nil && now.Sub(r.StartedAt) > runStuckAfter:
		v.Result, v.Failed = "Did not finish", true
	case r.FinishedAt == nil:
		v.Result = "Running"
	case r.Note == runNoteSwitchedOff:
		v.Result = "Switched off"
	case r.StopReason != "":
		v.Result, v.Failed = cmp.Or(stopWords[r.StopReason], "Stopped"), true
	case r.Note == notePaused:
		v.Result = "Sending was off"
	case r.Failures > 0:
		v.Result, v.Failed = "Finished with "+plural(r.Failures, "failure", "failures"), true
	default:
		v.Result = "Finished"
	}
	return v
}

// latestRuns is the latest run of each of commands, by command (a command never run has none).
func latestRuns(ctx context.Context, db store.DBTX, commands []string) (map[string]store.Run, error) {
	rows, err := db.Query(ctx,
		`SELECT DISTINCT ON (command) id, command, source, started_at, finished_at, requests, failures, stop_reason, note
		 FROM run WHERE command = ANY($1) ORDER BY command, started_at DESC, id DESC`, commands)
	if err != nil {
		return nil, fmt.Errorf("console: latest runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Run])
	if err != nil {
		return nil, fmt.Errorf("console: latest runs: %w", err)
	}
	out := make(map[string]store.Run, len(runs))
	for _, r := range runs {
		out[r.Command] = r
	}
	return out, nil
}

// sendPauseActor is who set the sending switch last ("" when never).
func sendPauseActor(ctx context.Context, db store.DBTX) (string, error) {
	var actor string
	err := db.QueryRow(ctx, `SELECT actor FROM send_pause ORDER BY id DESC LIMIT 1`).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("console: send pause actor: %w", err)
	}
	return actor, nil
}

// guardsHow says where a guard's line goes.
const guardsHow = "Each is set in /etc/leadhound/env on the server and takes effect with the next make deploy. The console never changes them: a stolen console session must not be able to send for real or spend money."

// serverGuards are the three switches set on the server only, with their values and the line
// that flips each: LEADHOUND_OUTREACH (shadow or live sending), LEADHOUND_LIVE (outbound
// fetches and paid Claude and Jev calls) and LEADHOUND_LEADS_TIMERS (whether the deploy
// installs the leads timers at all).
func serverGuards(getenv func(string) string) []console.ServerGuard {
	outreachV := getenv("LEADHOUND_OUTREACH")
	o := console.ServerGuard{Name: "LEADHOUND_OUTREACH", Value: outreachV,
		State: "Shadow: nothing is sent for real.", Line: "LEADHOUND_OUTREACH=live"}
	if strings.TrimSpace(outreachV) == leadconfig.OutreachLive {
		o.State, o.Line = "Live: approved emails are sent for real.", "LEADHOUND_OUTREACH="
	}
	liveV := getenv("LEADHOUND_LIVE")
	l := console.ServerGuard{Name: "LEADHOUND_LIVE", Value: liveV}
	live, err := config.ParseLive(liveV)
	switch {
	case err != nil:
		l.State, l.Line = "Not understood, so off: no outbound fetches and no paid Claude or Jev calls.", "LEADHOUND_LIVE=1"
	case live == config.LiveAll:
		l.State, l.Line = "On: outbound fetches and paid Claude and Jev calls are allowed.", "LEADHOUND_LIVE="
	case live == config.LiveATS:
		l.State, l.Line = "Job boards only: no site fetches and no paid Claude or Jev calls for leads.", "LEADHOUND_LIVE=1"
	default:
		l.State, l.Line = "Off: no outbound fetches and no paid Claude or Jev calls.", "LEADHOUND_LIVE=1"
	}
	timersV := getenv("LEADHOUND_LEADS_TIMERS")
	t := console.ServerGuard{Name: "LEADHOUND_LEADS_TIMERS", Value: timersV,
		State: "Off: the deploy installs no leads timer.", Line: "LEADHOUND_LEADS_TIMERS=on"}
	if strings.TrimSpace(timersV) == "on" {
		t.State, t.Line = "On: the deploy installs the leads timers (batch, send and sync only with live sending).", "LEADHOUND_LEADS_TIMERS=off"
	}
	return []console.ServerGuard{o, l, t}
}

// ControlAct runs one Controls write: a job switch, or a campaign's mailbox.
// It reads no leads settings: a switch must work while leads.yaml is broken (switching a job
// off is how a broken run is stopped), and the request file a switch-on writes lives in
// LEADHOUND_DATA_DIR itself.
func (b *backend) ControlAct(ctx context.Context, a console.ControlAction) (console.ControlOutcome, error) {
	switch a.Do {
	case console.ControlSwitchOn, console.ControlSwitchOff:
		return b.setJobSwitch(ctx, a.Job, a.Do == console.ControlSwitchOn, a.Reason)
	case console.ControlAssign, console.ControlUnassign:
		return b.setCampaignMailbox(ctx, a.Campaign, a.MailboxID, a.Do == console.ControlAssign)
	}
	return console.ControlOutcome{}, refused("That is not something the Controls page does.")
}

// setJobSwitch switches job on or off, keeping reason (one line, at most
// console.ControlReasonMax characters). Research switched back on writes its request file,
// so what a star queued while it was off starts now.
func (b *backend) setJobSwitch(ctx context.Context, job string, on bool, reason string) (console.ControlOutcome, error) {
	if !slices.Contains(leadstore.Jobs, job) {
		return console.ControlOutcome{}, refused("There is no job called %q.", job)
	}
	reason = strings.TrimSpace(reason)
	if strings.ContainsAny(reason, "\r\n") || utf8.RuneCountInString(reason) > console.ControlReasonMax {
		return console.ControlOutcome{}, refused("The reason takes one line of at most %d characters; nothing was changed.", console.ControlReasonMax)
	}
	lq := b.lq()
	changed, err := lq.SetJobSwitch(ctx, leadstore.DefaultAccountID, job, on, reason, leadstore.ActorGremy)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	name := jobName(job)
	switch {
	case !changed && on:
		return console.ControlOutcome{Message: name + " is already on."}, nil
	case !changed:
		sw, err := lq.JobSwitch(ctx, leadstore.DefaultAccountID, job)
		if err != nil {
			return console.ControlOutcome{}, err
		}
		return console.ControlOutcome{Message: fmt.Sprintf("%s is already off (since %s).", name, whenWords(sw.At, b.loc))}, nil
	case !on:
		return console.ControlOutcome{Message: name + " is switched off: its runs do nothing until you switch it on."}, nil
	}
	msg := name + " is switched on: its next run goes ahead."
	if has, err := requestOnSwitchOn(b.cfg.DataDir, job, b.now()); has {
		if err != nil {
			slog.Default().Warn("console switch request file", "job", job, "failed", 1)
		}
		msg += requestedWords(job, err)
	}
	return console.ControlOutcome{Message: msg}, nil
}

// setCampaignMailbox assigns or unassigns mailbox id to campaign slug, as the operator.
func (b *backend) setCampaignMailbox(ctx context.Context, slug string, id int64, assign bool) (console.ControlOutcome, error) {
	lq := b.lq()
	c, err := lq.CampaignBySlug(ctx, leadstore.DefaultAccountID, slug)
	if errors.Is(err, leadstore.ErrCampaignNotFound) {
		return console.ControlOutcome{}, refused("There is no campaign %q.", slug)
	} else if err != nil {
		return console.ControlOutcome{}, err
	}
	changed, err := lq.SetCampaignMailbox(ctx, c.ID, id, assign, leadstore.ActorGremy)
	switch {
	case errors.Is(err, leadstore.ErrMailboxNotFound), errors.Is(err, leadstore.ErrCrossAccount):
		return console.ControlOutcome{}, refused("There is no mailbox %d.", id)
	case errors.Is(err, leadstore.ErrMailboxRetired):
		return console.ControlOutcome{}, refused("Mailbox %d is retired, so it cannot be assigned.", id)
	case err != nil:
		return console.ControlOutcome{}, err
	}
	m, err := lq.MailboxByID(ctx, id)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	switch {
	case assign && !changed:
		return console.ControlOutcome{Message: fmt.Sprintf("%s is already assigned to %s.", m.Address, c.Name)}, nil
	case !assign && !changed:
		return console.ControlOutcome{Message: fmt.Sprintf("%s is not assigned to %s.", m.Address, c.Name)}, nil
	case assign:
		return console.ControlOutcome{Message: fmt.Sprintf("%s is assigned to %s: its live batches may draft onto it.", m.Address, c.Name)}, nil
	}
	return console.ControlOutcome{Message: fmt.Sprintf("%s is no longer assigned to %s: its approved emails of %s wait until it is assigned again.",
		m.Address, c.Name, c.Name)}, nil
}

// CampaignMailboxes is every campaign with every mailbox not retired (and any retired one
// still assigned to it), each with its assignment and where its host's website lands, warned
// off-site against the campaign's product.site_url.
func (b *backend) CampaignMailboxes(ctx context.Context) ([]console.CampaignMailboxes, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return nil, err
	}
	lq := b.lq()
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	ms, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(b.now(), b.loc))
	if err != nil {
		return nil, err
	}
	landings, err := b.landings(ctx, ms)
	if err != nil {
		return nil, err
	}
	file := setup.file
	if file.Countries == nil {
		file = setup.leads
	}
	out := make([]console.CampaignMailboxes, 0, len(cs))
	for _, c := range cs {
		v := console.CampaignMailboxes{Slug: c.Slug, Name: c.Name}
		v.SiteURL = campaignSiteURL(c, &file)
		v.SiteHost = urlHost(v.SiteURL)
		cms, err := lq.CampaignMailboxes(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		assigned := map[int64]bool{}
		for _, cm := range cms {
			assigned[cm.MailboxID] = true
		}
		for _, m := range ms {
			if m.State == leadstore.MailboxRetired && !assigned[m.ID] {
				continue
			}
			row := console.CampaignMailboxRow{MailboxID: m.ID, Address: m.Address, Host: m.Host(), Domain: m.Domain, Assigned: assigned[m.ID],
				Landing: landings[m.ID]}
			row.State, row.Reason = consoleMailboxState(m)
			row.Landing.OffSite = row.Landing.State == console.LandingFound && v.SiteHost != "" && !sameSite(row.Landing.Host, v.SiteHost)
			if row.Assigned {
				v.Assigned++
			}
			v.Rows = append(v.Rows, row)
		}
		out = append(out, v)
	}
	return out, nil
}

// campaignSiteURL is campaign c's product.site_url from its latest spec, or the legacy spec's
// (leads.yaml's sender.site_url) for the legacy campaign with no version; "" when none.
func campaignSiteURL(c leadstore.Campaign, cfg *leadconfig.Config) string {
	switch {
	case len(c.Spec) > 0:
		var spec campaign.Spec
		if err := json.Unmarshal(c.Spec, &spec); err != nil {
			return ""
		}
		return spec.Product.SiteURL
	case c.Slug == campaign.LegacySlug:
		return campaign.Legacy(cfg).Product.SiteURL
	}
	return ""
}

// urlHost is u's host, lower-cased, "" when u does not parse or has none.
func urlHost(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return ""
	}
	return strings.ToLower(p.Hostname())
}

// sameSite reports whether hosts a and b are the same site, a leading "www." left out of both.
func sameSite(a, b string) bool {
	trim := func(h string) string { return strings.TrimPrefix(strings.ToLower(h), "www.") }
	return trim(a) == trim(b)
}

// landingCutoff is when the landing URL was first read (migration 00024 applied): a check
// that finished before it never looked, so it reads as not checked. Zero when it cannot be
// read; every finished check then counts. goose's tstamp is a timestamp without a zone,
// written as now() in the migrating session's zone; read bare it would come back as UTC, so a
// database whose zone is ahead of UTC would put the cutoff hours late and hide every landing
// read in those hours. It is read as a time in this session's zone (the database's own, as
// for the migration) instead.
func landingCutoff(ctx context.Context, db store.DBTX) time.Time {
	var at time.Time
	if err := db.QueryRow(ctx,
		`SELECT tstamp::timestamptz FROM goose_db_version_leads WHERE version_id = 24 AND is_applied ORDER BY id DESC LIMIT 1`).Scan(&at); err != nil {
		return time.Time{}
	}
	return at
}

// landings is where each of ms's hosts landed at its latest finished check (passed or failed;
// a check that did not run never looked).
func (b *backend) landings(ctx context.Context, ms []leadstore.MailboxStatus) (map[int64]console.LandingView, error) {
	cutoff := landingCutoff(ctx, b.s.Pool())
	out := make(map[int64]console.LandingView, len(ms))
	for _, m := range ms {
		var (
			landing *string
			at      time.Time
		)
		err := b.s.Pool().QueryRow(ctx,
			`SELECT landing_url, checked_at FROM mailbox_check
			 WHERE mailbox_id = $1 AND checked_at IS NOT NULL AND outcome IN ('passed', 'failed')
			 ORDER BY id DESC LIMIT 1`, m.ID).Scan(&landing, &at)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			out[m.ID] = console.LandingView{State: console.LandingNotChecked}
			continue
		case err != nil:
			return nil, fmt.Errorf("console: mailbox landing: %w", err)
		}
		v := console.LandingView{At: &at}
		switch {
		case landing != nil:
			v.State, v.URL, v.Host = console.LandingFound, *landing, urlHost(*landing)
		case at.Before(cutoff):
			v = console.LandingView{State: console.LandingNotChecked}
		default:
			v.State = console.LandingNoAnswer
		}
		out[m.ID] = v
	}
	return out, nil
}

// MailboxSending is mailbox id's sending card: its campaigns, where its host lands, how many
// approved live emails wait on it, and why none goes out now (whyNotSendingFrom).
func (b *backend) MailboxSending(ctx context.Context, id int64) (console.MailboxSendingView, error) {
	m, setup, _, err := b.mailboxStatus(ctx, id)
	if err != nil {
		return console.MailboxSendingView{}, err
	}
	lq := b.lq()
	cms, err := lq.MailboxCampaigns(ctx, id)
	if err != nil {
		return console.MailboxSendingView{}, err
	}
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return console.MailboxSendingView{}, err
	}
	byID := map[int64]leadstore.Campaign{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	v := console.MailboxSendingView{MailboxID: id}
	for _, cm := range cms {
		c := byID[cm.CampaignID]
		v.Campaigns = append(v.Campaigns, console.FilterOption{Value: c.Slug, Label: c.Name})
	}
	landings, err := b.landings(ctx, []leadstore.MailboxStatus{m})
	if err != nil {
		return console.MailboxSendingView{}, err
	}
	v.Landing = landings[id]
	f, err := b.loadSendFacts(ctx, setup, readTimers(ctx, controlsGetenv), id)
	if err != nil {
		return console.MailboxSendingView{}, err
	}
	v.Waiting = len(f.rows)
	v.Why = whyNot(f).view()
	return v, nil
}
