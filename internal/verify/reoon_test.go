package verify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
)

const testKey = "rk-test-SECRET-123" //nolint:gosec // G101: a fake key for the test server

// reoonServer is a fake Reoon: verify answers body for every address (and records the query);
// the balance endpoint answers balance.
func reoonServer(t *testing.T, code int, body, balance string, delay time.Duration) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(code)
		switch r.URL.Path {
		case "/api/v1/verify":
			_, _ = fmt.Fprint(w, body)
		case "/api/v1/check-account-balance/":
			_, _ = fmt.Fprint(w, balance)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func client(srv *httptest.Server) *Client {
	c := New(config.Secret(testKey), true)
	c.BaseURL = srv.URL
	return c
}

// TestCheckEachStatus: every power status is read with its score and flags, in power mode
// with the key and the address on the query.
func TestCheckEachStatus(t *testing.T) {
	for _, st := range Statuses {
		t.Run(st, func(t *testing.T) {
			srv, qs := reoonServer(t, 200, fmt.Sprintf(`{"email":"a@b.example","status":%q,"overall_score":87,"is_catch_all":%v,"is_role_account":true}`,
				st, st == StatusCatchAll), "", 0)
			r, err := client(srv).Check(context.Background(), "a@b.example")
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != st || r.Score == nil || *r.Score != 87 || r.Role == nil || !*r.Role || r.CatchAll == nil || *r.CatchAll != (st == StatusCatchAll) {
				t.Errorf("result %+v", r)
			}
			q := (*qs)[0]
			for _, want := range []string{"mode=power", "key=" + testKey, "email=a%40b.example"} {
				if !strings.Contains(q, want) {
					t.Errorf("query %q lacks %q", q, want)
				}
			}
		})
	}
}

// TestCheckErrorsNeverStatuses: an HTTP error, an error body, a status not known, malformed
// JSON and a slow answer are errors, and none of them carries the key.
func TestCheckErrorsNeverStatuses(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		body  string
		delay time.Duration
		want  string
	}{
		{"http 500", 500, `{"status":"safe"}`, 0, "HTTP 500"},
		{"http 401", 401, `{"status":"error","reason":"Invalid key ` + testKey + `"}`, 0, "HTTP 401"},
		{"error body", 200, `{"status":"error","reason":"Invalid API key ` + testKey + `"}`, 0, "[redacted]"},
		{"unknown word", 200, `{"status":"maybe"}`, 0, "not a check result"},
		{"empty status", 200, `{}`, 0, "not a check result"},
		{"malformed", 200, `{"status":`, 0, "not the JSON expected"},
		{"redirect", 302, ``, 0, "HTTP 302"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := reoonServer(t, tc.code, tc.body, "", tc.delay)
			_, err := client(srv).Check(context.Background(), "a@b.example")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the error carries the key: %v", err)
			}
		})
	}
}

// TestCheckTimeout: a check slower than its limit is an error that names the deadline and not
// the key (the URL, which holds the key, is never in it).
func TestCheckTimeout(t *testing.T) {
	srv, _ := reoonServer(t, 200, `{"status":"safe"}`, "", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client(srv).Check(ctx, "a@b.example")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline", err)
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("the error carries the URL or key: %v", err)
	}
}

// TestBalance: credits read; api_status not active, a non-success status, missing counts and
// malformed JSON are errors.
func TestBalance(t *testing.T) {
	srv, _ := reoonServer(t, 200, "", `{"api_status":"active","remaining_daily_credits":20,"remaining_instant_credits":0,"status":"success"}`, 0)
	b, err := client(srv).Balance(context.Background())
	if err != nil || b != (Balance{Daily: 20}) || b.Empty() {
		t.Fatalf("balance %+v, %v", b, err)
	}
	if !(Balance{}).Empty() || (Balance{Instant: 1}).Empty() {
		t.Error("Empty is wrong")
	}
	for _, tc := range []struct{ body, want string }{
		{`{"api_status":"inactive","remaining_daily_credits":20,"remaining_instant_credits":0,"status":"success"}`, "not active"},
		{`{"status":"error","reason":"bad key"}`, `status "error"`},
		{`{"api_status":"active","status":"success"}`, "no credit counts"},
		{`not json`, "not the JSON expected"},
	} {
		srv, _ := reoonServer(t, 200, "", tc.body, 0)
		if _, err := client(srv).Balance(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.body, err, tc.want)
		}
	}
}

// TestLiveOffAndNoKey: a client built not live, or with no key, makes no request.
func TestLiveOffAndNoKey(t *testing.T) {
	srv, qs := reoonServer(t, 200, `{"status":"safe"}`, `{}`, 0)
	off := New(config.Secret(testKey), false)
	off.BaseURL = srv.URL
	if _, err := off.Check(context.Background(), "a@b.example"); !errors.Is(err, ErrLiveOff) {
		t.Errorf("live off: %v", err)
	}
	if _, err := off.Balance(context.Background()); !errors.Is(err, ErrLiveOff) {
		t.Errorf("live off balance: %v", err)
	}
	nokey := New("", true)
	nokey.BaseURL = srv.URL
	if _, err := nokey.Check(context.Background(), "a@b.example"); !errors.Is(err, ErrNoKey) {
		t.Errorf("no key: %v", err)
	}
	if len(*qs) != 0 {
		t.Errorf("requests made: %v", *qs)
	}
}

// TestLoadKey: the credential first, then the key file with its mode check; an empty file is
// no key; nothing is ErrNoKey.
func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if _, err := LoadKey(getenv); !errors.Is(err, ErrNoKey) {
		t.Fatalf("nothing set: %v", err)
	}
	cred := filepath.Join(dir, "creds")
	if err := os.MkdirAll(cred, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cred, CredentialName), []byte(" k1\n"), 0o640); err != nil { // #nosec G306 -- systemd hands a credential over group-readable; the test mirrors it
		t.Fatal(err)
	}
	env["CREDENTIALS_DIRECTORY"] = cred
	if k, err := LoadKey(getenv); err != nil || k.Reveal() != "k1" {
		t.Fatalf("credential: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cred, CredentialName), nil, 0o640); err != nil { // #nosec G306 -- as above
		t.Fatal(err)
	}
	if _, err := LoadKey(getenv); !errors.Is(err, ErrNoKey) {
		t.Errorf("empty credential (the unit's fallback): %v, want ErrNoKey", err)
	}
	delete(env, "CREDENTIALS_DIRECTORY")
	file := filepath.Join(dir, "reoon.key")
	if err := os.WriteFile(file, []byte("k2"), 0o600); err != nil {
		t.Fatal(err)
	}
	env[KeyFileEnv] = file
	if k, err := LoadKey(getenv); err != nil || k.Reveal() != "k2" {
		t.Fatalf("key file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(file, 0o644); err != nil { // #nosec G302 -- the group-readable file the loader must refuse
			t.Fatal(err)
		}
		if _, err := LoadKey(getenv); err == nil || strings.Contains(err.Error(), "k2") {
			t.Errorf("group-readable key file: %v", err)
		}
	}
}

// TestDecide: the verdict for every status, the catch-all setting, the second unknown, the
// next-day recheck and the 90-day freshness.
func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	one := func(st string) []Check { return []Check{{Status: st, CheckedAt: now.Add(-time.Hour)}} }
	for st, want := range map[string]Decision{
		StatusSafe: Send, StatusRole: Send, StatusCatchAll: Send,
		StatusInvalid: Block, StatusDisabled: Block, StatusDisposable: Block, StatusInboxFull: Block, StatusSpamtrap: Block,
		StatusUnknown: Wait, "some_new_word": Block,
	} {
		if v := Decide(one(st), "", now, day); v.Decision != want || v.Status != st {
			t.Errorf("%s: %+v, want decision %d", st, v, want)
		}
	}
	if v := Decide(one(StatusCatchAll), CatchAllBlock, now, day); v.Decision != Block {
		t.Errorf("catch_all with block: %+v", v)
	}
	if v := Decide(one(StatusCatchAll), CatchAllSend, now, day); v.Decision != Send {
		t.Errorf("catch_all with send: %+v", v)
	}
	if v := Decide(nil, "", now, day); v.Decision != Unchecked || !v.Recheck {
		t.Errorf("no result: %+v", v)
	}
	// unknown today: wait, no recheck today; unknown yesterday: wait and recheck now.
	if v := Decide(one(StatusUnknown), "", now, day); v.Decision != Wait || v.Recheck {
		t.Errorf("unknown today: %+v", v)
	}
	yesterday := []Check{{Status: StatusUnknown, CheckedAt: day.Add(-time.Hour)}}
	if v := Decide(yesterday, "", now, day); v.Decision != Wait || !v.Recheck {
		t.Errorf("unknown yesterday: %+v", v)
	}
	two := []Check{{Status: StatusUnknown, CheckedAt: now.Add(-time.Hour)}, {Status: StatusUnknown, CheckedAt: day.Add(-time.Hour)}}
	if v := Decide(two, "", now, day); v.Decision != Block || v.Status != StatusUnknown {
		t.Errorf("second unknown: %+v", v)
	}
	// An unknown after a safe is a first unknown; a safe after an unknown sends.
	if v := Decide([]Check{two[0], {Status: StatusSafe, CheckedAt: day.Add(-48 * time.Hour)}}, "", now, day); v.Decision != Wait {
		t.Errorf("unknown after safe: %+v", v)
	}
	if v := Decide([]Check{{Status: StatusSafe, CheckedAt: now}, two[0]}, "", now, day); v.Decision != Send {
		t.Errorf("safe after unknown: %+v", v)
	}
	// Older than 90 days: unchecked, even an invalid.
	old := []Check{{Status: StatusInvalid, CheckedAt: now.Add(-FreshFor - time.Hour)}}
	if v := Decide(old, "", now, day); v.Decision != Unchecked {
		t.Errorf("stale invalid: %+v", v)
	}
	// An old unknown does not count toward a second one.
	staleRun := []Check{two[0], {Status: StatusUnknown, CheckedAt: now.Add(-FreshFor - time.Hour)}}
	if v := Decide(staleRun, "", now, day); v.Decision != Wait {
		t.Errorf("unknown after a stale unknown: %+v", v)
	}
	if !Sendable(StatusRole, "") || Sendable(StatusUnknown, "") || Sendable(StatusCatchAll, CatchAllBlock) {
		t.Error("Sendable is wrong")
	}
}
