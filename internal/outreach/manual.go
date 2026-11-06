package outreach

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// The parts of a manual answer, as its JSON names them.
const (
	PartNote    = "note"
	PartMessage = "message"
)

// ManualWriterClaude is the writer of a manual draft Claude wrote: the value the caller stores as
// outreach_message.slots.writer, which the lead's timeline reads. It equals campaign.WriterClaude;
// campaign imports this package, so this package cannot use that constant.
const ManualWriterClaude = "claude"

const (
	// ManualMessageMinWords and ManualMessageMaxWords are the length the prompt asks of the
	// LinkedIn message after they accept. Only the maximum is checked (G7), as for an email.
	ManualMessageMinWords = 60
	ManualMessageMaxWords = 90
	// manualGroundSubject stands in for the subject a manual text does not have: Ground requires
	// one on the first step (G1), and this one holds no word for any other rule to read.
	manualGroundSubject = "-"
	// A LinkedIn note is the channel's step 0: Failures on it, and only it, carry the rune limit.
	manualNoteStep = 0
	// An Instagram DM is that channel's one text, step 0, held to its own rune limit.
	manualDMStep = 0
)

// ManualInput is one lead's job for a text sent by hand. Of the embedded WriteInput, Examples must
// be empty (an email example has steps and subjects; ManualExamples are this group's), and
// Render.Footer is not read: a manual text has no footer. Rules.MaxWords is read for the first
// email's step only, which holds the form message to the same limit; an Instagram DM has its
// rune limit (DMMax) instead.
type ManualInput struct {
	WriteInput
	Channel string // leadconfig.ChannelForm, leadconfig.ChannelLinkedIn or leadconfig.ChannelInstagram
	NoteMax int    // the note's longest length in runes; 0: leadconfig.DefaultLinkedInNoteMax
	DMMax   int    // the Instagram DM's longest length in runes; 0: leadconfig.DefaultInstagramDMMax
	// ManualExamples are at most MaxExamples approved manual drafts, for the system prompt.
	ManualExamples []ManualWritten
}

// ManualPart is one text of a manual answer.
type ManualPart struct {
	Paragraphs []Paragraph `json:"paragraphs"`
}

// ManualWritten is the model's manual answer, and the shape a manual generation stores.
type ManualWritten struct {
	Note    *ManualPart `json:"note,omitempty"`
	Message *ManualPart `json:"message,omitempty"`
}

// ManualDraft is one rendered, checked manual text.
type ManualDraft struct {
	// Step is the place in this channel's own texts, as the check numbers them: a form's message
	// 0; a LinkedIn note 0 and message 1; an Instagram DM 0. A form lead's LinkedIn texts follow its form message in
	// the sequence (Decision 10), so the caller adds that offset.
	Step       int
	Part       string // PartNote or PartMessage
	Writer     string // ManualWriterClaude
	Draft      Draft
	Paragraphs []Paragraph // the text as written, with the cites the caller records
}

// ManualResult is the outcome for one lead. Exactly one of Drafts or Fallback is set when
// WriteManual returns no error.
type ManualResult struct {
	FactSHA256 string
	System     string
	Attempts   []Attempt
	Written    *ManualWritten
	Drafts     []ManualDraft
	// Fallback is why there is no Claude draft, in plain words: the caller falls back to the
	// templates and shows it.
	Fallback string
}

// WriteManual drafts one lead's form, LinkedIn or Instagram texts: generate → check → (one retry naming the
// failures) → check → the rendered drafts or a Fallback. An error means the job could not run (bad
// input, a cap, the usage store, the provider unavailable, the operator's own slots failing to
// render); the result then holds the attempts made so far.
func (w *Writer) WriteManual(ctx context.Context, in ManualInput) (ManualResult, error) {
	res := ManualResult{FactSHA256: in.Sheet.SHA256()}
	switch {
	case w.Caller == nil:
		return res, errors.New("outreach: writer: no caller")
	case in.Channel != leadconfig.ChannelForm && in.Channel != leadconfig.ChannelLinkedIn && in.Channel != leadconfig.ChannelInstagram:
		return res, fmt.Errorf("outreach: writer: channel %q has no manual text", in.Channel)
	case len(in.Examples) > 0:
		return res, errors.New("outreach: writer: email examples given for a manual text")
	case len(in.ManualExamples) > MaxExamples:
		return res, fmt.Errorf("outreach: writer: %d examples, at most %d", len(in.ManualExamples), MaxExamples)
	case len(in.OperatorVersions) > MaxOperatorVersions:
		return res, fmt.Errorf("outreach: writer: %d operator versions, at most %d", len(in.OperatorVersions), MaxOperatorVersions)
	}
	res.System = ManualSystemPrompt(in)
	attempts, err := w.generate(ctx, in.LeadID, res.System, ManualUserTurn(in), ManualSchema(in.Channel), func(raw json.RawMessage, a *Attempt) error {
		m, err := DecodeManual(raw)
		if err != nil {
			a.Failures = []Failure{{Code: CodeSchema, Step: StepTalkingPoints, Para: ParaSubject, Msg: "the answer does not follow the schema"}}
			return nil
		}
		if a.Output, err = json.Marshal(m); err != nil {
			return err
		}
		if a.Failures = checkManual(m, in); len(a.Failures) > 0 {
			return nil
		}
		drafts, fails, err := renderManual(m, in)
		if err != nil {
			return err
		}
		if a.Failures = fails; len(fails) == 0 {
			res.Written, res.Drafts = &m, drafts
		}
		return nil
	})
	res.Attempts = attempts
	if err != nil {
		return res, err
	}
	if res.Written == nil {
		res.Fallback = fallbackReason(attempts[len(attempts)-1])
	}
	return res, nil
}

// manualRules are the lead's rules as a manual text meets them: a note is step 0 and its message
// step 1, a form's message and an Instagram DM step 0. The campaign's word limits are an email's:
// the note and the DM have their rune limits instead, the message after they accept its own
// limit, and the form message the first email's. The email's rule on links in the first email
// does not hold (a form message may carry an allowed link; a LinkedIn or Instagram text is
// refused any by checkManual), and no text has talking points.
func manualRules(in ManualInput) GroundRules {
	r := in.Rules
	r.CallStep, r.NoLinksInStep0 = false, false
	switch in.Channel {
	case leadconfig.ChannelLinkedIn:
		r.EmailSteps = []int{manualNoteStep, 1}
		r.MaxWords = map[int]int{1: ManualMessageMaxWords}
		return r
	case leadconfig.ChannelInstagram:
		r.EmailSteps, r.MaxWords = []int{manualDMStep}, map[int]int{}
		return r
	}
	limits := map[int]int{}
	if most, ok := in.Rules.MaxWords[in.Rules.FirstEmail()]; ok {
		limits[0] = most
	}
	r.EmailSteps, r.MaxWords = []int{0}, limits
	return r
}

// manualSteps lists m's parts for Ground and for rendering, in order. A part the channel does not
// have is kept at a step its rules refuse, so it fails G1 rather than vanish.
func manualSteps(m ManualWritten, channel string) []manualStep {
	steps := []manualStep{{0, PartNote, m.Note}, {1, PartMessage, m.Message}}
	if channel == leadconfig.ChannelForm || channel == leadconfig.ChannelInstagram {
		steps = []manualStep{{0, PartMessage, m.Message}, {1, PartNote, m.Note}}
	}
	out := steps[:0]
	for _, s := range steps {
		if s.body != nil {
			out = append(out, s)
		}
	}
	return out
}

type manualStep struct {
	step int
	part string
	body *ManualPart
}

// checkManual is G1-G9 through Ground, the cites rule, no link in a LinkedIn or Instagram text,
// the note's rune limit and the Instagram DM's. Nothing is added to a note or a DM when it
// renders, so its text here is its rendered length.
func checkManual(m ManualWritten, in ManualInput) []Failure {
	var d WrittenDraft
	steps := manualSteps(m, in.Channel)
	for _, s := range steps {
		d.Steps = append(d.Steps, WrittenStep{Step: s.step, Subject: manualGroundSubject, Paragraphs: s.body.Paragraphs})
	}
	fails := append(Ground(d, in.Sheet, manualRules(in)), uncitedFailures(d)...)
	var where string
	switch in.Channel {
	case leadconfig.ChannelLinkedIn:
		where = "a LinkedIn text"
	case leadconfig.ChannelInstagram:
		where = "an Instagram DM"
	default:
		return fails
	}
	for _, s := range steps {
		for i, p := range s.body.Paragraphs {
			for _, u := range checkURLs(p.Text) {
				fails = append(fails, Failure{Code: CodeCopy, Step: s.step, Para: i, Token: u, Msg: where + " may hold no link"})
			}
		}
		switch {
		case in.Channel == leadconfig.ChannelLinkedIn && s.part == PartNote:
			fails = append(fails, CheckNote(joinParagraphs(s.body.Paragraphs), in.NoteMax)...)
		case in.Channel == leadconfig.ChannelInstagram && s.part == PartMessage:
			fails = append(fails, CheckDM(joinParagraphs(s.body.Paragraphs), in.DMMax)...)
		}
	}
	return fails
}

// NoteRunes is a LinkedIn note's length as the limit measures it: runes, outer white space aside.
func NoteRunes(text string) int { return utf8.RuneCountInString(strings.TrimSpace(text)) }

// CheckNote holds a rendered LinkedIn note to sender.linkedin_note_max (noteMax; 0: the default).
func CheckNote(text string, noteMax int) []Failure {
	noteMax = cmp.Or(noteMax, leadconfig.DefaultLinkedInNoteMax)
	if n := NoteRunes(text); n > noteMax {
		return []Failure{{Code: CodeCopy, Step: manualNoteStep, Para: ParaSubject, Token: strconv.Itoa(n),
			Msg: fmt.Sprintf("the note is %d characters, at most %d", n, noteMax)}}
	}
	return nil
}

// CheckDM holds a rendered Instagram DM to sender.instagram_dm_max (dmMax; 0: the default),
// measured as NoteRunes measures a note.
func CheckDM(text string, dmMax int) []Failure {
	dmMax = cmp.Or(dmMax, leadconfig.DefaultInstagramDMMax)
	if n := NoteRunes(text); n > dmMax {
		return []Failure{{Code: CodeCopy, Step: manualDMStep, Para: ParaSubject, Token: strconv.Itoa(n),
			Msg: fmt.Sprintf("the Instagram DM is %d characters, at most %d", n, dmMax)}}
	}
	return nil
}

func joinParagraphs(paras []Paragraph) string {
	lines := make([]string, len(paras))
	for i, p := range paras {
		lines[i] = strings.TrimSpace(p.Text)
	}
	return strings.Join(lines, "\n\n")
}

// renderManual renders each part through Render and runs CheckRendered (G10) on it. Model text is
// the template's literal text. A form message gets the greeting and the sender's name and identity
// line, as the form templates do; a LinkedIn text or an Instagram DM gets nothing (it comes from
// the sender's own account). Failures are the model's; an error
// is the operator's slots failing to render.
func renderManual(m ManualWritten, in ManualInput) ([]ManualDraft, []Failure, error) {
	var (
		drafts []ManualDraft
		fails  []Failure
	)
	rules := manualRules(in)
	for _, s := range manualSteps(m, in.Channel) {
		body := joinParagraphs(s.body.Paragraphs)
		if strings.ContainsRune(body, 0) {
			fails = append(fails, Failure{Code: CodeSchema, Step: s.step, Para: ParaSubject, Msg: "the text holds a control character"})
			continue
		}
		kind := DraftLinkedIn
		switch in.Channel {
		case leadconfig.ChannelInstagram:
			kind = DraftInstagram
		case leadconfig.ChannelForm:
			kind = DraftForm
			if g := strings.TrimSpace(in.Render.Greeting); g != "" {
				body = g + "\n\n" + body
			}
			body += "\n\n" + claudeDelimL + ".SenderName" + claudeDelimR + ", " + claudeDelimL + ".IdentityLine" + claudeDelimR
		}
		tm, err := literalTemplate(kind, "", body, nil)
		if err != nil {
			return nil, nil, err
		}
		dr, err := Render(tm, in.Render.Slots)
		if err != nil {
			return nil, nil, fmt.Errorf("render %s: %w", s.part, err)
		}
		fails = append(fails, CheckRendered(dr, s.step, in.Render.Cfg, rules.StepRules)...)
		drafts = append(drafts, ManualDraft{Step: s.step, Part: s.part, Writer: ManualWriterClaude, Draft: dr, Paragraphs: s.body.Paragraphs})
	}
	return drafts, fails, nil
}

// ManualSystemPrompt is the frozen, cached system prompt of a campaign's manual texts: the same
// bytes for every lead and for the form and LinkedIn channels, changing only with the campaign's
// rules or the examples and versions the caller gives. An Instagram DM adds its own block
// (writeInstagramRules), so a campaign without Instagram never sees it. What varies by lead is
// ManualUserTurn's.
func ManualSystemPrompt(in ManualInput) string {
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
	if in.Channel == leadconfig.ChannelInstagram {
		writeInstagramRules(line)
	}
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

// writeInstagramRules is the Instagram DM's block of the system prompt, written only when the
// channel is Instagram.
func writeInstagramRules(line func(string, ...any)) {
	line("# Instagram")
	line("- This lead gets a first direct message on Instagram to the café's own account, read in its inbox from a sender it does not follow: (1) who you are and why you write, citing sender facts only; (2) one pain the reader sees, with its pain_id; (3) the free offer from the offer facts and the call to action, word for word.")
	line("- An Instagram message holds no link and no web address: Instagram flags links in a first message. Write no greeting and no sign-off; the code adds nothing to it.")
	line("")
}

func writeManualExample(b *strings.Builder, m ManualWritten) {
	for _, s := range []struct {
		name string
		part *ManualPart
	}{{"Note", m.Note}, {"Message", m.Message}} {
		if s.part == nil {
			continue
		}
		fmt.Fprintf(b, "%s:\n", s.name)
		for _, p := range s.part.Paragraphs {
			writeParagraph(b, p)
		}
	}
}

// ManualUserTurn is the lead's turn: the language, the pains and forbidden patterns, the fact
// sheet, and each part to write with its limit. The checks name a part by its step, so the turn
// says which is which for the retry.
func ManualUserTurn(in ManualInput) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	linkedIn, instagram := in.Channel == leadconfig.ChannelLinkedIn, in.Channel == leadconfig.ChannelInstagram
	switch {
	case linkedIn:
		line("Write the LinkedIn note and the message after they accept, for this lead.")
	case instagram:
		line("Write a first direct message on Instagram to the café's own account, for this lead.")
	default:
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
	if instagram {
		line("- message (step 0): at most %d characters, no link.", cmp.Or(in.DMMax, leadconfig.DefaultInstagramDMMax))
		line("")
		line("Write the message. It needs at least one paragraph.")
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
