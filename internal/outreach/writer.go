package outreach

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// Writer defaults.
const (
	// DefaultWriterMaxTokens bounds one draft answer. Measured on dev (28 Oct 2026, Sonnet 5.5,
	// effort medium): a four-email sequence with talking points used 57 to 958 output tokens,
	// thinking included, so 3000 leaves about three times the largest seen. It also sets the
	// capped worst case per call (claude.WorstWrite reserves MaxTokens of output), which must
	// fit twice under the per-lead drafting cap so the one retry is never refused. A max_tokens
	// stop is a failed, billed attempt and is retried, so a rare long answer costs a retry,
	// never a wrong draft.
	DefaultWriterMaxTokens = 3000
	// MaxExamples is how many approved drafts the system prompt may carry.
	MaxExamples = 2
	// WriterAttempts is the first try plus the one retry.
	WriterAttempts = 2
)

// Stop reasons an Attempt records.
const (
	StopEndTurn   = "end_turn"
	StopRefusal   = "refusal"
	StopMaxTokens = "max_tokens"
)

// Caller is the drafting call the Writer makes: claude.Capped in production, claude.Client
// or claude.Fake elsewhere. A refusal is claude.ErrRefused and a max_tokens stop
// claude.ErrTruncated, each with the usage billed; text that is not JSON is ai.ErrBadAnswer.
type Caller interface {
	Write(ctx context.Context, r claude.WriteRequest) (claude.WriteResult, claude.Usage, error)
}

// Writer writes one lead's sequence with Claude and checks it: generate, Ground, render
// through the templates' own footer, CheckRendered (G10); on any refusal one retry that
// names each failure; then the grounded draft, or a fallback reason.
type Writer struct {
	Caller    Caller
	Model     string // claude.ModelSonnet when empty
	MaxTokens int    // DefaultWriterMaxTokens when 0
	Effort    string // the client's default (medium) when empty
	// Log gets one line per attempt with counts, codes, tokens and cost only: never draft
	// text, fact values or failure tokens. Nil logs nothing.
	Log *slog.Logger
}

// WriteRender is what the rendering tail needs, the same values the template path uses.
type WriteRender struct {
	Cfg      leadconfig.Config
	Footer   *template.Template // the country's footer template, as for EmailTemplate
	Slots    Slots              // the lead's slots (SenderName, IdentityLine, OptOutLine…)
	Greeting string             // a line above the first paragraph ("Hello,"); "" for none
}

// WriteInput is one lead's drafting job, all plain values: this package never reads a
// campaign or the store.
type WriteInput struct {
	LeadID   int64
	Campaign string // the campaign's name, as the system prompt states it
	Lang     string // the language to write in ("en")
	// Sheet is the lead's fact sheet, the only lead data the model sees. It holds none of the
	// org's own text today: sheetInputForLead (cmd/leadhound) puts no description, social
	// profile or page text on it, so the G6 echo check is left unwired (no echo_guard facts,
	// GroundRules.EchoTexts empty) because there is nothing for a draft to echo. Org text
	// added later must come in as SheetInput.OrgTexts, which BuildFactSheet marks echo_guard
	// (context only, never cited, and G6 holds the draft to it); TestNoOrgTextReachesWriter
	// (cmd/leadhound) fails on org text that reaches the writer any other way.
	Sheet FactSheet
	// Rules are the lead's grounding rules. Rules.EmailSteps are the steps to write and must
	// be set. The system prompt reads only the campaign-wide fields (email steps, word
	// limits, banned words, links, "!", allowed names, research openers, inflation words,
	// echo length, the call step); the lead's own Pains and Forbid go in the user turn.
	Rules GroundRules
	// CallSteps are the sequence's call steps, so the prompt can say which email follows a
	// call ("I tried to call").
	CallSteps []int
	// AfterCall says the first email goes only after a phone call where they agreed to get
	// it (the call_first sequence): it thanks them for the call and brings the details
	// promised, and no email says "I tried to call".
	AfterCall bool
	// Examples are at most MaxExamples approved drafts (golden exemplars), in the order the
	// caller picks; they sit in the system prompt.
	Examples []WrittenDraft
	Render   WriteRender
}

// Attempt is one billed (or failed) generation, as a draft_generation row stores it.
type Attempt struct {
	Output     json.RawMessage // the answer as a WrittenDraft's JSON (the stored shape); the raw answer when it does not decode; nil when there was none
	Raw        json.RawMessage // the model's answer as sent, in the wire shape (for the retry turn); not stored
	Failures   []Failure       // nil: grounded and rendered clean
	Usage      claude.Usage
	CostMicros int64
	Model      string
	StopReason string // StopEndTurn, StopRefusal or StopMaxTokens
}

// OK says the attempt produced a grounded draft.
func (a Attempt) OK() bool {
	return a.StopReason == StopEndTurn && a.Output != nil && len(a.Failures) == 0
}

// StepDraft is one rendered email of the written sequence.
type StepDraft struct {
	Step  int
	Draft Draft
}

// WriteResult is the outcome for one lead. Exactly one of Emails or Fallback is set when
// WriteLead returns no error.
type WriteResult struct {
	FactSHA256 string
	System     string // the system prompt sent (stable per campaign)
	Attempts   []Attempt
	Written    *WrittenDraft // the grounded draft, cites included
	Emails     []StepDraft   // rendered through the footer and checked (G10)
	CallCard   *Draft        // the talking points as a call card, when the sequence has a call
	// Fallback is why there is no Claude draft, in plain words ("Draft refused: G3 '2019'
	// is not in a cited fact"): the caller falls back to templates or drops the lead.
	Fallback string
}

// WriteLead drafts one lead: generate → check → (one retry naming the failures) → check →
// the rendered draft or a Fallback. An error means the job could not run (bad input, a
// cap, the usage store, the provider unavailable, a render of the operator's own footer
// failing); the result then holds the attempts made so far.
func (w *Writer) WriteLead(ctx context.Context, in WriteInput) (WriteResult, error) {
	res := WriteResult{FactSHA256: in.Sheet.SHA256()}
	if w.Caller == nil {
		return res, errors.New("outreach: writer: no caller")
	}
	if len(in.Rules.EmailSteps) == 0 {
		return res, errors.New("outreach: writer: no email steps to write")
	}
	if len(in.Examples) > MaxExamples {
		return res, fmt.Errorf("outreach: writer: %d examples, at most %d", len(in.Examples), MaxExamples)
	}
	if in.Render.Footer == nil {
		return res, errors.New("outreach: writer: no footer template")
	}
	res.System = SystemPrompt(in)
	user := UserTurn(in)
	model := cmp.Or(w.Model, claude.ModelSonnet)
	maxTokens := w.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultWriterMaxTokens
	}

	prompt := user
	for n := 1; n <= WriterAttempts; n++ {
		req := claude.WriteRequest{
			LeadID: in.LeadID, Model: model, System: res.System, Prompt: prompt,
			Schema: WriterSchema(in.Rules), MaxTokens: maxTokens, Effort: w.Effort,
		}
		out, u, err := w.Caller.Write(ctx, req)
		a := Attempt{Usage: u, Model: cmp.Or(u.Model, model), StopReason: StopEndTurn}
		switch {
		case err == nil:
		case errors.Is(err, claude.ErrRefused):
			a.StopReason = StopRefusal
		case errors.Is(err, claude.ErrTruncated):
			a.StopReason = StopMaxTokens
		case errors.Is(err, ai.ErrBadAnswer):
			a.Failures = []Failure{{Code: CodeSchema, Step: StepTalkingPoints, Para: ParaSubject, Msg: "the answer is not JSON"}}
		default:
			return res, fmt.Errorf("outreach: writer: lead %d attempt %d: %w", in.LeadID, n, err)
		}
		if a.CostMicros, err = attemptCost(u); err != nil {
			return res, fmt.Errorf("outreach: writer: lead %d attempt %d: %w", in.LeadID, n, err)
		}
		var d WrittenDraft
		if out.JSON != nil && a.StopReason == StopEndTurn && a.Failures == nil {
			a.Raw, a.Output = slices.Clone(out.JSON), slices.Clone(out.JSON)
			if d, err = DecodeWireDraft(out.JSON); err != nil {
				a.Failures = []Failure{{Code: CodeSchema, Step: StepTalkingPoints, Para: ParaSubject, Msg: "the answer does not follow the schema"}}
			} else if a.Output, err = json.Marshal(d); err != nil {
				return res, fmt.Errorf("outreach: writer: lead %d: %w", in.LeadID, err)
			} else if a.Failures = Ground(d, in.Sheet, in.Rules); len(a.Failures) == 0 {
				emails, card, fails, err := renderWritten(d, in)
				if err != nil {
					res.Attempts = append(res.Attempts, a)
					return res, fmt.Errorf("outreach: writer: lead %d: %w", in.LeadID, err)
				}
				if a.Failures = fails; len(fails) == 0 {
					res.Written, res.Emails, res.CallCard = &d, emails, card
				}
			}
		}
		res.Attempts = append(res.Attempts, a)
		w.logAttempt(in.LeadID, n, a)
		if a.OK() {
			return res, nil
		}
		prompt = user + "\n\n" + retryTurn(a)
	}
	res.Fallback = fallbackReason(res.Attempts[len(res.Attempts)-1])
	return res, nil
}

func attemptCost(u claude.Usage) (int64, error) {
	if u.Model == "" || u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens+u.WebSearches == 0 {
		return 0, nil
	}
	return claude.Cost(u)
}

func (w *Writer) logAttempt(leadID int64, n int, a Attempt) {
	if w.Log == nil {
		return
	}
	codes := make([]string, 0, len(a.Failures))
	for _, f := range a.Failures {
		if !slices.Contains(codes, f.Code) {
			codes = append(codes, f.Code)
		}
	}
	w.Log.Info("outreach: claude draft attempt",
		"lead_id", leadID, "attempt", n, "ok", a.OK(), "stop", a.StopReason,
		"failures", len(a.Failures), "codes", strings.Join(codes, ","),
		"input_tokens", a.Usage.InputTokens, "output_tokens", a.Usage.OutputTokens,
		"cache_read_tokens", a.Usage.CacheReadTokens, "cache_write_tokens", a.Usage.CacheWriteTokens,
		"cost_micros", a.CostMicros, "model", a.Model)
}

// fallbackReason says in plain words why the last attempt gave no draft.
func fallbackReason(a Attempt) string {
	switch a.StopReason {
	case StopRefusal:
		return "Draft refused: the model declined to write it"
	case StopMaxTokens:
		return "Draft refused: the answer was cut off at the length limit"
	}
	if len(a.Failures) == 0 {
		return "Draft refused: no usable answer"
	}
	f := a.Failures[0]
	code := f.Code
	var s string
	switch {
	case f.Token != "" && strings.HasPrefix(f.Msg, f.Token+" "):
		s = fmt.Sprintf("Draft refused: %s '%s' %s", code, f.Token, strings.TrimPrefix(f.Msg, f.Token+" "))
	case f.Token != "":
		s = fmt.Sprintf("Draft refused: %s '%s': %s", code, f.Token, f.Msg)
	default:
		s = fmt.Sprintf("Draft refused: %s: %s", code, f.Msg)
	}
	if n := len(a.Failures) - 1; n > 0 {
		s += fmt.Sprintf(" (and %d more)", n)
	}
	return s
}

// retryTurn is what the retry adds below the original user turn: the previous answer and
// every failure with its code and token.
func retryTurn(a Attempt) string {
	var b strings.Builder
	switch a.StopReason {
	case StopRefusal:
		b.WriteString("Your previous attempt stopped without an answer (stop reason: refusal). These are ordinary business emails built only from the stored facts above. Write the whole answer again.")
		return b.String()
	case StopMaxTokens:
		b.WriteString("Your previous attempt was cut off at the length limit (stop reason: max_tokens). Write the whole answer again, shorter: keep every email inside its word limit.")
		return b.String()
	}
	b.WriteString("Your previous answer was refused by the deterministic check. Fix every failure listed below and write the whole answer again, as JSON in the same schema.")
	if prev := cmp.Or(string(a.Raw), string(a.Output)); prev != "" {
		b.WriteString("\n\nYour previous answer:\n")
		b.WriteString(prev)
	}
	b.WriteString("\n\nFailures (code, where, offending token: reason):")
	for _, f := range a.Failures {
		b.WriteString("\n- ")
		b.WriteString(f.String())
	}
	return b.String()
}

// --- rendering: the templates' own tail ---

// claudeDelims are the delimiters of the templates built from model text. They hold a NUL,
// which is refused in model text, so nothing the model writes can become an action: its
// text renders literally and only the footer action runs.
const (
	claudeDelimL = "\x00{"
	claudeDelimR = "}\x00"
)

// renderWritten renders each email step through the same Render and footer as a template
// draft, and runs CheckRendered (G10) on each. Model text becomes the template's literal
// text, so FixedText holds the grounded paragraphs (G3 already traced their numbers).
// Failures are the model's; an error is the operator's footer or slots failing to render.
func renderWritten(d WrittenDraft, in WriteInput) ([]StepDraft, *Draft, []Failure, error) {
	var (
		emails []StepDraft
		fails  []Failure
		first  string
	)
	firstAt := firstStep(d)
	for _, st := range d.Steps {
		if st.Step == firstAt {
			first = strings.TrimSpace(st.Subject)
		}
	}
	steps := slices.Clone(d.Steps)
	slices.SortFunc(steps, func(a, b WrittenStep) int { return a.Step - b.Step })
	for _, st := range steps {
		subject := strings.TrimSpace(st.Subject)
		if subject == "" && first != "" {
			subject = "Re: " + first
		}
		paras := make([]string, 0, len(st.Paragraphs)+1)
		if g := strings.TrimSpace(in.Render.Greeting); g != "" {
			paras = append(paras, g)
		}
		for _, p := range st.Paragraphs {
			paras = append(paras, strings.TrimSpace(p.Text))
		}
		text := subject + strings.Join(paras, "")
		if strings.ContainsRune(text, 0) || strings.ContainsAny(subject, "\r\n") {
			fails = append(fails, Failure{Code: CodeSchema, Step: st.Step, Para: ParaSubject, Msg: "the text holds a control character or a line break in the subject"})
			continue
		}
		tm, err := literalTemplate(DraftEmail, subject, strings.Join(paras, "\n\n")+"\n\n"+claudeDelimL+".Footer"+claudeDelimR+"\n", in.Render.Footer)
		if err != nil {
			return nil, nil, nil, err
		}
		dr, err := Render(tm, in.Render.Slots)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("render step %d: %w", st.Step, err)
		}
		fails = append(fails, CheckRendered(dr, st.Step, in.Render.Cfg, in.Rules.StepRules)...)
		emails = append(emails, StepDraft{Step: st.Step, Draft: dr})
	}
	var card *Draft
	if in.Rules.CallStep && len(d.TalkingPoints) > 0 {
		lines := make([]string, len(d.TalkingPoints))
		for i, tp := range d.TalkingPoints {
			lines[i] = strings.Join(strings.Fields(tp.Text), " ")
		}
		body := strings.Join(lines, "\n")
		if strings.ContainsRune(body, 0) {
			fails = append(fails, Failure{Code: CodeSchema, Step: StepTalkingPoints, Para: ParaSubject, Msg: "a talking point holds a control character"})
		} else {
			tm, err := literalTemplate(DraftForm, "", body, nil)
			if err != nil {
				return nil, nil, nil, err
			}
			dr, err := Render(tm, in.Render.Slots)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("render call card: %w", err)
			}
			for _, f := range CheckRendered(dr, 0, in.Render.Cfg, StepRules{Forbid: in.Rules.Forbid, BannedWords: in.Rules.BannedWords, Links: in.Rules.Links}) {
				f.Step = StepTalkingPoints
				fails = append(fails, f)
			}
			card = &dr
		}
	}
	return emails, card, fails, nil
}

// literalTemplate builds a Template whose subject and body are the given text, read with
// the NUL delimiters so only an explicit footer action is one.
func literalTemplate(kind DraftKind, subject, body string, footer *template.Template) (Template, error) {
	parse := func(name, text string) (*template.Template, error) {
		t, err := template.New(name).Delims(claudeDelimL, claudeDelimR).Option("missingkey=error").Parse(text)
		if err != nil {
			return nil, fmt.Errorf("outreach: writer: %s: %w", name, err)
		}
		return t, nil
	}
	b, err := parse("claude-body", body)
	if err != nil {
		return Template{}, err
	}
	t := Template{Kind: kind, Body: b, Footer: footer}
	if kind == DraftEmail {
		if t.Subject, err = parse("claude-subject", subject); err != nil {
			return Template{}, err
		}
	}
	return t, nil
}

// --- the prompt ---

// SystemPrompt is the frozen, cached system prompt: the same bytes for every lead of a
// campaign (no timestamps, lists sorted), changing only with the campaign's rules or its
// examples. It reads only the campaign-wide fields of in.Rules (allowed names, inflation
// words, banned words, echo length, "!" and links), in.Campaign and in.Examples: anything
// that varies by lead, kind, country or phone (the steps, their roles and word limits, the
// talking points, the pains, the forbidden patterns) is the user turn's, so one campaign is
// one cache entry.
func SystemPrompt(in WriteInput) string {
	r := in.Rules
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	line("You write short cold emails, and when asked the talking points for one phone call, for one sender. You write as the sender, in the first person, to one organisation at a time.")
	if in.Campaign != "" {
		line("The campaign: %s.", in.Campaign)
	}
	line("")
	line("# Facts and cites")
	line("The user turn holds the fact sheet for one lead: lead facts (ids F1, F2 …), sender facts (S1 …) and offer facts (O1 …). Everything you claim rests on those facts and nothing else.")
	line("- Every paragraph and talking point lists in \"cites\" the ids of the facts it rests on.")
	line("- Write no number, year, date, version, URL, domain, email address, phone number, money amount or percentage unless it appears in the value of a fact that the same paragraph cites.")
	line("- A sentence about yourself (I, me, my, we, our) that holds a number must sit in a paragraph that cites a sender fact.")
	line("- Name no person, company, product, place or brand unless it appears in a fact the paragraph cites or in these allowed names: %s.", listOr(sortedClone(r.AllowedNames), "none"))
	line("- If you use the organisation's name, write it exactly as its fact gives it, every time.")
	line("- Facts marked context_only are the organisation's own words, for your understanding only: never cite them, never quote them, and never repeat %d or more of their words in a row.", echoN(r.EchoWords))
	line("- A paragraph about the reader's situation names the pain it rewords in pain_id, and the pain must be one the user turn lists for this lead. Reword the pain; never add a claim to it, and never invent a pain. A paragraph that cites a lead fact other than the organisation's name, country or domain must name its pain. Every other paragraph has pain_id \"\".")
	line("- The call to action is an offer fact: use it word for word and cite it.")
	line("")
	line("# Voice")
	line("- Open the first email with who you are and why you write. Its first paragraph cites sender facts only and holds nothing about the organisation: not its name, domain, site, version or any other lead fact.")
	line("- Never open with research or tell them what they already know about themselves: no \"I noticed\", \"I saw\", \"I came across\", \"I was looking at\", \"your site\" or \"your website\" in that first paragraph.")
	line("- Never echo the organisation's own site or description back to them. Show you understand their situation in your own words.")
	line("- One true proof line: if you use the proof line, use it once, word for word, in a paragraph that cites it. Never inflate it, never add another claim of experience, and never use boast words: %s.", listOr(r.InflationWords, strings.Join(DefaultInflationWords, ", ")))
	line("- A move from Drupal 10 to Drupal 11 is an update, never a migration. The user turn lists any patterns this lead's kind forbids; never write text that matches them.")
	line("- Short, plain sentences and plain words. No jargon, no hype, no flattery.")
	if r.NoExclamation {
		line("- No exclamation marks anywhere.")
	}
	if r.NoLinksInStep0 {
		line("- No link and no web address in the first email.")
	}
	if banned := sortedClone(append(slices.Clone(r.ConfigBanned), r.BannedWords...)); len(banned) > 0 {
		line("- Never use these words: %s.", strings.Join(banned, ", "))
	}
	line("")
	line("# Structure")
	line("The user turn lists the steps to write for this lead, each with its role and word limit. The roles:")
	line("- The first email: (1) who you are and who you work with, citing sender facts only; (2) three or four pains the reader sees, one paragraph each, each with its pain_id; (3) the proof line, if it fits, word for word; (4) the free report from the offer facts, which is theirs to keep; (5) the call to action, word for word.")
	line("- The first email after a call: sent after a phone call where they agreed to get it: (1) thank them for the call, say who you are, citing sender facts only, and that as promised here are the details; (2) up to three pains the reader sees, one paragraph each, each with its pain_id; (3) the proof line, if it fits, word for word; (4) the free report from the offer facts, which is theirs to keep. When the first email is this one, no email says you tried to call.")
	line("- The follow-up after a call attempt: say you tried to call and did not get through, then a short reminder of the offer.")
	line("- A follow-up: a short follow-up, shorter than the first email; one pain or the offer, not both at length.")
	line("- The last email: close the loop politely and briefly, with no pressure; the offer stays open if they want it later.")
	line("- Follow-ups are shorter than the first email. Word limits count the text you write.")
	line("- Every email has at least one paragraph with text. Write every step the user turn lists, and only those.")
	line("- Subjects: at most %d characters, no exclamation mark, and only claims the facts back. The first email needs a subject; it may use the organisation's name exactly as its fact gives it. For a follow-up write \"\" and the code uses \"Re: \" and the first subject.", MaxSubjectRunes)
	line("- Write no greeting, no sign-off, no name under the email, no identity line and no opt-out line: the code adds the greeting, the signature, the identity line and the opt-out line.")
	line("- Talking points: when the user turn asks for them, exactly %d, one short line each, for you to say on the phone, each citing its facts. When it asks for none, write none.", TalkingPoints)
	line("")
	line("# Output")
	line("Answer with JSON only, in the schema given: in \"steps\" one entry per step the user turn asks for, keyed step_<number> (step_0, step_3), with its subject and paragraphs; each paragraph with its text, cites and pain_id. Talking points, when asked for, are point_1, point_2 and point_3.")
	line("")
	line("# Examples")
	if len(in.Examples) == 0 {
		line("No approved examples yet.")
	} else {
		line("Approved drafts for other leads. Copy their voice and shape, never their facts: their cites point at another lead's sheet.")
		for i, ex := range in.Examples {
			line("")
			line("## Example %d", i+1)
			writeExample(&b, ex)
		}
	}
	return b.String()
}

func writeExample(b *strings.Builder, d WrittenDraft) {
	steps := slices.Clone(d.Steps)
	slices.SortFunc(steps, func(a, b WrittenStep) int { return a.Step - b.Step })
	for _, st := range steps {
		fmt.Fprintf(b, "Step %d. Subject: %s\n", st.Step, st.Subject)
		for _, p := range st.Paragraphs {
			fmt.Fprintf(b, "%s [cites %s", p.Text, strings.Join(p.Cites, ", "))
			if p.PainID != "" {
				fmt.Fprintf(b, "; pain %s", p.PainID)
			}
			b.WriteString("]\n")
		}
	}
	if len(d.TalkingPoints) > 0 {
		b.WriteString("Talking points:\n")
		for _, tp := range d.TalkingPoints {
			fmt.Fprintf(b, "- %s [cites %s]\n", tp.Text, strings.Join(tp.Cites, ", "))
		}
	}
}

func followsCall(step int, calls []int) bool {
	return slices.Contains(calls, step-1)
}

func echoN(n int) int {
	if n > 0 {
		return n
	}
	return DefaultEchoWords
}

func sortedClone(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func listOr(list []string, none string) string {
	if len(list) == 0 {
		return none
	}
	return strings.Join(list, ", ")
}

// userFact is one fact as the user turn lists it.
type userFact struct {
	ID          string    `json:"id"`
	Group       FactGroup `json:"group"`
	Label       string    `json:"label"`
	Value       string    `json:"value"`
	ContextOnly bool      `json:"context_only,omitempty"`
}

// UserTurn is the lead's turn: the language, the pains and forbidden patterns this lead's
// signals and kind give, the fact sheet in sheet order (echo_guard facts marked context
// only), each step to write with its role and word limit, and, last, the plain list of what
// to write: every step, the subject, the talking points.
func UserTurn(in WriteInput) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	steps := slices.Clone(in.Rules.EmailSteps)
	slices.Sort(steps)
	steps = slices.Compact(steps)
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = strconv.Itoa(s)
	}
	line("Write the emails for this lead.")
	line("Language: %s", cmp.Or(in.Lang, LangEnglish))
	line("Email steps to write: %s", strings.Join(parts, ", "))
	if in.Rules.CallStep {
		line("Talking points: %d (the sequence has a call step)", TalkingPoints)
	} else {
		line("Talking points: none")
	}
	line("")
	line("Pains this lead's signals allow (pain_id: text):")
	if len(in.Rules.Pains) == 0 {
		line("none: write no paragraph about the reader's situation")
	}
	for _, p := range in.Rules.Pains {
		line("- %s: %s", p.ID, p.Text)
	}
	if len(in.Rules.Forbid) > 0 {
		line("")
		line("Patterns this lead's kind forbids (Go regular expressions; never write text that matches):")
		for _, re := range in.Rules.Forbid {
			line("- %s", re.String())
		}
	}
	line("")
	line("Fact sheet (one JSON object per fact; context_only facts: context only, never quote or cite):")
	for _, f := range in.Sheet.Facts {
		j, err := json.Marshal(userFact{ID: f.ID, Group: f.Group, Label: f.Label, Value: f.Value, ContextOnly: f.EchoGuard})
		if err != nil { // strings and a bool only: cannot fail
			panic(fmt.Sprintf("outreach: writer: fact: %v", err))
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	line("")
	line("The steps, in order:")
	for i, s := range steps {
		words := ""
		if most, ok := in.Rules.MaxWords[s]; ok {
			words = fmt.Sprintf(", at most %d words", most)
		}
		line("- Step %d: %s%s.", s, stepRole(i, len(steps), s, in), words)
	}
	line("")
	first := ""
	if len(steps) > 0 {
		first = fmt.Sprintf(" Step %d needs a subject; every other step's subject is \"\".", steps[0])
	}
	tps := "Write no talking points."
	if in.Rules.CallStep {
		tps = fmt.Sprintf("Write exactly %d talking points.", TalkingPoints)
	}
	line("Write all of these steps: %s.%s Every step needs at least one paragraph. %s", strings.Join(parts, ", "), first, tps)
	return b.String()
}

// stepRole names the role (as the system prompt describes it) of step s, the i-th of n steps
// to write.
func stepRole(i, n, s int, in WriteInput) string {
	switch {
	case i == 0 && in.AfterCall:
		return "the first email after a call"
	case i == 0:
		return "the first email"
	case i == n-1:
		return "the last email"
	case !in.AfterCall && followsCall(s, in.CallSteps):
		return "the follow-up after a call attempt"
	default:
		return "a follow-up"
	}
}
