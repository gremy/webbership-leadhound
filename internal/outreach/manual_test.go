package outreach

import (
	"cmp"
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

// TestWriteManualInstagram: one DM at step 0, written by Claude, with nothing added when it renders
// (no greeting, no name, no identity line, no subject, no footer), and the stored output reads back.
func TestWriteManualInstagram(t *testing.T) {
	fake := &claude.Fake{WriteAnswer: claude.WriteResult{JSON: manualAnswer(t, "m0_clean_instagram")}}
	in := manualInput(t, "d7", leadconfig.ChannelInstagram)
	res, err := (&Writer{Caller: fake}).WriteManual(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback != "" || len(res.Attempts) != 1 || !res.Attempts[0].OK() || fake.Calls() != 1 || res.Written == nil || len(res.Drafts) != 1 {
		t.Fatalf("result %+v, calls %d", res, fake.Calls())
	}
	d := res.Drafts[0]
	if d.Step != 0 || d.Part != PartMessage || d.Writer != "claude" {
		t.Fatalf("draft %+v", d)
	}
	if d.Draft.Kind != DraftInstagram || d.Draft.Subject != "" || d.Draft.Used != nil || d.Draft.Body != d.Draft.BodyAboveFooter {
		t.Fatalf("draft %+v", d.Draft)
	}
	if len(d.Paragraphs) != 3 || len(d.Paragraphs[0].Cites) == 0 {
		t.Fatalf("paragraphs %+v", d.Paragraphs)
	}
	want := joinParagraphs(manualFixtures(t)["m0_clean_instagram"].Draft.Message.Paragraphs) + "\n"
	if d.Draft.Body != want {
		t.Fatalf("body:\n%s\nwant:\n%s", d.Draft.Body, want)
	}
	for _, added := range []string{"Hello,", "Demo Sender", "Demo Ltd", in.Render.Slots.OptOutLine.Value} {
		if strings.Contains(d.Draft.Body, added) {
			t.Fatalf("an Instagram DM got %q:\n%s", added, d.Draft.Body)
		}
	}
	if n := NoteRunes(d.Draft.Body); n > leadconfig.DefaultInstagramDMMax {
		t.Fatalf("the clean DM is %d characters", n)
	}
	back, err := DecodeManual(res.Attempts[0].Output)
	if err != nil || back.Note != nil || back.Message == nil {
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

// TestManualDMLimit: the Instagram DM is held to sender.instagram_dm_max in runes (0: the default
// 500), the failure names step 0 and the length, and the checked length is the rendered one.
// copy.max_words never reaches the DM.
func TestManualDMLimit(t *testing.T) {
	dm := func(n int) ManualWritten {
		text := "I'm Sam Carter, a Drupal developer."
		text += strings.Repeat("é", n-len(text))
		return ManualWritten{Message: &ManualPart{Paragraphs: []Paragraph{{Text: text, Cites: []string{"S1", "S2"}}}}}
	}
	for _, tc := range []struct {
		dmMax, runes int
		ok           bool
	}{
		{0, 500, true}, {0, 501, false},
		{1000, 1000, true}, {1000, 1001, false},
		{80, 80, true}, {80, 81, false},
	} {
		in := manualInput(t, "d7", leadconfig.ChannelInstagram)
		in.DMMax = tc.dmMax
		in.NoteMax = 50 // the LinkedIn note's limit never reaches a DM
		in.Rules.MaxWords = map[int]int{0: 3}
		m := dm(tc.runes)
		fails := checkManual(m, in)
		if tc.ok != (len(fails) == 0) {
			t.Errorf("max %d, %d runes: %v", tc.dmMax, tc.runes, fails)
			continue
		}
		if !tc.ok {
			if len(fails) != 1 || fails[0].Code != CodeCopy || fails[0].Step != 0 || fails[0].Token != fmt.Sprint(tc.runes) || !strings.Contains(fails[0].Msg, "Instagram DM") {
				t.Errorf("max %d, %d runes: %v", tc.dmMax, tc.runes, fails)
			}
			continue
		}
		drafts, rfails, err := renderManual(m, in)
		if err != nil || len(rfails) > 0 || NoteRunes(drafts[0].Draft.Body) != tc.runes {
			t.Errorf("rendered length differs from the checked one: %v %v", rfails, err)
		}
	}
	if fails := CheckDM(strings.Repeat("a", 501), 0); len(fails) != 1 {
		t.Errorf("CheckDM at the default: %v", fails)
	}
	if fails := CheckDM("  "+strings.Repeat("a", 500)+"\n", 0); len(fails) != 0 {
		t.Errorf("outer white space counted: %v", fails)
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
		"none channel":   func(in *ManualInput) { in.Channel = leadconfig.ChannelNone },
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
	ig := ManualSchema(leadconfig.ChannelInstagram)
	if !slices.Equal(required(ig), []string{PartMessage}) || ig["additionalProperties"] != false {
		t.Fatalf("instagram schema %v", ig)
	}
	if props := ig["properties"].(map[string]any); len(props) != 1 || props[PartNote] != nil {
		t.Fatalf("an Instagram answer offers more than its one message: %v", props)
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
	ig := manualInput(t, "d7", leadconfig.ChannelInstagram)
	if u := ManualUserTurn(ig); strings.Contains(u, "note (step") || strings.Contains(u, "words") ||
		!strings.Contains(u, "Write a first direct message on Instagram to the café's own account, for this lead.") ||
		!strings.Contains(u, "- message (step 0): at most 500 characters, no link.") {
		t.Errorf("instagram user turn:\n%s", u)
	}
	ig.DMMax = 320
	if u := ManualUserTurn(ig); !strings.Contains(u, "at most 320 characters") {
		t.Errorf("DM limit not stated:\n%s", u)
	}
	// Instagram's words reach the Instagram prompt only; form and LinkedIn share one prompt.
	igSys := ManualSystemPrompt(ig)
	for _, want := range []string{
		"# Instagram\n",
		"a first direct message on Instagram to the café's own account",
		"An Instagram message holds no link and no web address",
	} {
		if !strings.Contains(igSys, want) {
			t.Errorf("Instagram system prompt lacks %q", want)
		}
	}
	formSys := ManualSystemPrompt(manualInput(t, "d7", leadconfig.ChannelForm))
	if formSys != sys || strings.Contains(sys, "Instagram") {
		t.Error("the form and LinkedIn system prompts differ, or name Instagram")
	}
}

// TestManualPromptsUnchangedForFormAndLinkedIn pins the form and LinkedIn prompts to their bytes
// before the Instagram channel (headManualSystemPrompt and headManualUserTurn are that version's
// functions, copied verbatim), over inputs that reach every branch: campaign name, examples,
// operator versions, note limit and word limits.
func TestManualPromptsUnchangedForFormAndLinkedIn(t *testing.T) {
	fx := manualFixtures(t)
	for _, channel := range []string{leadconfig.ChannelForm, leadconfig.ChannelLinkedIn} {
		for name, change := range map[string]func(*ManualInput){
			"plain":    func(*ManualInput) {},
			"campaign": func(in *ManualInput) { in.Campaign = "Demo campaign" },
			"examples": func(in *ManualInput) {
				in.ManualExamples = []ManualWritten{fx["m0_clean_linkedin"].Draft, fx["m0_clean_form"].Draft}
			},
			"versions":  func(in *ManualInput) { in.OperatorVersions = []OperatorVersion{{Body: "Hello <org>, <name> here."}} },
			"note max":  func(in *ManualInput) { in.NoteMax = 300; in.DMMax = 900 },
			"no limits": func(in *ManualInput) { in.Rules.MaxWords = nil },
		} {
			in := manualInput(t, "d7", channel)
			change(&in)
			if got, want := ManualSystemPrompt(in), headManualSystemPrompt(in); got != want {
				t.Errorf("%s %s: system prompt changed:\n%s\nwant:\n%s", channel, name, got, want)
			}
			if got, want := ManualUserTurn(in), headManualUserTurn(in); got != want {
				t.Errorf("%s %s: user turn changed:\n%s\nwant:\n%s", channel, name, got, want)
			}
		}
	}
}

// headManualSystemPrompt is ManualSystemPrompt as it was before the Instagram channel, verbatim.
func headManualSystemPrompt(in ManualInput) string {
	r := in.Rules
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	line("You write short texts one sender sends by hand: a LinkedIn note, the LinkedIn message after they accept, or a message for a contact form. You write as the sender, in the first person, to one organisation at a time.")
	if in.Campaign != "" {
		line("The campaign: %s.", in.Campaign)
	}
	line("")
	writeFactRules(line, r)
	line("")
	writeVoiceRules(line, r, "a LinkedIn note or a contact-form message",
		"- Only a contact-form message may hold a link, and only one a fact it cites holds. A LinkedIn note or message holds no link and no web address.")
	line("")
	line("# Structure")
	line("The user turn lists the parts to write for this lead, each with its limit.")
	line("- A LinkedIn note is read in a notification: one short sentence on who you are and why you write, no pitch, no link.")
	line("- The message after they accept is %d-%d words: who you are in a few words, one or two pains the reader sees, each with its pain_id, and the free report from the offer facts, which is theirs to keep.", ManualMessageMinWords, ManualMessageMaxWords)
	line("- A contact-form message: (1) who you are and why you write, citing sender facts only; (2) up to three pains the reader sees, one paragraph each, each with its pain_id; (3) one proof line, if it fits, verbatim; (4) the free report from the offer facts, which is theirs to keep; (5) the call to action, word for word.")
	line("- Every part has at least one paragraph with text, and every paragraph cites at least one fact. Write every part the user turn lists, and only those.")
	line("- These texts have no subject. Write no greeting, no sign-off, no name under the text and no identity line: the code adds what a contact form needs, and a LinkedIn text needs none of them.")
	line("")
	line("# Output")
	line("Answer with JSON only, in the schema given: each part the user turn asks for under its name (\"note\", \"message\"), with its paragraphs; each paragraph with its text, cites and pain_id.")
	line("")
	line("# Examples")
	if len(in.ManualExamples) == 0 {
		line("No approved examples yet.")
	} else {
		line("Approved texts for other leads. Copy their voice and shape, never their facts: their cites point at another lead's sheet.")
		for i, ex := range in.ManualExamples {
			line("")
			line("## Example %d", i+1)
			writeManualExample(&b, ex)
		}
	}
	writeOperatorVersions(&b, in.OperatorVersions)
	return b.String()
}

// headManualUserTurn is ManualUserTurn as it was before the Instagram channel, verbatim.
func headManualUserTurn(in ManualInput) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	linkedIn := in.Channel == leadconfig.ChannelLinkedIn
	if linkedIn {
		line("Write the LinkedIn note and the message after they accept, for this lead.")
	} else {
		line("Write the contact-form message for this lead.")
	}
	line("Language: %s", cmp.Or(in.Lang, LangEnglish))
	line("")
	writeLeadContext(&b, in.WriteInput)
	line("")
	line("The parts:")
	if linkedIn {
		line("- note (step 0): at most %d characters.", cmp.Or(in.NoteMax, leadconfig.DefaultLinkedInNoteMax))
		line("- message (step 1): %d-%d words.", ManualMessageMinWords, ManualMessageMaxWords)
		line("")
		line("Write both parts. Each needs at least one paragraph.")
		return b.String()
	}
	if most, ok := manualRules(in).MaxWords[0]; ok {
		line("- message (step 0): at most %d words.", most)
	} else {
		line("- message (step 0).")
	}
	line("")
	line("Write the message. It needs at least one paragraph.")
	return b.String()
}
