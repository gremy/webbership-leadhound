package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// ResendBase is Resend's API (https://resend.com/docs/api-reference/emails/send-email).
const ResendBase = "https://api.resend.com"

const (
	resendTimeout     = 20 * time.Second
	maxReply          = 64 << 10 // bytes read from any response
	maxErrText        = 300      // runes of a provider message kept in an error
	maxIdempotencyKey = 256      // Resend's limit
)

// ErrBadIdempotencyKey: the key is longer than Resend accepts.
var ErrBadIdempotencyKey = errors.New("mail: idempotency key over 256 characters")

// Resend sends through Resend's HTTP API. From is an address on a domain verified in
// Resend; both it and the key stay redacted however a Resend is printed or logged.
type Resend struct {
	APIKey config.Secret
	From   config.Secret
	Base   string       // ResendBase unless testing
	Client *http.Client // a 20 s client unless set
}

// Provider is "resend".
func (*Resend) Provider() string { return "resend" }

// resendEmail is the whole request body: no tags, headers or tracking options.
type resendEmail struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	HTML    string   `json:"html,omitempty"`
}

// Send posts one email and returns Resend's message id. An error names Resend's status and
// message, never the key, the sender or the recipient.
func (r *Resend) Send(ctx context.Context, m Message) (string, error) {
	if err := Check(m.To); err != nil {
		return "", err
	}
	if len(m.IdempotencyKey) > maxIdempotencyKey {
		return "", ErrBadIdempotencyKey
	}
	raw, err := json.Marshal(resendEmail{From: sender(m.FromName, r.From.Reveal()), To: []string{m.To}, Subject: m.Subject, Text: m.Text, HTML: m.HTML})
	if err != nil {
		return "", fmt.Errorf("resend: encode: %w", err)
	}
	base := r.Base
	if base == "" {
		base = ResendBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/emails", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("resend: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey.Reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "webbership-leadhound/1")
	if m.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: resendTimeout}
	}
	resp, err := client.Do(req) // #nosec G704 -- the URL is the configured API base, never input
	if err != nil {
		// A *url.Error names the URL and the cause, never the headers. The message may have
		// gone out: uncertain.
		return "", fmt.Errorf("resend: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if resp.StatusCode/100 == 2 {
		var ok struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(reply, &ok)
		return ok.ID, nil
	}
	var e struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(reply, &e)
	msg := e.Message
	if msg == "" {
		msg = e.Name
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	// Before cutting, so no part of a value survives the cut.
	msg = scrub(msg, r.APIKey.Reveal(), r.From.Reveal(), m.To)
	if rs := []rune(msg); len(rs) > maxErrText {
		msg = string(rs[:maxErrText])
	}
	return "", &ProviderError{Status: resp.StatusCode, Name: e.Name, Text: msg}
}

// ProviderError is a refusal the provider answered with.
type ProviderError struct {
	Status     int
	Name, Text string
}

func (e *ProviderError) Error() string { return fmt.Sprintf("resend: %d %s", e.Status, e.Text) }

// AlreadySent reports a refusal that proves the key's first request went out: Resend keeps
// the key of a completed request and refuses it with another payload (the brief changed
// between two runs on the same day).
func AlreadySent(err error) bool {
	var p *ProviderError
	return errors.As(err, &p) && p.Name == "invalid_idempotent_request"
}

// Uncertain reports whether a failed send may still have delivered the message: no answer
// (time-out, connection lost), a server error, or the same key still in progress. A retry
// after it must reuse the idempotency key; any other refusal sent nothing.
func Uncertain(err error) bool {
	if err == nil || errors.Is(err, ErrBadAddress) || errors.Is(err, ErrBadIdempotencyKey) {
		return false
	}
	var p *ProviderError
	if !errors.As(err, &p) {
		return true
	}
	return p.Status >= 500 || p.Name == "concurrent_idempotent_requests"
}

// scrub replaces each non-empty value in s, in any letter case, with "[redacted]": the
// key, the sender and the recipient never reach an error.
func scrub(s string, values ...string) string {
	for _, v := range values {
		if v != "" {
			s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(v)).ReplaceAllLiteralString(s, config.Secret("").String())
		}
	}
	return s
}

// sender is `"Name" <address>` in plain UTF-8 (Resend builds the MIME header itself), with
// characters that could break the header removed; the bare address without a name.
func sender(name, address string) string {
	name = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '<' || r == '>' || r == '\\':
			return -1
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, name)), " ")
	if name == "" {
		return address
	}
	return `"` + name + `" <` + address + `>`
}
