package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/diskfree"
	"github.com/gremy/webbership-leadhound/internal/leadbrief"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The console's StatusBackend: the Status page's raw figures, read live, and its two writes
// (ask for a fresh Reoon balance, record a Claude top-up). The console holds no key: the Reoon
// balance reaches it only as the row the balance unit stores (service_status), and the
// Claude spend only as the ai_usage rows the units that call Claude write. The dashboard's
// Claude, Reoon and Jev figures come from the same helpers as the Status page's
// (claudeFigures, reoonChecksToday, jevToday), so the two never disagree.

// reoonBalanceRequest is the request file the balance unit's watcher starts
// leadhound-leads-verify-balance from: <data>/run/leads-verify-balance.
const reoonBalanceRequest = "leads-verify-balance"

// refreshWait is how long a balance request is left to its unit before a new one is taken.
const refreshWait = 2 * time.Minute

// statusUnit is one unit as the Status page lists it: its name, the systemd unit its next
// run and switch are read from, and which run rows are its runs (nil: it records none).
type statusUnit struct {
	name string
	unit string
	runs func(store.Run) bool
}

func commandIs(command string) func(store.Run) bool {
	return func(r store.Run) bool { return r.Command == command }
}

// statusUnits are the units the Status page lists: every leadhound timer, the request-driven
// units the console starts (Send now, the mailbox check, research, the Reoon balance) and the
// email check, which runs inside the batch unit. TestStatusUnitsMatchDeploy holds it to
// deploy/.
var statusUnits = []statusUnit{
	{name: "leads-send", unit: unitSend, runs: func(r store.Run) bool {
		return r.Command == commandLeadsSend && !leadbrief.NoteWords(r.Note)[noteSendNow]
	}},
	{name: "leads-send-now", unit: "leadhound-leads-send-now.path", runs: func(r store.Run) bool {
		return r.Command == commandLeadsSend && leadbrief.NoteWords(r.Note)[noteSendNow]
	}},
	{name: "leads-batch", unit: unitBatch, runs: commandIs(commandLeadsBatch)},
	{name: "leads-verify", unit: unitBatch, runs: commandIs(commandLeadsVerify)},
	{name: "leads-verify-balance", unit: "leadhound-leads-verify-balance.timer"}, // its reading is its run
	{name: "leads-sync", unit: unitSync, runs: commandIs(commandLeadsSync)},
	{name: "leads-contacts", unit: unitContacts, runs: commandIs(commandLeadsContacts)},
	{name: "leads-probe", unit: unitProbe, runs: commandIs(commandLeadsSitesProbe)},
	{name: "leads-research", unit: unitResearch, runs: commandIs(commandLeadsResearch)},
	{name: "mailbox-check", unit: "leadhound-mailbox-check.path"}, // its checks are its runs
	{name: "collect", unit: "leadhound-collect.timer", runs: isLinkedInCollect},
	{name: "ats", unit: "leadhound-ats.timer", runs: isATSCollect},
	{name: "prebrief", unit: "leadhound-prebrief.timer", runs: commandIs(linkedin.CommandCanary)},
	{name: "brief", unit: "leadhound-brief.timer", runs: commandIs("brief")},
	{name: "trim", unit: "leadhound-trim.timer", runs: commandIs("trim")},
	{name: "backup", unit: "leadhound-backup.timer"}, // keeps no run record
}

// statusCommands are the run commands statusUnits read.
var statusCommands = []string{commandLeadsSend, commandLeadsBatch, commandLeadsVerify, commandLeadsSync, commandLeadsContacts,
	commandLeadsSitesProbe, commandLeadsResearch, linkedin.CommandCollect, linkedin.CommandCanary, "brief", "trim"}

// Status is every area's figures as they stand (console.StatusBackend). It never writes and
// never reads a key. An area never read has nil times.
func (b *backend) Status(ctx context.Context) (console.Status, error) {
	now := b.now()
	setup, setupErr := b.leadsSetup(ctx, leadsForProbe)
	leadsOn := setupErr == nil
	var st console.Status
	var err error
	var health []leadstore.MailboxHealthRow
	if leadsOn {
		if st.Mailboxes, health, err = b.mailboxesStatus(ctx, setup, now); err != nil {
			return console.Status{}, err
		}
	}
	if st.Server, err = b.serverStatus(ctx, now, health); err != nil {
		return console.Status{}, err
	}
	if st.Claude, err = b.claudeStatus(ctx, now); err != nil {
		return console.Status{}, err
	}
	if st.Reoon, err = b.reoonStatus(ctx, now, leadsOn); err != nil {
		return console.Status{}, err
	}
	if st.Jev, err = b.jevStatus(ctx, now); err != nil {
		return console.Status{}, err
	}
	return st, nil
}

// mailboxesStatus is every mailbox that is not retired, in the Mailboxes page's order, with
// its checks, today's sends against its warm-up limit and whether the next send run may send
// from it (outreach.RoomToday over the mailboxes as a send run loads them), and the rows read.
func (b *backend) mailboxesStatus(ctx context.Context, setup leadsSetup, now time.Time) (console.MailboxesStatus, []leadstore.MailboxHealthRow, error) {
	lq := b.lq()
	today := startOfDay(now, b.loc)
	rows, err := lq.MailboxHealth(ctx, today, b.loc, now)
	if err != nil {
		return console.MailboxesStatus{}, nil, err
	}
	ms, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, today)
	if err != nil {
		return console.MailboxesStatus{}, nil, err
	}
	f := sendFacts{cfg: setup.leads, loc: b.loc, mailboxes: map[int64]leadstore.MailboxStatus{},
		firstSend: setup.leads.Caps.StartDate.IsFirstSend()}
	order := map[int64]int{}
	for i, m := range ms {
		f.mailboxes[m.ID] = m
		order[m.ID] = i
	}
	local := now.In(b.loc)
	loads := f.loads()
	room := outreach.RoomToday(f.cfg, loads, local)
	ready := map[int64]bool{}
	for _, ml := range loads {
		ready[ml.ID] = ml.Ready && room[ml.ID] > 0
	}
	slices.SortStableFunc(rows, func(a, c leadstore.MailboxHealthRow) int {
		ia, oka := order[a.ID]
		ic, okc := order[c.ID]
		switch {
		case oka && okc:
			return ia - ic
		case oka:
			return -1
		case okc:
			return 1
		}
		return 0
	})
	var out console.MailboxesStatus
	for _, r := range rows {
		row := console.MailboxStatus{ID: r.ID, Name: r.FromName, Address: r.Address, State: console.StatusMailboxActive,
			SentToday: r.SentToday, HardBounces7d: r.Bounces7d, Sent7d: r.Sent7d, SyncedAt: r.SyncedAt, Ready: ready[r.ID]}
		if row.Name == "" {
			row.Name = setup.leads.Sender.Name
		}
		if r.State == leadstore.MailboxPaused {
			row.State, row.Reason = console.StatusMailboxPaused, r.Reason
		}
		if m, ok := f.mailboxes[r.ID]; ok {
			row.RampToday, _ = mailboxRamp(f.cfg, m, b.loc, now)
		}
		row.CheckedAt, row.Checks = mailboxChecks(r)
		out.Rows = append(out.Rows, row)
		for _, t := range []*time.Time{row.CheckedAt, r.SyncedAt, senderCheckAt(r)} {
			out.UpdatedAt = later(out.UpdatedAt, t)
		}
	}
	return out, rows, nil
}

// mailboxChecks is a mailbox's latest guided check and its parts, in the order SMTP, IMAP,
// folders, SPF, DKIM, DMARC; a part that never ran is left out. SPF, DKIM and DMARC come from
// the latest record check of its host when that is newer than the guided check. Nil and none
// when the mailbox was never checked (a check only requested has not run).
func mailboxChecks(r leadstore.MailboxHealthRow) (*time.Time, []console.CheckPart) {
	c := r.LastCheck
	if c == nil || c.CheckedAt == nil {
		return nil, nil
	}
	var parts []console.CheckPart
	add := func(name string, ok *bool) {
		if ok != nil {
			parts = append(parts, console.CheckPart{Name: name, OK: *ok})
		}
	}
	add("SMTP", c.SMTPOK)
	add("IMAP", c.IMAPOK)
	add("folders", c.FoldersOK)
	spf, dkim, dmarc := c.SPF, c.DKIM, c.DMARC
	if s := r.SenderCheck; s != nil && s.CheckedAt.After(*c.CheckedAt) {
		spf, dkim, dmarc = &s.SPF, &s.DKIM, &s.DMARC
	}
	add("SPF", spf)
	add("DKIM", dkim)
	add("DMARC", dmarc)
	at := *c.CheckedAt
	return &at, parts
}

func senderCheckAt(r leadstore.MailboxHealthRow) *time.Time {
	if r.SenderCheck == nil {
		return nil
	}
	return &r.SenderCheck.CheckedAt
}

// later is the later of a and t (either may be nil), as a copy.
func later(a, t *time.Time) *time.Time {
	if t == nil || a != nil && !t.After(*a) {
		return a
	}
	v := *t
	return &v
}

// serverStatus is each unit's last run, next run and switch, the database's size, the disk
// and the machine's figures, read at now. UpdatedAt is now once any unit has run, nil before. health is the mailboxes as mailboxesStatus read
// them (nil without leads): their latest check is the mailbox check's last run.
func (b *backend) serverStatus(ctx context.Context, now time.Time, health []leadstore.MailboxHealthRow) (console.ServerStatus, error) {
	sv := console.ServerStatus{DBMaxBytes: b.cfg.DBMaxBytes, DiskFloorPct: float64(b.cfg.DiskMinFreePct),
		Started: b.started, Version: buildRevision()}
	var err error
	if sv.Units, err = b.unitsStatus(ctx, now, health); err != nil {
		return console.ServerStatus{}, err
	}
	// The figures are read now; before any unit has run there is nothing to judge the units
	// by, so the area stays "not checked yet" rather than "all units ran clean".
	for _, u := range sv.Units {
		if u.LastRun != nil {
			sv.UpdatedAt = &now
			break
		}
	}
	if sv.DBBytes, err = b.s.Q().DatabaseSize(ctx); err != nil {
		return console.ServerStatus{}, err
	}
	free, total, ok, err := diskSpace(storagePath(b.cfg))
	switch {
	case err != nil:
		slog.Default().Warn("console status: free disk not read", "err", err.Error())
	case ok:
		sv.DiskFreeBytes, sv.DiskTotalBytes = clampInt64(free), clampInt64(total)
	}
	sv.Load = readLoad(procFile("/proc/loadavg"))
	sv.MemoryPct = readMemoryPct(procFile("/proc/meminfo"))
	return sv, nil
}

// diskSpace reads a filesystem's free and total bytes; tests replace it.
var diskSpace = diskfree.Space

// procFile is the text of a /proc file, "" when it cannot be read (every machine but Linux).
var procFile = func(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed /proc paths
	if err != nil {
		return ""
	}
	return string(b)
}

func clampInt64(n uint64) int64 {
	if n > uint64(1<<63-1) {
		return 1<<63 - 1
	}
	return int64(n)
}

// readLoad is the one-minute load average from /proc/loadavg's text; nil when there is none.
func readLoad(text string) *float64 {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

// readMemoryPct is the share of memory in use, from /proc/meminfo's MemTotal and
// MemAvailable; nil when either is missing.
func readMemoryPct(text string) *float64 {
	kb := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseFloat(f[0], 64); err == nil {
			kb[k] = n
		}
	}
	total, okT := kb["MemTotal"]
	avail, okA := kb["MemAvailable"]
	if !okT || !okA || total <= 0 || avail > total {
		return nil
	}
	pct := (total - avail) * 100 / total
	return &pct
}

// buildRevision is the build's commit, short; "" when the build carries none.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value[:min(len(s.Value), 7)]
		}
	}
	return ""
}

// unitsStatus is every statusUnits row: its last run (from the run rows, the mailboxes'
// latest check, or the Reoon reading), and its next run and switch from systemd. Where
// systemd cannot be asked (a machine without it), every switch is unknown and no next run is
// known; that is not an error.
func (b *backend) unitsStatus(ctx context.Context, now time.Time, health []leadstore.MailboxHealthRow) ([]console.UnitStatus, error) {
	runs, err := latestRunsBySource(ctx, b.s.Pool(), statusCommands)
	if err != nil {
		return nil, err
	}
	var asked []string
	for _, u := range statusUnits {
		if !slices.Contains(asked, u.unit) {
			asked = append(asked, u.unit)
		}
	}
	var units map[string]unitState
	if out, err := systemctlShow(ctx, asked); err == nil {
		units, _ = parseSystemctlShow(out, asked)
	}
	reoon, err := b.reoonRow(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]console.UnitStatus, 0, len(statusUnits))
	for _, u := range statusUnits {
		us := console.UnitStatus{Name: u.name, Result: console.UnitNever, Switch: console.SwitchUnknown}
		switch {
		case u.runs != nil:
			var last *store.Run
			for i := range runs {
				if u.runs(runs[i]) && (last == nil || runs[i].StartedAt.After(last.StartedAt)) {
					last = &runs[i]
				}
			}
			if last != nil {
				at := last.StartedAt
				us.LastRun = &at
				us.Result, us.Note = runResult(*last, now)
			}
		case u.name == "mailbox-check":
			for _, r := range health {
				if r.LastCheck != nil && r.LastCheck.CheckedAt != nil {
					us.LastRun = later(us.LastRun, r.LastCheck.CheckedAt)
				}
			}
			if us.LastRun != nil {
				us.Result = console.UnitOK
			}
		case u.name == "leads-verify-balance" && reoon != nil:
			us.LastRun, us.Result = reoon.FetchedAt, console.UnitOK
			if reoon.ErrorAt != nil && (reoon.FetchedAt == nil || reoon.ErrorAt.After(*reoon.FetchedAt)) {
				us.LastRun, us.Result, us.Note = reoon.ErrorAt, console.UnitFailed, "Reoon did not answer"
			}
			us.LastRun = later(nil, us.LastRun)
		}
		if st, ok := units[u.unit]; ok {
			us.Next = st.Next
			switch {
			case st.Enabled:
				us.Switch = console.SwitchOn
			case st.Loaded:
				us.Switch = console.SwitchOff
			}
		}
		out = append(out, us)
	}
	return out, nil
}

// latestRunsBySource is the latest run of each command and source among commands: the units
// that share a command (Send now and the hourly send, LinkedIn and the ATS boards) each find
// theirs among them.
func latestRunsBySource(ctx context.Context, db store.DBTX, commands []string) ([]store.Run, error) {
	rows, err := db.Query(ctx,
		`SELECT DISTINCT ON (command, source, note LIKE '%`+noteSendNow+`%') id, command, source, started_at, finished_at,
		   requests, failures, stop_reason, note
		 FROM run WHERE command = ANY($1)
		 ORDER BY command, source, note LIKE '%`+noteSendNow+`%', started_at DESC, id DESC`, commands)
	if err != nil {
		return nil, fmt.Errorf("console: status runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Run])
	if err != nil {
		return nil, fmt.Errorf("console: status runs: %w", err)
	}
	return runs, nil
}

// runResult is a run's result and its short note at now, judged as the Controls page judges
// it (runView): running while open, failed when runView says so (not finished after
// runStuckAfter, stopped, failures, no key), else ok with how long it took.
func runResult(r store.Run, now time.Time) (result, note string) {
	v := runView(r, now)
	switch {
	case v.Failed:
		return console.UnitFailed, v.Result
	case r.FinishedAt == nil:
		return console.UnitRunning, ""
	}
	took := durationWords(r.FinishedAt.Sub(r.StartedAt))
	if v.Result != "Finished" {
		return console.UnitOK, took + " · " + v.Result
	}
	return console.UnitOK, took
}

// durationWords is how long a run took: "41 s", "3 min", "2 h 5 min".
func durationWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(max(d, 0)/time.Second)) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min"
	}
	return fmt.Sprintf("%d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// claudeFigures is the Claude spend the dashboard and the Status page both show.
type claudeFigures struct {
	today     int64 // the operator's account, today's local date
	byPurpose []console.PurposeSpend
	month     int64 // every account, since the start of the local month
	capMicros int64 // LEADHOUND_CLAUDE_DAILY_USD_CAP
}

// claudeFigures reads today's spend (claudeSpendToday), the month's (ClaudeSpendBetween from
// the start of now's local month to now) and the daily cap.
func (b *backend) claudeFigures(ctx context.Context, now time.Time) (claudeFigures, error) {
	q := b.s.Q()
	local := now.In(b.loc)
	f := claudeFigures{capMicros: b.cfg.ClaudeDailyCapMicros}
	var err error
	if f.today, f.byPurpose, err = claudeSpendToday(ctx, q, local); err != nil {
		return claudeFigures{}, err
	}
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, b.loc)
	if f.month, err = q.ClaudeSpendBetween(ctx, month, now); err != nil {
		return claudeFigures{}, err
	}
	return f, nil
}

// claudeStatus is the Claude account: today's spend in all and per purpose against the caps,
// the month's, the last top-up and the spend since it, and the last API error.
func (b *backend) claudeStatus(ctx context.Context, now time.Time) (console.ClaudeStatus, error) {
	f, err := b.claudeFigures(ctx, now)
	if err != nil {
		return console.ClaudeStatus{}, err
	}
	c := console.ClaudeStatus{TodayMicros: f.today, DailyCapMicros: f.capMicros, MonthMicros: f.month}
	spent := map[string]int64{}
	var extra []string
	for _, p := range f.byPurpose {
		spent[p.Purpose] = p.Micros
		if !slices.Contains(claudePurposeOrder, p.Purpose) {
			extra = append(extra, p.Purpose)
		}
	}
	for _, p := range append(slices.Clone(claudePurposeOrder), extra...) {
		pc := console.PurposeCap{Purpose: p, Micros: spent[p]}
		if caps, err := b.cfg.ClaudeCapsFor(p, 0, 0); err == nil {
			pc.CapMicros = caps.DailyMicros
		}
		c.ByPurpose = append(c.ByPurpose, pc)
	}
	claudeAt, _, err := b.aiUsageLatest(ctx)
	if err != nil {
		return console.ClaudeStatus{}, err
	}
	c.UpdatedAt = claudeAt
	lq := b.lq()
	switch t, err := lq.LatestClaudeTopup(ctx); {
	case errors.Is(err, leadstore.ErrTopupNotFound):
	case err != nil:
		return console.ClaudeStatus{}, err
	default:
		c.Topup = &console.ClaudeTopup{AmountCents: t.AmountCents, At: t.At}
		if c.SinceTopupMicros, err = b.s.Q().ClaudeSpendBetween(ctx, t.At, now); err != nil {
			return console.ClaudeStatus{}, err
		}
	}
	switch row, err := lq.ServiceStatus(ctx, leadstore.ServiceClaude); {
	case errors.Is(err, leadstore.ErrServiceNotFound):
	case err != nil:
		return console.ClaudeStatus{}, err
	case row.ErrorAt != nil:
		c.LastError, c.LastErrorAt = row.Error, row.ErrorAt
	}
	return c, nil
}

// aiUsageLatest is when Claude and Jev were last recorded as called (ai_usage, by when the
// row was written); nil for one never called.
func (b *backend) aiUsageLatest(ctx context.Context) (claudeAt, jevAt *time.Time, err error) {
	err = b.s.Pool().QueryRow(ctx,
		`SELECT max(created_at) FILTER (WHERE provider = $1), max(created_at) FILTER (WHERE provider <> $1) FROM ai_usage`,
		claude.Provider).Scan(&claudeAt, &jevAt)
	if err != nil {
		return nil, nil, fmt.Errorf("console: latest ai usage: %w", err)
	}
	return claudeAt, jevAt, nil
}

// reoonChecksToday is today's email checks (the local day of now) by what they found.
func (b *backend) reoonChecksToday(ctx context.Context, now time.Time) (leadstore.EmailCheckCounts, error) {
	day := startOfDay(now, b.loc)
	return b.lq().EmailChecksOn(ctx, day, day.AddDate(0, 0, 1))
}

// reoonDailyLimit is the day's cap on email checks (LEADHOUND_REOON_DAILY_CAP, as the check
// run reads it); 0 when it cannot be read.
func reoonDailyLimit() int {
	n, err := reoonDailyCap(controlsGetenv)
	if err != nil {
		return 0
	}
	return n
}

// reoonRow is the stored Reoon reading's row; nil when none was ever stored.
func (b *backend) reoonRow(ctx context.Context) (*leadstore.ServiceStatusRow, error) {
	row, err := b.lq().ServiceStatus(ctx, leadstore.ServiceReoon)
	if errors.Is(err, leadstore.ErrServiceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// reoonReadingOf reads a stored Reoon reading, {"api_status": "...", "daily": N, "instant":
// N}; other fields are ignored. ok is false when one of the three is missing or of another
// type: such a reading counts as none.
func reoonReadingOf(data []byte) (apiStatus string, daily, instant int, ok bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return "", 0, 0, false
	}
	strict := func(key string, v any) bool {
		m, found := raw[key]
		if !found {
			return false
		}
		d := json.NewDecoder(bytes.NewReader(m))
		return d.Decode(v) == nil
	}
	if !strict("api_status", &apiStatus) || !strict("daily", &daily) || !strict("instant", &instant) {
		return "", 0, 0, false
	}
	return apiStatus, daily, instant, true
}

// reoonStatus is the email check's account: the last good balance reading, the last failed
// one, a refresh asked and not yet answered, and today's checks (with leads set up here).
func (b *backend) reoonStatus(ctx context.Context, now time.Time, leadsOn bool) (console.ReoonStatus, error) {
	var r console.ReoonStatus
	row, err := b.reoonRow(ctx)
	if err != nil {
		return console.ReoonStatus{}, err
	}
	if row != nil {
		if row.FetchedAt != nil {
			if api, daily, instant, ok := reoonReadingOf(row.Data); ok {
				r.FetchedAt, r.APIStatus, r.DailyCredits, r.InstantCredits = row.FetchedAt, api, daily, instant
			}
		}
		if row.ErrorAt != nil {
			r.LastError, r.LastErrorAt = row.Error, row.ErrorAt
		}
	}
	if b.cfg.DataDir != "" {
		asked, err := readRunRequest(b.cfg.DataDir, reoonBalanceRequest)
		if err != nil {
			return console.ReoonStatus{}, err
		}
		if waitingRefresh(asked, row) {
			r.RefreshAskedAt = asked
		}
	}
	if leadsOn {
		c, err := b.reoonChecksToday(ctx, now)
		if err != nil {
			return console.ReoonStatus{}, err
		}
		r.ChecksToday = console.ReoonChecks{Valid: c.Valid, Invalid: c.Invalid, Other: c.CatchAll + c.Other}
	}
	return r, nil
}

// waitingRefresh reports whether a balance request asked at asked is not yet answered: newer
// than the stored reading and the stored failure.
func waitingRefresh(asked *time.Time, row *leadstore.ServiceStatusRow) bool {
	if asked == nil {
		return false
	}
	if row == nil {
		return true
	}
	return (row.FetchedAt == nil || asked.After(*row.FetchedAt)) && (row.ErrorAt == nil || asked.After(*row.ErrorAt))
}

// jevToday is today's Jev usage and the two daily call caps: the jobs cap
// (LEADHOUND_JEV_DAILY_CALL_CAP) and the leads cap in force, which is the one leads rate runs
// under (the saved settings may lower the server's cap; without leads here the server's).
func (b *backend) jevToday(ctx context.Context, now time.Time) (u store.AIUsage, jobsCap, leadsCap int, err error) {
	if u, err = b.s.Q().AIUsageOn(ctx, now.In(b.loc)); err != nil {
		return store.AIUsage{}, 0, 0, err
	}
	jobsCap, leadsCap = b.cfg.JevDailyCallCap, b.cfg.JevLeadsDailyCallCap
	if setup, err := b.leadsSetup(ctx, leadsForProbe); err == nil {
		leadsCap = setup.jevLeadsDailyCap()
	}
	return u, jobsCap, leadsCap, nil
}

// jevStatus is today's Jev calls against the caps, and when Jev was last called.
func (b *backend) jevStatus(ctx context.Context, now time.Time) (console.JevStatus, error) {
	u, jobsCap, leadsCap, err := b.jevToday(ctx, now)
	if err != nil {
		return console.JevStatus{}, err
	}
	_, jevAt, err := b.aiUsageLatest(ctx)
	if err != nil {
		return console.JevStatus{}, err
	}
	return console.JevStatus{UpdatedAt: jevAt, JobsCalls: u.JobsCalls, JobsCap: jobsCap, LeadsCalls: u.LeadsCalls, LeadsCap: leadsCap}, nil
}

// RefreshStatus asks for a fresh Reoon balance (console.StatusBackend): it writes the request
// file the balance unit's watcher starts it from. It refuses while leads are not set up here,
// and while a request asked less than refreshWait ago is not yet answered.
func (b *backend) RefreshStatus(ctx context.Context) (console.ControlOutcome, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return console.ControlOutcome{}, refuse("Leads are not set up on this server, so there is no Reoon balance to ask for.")
	}
	now := b.now()
	asked, err := readRunRequest(setup.dataDir, reoonBalanceRequest)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	row, err := b.reoonRow(ctx)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	if waitingRefresh(asked, row) && now.Sub(*asked) < refreshWait {
		return console.ControlOutcome{}, refuse("A fresh Reoon balance was already asked at %s; it shows here within a minute.", hhmm(*asked, now.In(b.loc)))
	}
	if err := writeRunRequest(setup.dataDir, reoonBalanceRequest, now); err != nil {
		return console.ControlOutcome{}, err
	}
	return console.ControlOutcome{Message: "Asked for a fresh Reoon balance. It shows here within a minute."}, nil
}

// AddClaudeTopup records a top-up of the Claude account (console.StatusBackend).
func (b *backend) AddClaudeTopup(ctx context.Context, amountCents int64, at time.Time) (console.ControlOutcome, error) {
	if amountCents <= 0 {
		return console.ControlOutcome{}, refuse("The amount must be more than $0.00.")
	}
	if err := b.lq().AddClaudeTopup(ctx, amountCents, at); err != nil {
		return console.ControlOutcome{}, err
	}
	return console.ControlOutcome{Message: fmt.Sprintf("Recorded a $%d.%02d top-up on %s.",
		amountCents/100, amountCents%100, at.In(b.loc).Format("2 Jan"))}, nil
}

// dashboardLeads fills the dashboard's leads widgets: the leads waiting on the operator, each
// campaign's funnel over the last 30 days and today's email checks against the day's cap.
// Without leads set up here it leaves them empty; a failed read is one logged line and that
// widget empty, never a failed dashboard, as leadsSummary.
func (b *backend) dashboardLeads(ctx context.Context, now time.Time, d *console.Dashboard) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return
	}
	lq := b.lq()
	if rows, total, err := lq.NeedsMeAt(ctx, dashboardNeedsMe, now, b.loc); err != nil {
		slog.Default().Warn("console dashboard: needs me", "err", err.Error())
	} else {
		d.NeedsMeTotal = total
		for _, r := range rows {
			d.NeedsMe = append(d.NeedsMe, console.NeedsMeRow{LeadID: r.LeadID, Name: r.Name, What: needsMeWords(r), At: r.At})
		}
	}
	if rows, err := lq.CampaignFunnel(ctx, now.AddDate(0, 0, -30)); err != nil {
		slog.Default().Warn("console dashboard: campaign funnel", "err", err.Error())
	} else {
		for _, r := range rows {
			name := r.Name
			if name == "" {
				name = r.Slug
			}
			d.CampaignFunnel = append(d.CampaignFunnel, console.CampaignFunnelRow{Campaign: name, Sent: r.Sent, Replied: r.Replied, Positive: r.Positive})
		}
	}
	if c, err := b.reoonChecksToday(ctx, now); err != nil {
		slog.Default().Warn("console dashboard: email checks", "err", err.Error())
	} else {
		d.ReoonChecksToday = c.Total
	}
	d.ReoonDailyLimit = reoonDailyLimit()
}

// dashboardNeedsMe is how many waiting leads the dashboard lists.
const dashboardNeedsMe = 5

// needsMeWords is what a waiting lead's row says happened: a reply or a connected call with
// nothing scheduled, or the operator's own next action fallen due.
func needsMeWords(r leadstore.NeedsMeRow) string {
	switch r.What {
	case leadstore.NeedsConnected:
		return "Connected on a call · no next step"
	case leadstore.NeedsNextAction:
		if r.NextAction == "" {
			return "Next action due"
		}
		return "Next action due · " + r.NextAction
	}
	return "Replied · nothing scheduled"
}
