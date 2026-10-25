package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/eval"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["serve"] = runServe }

const serveUsage = "usage: leadhound serve [--dev]"

// The console server's limits.
const (
	serveReadHeaderTimeout = 5 * time.Second
	serveReadTimeout       = 15 * time.Second
	serveWriteTimeout      = 30 * time.Second
	serveIdleTimeout       = 60 * time.Second
	serveMaxHeaderBytes    = 64 << 10
	serveShutdownTimeout   = 10 * time.Second
)

// The dashboard's snapshot: the brief input and the day's funnel, built in the background
// and served until it is older than dashboardFreshFor, from another local day, or stale
// after a mark. A request with no snapshot yet waits dashboardWaitFor for the build; a
// build ends at dashboardBuildTimeout; a failed build is not retried for
// dashboardRetryAfter.
const (
	dashboardFreshFor     = 10 * time.Minute
	dashboardBuildTimeout = 4 * time.Minute
	dashboardWaitFor      = 500 * time.Millisecond
	dashboardRetryAfter   = 5 * time.Minute
)

// dashboardRuns is how many runs the dashboard lists.
const dashboardRuns = 8

// maxTrackReason is the longest reason a mark takes, in characters, as the form's maxlength.
const maxTrackReason = 500

// runServe is `leadhound serve [--dev]`: the operator's console on LEADHOUND_CONSOLE_ADDR,
// which must be loopback (a reverse proxy is the only way in). It refuses to start (exit 2)
// without the operator's email and a bcrypt hash of cost 12 or more, on an address off
// loopback, with a console URL that is not https, and with --dev unless live fetching is
// off. When its context ends (SIGTERM from systemd) it stops taking requests, lets the open
// ones finish for up to 10 seconds, closes the store and returns nil.
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dev := fs.Bool("dev", false, "plain-http cookies for local use; refused unless LEADHOUND_LIVE is off")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", serveUsage)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL", "LEADHOUND_DATA_DIR", "LEADHOUND_CONSOLE_EMAIL", "LEADHOUND_CONSOLE_PASSWORD_HASH")
	if err != nil {
		return err
	}
	if err := config.ValidConsoleAddr(cfg.ConsoleAddr); err != nil {
		return usageError("%v", err)
	}
	if cfg.ConsoleURL != "" {
		if err := config.ValidConsoleURL(cfg.ConsoleURL); err != nil {
			return usageError("%v", err)
		}
	}
	if *dev && cfg.Live != config.LiveOff {
		return usageError("--dev runs only with live fetching off (LEADHOUND_LIVE unset), not %s", cfg.Live)
	}
	auth, err := console.NewAuth(cfg.ConsoleEmail.Reveal(), cfg.ConsolePasswordHash.Reveal(), nil)
	if err != nil {
		return usageError("%v", err)
	}
	prof, err := loadProfileDir(cfg.DataDir)
	if err != nil {
		return err
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	// The dashboard's builds end with serve: cancelled, then waited for before the store
	// closes.
	bctx, stopBuilds := context.WithCancel(ctx)
	b := newBackend(bctx, s, prof, loc, cfg)
	defer func() { stopBuilds(); b.wait() }()

	logger := slog.Default()
	if *dev {
		logger.Warn("console dev mode: cookies are not Secure and carry no __Host- prefix; for local use only")
	}
	srv := &http.Server{
		Addr:              cfg.ConsoleAddr,
		Handler:           console.New(b, auth, console.Options{Dev: *dev, Location: loc, Logger: logger}),
		ReadHeaderTimeout: serveReadHeaderTimeout,
		ReadTimeout:       serveReadTimeout,
		WriteTimeout:      serveWriteTimeout,
		IdleTimeout:       serveIdleTimeout,
		MaxHeaderBytes:    serveMaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ConsoleAddr)
	if err != nil {
		return err
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("console listening", "addr", ln.Addr().String(), "dev", *dev)

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("console: shut down: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("console stopped")
	return nil
}

// backend is the console's view of the store, built from the same code the commands run:
// buildInput and the brief's health lines for the dashboard, resolveJobRef and trackJob
// for a mark.
type backend struct {
	s    *store.Store
	prof candidate.Config
	loc  *time.Location
	cfg  config.Config
	now  func() time.Time

	// base ends every build: serve cancels it at shutdown. A request's own context
	// never does.
	base    context.Context
	build   func(ctx context.Context, now time.Time) (snapshot, error)
	waitFor time.Duration // how long a request with no snapshot waits for the build

	mu        sync.Mutex
	snap      *snapshot     // the latest built; nil before the first build lands
	snapAt    time.Time     // the time snap was built for
	building  chan struct{} // non-nil while a build runs; closed when it ends
	stale     bool          // a mark changed what snap shows
	marks     int           // marks so far, so a build that began before one lands stale
	lastErr   error         // the last build's failure; nil once one succeeds
	lastErrAt time.Time
	builds    sync.WaitGroup
}

// snapshot is the dashboard's costly part, built away from requests: the brief input and
// the day's funnel.
type snapshot struct {
	in     brief.Input
	funnel store.Funnel
}

// snapshotView is the snapshot one request sees: the latest built (none when at is zero),
// whether a build runs, and when the last one failed if it did.
type snapshotView struct {
	snap     snapshot
	at       time.Time
	updating bool
	failedAt *time.Time
}

var _ console.Backend = (*backend)(nil)

func newBackend(base context.Context, s *store.Store, prof candidate.Config, loc *time.Location, cfg config.Config) *backend {
	b := &backend{s: s, prof: prof, loc: loc, cfg: cfg, now: time.Now, base: base, waitFor: dashboardWaitFor}
	b.build = b.buildSnapshot
	return b
}

// buildSnapshot is buildInput and the funnel for now. The console never rechecks, so the
// recheck is off unless LEADHOUND_LIVE allows LinkedIn, the same test linkedinClients makes
// for the brief.
func (b *backend) buildSnapshot(ctx context.Context, now time.Time) (snapshot, error) {
	local := now.In(b.loc)
	recheckOff := !b.cfg.Live.Allows(linkedinHost)
	in, err := buildInput(ctx, b.s.Q(), b.prof, b.loc, local, false, recheckOff)
	if err != nil {
		return snapshot{}, err
	}
	f, err := b.s.Q().FunnelOn(ctx, b.loc, local)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{in: in, funnel: f}, nil
}

// current is the snapshot for a request at now. A fresh one (same local day, younger than
// dashboardFreshFor, no mark since) is served as it is. Otherwise a build starts unless one
// runs, one failed less than dashboardRetryAfter ago, or serve is stopping; an old snapshot
// is served at once, marked updating, and with none the request waits up to waitFor for
// the running build before it takes what there is.
func (b *backend) current(ctx context.Context, now time.Time) snapshotView {
	b.mu.Lock()
	done := b.building
	if !b.freshLocked(now) && done == nil {
		done = b.startLocked(now)
	}
	if b.snap != nil || done == nil {
		v := b.viewLocked()
		b.mu.Unlock()
		return v
	}
	b.mu.Unlock()
	t := time.NewTimer(b.waitFor)
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
	t.Stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.viewLocked()
}

func (b *backend) freshLocked(now time.Time) bool {
	return b.snap != nil && !b.stale && b.snapAt.In(b.loc).Format(time.DateOnly) == now.In(b.loc).Format(time.DateOnly) &&
		!now.Before(b.snapAt) && now.Sub(b.snapAt) < dashboardFreshFor
}

func (b *backend) viewLocked() snapshotView {
	v := snapshotView{at: b.snapAt, updating: b.building != nil}
	if b.snap != nil {
		v.snap = *b.snap
	}
	if b.lastErr != nil {
		at := b.lastErrAt
		v.failedAt = &at
	}
	return v
}

// startLocked starts a build for now in its own goroutine and returns the channel closed
// when it ends; nil when a build already runs, the last one failed less than
// dashboardRetryAfter ago, or base has ended. b.mu is held; the build never holds it.
func (b *backend) startLocked(now time.Time) chan struct{} {
	if b.building != nil || b.base.Err() != nil ||
		b.lastErr != nil && b.now().Sub(b.lastErrAt) < dashboardRetryAfter {
		return nil
	}
	done := make(chan struct{})
	b.building = done
	marks := b.marks
	b.builds.Add(1)
	go func() {
		defer b.builds.Done()
		ctx, cancel := context.WithTimeout(b.base, dashboardBuildTimeout)
		snap, err := b.build(ctx, now)
		cancel()
		b.mu.Lock()
		defer b.mu.Unlock()
		b.building = nil
		close(done)
		if err != nil {
			b.lastErr, b.lastErrAt = err, b.now()
			return
		}
		b.snap, b.snapAt, b.lastErr = &snap, now, nil
		// A mark made while this build read the store is not in it: build again.
		if b.stale = marks != b.marks; b.stale {
			b.startLocked(b.now())
		}
	}()
	return done
}

// marked makes the snapshot stale after a mark and starts a rebuild at once.
func (b *backend) marked() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.marks++
	b.stale = true
	b.startLocked(b.now())
}

// wait returns once no build runs; serve calls it after cancelling base.
func (b *backend) wait() { b.builds.Wait() }

// Dashboard is the brief's health lines and tiers and the day's funnel from the snapshot,
// with whether it is being updated or its last update failed, and, read live, the day's
// scoring spend, the board registry, the latest runs, the last sent brief with its own
// stored health lines, and the leads card when leads are set up here.
func (b *backend) Dashboard(ctx context.Context) (console.Dashboard, error) {
	now := b.now()
	v := b.current(ctx, now)
	d := console.Dashboard{
		AsOf:              v.at,
		Updating:          v.updating && !v.at.IsZero(),
		Building:          v.updating && v.at.IsZero(),
		LastBuildFailedAt: v.failedAt,
		CallCap:           b.cfg.JevDailyCallCap,
	}
	if !v.at.IsZero() {
		in, f := v.snap.in, v.snap.funnel
		d.Day = in.Day
		d.Health = brief.HealthLines(in)
		d.ApplyNow, d.YourCallNow, d.Unscored, d.Unreachable = len(in.Apply), len(in.YourCall), in.Unscored, in.Unreachable
		d.Funnel = console.Funnel{NewBySource: f.NewBySource, Gated: f.Gated, GateFailed: f.GateFailed, Scored: f.Scored}
	}
	q := b.s.Q()
	local := now.In(b.loc)
	u, err := q.AIUsageOn(ctx, local)
	if err != nil {
		return console.Dashboard{}, err
	}
	d.AICalls, d.AIInputTokens = u.Calls, u.InputTokens
	d.AICostUSD = float64(u.InputTokens) * eval.CostPerMillionInput / 1e6
	boards, err := q.CountBoards(ctx, store.BoardFilter{})
	if err != nil {
		return console.Dashboard{}, err
	}
	for _, c := range boards {
		d.Boards = append(d.Boards, console.BoardCount{ATS: c.ATS, State: c.State, N: c.N})
	}
	if d.LastRuns, err = b.Runs(ctx, dashboardRuns); err != nil {
		return console.Dashboard{}, err
	}
	lb, err := q.LatestSentBrief(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return console.Dashboard{}, err
	default:
		day, err := time.ParseInLocation(time.DateOnly, lb.Day, b.loc)
		if err != nil {
			return console.Dashboard{}, fmt.Errorf("console: brief day %q: %w", lb.Day, err)
		}
		stamp := &console.BriefStamp{Day: day, Listed: len(lb.JobIDs), Health: storedHealth(lb.BodyText)}
		if lb.SentAt != nil {
			stamp.SentAt = *lb.SentAt
		}
		d.LastBrief = stamp
	}
	d.Leads = b.leadsSummary(ctx, now)
	return d, nil
}

// storedHealth is a sent brief's own "Health: " lines, from its stored text.
func storedHealth(body string) []string {
	var out []string
	for l := range strings.Lines(body) {
		l = strings.TrimRight(l, "\r\n")
		if strings.HasPrefix(l, "Health: ") {
			out = append(out, l)
		}
	}
	return out
}

// BriefJobs is the jobs of day's sent brief (YYYY-MM-DD), or of the latest sent brief when
// day is "", numbered from 1 as track resolves them, each with its current status. A job
// the store no longer has is shown as "job:ID (gone)" with no status. No such brief, or one
// never sent, is no jobs and no error; a day that is not YYYY-MM-DD is ErrUserFixable.
func (b *backend) BriefJobs(ctx context.Context, day string) (console.BriefJobs, error) {
	q := b.s.Q()
	var lb store.BriefLog
	var err error
	if day == "" {
		lb, err = q.LatestSentBrief(ctx)
	} else {
		if err := validTrackDay(day); err != nil {
			return console.BriefJobs{}, userFixable(err)
		}
		lb, err = q.BriefByDay(ctx, day)
		if err == nil && lb.SentAt == nil {
			return console.BriefJobs{}, nil
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		return console.BriefJobs{}, nil
	}
	if err != nil {
		return console.BriefJobs{}, err
	}
	briefDay, err := time.ParseInLocation(time.DateOnly, lb.Day, b.loc)
	if err != nil {
		return console.BriefJobs{}, fmt.Errorf("console: brief day %q: %w", lb.Day, err)
	}
	sums, err := q.JobSummaries(ctx, lb.JobIDs)
	if err != nil {
		return console.BriefJobs{}, err
	}
	out := console.BriefJobs{Day: briefDay, SentAt: lb.SentAt}
	for i, id := range lb.JobIDs {
		j := console.BriefJob{N: i + 1, JobID: id, Title: fmt.Sprintf("job:%d (gone)", id)}
		if s, ok := sums[id]; ok {
			j = briefJobOf(i+1, s)
		}
		out.Jobs = append(out.Jobs, j)
	}
	return out, nil
}

func briefJobOf(n int, s store.JobSummary) console.BriefJob {
	return console.BriefJob{N: n, JobID: s.JobID, Title: s.Title, Company: s.Company, Status: s.Status,
		ListingStatus: s.ListingStatus, URL: s.URL}
}

// trackedCap is the most jobs the Tracked page lists: "all" and "new" match nearly every job.
const trackedCap = 500

// Tracked is the tracker, as `leadhound track list --status` shows it, the latest trackedCap
// changes at most, with how many jobs match.
func (b *backend) Tracked(ctx context.Context, status string) (console.TrackedList, error) {
	if status != "" && status != "all" && !slices.Contains(store.JobStatuses, status) {
		return console.TrackedList{}, fmt.Errorf("%w: status must be all or one of %s", console.ErrUserFixable, strings.Join(store.JobStatuses, ", "))
	}
	rows, total, err := b.s.Q().TrackList(ctx, status, trackedCap)
	if err != nil {
		return console.TrackedList{}, err
	}
	out := console.TrackedList{Rows: make([]console.Tracked, 0, len(rows)), Total: total}
	for _, r := range rows {
		out.Rows = append(out.Rows, console.Tracked{JobID: r.JobID, Title: r.Title, Company: r.Company, Status: r.Status, Reason: r.Reason, Since: r.Since})
	}
	return out, nil
}

// Runs is the latest limit runs, newest first.
func (b *backend) Runs(ctx context.Context, limit int) ([]console.RunRow, error) {
	runs, err := b.s.Q().RecentRuns(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]console.RunRow, 0, len(runs))
	for _, r := range runs {
		out = append(out, console.RunRow{ID: r.ID, Command: r.Command, Source: r.Source, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
			Requests: r.Requests, Failures: r.Failures, StopReason: r.StopReason, Note: r.Note})
	}
	return out, nil
}

// Rules is every rule, disabled ones included.
func (b *backend) Rules(ctx context.Context) ([]console.RuleRow, error) {
	rules, err := b.s.Q().ListRules(ctx, true)
	if err != nil {
		return nil, err
	}
	out := make([]console.RuleRow, 0, len(rules))
	for _, r := range rules {
		out = append(out, console.RuleRow{ID: r.ID, Kind: r.Kind, Field: r.Field, Pattern: r.Pattern, Reason: r.Reason,
			FromJobID: r.CreatedFromJobID, CreatedAt: r.CreatedAt, DisabledAt: r.DisabledAt, DisabledReason: r.DisabledReason})
	}
	return out, nil
}

// Track marks one job as `leadhound track` does, through the same checks and trackJob; a
// number is read in the brief of req.Day, as --day does, or in the latest sent brief when
// it is "". Every refusal the operator can fix wraps console.ErrUserFixable with the CLI's
// own words, and Message is the CLI's outcome line. A reason is at most 500 characters.
func (b *backend) Track(ctx context.Context, req console.TrackRequest) (console.TrackOutcome, error) {
	if !slices.Contains(trackActions, req.Action) {
		return console.TrackOutcome{}, userFixable(usageError("%q is not an action; use one of %s", req.Action, strings.Join(trackActions, ", ")))
	}
	if utf8.RuneCountInString(req.Reason) > maxTrackReason {
		return console.TrackOutcome{}, userFixable(usageError("the reason is longer than %d characters", maxTrackReason))
	}
	if req.Day != "" {
		if err := validTrackDay(req.Day); err != nil {
			return console.TrackOutcome{}, userFixable(err)
		}
	}
	rule := ""
	if req.RuleField != "" || req.RulePattern != "" {
		rule = req.RuleField + "=" + req.RulePattern
	}
	if err := ruleGoesWithSkip(req.Action, rule); err != nil {
		return console.TrackOutcome{}, userFixable(err)
	}
	field, pattern, err := parseTrackRule(rule)
	if err != nil {
		return console.TrackOutcome{}, userFixable(err)
	}
	jobID, _, err := resolveJobRef(ctx, b.s.Q(), req.Ref, req.Day)
	if err != nil {
		return console.TrackOutcome{}, userFixable(err)
	}
	res, err := trackJob(ctx, b.s, trackRequest{JobID: jobID, Action: req.Action, Reason: req.Reason, RuleField: field, RulePattern: pattern})
	if err != nil {
		return console.TrackOutcome{}, userFixable(trackRefusal(err, jobID, res))
	}
	if !res.NoOp {
		b.marked()
	}
	n := 0
	if !strings.HasPrefix(req.Ref, "job:") {
		n, _ = strconv.Atoi(req.Ref) // already accepted by resolveJobRef
	}
	job := briefJobOf(n, res.Job)
	job.Status = res.To
	return console.TrackOutcome{Job: job, From: res.From, To: res.To, RuleID: res.RuleID, NoOp: res.NoOp, Noted: res.Noted,
		Message: trackOutcomeLine(res)}, nil
}

// validTrackDay refuses a brief day that is not YYYY-MM-DD.
func validTrackDay(day string) error {
	if _, err := time.Parse(time.DateOnly, day); err != nil {
		return usageError("the day must be YYYY-MM-DD, not %q", day)
	}
	return nil
}

// userFixable turns a usage error into the console's refusal, with the words after
// "usage: "; any other error comes back as it is.
func userFixable(err error) error {
	if !errors.Is(err, errUsage) {
		return err
	}
	words := strings.TrimPrefix(err.Error(), errUsage.Error()+": ")
	return fmt.Errorf("%w: %s", console.ErrUserFixable, words)
}
