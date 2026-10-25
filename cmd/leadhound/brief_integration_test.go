//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/mail"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// fakeMailer counts sends; the first failFirst sends fail as a provider error would, and
// otherwise send n fails with errs[n-1] when it is set.
type fakeMailer struct {
	hits, failFirst int
	errs            []error
	keys            []string
	msgs            []mail.Message
}

func (*fakeMailer) Provider() string { return "fake" }

func (f *fakeMailer) Send(_ context.Context, m mail.Message) (string, error) {
	f.hits++
	f.keys = append(f.keys, m.IdempotencyKey)
	f.msgs = append(f.msgs, m)
	if err := mail.Check(m.To); err != nil {
		return "", err
	}
	if f.hits <= f.failFirst {
		return "", &mail.ProviderError{Status: 503, Text: "unavailable"}
	}
	if f.hits <= len(f.errs) && f.errs[f.hits-1] != nil {
		return "", f.errs[f.hits-1]
	}
	return fmt.Sprintf("msg-%d", f.hits), nil
}

// useMailer points --send at m and sets fictional mail settings.
func useMailer(t *testing.T, m *fakeMailer) {
	t.Helper()
	saved := newMailer
	newMailer = func(config.Config) mail.Mailer { return m }
	t.Cleanup(func() { newMailer = saved })
	t.Setenv("RESEND_API_KEY", "test-key-not-real")
	t.Setenv("LEADHOUND_MAIL_FROM", "brief@example.invalid")
	t.Setenv("LEADHOUND_MAIL_TO", "reader@example.invalid")
}

// runCmdErr is runCmd that also returns stderr.
func runCmdErr(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	out := captureStdout(t)
	var stderr bytes.Buffer
	code := run(context.Background(), args, &stderr)
	return code, out.String(), stderr.String()
}

// fourJobs are the fixtures with three more English details, so four jobs pass every gate.
var fourJobs = map[string]string{
	"*jobPosting/1000000003": "detail_offsite.html",
	"*jobPosting/1000000009": "detail_offsite.html",
	"*jobPosting/1000000010": "detail_offsite.html",
}

// scoredFixtures collects the four-job fixtures and scores them with the fake, as a dev
// dry run does, and returns the fixture directory.
func scoredFixtures(t *testing.T) string {
	t.Helper()
	commandDB(t)
	dir := fixtureDir(t, fourJobs)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	if code, out := runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", dir); code != 0 {
		t.Fatalf("collect: exit %d, %q", code, out)
	}
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LEADHOUND_JEV_DAILY_CALL_CAP", "")
	for _, k := range []string{"RESEND_API_KEY", "LEADHOUND_MAIL_FROM", "LEADHOUND_MAIL_TO"} {
		t.Setenv(k, "")
	}
	if code, out := runCmd(t, "score", "--decider", "fake"); code != 0 {
		t.Fatalf("score: exit %d, %q", code, out)
	}
	return dir
}

func today() string {
	loc, err := time.LoadLocation(config.DefaultTZ)
	if err != nil {
		panic(err)
	}
	return time.Now().In(loc).Format(time.DateOnly)
}

func outboxFiles(t *testing.T, dir string) int {
	t.Helper()
	es, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

// TestBriefColdStart: on an empty database the brief says nothing is listed and why,
// without crashing, and exits 0.
func TestBriefColdStart(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	ob := filepath.Join(t.TempDir(), "outbox")
	code, out, stderr := runCmdErr(t, "brief", "--outbox", ob)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"nothing listed, see health", "Nothing listed today", "Health: LinkedIn did not run", "Health: recheck off (not live)", "not sent, not logged"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "LinkedIn fixtures") {
		t.Errorf("a source that did not run is listed:\n%s", out)
	}
	if outboxFiles(t, ob) != 1 {
		t.Errorf("outbox files = %d, want 1", outboxFiles(t, ob))
	}
	if got := dbCount(t, `SELECT count(*) FROM brief_log`); got != 0 {
		t.Errorf("brief_log rows = %d, want 0", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'brief' AND finished_at IS NOT NULL`); got != 1 {
		t.Errorf("brief runs = %d, want 1", got)
	}
}

// TestBriefDryRun: with the fake's verdicts allowed the four jobs are listed; nothing is
// logged, shortlisted or requested (live is off, so the recheck says so). Without
// --allow-fake the same jobs are honestly not scored.
func TestBriefDryRun(t *testing.T) {
	scoredFixtures(t)
	fetches := dbCount(t, `SELECT count(*) FROM http_fetch`)
	ob := filepath.Join(t.TempDir(), "outbox")

	code, out, stderr := runCmdErr(t, "brief", "--allow-fake", "--outbox", ob)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"4 to apply", "\nApply\n1. ", "\n4. ", "Health: LinkedIn did not run", "Health: LinkedIn fixtures ok · 10 new", "Health: recheck off (not live)", "Find a referral: ", "Skipped today: "} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if got := dbCount(t, `SELECT count(*) FROM http_fetch`); got != fetches {
		t.Errorf("fetch rows = %d, want %d: the recheck made a request with live off", got, fetches)
	}
	for _, q := range []string{`SELECT count(*) FROM brief_log`, `SELECT count(*) FROM status_event WHERE status = 'shortlisted'`, `SELECT count(*) FROM run WHERE command = 'recheck'`} {
		if got := dbCount(t, q); got != 0 {
			t.Errorf("%s = %d, want 0", q, got)
		}
	}

	code, out, _ = runCmdErr(t, "brief", "--outbox", ob)
	if code != 0 || !strings.Contains(out, "Nothing listed today") || !strings.Contains(out, "not scored yet") {
		t.Fatalf("without --allow-fake: exit %d\n%s", code, out)
	}
	if outboxFiles(t, ob) != 2 {
		t.Errorf("outbox files = %d, want 2", outboxFiles(t, ob))
	}
}

func TestBriefSendNeedsMailSettings(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	for _, k := range []string{"RESEND_API_KEY", "LEADHOUND_MAIL_FROM", "LEADHOUND_MAIL_TO"} {
		t.Setenv(k, "")
	}
	code, _, stderr := runCmdErr(t, "brief", "--send")
	if code != 2 || !strings.Contains(stderr, "RESEND_API_KEY is not set") || !strings.Contains(stderr, "LEADHOUND_MAIL_TO is not set") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	t.Setenv("RESEND_API_KEY", "test-key-not-real")
	t.Setenv("LEADHOUND_MAIL_FROM", "brief@example.invalid")
	t.Setenv("LEADHOUND_MAIL_TO", "Reader <reader@example.invalid>")
	if code, _, stderr := runCmdErr(t, "brief", "--send"); code != 2 || !strings.Contains(stderr, "LEADHOUND_MAIL_TO must be one plain address") {
		t.Fatalf("display-name recipient: exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stderr, "test-key-not-real") {
		t.Error("the key reached stderr")
	}
	if got := dbCount(t, `SELECT count(*) FROM run`); got != 0 {
		t.Errorf("runs = %d, want 0: a usage error touched the database", got)
	}
}

// TestBriefSendOnce: a sent day is logged, its new jobs shortlisted, and a second send the
// same day is refused without a second email.
func TestBriefSendOnce(t *testing.T) {
	scoredFixtures(t)
	m := &fakeMailer{}
	useMailer(t, m)
	code, out, stderr := runCmdErr(t, "brief", "--send", "--allow-fake")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if m.hits != 1 || m.keys[0] != "brief-"+today() {
		t.Fatalf("mailer hits %d, keys %v", m.hits, m.keys)
	}
	if !strings.Contains(out, "sent through fake · 4 listed · 4 shortlisted") {
		t.Errorf("stdout:\n%s", out)
	}
	if got := dbCount(t, `SELECT count(*) FROM brief_log WHERE sent_at IS NOT NULL AND provider_id = 'msg-1' AND cardinality(job_ids) = 4`); got != 1 {
		t.Errorf("sent brief rows = %d, want 1", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM status_event WHERE status = 'shortlisted' AND actor = 'system'`); got != 4 {
		t.Errorf("shortlisted = %d, want 4", got)
	}

	code, out, _ = runCmdErr(t, "brief", "--send", "--allow-fake")
	if code != 0 || !strings.Contains(out, "already sent") {
		t.Fatalf("second send: exit %d\n%s", code, out)
	}
	if m.hits != 1 {
		t.Errorf("mailer hits after the second send = %d, want 1", m.hits)
	}
	if got := dbCount(t, `SELECT count(*) FROM status_event WHERE status = 'shortlisted'`); got != 4 {
		t.Errorf("shortlisted after the refusal = %d, want 4", got)
	}
}

// TestBriefFailedSendRetries: a failed send exits 1 and leaves the day unsent, so the
// re-run delivers it under the same key.
func TestBriefFailedSendRetries(t *testing.T) {
	scoredFixtures(t)
	m := &fakeMailer{failFirst: 1}
	useMailer(t, m)
	code, _, stderr := runCmdErr(t, "brief", "--send", "--allow-fake")
	if code != 1 {
		t.Fatalf("failed send: exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stderr, "test-key-not-real") {
		t.Error("the key reached stderr")
	}
	if got := dbCount(t, `SELECT count(*) FROM brief_log WHERE sent_at IS NULL`); got != 1 {
		t.Errorf("unsent rows = %d, want 1", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM status_event WHERE status = 'shortlisted'`); got != 0 {
		t.Errorf("shortlisted after a failed send = %d, want 0", got)
	}

	code, _, stderr = runCmdErr(t, "brief", "--send", "--allow-fake")
	if code != 0 {
		t.Fatalf("retry: exit %d, stderr %q", code, stderr)
	}
	if m.hits != 2 || m.keys[0] != m.keys[1] {
		t.Fatalf("mailer hits %d, keys %v", m.hits, m.keys)
	}
	if got := dbCount(t, `SELECT count(*) FROM brief_log WHERE sent_at IS NOT NULL AND provider_id = 'msg-2'`); got != 1 {
		t.Errorf("sent rows = %d, want 1", got)
	}
}

// TestBriefStatusGrid: the grid's brief column. New is listed and shortlisted, shortlisted
// is listed again without a second event, applied and skipped are never listed.
func TestBriefStatusGrid(t *testing.T) {
	scoredFixtures(t)
	conn := storetest.OwnerConn(t)
	ctx := context.Background()
	jobs := func(q string) []int64 {
		rows, err := conn.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	listable := jobs(`SELECT DISTINCT job_id FROM decision ORDER BY job_id`)
	if len(listable) != 4 {
		t.Fatalf("listable jobs = %v, want 4", listable)
	}
	applied, skipped, shortlisted, fresh := listable[0], listable[1], listable[2], listable[3]
	for id, st := range map[int64]string{applied: "applied", skipped: "skipped", shortlisted: "shortlisted"} {
		if _, err := conn.Exec(ctx, `INSERT INTO status_event (job_id, status, actor) VALUES ($1, $2, 'gremy')`, id, st); err != nil {
			t.Fatal(err)
		}
	}
	m := &fakeMailer{}
	useMailer(t, m)
	if code, out, stderr := runCmdErr(t, "brief", "--send", "--allow-fake"); code != 0 || !strings.Contains(out, "2 to apply") {
		t.Fatalf("exit %d, stderr %q\n%s", code, stderr, out)
	}
	got := jobs(`SELECT unnest(job_ids) FROM brief_log ORDER BY 1`)
	if len(got) != 2 || got[0] != shortlisted || got[1] != fresh {
		t.Errorf("listed = %v, want [%d %d]", got, shortlisted, fresh)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM status_event WHERE job_id = %d AND status = 'shortlisted'`, shortlisted)); n != 1 {
		t.Errorf("shortlisted job's shortlisted events = %d, want 1", n)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM status_event WHERE job_id = %d AND status = 'shortlisted' AND actor = 'system'`, fresh)); n != 1 {
		t.Errorf("new job not shortlisted")
	}
	for id, want := range map[int64]string{applied: "applied", skipped: "skipped"} {
		var st string
		if err := conn.QueryRow(ctx, `SELECT status FROM status_event WHERE job_id = $1 ORDER BY at DESC, id DESC LIMIT 1`, id).Scan(&st); err != nil || st != want {
			t.Errorf("job %d status = %q (%v), want %s", id, st, err, want)
		}
	}
}

// TestRecheckCommand: live off is said with no request; a dry run rechecks the stale
// listed jobs from fixtures, and the rescore asks the decider nothing for unchanged text.
func TestRecheckCommand(t *testing.T) {
	dir := scoredFixtures(t)
	fetches := dbCount(t, `SELECT count(*) FROM http_fetch`)
	code, out, _ := runCmdErr(t, "recheck", "--allow-fake")
	if code != 0 || !strings.Contains(out, "live fetching is off here") {
		t.Fatalf("live off: exit %d, %q", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM http_fetch`); got != fetches {
		t.Fatalf("fetch rows = %d, want %d", got, fetches)
	}

	// Fresh listings are not due.
	if code, out, _ := runCmdErr(t, "recheck", "--allow-fake", "--dry-run", "--fixtures", dir); code != 0 || !strings.Contains(out, "nothing due") {
		t.Fatalf("fresh: exit %d, %q", code, out)
	}
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), `UPDATE http_fetch SET fetched_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	usage := dbCount(t, `SELECT coalesce(sum(calls), 0) FROM ai_usage`)
	code, out, stderr := runCmdErr(t, "recheck", "--allow-fake", "--dry-run", "--fixtures", dir)
	if code != 0 || !strings.Contains(out, "4 due, 4 open, 0 expired, 0 unreachable") || !strings.Contains(out, "rescored: scored 0 · unchanged 4") {
		t.Fatalf("stale: exit %d, stderr %q\n%s", code, stderr, out)
	}
	if got := dbCount(t, `SELECT coalesce(sum(calls), 0) FROM ai_usage`); got != usage {
		t.Errorf("decider calls = %d, want %d: unchanged text was sent again", got, usage)
	}
	if got := dbCount(t, `SELECT count(*) FROM job WHERE listing_status = 'open' AND status_checked_at IS NOT NULL`); got != 4 {
		t.Errorf("checked open jobs = %d, want 4", got)
	}
}

// TestRecheckRescoreWaitsForScoreLock: while another score run holds the lock, the
// recheck's rescore records no score run and says why; the recheck itself still runs.
func TestRecheckRescoreWaitsForScoreLock(t *testing.T) {
	dir := scoredFixtures(t)
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), `UPDATE http_fetch SET fetched_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	runs := dbCount(t, `SELECT count(*) FROM run WHERE command = 'score'`)
	holdScoreLock(t)
	code, out, stderr := runCmdErr(t, "recheck", "--allow-fake", "--dry-run", "--fixtures", dir)
	if code != 0 || !strings.Contains(out, "4 due") || !strings.Contains(out, "not rescored: another score run is in progress") {
		t.Fatalf("exit %d, stderr %q\n%s", code, stderr, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'score'`); got != runs {
		t.Errorf("score runs = %d, want %d", got, runs)
	}
}

// TestBriefRecordedStopsExitZero: a source that broke and a scorer stopped by the cap are
// reported in the brief, which exits 0, never "nothing new".
func TestBriefRecordedStopsExitZero(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`INSERT INTO run (command, source, finished_at, stop_reason, note) VALUES
		   ('collect', 'linkedin', now(), 'parser_broke', 'parser broke on search page'),
		   ('canary', 'linkedin', now(), '', 'failed: no response: connection reset'),
		   ('score', '', now(), 'spend_cap', '')`); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCmdErr(t, "brief", "--outbox", filepath.Join(t.TempDir(), "o"))
	if code != 0 {
		t.Fatalf("exit %d, %q", code, stderr)
	}
	for _, want := range []string{"Health: LinkedIn parser broke: no LinkedIn jobs today until it is fixed", "Health: Scoring paused: daily Jev cap reached", "Nothing listed today"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Nothing new worth applying") {
		t.Errorf("a broken source read as a quiet day:\n%s", out)
	}
}

// discardRecorder records nothing, so a recheck finds no fetch row for its page: an error
// inside the recheck run.
type discardRecorder struct{}

func (discardRecorder) Record(context.Context, fetch.Record) error { return nil }

// useRecheck turns the brief's recheck on, served from the fixtures in dir under the
// fixture source; with brokenRecorder each recheck fails after its first request.
func useRecheck(t *testing.T, dir string, brokenRecorder bool) *fetch.FixtureTransport {
	t.Helper()
	ft, err := fetch.NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := briefClients
	briefClients = func(_ config.Config, _ fetchFlags, budgets fetch.Budgets) (func(fetch.Recorder) *fetch.Client, string, error) {
		return func(rec fetch.Recorder) *fetch.Client {
			if brokenRecorder {
				rec = discardRecorder{}
			}
			return fetch.New(fetch.WithFixtures(ft), fetch.WithRecorder(rec), fetch.WithProfile(fetch.LinkedInJobs),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)), fetch.WithJitter(0), fetch.WithBreaker(fetch.NewBreaker(budgets)))
		}, linkedin.SourceFixture, nil
	}
	t.Cleanup(func() { briefClients = saved })
	return ft
}

// makeStale ages every listing's last check past the recheck window, except keep's.
func makeStale(t *testing.T, keep ...int64) {
	t.Helper()
	conn := storetest.OwnerConn(t)
	ctx := context.Background()
	if keep == nil {
		keep = []int64{}
	}
	if _, err := conn.Exec(ctx, `UPDATE http_fetch SET fetched_at = now() - interval '2 days'
		WHERE id NOT IN (SELECT fetch_id FROM job_observation WHERE job_id = ANY($1))`, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE job SET status_checked_at = now() - interval '2 days' WHERE NOT (id = ANY($1))`, keep); err != nil {
		t.Fatal(err)
	}
}

func briefJobIDs(t *testing.T) []int64 {
	t.Helper()
	var ids []int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT job_ids FROM brief_log`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

// skipJob files a job as skipped, so a fresh compose would no longer list it.
func skipJob(t *testing.T, id int64) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`INSERT INTO status_event (job_id, status, actor) VALUES ($1, 'skipped', 'gremy')`, id); err != nil {
		t.Fatal(err)
	}
}

// TestBriefRetryResendsStoredBrief: the first try delivers but its answer is lost; by the
// retry the list has changed. The retry rechecks nothing, sends the identical payload
// under the same key, and marks the list that was delivered.
func TestBriefRetryResendsStoredBrief(t *testing.T) {
	dir := scoredFixtures(t)
	ft := useRecheck(t, dir, false)
	makeStale(t)
	m := &fakeMailer{errs: []error{errors.New("connection reset after the request was written")}}
	useMailer(t, m)

	if code, _, stderr := runCmdErr(t, "brief", "--send", "--allow-fake"); code != 1 {
		t.Fatalf("lost answer: exit %d, stderr %q", code, stderr)
	}
	delivered := briefJobIDs(t)
	requests, rechecks := ft.Hits(), dbCount(t, `SELECT count(*) FROM run WHERE command = 'recheck'`)
	if len(delivered) != 4 || requests == 0 || rechecks != 1 {
		t.Fatalf("first try: listed %v, recheck requests %d, recheck runs %d", delivered, requests, rechecks)
	}

	skipJob(t, delivered[0])
	makeStale(t)
	code, out, stderr := runCmdErr(t, "brief", "--send", "--allow-fake")
	if code != 0 || !strings.Contains(out, "resending the unsent brief") {
		t.Fatalf("retry: exit %d, stderr %q\n%s", code, stderr, out)
	}
	if ft.Hits() != requests || dbCount(t, `SELECT count(*) FROM run WHERE command = 'recheck'`) != rechecks {
		t.Errorf("the retry rechecked: requests %d → %d", requests, ft.Hits())
	}
	if m.hits != 2 || !reflect.DeepEqual(m.msgs[0], m.msgs[1]) || m.msgs[0].HTML == "" {
		t.Errorf("payloads differ (or no HTML):\n%+v\n%+v", m.msgs[0], m.msgs[1])
	}
	if got := briefJobIDs(t); !slices.Equal(got, delivered) {
		t.Errorf("job_ids = %v, want the delivered %v", got, delivered)
	}
	if n := dbCount(t, `SELECT count(*) FROM brief_log WHERE sent_at IS NOT NULL AND provider_id = 'msg-2'`); n != 1 {
		t.Errorf("sent rows = %d, want 1", n)
	}
	// Every stored job but the one skipped meanwhile was new, and is shortlisted.
	if n := dbCount(t, `SELECT count(*) FROM status_event WHERE status = 'shortlisted' AND actor = 'system'`); n != 3 {
		t.Errorf("shortlisted = %d, want 3", n)
	}
}

// TestBriefRetryAlreadySent: a retry the provider answers with "key already used" counts as
// delivered and marks the stored list.
func TestBriefRetryAlreadySent(t *testing.T) {
	scoredFixtures(t)
	m := &fakeMailer{errs: []error{
		&mail.ProviderError{Status: 504, Text: "gateway time-out"},
		&mail.ProviderError{Status: 409, Name: "invalid_idempotent_request", Text: "different payload"},
	}}
	useMailer(t, m)
	if code, _, _ := runCmdErr(t, "brief", "--send", "--allow-fake"); code != 1 {
		t.Fatalf("first try: exit %d", code)
	}
	delivered := briefJobIDs(t)
	skipJob(t, delivered[1])
	if code, out, stderr := runCmdErr(t, "brief", "--send", "--allow-fake"); code != 0 || !strings.Contains(out, "4 listed · 3 shortlisted") {
		t.Fatalf("retry: exit %d, stderr %q\n%s", code, stderr, out)
	}
	if got := briefJobIDs(t); !slices.Equal(got, delivered) {
		t.Errorf("job_ids = %v, want %v", got, delivered)
	}
	if n := dbCount(t, `SELECT count(*) FROM brief_log WHERE sent_at IS NOT NULL AND provider_id = ''`); n != 1 {
		t.Errorf("sent rows = %d, want 1", n)
	}
	if m.keys[0] != m.keys[1] {
		t.Errorf("keys %v", m.keys)
	}
}

// TestBriefRefusedRecomposes: a first try the provider refused sent nothing, so the next
// run composes afresh, under a new key, still without rechecking.
func TestBriefRefusedRecomposes(t *testing.T) {
	dir := scoredFixtures(t)
	m := &fakeMailer{errs: []error{&mail.ProviderError{Status: 422, Name: "validation_error", Text: "invalid from"}}}
	useMailer(t, m)
	if code, _, _ := runCmdErr(t, "brief", "--send", "--allow-fake"); code != 1 {
		t.Fatalf("refused: exit %d", code)
	}
	if n := dbCount(t, `SELECT count(*) FROM brief_log WHERE send_state = 'refused'`); n != 1 {
		t.Fatalf("refused rows = %d, want 1", n)
	}
	first := briefJobIDs(t)
	skipJob(t, first[0])
	ft := useRecheck(t, dir, false)
	makeStale(t, first[1:]...)
	if code, _, stderr := runCmdErr(t, "brief", "--send", "--allow-fake"); code != 0 {
		t.Fatalf("recompose: exit %d, stderr %q", code, stderr)
	}
	if ft.Hits() != 0 {
		t.Errorf("the recompose rechecked: %d requests", ft.Hits())
	}
	if !strings.HasPrefix(m.keys[1], "brief-"+today()+"-r") {
		t.Errorf("keys %v: the recompose reused the refused key", m.keys)
	}
	if got := briefJobIDs(t); !slices.Equal(got, first[1:]) {
		t.Errorf("job_ids = %v, want %v", got, first[1:])
	}
}

// TestBriefRecheckFailureStillComposes: a recheck that fails is recorded in its run row and
// the brief goes out anyway; the jobs it never reached are not listed.
func TestBriefRecheckFailureStillComposes(t *testing.T) {
	dir := scoredFixtures(t)
	useRecheck(t, dir, true)
	makeStale(t)
	code, out, stderr := runCmdErr(t, "brief", "--allow-fake", "--outbox", filepath.Join(t.TempDir(), "o"))
	if code != 0 || !strings.Contains(out, "Nothing listed today") {
		t.Fatalf("exit %d, stderr %q\n%s", code, stderr, out)
	}
	if n := dbCount(t, `SELECT count(*) FROM run WHERE command = 'recheck' AND finished_at IS NOT NULL AND note LIKE 'failed: %no fetch row recorded%'`); n != 1 {
		t.Errorf("failed recheck runs = %d, want 1", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM job WHERE listing_status = 'unreachable'`); n != 4 {
		t.Errorf("unreachable = %d, want 4", n)
	}
}

// TestBriefLeavesOutStaleJobs: with caps of one, the recheck covers the top two; both turn
// out unreachable. The third, past the window and unchecked, is left out; the fourth,
// checked recently, is listed.
func TestBriefLeavesOutStaleJobs(t *testing.T) {
	scoredFixtures(t)
	demo, err := os.ReadFile(filepath.Join(examplesDir, demoProfileFile))
	if err != nil {
		t.Fatal(err)
	}
	prof := strings.Replace(string(demo), "brief: {max_apply: 5, max_your_call: 5}", "brief: {max_apply: 1, max_your_call: 1}", 1)
	if prof == string(demo) {
		t.Fatal("the demo's brief caps moved")
	}
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, candidate.FileName), []byte(prof), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_DATA_DIR", data)
	loc, err := time.LoadLocation(config.DefaultTZ)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	full := scoring.Rank(mustCandidates(t, st), loc, scoring.Caps{Apply: 10, YourCall: 10})
	if len(full.Apply) != 4 {
		t.Fatalf("apply tier = %d, want 4", len(full.Apply))
	}
	third, fourth := full.Apply[2].JobID, full.Apply[3].JobID

	// No route for any job page: the two rechecked come back 404, so unreachable.
	fx := t.TempDir()
	if err := os.WriteFile(filepath.Join(fx, fetch.ManifestFile), []byte(`{"*nothing-here*": "page.html"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx, "page.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	ft := useRecheck(t, fx, false)
	makeStale(t, fourth)

	useMailer(t, &fakeMailer{})
	code, out, stderr := runCmdErr(t, "brief", "--send", "--allow-fake")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q\n%s", code, stderr, out)
	}
	if ft.Hits() != 2 {
		t.Errorf("recheck requests = %d, want 2", ft.Hits())
	}
	var status3 string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT listing_status FROM job WHERE id = $1`, third).Scan(&status3); err != nil || status3 != "unreachable" {
		t.Errorf("third job = %q (%v), want unreachable", status3, err)
	}
	if got := briefJobIDs(t); !slices.Equal(got, []int64{fourth}) {
		t.Errorf("listed %v, want only the recently checked job %d:\n%s", got, fourth, out)
	}
}

// mustCandidates reads every job as the ranking sees it, through the test oracle (the old,
// read-everything ranking read).
func mustCandidates(t *testing.T, s *store.Store) []scoring.Candidate {
	t.Helper()
	ins, err := storetest.OracleRankInputs(context.Background(), s, true)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := s.Q().ActiveRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return scoring.CandidatesFrom(ins, rules)
}
