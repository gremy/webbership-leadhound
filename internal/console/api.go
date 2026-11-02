package console

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// The next route's numbers: over the maximum is clamped, anything not a positive whole
	// number is refused.
	apiLimitDefault, apiLimitMax = 20, 50
	apiDaysDefault, apiDaysMax   = 14, 60

	// apiTrackedDefault is the status the tracked route lists when it is given none.
	apiTrackedDefault = "applied"

	apiNotFound = "Not found."
	apiRefused  = "A valid bearer token is required."
)

// apiActions are the marks the API makes: the only three the /apply skill uses.
var apiActions = []string{"applied", "skip", "maybe"}

// apiAllowedFrom is where each mark may start from, so a mark through the API can never move a
// job back: applied and skip also from where they already stand, which is the server's no-op.
// maybe is absent because it only notes: it writes the reason and the status stays.
var apiAllowedFrom = map[string]string{
	"applied": "new shortlisted applied",
	"skip":    "new shortlisted skipped",
}

// apiRoute is one entry of the API's routing table. The tests sweep it.
type apiRoute struct {
	pattern string // ServeMux pattern
	sample  string // a concrete path that reaches the route
	handle  func(*server, http.ResponseWriter, *http.Request)
}

// apiRoutes is every route of the API, kept apart from routes so none can ever be public, wrapped
// in a session, or become somewhere a sign-in returns to. The API exposes jobs and nothing else.
var apiRoutes = []apiRoute{
	{"GET /api/jobs/next", "/api/jobs/next", (*server).apiNextJobs},
	{"GET /api/jobs/{id}", "/api/jobs/42", (*server).apiJob},
	{"GET /api/tracked", "/api/tracked", (*server).apiTracked},
	{"POST /api/jobs/{id}/track", "/api/jobs/42/track", (*server).apiTrack},
}

// APIError is every refusal and failure the API answers.
type APIError struct {
	Error string `json:"error"`
}

// APINextJobs answers GET /api/jobs/next.
type APINextJobs struct {
	Jobs []APINextJob `json:"jobs"`
}

// APINextJob is one job of APINextJobs. BriefDay is YYYY-MM-DD.
type APINextJob struct {
	JobID         int64         `json:"job_id"`
	Title         string        `json:"title"`
	Company       string        `json:"company"`
	Location      string        `json:"location"`
	Status        string        `json:"status"`
	ListingStatus string        `json:"listing_status"`
	URL           string        `json:"url"`
	ApplyURL      string        `json:"apply_url"`
	ApplyType     string        `json:"apply_type"`
	BriefDay      string        `json:"brief_day"`
	BriefN        int           `json:"brief_n"`
	Prior         []APIPriorJob `json:"prior"`
}

// APIPriorJob is another job at an APINextJob's company, already acted on.
type APIPriorJob struct {
	JobID  int64     `json:"job_id"`
	Title  string    `json:"title"`
	Status string    `json:"status"`
	Since  time.Time `json:"since"`
}

// APIJob answers GET /api/jobs/{id}: one job's whole record. ApplicantCount and PostedAt are
// null when unknown. Closed and FailedGates are what the latest observation says, which
// ListingStatus and Unclear do not show. VerdictsCurrent is false when there are no verdicts, or
// any was made on text other than the latest observation's and has not been made again.
type APIJob struct {
	JobID          int64           `json:"job_id"`
	Title          string          `json:"title"`
	Company        string          `json:"company"`
	Location       string          `json:"location"`
	URL            string          `json:"url"`
	ApplyURL       string          `json:"apply_url"`
	ApplicantCount *int            `json:"applicant_count"`
	PostedAt       *time.Time      `json:"posted_at"`
	ApplyType      string          `json:"apply_type"`
	ListingStatus  string          `json:"listing_status"`
	Status         string          `json:"status"`
	Description    string          `json:"description"`
	Criteria       []APICriterion  `json:"criteria"`
	Unclear        []APIPoint      `json:"unclear"`
	History        []APIStatusLine `json:"history"`
	Closed         bool            `json:"closed"`
	FailedGates    []APIPoint      `json:"failed_gates"`

	VerdictsCurrent bool `json:"verdicts_current"`
}

// APICriterion is one criterion's verdict, the keywords it matched and the lines that back it.
type APICriterion struct {
	Name     string   `json:"name"`
	Verdict  string   `json:"verdict"`
	Decider  string   `json:"decider"`
	Keywords string   `json:"keywords"`
	Evidence []string `json:"evidence"`
}

// APIPoint is one flagged or failed gate or unclear criterion.
type APIPoint struct {
	Name    string `json:"name"`
	Snippet string `json:"snippet"`
}

// APIStatusLine is one entry of a job's status history, newest first.
type APIStatusLine struct {
	Status string    `json:"status"`
	Reason string    `json:"reason"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

// APITracked answers GET /api/tracked. Total is how many jobs match, more than len(Rows) when
// the list is capped.
type APITracked struct {
	Total int             `json:"total"`
	Rows  []APITrackedRow `json:"rows"`
}

// APITrackedRow is one job of APITracked.
type APITrackedRow struct {
	JobID   int64     `json:"job_id"`
	Title   string    `json:"title"`
	Company string    `json:"company"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
}

// APITrackRequest is the body of POST /api/jobs/{id}/track. It has no rule fields: adding a skip
// rule stays a console action.
type APITrackRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// APITrackOutcome answers POST /api/jobs/{id}/track.
type APITrackOutcome struct {
	From    string `json:"from"`
	To      string `json:"to"`
	NoOp    bool   `json:"no_op"`
	Noted   bool   `json:"noted"`
	Message string `json:"message"`
}

// isAPIPath reports whether p belongs to the API, whatever follows the prefix: such a path never
// reaches the console's mux, so it can never meet requireSession.
func isAPIPath(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }

// api is the handler for every API path, in the order a request has to pass: on at all, then the
// token, then the same cross-origin check as the console, answered as JSON.
func (s *server) api(cop *http.CrossOriginProtection) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range apiRoutes {
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { rt.handle(s, w, r) })
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apiNoRoute(mux, w, r) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case s.opt.APITokenSHA256 == "":
			writeAPIError(w, http.StatusNotFound, apiNotFound)
		case !s.apiTokenOK(r):
			s.log.Info("console api refused", "reason", "token")
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeAPIError(w, http.StatusUnauthorized, apiRefused)
		case cop.Check(r) != nil:
			s.log.Info("console api refused", "reason", "cross-origin")
			writeAPIError(w, http.StatusForbidden, "Cross-origin requests are refused.")
		case path.Clean(r.URL.Path) != strings.TrimSuffix(r.URL.Path, "/"):
			// The mux would redirect this to its clean form, and a dot segment can name a path
			// outside the API. The token opens nothing there.
			writeAPIError(w, http.StatusNotFound, apiNotFound)
		default:
			mux.ServeHTTP(w, r)
		}
	})
}

// apiTokenOK reports whether the request carries the token as a bearer header. A query string or
// a session cookie never counts.
func (s *server) apiTokenOK(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(s.opt.APITokenSHA256)) == 1
}

// apiNoRoute answers what no API route matched: 405 when the path has a route under another
// method, else 404, both as JSON where the mux's own answers are plain text.
func apiNoRoute(mux *http.ServeMux, w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := mux.Handler(probe); pattern != "/" {
			allow = append(allow, method)
		}
	}
	if len(allow) == 0 {
		writeAPIError(w, http.StatusNotFound, apiNotFound)
		return
	}
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, "That method is not allowed here.")
}

// writeAPIError is the API's one error answer. Nothing under /api/ answers any other way:
// s.fail, http.NotFound and parseForm all answer in plain text.
func writeAPIError(w http.ResponseWriter, code int, msg string) {
	writeAPIJSON(w, code, APIError{Error: msg})
}

// writeAPIJSON writes v. HTML is left unescaped so a link reads the same in the raw answer as
// decoded; the answer is never rendered as HTML, as its type and nosniff say.
func writeAPIJSON(w http.ResponseWriter, code int, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		code = http.StatusInternalServerError
		b.Reset()
		b.WriteString(`{"error":"` + serverFailed + `"}` + "\n")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b.Bytes())
}

// apiFail logs err under the route's pattern and answers 500 with no detail.
func (s *server) apiFail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("console api error", "route", r.Pattern, "err", err.Error())
	writeAPIError(w, http.StatusInternalServerError, serverFailed)
}

// apiNumber reads a query number: def when absent, clamped to limit, false when it is not a
// positive whole number. A number too big for an int is still just over the maximum.
func apiNumber(r *http.Request, name string, def, limit int) (int, bool) {
	q := r.URL.Query()
	if !q.Has(name) {
		return def, true
	}
	n, err := strconv.Atoi(q.Get(name))
	if errors.Is(err, strconv.ErrRange) && n > 0 {
		return limit, true
	}
	if err != nil || n <= 0 {
		return 0, false
	}
	return min(n, limit), true
}

// apiList converts in, never to a nil slice: an empty list encodes as [], not null.
func apiList[T, U any](in []T, f func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}

// apiNextJobs is GET /api/jobs/next?limit=N&days=D: the jobs to apply to next.
func (s *server) apiNextJobs(w http.ResponseWriter, r *http.Request) {
	limit, ok := apiNumber(r, "limit", apiLimitDefault, apiLimitMax)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "limit must be a positive whole number.")
		return
	}
	days, ok := apiNumber(r, "days", apiDaysDefault, apiDaysMax)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "days must be a positive whole number.")
		return
	}
	jobs, err := s.b.NextJobs(r.Context(), days, limit)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, APINextJobs{Jobs: apiList(jobs, apiNextJobOf)})
}

func apiNextJobOf(j NextJob) APINextJob {
	day := ""
	if !j.BriefDay.IsZero() {
		day = j.BriefDay.Format(time.DateOnly)
	}
	return APINextJob{JobID: j.JobID, Title: j.Title, Company: j.Company, Location: j.Location, Status: j.Status,
		ListingStatus: j.ListingStatus, URL: j.URL, ApplyURL: j.ApplyURL, ApplyType: j.ApplyType, BriefDay: day, BriefN: j.BriefN,
		Prior: apiList(j.Prior, func(p PriorJob) APIPriorJob { return APIPriorJob(p) })}
}

// apiJob is GET /api/jobs/{id}: one job's whole record.
func (s *server) apiJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeAPIError(w, http.StatusNotFound, apiNotFound)
		return
	}
	j, err := s.b.Job(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "There is no job "+strconv.FormatInt(id, 10)+".")
		return
	}
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, apiJobOf(j))
}

func apiJobOf(j JobPage) APIJob {
	return APIJob{JobID: j.JobID, Title: j.Title, Company: j.Company, Location: j.Location, URL: j.URL, ApplyURL: j.ApplyURL,
		ApplicantCount: j.ApplicantCount, PostedAt: j.PostedAt, ApplyType: j.ApplyType, ListingStatus: j.ListingStatus,
		Status: j.Status, Description: j.Description, Closed: j.Closed, VerdictsCurrent: j.VerdictsCurrent,
		Criteria: apiList(j.Criteria, func(c JobCriterion) APICriterion {
			return APICriterion{Name: c.Name, Verdict: c.Verdict, Decider: c.Decider, Keywords: c.Keywords, Evidence: apiList(c.Evidence, func(e string) string { return e })}
		}),
		Unclear:     apiList(j.Unclear, apiPointOf),
		FailedGates: apiList(j.FailedGates, apiPointOf),
		History:     apiList(j.History, func(l StatusLine) APIStatusLine { return APIStatusLine(l) })}
}

func apiPointOf(p JobPoint) APIPoint { return APIPoint(p) }

// apiTracked is GET /api/tracked?status=S: the jobs at one status. S is one of the Tracked page's
// filters that names a status, which a test pins to store.JobStatuses; "all" and the page's
// default are out, because Tracked caps at 500 and "all" matches nearly every job. Tracked's own
// refusal of a status is the same bad request.
func (s *server) apiTracked(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = apiTrackedDefault
	}
	if status == "all" || trackedStatus(status) != status {
		writeAPIError(w, http.StatusBadRequest, "status must be one job status, not all.")
		return
	}
	l, err := s.b.Tracked(r.Context(), status)
	if errors.Is(err, ErrUserFixable) {
		writeAPIError(w, http.StatusBadRequest, refusalWords(err))
		return
	}
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, APITracked{Total: l.Total, Rows: apiList(l.Rows, func(t Tracked) APITrackedRow { return APITrackedRow(t) })})
}

// apiTrack is POST /api/jobs/{id}/track: the same mark as trackJob, through the same Track, so it
// inherits every refusal and the no-op, limited further by apiAllowedFrom. The reason is never
// logged.
func (s *server) apiTrack(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeAPIError(w, http.StatusNotFound, apiNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeAPIError(w, http.StatusRequestEntityTooLarge, tooLarge)
		return
	}
	req, ok := apiTrackRequestOf(body)
	if err != nil || !ok {
		writeAPIError(w, http.StatusBadRequest, "The body must be one JSON object with only action and reason.")
		return
	}
	if !slices.Contains(apiActions, req.Action) {
		writeAPIError(w, http.StatusBadRequest, "action must be one of "+strings.Join(apiActions, ", ")+".")
		return
	}
	out, err := s.b.Track(r.Context(), TrackRequest{Ref: "job:" + strconv.FormatInt(id, 10), Action: req.Action, Reason: req.Reason, AllowedFrom: apiAllowedFrom[req.Action]})
	switch {
	case errors.Is(err, ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "There is no job "+strconv.FormatInt(id, 10)+".")
	case errors.Is(err, ErrUserFixable):
		s.log.Info("console api track refused", "job", id, "action", req.Action)
		writeAPIError(w, http.StatusUnprocessableEntity, refusalWords(err))
	case err != nil:
		s.apiFail(w, r, err)
	default:
		s.log.Info("console api track", "job", id, "from", out.From, "to", out.To)
		writeAPIJSON(w, http.StatusOK, APITrackOutcome{From: out.From, To: out.To, NoOp: out.NoOp, Noted: out.Noted, Message: out.Message})
	}
}

// apiTrackRequestOf reads body: one JSON object of known fields and nothing after it.
func apiTrackRequestOf(body []byte) (APITrackRequest, bool) {
	var req APITrackRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return APITrackRequest{}, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return APITrackRequest{}, false
	}
	return req, true
}
