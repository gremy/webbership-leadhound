package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// canaryKey is built at runtime so no key-shaped value sits whole in source.
var canaryKey = config.Secret("re" + "_" + "canary-key-0042")

func TestResendSends(t *testing.T) {
	var got map[string]any
	var auth, ctype, path, idem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ctype, path = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Method+" "+r.URL.Path
		idem = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = fmt.Fprint(w, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)
	}))
	defer srv.Close()

	var m Mailer = &Resend{APIKey: canaryKey, From: "brief@mail.example", Base: srv.URL}
	if m.Provider() != "resend" {
		t.Errorf("provider = %q", m.Provider())
	}
	id, err := m.Send(context.Background(), Message{To: "user@mail.example", FromName: "Leadhound", Subject: "Leadhound · Fri 23 Oct",
		Text: "1. A job", HTML: "<ol><li>A job</li></ol>", IdempotencyKey: "brief-2026-10-23"})
	if err != nil || id != "49a3999c-0ce1-4ea6-ab68-afcd6dc2e794" {
		t.Fatalf("id %q err %v", id, err)
	}
	if auth != "Bearer "+canaryKey.Reveal() || ctype != "application/json" || path != "POST /emails" || idem != "brief-2026-10-23" {
		t.Errorf("auth %q content-type %q path %q idempotency %q", auth, ctype, path, idem)
	}
	want := map[string]any{
		"from":    `"Leadhound" <brief@mail.example>`,
		"to":      []any{"user@mail.example"},
		"subject": "Leadhound · Fri 23 Oct",
		"text":    "1. A job",
		"html":    "<ol><li>A job</li></ol>",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("request = %v\nwant      %v", got, want) // nothing else: no tags, headers or tracking fields
	}
}

// Without a key there is no Idempotency-Key header, and an empty HTML part is left out.
func TestResendOptionalParts(t *testing.T) {
	var got map[string]any
	var hasIdem bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasIdem = r.Header["Idempotency-Key"]
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = fmt.Fprint(w, `{"id":"x"}`)
	}))
	defer srv.Close()
	r := &Resend{APIKey: "k", From: "brief@mail.example", Base: srv.URL}
	if _, err := r.Send(context.Background(), Message{To: "user@mail.example", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if hasIdem {
		t.Error("idempotency header sent without a key")
	}
	if _, ok := got["html"]; ok || got["from"] != "brief@mail.example" {
		t.Errorf("request = %v", got)
	}
}

func TestResendRefusesBadInputWithoutCalling(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	r := &Resend{APIKey: "k", From: "brief@mail.example", Base: srv.URL}
	if _, err := r.Send(context.Background(), Message{To: "nu"}); !errors.Is(err, ErrBadAddress) || called {
		t.Errorf("bad recipient: err %v called %v", err, called)
	}
	long := Message{To: "user@mail.example", IdempotencyKey: strings.Repeat("k", maxIdempotencyKey+1)}
	if _, err := r.Send(context.Background(), long); !errors.Is(err, ErrBadIdempotencyKey) || called {
		t.Errorf("long key: err %v called %v", err, called)
	}
}

func TestResendErrorNeverShowsTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// A server that echoes the key back must not get it into the error.
		_, _ = fmt.Fprintf(w, `{"statusCode":401,"name":"validation_error","message":"API key %s is invalid"}`, canaryKey.Reveal())
	}))
	defer srv.Close()
	r := &Resend{APIKey: canaryKey, From: "brief@mail.example", Base: srv.URL}
	_, err := r.Send(context.Background(), Message{To: "user@mail.example"})
	if err == nil {
		t.Fatal("no error")
	}
	for _, s := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(s, canaryKey.Reveal()) || !strings.Contains(err.Error(), "401") {
			t.Errorf("error = %s", s)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "resend", r, "key", r.APIKey)
	if s := fmt.Sprintf("%v %+v %#v %s", r, r, *r, buf.String()); strings.Contains(s, canaryKey.Reveal()) {
		t.Errorf("key printed: %s", s)
	}
}

func TestResendSenderName(t *testing.T) {
	for name, want := range map[string]string{
		"Leadhound":                        `"Leadhound" <f@mail.example>`,
		"Lead\"<x@mail.example>\r\nBcc: z": `"Leadx@mail.example Bcc: z" <f@mail.example>`,
		"  ":                               "f@mail.example",
		"":                                 "f@mail.example",
	} {
		if got := sender(name, "f@mail.example"); got != want {
			t.Errorf("sender(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestResendErrorText(t *testing.T) {
	long := strings.Repeat("x", 1000)
	for _, c := range []struct {
		code       int
		body, want string
	}{
		{403, `{"name":"validation_error","message":"Domain not verified."}`, "resend: 403 Domain not verified."},
		{422, `{"name":"missing_required_field"}`, "resend: 422 missing_required_field"},
		{502, `<html>Bad Gateway</html>`, "resend: 502 Bad Gateway"},
		{400, `{"message":"` + long + `"}`, "resend: 400 " + long[:300]},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("User-Agent") != "webbership-leadhound/1" {
				t.Errorf("user agent %q", r.Header.Get("User-Agent"))
			}
			w.WriteHeader(c.code)
			_, _ = fmt.Fprint(w, c.body)
		}))
		_, err := (&Resend{APIKey: "k", From: "f@mail.example", Base: srv.URL}).Send(context.Background(), Message{To: "user@mail.example"})
		srv.Close()
		if err == nil || err.Error() != c.want {
			t.Errorf("%d: err = %v, want %q", c.code, err, c.want)
		}
	}
}

// Uncertain: the message may have gone out, so a retry must reuse the key. A refusal the
// provider stated (4xx, a bad address) sent nothing.
func TestUncertain(t *testing.T) {
	for _, c := range []struct {
		code int
		body string
		want bool
	}{
		{500, `{"name":"internal_server_error"}`, true},
		{502, `bad gateway`, true},
		{409, `{"name":"concurrent_idempotent_requests","message":"in progress"}`, true},
		{409, `{"name":"invalid_idempotent_request","message":"different payload"}`, false},
		{422, `{"name":"validation_error"}`, false},
		{429, `{"name":"rate_limit_exceeded"}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			_, _ = fmt.Fprint(w, c.body)
		}))
		_, err := (&Resend{APIKey: "k", From: "f@mail.example", Base: srv.URL}).Send(context.Background(), Message{To: "user@mail.example"})
		srv.Close()
		if got := Uncertain(err); got != c.want {
			t.Errorf("%d %s: Uncertain = %v, want %v (%v)", c.code, c.body, got, c.want, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // connection refused: a transport error
	_, err := (&Resend{APIKey: "k", From: "f@mail.example", Base: srv.URL}).Send(context.Background(), Message{To: "user@mail.example"})
	if !Uncertain(err) {
		t.Errorf("transport error: %v", err)
	}
	if !AlreadySent(&ProviderError{Status: 409, Name: "invalid_idempotent_request"}) || AlreadySent(&ProviderError{Status: 409, Name: "concurrent_idempotent_requests"}) {
		t.Error("AlreadySent")
	}
	if Uncertain(nil) || Uncertain(ErrBadAddress) || Uncertain(ErrBadIdempotencyKey) {
		t.Error("nil or a refused input is not uncertain")
	}
}

// A refusal that echoes the sender and the recipient, in any case, keeps neither.
func TestResendErrorNeverShowsTheAddresses(t *testing.T) {
	from, to := "brief"+"@"+"mail.example", "reader"+"@"+"inbox.example"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"name":"validation_error","message":"%s may not send to %s"}`, strings.ToUpper(from), to)
	}))
	defer srv.Close()
	_, err := (&Resend{APIKey: canaryKey, From: config.Secret(from), Base: srv.URL}).Send(context.Background(), Message{To: to})
	if err == nil {
		t.Fatal("no error")
	}
	if want := "resend: 403 [redacted] may not send to [redacted]"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}
