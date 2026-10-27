package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/config"
)

// Models LeadHound calls.
const (
	ModelSonnet = "claude-sonnet-5-5"
	ModelHaiku  = "claude-haiku-5-5"
)

// DefaultBaseURL is the Anthropic API. It is always passed to the SDK explicitly, so a stray
// ANTHROPIC_BASE_URL in the process environment can never send the key elsewhere.
const DefaultBaseURL = "https://api.anthropic.com"

// Defaults for a Client.
const (
	defaultTimeout    = 2 * time.Minute
	defaultMaxRetries = 2
	defaultEffort     = "medium"
	maxErrSnippet     = 300
)

// Web tool versions a ResearchRequest can ask for.
const (
	// WebTools20260209 is web_search_20260209 + web_fetch_20260209 (the default).
	WebTools20260209 = "20260209"
	// WebToolsBasic is web_search_20250305 + web_fetch_20250910, the fallback when a model
	// does not take the 20260209 tools.
	WebToolsBasic = "basic"
)

// MaxContinuations is how many times a pause_turn is continued before Research gives up
// with ErrTruncated.
const MaxContinuations = 2

var (
	// ErrRefused is a call the model refused (stop_reason refusal). It was billed.
	ErrRefused = errors.New("claude: model refused")
	// ErrTruncated is a call that hit max_tokens, or a research turn still paused after
	// MaxContinuations continuations. It was billed.
	ErrTruncated = errors.New("claude: answer truncated")
	// ErrBadRequest is a 4xx the API rejected; it is never retried.
	ErrBadRequest = errors.New("claude: bad request")
)

// Usage is what one call (with its continuations) was billed for.
type Usage struct {
	Model            string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	WebSearches      int
	WebFetches       int
	Batch            bool
}

// billed says whether the usage reports anything the provider charges for.
func (u Usage) billed() bool {
	return u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens+u.WebSearches+u.WebFetches > 0
}

func (u *Usage) add(m *anthropic.Message) {
	u.InputTokens += int(m.Usage.InputTokens)
	u.OutputTokens += int(m.Usage.OutputTokens)
	u.CacheReadTokens += int(m.Usage.CacheReadInputTokens)
	u.CacheWriteTokens += int(m.Usage.CacheCreationInputTokens)
	u.WebSearches += int(m.Usage.ServerToolUse.WebSearchRequests)
	u.WebFetches += int(m.Usage.ServerToolUse.WebFetchRequests)
}

// WriteRequest asks for one structured JSON answer.
type WriteRequest struct {
	LeadID    int64          // the lead the spend counts against; 0 = none (Capped reads it)
	Model     string         // e.g. ModelSonnet
	System    string         // sent as one cached (ephemeral) block
	Prompt    string         // the user turn
	Schema    map[string]any // the JSON schema the answer must follow
	MaxTokens int
	Effort    string // low, medium (default), high, xhigh, max
}

// WriteResult is the answer, as the JSON text the model produced.
type WriteResult struct {
	JSON json.RawMessage
}

// ResearchRequest asks for a web-researched answer.
type ResearchRequest struct {
	LeadID                int64 // as WriteRequest
	Model                 string
	System                string // optional, cached like WriteRequest.System
	Prompt                string
	MaxTokens             int
	Effort                string
	SearchMaxUses         int    // web_search max_uses; must be > 0
	FetchMaxUses          int    // web_fetch max_uses; 0 leaves fetch out
	FetchMaxContentTokens int    // web_fetch max_content_tokens; required with FetchMaxUses
	WebTools              string // WebTools20260209 (default) or WebToolsBasic

	// beforeLeg, set by Capped, runs before each pause_turn continuation with the usage
	// billed so far; an error stops the turn there.
	beforeLeg func(leg int, sofar Usage) error
}

// ResearchResult is the model's final text and the web sources it cited.
type ResearchResult struct {
	Text          string
	Sources       []Source
	Continuations int // pause_turn continuations used
}

// Source is one cited web result.
type Source struct {
	URL, Title, CitedText string
}

// Caller is what Capped wraps: the real Client or a Fake.
type Caller interface {
	Write(ctx context.Context, r WriteRequest) (WriteResult, Usage, error)
	Research(ctx context.Context, r ResearchRequest) (ResearchResult, Usage, error)
}

// Client calls the Anthropic Messages API with an explicit key and base URL; it never reads
// ANTHROPIC_* from the environment.
type Client struct {
	key config.Secret
	api anthropic.Client
}

type settings struct {
	baseURL    string
	timeout    time.Duration
	maxRetries int
}

// Option changes how New builds a Client.
type Option func(*settings)

// WithBaseURL points the client at another server (tests pass their httptest URL).
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithTimeout sets the per-attempt request timeout.
func WithTimeout(d time.Duration) Option { return func(s *settings) { s.timeout = d } }

// WithMaxRetries sets the SDK's retry count for 408, 409, 429 and 5xx.
func WithMaxRetries(n int) Option { return func(s *settings) { s.maxRetries = n } }

// New builds a Client. An empty key is refused.
func New(key config.Secret, opts ...Option) (*Client, error) {
	if strings.TrimSpace(key.Reveal()) == "" {
		return nil, errors.New("claude: empty API key")
	}
	s := settings{baseURL: DefaultBaseURL, timeout: defaultTimeout, maxRetries: defaultMaxRetries}
	for _, o := range opts {
		o(&s)
	}
	if s.baseURL == "" {
		return nil, errors.New("claude: empty base URL")
	}
	api := anthropic.NewClient(
		// Without this the SDK reads ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY,
		// ANTHROPIC_AUTH_TOKEN (a Bearer header) and profiles from the process env.
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(key.Reveal()),
		option.WithBaseURL(s.baseURL),
		option.WithMaxRetries(s.maxRetries),
		option.WithRequestTimeout(s.timeout),
	)
	return &Client{key: key, api: api}, nil
}

// Write asks for one JSON answer following r.Schema. A refusal is ErrRefused and a
// max_tokens stop ErrTruncated, both with the usage billed; text that is not JSON is
// ai.ErrBadAnswer, also billed.
func (c *Client) Write(ctx context.Context, r WriteRequest) (WriteResult, Usage, error) {
	if r.Model == "" || r.MaxTokens <= 0 || r.Schema == nil {
		return WriteResult{}, Usage{}, errors.New("claude: write: model, max tokens and schema are required")
	}
	effort, err := effortOf(r.Effort)
	if err != nil {
		return WriteResult{}, Usage{}, err
	}
	p := anthropic.MessageNewParams{
		Model:     r.Model,
		MaxTokens: int64(r.MaxTokens),
		OutputConfig: anthropic.OutputConfigParam{
			Effort: effort,
			Format: anthropic.JSONOutputFormatParam{Schema: r.Schema},
		},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(r.Prompt))},
	}
	if r.System != "" {
		p.System = cachedSystem(r.System)
	}
	resp, err := c.api.Messages.New(ctx, p)
	if err != nil {
		return WriteResult{}, Usage{}, c.mapErr(ctx, "write", err)
	}
	// Usage carries the model asked for, the id the price table and the cap know; the
	// response may name a dated snapshot of it.
	u := Usage{Model: r.Model}
	u.add(resp)
	if err := stopErr(resp); err != nil {
		return WriteResult{}, u, err
	}
	text := strings.TrimSpace(textOf(resp))
	if !json.Valid([]byte(text)) {
		return WriteResult{}, u, fmt.Errorf("claude: write: answer is not JSON: %w", ai.ErrBadAnswer)
	}
	return WriteResult{JSON: json.RawMessage(text)}, u, nil
}

// Research runs one web-researched turn, continuing a pause_turn at most MaxContinuations
// times. The usage returned covers every leg billed, also when an error is returned.
func (c *Client) Research(ctx context.Context, r ResearchRequest) (ResearchResult, Usage, error) {
	if r.Model == "" || r.MaxTokens <= 0 || r.SearchMaxUses <= 0 {
		return ResearchResult{}, Usage{}, errors.New("claude: research: model, max tokens and search max uses are required")
	}
	if r.FetchMaxUses > 0 && r.FetchMaxContentTokens <= 0 {
		return ResearchResult{}, Usage{}, errors.New("claude: research: web fetch needs max content tokens")
	}
	effort, err := effortOf(r.Effort)
	if err != nil {
		return ResearchResult{}, Usage{}, err
	}
	tools, err := webTools(r)
	if err != nil {
		return ResearchResult{}, Usage{}, err
	}
	p := anthropic.MessageNewParams{
		Model:        r.Model,
		MaxTokens:    int64(r.MaxTokens),
		OutputConfig: anthropic.OutputConfigParam{Effort: effort},
		Tools:        tools,
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(r.Prompt))},
	}
	if r.System != "" {
		p.System = cachedSystem(r.System)
	}

	var (
		u   = Usage{Model: r.Model} // as in Write
		res ResearchResult
		txt strings.Builder
	)
	for leg := 0; ; leg++ {
		if leg > 0 && r.beforeLeg != nil {
			if err := r.beforeLeg(leg, u); err != nil {
				return ResearchResult{}, u, err
			}
		}
		resp, err := c.api.Messages.New(ctx, p)
		if err != nil {
			return ResearchResult{}, u, c.mapErr(ctx, "research", err)
		}
		u.add(resp)
		collect(resp, &txt, &res.Sources)
		if resp.StopReason == anthropic.StopReasonPauseTurn {
			if leg == MaxContinuations {
				return ResearchResult{}, u, fmt.Errorf("claude: research: still paused after %d continuations: %w", leg, ErrTruncated)
			}
			res.Continuations++
			p.Messages = append(p.Messages, resp.ToParam())
			continue
		}
		if err := stopErr(resp); err != nil {
			return ResearchResult{}, u, err
		}
		res.Text = strings.TrimSpace(txt.String())
		return res, u, nil
	}
}

func cachedSystem(s string) []anthropic.TextBlockParam {
	return []anthropic.TextBlockParam{{Text: s, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
}

func effortOf(e string) (anthropic.OutputConfigEffort, error) {
	if e == "" {
		e = defaultEffort
	}
	switch anthropic.OutputConfigEffort(e) {
	case anthropic.OutputConfigEffortLow, anthropic.OutputConfigEffortMedium, anthropic.OutputConfigEffortHigh,
		anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortMax:
		return anthropic.OutputConfigEffort(e), nil
	}
	return "", fmt.Errorf("claude: unknown effort %q", e)
}

func webTools(r ResearchRequest) ([]anthropic.ToolUnionParam, error) {
	var tools []anthropic.ToolUnionParam
	switch r.WebTools {
	case "", WebTools20260209:
		tools = append(tools, anthropic.ToolUnionParam{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{
			MaxUses: anthropic.Int(int64(r.SearchMaxUses)),
		}})
		if r.FetchMaxUses > 0 {
			tools = append(tools, anthropic.ToolUnionParam{OfWebFetchTool20260209: &anthropic.WebFetchTool20260209Param{
				MaxUses:          anthropic.Int(int64(r.FetchMaxUses)),
				MaxContentTokens: anthropic.Int(int64(r.FetchMaxContentTokens)),
			}})
		}
	case WebToolsBasic:
		tools = append(tools, anthropic.ToolUnionParam{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
			MaxUses: anthropic.Int(int64(r.SearchMaxUses)),
		}})
		if r.FetchMaxUses > 0 {
			tools = append(tools, anthropic.ToolUnionParam{OfWebFetchTool20250910: &anthropic.WebFetchTool20250910Param{
				MaxUses:          anthropic.Int(int64(r.FetchMaxUses)),
				MaxContentTokens: anthropic.Int(int64(r.FetchMaxContentTokens)),
			}})
		}
	default:
		return nil, fmt.Errorf("claude: unknown web tools version %q", r.WebTools)
	}
	return tools, nil
}

// stopErr maps a billed response's stop reason to an error, or nil.
func stopErr(m *anthropic.Message) error {
	switch m.StopReason {
	case anthropic.StopReasonRefusal:
		return fmt.Errorf("claude: %s: %w", m.StopDetails.Category, ErrRefused)
	case anthropic.StopReasonMaxTokens:
		return fmt.Errorf("claude: max_tokens reached: %w", ErrTruncated)
	}
	return nil
}

func textOf(m *anthropic.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if t, ok := blk.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// collect appends a response's text and its web citations.
func collect(m *anthropic.Message, txt *strings.Builder, sources *[]Source) {
	for _, blk := range m.Content {
		t, ok := blk.AsAny().(anthropic.TextBlock)
		if !ok {
			continue
		}
		txt.WriteString(t.Text)
		for _, cit := range t.Citations {
			if cit.URL == "" {
				continue
			}
			*sources = append(*sources, Source{URL: cit.URL, Title: cit.Title, CitedText: cit.CitedText})
		}
	}
}

// mapErr turns an SDK error into LeadHound's: 429, 529 and 5xx (after the SDK's retries)
// and transport failures are ai.ErrUnavailable; other 4xx are ErrBadRequest. The SDK error
// is never wrapped, only its scrubbed text kept, so the key cannot ride along.
func (c *Client) mapErr(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("claude: %s: %w", op, ctxErr)
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		code := apiErr.StatusCode
		msg := c.snippet(apiErr.RawJSON())
		switch {
		case code == 429 || code >= 500:
			return fmt.Errorf("claude: %s: status %d after retries: %s: %w", op, code, msg, ai.ErrUnavailable)
		default:
			return fmt.Errorf("claude: %s: status %d: %s: %w", op, code, msg, ErrBadRequest)
		}
	}
	return fmt.Errorf("claude: %s: %s: %w", op, c.snippet(err.Error()), ai.ErrUnavailable)
}

// snippet is the start of an error text, with the key removed should it appear.
func (c *Client) snippet(s string) string {
	if k := c.key.Reveal(); k != "" {
		s = strings.ReplaceAll(s, k, c.key.String()) // before cutting, so no part survives
	}
	if len(s) > maxErrSnippet {
		s = s[:maxErrSnippet] + "…"
	}
	return s
}
