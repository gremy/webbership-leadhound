package outreach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// writerFixture returns the written draft of a testdata/ground fixture as the model answers
// it, in the wire shape.
func writerFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	for _, f := range groundFixtures(t) {
		if f.name == name {
			b, err := EncodeWireDraft(f.Draft)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("no ground fixture %q", name)
	return nil
}

// writerSlots are the slots the template path fills for the fictional ground lead.
func writerSlots(cfg leadconfig.Config) Slots {
	org := Provenance{Table: "org", ID: 7}
	probe := Provenance{Table: "site_probe", ID: 11}
	return Slots{
		OrgName:      Slot{Value: "Northfield Museum Trust", Source: org},
		Domain:       Slot{Value: "northfield-museum.example", Source: org},
		Host:         Slot{Value: "northfield-museum.example", Source: org},
		Major:        Slot{Value: "7", Source: probe},
		Exact:        Slot{Value: "7.98", Source: probe},
		FindingURL:   Slot{Value: "https://northfield-museum.example/CHANGELOG.txt", Source: probe},
		EOLDate:      ConfigSlot("5 January 2025"),
		EvidenceLine: Slot{Value: "the page loads Drupal's core files at version 7.98", Source: probe},
		SenderName:   ConfigSlot(cfg.Sender.Name),
		IdentityLine: ConfigSlot(cfg.Sender.IdentityLine),
		OptOutLine:   ConfigSlot("If you would rather not hear from me, reply with the word stop."),

		EvidenceSignal: drupal.Signal{Name: drupal.SigCoreAssetV, Value: "7.98"},
	}
}

func writerInput(t *testing.T, lead string) WriteInput {
	t.Helper()
	cfg := renderDemoConfig(t)
	return WriteInput{
		LeadID:    42,
		Campaign:  "Drupal end-of-life",
		Lang:      LangEnglish,
		Sheet:     groundSheet(t, lead),
		Rules:     groundRules(lead),
		CallSteps: []int{2},
		Render: WriteRender{
			Cfg: cfg, Footer: cfg.Templates.Footers[cfg.Policy("GB").Footer],
			Slots: writerSlots(cfg), Greeting: "Hello,",
		},
	}
}

// scripted answers each Write with the next reply and records every request.
type scripted struct {
	mu      sync.Mutex
	replies []scriptReply
	reqs    []claude.WriteRequest
}

type scriptReply struct {
	json json.RawMessage
	err  error
}

var sonnetUsage = claude.Usage{Model: claude.ModelSonnet, InputTokens: 900, OutputTokens: 600, CacheReadTokens: 1500}

func (s *scripted) Write(_ context.Context, r claude.WriteRequest) (claude.WriteResult, claude.Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
	if len(s.replies) == 0 {
		return claude.WriteResult{}, claude.Usage{}, ai.ErrNotScripted
	}
	rep := s.replies[0]
	s.replies = s.replies[1:]
	return claude.WriteResult{JSON: rep.json}, sonnetUsage, rep.err
}

func TestWriterCleanFirstTry(t *testing.T) {
	fake := &claude.Fake{WriteAnswer: claude.WriteResult{JSON: writerFixture(t, "clean_d7")}}
	w := &Writer{Caller: fake}
	res, err := w.WriteLead(context.Background(), writerInput(t, "d7"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "" || len(res.Attempts) != 1 || !res.Attempts[0].OK() || fake.Calls() != 1 {
		t.Fatalf("fallback %q, attempts %+v, calls %d", res.Fallback, res.Attempts, fake.Calls())
	}
	if res.Written == nil || len(res.Emails) != 4 || res.CallCard == nil {
		t.Fatalf("written %v, emails %d, card %v", res.Written != nil, len(res.Emails), res.CallCard)
	}
	var steps []int
	for _, e := range res.Emails {
		steps = append(steps, e.Step)
		if !strings.HasPrefix(e.Draft.Body, "Hello,\n\n") {
			t.Errorf("step %d: no greeting:\n%s", e.Step, e.Draft.Body)
		}
	}
	if !slices.Equal(steps, []int{0, 1, 3, 4}) {
		t.Fatalf("steps %v", steps)
	}
	if n := len(leadconfig.CallLines(res.CallCard.Body)); n != leadconfig.CallPoints {
		t.Fatalf("call card has %d lines", n)
	}
	if res.FactSHA256 != groundSheet(t, "d7").SHA256() || res.Attempts[0].Model != claude.FakeModel {
		t.Fatalf("hash %s model %s", res.FactSHA256, res.Attempts[0].Model)
	}
}

func TestWriterG3ThenCleanRetry(t *testing.T) {
	s := &scripted{replies: []scriptReply{{json: writerFixture(t, "g3_invented_year")}, {json: writerFixture(t, "clean_d7")}}}
	res, err := (&Writer{Caller: s}).WriteLead(context.Background(), writerInput(t, "d7"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "" || len(res.Attempts) != 2 || len(s.reqs) != 2 {
		t.Fatalf("fallback %q attempts %d reqs %d", res.Fallback, len(res.Attempts), len(s.reqs))
	}
	first, second := res.Attempts[0], res.Attempts[1]
	if first.OK() || len(first.Failures) != 1 || first.Failures[0].Code != CodeClaim || first.Failures[0].Token != "2019" || first.Output == nil {
		t.Fatalf("first attempt %+v", first)
	}
	if !second.OK() || res.Written == nil || len(res.Emails) != 4 {
		t.Fatalf("second attempt %+v", second)
	}
	if first.CostMicros <= 0 || first.Model != claude.ModelSonnet || first.StopReason != StopEndTurn {
		t.Fatalf("first attempt billing %+v", first)
	}
	retry := s.reqs[1].Prompt
	if !strings.HasPrefix(retry, s.reqs[0].Prompt) || !strings.Contains(retry, "G3 step 0 paragraph 1 \"2019\"") || !strings.Contains(retry, "Your previous answer:") {
		t.Fatalf("retry turn does not name the failure:\n%s", retry[len(s.reqs[0].Prompt):])
	}
	if s.reqs[0].System != s.reqs[1].System || s.reqs[0].System != res.System {
		t.Fatal("the system prompt changed between attempts")
	}
}

func TestWriterTwoFailuresFallback(t *testing.T) {
	fake := &claude.Fake{WriteAnswer: claude.WriteResult{JSON: writerFixture(t, "g3_invented_year")}}
	res, err := (&Writer{Caller: fake}).WriteLead(context.Background(), writerInput(t, "d7"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 2 || fake.Calls() != 2 || res.Written != nil || res.Emails != nil {
		t.Fatalf("attempts %d calls %d written %v", len(res.Attempts), fake.Calls(), res.Written != nil)
	}
	if want := "Draft refused: G3 '2019' is not in a cited fact"; res.Fallback != want {
		t.Fatalf("fallback %q, want %q", res.Fallback, want)
	}
}

func TestWriterRefusalRetryThenFallback(t *testing.T) {
	for _, tc := range []struct {
		err      error
		stop     string
		fallback string
	}{
		{fmt.Errorf("claude: cyber: %w", claude.ErrRefused), StopRefusal, "declined"},
		{fmt.Errorf("claude: max_tokens reached: %w", claude.ErrTruncated), StopMaxTokens, "cut off"},
	} {
		fake := &claude.Fake{Err: tc.err, Usage: sonnetUsage}
		res, err := (&Writer{Caller: fake}).WriteLead(context.Background(), writerInput(t, "d7"))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Attempts) != 2 || fake.Calls() != 2 {
			t.Fatalf("%s: attempts %d calls %d", tc.stop, len(res.Attempts), fake.Calls())
		}
		for _, a := range res.Attempts {
			if a.StopReason != tc.stop || a.Output != nil || a.CostMicros <= 0 || a.OK() {
				t.Fatalf("%s: attempt %+v (billed, failed, no output)", tc.stop, a)
			}
		}
		if !strings.HasPrefix(res.Fallback, "Draft refused: ") || !strings.Contains(res.Fallback, tc.fallback) {
			t.Fatalf("%s: fallback %q", tc.stop, res.Fallback)
		}
	}
}

func TestWriterBadAnswerIsAFailedAttempt(t *testing.T) {
	s := &scripted{replies: []scriptReply{
		{json: json.RawMessage(`{"steps":[],"talking_points":[],"extra":1}`)},
		{err: fmt.Errorf("claude: write: answer is not JSON: %w", ai.ErrBadAnswer)},
	}}
	res, err := (&Writer{Caller: s}).WriteLead(context.Background(), writerInput(t, "d7"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 2 || res.Attempts[0].Failures[0].Code != CodeSchema || res.Attempts[1].Failures[0].Code != CodeSchema {
		t.Fatalf("attempts %+v", res.Attempts)
	}
	if !strings.HasPrefix(res.Fallback, "Draft refused: G1: ") {
		t.Fatalf("fallback %q", res.Fallback)
	}
}

func TestWriterStopsOnCallerError(t *testing.T) {
	for _, e := range []error{ai.ErrSpendCap, ai.ErrUsageStore, ai.ErrUnavailable, claude.ErrBadRequest} {
		fake := &claude.Fake{Err: e}
		res, err := (&Writer{Caller: fake}).WriteLead(context.Background(), writerInput(t, "d7"))
		if !errors.Is(err, e) || len(res.Attempts) != 0 || fake.Calls() != 1 {
			t.Fatalf("%v: err %v attempts %d calls %d", e, err, len(res.Attempts), fake.Calls())
		}
	}
}

func TestWriterRefusesBadInput(t *testing.T) {
	fake := &claude.Fake{}
	w := &Writer{Caller: fake}
	in := writerInput(t, "d7")
	in.Examples = make([]WrittenDraft, MaxExamples+1)
	if _, err := w.WriteLead(context.Background(), in); err == nil {
		t.Fatal("three examples accepted")
	}
	in = writerInput(t, "d7")
	in.Rules.EmailSteps = nil
	if _, err := w.WriteLead(context.Background(), in); err == nil {
		t.Fatal("no email steps accepted")
	}
	if fake.Calls() != 0 {
		t.Fatal("called on bad input")
	}
}

func writerExamples(t *testing.T) []WrittenDraft {
	t.Helper()
	var out []WrittenDraft
	for _, name := range []string{"clean_d7", "clean_d10"} {
		d, err := DecodeWireDraft(writerFixture(t, name))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestWriterSystemPromptStable(t *testing.T) {
	s := &scripted{replies: []scriptReply{{json: writerFixture(t, "clean_d7")}, {json: writerFixture(t, "clean_d10")}}}
	w := &Writer{Caller: s}
	d7, d10 := writerInput(t, "d7"), writerInput(t, "d10")
	d10.LeadID = 43
	for _, in := range []WriteInput{d7, d10} {
		if _, err := w.WriteLead(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.reqs) < 2 || s.reqs[0].System != s.reqs[len(s.reqs)-1].System {
		t.Fatal("the system prompt differs between two leads of one campaign")
	}
	if s.reqs[0].Prompt == s.reqs[len(s.reqs)-1].Prompt {
		t.Fatal("two leads got the same user turn")
	}
	if strings.Contains(s.reqs[0].System, "Northfield") || strings.Contains(s.reqs[0].System, "eol_no_security") || strings.Contains(s.reqs[0].System, "202") {
		t.Fatal("the system prompt holds lead facts, pains or a date")
	}
	withEx := d7
	withEx.Examples = writerExamples(t)[:1]
	if SystemPrompt(withEx) == SystemPrompt(d7) {
		t.Fatal("examples do not change the system prompt")
	}
	other := withEx
	other.Examples = writerExamples(t)[1:]
	if SystemPrompt(withEx) == SystemPrompt(other) {
		t.Fatal("different examples give the same system prompt")
	}
	again := withEx
	again.Examples = writerExamples(t)[:1]
	if SystemPrompt(withEx) != SystemPrompt(again) {
		t.Fatal("not deterministic")
	}
	// The cache needs a long enough prefix: about 4 bytes a token, well past 512 tokens.
	if n := len(SystemPrompt(d7)); n < 4*700 {
		t.Fatalf("system prompt only %d bytes", n)
	}
}

func TestWriterColdStartTwin(t *testing.T) {
	cold := writerInput(t, "d7")
	warm := cold
	warm.Examples = writerExamples(t)
	a, b := SystemPrompt(cold), SystemPrompt(warm)
	const head = "# Examples\n"
	ia, ib := strings.Index(a, head), strings.Index(b, head)
	if ia < 0 || ib < 0 || a[:ia] != b[:ib] {
		t.Fatal("the prompts differ outside the examples block")
	}
	if a[ia:] == b[ib:] || !strings.Contains(b[ib:], "## Example 2") {
		t.Fatal("the examples block did not change")
	}
	if UserTurn(cold) != UserTurn(warm) {
		t.Fatal("examples leaked into the user turn")
	}
}

func TestWriterUserTurnFacts(t *testing.T) {
	in := writerInput(t, "d10")
	u := UserTurn(in)
	guards := 0
	for _, f := range in.Sheet.Facts {
		line := ""
		for _, l := range strings.Split(u, "\n") {
			if strings.HasPrefix(l, `{"id":"`+f.ID+`"`) {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("fact %s missing from the user turn", f.ID)
		}
		var got userFact
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatal(err)
		}
		if got.Value != f.Value || got.Label != f.Label || got.Group != f.Group || got.ContextOnly != f.EchoGuard {
			t.Fatalf("fact %s listed as %+v", f.ID, got)
		}
		if f.EchoGuard {
			guards++
		}
	}
	if guards == 0 || !strings.Contains(u, "context_only facts: context only, never quote or cite") {
		t.Fatal("guard facts not marked")
	}
	for _, want := range []string{"Email steps to write: 0, 1, 3, 4", "Talking points: 3", "- d10_eol: ", "(?i)migrat", "Language: en"} {
		if !strings.Contains(u, want) {
			t.Fatalf("user turn lacks %q", want)
		}
	}
}

// TestWriterContractDriftTail: a Claude email renders the same tail (footer: sender name,
// identity line, opt-out line) as the template email for the same lead.
func TestWriterContractDriftTail(t *testing.T) {
	in := writerInput(t, "d7")
	res, err := (&Writer{Caller: &claude.Fake{WriteAnswer: claude.WriteResult{JSON: writerFixture(t, "clean_d7")}}}).WriteLead(context.Background(), in)
	if err != nil || len(res.Emails) == 0 {
		t.Fatalf("err %v emails %d", err, len(res.Emails))
	}
	set, ok := in.Render.Cfg.Templates.Set(leadconfig.KindUnsupported, LangEnglish)
	if !ok || len(set.Steps) == 0 {
		t.Fatal("no demo template")
	}
	tmpl, err := Render(EmailTemplate(set.Steps[0], in.Render.Footer), in.Render.Slots)
	if err != nil {
		t.Fatal(err)
	}
	tail := func(d Draft) string { return d.Body[len(d.BodyAboveFooter):] }
	want := tail(tmpl)
	if !strings.Contains(want, in.Render.Slots.OptOutLine.Value) || !strings.Contains(want, in.Render.Slots.IdentityLine.Value) {
		t.Fatalf("template tail lacks the lines: %q", want)
	}
	for _, e := range res.Emails {
		if got := tail(e.Draft); got != want {
			t.Fatalf("step %d tail %q, template tail %q", e.Step, got, want)
		}
		if !strings.HasSuffix(e.Draft.BodyAboveFooter, "\n\n") {
			t.Fatalf("step %d: no blank line above the footer", e.Step)
		}
		if !slices.Equal(e.Draft.Used, []string{"SenderName", "IdentityLine", "OptOutLine", "Footer"}) {
			t.Fatalf("step %d uses %v", e.Step, e.Draft.Used)
		}
	}
}

// TestWriterModelTextIsNeverATemplate: braces in model text render literally, and G10
// refuses them as a placeholder.
func TestWriterModelTextIsNeverATemplate(t *testing.T) {
	d, err := DecodeWireDraft(writerFixture(t, "clean_d7"))
	if err != nil {
		t.Fatal(err)
	}
	d.Steps[1].Paragraphs[0].Text += " {{.OptOutLine}}"
	in := writerInput(t, "d7")
	emails, _, fails, err := renderWritten(d, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(emails[1].Draft.Body, "{{.OptOutLine}}") || len(fails) == 0 || fails[0].Token != RulePlaceholder {
		t.Fatalf("fails %v", fails)
	}
}

func TestWriterLogsNoDraftText(t *testing.T) {
	var buf bytes.Buffer
	s := &scripted{replies: []scriptReply{{json: writerFixture(t, "g3_invented_year")}, {json: writerFixture(t, "clean_d7")}}}
	// Drop the timestamp: its fractional seconds can contain "2019" by chance.
	noTime := func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	w := &Writer{Caller: s, Log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: noTime}))}
	in := writerInput(t, "d7")
	if _, err := w.WriteLead(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if strings.Count(logs, "claude draft attempt") != 2 || !strings.Contains(logs, `"codes":"G3"`) {
		t.Fatalf("logs: %s", logs)
	}
	d, _ := DecodeWrittenDraft(writerFixture(t, "g3_invented_year"))
	var secrets []string
	for _, st := range d.Steps {
		secrets = append(secrets, st.Subject)
		for _, p := range st.Paragraphs {
			secrets = append(secrets, p.Text)
		}
	}
	for _, f := range in.Sheet.Facts {
		if len(f.Value) >= 5 {
			secrets = append(secrets, f.Value)
		}
	}
	secrets = append(secrets, "2019", "Sam Carter", "Northfield")
	for _, sec := range secrets {
		if strings.Contains(logs, sec) {
			t.Fatalf("logs hold draft text or a fact: %q", sec)
		}
	}
}

// TestWriteThroughClientHTTP: through the real client, the system prompt is one cached
// block, the answer schema rides in output_config, and the user turn is the fact sheet.
func TestWriteThroughClientHTTP(t *testing.T) {
	answer := string(writerFixture(t, "clean_d7"))
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		content, _ := json.Marshal([]map[string]any{{"type": "text", "text": answer, "citations": []any{}}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5",
"content":%s,"stop_reason":"end_turn","stop_details":null,
"usage":{"input_tokens":800,"output_tokens":700,"cache_read_input_tokens":1600,"cache_creation_input_tokens":0,
"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}}`, content)
	}))
	defer srv.Close()
	c, err := claude.New(config.Secret("sk-ant-test-0000"), claude.WithBaseURL(srv.URL), claude.WithTimeout(5*time.Second), claude.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	in := writerInput(t, "d7")
	res, err := (&Writer{Caller: c}).WriteLead(context.Background(), in)
	if err != nil || res.Fallback != "" || len(res.Attempts) != 1 {
		t.Fatalf("err %v fallback %q attempts %d", err, res.Fallback, len(res.Attempts))
	}
	if a := res.Attempts[0]; a.Usage.CacheReadTokens != 1600 || a.CostMicros <= 0 || a.Model != claude.ModelSonnet {
		t.Fatalf("attempt %+v", a)
	}
	var req struct {
		Model  string `json:"model"`
		System []struct {
			Text         string `json:"text"`
			CacheControl struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
		OutputConfig struct {
			Format struct {
				Type   string         `json:"type"`
				Schema map[string]any `json:"schema"`
			} `json:"format"`
		} `json:"output_config"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != claude.ModelSonnet || len(req.System) != 1 || req.System[0].CacheControl.Type != "ephemeral" || req.System[0].Text != SystemPrompt(in) {
		t.Fatalf("system %+v", req.System)
	}
	want, _ := json.Marshal(WriterSchema(in.Rules))
	got, _ := json.Marshal(req.OutputConfig.Format.Schema)
	if req.OutputConfig.Format.Type != "json_schema" || string(got) != string(want) {
		t.Fatalf("output format %s %s", req.OutputConfig.Format.Type, got)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content[0].Text != UserTurn(in) {
		t.Fatal("the user turn is not the fact sheet turn")
	}
}

// TestWriterUserTurnAfterCall: for a call_first lead (AfterCall, emails 2 and 3 after the
// calls 0 and 1) the first email to write is step 2, the first email after a call; no step
// is the follow-up after a call attempt. The default sequence keeps its cold first email and
// its "tried to call" step. Both get the same system prompt.
func TestWriterUserTurnAfterCall(t *testing.T) {
	in := writerInput(t, "d7")
	in.Rules.EmailSteps, in.CallSteps, in.AfterCall = []int{2, 3}, []int{0, 1}, true
	u := UserTurn(in)
	if !strings.Contains(u, "- Step 2: the first email after a call") || strings.Contains(u, "after a call attempt") || strings.Contains(u, "Step 0") {
		t.Fatalf("call_first user turn:\n%s", u)
	}
	def := writerInput(t, "d7")
	def.Rules.EmailSteps, def.CallSteps = []int{0, 1, 3, 4}, []int{2}
	d := UserTurn(def)
	if strings.Contains(d, "after a call\n") || !strings.Contains(d, "- Step 3: the follow-up after a call attempt") {
		t.Fatalf("default user turn:\n%s", d)
	}
	if SystemPrompt(in) != SystemPrompt(def) {
		t.Fatal("call_first and default leads get different system prompts")
	}
}

// TestGroundFirstEmailIsLowestStep: the first email (G1's subject, G5's opener, the
// follow-ups' "Re: " subject) is the lowest step written: step 2 on call_first. The batch
// path through it is TestCallFirstDefaultSequenceTemplatesAndClaude.
func TestGroundFirstEmailIsLowestStep(t *testing.T) {
	d := WrittenDraft{Steps: []WrittenStep{{Step: 3}, {Step: 2}}}
	if got := firstStep(d); got != 2 {
		t.Fatalf("first step %d, want 2", got)
	}
	if got := firstStep(WrittenDraft{}); got != -1 {
		t.Fatalf("first step of nothing %d, want -1", got)
	}
}

// TestWriterRetryFitsLeadCap: the worst case the capped client reserves for the largest
// realistic drafting call, the retry, fits twice under the default per-lead cap of every
// purpose that drafts, so a retry is never refused after one attempt (each attempt costs at
// most its own reservation). Sizes are bytes, as claude.WorstWrite counts them: a system
// prompt of ~1,300 tokens, a user turn of ~1,500 and a retry block of ~1,500 (the previous
// answer and its failures), at about 4 bytes a token; the fixture's own text is used when
// larger. Found live on dev, 28 Oct 2026: at 4096 output tokens and a $0.08 cap the retry
// reserved $0.0870 and was refused, so every first-try failure became a template.
func TestWriterRetryFitsLeadCap(t *testing.T) {
	in := writerInput(t, "agency")
	pad := func(s string, n int) string {
		if len(s) >= n {
			return s
		}
		return s + strings.Repeat("x", n-len(s))
	}
	req := claude.WriteRequest{
		LeadID: in.LeadID, Model: claude.ModelSonnet,
		System: pad(SystemPrompt(in), 1_300*4), Prompt: pad(UserTurn(in), 1_500*4),
		Schema: WriterSchema(in.Rules), MaxTokens: DefaultWriterMaxTokens,
	}
	first, err := claude.WorstWrite(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Prompt += "\n\n" + strings.Repeat("x", 1_500*4)
	retry, err := claude.WorstWrite(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("worst case: first %d, retry %d micro-dollars", first, retry)
	for _, purpose := range []string{claude.PurposeDrafting, claude.PurposeDraftsEval} {
		_, lead, ok := config.DefaultClaudeCaps(purpose)
		if !ok {
			t.Fatalf("no default caps for %s", purpose)
		}
		if 2*retry > lead {
			t.Errorf("%s: two retry reservations cost %d, over the per-lead cap %d", purpose, 2*retry, lead)
		}
	}
}
