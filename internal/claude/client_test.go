package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/config"
)

const testKey = "sk-ant-file-key-0123456789"

var testSchema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"subject": map[string]any{"type": "string"}},
	"required":             []string{"subject"},
	"additionalProperties": false,
}

// message is a Messages API response body.
func message(stop, text string, in, out, searches int) string {
	content, _ := json.Marshal([]map[string]any{{"type": "text", "text": text, "citations": []any{}}})
	return fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5",
"content":%s,"stop_reason":%q,"stop_details":null,
"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,
"server_tool_use":{"web_search_requests":%d,"web_fetch_requests":0}}}`, content, stop, in, out, searches)
}

type capture struct {
	calls  atomic.Int32
	header atomic.Value // http.Header of the last request
	body   atomic.Value // []byte of the last request
}

// server answers every request with reply(n), n counting from 1.
func server(t *testing.T, reply func(n int) (int, string)) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(c.calls.Add(1))
		b, _ := io.ReadAll(r.Body)
		c.header.Store(r.Header.Clone())
		c.body.Store(b)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		status, body := reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After-Ms", "1")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func newClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(config.Secret(testKey), WithBaseURL(url), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeReq() WriteRequest {
	return WriteRequest{Model: ModelSonnet, System: "You write emails.", Prompt: "Write one.", Schema: testSchema, MaxTokens: 1000}
}

func TestNewRefusesEmptyKey(t *testing.T) {
	for _, k := range []string{"", "  \n"} {
		if _, err := New(config.Secret(k)); err == nil {
			t.Errorf("New(%q) = nil error, want refusal", k)
		}
	}
}

// The SDK reads ANTHROPIC_* from the process environment unless told not to; the request
// must reach the explicit server with the file key and nothing from the environment.
func TestEnvironmentIgnored(t *testing.T) {
	envSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request reached ANTHROPIC_BASE_URL")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer envSrv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", envSrv.URL)
	t.Setenv("ANTHROPIC_API_KEY", "sentinel-env")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel-token")

	srv, got := server(t, func(int) (int, string) { return 200, message("end_turn", `{"subject":"hi"}`, 10, 5, 0) })
	res, u, err := newClient(t, srv.URL).Write(context.Background(), writeReq())
	if err != nil {
		t.Fatal(err)
	}
	if string(res.JSON) != `{"subject":"hi"}` || u.InputTokens != 10 || u.OutputTokens != 5 || u.Model != ModelSonnet {
		t.Errorf("result %s usage %+v", res.JSON, u)
	}
	h := got.header.Load().(http.Header)
	if k := h.Get("X-Api-Key"); k != testKey {
		t.Errorf("X-Api-Key = %q, want the file key", k)
	}
	if a := h.Get("Authorization"); a != "" {
		t.Errorf("Authorization = %q, want none", a)
	}
	for k, v := range h {
		if strings.Contains(strings.Join(v, " "), "sentinel") {
			t.Errorf("header %s carries an environment value", k)
		}
	}
	body := string(got.body.Load().([]byte))
	for _, want := range []string{`"cache_control":{"type":"ephemeral"`, `"effort":"medium"`, `"type":"json_schema"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request body lacks %s: %s", want, body)
		}
	}
	for _, bad := range []string{"temperature", "top_p", "thinking", "sentinel"} {
		if strings.Contains(body, bad) {
			t.Errorf("request body carries %q: %s", bad, body)
		}
	}
}

func TestWriteStopReasons(t *testing.T) {
	cases := []struct {
		name, stop, text string
		want             error
	}{
		{"refusal", "refusal", "", ErrRefused},
		{"max tokens", "max_tokens", `{"subj`, ErrTruncated},
		{"not json", "end_turn", "Sure! Here it is.", ai.ErrBadAnswer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := server(t, func(int) (int, string) { return 200, message(tc.stop, tc.text, 40, 7, 0) })
			_, u, err := newClient(t, srv.URL).Write(context.Background(), writeReq())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if u.InputTokens != 40 || u.OutputTokens != 7 {
				t.Errorf("billed usage lost: %+v", u)
			}
		})
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status    int
		wantCalls int32
		want      error
	}{
		{429, 3, ai.ErrUnavailable},
		{529, 3, ai.ErrUnavailable},
		{500, 3, ai.ErrUnavailable},
		{400, 1, ErrBadRequest},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv, got := server(t, func(int) (int, string) {
				return tc.status, `{"type":"error","error":{"type":"x","message":"nope"}}`
			})
			_, u, err := newClient(t, srv.URL).Write(context.Background(), writeReq())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == ErrBadRequest && errors.Is(err, ai.ErrUnavailable) {
				t.Error("400 mapped to ErrUnavailable")
			}
			if n := got.calls.Load(); n != tc.wantCalls {
				t.Errorf("calls = %d, want %d", n, tc.wantCalls)
			}
			if u.billed() {
				t.Errorf("failed call reported usage %+v", u)
			}
		})
	}
}

func TestKeyScrubbedFromErrors(t *testing.T) {
	srv, _ := server(t, func(int) (int, string) {
		return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad key ` + testKey + `"}}`
	})
	_, _, err := newClient(t, srv.URL).Write(context.Background(), writeReq())
	if err == nil || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "file-key") {
		t.Fatalf("err = %v, want an error without the key", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("err = %v, want the key shown as [redacted]", err)
	}
}

func researchReq() ResearchRequest {
	return ResearchRequest{Model: ModelHaiku, Prompt: "Find the phone of example.com", MaxTokens: 2000,
		SearchMaxUses: 3, FetchMaxUses: 2, FetchMaxContentTokens: 8000}
}

func TestResearchPauseTurn(t *testing.T) {
	srv, got := server(t, func(n int) (int, string) {
		if n <= 2 {
			return 200, message("pause_turn", "part ", 100, 10, 1)
		}
		body := `{"id":"m","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"end_turn",
"content":[{"type":"text","text":"+44 20 7946 0000","citations":[{"type":"web_search_result_location","url":"https://example.com/contact","title":"Contact","cited_text":"call us","encrypted_index":"x"}]}],
"usage":{"input_tokens":100,"output_tokens":10,"server_tool_use":{"web_search_requests":1,"web_fetch_requests":1}}}`
		return 200, body
	})
	res, u, err := newClient(t, srv.URL).Research(context.Background(), researchReq())
	if err != nil {
		t.Fatal(err)
	}
	if got.calls.Load() != 3 || res.Continuations != 2 {
		t.Errorf("calls %d continuations %d, want 3 and 2", got.calls.Load(), res.Continuations)
	}
	if u.InputTokens != 300 || u.OutputTokens != 30 || u.WebSearches != 3 || u.WebFetches != 1 {
		t.Errorf("usage %+v, want the sum of three legs", u)
	}
	if len(res.Sources) != 1 || res.Sources[0].URL != "https://example.com/contact" {
		t.Errorf("sources %+v", res.Sources)
	}
	if !strings.HasSuffix(res.Text, "+44 20 7946 0000") {
		t.Errorf("text %q", res.Text)
	}
	body := string(got.body.Load().([]byte))
	for _, want := range []string{`"type":"web_search_20260209"`, `"type":"web_fetch_20260209"`, `"max_uses":3`, `"max_content_tokens":8000`} {
		if !strings.Contains(body, want) {
			t.Errorf("request lacks %s", want)
		}
	}
}

func TestResearchPausedTooOften(t *testing.T) {
	srv, got := server(t, func(int) (int, string) { return 200, message("pause_turn", "", 50, 5, 1) })
	_, u, err := newClient(t, srv.URL).Research(context.Background(), researchReq())
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if got.calls.Load() != 1+MaxContinuations || u.InputTokens != 150 {
		t.Errorf("calls %d usage %+v", got.calls.Load(), u)
	}
}

func TestResearchBasicTools(t *testing.T) {
	srv, got := server(t, func(int) (int, string) { return 200, message("end_turn", "ok", 1, 1, 0) })
	r := researchReq()
	r.WebTools = WebToolsBasic
	if _, _, err := newClient(t, srv.URL).Research(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	body := string(got.body.Load().([]byte))
	if !strings.Contains(body, `"web_search_20250305"`) || !strings.Contains(body, `"web_fetch_20250910"`) {
		t.Errorf("basic tools missing: %s", body)
	}
	r.WebTools = "19990101"
	if _, _, err := newClient(t, srv.URL).Research(context.Background(), r); err == nil {
		t.Error("unknown web tools version accepted")
	}
}
