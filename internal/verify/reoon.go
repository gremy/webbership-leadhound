// Package verify is the email check before sending: a client for Reoon's email verifier and
// the one function that decides, from an address's stored results, whether it may enter a
// live batch.
//
// Reoon (API docs read 29 Oct 2026, https://www.reoon.com/articles/api-documentation-of-reoon-email-verifier/):
// a single check is GET /api/v1/verify?email=…&key=…&mode=power, which takes from a few seconds
// to over a minute (at most 5 parallel requests on that endpoint); the balance is
// GET /api/v1/check-account-balance/?key=…, answering api_status, remaining_daily_credits,
// remaining_instant_credits and status. Credits are refunded for an unknown result.
//
// What each status means, as Reoon's own article puts it ("The Meaning of Different Email
// Verification Statuses", read 30 Oct 2026,
// https://www.reoon.com/articles/meaning-of-different-email-verification-statuses/), and what
// LeadHound does with it:
//
//   - safe: the mailbox confirmed it can receive mail, with no risk flags; "your sending
//     list". Sent.
//   - role_account: a valid shared address (info@, support@); Reoon says it is often worth
//     emailing for B2B outreach to small businesses, since the owner may read it, and that
//     excluding it is a use-case decision, not a validity problem. LeadHound writes to role
//     addresses like contact@ on purpose: sent.
//   - catch_all: the domain accepts mail for any name, so the mailbox cannot be confirmed;
//     "neither safe nor unsafe", some real and read, some bounce after acceptance; Reoon
//     advises small batches and watching bounces. Common on small-business domains, and
//     LeadHound's batches are small and its sync pauses a mailbox on bounces: sent by default,
//     blocked when the setting verify.catch_all is block.
//   - invalid: no mailbox exists, sending hard-bounces; delete and never re-add. Blocked.
//   - disabled: the account existed but the provider shut it down; same as invalid. Blocked.
//   - disposable: a short-lived throwaway address; delete. Blocked.
//   - inbox_full: a real mailbox over its storage limit, refusing mail for now; Reoon says
//     re-verify before the next campaign. Blocked (a result older than FreshFor counts as
//     unchecked, so it is checked again later).
//   - spamtrap: an address that exists to catch senders with poor list hygiene; delete
//     immediately. Blocked.
//   - unknown: the server gave no definitive answer; Reoon says re-verify after a few days and
//     treat addresses that stay unknown like catch-alls or leave them out. LeadHound checks it
//     again the next day and blocks it after the second unknown in a row.
//
// A response that is not a check (an HTTP error, a status LeadHound does not know, an error
// body) is an error, never a status: nothing is stored and the address stays unchecked.
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// Provider is the email_check.provider of a Reoon result.
const Provider = "reoon"

// The statuses Reoon's power mode answers.
const (
	StatusSafe       = "safe"
	StatusInvalid    = "invalid"
	StatusDisabled   = "disabled"
	StatusDisposable = "disposable"
	StatusInboxFull  = "inbox_full"
	StatusCatchAll   = "catch_all"
	StatusRole       = "role_account"
	StatusSpamtrap   = "spamtrap"
	StatusUnknown    = "unknown"
)

// Statuses are every status a check may store.
var Statuses = []string{StatusSafe, StatusInvalid, StatusDisabled, StatusDisposable, StatusInboxFull,
	StatusCatchAll, StatusRole, StatusSpamtrap, StatusUnknown}

// The setting verify.catch_all's values (leadconfig.Verify).
const (
	CatchAllSend  = leadconfig.CatchAllSend
	CatchAllBlock = leadconfig.CatchAllBlock
)

const (
	// DefaultBaseURL is Reoon's verifier API.
	DefaultBaseURL = "https://emailverifier.reoon.com"
	// CheckTimeout bounds one power-mode check, which Reoon says can take over a minute.
	CheckTimeout = 90 * time.Second
	// BalanceTimeout bounds one balance read.
	BalanceTimeout = 30 * time.Second
	// FreshFor is how long a result counts: an older one is unchecked.
	FreshFor = 90 * 24 * time.Hour
	// maxBody is the most of a response read.
	maxBody = 64 << 10
)

// KeyFileEnv names the local key file (mode 0600), for a hand run off the server. The env
// file reaches every unit, the credential only the batch unit, so the key is never an env
// variable.
const KeyFileEnv = "LEADHOUND_REOON_KEY_FILE"

// CredentialName is the systemd LoadCredential name the batch unit gets the key as.
const CredentialName = "reoon.key" //nolint:gosec // G101: a systemd credential name, not a credential

// ErrNoKey is a run with no Reoon key: neither the credential nor KeyFileEnv.
var ErrNoKey = errors.New("verify: no Reoon key: place it at /etc/leadhound/reoon.key (deploy/README.md, The Reoon key) or set " + KeyFileEnv)

// ErrLiveOff is a call while LEADHOUND_LIVE does not allow outbound calls.
var ErrLiveOff = errors.New("verify: live fetching is off here")

// LoadKey finds the Reoon key exactly as claude.LoadKey finds the Anthropic key: the systemd
// credential first (read without a mode check, because systemd hands it over group-readable
// with an ACL for the service user), then KeyFileEnv with the mode check. The value is
// trimmed; an empty file is ErrNoKey (the batch unit's fallback credential is empty, so a
// server without the key reads as having none).
func LoadKey(getenv func(string) string) (config.Secret, error) {
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		path := filepath.Join(dir, CredentialName)
		if _, err := os.Stat(path); err == nil {
			return readKey(path, false)
		}
	}
	if path := getenv(KeyFileEnv); path != "" {
		return readKey(path, true)
	}
	return "", ErrNoKey
}

// readKey reads a key file. With checkMode it refuses a file that group or other can read
// (skipped on Windows, which has no such bits). Errors never carry the file's content.
func readKey(path string, checkMode bool) (config.Secret, error) {
	if checkMode && runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("reoon key: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("reoon key %s is readable by group or other (mode %v); run chmod 600 on it", path, fi.Mode().Perm())
		}
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the operator names the key file
	if err != nil {
		return "", fmt.Errorf("reoon key: %w", err)
	}
	defer clear(b)
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", ErrNoKey
	}
	return config.Secret(k), nil
}

// Result is one check's answer.
type Result struct {
	Status   string // one of Statuses
	Score    *int   // overall_score, nil when not given
	CatchAll *bool  // is_catch_all, nil when not given
	Role     *bool  // is_role_account, nil when not given
}

// Balance is the credits left: today's daily credits and the one-off (instant) credits.
type Balance struct {
	Daily   int
	Instant int
}

// Empty reports whether no credit of either kind is left.
func (b Balance) Empty() bool { return b.Daily <= 0 && b.Instant <= 0 }

// Client calls Reoon. Its zero BaseURL is DefaultBaseURL and its zero HTTP a client with no
// timeout of its own (each call has its own). It refuses every call unless built live.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	key     config.Secret
	live    bool
}

// New is a client with key, live when LEADHOUND_LIVE allows outbound calls.
func New(key config.Secret, live bool) *Client {
	return &Client{key: key, live: live}
}

// Check asks Reoon about address in power mode, within CheckTimeout.
func (c *Client) Check(ctx context.Context, address string) (Result, error) {
	var body struct {
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		Score    *int   `json:"overall_score"`
		CatchAll *bool  `json:"is_catch_all"`
		Role     *bool  `json:"is_role_account"`
	}
	q := url.Values{"email": {address}, "key": {c.key.Reveal()}, "mode": {"power"}}
	if err := c.get(ctx, "/api/v1/verify", q, CheckTimeout, &body); err != nil {
		return Result{}, err
	}
	if !slices.Contains(Statuses, body.Status) {
		if body.Reason != "" {
			return Result{}, c.scrub(fmt.Errorf("verify: reoon answered %q: %s", body.Status, oneLine(body.Reason)))
		}
		return Result{}, fmt.Errorf("verify: reoon answered status %q, which is not a check result", oneLine(body.Status))
	}
	return Result{Status: body.Status, Score: body.Score, CatchAll: body.CatchAll, Role: body.Role}, nil
}

// Balance reads the credits left, within BalanceTimeout. A key that is not active, or a
// response that is not a success, is an error.
func (c *Client) Balance(ctx context.Context) (Balance, error) {
	var body struct {
		APIStatus string `json:"api_status"`
		Daily     *int   `json:"remaining_daily_credits"`
		Instant   *int   `json:"remaining_instant_credits"`
		Status    string `json:"status"`
		Reason    string `json:"reason"`
	}
	if err := c.get(ctx, "/api/v1/check-account-balance/", url.Values{"key": {c.key.Reveal()}}, BalanceTimeout, &body); err != nil {
		return Balance{}, err
	}
	switch {
	case body.Status != "success":
		return Balance{}, c.scrub(fmt.Errorf("verify: reoon balance: status %q %s", oneLine(body.Status), oneLine(body.Reason)))
	case body.APIStatus != "active":
		return Balance{}, fmt.Errorf("verify: reoon balance: the API key is %q, not active", oneLine(body.APIStatus))
	case body.Daily == nil || body.Instant == nil:
		return Balance{}, errors.New("verify: reoon balance: the answer holds no credit counts")
	}
	return Balance{Daily: *body.Daily, Instant: *body.Instant}, nil
}

// get GETs path with q and decodes the JSON answer into out. Errors never carry the URL,
// which holds the key.
func (c *Client) get(ctx context.Context, path string, q url.Values, timeout time.Duration, out any) error {
	if !c.live {
		return ErrLiveOff
	}
	if c.key == "" {
		return ErrNoKey
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return errors.New("verify: reoon: the request could not be built")
	}
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	// A redirect would carry the key to another URL: refused.
	noRedirect := *hc
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req) // #nosec G107 G704 -- the base URL is the fixed Reoon API (tests swap it)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error prints the URL, and the URL holds the key
		}
		return c.scrub(fmt.Errorf("verify: reoon %s: %w", path, err))
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return c.scrub(fmt.Errorf("verify: reoon %s: %w", path, err))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("verify: reoon %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("verify: reoon %s: the answer is not the JSON expected", path)
	}
	return nil
}

// scrub replaces the key wherever it appears in err's words (a belt: no error is built from
// the URL), keeping err's chain for errors.Is.
func (c *Client) scrub(err error) error {
	k := c.key.Reveal()
	if k == "" || !strings.Contains(err.Error(), k) {
		return err
	}
	return scrubbed{msg: strings.ReplaceAll(err.Error(), k, "[redacted]"), err: err}
}

type scrubbed struct {
	msg string
	err error
}

func (s scrubbed) Error() string { return s.msg }
func (s scrubbed) Unwrap() error { return errors.Unwrap(s.err) }

// oneLine keeps the first 200 runes of s on one line, for a provider's words in an error.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// Decision is what the email check says about an address.
type Decision int

const (
	// Unchecked: no fresh result; it must be checked before it can enter a live batch.
	Unchecked Decision = iota
	// Wait: the latest fresh result is the first unknown; not sent yet, checked again the
	// next day (Recheck once that day comes).
	Wait
	// Send: the address may enter a live batch.
	Send
	// Block: the address must not enter a live batch; Status says why.
	Block
)

// Check is one stored result as Decide reads it.
type Check struct {
	Status    string
	CheckedAt time.Time
}

// Verdict is Decide's answer: the decision, the latest status it rests on ("" when
// unchecked) and whether a check is due now (unchecked, or an unknown from an earlier day).
type Verdict struct {
	Decision Decision
	Status   string
	Recheck  bool
}

// Decide is the email check's one rule, over an address's results newest first: only those
// checked after now-FreshFor count; none is Unchecked. The latest decides: safe and
// role_account send; invalid, disabled, disposable, inbox_full and spamtrap block; catch_all
// sends unless catchAll is CatchAllBlock; unknown waits for a second check on a later day than
// dayStart (the start of the operator's day) and blocks once two unknowns in a row stand. Any
// other word blocks: a status this code does not know never sends.
func Decide(history []Check, catchAll string, now, dayStart time.Time) Verdict {
	cutoff := now.Add(-FreshFor)
	var fresh []Check
	for _, h := range history {
		if h.CheckedAt.After(cutoff) {
			fresh = append(fresh, h)
		}
	}
	if len(fresh) == 0 {
		return Verdict{Decision: Unchecked, Recheck: true}
	}
	latest := fresh[0]
	v := Verdict{Status: latest.Status}
	switch latest.Status {
	case StatusSafe, StatusRole:
		v.Decision = Send
	case StatusCatchAll:
		v.Decision = Send
		if catchAll == CatchAllBlock {
			v.Decision = Block
		}
	case StatusUnknown:
		run := 0
		for _, h := range fresh {
			if h.Status != StatusUnknown {
				break
			}
			run++
		}
		if run >= 2 {
			v.Decision = Block
		} else {
			v.Decision, v.Recheck = Wait, latest.CheckedAt.Before(dayStart)
		}
	default:
		v.Decision = Block
	}
	return v
}

// Sendable reports whether status alone sends (catch_all by the setting), for counting a
// day's results; an unknown counts as not sendable.
func Sendable(status, catchAll string) bool {
	return Decide([]Check{{Status: status, CheckedAt: time.Now()}}, catchAll, time.Now(), time.Now()).Decision == Send
}
