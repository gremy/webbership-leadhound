package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["api"] = runAPI }

const apiClientUsage = "usage: leadhound api next [--limit N] [--days D] [--json] [--env FILE]\n" +
	"       leadhound api job ID [--auto-check] [--no-description] [--json] [--env FILE]\n" +
	"       leadhound api tracked [--status S] [--json] [--env FILE]\n" +
	"       leadhound api mark ID applied|skip|maybe [--reason TEXT] [--env FILE]\n" +
	"       leadhound api new-token --url URL --out FILE [--force]"

const (
	// apiClientRetries is how many times a request that may repeat is sent again.
	apiClientRetries = 2
	// apiClientMaxBody is the most of an answer the client reads: well over 500 tracked rows or
	// one posting's description.
	apiClientMaxBody = 8 << 20

	apiClientPostingStart = "--- untrusted posting text ---"
	apiClientPostingEnd   = "--- end untrusted posting text ---"
)

// The clock and the executable, which the tests replace.
var (
	apiClientTimeout    = 10 * time.Second
	apiClientRetryDelay = 500 * time.Millisecond
	apiClientExecutable = os.Executable
)

// apiClientMarker matches what could pass for a posting marker, in any case or spacing, once
// invisible characters are gone. The phrase is what is taken out, not only the whole marker, so no
// dashes or spacing around it can rebuild one.
var apiClientMarker = regexp.MustCompile(`(?i)untrusted\s+posting\s+text`)

// runAPI is `leadhound api …`, the local client of the console's /api/ routes that the /apply skill
// drives. It opens no database and needs none of the server's settings: only the URL and token in
// its env file. Exit codes: 0 success, 1 a refusal or a server error with its words, 2 usage, 3
// production cannot be reached. It never prints the token or a body that is not JSON.
func runAPI(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("%s", apiClientUsage)
	}
	switch args[0] {
	case "next":
		return runAPINext(ctx, args[1:])
	case "job":
		return runAPIJob(ctx, args[1:])
	case "tracked":
		return runAPITracked(ctx, args[1:])
	case "mark":
		return runAPIMark(ctx, args[1:])
	case "new-token":
		return runAPINewToken(args[1:])
	}
	return usageError("%s", apiClientUsage)
}

// apiClientFlags is a flag set with the flag every call to the server takes.
func apiClientFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs, fs.String("env", "", "the file with LEADHOUND_API_URL and LEADHOUND_API_TOKEN (default ../data/api.env from the executable's folder)")
}

// apiClientFailed is a refusal or failure, as one plain line and exit 1.
func apiClientFailed(format string, a ...any) error {
	return exitError{code: 1, line: "leadhound api: " + apiClientLine(fmt.Sprintf(format, a...))}
}

// apiClientUnreachable is production not reached, as one plain line and exit 3.
func apiClientUnreachable(format string, a ...any) error {
	return exitError{code: 3, line: "leadhound api: " + apiClientLine(fmt.Sprintf(format, a...))}
}

// apiClientLine makes s safe to print as one field of one line, so posting text cannot forge a
// row or a field: every control character and line or paragraph separator becomes a space, an
// invisible format character goes, and runs of white space close up.
func apiClientLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), unicode.In(r, unicode.Zl, unicode.Zp):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, " "))
	return strings.Join(strings.Fields(s), " ")
}

// apiClientPostingText makes a description safe to print between the markers: its lines stay, but
// control and invisible characters go, and nothing in it can pass for a marker.
func apiClientPostingText(s string) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n").Replace(strings.ToValidUTF8(s, " "))
	s = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return -1
		}
		return r
	}, s)
	return strings.TrimRight(apiClientMarker.ReplaceAllString(s, "[marker removed]"), " \t\n")
}

// apiClientCheckURL checks a console URL and returns it with no trailing slash: https, or http
// only to this machine, so the token never crosses a network in the clear. Credentials, a query or
// a fragment in it are refused. A refusal never repeats the value.
func apiClientCheckURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || strings.ContainsAny(raw, " \t\r\n\"'") || u.Hostname() == "":
		return "", usageError("the console URL is not a URL with a host")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", usageError("the console URL must have no credentials, query or fragment")
	case u.Scheme == "https":
	case u.Scheme == "http" && apiClientLoopback(u.Hostname()):
	default:
		return "", usageError("the console URL must be https, or http only on loopback")
	}
	return strings.TrimRight(raw, "/"), nil
}

func apiClientLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// apiClientDefaultEnv is ../data/api.env from the executable's folder, because the skill runs from
// another folder and a relative path would miss.
func apiClientDefaultEnv() (string, error) {
	exe, err := apiClientExecutable()
	if err != nil {
		return "", usageError("cannot find the executable's folder; use --env")
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), "..", "data", "api.env"), nil
}

// apiClientReadEnv reads the two settings from the file at path. Nothing of the file's content
// ever reaches an error.
func apiClientReadEnv(path string) (baseURL, token string, err error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own env file
	if err != nil {
		return "", "", usageError("cannot read %s; make api-token writes it, or name it with --env", path)
	}
	for line := range strings.Lines(string(b)) {
		key, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		switch strings.TrimSpace(key) {
		case "LEADHOUND_API_URL":
			baseURL = value
		case "LEADHOUND_API_TOKEN":
			token = value
		}
	}
	if baseURL == "" || token == "" {
		return "", "", usageError("%s must set LEADHOUND_API_URL and LEADHOUND_API_TOKEN", path)
	}
	if strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r >= 0x7f }) >= 0 {
		return "", "", usageError("the LEADHOUND_API_TOKEN in %s is malformed", path)
	}
	return baseURL, token, nil
}

// apiClient calls one console. It carries the token only as its bearer header.
type apiClient struct {
	base, token string
	http        *http.Client
}

// apiClientOpen reads the env file and builds the client. A redirect is never followed, so the
// token can never go anywhere else; each request opens its own connection, so the transport can
// never repeat one on its own and every repeat is the policy's.
func apiClientOpen(envPath string) (*apiClient, error) {
	if envPath == "" {
		var err error
		if envPath, err = apiClientDefaultEnv(); err != nil {
			return nil, err
		}
	}
	raw, token, err := apiClientReadEnv(envPath)
	if err != nil {
		return nil, err
	}
	base, err := apiClientCheckURL(raw)
	if err != nil {
		return nil, err
	}
	return &apiClient{base: base, token: token, http: &http.Client{
		Timeout:       apiClientTimeout,
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// apiClientPolicy is when a request is sent again.
type apiClientPolicy int

const (
	apiClientNoRetry       apiClientPolicy = iota // never: a repeat would write again
	apiClientRetryNoAnswer                        // only when no answer came: the server's no-op makes a repeat safe
	apiClientRetryRead                            // also on a lost body or a 5xx: a read has nothing to repeat
)

var (
	// errAPIClientNoAnswer is a connection failure before any response.
	errAPIClientNoAnswer = errors.New("no answer")
	// errAPIClientLostBody is a response whose body broke off.
	errAPIClientLostBody = errors.New("the answer broke off")
	// errAPIClientTooBig is an answer over apiClientMaxBody.
	errAPIClientTooBig = errors.New("the answer is too big")
)

// once sends one request and returns the status and the capped body.
func (c *apiClient) once(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", errAPIClientNoAnswer, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the operation and the URL are not news
		}
		return 0, nil, fmt.Errorf("%w: %w", errAPIClientNoAnswer, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, apiClientMaxBody+1))
	switch {
	case err != nil:
		return resp.StatusCode, nil, fmt.Errorf("%w: %w", errAPIClientLostBody, err)
	case len(raw) > apiClientMaxBody:
		return resp.StatusCode, nil, errAPIClientTooBig
	}
	return resp.StatusCode, raw, nil
}

// repeats reports whether a result may be sent again under the policy.
func (p apiClientPolicy) repeats(status int, err error) bool {
	switch p {
	case apiClientRetryNoAnswer:
		return errors.Is(err, errAPIClientNoAnswer)
	case apiClientRetryRead:
		return errors.Is(err, errAPIClientNoAnswer) || errors.Is(err, errAPIClientLostBody) || (err == nil && status >= http.StatusInternalServerError)
	}
	return false
}

// call sends a request under the policy and returns the body of a 200 that holds JSON. Anything
// else is an exitError with its line.
func (c *apiClient) call(ctx context.Context, method, path string, body []byte, policy apiClientPolicy) ([]byte, error) {
	var status int
	var raw []byte
	var err error
	for attempt := 0; ; attempt++ {
		status, raw, err = c.once(ctx, method, path, body)
		if attempt == apiClientRetries || ctx.Err() != nil || !policy.repeats(status, err) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(apiClientRetryDelay * time.Duration(attempt+1)):
		}
	}
	host := apiClientHost(c.base)
	switch {
	case ctx.Err() != nil:
		return nil, apiClientFailed("interrupted")
	case errors.Is(err, errAPIClientTooBig):
		return nil, apiClientFailed("%s answered with more than the client reads", host)
	case err != nil:
		return nil, apiClientUnreachable("cannot reach %s: %v", host, err)
	case status >= 300 && status < 400:
		return nil, apiClientFailed("%s answered with a redirect, which the client never follows; check LEADHOUND_API_URL", host)
	case status != http.StatusOK:
		return nil, apiClientRefusal(host, status, raw)
	case !json.Valid(raw):
		return nil, apiClientFailed("%s answered %d with a body that is not JSON", host, status)
	}
	return raw, nil
}

// apiClientRefusal is an answer other than 200: the server's words when it sent any, or a plain
// line that never quotes the body. A gateway's error page means production is not there to answer.
func apiClientRefusal(host string, status int, raw []byte) error {
	var e console.APIError
	if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
		return apiClientFailed("%s", e.Error)
	}
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return apiClientUnreachable("cannot reach the console: %s answered %d with a body that is not JSON", host, status)
	}
	return apiClientFailed("%s answered %d with a body that is not JSON", host, status)
}

// apiClientDecode decodes a checked answer into the contract's own struct.
func apiClientDecode[T any](raw []byte) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, apiClientFailed("the answer is not the shape this client knows; is the server the same version?")
	}
	return v, nil
}

// apiClientHost is the host of a link, or "" when it has none.
func apiClientHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return apiClientLine(u.Hostname())
}

// apiClientJobID reads a job's id, as ID or as the job:ID next prints.
func apiClientJobID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "job:"), 10, 64)
	if err != nil || id <= 0 {
		return 0, usageError("%q is not a job id; use ID or job:ID", s)
	}
	return id, nil
}

// apiClientFlagGiven reports whether the flag name was on the command line.
func apiClientFlagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// runAPINext is `api next`: one tab-separated line per job to apply to next.
func runAPINext(ctx context.Context, args []string) error {
	fs, env := apiClientFlags("api next")
	limit := fs.Int("limit", 0, "at most this many jobs (the server's default and maximum apply)")
	days := fs.Int("days", 0, "from the briefs of this many days (the server's default and maximum apply)")
	asJSON := fs.Bool("json", false, "print the server's JSON answer unchanged")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", apiClientUsage)
	}
	q := url.Values{}
	for name, v := range map[string]int{"limit": *limit, "days": *days} {
		if !apiClientFlagGiven(fs, name) {
			continue
		}
		if v <= 0 {
			return usageError("--%s must be a positive whole number", name)
		}
		q.Set(name, strconv.Itoa(v))
	}
	path := "/api/jobs/next"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	c, err := apiClientOpen(*env)
	if err != nil {
		return err
	}
	raw, err := c.call(ctx, http.MethodGet, path, nil, apiClientRetryRead)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := stdout.Write(raw)
		return err
	}
	next, err := apiClientDecode[console.APINextJobs](raw)
	if err != nil {
		return err
	}
	if len(next.Jobs) == 0 {
		_, _ = fmt.Fprintln(stdout, "no jobs")
		return nil
	}
	for _, j := range next.Jobs {
		_, _ = fmt.Fprintln(stdout, apiClientNextRow(j))
	}
	return nil
}

// apiClientNextRow is one job of `api next`. The link is the apply link, or the posting when the
// job has none of its own. A job with earlier jobs at its company ends with every status they
// stand at, as prior:applied,rejected, so none is hidden.
func apiClientNextRow(j console.APINextJob) string {
	link := j.ApplyURL
	if link == "" {
		link = j.URL
	}
	row := []string{fmt.Sprintf("job:%d", j.JobID), apiClientLine(j.Company), apiClientLine(j.Title), apiClientHost(link), apiClientLine(j.ApplyType), apiClientLine(j.BriefDay)}
	var prior []string
	for _, p := range j.Prior {
		if s := apiClientLine(p.Status); !slices.Contains(prior, s) {
			prior = append(prior, s)
		}
	}
	if len(prior) > 0 {
		row = append(row, "prior:"+strings.Join(prior, ","))
	}
	return strings.Join(row, "\t")
}

// runAPIJob is `api job ID`: the record in short, the --auto-check line, or the raw JSON.
func runAPIJob(ctx context.Context, args []string) error {
	fs, env := apiClientFlags("api job")
	autoCheck := fs.Bool("auto-check", false, "print auto-ok, or the first reason this job may not go alone")
	noDescription := fs.Bool("no-description", false, "leave the posting's text out")
	asJSON := fs.Bool("json", false, "print the server's JSON answer unchanged")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*autoCheck && (*asJSON || *noDescription)) || (*asJSON && *noDescription) {
		return usageError("%s", apiClientUsage)
	}
	id, err := apiClientJobID(pos[0])
	if err != nil {
		return err
	}
	c, err := apiClientOpen(*env)
	if err != nil {
		return err
	}
	raw, err := c.call(ctx, http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), nil, apiClientRetryRead)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := stdout.Write(raw)
		return err
	}
	j, err := apiClientDecode[console.APIJob](raw)
	if err != nil {
		return err
	}
	if *autoCheck {
		_, _ = fmt.Fprintln(stdout, apiClientAutoCheck(j))
		return nil
	}
	apiClientPrintJob(j, !*noDescription)
	return nil
}

// apiClientAutoCheck is the deterministic half of the gate's score rule: "auto-ok" only when the
// latest observation is not closed and failed no gate, and the record shows every criterion of the
// scorer passing, nothing unclear, a status still to act on and an open listing; else the first
// reason not. Only the known passing verdict passes, so an unknown one never reads as auto-ok, and
// a criterion with no verdict has not been shown to pass. A record with no observation has no
// criteria, so it stops there.
func apiClientAutoCheck(j console.APIJob) string {
	if j.Closed {
		return "listing: closed"
	}
	if len(j.FailedGates) > 0 {
		return apiClientLine("failed gate: " + j.FailedGates[0].Name)
	}
	if len(j.Criteria) == 0 {
		return "no criteria on record"
	}
	for _, c := range j.Criteria {
		if c.Verdict != scoring.Pass {
			return apiClientLine("criterion " + c.Name + ": " + cmpOr(c.Verdict, "no verdict"))
		}
	}
	for _, name := range candidate.CriterionNames {
		if !slices.ContainsFunc(j.Criteria, func(c console.APICriterion) bool { return c.Name == name }) {
			return "criterion " + name + ": no verdict"
		}
	}
	if len(j.Unclear) > 0 {
		return apiClientLine("unclear: " + j.Unclear[0].Name)
	}
	if j.Status != "new" && j.Status != "shortlisted" {
		return apiClientLine("status: " + cmpOr(j.Status, "unknown"))
	}
	if j.ListingStatus != store.ListingOpen {
		return apiClientLine("listing: " + cmpOr(j.ListingStatus, "unknown"))
	}
	return "auto-ok"
}

// apiClientPrintJob prints a short header, one line per criterion, the unclear points and, between
// the fixed markers, the posting's text. Everything but the markers came from a posting or a score
// and is cleaned to its line.
func apiClientPrintJob(j console.APIJob, description bool) {
	_, _ = fmt.Fprintf(stdout, "job:%d\t%s\t%s\n", j.JobID, apiClientLine(j.Company), apiClientLine(j.Title))
	head := []string{"status:" + apiClientLine(j.Status), "listing:" + apiClientLine(j.ListingStatus), "apply_type:" + apiClientLine(j.ApplyType)}
	if j.ApplicantCount != nil {
		head = append(head, fmt.Sprintf("applicants:%d", *j.ApplicantCount))
	}
	if j.PostedAt != nil {
		head = append(head, "posted:"+j.PostedAt.Format(time.DateOnly))
	}
	if j.Closed {
		head = append(head, "closed:true")
	}
	_, _ = fmt.Fprintln(stdout, strings.Join(head, "\t"))
	for _, l := range [][2]string{{"location", j.Location}, {"url", j.URL}, {"apply", j.ApplyURL}} {
		if v := apiClientLine(l[1]); v != "" {
			_, _ = fmt.Fprintf(stdout, "%s:%s\n", l[0], v)
		}
	}
	for _, g := range j.FailedGates {
		_, _ = fmt.Fprintf(stdout, "failed_gate:%s\t%s\n", apiClientLine(g.Name), apiClientLine(g.Snippet))
	}
	for _, c := range j.Criteria {
		_, _ = fmt.Fprintf(stdout, "criterion:%s\t%s\t%s\n", apiClientLine(c.Name), apiClientLine(c.Verdict), apiClientLine(c.Keywords))
	}
	for _, u := range j.Unclear {
		_, _ = fmt.Fprintf(stdout, "unclear:%s\t%s\n", apiClientLine(u.Name), apiClientLine(u.Snippet))
	}
	if !description {
		return
	}
	_, _ = fmt.Fprintln(stdout, apiClientPostingStart)
	if text := apiClientPostingText(j.Description); text != "" {
		_, _ = fmt.Fprintln(stdout, text)
	}
	_, _ = fmt.Fprintln(stdout, apiClientPostingEnd)
}

// runAPITracked is `api tracked`: the same lines as `track list`, with times in this machine's
// zone. A list the server capped says so last.
func runAPITracked(ctx context.Context, args []string) error {
	fs, env := apiClientFlags("api tracked")
	status := fs.String("status", "", "only this status (the server's default applies)")
	asJSON := fs.Bool("json", false, "print the server's JSON answer unchanged")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", apiClientUsage)
	}
	path := "/api/tracked"
	if *status != "" {
		path += "?" + url.Values{"status": {*status}}.Encode()
	}
	c, err := apiClientOpen(*env)
	if err != nil {
		return err
	}
	raw, err := c.call(ctx, http.MethodGet, path, nil, apiClientRetryRead)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := stdout.Write(raw)
		return err
	}
	tr, err := apiClientDecode[console.APITracked](raw)
	if err != nil {
		return err
	}
	if len(tr.Rows) == 0 {
		_, _ = fmt.Fprintln(stdout, "nothing tracked")
		return nil
	}
	for _, r := range tr.Rows {
		line := fmt.Sprintf("job:%d\t%s · %s\t%s\tsince %s", r.JobID, apiClientLine(r.Title), apiClientLine(r.Company), apiClientLine(r.Status), r.Since.Local().Format("2006-01-02 15:04"))
		if reason := apiClientLine(r.Reason); reason != "" {
			line += "\t" + reason
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	if tr.Total > len(tr.Rows) {
		_, _ = fmt.Fprintf(stdout, "showing %d of %d\n", len(tr.Rows), tr.Total)
	}
	return nil
}

// runAPIMark is `api mark ID applied|skip|maybe`: the server's one outcome line. applied and skip
// are repeated when no answer came, because a repeat is the server's no-op; maybe writes each time,
// so it is never repeated.
func runAPIMark(ctx context.Context, args []string) error {
	fs, env := apiClientFlags("api mark")
	reason := fs.String("reason", "", "why (stored with the status)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || !slices.Contains([]string{"applied", "skip", "maybe"}, pos[1]) {
		return usageError("%s", apiClientUsage)
	}
	id, err := apiClientJobID(pos[0])
	if err != nil {
		return err
	}
	action := pos[1]
	c, err := apiClientOpen(*env)
	if err != nil {
		return err
	}
	body, err := json.Marshal(console.APITrackRequest{Action: action, Reason: *reason})
	if err != nil {
		return err
	}
	policy := apiClientRetryNoAnswer
	if action == "maybe" {
		policy = apiClientNoRetry
	}
	raw, err := c.call(ctx, http.MethodPost, fmt.Sprintf("/api/jobs/%d/track", id), body, policy)
	var ee exitError
	if errors.As(err, &ee) && ee.code == 3 {
		ee.line += fmt.Sprintf("; the mark may or may not have landed, so read it back with api job %d", id)
		return ee
	}
	if err != nil {
		return err
	}
	out, err := apiClientDecode[console.APITrackOutcome](raw)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, apiClientLine(cmpOr(out.Message, out.From+" → "+out.To)))
	return nil
}

// runAPINewToken is `api new-token --url URL --out FILE [--force]`: it makes the token, writes it
// and the URL to FILE and prints only the hash line for the server's env file. The token itself
// never reaches stdout, stderr or a command line.
func runAPINewToken(args []string) error {
	fs := flag.NewFlagSet("api new-token", flag.ContinueOnError)
	rawURL := fs.String("url", "", "the console's URL")
	out := fs.String("out", "", "the file to write")
	force := fs.Bool("force", false, "replace the file if it exists")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *rawURL == "" || *out == "" {
		return usageError("%s", apiClientUsage)
	}
	base, err := apiClientCheckURL(*rawURL)
	if err != nil {
		return err
	}
	if !*force {
		_, err := os.Lstat(*out)
		if err == nil {
			return usageError("%s already exists; --force replaces it, and the old token stops working once the server has the new hash", *out)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return apiClientFailed("cannot check %s: %v", *out, err)
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return apiClientFailed("no random bytes: %v", err)
	}
	token := hex.EncodeToString(secret)
	if err := apiClientWriteSecret(*out, "LEADHOUND_API_URL="+base+"\nLEADHOUND_API_TOKEN="+token+"\n"); err != nil {
		return apiClientFailed("cannot write %s: %v", *out, err)
	}
	sum := sha256.Sum256([]byte(token))
	_, err = fmt.Fprintf(stdout, "LEADHOUND_API_TOKEN_SHA256=%s\n", hex.EncodeToString(sum[:]))
	return err
}

// apiClientWriteSecret writes content to path through a temp file beside it, so the file is never
// half there and is at mode 0600 from its first byte. Replacing a link replaces the link, so the
// token never lands where one points. Mode bits are advisory on Windows, so failing to set them
// does not fail there.
func apiClientWriteSecret(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = f.Chmod(0o600)
	if runtime.GOOS == "windows" {
		err = nil
	}
	if err == nil {
		_, err = f.WriteString(content)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
