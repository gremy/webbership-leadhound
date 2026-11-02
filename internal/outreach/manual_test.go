package outreach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// manualFixture is one file of testdata/manual. They live apart from testdata/ground because
// TestGroundFixtures reads every file there as an email draft.
type manualFixture struct {
	name    string
	Lead    string        `json:"lead"`
	Channel string        `json:"channel"`
	Want    string        `json:"want"`
	Why     string        `json:"why"`
	Draft   ManualWritten `json:"draft"`
}

func manualFixtures(t *testing.T) map[string]manualFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "manual", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no manual fixtures: %v", err)
	}
	out := map[string]manualFixture{}
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // test fixture path from a fixed glob
		if err != nil {
			t.Fatal(err)
		}
		var f manualFixture
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		f.name = strings.TrimSuffix(filepath.Base(p), ".json")
		out[f.name] = f
	}
	return out
}

func manualInput(t *testing.T, lead, channel string) ManualInput {
	t.Helper()
	return ManualInput{WriteInput: writerInput(t, lead), Channel: channel}
}

// manualAnswer is a fixture's draft as the model answers it.
func manualAnswer(t *testing.T, name string) json.RawMessage {
	t.Helper()
	f, ok := manualFixtures(t)[name]
	if !ok {
		t.Fatalf("no manual fixture %q", name)
	}
	b, err := json.Marshal(f.Draft)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestManualFixtures: each adversarial text is refused with exactly its code; clean ones pass
// the check and the rendered check.
func TestManualFixtures(t *testing.T) {
	for name, f := range manualFixtures(t) {
		t.Run(name, func(t *testing.T) {
			in := manualInput(t, f.Lead, f.Channel)
			got := checkManual(f.Draft, in)
			if f.Want != "" {
				if codes := failureCodes(got); len(codes) != 1 || codes[0] != f.Want {
					t.Fatalf("%s: want exactly %s, got %v", f.Why, f.Want, got)
				}
				return
			}
			if len(got) > 0 {
				t.Fatalf("%s: clean text refused: %v", f.Why, got)
			}
			if _, fails, err := renderManual(f.Draft, in); err != nil || len(fails) > 0 {
				t.Fatalf("%s: rendered: %v, %v", f.Why, fails, err)
			}
		})
	}
}

func TestWriteManualLinkedIn(t *testing.T) {
	fake := &claude.Fake{WriteAnswer: claude.WriteResult{JSON: manualAnswer(t, "m0_clean_linkedin")}}
	res, err := (&Writer{Caller: fake}).WriteManual(context.Background(), manualInput(t, "d7", leadconfig.ChannelLinkedIn))
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "" || len(res.Attempts) != 1 || !res.Attempts[0].OK() || fake.Calls() != 1 || res.Written == nil || len(res.Drafts) != 2 {
		t.Fatalf("result %+v, calls %d", res, fake.Calls())
	}
	if res.FactSHA256 != groundSheet(t, "d7").SHA256() {
		t.Fatalf("hash %s", res.FactSHA256)
	}
	note, msg := res.Drafts[0], res.Drafts[1]
	if note.Step != 0 || note.Part != PartNote || msg.Step != 1 || msg.Part != PartMessage {
		t.Fatalf("parts %+v %+v", note, msg)
	}
	for _, d := range res.Drafts {
		// slots.writer is what the lead's timeline reads: its only other value is "template".
		if d.Writer != "claude" {
			t.Fatalf("%s: writer %q", d.Part, d.Writer)
		}
		if d.Draft.Kind != DraftLinkedIn || d.Draft.Subject != "" || d.Draft.Used != nil {
			t.Fatalf("%s: draft %+v", d.Part, d.Draft)
		}
		if len(d.Paragraphs) == 0 || len(d.Paragraphs[0].Cites) == 0 {
			t.Fatalf("%s: no cites for the caller to record", d.Part)
		}
		for _, added := range []string{"Hello,", "Demo Sender", "Demo Ltd"} {
			if strings.Contains(d.Draft.Body, added) {
				t.Fatalf("%s: a LinkedIn text got %q:\n%s", d.Part, added, d.Draft.Body)
			}
		}
	}
	if want := "I'm Sam Carter, a Drupal developer who works with organisations and agencies that still run older Drupal sites.\n"; note.Draft.Body != want {
		t.Fatalf("note body %q", note.Draft.Body)
	}
	if n := len(strings.Fields(msg.Draft.Body)); n < ManualMessageMinWords || n > ManualMessageMaxWords {
		t.Fatalf("the clean message is %d words", n)
	}
	// The stored output reads back through DecodeManual.
	back, err := DecodeManual(res.Attempts[0].Output)
	if err != nil || back.Note == nil || back.Message == nil {
		t.Fatalf("stored output %s: %v", res.Attempts[0].Output, err)
	}
}

func TestWriteManualFormAddsIdentity(t *testing.T) {
	in := manualInput(t, "d7", leadconfig.ChannelForm)
	res, err := (&Writer{Caller: &claude.Fake{WriteAnswer: claude.WriteResult{JSON: manualAnswer(t, "m0_clean_form")}}}).WriteManual(context.Background(), in)
	if err != nil || res.Fallback != "" || len(res.Drafts) != 1 {
		t.Fatalf("err %v result %+v", err, res)
	}
	d := res.Drafts[0]
	if d.Step != 0 || d.Part != PartMessage || d.Writer != "claude" || d.Draft.Kind != DraftForm {
		t.Fatalf("draft %+v", d)
	}
	wantTail := "\n\n" + in.Render.Slots.SenderName.Value + ", " + in.Render.Slots.IdentityLine.Value + "\n"
	if !strings.HasPrefix(d.Draft.Body, "Hello,\n\n") || !strings.HasSuffix(d.Draft.Body, wantTail) {
		t.Fatalf("form body:\n%s", d.Draft.Body)
	}
	if !slices.Equal(d.Draft.Used, []string{"SenderName", "IdentityLine"}) {
		t.Fatalf("used %v", d.Draft.Used)
	}
	if strings.Contains(d.Draft.Body, in.Render.Slots.OptOutLine.Value) {
		t.Fatal("a form message has no opt-out line, as the form templates have none")
	}
}

// TestWriteManualFormLinks: a link in a form message passes only as a link the campaign allows: a
// cited fact holding one the render check does not know is G10, and Rules.Links lets it through.
func TestWriteManualFormLinks(t *testing.T) {
	in := manualInput(t, "d7", leadconfig.ChannelForm)
	in.Render.Cfg.Sender.SiteURL = "https://other.example/"
	f := manualFixtures(t)["m0_clean_form_allowed_link"]
	if fails := checkManual(f.Draft, in); len(fails) > 0 {
		t.Fatalf("the check refused: %v", fails)
	}
	_, fails, err := renderManual(f.Draft, in)
	if err != nil || len(fails) != 1 || fails[0].Code != CodeRendered || fails[0].Token != RuleURL {
		t.Fatalf("fails %v err %v", fails, err)
	}
	in.Rules.Links = []string{"https://sender.example/"}
	if _, fails, err := renderManual(f.Draft, in); err != nil || len(fails) > 0 {
		t.Fatalf("an allowed link refused: %v %v", fails, err)
	}
	// The email's "no link in the first email" is no rule of a form message.
	in = manualInput(t, "d7", leadconfig.ChannelForm)
	if !in.Rules.NoLinksInStep0 {
		t.Fatal("the fixture rules should refuse links in an email's step 0")
	}
	if fails := checkManual(f.Draft, in); len(fails) > 0 {
		t.Fatalf("refused under the email's link rule: %v", fails)
	}
}

func TestManualNoteLimit(t *testing.T) {
	note := func(n int) ManualWritten {
		text := "I'm Sam Carter, a Drupal developer."
		text += strings.Repeat("é", n-len(text))
		m := manualFixtures(t)["m0_clean_linkedin"].Draft
		m.Note = &ManualPart{Paragraphs: []Paragraph{{Text: text, Cites: []string{"S1", "S2"}}}}
		return m
	}
	for _, tc := range []struct {
		noteMax, runes int
		ok             bool
	}{
		{0, 200, true}, {0, 201, false},
		{300, 300, true}, {300, 301, false},
		{50, 50, true}, {50, 51, false},
	} {
		in := manualInput(t, "d7", leadconfig.ChannelLinkedIn)
		in.NoteMax = tc.noteMax
		m := note(tc.runes)
		fails := checkManual(m, in)
		if tc.ok != (len(fails) == 0) {
			t.Errorf("max %d, %d runes: %v", tc.noteMax, tc.runes, fails)
			continue
		}
		if !tc.ok {
			if len(fails) != 1 || fails[0].Code != CodeCopy || fails[0].Step != 0 || fails[0].Token != fmt.Sprint(tc.runes) {
				t.Errorf("max %d, %d runes: %v", tc.noteMax, tc.runes, fails)
			}
			continue
		}
		drafts, rfails, err := renderManual(m, in)
		if err != nil || len(rfails) > 0 || NoteRunes(drafts[0].Draft.Body) != tc.runes {
			t.Errorf("rendered length differs from the checked one: %v %v %d", rfails, err, NoteRunes(drafts[0].Draft.Body))
		}
	}
}

// TestManualWordLimits: copy.max_words never reaches the note or the message after they accept,
// which have their own limits; the form message keeps the first email's.
func TestManualWordLimits(t *testing.T) {
	li := manualInput(t, "d7", leadconfig.ChannelLinkedIn)
	li.Rules.MaxWords = map[int]int{0: 3, 1: 3}
	clean := manualFixtures(t)["m0_clean_linkedin"].Draft
	if fails := checkManual(clean, li); len(fails) > 0 {
		t.Fatalf("the email's word limits reached a LinkedIn text: %v", fails)
	}
	long := clean
	long.Message = &ManualPart{Paragraphs: slices.Concat(clean.Message.Paragraphs,
		[]Paragraph{{Text: strings.Repeat("I can help ", 10) + "here.", Cites: []string{"S2"}}})}
	if fails := checkManual(long, li); len(fails) != 1 || fails[0].Code != CodeCopy || fails[0].Step != 1 {
		t.Fatalf("a message over %d words: %v", ManualMessageMaxWords, fails)
	}
	form := manualInput(t, "d7", leadconfig.ChannelForm)
	f := manualFixtures(t)["m0_clean_form"].Draft
	form.Rules.MaxWords = map[int]int{0: 5}
	if fails := checkManual(f, form); len(fails) != 1 || fails[0].Code != CodeCopy {
		t.Fatalf("a form message over the first email's limit: %v", fails)
	}
	form.Rules.EmailSteps, form.Rules.MaxWords = []int{2, 3}, map[int]int{0: 5, 2: 500}
	if fails := checkManual(f, form); len(fails) > 0 {
		t.Fatalf("the first email is step 2 here, whose limit is 500: %v", fails)
	}
	if u := ManualUserTurn(form); !strings.Contains(u, "- message (step 0): at most 500 words.") {
		t.Fatalf("user turn:\n%s", u)
	}
}

func TestWriteManualRetryThenClean(t *testing.T) {
	s := &scripted{replies: []scriptReply{{json: manualAnswer(t, "m1_note_too_long")}, {json: manualAnswer(t, "m0_clean_linkedin")}}}
	res, err := (&Writer{Caller: s}).WriteManual(context.Background(), manualInput(t, "d7", leadconfig.ChannelLinkedIn))
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "" || len(res.Attempts) != 2 || len(res.Drafts) != 2 {
		t.Fatalf("result %+v", res)
	}
	if first := res.Attempts[0]; first.OK() || first.CostMicros <= 0 || first.Output == nil || first.Failures[0].Code != CodeCopy {
		t.Fatalf("first attempt %+v", first)
	}
	retry := s.reqs[1].Prompt
	if !strings.HasPrefix(retry, s.reqs[0].Prompt) || !strings.Contains(retry, "G7 step 0") || !strings.Contains(retry, "Your previous answer:") {
		t.Fatalf("retry does not name the failure:\n%s", retry[len(s.reqs[0].Prompt):])
	}
	if s.reqs[0].System != s.reqs[1].System || s.reqs[0].System != res.System {
		t.Fatal("the system prompt changed between attempts")
	}
}

// TestWriteManualFallback: refused twice, a refusal or a cut-off answer gives a reason the caller
// can show and no drafts; going to the templates is the caller's.
func TestWriteManualFallback(t *testing.T) {
	in := manualInput(t, "d7", leadconfig.ChannelLinkedIn)
	fake := &claude.Fake{WriteAnswer: claude.WriteResult{JSON: manualAnswer(t, "m2_note_cites_lead_fact")}}
	res, err := (&Writer{Caller: fake}).WriteManual(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 2 || fake.Calls() != 2 || res.Drafts != nil || res.Written != nil || !strings.HasPrefix(res.Fallback, "Draft refused: G5") {
		t.Fatalf("result %+v", res)
	}
	for _, tc := range []struct {
		err  error
		stop string
		says string
	}{
		{fmt.Errorf("claude: %w", claude.ErrRefused), StopRefusal, "declined"},
		{fmt.Errorf("claude: %w", claude.ErrTruncated), StopMaxTokens, "cut off"},
	} {
		fake := &claude.Fake{Err: tc.err, Usage: sonnetUsage}
		res, err := (&Writer{Caller: fake}).WriteManual(context.Background(), in)
		if err != nil || len(res.Attempts) != 2 || res.Attempts[0].StopReason != tc.stop || !strings.Contains(res.Fallback, tc.says) {
			t.Fatalf("%s: err %v result %+v", tc.stop, err, res)
		}
	}
	bad := &scripted{replies: []scriptReply{{json: json.RawMessage(`{"note":{"paragraphs":[]},"extra":1}`)}, {err: fmt.Errorf("claude: %w", ai.ErrBadAnswer)}}}
	res, err = (&Writer{Caller: bad}).WriteManual(context.Background(), in)
	if err != nil || len(res.Attempts) != 2 || res.Attempts[0].Failures[0].Code != CodeSchema || !strings.HasPrefix(res.Fallback, "Draft refused: G1: ") {
		t.Fatalf("bad answer: err %v result %+v", err, res)
	}
}

func TestWriteManualStopsOnCallerError(t *testing.T) {
	for _, e := range []error{ai.ErrSpendCap, ai.ErrUsageStore, ai.ErrUnavailable} {
		fake := &claude.Fake{Err: e}
		res, err := (&Writer{Caller: fake}).WriteManual(context.Background(), manualInput(t, "d7", leadconfig.ChannelForm))
		if !errors.Is(err, e) || len(res.Attempts) != 0 || fake.Calls() != 1 {
			t.Fatalf("%v: err %v attempts %d calls %d", e, err, len(res.Attempts), fake.Calls())
		}
	}
}

func TestWriteManualRefusesBadInput(t *testing.T) {
	fake := &claude.Fake{}
	w := &Writer{Caller: fake}
	for name, change := range map[string]func(*ManualInput){
		"email channel":  func(in *ManualInput) { in.Channel = leadconfig.ChannelEmail },
		"email examples": func(in *ManualInput) { in.Examples = []WrittenDraft{{}} },
		"three examples": func(in *ManualInput) { in.ManualExamples = make([]ManualWritten, MaxExamples+1) },
		"three versions": func(in *ManualInput) { in.OperatorVersions = make([]OperatorVersion, MaxOperatorVersions+1) },
	} {
		in := manualInput(t, "d7", leadconfig.ChannelForm)
		change(&in)
		if _, err := w.WriteManual(context.Background(), in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if fake.Calls() != 0 {
		t.Fatal("called on bad input")
	}
}

func TestManualSchema(t *testing.T) {
	required := func(s map[string]any) []string {
		r, _ := s["required"].([]string)
		return r
	}
	li := ManualSchema(leadconfig.ChannelLinkedIn)
	if !slices.Equal(required(li), []string{PartNote, PartMessage}) || li["additionalProperties"] != false {
		t.Fatalf("linkedin schema %v", li)
	}
	form := ManualSchema(leadconfig.ChannelForm)
	if !slices.Equal(required(form), []string{PartMessage}) {
		t.Fatalf("form schema %v", form)
	}
	if _, ok := form["properties"].(map[string]any)[PartNote]; ok {
		t.Fatal("a form answer offers a note")
	}
	part := form["properties"].(map[string]any)[PartMessage].(map[string]any)
	if !slices.Equal(required(part), []string{"paragraphs"}) {
		t.Fatalf("a part holds more than paragraphs (a subject?): %v", part)
	}
	paras := part["properties"].(map[string]any)["paragraphs"].(map[string]any)
	if paras["minItems"] != 1 || !slices.Equal(required(paras["items"].(map[string]any)), []string{"text", "cites", "pain_id"}) {
		t.Fatalf("paragraphs %v", paras)
	}
	// Only the keywords the API's structured outputs support, as for the email schema.
	b, err := json.Marshal(li)
	if err != nil {
		t.Fatal(err)
	}
	for _, no := range []string{"maxItems", "maxLength", "minLength", "pattern"} {
		if strings.Contains(string(b), no) {
			t.Fatalf("schema uses %s", no)
		}
	}
}

func TestDecodeManualStrict(t *testing.T) {
	for name, in := range map[string]string{
		"unknown field":  `{"message":{"paragraphs":[]},"subject":"x"}`,
		"part field":     `{"message":{"subject":"x","paragraphs":[]}}`,
		"trailing":       `{"message":{"paragraphs":[]}} {}`,
		"no paragraphs":  `{"message":{}}`,
		"null cites":     `{"message":{"paragraphs":[{"text":"x","cites":null,"pain_id":""}]}}`,
		"missing cites":  `{"note":{"paragraphs":[{"text":"x","pain_id":""}]}}`,
		"not an object":  `[]`,
		"not even JSON ": `nope`,
	} {
		if _, err := DecodeManual([]byte(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	m, err := DecodeManual([]byte(`{"message":{"paragraphs":[{"text":"x","cites":["S1"],"pain_id":"p"}]}}`))
	if err != nil || m.Note != nil || m.Message.Paragraphs[0].PainID != "p" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestManualPromptWords(t *testing.T) {
	in := manualInput(t, "d7", leadconfig.ChannelLinkedIn)
	sys := ManualSystemPrompt(in)
	for _, want := range []string{
		"A LinkedIn note is read in a notification: one short sentence on who you are and why you write, no pitch, no link",
		"The message after they accept is 60-90 words",
		"Use at most one proof line, verbatim, never in the first paragraph",
		"Facts marked context_only",
		"These texts have no subject",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	u := ManualUserTurn(in)
	for _, want := range []string{"- note (step 0): at most 200 characters.", "- message (step 1): 60-90 words.", "- eol_no_security: ", `{"id":"S1"`} {
		if !strings.Contains(u, want) {
			t.Errorf("user turn lacks %q", want)
		}
	}
	in.NoteMax = 300
	if u := ManualUserTurn(in); !strings.Contains(u, "at most 300 characters") {
		t.Errorf("note limit not stated:\n%s", u)
	}
	form := ManualUserTurn(manualInput(t, "d7", leadconfig.ChannelForm))
	if strings.Contains(form, "note (step") || !strings.Contains(form, "- message (step 0): at most 120 words.") {
		t.Errorf("form user turn:\n%s", form)
	}
}
