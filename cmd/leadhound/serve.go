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

// dashboardCacheFor is how long the dashboard keeps the brief input it built, on one local
// day: building it reads every rank input.
const dashboardCacheFor = time.Minute

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

	logger := slog.Default()
	if *dev {
		logger.Warn("console dev mode: cookies are not Secure and carry no __Host- prefix; for local use only")
	}
	srv := &http.Server{
		Addr:              cfg.ConsoleAddr,
		Handler:           console.New(newBackend(s, prof, loc, cfg), auth, console.Options{Dev: *dev, Location: loc, Logger: logger}),
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

	mu    sync.Mutex
	cache dashboardInput
}

// dashboardInput is the brief input the dashboard last built, for one local day.
type dashboardInput struct {
	day string // YYYY-MM-DD in loc; "" when nothing is kept
	at  time.Time
	in  brief.Input
}

var _ console.Backend = (*backend)(nil)

func newBackend(s *store.Store, prof candidate.Config, loc *time.Location, cfg config.Config) *backend {
	return &backend{s: s, prof: prof, loc: loc, cfg: cfg, now: time.Now}
}

// briefInput is buildInput for now, kept for a minute on the same local day. The console
// never rechecks, so the recheck is off unless LEADHOUND_LIVE allows LinkedIn, the same
// test linkedinClients makes for the brief.
func (b *backend) briefInput(ctx context.Context, now time.Time) (brief.Input, time.Time, error) {
	day := now.In(b.loc).Format(time.DateOnly)
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.cache; c.day == day && now.Sub(c.at) < dashboardCacheFor && !now.Before(c.at) {
		return c.in, c.at, nil
	}
	recheckOff := !b.cfg.Live.Allows(linkedinHost)
	in, err := buildInput(ctx, b.s.Q(), b.prof, b.loc, now.In(b.loc), false, recheckOff)
	if err != nil {
		return brief.Input{}, time.Time{}, err
	}
	b.cache = dashboardInput{day: day, at: now, in: in}
	return in, now, nil
}

// forgetInput drops the kept brief input, after a mark changed what it would show.
func (b *backend) forgetInput() {
	b.mu.Lock()
	b.cache = dashboardInput{}
	b.mu.Unlock()
}

// Dashboard is the live state: the brief's health lines and tiers as the brief would build
// them now, today's funnel and scoring spend, the board registry, the latest runs and the
// last sent brief with its own stored health lines.
func (b *backend) Dashboard(ctx context.Context) (console.Dashboard, error) {
	now := b.now()
	in, asOf, err := b.briefInput(ctx, now)
	if err != nil {
		return console.Dashboard{}, err
	}
	d := console.Dashboard{
		AsOf:        asOf,
		Day:         in.Day,
		Health:      brief.HealthLines(in),
		ApplyNow:    len(in.Apply),
		YourCallNow: len(in.YourCall),
		Unscored:    in.Unscored,
		Unreachable: in.Unreachable,
		CallCap:     b.cfg.JevDailyCallCap,
	}
	q := b.s.Q()
	local := now.In(b.loc)
	f, err := q.FunnelOn(ctx, b.loc, local)
	if err != nil {
		return console.Dashboard{}, err
	}
	d.Funnel = console.Funnel{NewBySource: f.NewBySource, Gated: f.Gated, GateFailed: f.GateFailed, Scored: f.Scored}
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

// Tracked is the tracker, as `leadhound track list --status` shows it.
func (b *backend) Tracked(ctx context.Context, status string) ([]console.Tracked, error) {
	if status != "" && status != "all" && !slices.Contains(store.JobStatuses, status) {
		return nil, fmt.Errorf("%w: status must be all or one of %s", console.ErrUserFixable, strings.Join(store.JobStatuses, ", "))
	}
	rows, err := b.s.Q().TrackList(ctx, status)
	if err != nil {
		return nil, err
	}
	out := make([]console.Tracked, 0, len(rows))
	for _, r := range rows {
		out = append(out, console.Tracked{JobID: r.JobID, Title: r.Title, Company: r.Company, Status: r.Status, Reason: r.Reason, Since: r.Since})
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
		b.forgetInput()
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
