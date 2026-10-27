package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// DefaultBaseURL is TypeSafe's API; Jev POSTs to DefaultBaseURL + "/v1/systemone".
const DefaultBaseURL = "https://api.typesafe.ai"

const (
	jevTries       = 3                      // tries per Decide, the first included
	jevBackoffBase = 500 * time.Millisecond // doubled after each retry
	jevTimeout     = 20 * time.Second
	maxBody        = 4 << 20 // bytes read from any response
	maxErrSnippet  = 200     // bytes of an error body kept in an error
	statusOverload = 529     // TypeSafe's "overloaded"
)

// Jev is the TypeSafe System One client for one pinned model. Every question for one state
// goes out in one call. The key stays redacted however a Jev is printed.
type Jev struct {
	Model      string                                           // pinned, e.g. "jev-1.13.0"
	BaseURL    string                                           // DefaultBaseURL when empty; tests point it at httptest
	HTTPClient *http.Client                                     // a 20 s client when nil
	Sleep      func(ctx context.Context, d time.Duration) error // the backoff wait; a real timer when nil

	key config.Secret
}

// NewJev returns a client for model that authenticates with key.
func NewJev(key config.Secret, model string) *Jev {
	return &Jev{Model: model, BaseURL: DefaultBaseURL, HTTPClient: &http.Client{Timeout: jevTimeout}, key: key}
}

// Name is the pinned model.
func (j *Jev) Name() string { return j.Model }

// Provider is "typesafe".
func (*Jev) Provider() string { return "typesafe" }

// String and GoString keep the key out of every fmt verb.
func (j Jev) String() string   { return fmt.Sprintf("ai.Jev{model: %s, key: %s}", j.Model, j.key) }
func (j Jev) GoString() string { return j.String() }

type jevQuestion struct {
	Type         string             `json:"type"`
	Criteria     map[string]*string `json:"criteria"`
	Instructions string             `json:"instructions"`
}

type jevRequest struct {
	Model     string                 `json:"model"`
	State     string                 `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevResponse struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Type          string             `json:"type"`
		Choice        string             `json:"choice"`
		Confidence    float64            `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Decide asks every question about state in one call. A 429 or 529 is retried with
// exponential backoff (500 ms, then 1 s; 3 tries in all) and ends in ErrUnavailable; a 400
// naming max_tokens_exceeded is ErrTooLong. Other failures are not retried.
func (j *Jev) Decide(ctx context.Context, state string, qs []Question) (map[string]Answer, Usage, error) {
	if err := checkQuestions(qs); err != nil {
		return nil, Usage{}, err
	}
	body, err := json.Marshal(j.request(state, qs))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("jev: encode request: %w", err)
	}

	backoff := jevBackoffBase
	for try := 1; ; try++ {
		status, resp, err := j.post(ctx, body)
		if err != nil {
			return nil, Usage{}, err
		}
		switch {
		case status == http.StatusOK:
			return j.answers(resp, qs)
		case status == http.StatusTooManyRequests || status == statusOverload:
			if try == jevTries {
				return nil, Usage{}, fmt.Errorf("jev: status %d after %d tries: %w", status, try, ErrUnavailable)
			}
			if err := j.sleep(ctx, backoff); err != nil {
				return nil, Usage{}, fmt.Errorf("jev: backoff: %w", err)
			}
			backoff *= 2
		case status == http.StatusBadRequest && bytes.Contains(resp, []byte("max_tokens_exceeded")):
			return nil, Usage{}, fmt.Errorf("jev: %d questions: %w", len(qs), ErrTooLong)
		default:
			return nil, Usage{}, fmt.Errorf("jev: status %d: %s", status, j.snippet(resp))
		}
	}
}

func (j *Jev) request(state string, qs []Question) jevRequest {
	req := jevRequest{Model: j.Model, State: state, Questions: make(map[string]jevQuestion, len(qs))}
	for _, q := range qs {
		crit := make(map[string]*string, len(q.Options))
		for _, o := range q.Options {
			if o.Description == "" {
				crit[o.Name] = nil // the API's "no description"
				continue
			}
			d := o.Description
			crit[o.Name] = &d
		}
		req.Questions[q.Name] = jevQuestion{Type: "choice", Criteria: crit, Instructions: q.Instructions}
	}
	return req
}

// post sends one request and returns the status and the (capped) body.
func (j *Jev) post(ctx context.Context, body []byte) (int, []byte, error) {
	base := j.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("jev: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+j.key.Reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := j.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: jevTimeout}
	}
	resp, err := client.Do(req) // #nosec G704 -- the URL is the configured API base, never input
	if err != nil {
		// A *url.Error names the URL and the cause, never the headers.
		return 0, nil, fmt.Errorf("jev: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("jev: read response: %w", err)
	}
	return resp.StatusCode, b, nil
}

// answers decodes a 200 body and checks it answers every question with one of its options.
// A 200 is billed whatever it holds, so every failure here is ErrBadAnswer and carries the
// usage the body reports (none when the body does not decode).
func (j *Jev) answers(body []byte, qs []Question) (map[string]Answer, Usage, error) {
	var r jevResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, Usage{}, fmt.Errorf("jev: decode response: %w: %w", ErrBadAnswer, err)
	}
	usage := Usage{InputTokens: r.Usage.InputTokens, Model: r.Model}
	out := make(map[string]Answer, len(qs))
	for _, q := range qs {
		a, ok := r.Answers[q.Name]
		if !ok {
			return nil, usage, fmt.Errorf("jev: no answer for question %q: %w", q.Name, ErrBadAnswer)
		}
		if !hasOption(q, a.Choice) {
			return nil, usage, fmt.Errorf("jev: question %q: choice %q is not an option: %w", q.Name, j.scrub(a.Choice), ErrBadAnswer)
		}
		out[q.Name] = Answer{Choice: a.Choice, Confidence: a.Confidence, Probabilities: a.Probabilities}
	}
	return out, usage, nil
}

// snippet is the start of an error body, with the key removed should the server echo it.
func (j *Jev) snippet(b []byte) string {
	s := j.scrub(string(b)) // before cutting, so no part of the key survives the cut
	if len(s) > maxErrSnippet {
		s = s[:maxErrSnippet] + "…"
	}
	return s
}

func (j *Jev) scrub(s string) string {
	if k := j.key.Reveal(); k != "" {
		s = strings.ReplaceAll(s, k, j.key.String())
	}
	return s
}

func (j *Jev) sleep(ctx context.Context, d time.Duration) error {
	if j.Sleep != nil {
		return j.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
