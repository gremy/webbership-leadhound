//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const apiTestToken = "a-test-api-token-only"

func apiTestHash() string {
	sum := sha256.Sum256([]byte(apiTestToken))
	return hex.EncodeToString(sum[:])
}

// apiHandler is the console over b with the API on, and the Cookie header of a signed-in session.
func apiHandler(t *testing.T, b *backend) (http.Handler, string) {
	t.Helper()
	a, err := console.NewAuth("operator@mail.example", testHash(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return console.New(b, a, console.Options{Dev: true, Location: b.loc, Now: b.now, APITokenSHA256: apiTestHash()}), "lh_session=" + token
}

// apiCall sends one bearer request to h and returns the status and the raw body.
func apiCall(t *testing.T, h http.Handler, method, target, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+apiTestToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s %s: Content-Type %q", method, target, ct)
	}
	return w.Code, w.Body.String()
}

func apiDecoded[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return v
}

func apiNext(t *testing.T, h http.Handler, query string) []console.APINextJob {
	t.Helper()
	code, raw := apiCall(t, h, http.MethodGet, "/api/jobs/next"+query, "")
	if code != http.StatusOK {
		t.Fatalf("next: %d %s", code, raw)
	}
	return apiDecoded[console.APINextJobs](t, raw).Jobs
}

func apiNextIDs(t *testing.T, h http.Handler) []int64 {
	t.Helper()
	var ids []int64
	for _, j := range apiNext(t, h, "") {
		ids = append(ids, j.JobID)
	}
	return ids
}

// apiMark is one mark through the API.
func apiMark(t *testing.T, h http.Handler, id int64, action, reason string) (int, string) {
	t.Helper()
	body, err := json.Marshal(console.APITrackRequest{Action: action, Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return apiCall(t, h, http.MethodPost, fmt.Sprintf("/api/jobs/%d/track", id), string(body))
}

// sessionMark is the same mark through the console's own form route.
func sessionMark(t *testing.T, h http.Handler, cookie string, id int64, action, reason string) (int, string) {
	t.Helper()
	form := url.Values{"action": {action}, "reason": {reason}}
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/jobs/%d/track", id), strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// setStatus gives a job a status as the pipeline would, so a case starts where it says.
func setStatus(t *testing.T, b *backend, id int64, status string) {
	t.Helper()
	if _, err := b.s.Q().InsertStatusEvent(context.Background(), id, status, "", "system", nil); err != nil {
		t.Fatal(err)
	}
}

// bareJob is a job as a card first makes it: no observation, only its new event.
func bareJob(t *testing.T, b *backend, name string) int64 {
	t.Helper()
	ctx := context.Background()
	q := b.s.Q()
	company, err := q.UpsertCompany(ctx, name+" Co", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := q.UpsertJob(ctx, store.JobUpsert{Source: "linkedin", SourceJobID: name, CompanyID: company, Title: name + " card title", URL: "https://jobs.example/" + name})
	if err != nil {
		t.Fatal(err)
	}
	setStatus(t, b, id, "new")
	return id
}

// sendBriefOn stores a brief sent on day that lists ids.
func sendBriefOn(t *testing.T, day string, ids ...int64) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`INSERT INTO brief_log (day, sent_at, subject, body_text, job_ids, send_state)
		 VALUES ($1::date, now(), 's', 'b', $2, 'maybe_sent')`, day, ids); err != nil {
		t.Fatal(err)
	}
}

// TestAPIContractDrift: the same mark through the session route and the API, on twin jobs from
// the same start, writes the same status, reason and actor, and a refusal is refused alike.
func TestAPIContractDrift(t *testing.T) {
	ids := sentBrief(t)
	b := testBackend(t)
	h, cookie := apiHandler(t, b)
	viaForm, viaAPI := ids[0], ids[1]

	for _, c := range []struct {
		from, action, reason string
		code                 int
		to                   string
	}{
		{"new", "applied", "  via their site  ", http.StatusOK, "applied"},
		{"shortlisted", "applied", "", http.StatusOK, "applied"},
		{"shortlisted", "skip", "too senior", http.StatusOK, "skipped"},
		{"shortlisted", "maybe", "ask about the visa", http.StatusOK, "shortlisted"},
		{"shortlisted", "maybe", "", http.StatusOK, "shortlisted"},
		{"applied", "skip", "changed my mind", http.StatusUnprocessableEntity, "applied"},
	} {
		t.Run(c.from+" "+c.action+" "+c.reason, func(t *testing.T) {
			setStatus(t, b, viaForm, c.from)
			setStatus(t, b, viaAPI, c.from)
			formCode, formBody := sessionMark(t, h, cookie, viaForm, c.action, c.reason)
			apiCode, apiBody := apiMark(t, h, viaAPI, c.action, c.reason)
			if formCode != c.code || apiCode != c.code {
				t.Fatalf("form %d, api %d (%s), want %d", formCode, apiCode, apiBody, c.code)
			}
			if a, f := readTrackRows(t, viaAPI), readTrackRows(t, viaForm); !equalRows(a, f) || a.status != c.to {
				t.Errorf("api wrote %+v, form wrote %+v; want status %s", a, f, c.to)
			}
			if c.code != http.StatusOK {
				if words := apiDecoded[console.APIError](t, apiBody).Error; !strings.Contains(formBody, words) {
					t.Errorf("the API's refusal %q is not the form's", words)
				}
				return
			}
			if out := apiDecoded[console.APITrackOutcome](t, apiBody); out.From != c.from || out.To != c.to {
				t.Errorf("outcome = %+v", out)
			}
		})
	}
}

// TestAPINextAgreesWithTracked: every job next lists stands in Tracked at the status next gives,
// and a marked job leaves next and stands in Tracked at its mark.
func TestAPINextAgreesWithTracked(t *testing.T) {
	ids := sentBrief(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	ctx := context.Background()

	if code, raw := apiMark(t, h, ids[0], "applied", "via their site"); code != http.StatusOK {
		t.Fatalf("mark: %d %s", code, raw)
	}
	if code, raw := apiMark(t, h, ids[1], "skip", ""); code != http.StatusOK {
		t.Fatalf("mark: %d %s", code, raw)
	}
	if got := apiNextIDs(t, h); !slices.Equal(got, ids[2:]) {
		t.Errorf("next = %v, want the unmarked %v", got, ids[2:])
	}
	for _, j := range apiNext(t, h, "") {
		tr, err := b.Tracked(ctx, j.Status)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(tr.Rows, func(r console.Tracked) bool { return r.JobID == j.JobID && r.Status == j.Status }) {
			t.Errorf("next has job %d at %s; Tracked has %+v", j.JobID, j.Status, tr.Rows)
		}
	}
	code, raw := apiCall(t, h, http.MethodGet, "/api/tracked", "")
	if code != http.StatusOK {
		t.Fatalf("tracked: %d %s", code, raw)
	}
	tr := apiDecoded[console.APITracked](t, raw)
	if tr.Total != 1 || len(tr.Rows) != 1 || tr.Rows[0].JobID != ids[0] || tr.Rows[0].Reason != "via their site" {
		t.Errorf("tracked = %+v", tr)
	}
}

// TestAPIStatusGrid: every status against every mark the API makes, through the real backend and
// handler. What a mark may do is the API's limit on Track's decision: applied and skip only from
// new and shortlisted, or as the no-op from where they already stand. These rows pin it, and that
// next lists only new and shortlisted. A no-op and a refusal write nothing, and maybe only notes.
func TestAPIStatusGrid(t *testing.T) {
	const moves, noop, refused, pastApplying = "moves", "no-op", "refused", "past applying"
	ids := sentBrief(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	id := ids[0]

	for _, c := range []struct {
		status        string
		inNext        bool
		applied, skip string
	}{
		{"new", true, moves, moves},
		{"shortlisted", true, moves, moves},
		{"applied", false, noop, pastApplying},
		{"interviewing", false, refused, pastApplying},
		{"offer", false, refused, pastApplying},
		{"rejected", false, refused, refused},
		{"skipped", false, refused, noop},
		{"withdrawn", false, refused, refused},
	} {
		t.Run(c.status, func(t *testing.T) {
			setStatus(t, b, id, c.status)
			if got := slices.Contains(apiNextIDs(t, h), id); got != c.inNext {
				t.Errorf("in next = %v, want %v", got, c.inNext)
			}
			check := func(action, outcome, to string) {
				t.Helper()
				setStatus(t, b, id, c.status)
				before := events(t)
				code, raw := apiMark(t, h, id, action, "grid")
				written := events(t) - before
				switch outcome {
				case refused, pastApplying:
					want := "already " + c.status + "; change it in the console"
					if outcome == pastApplying {
						want = "already " + c.status + "; use withdrawn"
					}
					if code != http.StatusUnprocessableEntity || apiDecoded[console.APIError](t, raw).Error != want || written != 0 {
						t.Errorf("%s: %d %s, %d written; want 422 %q and nothing written", action, code, raw, written, want)
					}
				case noop:
					if out := apiDecoded[console.APITrackOutcome](t, raw); code != http.StatusOK || !out.NoOp || out.To != c.status || written != 0 {
						t.Errorf("%s: %d %s, %d written; want a no-op", action, code, raw, written)
					}
				default:
					if out := apiDecoded[console.APITrackOutcome](t, raw); code != http.StatusOK || out.NoOp || out.From != c.status || out.To != to || written != 1 {
						t.Errorf("%s: %d %s, %d written; want %s → %s", action, code, raw, written, c.status, to)
					}
					if st, _, actor := currentStatus(t, id); st != to || actor != "gremy" {
						t.Errorf("%s: status %s by %s, want %s by gremy", action, st, actor, to)
					}
				}
			}
			check("applied", c.applied, "applied")
			check("skip", c.skip, "skipped")
			check("maybe", moves, c.status)
			if _, reason, _ := currentStatus(t, id); reason != "maybe: grid" {
				t.Errorf("maybe's reason = %q", reason)
			}
		})
	}
}

// TestAPITrackUnknownJob: through the real backend an unknown job on track is the operator's to
// fix: 422 as JSON, nothing written. On the job route it is 404.
func TestAPITrackUnknownJob(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	code, raw := apiMark(t, h, 999, "applied", "")
	if code != http.StatusUnprocessableEntity || apiDecoded[console.APIError](t, raw).Error != "job:999 does not exist" {
		t.Errorf("track: %d %s", code, raw)
	}
	if got := events(t); got != 0 {
		t.Errorf("status events = %d, want 0", got)
	}
	if code, raw := apiCall(t, h, http.MethodGet, "/api/jobs/999", ""); code != http.StatusNotFound {
		t.Errorf("job: %d %s", code, raw)
	}
}

// TestAPIColdStart: before any brief, next and the tracker are empty lists, never null.
func TestAPIColdStart(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	if code, raw := apiCall(t, h, http.MethodGet, "/api/jobs/next", ""); code != http.StatusOK || raw != `{"jobs":[]}`+"\n" {
		t.Errorf("next: %d %q", code, raw)
	}
	if code, raw := apiCall(t, h, http.MethodGet, "/api/tracked", ""); code != http.StatusOK || raw != `{"total":0,"rows":[]}`+"\n" {
		t.Errorf("tracked: %d %q", code, raw)
	}
	jobs, err := b.NextJobs(context.Background(), 14, 20)
	if err != nil || jobs == nil || len(jobs) != 0 {
		t.Errorf("NextJobs = %#v, %v; want empty and not nil", jobs, err)
	}
}

// TestAPIBareJob: a job with no observation and only its new event lists in next with its card's
// title, answers with an empty description, and takes its first mark.
func TestAPIBareJob(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	id := bareJob(t, b, "bare")
	sendBriefOn(t, today(), id)

	code, raw := apiCall(t, h, http.MethodGet, "/api/jobs/next", "")
	if code != http.StatusOK || !strings.Contains(raw, `"prior":[]`) {
		t.Errorf("next: %d %s", code, raw)
	}
	next := apiDecoded[console.APINextJobs](t, raw).Jobs
	if len(next) != 1 || next[0].JobID != id || next[0].Title != "bare card title" || next[0].Status != "new" ||
		next[0].ApplyURL != "" || next[0].Location != "" || next[0].BriefDay != today() || next[0].BriefN != 1 {
		t.Errorf("next = %+v", next)
	}

	code, raw = apiCall(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), "")
	if code != http.StatusOK {
		t.Fatalf("job: %d %s", code, raw)
	}
	for _, want := range []string{`"description":""`, `"criteria":[]`, `"unclear":[]`, `"applicant_count":null`, `"posted_at":null`} {
		if !strings.Contains(raw, want) {
			t.Errorf("job lacks %s: %s", want, raw)
		}
	}
	job := apiDecoded[console.APIJob](t, raw)
	if job.Title != "bare card title" || len(job.History) != 1 || job.History[0].Status != "new" || job.History[0].Actor != "system" {
		t.Errorf("job = %+v", job)
	}

	code, raw = apiMark(t, h, id, "applied", "first mark")
	if out := apiDecoded[console.APITrackOutcome](t, raw); code != http.StatusOK || out.From != "new" || out.To != "applied" || out.NoOp {
		t.Errorf("first mark: %d %s", code, raw)
	}
	if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM status_event WHERE job_id = %d`, id)); got != 2 {
		t.Errorf("events = %d, want the new event and the mark", got)
	}
	if got := apiNextIDs(t, h); len(got) != 0 {
		t.Errorf("next after the mark = %v, want none", got)
	}
}

// TestNextJobsBackendClock: days counts back from the backend's clock in the backend's zone, where
// the day has already turned, and the brief's day comes through with no zone conversion.
func TestNextJobsBackendClock(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	id := bareJob(t, b, "clock")
	sendBriefOn(t, "2026-03-10", id)
	// 22:30 UTC on the 24th is 00:30 on the 25th in Bucharest: fourteen days back is the 11th
	// there, the 10th in UTC.
	b.now = func() time.Time { return time.Date(2026, 3, 24, 22, 30, 0, 0, time.UTC) }
	ctx := context.Background()

	jobs, err := b.NextJobs(ctx, 14, 20)
	if err != nil || len(jobs) != 0 {
		t.Errorf("14 days: %+v, %v; want the brief just out of range", jobs, err)
	}
	jobs, err = b.NextJobs(ctx, 15, 20)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("15 days: %+v, %v; want the brief", jobs, err)
	}
	if want := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC); !jobs[0].BriefDay.Equal(want) || jobs[0].Prior == nil {
		t.Errorf("job = %+v, want BriefDay %v and Prior not nil", jobs[0], want)
	}
	h, _ := apiHandler(t, b)
	if got := apiNext(t, h, "?days=15"); len(got) != 1 || got[0].BriefDay != "2026-03-10" {
		t.Errorf("next = %+v", got)
	}
}

// apiRealDecider is a decider's name as a real scoring run writes it.
const apiRealDecider = "jev-test"

// observeText writes an observation of the job with this description and closed flag, and returns
// its id. The gate fours are gate, verdict, snippet, reason.
func observeText(t *testing.T, b *backend, id int64, name, description string, closed bool, gate ...string) int64 {
	t.Helper()
	ctx := context.Background()
	q := b.s.Q()
	runID, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.NewFetchRecorder(q, runID, "linkedin")
	if err := rec.Record(ctx, fetch.Record{Kind: fetch.KindDetail, URL: "https://jobs.example/" + name, Status: 200, Body: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	obsID, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: id, FetchID: rec.LastID(), DerivedBy: "test@1",
		Title: name, DescriptionClean: description, ApplyType: "offsite", Closed: closed})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+3 < len(gate); i += 4 {
		if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: id, ObservationID: obsID, Gate: gate[i], Verdict: gate[i+1],
			Snippet: gate[i+2], Reason: gate[i+3], DerivedBy: "gates@1"}); err != nil {
			t.Fatal(err)
		}
	}
	return obsID
}

// decideAll writes a passing decision by decider on the observation for each criterion named.
func decideAll(t *testing.T, b *backend, id, obsID int64, decider string, criteria ...string) {
	t.Helper()
	for _, c := range criteria {
		if _, err := b.s.Q().InsertDecision(context.Background(), store.DecisionInsert{JobID: id, ObservationID: obsID, Decider: decider, Criterion: c, Verdict: "pass",
			Confidence: 0.9, Probabilities: map[string]float64{"pass": 0.9}, Reason: c, InputSHA256: []byte(c)}); err != nil {
			t.Fatal(err)
		}
	}
}

// observedJob is a job with a latest observation and every criterion passing from a real decider,
// so what its gates and closed flag say is all that can keep it from auto-ok. An earlier observation
// says the opposite, because only the latest one counts.
func observedJob(t *testing.T, b *backend, name string, closed bool, gate ...string) int64 {
	t.Helper()
	id := bareJob(t, b, name)
	observeText(t, b, id, name, "We write Go.", !closed, "rule", "fail", "", "an older observation")
	decideAll(t, b, id, observeText(t, b, id, name, "We write Go.", closed, gate...), apiRealDecider, candidate.CriterionNames...)
	return id
}

// autoCheck is --auto-check's line for the job through the client and the real handler.
func autoCheck(t *testing.T, env string, id int64) string {
	t.Helper()
	exit, out, stderr := apiClientRun(t, "job", strconv.FormatInt(id, 10), "--auto-check", "--env", env)
	if exit != 0 {
		t.Fatalf("--auto-check: exit %d, stderr %q", exit, stderr)
	}
	return strings.TrimSuffix(out, "\n")
}

// TestAPIAutoCheckRefusesFake: a fake scoring run's pass rows are not a score, alone or over real
// ones, while the job's record still shows them with their decider.
func TestAPIAutoCheckRefusesFake(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	env, _ := apiClientServe(t, b, nil)

	fake := bareJob(t, b, "fake")
	decideAll(t, b, fake, observeText(t, b, fake, "fake", "We write Go.", false), ai.FakeName, candidate.CriterionNames...)
	if got, want := autoCheck(t, env, fake), "criterion title: not a real score (decider fake)"; got != want {
		t.Errorf("fake only: %q, want %q", got, want)
	}
	code, raw := apiCall(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d", fake), "")
	if code != http.StatusOK {
		t.Fatalf("job: %d %s", code, raw)
	}
	job := apiDecoded[console.APIJob](t, raw)
	if len(job.Criteria) != len(candidate.CriterionNames) || job.Criteria[0].Decider != ai.FakeName || job.Criteria[0].Verdict != "pass" {
		t.Errorf("criteria = %+v", job.Criteria)
	}

	over := observedJob(t, b, "over", false)
	if got := autoCheck(t, env, over); got != "auto-ok" {
		t.Fatalf("real: %q", got)
	}
	latest, err := b.s.Q().LatestObservation(context.Background(), over)
	if err != nil {
		t.Fatal(err)
	}
	decideAll(t, b, over, latest.ID, ai.FakeName, "location")
	if got, want := autoCheck(t, env, over), "criterion location: not a real score (decider fake)"; got != want {
		t.Errorf("a fake over real ones: %q, want %q", got, want)
	}
}

// TestAPIVerdictsCurrent: a recheck writes a new observation every time and the scorer writes no
// decision on unchanged text, so old decisions on the same text are current and stay auto-ok, the
// road a real job takes; on changed text they are not current until scored again.
func TestAPIVerdictsCurrent(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	env, _ := apiClientServe(t, b, nil)
	const stale = "verdicts: the text changed and has not been rescored"
	current := func(id int64) bool {
		t.Helper()
		code, raw := apiCall(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), "")
		if code != http.StatusOK {
			t.Fatalf("job: %d %s", code, raw)
		}
		return apiDecoded[console.APIJob](t, raw).VerdictsCurrent
	}

	id := bareJob(t, b, "scored")
	first := observeText(t, b, id, "scored", "We write Go.", false)
	decideAll(t, b, id, first, apiRealDecider, candidate.CriterionNames...)
	if got := autoCheck(t, env, id); got != "auto-ok" || !current(id) {
		t.Errorf("on the latest observation: %q, current %v", got, current(id))
	}
	observeText(t, b, id, "scored", "We write Go.", false)
	observeText(t, b, id, "scored", "We write Go.", false)
	if got := autoCheck(t, env, id); got != "auto-ok" || !current(id) {
		t.Errorf("rechecked, the same text: %q, current %v", got, current(id))
	}
	second := observeText(t, b, id, "scored", "We write Rust now.", false)
	if got := autoCheck(t, env, id); got != stale || current(id) {
		t.Errorf("changed text: %q, current %v", got, current(id))
	}
	decideAll(t, b, id, second, apiRealDecider, "title", "stack")
	if got := autoCheck(t, env, id); got != stale || current(id) {
		t.Errorf("rescored in part: %q, current %v", got, current(id))
	}
	decideAll(t, b, id, second, apiRealDecider, candidate.CriterionNames...)
	if got := autoCheck(t, env, id); got != "auto-ok" || !current(id) {
		t.Errorf("rescored: %q, current %v", got, current(id))
	}

	// An observation nothing has decided on has no verdicts to be current.
	unscored := bareJob(t, b, "unscored")
	observeText(t, b, unscored, "unscored", "We write Go.", false)
	if current(unscored) {
		t.Error("no decisions read as current")
	}

	// Past the length the scorer reads, a change is no change: it would not ask again.
	long := bareJob(t, b, "long")
	text := strings.Repeat("x", scoring.MaxDescription)
	decideAll(t, b, long, observeText(t, b, long, "long", text+" one", false), apiRealDecider, candidate.CriterionNames...)
	observeText(t, b, long, "long", text+" two", false)
	if got := autoCheck(t, env, long); got != "auto-ok" {
		t.Errorf("a change past the cut: %q", got)
	}

	// Blanked text on the observation decided on cannot be shown to match, so it does not.
	trimmed := bareJob(t, b, "trimmed")
	old := observeText(t, b, trimmed, "trimmed", "We write Go.", false)
	decideAll(t, b, trimmed, old, apiRealDecider, candidate.CriterionNames...)
	observeText(t, b, trimmed, "trimmed", "We write Go.", false)
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`UPDATE job_observation SET description_clean = '', description_trimmed_at = now() WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if got := autoCheck(t, env, trimmed); got != stale {
		t.Errorf("trimmed: %q", got)
	}
}

// TestConsoleTrackHasNoAPILimit: the console's own form still moves a job from where the API is
// refused, and the API's refusal names the status, through the client too, writing nothing.
func TestConsoleTrackHasNoAPILimit(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, cookie := apiHandler(t, b)
	env, _ := apiClientServe(t, b, nil)
	for _, c := range []struct{ from, action, to string }{
		{"interviewing", "applied", "applied"},
		{"offer", "applied", "applied"},
		{"rejected", "applied", "applied"},
		{"skipped", "applied", "applied"},
		{"withdrawn", "applied", "applied"},
		{"rejected", "skip", "skipped"},
		{"withdrawn", "skip", "skipped"},
	} {
		t.Run(c.from+" "+c.action, func(t *testing.T) {
			id := bareJob(t, b, c.from+"-"+c.action)
			setStatus(t, b, id, c.from)
			before := events(t)
			want := "already " + c.from + "; change it in the console"
			code, raw := apiMark(t, h, id, c.action, "no")
			if code != http.StatusUnprocessableEntity || apiDecoded[console.APIError](t, raw).Error != want {
				t.Errorf("api: %d %s; want 422 %q", code, raw, want)
			}
			exit, _, stderr := apiClientRun(t, "mark", strconv.FormatInt(id, 10), c.action, "--env", env)
			if exit != 1 || stderr != "leadhound api: "+want+"\n" {
				t.Errorf("client: exit %d, stderr %q", exit, stderr)
			}
			if st, _, _ := currentStatus(t, id); st != c.from || events(t) != before {
				t.Errorf("after the refusals: status %s, %d written", st, events(t)-before)
			}
			if code, body := sessionMark(t, h, cookie, id, c.action, "by hand"); code != http.StatusOK {
				t.Fatalf("console: %d %s", code, body)
			}
			if st, reason, actor := currentStatus(t, id); st != c.to || reason != "by hand" || actor != "gremy" {
				t.Errorf("console: status %s (%q by %s), want %s", st, reason, actor, c.to)
			}
		})
	}
}

// TestAPITrackLimitInsideTheLock: a mark waiting on the job's lock is judged on the status the lock's
// holder wrote, not the one it could have read first.
func TestAPITrackLimitInsideTheLock(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	id := bareJob(t, b, "locked")
	type answer struct {
		code int
		raw  string
	}
	done := make(chan answer, 1)
	err := b.s.InTx(context.Background(), func(tq store.Queries) error {
		if err := tq.LockJob(context.Background(), id); err != nil {
			return err
		}
		go func() {
			code, raw := apiMark(t, h, id, "applied", "racing")
			done <- answer{code, raw}
		}()
		time.Sleep(300 * time.Millisecond) // long enough for the mark to reach the lock
		_, err := tq.InsertStatusEvent(context.Background(), id, "interviewing", "", "gremy", nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.code != http.StatusUnprocessableEntity || !strings.Contains(got.raw, "already interviewing") {
		t.Errorf("%d %s; want 422 naming interviewing", got.code, got.raw)
	}
	if st, _, _ := currentStatus(t, id); st != "interviewing" {
		t.Errorf("status %s, want interviewing", st)
	}
}

// TestAPIJobClosedAndFailedGates: through the real backend, what the latest observation says reads
// back in /api/jobs/{id}, and the client's --auto-check refuses on it. The clean job is the control:
// the same job with neither is auto-ok. A flag stays an unclear point and is not a failed gate, and
// a failed gate never quotes what looks like a contact.
func TestAPIJobClosedAndFailedGates(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	h, _ := apiHandler(t, b)
	env, _ := apiClientServe(t, b, nil)
	for _, c := range []struct {
		name   string
		closed bool
		gates  []string
		failed []console.APIPoint
		check  string
	}{
		{"clean", false, []string{"language", "pass", "", "ok"}, []console.APIPoint{}, "auto-ok"},
		{"closed", true, nil, []console.APIPoint{}, "listing: closed"},
		{"failed", false, []string{"language", "pass", "", "ok", "work_rights", "fail", "must hold a visa", "needs a visa"},
			[]console.APIPoint{{Name: "work_rights", Snippet: "must hold a visa"}}, "failed gate: work_rights"},
		{"reason", false, []string{"language", "fail", "", "not in English"}, []console.APIPoint{{Name: "language", Snippet: "not in English"}}, "failed gate: language"},
		{"contact", false, []string{"rule", "fail", "write to hr@acme.example", "a skip rule"}, []console.APIPoint{{Name: "rule", Snippet: "a skip rule"}}, "failed gate: rule"},
		{"all contact", false, []string{"rule", "fail", "hr@acme.example", "see /in/someone"}, []console.APIPoint{{Name: "rule"}}, "failed gate: rule"},
		{"flag", false, []string{"workplace", "flag", "hybrid", "says hybrid"}, []console.APIPoint{}, "unclear: workplace"},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := observedJob(t, b, strings.ReplaceAll(c.name, " ", "-"), c.closed, c.gates...)
			code, raw := apiCall(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), "")
			if code != http.StatusOK {
				t.Fatalf("job: %d %s", code, raw)
			}
			if !strings.Contains(raw, `"failed_gates":[`) {
				t.Errorf("failed_gates is not a list: %s", raw)
			}
			job := apiDecoded[console.APIJob](t, raw)
			if job.Closed != c.closed || !slices.Equal(job.FailedGates, c.failed) {
				t.Errorf("closed %v, failed %+v; want %v, %+v", job.Closed, job.FailedGates, c.closed, c.failed)
			}
			if len(job.Criteria) != len(candidate.CriterionNames) {
				t.Errorf("criteria = %+v, want all of them", job.Criteria)
			}
			if strings.Contains(raw, "hr@acme.example") {
				t.Errorf("a contact reached the answer: %s", raw)
			}
			exit, out, stderr := apiClientRun(t, "job", strconv.FormatInt(id, 10), "--auto-check", "--env", env)
			if exit != 0 || out != c.check+"\n" {
				t.Errorf("--auto-check: exit %d, stdout %q, stderr %q; want %q", exit, out, stderr, c.check)
			}
		})
	}
	// A job with no observation has neither, as empty values, and still fails on its criteria.
	id := bareJob(t, b, "bare")
	code, raw := apiCall(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), "")
	if code != http.StatusOK || !strings.Contains(raw, `"closed":false`) || !strings.Contains(raw, `"failed_gates":[]`) {
		t.Errorf("no observation: %d %s", code, raw)
	}
	if exit, out, _ := apiClientRun(t, "job", strconv.FormatInt(id, 10), "--auto-check", "--env", env); exit != 0 || out != "no criteria on record\n" {
		t.Errorf("no observation: exit %d, stdout %q", exit, out)
	}
}

// TestServeAPITokenUppercase: a hash set in uppercase opens the API of a real serve, because it
// reaches the console lowercased, and a wrong token still does not.
func TestServeAPITokenUppercase(t *testing.T) {
	serveEnv(t)
	commandDB(t)
	addr := freeAddr(t)
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	t.Setenv("LEADHOUND_CONSOLE_ADDR", addr)
	t.Setenv("LEADHOUND_API_TOKEN_SHA256", strings.ToUpper(apiTestHash()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, []string{"--dev"}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v", err)
		}
	}()

	get := func(token string) (int, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/api/jobs/next", nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		return resp.StatusCode, resp.Body.Close()
	}
	var code int
	var err error
	for range 100 {
		if code, err = get(apiTestToken); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || code != http.StatusOK {
		t.Fatalf("the right token: %d, %v", code, err)
	}
	if code, err := get("not-the-token"); err != nil || code != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d, %v", code, err)
	}
}
