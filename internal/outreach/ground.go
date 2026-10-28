package outreach

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// The grounding codes. Every refusal carries one, so failures are countable per rule.
const (
	CodeSchema   = "G1"  // steps and talking points as the sequence needs them
	CodeCites    = "G2"  // every cite is a sheet fact, none echo_guard
	CodeClaim    = "G3"  // every number, date, version, URL, domain, email, phone, money, percentage is a cited fact
	CodeOrgName  = "G4"  // the org's name, if used, is exactly the fact value
	CodeNames    = "G4b" // proper nouns come from cited facts or copy.allowed_names
	CodeOpener   = "G5"  // the first email opens with who the sender is, never with research
	CodePains    = "G5b" // a claim about the reader is one of the pains its signals allow
	CodeEcho     = "G6"  // no run of N words shared with the org's own text
	CodeCopy     = "G7"  // banned words, "!", word limits, subject length, no link in step 0
	CodeKind     = "G8"  // phrases the campaign forbids for the lead's kind
	CodeProof    = "G9"  // the proof line verbatim, no other numbered first-person claim, no boasts
	CodeRendered = "G10" // outreach.Check on the final rendered email
)

// GroundCodes lists the codes Ground itself checks, in order (G10 runs on the rendered draft:
// CheckRendered).
var GroundCodes = []string{CodeSchema, CodeCites, CodeClaim, CodeOrgName, CodeNames, CodeOpener,
	CodePains, CodeEcho, CodeCopy, CodeKind, CodeProof}

// TalkingPoints is how many talking points a draft holds when the sequence has a call step.
const TalkingPoints = 3

// DefaultEchoWords is the echo run length when GroundRules.EchoWords is 0.
const DefaultEchoWords = 6

// DefaultResearchOpeners are the research-opener patterns when GroundRules.ResearchOpeners is nil.
var DefaultResearchOpeners = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bI (noticed|saw|came across|was looking at)\b`),
	regexp.MustCompile(`(?i)\byour (site|website)\b`),
}

// DefaultInflationWords are the boast words when GroundRules.InflationWords is nil.
var DefaultInflationWords = []string{"dozens", "hundreds", "many clients", "countless", "leading", "top", "best", "award"}

// WrittenDraft is the model's output, in the strict JSON schema the writer asks for.
type WrittenDraft struct {
	Steps         []WrittenStep  `json:"steps"`
	TalkingPoints []TalkingPoint `json:"talking_points"`
}

// WrittenStep is one email step.
type WrittenStep struct {
	Step       int         `json:"step"`
	Subject    string      `json:"subject"`
	Paragraphs []Paragraph `json:"paragraphs"`
}

// Paragraph is one paragraph of an email with the facts it cites. A paragraph that makes a
// claim about the reader names the pain it rewords.
type Paragraph struct {
	Text   string   `json:"text"`
	Cites  []string `json:"cites"`
	PainID string   `json:"pain_id,omitempty"`
}

// TalkingPoint is one line for the call card.
type TalkingPoint struct {
	Text  string   `json:"text"`
	Cites []string `json:"cites"`
}

// DecodeWrittenDraft reads the model's JSON strictly: unknown fields and trailing data are errors.
func DecodeWrittenDraft(b []byte) (WrittenDraft, error) {
	var d WrittenDraft
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return WrittenDraft{}, fmt.Errorf("outreach: written draft: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return WrittenDraft{}, errors.New("outreach: written draft: trailing data after the JSON object")
	}
	return d, nil
}

// Where a Failure sits, beside real step and paragraph numbers.
const (
	StepTalkingPoints = -1 // Failure.Step for a talking point, or for the whole draft
	ParaSubject       = -1 // Failure.Para for a step's subject, or the whole step
)

// Failure is one reason a written draft is refused.
type Failure struct {
	Code  string `json:"code"`
	Step  int    `json:"step"`  // the email step; StepTalkingPoints for talking points and the whole draft
	Para  int    `json:"para"`  // 0-based paragraph or talking point; ParaSubject for the subject or whole step
	Token string `json:"token"` // the offending token, cite, word or pattern; may be empty
	Msg   string `json:"msg"`
}

func (f Failure) String() string {
	where := "draft"
	switch {
	case f.Step >= 0 && f.Para >= 0:
		where = fmt.Sprintf("step %d paragraph %d", f.Step, f.Para+1)
	case f.Step >= 0:
		where = fmt.Sprintf("step %d", f.Step)
	case f.Para >= 0:
		where = fmt.Sprintf("talking point %d", f.Para+1)
	}
	if f.Token != "" {
		return fmt.Sprintf("%s %s %q: %s", f.Code, where, f.Token, f.Msg)
	}
	return fmt.Sprintf("%s %s: %s", f.Code, where, f.Msg)
}

// Pain is one pain the lead's stored signals allow (the caller picks them from the
// campaign's offer.pains by their needs). A paragraph may reword it, never add to it.
type Pain struct {
	ID   string
	Text string
}

// GroundRules are the campaign's rules for one lead, as plain values. The embedded StepRules
// are the same ones CheckIn takes (email steps, max words, the campaign's banned words, the
// kind's forbid patterns, links in step 0, exclamation), so G7/G8 and G10 read one source.
type GroundRules struct {
	StepRules
	ConfigBanned    []string         // leads.yaml copy.banned_words, checked beside StepRules.BannedWords
	CallStep        bool             // the sequence has a call step: exactly TalkingPoints talking points
	Pains           []Pain           // the pains this lead's signals allow
	AllowedNames    []string         // copy.allowed_names: names any text may use
	ResearchOpeners []*regexp.Regexp // copy.research_openers; nil: DefaultResearchOpeners
	InflationWords  []string         // copy.inflation_words; nil: DefaultInflationWords
	EchoTexts       []string         // stored page texts of the org, beside the sheet's echo_guard facts
	EchoWords       int              // copy.echo_words; 0: DefaultEchoWords
}

// Ground checks a written draft against the lead's fact sheet. It returns every failure,
// each with its code; nil means the draft is grounded.
func Ground(d WrittenDraft, sheet FactSheet, rules GroundRules) []Failure {
	return groundWith(d, sheet, rules, func(string) bool { return true })
}

// groundItem is one checked text: a subject, a paragraph or a talking point.
type groundItem struct {
	step, para int
	text       string
	cites      []string
	painID     string
	stepPains  []string // a subject's: the pain_ids its step's paragraphs name
	subject    bool
	talking    bool
	opener     bool // the first email's first paragraph: G5 owns its research wording
}

type grounder struct {
	sheet FactSheet
	r     GroundRules
	byID  map[string]Fact
	pains map[string]Pain
	on    func(string) bool
	out   []Failure
}

func (g *grounder) fail(code string, step, para int, token, format string, a ...any) {
	if g.on(code) {
		g.out = append(g.out, Failure{Code: code, Step: step, Para: para, Token: token, Msg: fmt.Sprintf(format, a...)})
	}
}

// groundWith is Ground with a switch per rule, so a test can turn one rule off.
func groundWith(d WrittenDraft, sheet FactSheet, rules GroundRules, on func(string) bool) []Failure {
	g := &grounder{sheet: sheet, r: rules, byID: map[string]Fact{}, pains: map[string]Pain{}, on: on}
	for _, f := range sheet.Facts {
		g.byID[f.ID] = f
	}
	for _, p := range rules.Pains {
		g.pains[p.ID] = p
	}

	var items []groundItem
	first := firstStep(d)
	for _, st := range d.Steps {
		var all, pains []string
		for _, p := range st.Paragraphs {
			all = append(all, p.Cites...)
			if p.PainID != "" {
				pains = append(pains, p.PainID)
			}
		}
		items = append(items, groundItem{step: st.Step, para: ParaSubject, text: st.Subject, cites: all, stepPains: pains, subject: true})
		for i, p := range st.Paragraphs {
			items = append(items, groundItem{step: st.Step, para: i, text: p.Text, cites: p.Cites, painID: p.PainID, opener: st.Step == first && i == 0})
		}
	}
	for i, tp := range d.TalkingPoints {
		items = append(items, groundItem{step: StepTalkingPoints, para: i, text: tp.Text, cites: tp.Cites, talking: true})
	}

	g.schema(d)
	echo := g.echoShingles()
	for _, it := range items {
		g.cites(it)
		g.claims(it)
		g.orgName(it)
		g.names(it)
		g.pain(it)
		g.echo(it, echo)
		g.kind(it)
		g.proof(it)
	}
	g.opener(d)
	g.copyRules(d, items)
	return g.out
}

// G1 schema.
func (g *grounder) schema(d WrittenDraft) {
	seen := map[int]bool{}
	first := firstStep(d)
	for _, st := range d.Steps {
		switch {
		case seen[st.Step]:
			g.fail(CodeSchema, st.Step, ParaSubject, "", "this email appears more than once")
		case g.r.EmailSteps != nil && !slices.Contains(g.r.EmailSteps, st.Step):
			g.fail(CodeSchema, st.Step, ParaSubject, "", "this email is not one the sequence asks for")
		}
		seen[st.Step] = true
		if st.Step == first && strings.TrimSpace(st.Subject) == "" {
			g.fail(CodeSchema, st.Step, ParaSubject, "", "the first email has no subject")
		}
		if len(st.Paragraphs) == 0 {
			g.fail(CodeSchema, st.Step, ParaSubject, "", "this email has no paragraphs")
		}
		for i, p := range st.Paragraphs {
			if strings.TrimSpace(p.Text) == "" {
				g.fail(CodeSchema, st.Step, i, "", "the paragraph is empty")
			}
		}
	}
	for _, s := range g.r.EmailSteps {
		if !seen[s] {
			g.fail(CodeSchema, s, ParaSubject, "", "this email is missing")
		}
	}
	switch n := len(d.TalkingPoints); {
	case g.r.CallStep && n != TalkingPoints:
		g.fail(CodeSchema, StepTalkingPoints, ParaSubject, "", "%d talking points; the call step needs %d", n, TalkingPoints)
	case !g.r.CallStep && n > 0:
		g.fail(CodeSchema, StepTalkingPoints, ParaSubject, "", "%d talking points; the sequence has no call step", n)
	}
	for i, tp := range d.TalkingPoints {
		if strings.TrimSpace(tp.Text) == "" {
			g.fail(CodeSchema, StepTalkingPoints, i, "", "the talking point is empty")
		}
	}
}

// G2 cites. A subject's cites are its step's, already checked on each paragraph.
func (g *grounder) cites(it groundItem) {
	if it.subject {
		return
	}
	for _, id := range it.cites {
		f, ok := g.byID[id]
		switch {
		case !ok:
			g.fail(CodeCites, it.step, it.para, id, "%s is not a fact on the sheet", id)
		case f.EchoGuard:
			g.fail(CodeCites, it.step, it.para, id, "%s is the org's own text (echo_guard): context only, never cited", id)
		}
	}
}

// cited returns the cited facts that exist and are not echo_guard.
func (g *grounder) cited(it groundItem) []Fact {
	var out []Fact
	for _, id := range it.cites {
		if f, ok := g.byID[id]; ok && !f.EchoGuard && !slices.ContainsFunc(out, func(x Fact) bool { return x.ID == id }) {
			out = append(out, f)
		}
	}
	return out
}

// sources are the texts an item's claims and names may rest on: its cited facts' values, and
// its pain's text when it names an allowed pain. A subject carries no cites of its own, so it
// rests on its step's: every fact its paragraphs cite and every allowed pain they name; and a
// subject may always name the org (its name fact), as "Drupal 9 end of life for <org>".
func (g *grounder) sources(it groundItem) []string {
	var src []string
	for _, f := range g.cited(it) {
		src = append(src, f.Value)
	}
	if p, ok := g.pains[it.painID]; ok && it.painID != "" {
		src = append(src, p.Text)
	}
	if it.subject {
		for _, id := range it.stepPains {
			if p, ok := g.pains[id]; ok {
				src = append(src, p.Text)
			}
		}
		for _, f := range g.sheet.WithLabel(LabelOrgName) {
			src = append(src, f.Value)
		}
	}
	return src
}

// G3 claim tokens.
func (g *grounder) claims(it groundItem) {
	for _, c := range uncoveredClaims(it.text, g.sources(it)) {
		g.fail(CodeClaim, it.step, it.para, c.raw, "%s is not in a cited fact", c.raw)
	}
}

// G4 the org's name.
func (g *grounder) orgName(it groundItem) {
	for _, f := range g.sheet.WithLabel(LabelOrgName) {
		if w := orgNameMisuse(it.text, f.Value, g.r.AllowedNames); w != "" {
			g.fail(CodeOrgName, it.step, it.para, w, "the org's name is %q, exactly", f.Value)
		}
	}
}

// G4b proper nouns.
func (g *grounder) names(it groundItem) {
	src := append(g.sources(it), g.r.AllowedNames...)
	for _, run := range capitalRuns(it.text) {
		if !nameAllowed(run, src) {
			g.fail(CodeNames, it.step, it.para, run, "%s is in no cited fact and not in your allowed names list", run)
		}
	}
}

// identityLabels are the lead facts that say who the lead is, not what is wrong: a paragraph
// may cite them without naming a pain.
var identityLabels = []string{LabelOrgName, LabelOrgCountry, LabelOrgDomain}

// G5b pains. A claim about the reader is one of the pains the lead's signals allow:
//
//   - A paragraph that names a pain_id must name an allowed one (G3/G4b then hold it to the
//     pain's text and its cites).
//   - A paragraph with no pain_id may not cite a lead fact beyond the org's name, country and
//     domain.
//   - A paragraph with no pain_id, a subject whose step names no pain, and a talking point with
//     no lead fact behind it may speak about the reader only as the sender's own action or as
//     the offer (readerClaim). This is what refuses "Your contact form is broken" whatever it
//     cites: a claim with no number or name in it still has to be an allowed pain.
//
// A talking point has no pain_id: one that cites a lead fact beyond the org's identity rests on
// that stored finding (G3/G4b check what it says of it) and is not read for reader claims.
func (g *grounder) pain(it groundItem) {
	switch {
	case it.subject:
		if len(it.stepPains) > 0 {
			return // the subject sums up its step's pains
		}
	case it.talking:
		for _, f := range g.cited(it) {
			if f.Group == GroupLead && !slices.Contains(identityLabels, f.Label) {
				return
			}
		}
	case it.painID != "":
		if _, ok := g.pains[it.painID]; !ok {
			g.fail(CodePains, it.step, it.para, it.painID, "pain %q is not one this lead's signals allow", it.painID)
		}
		return
	default:
		for _, f := range g.cited(it) {
			if f.Group == GroupLead && !slices.Contains(identityLabels, f.Label) {
				g.fail(CodePains, it.step, it.para, f.ID, "the paragraph cites lead fact %s (%s) but names no pain", f.ID, f.Label)
				return
			}
		}
	}
	if s := g.readerClaim(it); s != "" {
		if it.talking {
			g.fail(CodePains, it.step, it.para, s, "a talking point about the org must cite the lead fact it rests on")
			return
		}
		g.fail(CodePains, it.step, it.para, s, "this speaks about the reader but names no pain; reword an allowed pain with its pain_id, or say only what you will do")
	}
}

// --- reader claims (G5b) ---

// fold lower-cases s and drops Romanian diacritics (both the comma and the cedilla forms), so
// "Tău", "tau" and "TĂU" read alike.
var foldReplacer = strings.NewReplacer("ă", "a", "â", "a", "î", "i", "ș", "s", "ş", "s", "ț", "t", "ţ", "t", "’", "'")

func fold(s string) string { return foldReplacer.Replace(strings.ToLower(s)) }

func wordSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

var (
	// readerWords address the reader: English second person, and the Romanian second person
	// pronouns and possessives (folded). Romanian is read in every draft (the rules carry no
	// language); "tale" is also an English noun, accepted as the cost. The Romanian clitics
	// (te, îți, vă) are left out: they sit in sender sentences ("Vă scriu", "te sun") far
	// more often than in claims.
	readerWords = wordSet("you your you're you've yours yourself yourselves you'll you'd " +
		"tu ta tau tale tai voi vostru voastra vostri voastre dumneavoastra")
	// readerObjectWords are reader words that cannot carry a claim about the reader's things
	// on their own ("it is yours to keep", "if you want it", "theirs to keep").
	readerObjectWords = wordSet("you yours yourself yourselves theirs them tu voi")
	// thirdPersonWords are how a talking point (said to the operator) speaks of the org.
	thirdPersonWords = wordSet("they their theirs them themselves they're they've they'll they'd")
	// senderOpeners open a sentence about the sender's own doing (English pronouns; Romanian
	// eu/noi and the first-person am/sunt/suntem, as the pronoun is usually dropped).
	senderOpeners = wordSet("i i'm i've i'll i'd we we're we've we'll we'd my our eu noi am sunt suntem")
	// knowingWords turn a sender sentence into a report about the reader ("I noticed your…",
	// "Am văzut că site-ul tău…"); "let me know" is not one.
	knowingWords = wordSet("noticed notice noticing saw seen spotted found heard know knew think believe " +
		"understand gather suspect guess imagine realise realize realised realized looked checked " +
		"discovered learned learnt " +
		"vazut observat stiu aflat cred citit gasit remarcat auzit descoperit constatat")
	readerTokenRe = regexp.MustCompile(`[\p{L}'’]+`)
)

func foldedWords(s string) []string {
	ws := readerTokenRe.FindAllString(s, -1)
	for i, w := range ws {
		ws[i] = strings.Trim(fold(w), "'")
	}
	return ws
}

// readerClaim returns the first sentence of it that speaks about the reader and is neither
// the sender's own action nor the offer; "" when there is none.
//
// A sentence speaks about the reader when it holds a reader word (readerWords; for a talking
// point also they/their/them and "the site"), or the org's name or domain. It passes when:
//
//   - nothing in it reports what the sender knows or saw of the reader (knowingWords before
//     the first reader word), and
//   - it is the sender's own action: it opens with a first-person word or the sender's name
//     ("I tried to call you today about your Drupal site"), or
//   - it is the offer: its item cites an offer fact, and the sentence holds the value of a
//     cited offer fact ("the free report … is still yours if you want it") or its only reader
//     words are object words ("It is yours to keep.").
//
// Known limit: a first-person sentence can still carry an adjective about the reader's things
// ("I can fix your broken form"); the prompt asks for pains with pain_id, and review reads it.
func (g *grounder) readerClaim(it groundItem) string {
	var offers []string
	for _, f := range g.cited(it) {
		if f.Group == GroupOffer {
			offers = append(offers, f.Value)
		}
	}
	var orgRefs, senderNames []string
	for _, f := range g.sheet.Facts {
		switch {
		case f.Group == GroupLead && (f.Label == LabelOrgName || f.Label == LabelOrgDomain) && utf8.RuneCountInString(f.Value) >= 3:
			orgRefs = append(orgRefs, f.Value)
		case f.Label == LabelSenderName && f.Value != "":
			senderNames = append(senderNames, f.Value)
		}
	}
	for _, s := range sentenceEndRe.Split(it.text, -1) {
		s = strings.TrimSpace(s)
		ws := foldedWords(s)
		firstReader, objectOnly := -1, true
		for i, w := range ws {
			reader := readerWords[w] || it.talking && (thirdPersonWords[w] ||
				w == "the" && i+1 < len(ws) && (ws[i+1] == "site" || ws[i+1] == "website"))
			if !reader {
				continue
			}
			if firstReader < 0 {
				firstReader = i
			}
			if !readerObjectWords[w] {
				objectOnly = false
			}
		}
		org := slices.ContainsFunc(orgRefs, func(v string) bool { return containsWordFold(s, v) })
		if firstReader < 0 && !org {
			continue
		}
		if org {
			objectOnly = false
			if firstReader < 0 {
				firstReader = len(ws)
			}
		}
		knows := false
		for i := 0; i < firstReader && i < len(ws); i++ {
			letKnow := ws[i] == "know" && i >= 2 && ws[i-2] == "let" && (ws[i-1] == "me" || ws[i-1] == "us")
			if knowingWords[ws[i]] && !letKnow {
				knows = true
			}
		}
		if knows && !it.opener { // in the opener, "I noticed your site" is G5's research opener
			return s
		}
		if len(ws) > 0 && senderOpeners[ws[0]] ||
			slices.ContainsFunc(senderNames, func(n string) bool { return strings.HasPrefix(s, n) }) {
			continue
		}
		if len(offers) > 0 && (objectOnly ||
			slices.ContainsFunc(offers, func(v string) bool { return strings.Contains(fold(s), fold(v)) })) {
			continue
		}
		return s
	}
	return ""
}

// firstStep is the first email of d: its lowest step (0 on the default sequence, 2 on
// call_first, whose emails follow the calls). -1 when d has no step.
func firstStep(d WrittenDraft) int {
	first := -1
	for _, st := range d.Steps {
		if first < 0 || st.Step < first {
			first = st.Step
		}
	}
	return first
}

// G5 the opener: the first email (firstStep), paragraph 1.
func (g *grounder) opener(d WrittenDraft) {
	var p *Paragraph
	first := firstStep(d)
	for i := range d.Steps {
		if d.Steps[i].Step == first && len(d.Steps[i].Paragraphs) > 0 {
			p = &d.Steps[i].Paragraphs[0]
			break
		}
	}
	if p == nil {
		return // G1's
	}
	sender := false
	text := p.Text
	for _, id := range p.Cites {
		f, ok := g.byID[id]
		if !ok {
			continue
		}
		switch f.Group {
		case GroupSender:
			sender = true
			text = strings.ReplaceAll(text, f.Value, " ")
		case GroupLead:
			if !f.EchoGuard {
				g.fail(CodeOpener, 0, 0, id, "the opener cites lead fact %s; open with who you are and why you write", id)
			}
		}
	}
	if !sender {
		g.fail(CodeOpener, 0, 0, "", "the opener cites no sender fact")
	}
	for _, f := range g.sheet.Facts {
		if f.Group != GroupLead || f.EchoGuard || f.Label == LabelKind || f.Label == LabelOrgCountry || utf8.RuneCountInString(f.Value) < 3 {
			continue
		}
		if containsWordFold(text, f.Value) {
			g.fail(CodeOpener, 0, 0, f.Value, "the opener holds lead fact %s; open with who you are, not with research", f.ID)
		}
	}
	openers := g.r.ResearchOpeners
	if openers == nil {
		openers = DefaultResearchOpeners
	}
	for _, re := range openers {
		if m := re.FindString(p.Text); m != "" {
			g.fail(CodeOpener, 0, 0, m, "the opener reads as research; open with who you are and why you write")
		}
	}
}

// echoShingles are the N-word runs of the org's own texts.
func (g *grounder) echoShingles() map[string]bool {
	n := g.echoN()
	set := map[string]bool{}
	var texts []string
	for _, f := range g.sheet.Facts {
		if f.EchoGuard {
			texts = append(texts, f.Value)
		}
	}
	texts = append(texts, g.r.EchoTexts...)
	for _, t := range texts {
		for _, sh := range shingles(echoWords(t), n) {
			set[sh] = true
		}
	}
	return set
}

func (g *grounder) echoN() int {
	if g.r.EchoWords > 0 {
		return g.r.EchoWords
	}
	return DefaultEchoWords
}

// G6 echo. Stated fact values of two words or more are taken out first: quoting a stored
// fact (the org's name, the proof line) is not echoing the org's site.
func (g *grounder) echo(it groundItem, set map[string]bool) {
	if len(set) == 0 {
		return
	}
	segs := []string{it.text}
	for _, f := range g.sheet.Facts {
		if f.EchoGuard || len(strings.Fields(f.Value)) < 2 {
			continue
		}
		var next []string
		for _, s := range segs {
			next = append(next, strings.Split(s, f.Value)...)
		}
		segs = next
	}
	for _, s := range segs {
		for _, sh := range shingles(echoWords(s), g.echoN()) {
			if set[sh] {
				g.fail(CodeEcho, it.step, it.para, sh, "%d words echo the org's own text", g.echoN())
				return
			}
		}
	}
}

// G7 copy rules, with Check's own definitions (checkWordRe, checkURLs, MaxSubjectRunes).
func (g *grounder) copyRules(d WrittenDraft, items []groundItem) {
	for _, it := range items {
		for _, w := range append(slices.Clone(g.r.ConfigBanned), g.r.BannedWords...) {
			if strings.TrimSpace(w) != "" && wordRe(w).MatchString(it.text) {
				g.fail(CodeCopy, it.step, it.para, w, "%q is a banned word", w)
			}
		}
	}
	for _, st := range d.Steps {
		var body []string
		for _, p := range st.Paragraphs {
			body = append(body, p.Text)
		}
		text := strings.Join(body, "\n\n")
		if most, ok := g.r.MaxWords[st.Step]; ok {
			if n := len(strings.Fields(text)); n > most {
				g.fail(CodeCopy, st.Step, ParaSubject, strconv.Itoa(n), "%d words; this email allows at most %d", n, most)
			}
		}
		if n := utf8.RuneCountInString(st.Subject); n > MaxSubjectRunes {
			g.fail(CodeCopy, st.Step, ParaSubject, strconv.Itoa(n), "the subject is %d characters, at most %d", n, MaxSubjectRunes)
		}
		if strings.Contains(st.Subject, "!") {
			g.fail(CodeCopy, st.Step, ParaSubject, "!", "the subject holds \"!\"")
		}
		if g.r.NoExclamation {
			for i, p := range st.Paragraphs {
				if strings.Contains(p.Text, "!") {
					g.fail(CodeCopy, st.Step, i, "!", "this campaign allows no \"!\"")
				}
			}
		}
		if g.r.NoLinksInStep0 && st.Step == firstStep(d) {
			for i, p := range st.Paragraphs {
				for _, u := range checkURLs(p.Text) {
					g.fail(CodeCopy, st.Step, i, u, "the first email may hold no link")
				}
			}
		}
	}
}

// G8 kind phrases.
func (g *grounder) kind(it groundItem) {
	for _, re := range g.r.Forbid {
		if loc := re.FindStringIndex(it.text); loc != nil {
			g.fail(CodeKind, it.step, it.para, wordAround(it.text, loc), "%q is forbidden for this lead's kind", re.String())
		}
	}
}

// wordAround widens a match to the whole words it touches ("migrat" → "migration").
func wordAround(s string, loc []int) string {
	a, b := loc[0], loc[1]
	for a > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:a])
		if !isWordRune(r) {
			break
		}
		a -= n
	}
	for b < len(s) {
		r, n := utf8.DecodeRuneInString(s[b:])
		if !isWordRune(r) {
			break
		}
		b += n
	}
	return s[a:b]
}

var (
	firstPersonRe = regexp.MustCompile(`\bI\b|\b(?:[Mm]e|[Mm]y|[Ww]e|[Oo]ur|[Oo]urs|us|Us)\b`)
	sentenceEndRe = regexp.MustCompile(`[.!?]+(?:\s+|$)`)
)

// G9 proof.
func (g *grounder) proof(it groundItem) {
	proofs := g.sheet.WithLabel(LabelProofLine)
	text := it.text
	for _, f := range proofs {
		text = strings.ReplaceAll(text, f.Value, " ")
	}
	words := g.r.InflationWords
	if words == nil {
		words = DefaultInflationWords
	}
	for _, w := range words {
		if strings.TrimSpace(w) != "" && wordRe(w).MatchString(text) {
			g.fail(CodeProof, it.step, it.para, w, "%q inflates; the one true proof line says enough", w)
		}
	}
	if it.subject {
		return
	}
	cited := g.cited(it)
	sender := false
	for _, f := range cited {
		if f.Group == GroupSender {
			sender = true
		}
		if f.Label == LabelProofLine && !strings.Contains(it.text, f.Value) {
			g.fail(CodeProof, it.step, it.para, f.ID, "the paragraph cites the proof line but does not hold it verbatim")
		}
	}
	if sender {
		return
	}
	for _, s := range sentenceEndRe.Split(it.text, -1) {
		if firstPersonRe.MatchString(s) && len(extractClaims(s)) > 0 {
			g.fail(CodeProof, it.step, it.para, strings.TrimSpace(s), "a first-person claim with a number must cite a sender fact")
		}
	}
}

// CheckText runs G3 (claim tokens) and G8 (kind phrases) over any rendered text, such as the
// upgrade-risk report: every claim must appear in a fact of the sheet (echo_guard facts
// aside) or in one of the allowed texts (the operator's own fixed wording), and no forbid
// pattern may match. Failures carry Step and Para StepTalkingPoints/ParaSubject (-1).
func CheckText(text string, sheet FactSheet, allowed []string, forbid []*regexp.Regexp) []Failure {
	var src []string
	for _, f := range sheet.Facts {
		if !f.EchoGuard {
			src = append(src, f.Value)
		}
	}
	src = append(src, allowed...)
	var out []Failure
	for _, c := range uncoveredClaims(text, src) {
		out = append(out, Failure{Code: CodeClaim, Step: -1, Para: -1, Token: c.raw, Msg: c.raw + " is not a stored fact"})
	}
	for _, re := range forbid {
		if loc := re.FindStringIndex(text); loc != nil {
			out = append(out, Failure{Code: CodeKind, Step: -1, Para: -1, Token: wordAround(text, loc), Msg: fmt.Sprintf("%q is forbidden for this lead's kind", re.String())})
		}
	}
	return out
}

// CheckRendered is G10: the existing Check (CheckIn) on the final rendered email, each
// Problem a G10 failure whose Token is the Check rule. The writer calls it after rendering
// paragraphs + identity line + opt-out line + footer; it should set the Draft's FixedText to
// the grounded paragraphs, as G3 already traced their numbers.
func CheckRendered(d Draft, step int, cfg leadconfig.Config, rules StepRules) []Failure {
	var out []Failure
	for _, p := range CheckIn(d, step, cfg, rules) {
		out = append(out, Failure{Code: CodeRendered, Step: step, Para: ParaSubject, Token: p.Rule, Msg: p.Detail})
	}
	return out
}

// --- claim tokens (G3) ---

// claim kinds.
const (
	claimURL      = "url"
	claimEmail    = "email"
	claimDomain   = "domain"
	claimPhone    = "phone"
	claimMoney    = "money"
	claimPct      = "pct"
	claimDate     = "date"     // YYYY-MM-DD
	claimMonth    = "month"    // YYYY-MM
	claimDayMonth = "daymonth" // MM-DD
	claimYear     = "year"
	claimVersion  = "ver" // dotted, trailing .x dropped
	claimNum      = "num"
)

// claim is one claim token: its kind, its normalised values (a numeric date may read two
// ways), the text as written and where it sits.
type claim struct {
	kind       string
	vals       []string
	raw        string
	start, end int
}

// monthAlt reads English and Romanian month names (a date fact is written in the lead's
// language), longest first within each so a full name wins over its abbreviation.
const monthAlt = `january|february|march|april|may|june|july|august|september|october|november|december|` +
	`ianuarie|februarie|martie|aprilie|mai|iunie|iulie|septembrie|octombrie|noiembrie|decembrie|` +
	`jan|feb|mar|apr|jun|jul|aug|sept|sep|oct|nov|dec`

var (
	gURLRe       = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>"'()]+`)
	gEmailRe     = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	gISODateRe   = regexp.MustCompile(`\b(\d{4})-(\d{1,2})-(\d{1,2})\b`)
	gNumDateRe   = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})[./](\d{4})\b`)
	gPhoneRe     = regexp.MustCompile(`(?:\+|\b00)\d[\d \t().\-/]{6,}\d|\(0\d{1,4}\)[\d \t.\-/]{5,}\d|\b0\d{1,4}(?:[ .\-/]\d{2,5}){2,}\b`)
	gDomainRe    = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,63}\b`)
	gMoneyPreRe  = regexp.MustCompile(`(?i)([$€£]|\b(?:eur|usd|gbp|ron)\b)\s?(\d[\d,]*(?:\.\d+)?)(?:\s?([km])\b)?`)
	gMoneyPostRe = regexp.MustCompile(`(?i)\b(\d[\d,]*(?:\.\d+)?)(?:\s?([km]))?\s?(€|£|\$|(?:eur|euros?|usd|dollars?|gbp|pounds?|ron|lei)\b)`)
	gPctRe       = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s?(?:%|per\s?cent\b)`)
	gDMYRe       = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(?:of\s+)?(` + monthAlt + `)\b\.?,?\s+(\d{4})\b`)
	gMDYRe       = regexp.MustCompile(`(?i)\b(` + monthAlt + `)\b\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	gMYRe        = regexp.MustCompile(`(?i)\b(` + monthAlt + `)\b\.?,?\s+(\d{4})\b`)
	gDMRe        = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(?:of\s+)?(` + monthAlt + `)\b`)
	gMDRe        = regexp.MustCompile(`(?i)\b(` + monthAlt + `)\b\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b`)
	gDrupalVerRe = regexp.MustCompile(`(?i)\b(?:drupal\s?|d)(\d{1,2}(?:\.(?:\d+|x))*)\b`)
	gDotVerRe    = regexp.MustCompile(`\b\d+(?:\.(?:\d+|x))+\b`)
	gNumRe       = regexp.MustCompile(`\d(?:[\d,]*\d)?`)
)

// claimScan takes claims out of a text one pattern at a time, blanking each span so a later
// pattern cannot read it again.
type claimScan struct {
	buf    []byte
	claims []claim
}

func (cs *claimScan) take(re *regexp.Regexp, fn func(text string, m []int) (claim, bool)) {
	text := string(cs.buf)
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		c, ok := fn(text, m)
		if !ok {
			continue
		}
		if c.end == 0 {
			c.start, c.end = m[0], m[1]
		}
		if c.raw == "" {
			c.raw = text[c.start:c.end]
		}
		for i := c.start; i < c.end; i++ {
			cs.buf[i] = ' '
		}
		cs.claims = append(cs.claims, c)
	}
}

func sub(text string, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return text[m[2*i]:m[2*i+1]]
}

var monthNum = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12,
	"ianuarie": 1, "februarie": 2, "martie": 3, "aprilie": 4, "mai": 5, "iunie": 6, "iulie": 7,
	"septembrie": 9, "octombrie": 10, "noiembrie": 11, "decembrie": 12,
}

func monthOf(s string) int {
	s = strings.ToLower(s)
	if n, ok := monthNum[s]; ok {
		return n
	}
	if len(s) >= 3 {
		return monthNum[s[:3]]
	}
	return 0
}

// validDate formats y-m-d when it is a real date.
func validDate(y, m, d int) (string, bool) {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != m || t.Day() != d {
		return "", false
	}
	return t.Format(time.DateOnly), true
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// normNum is a number without separators, in its shortest form ("1,000" → "1000", "05" → "5").
func normNum(s string) string {
	s = strings.ReplaceAll(s, ",", "")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// normVersion is a dotted version with leading zeros and trailing ".x" parts dropped.
func normVersion(s string) string {
	parts := strings.Split(strings.ToLower(s), ".")
	for len(parts) > 1 && parts[len(parts)-1] == "x" {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		if n, err := strconv.Atoi(p); err == nil {
			parts[i] = strconv.Itoa(n)
		}
	}
	return strings.Join(parts, ".")
}

func normCurrency(s string) string {
	switch s = strings.ToLower(s); {
	case s == "$" || strings.HasPrefix(s, "usd") || strings.HasPrefix(s, "dollar"):
		return "usd"
	case s == "€" || strings.HasPrefix(s, "eur"):
		return "eur"
	case s == "£" || strings.HasPrefix(s, "gbp") || strings.HasPrefix(s, "pound"):
		return "gbp"
	default:
		return "ron"
	}
}

func moneyVal(cur, amount, mult string) string {
	a := strings.ReplaceAll(amount, ",", "")
	f, err := strconv.ParseFloat(a, 64)
	if err != nil {
		return normCurrency(cur) + ":" + a
	}
	switch strings.ToLower(mult) {
	case "k":
		f *= 1e3
	case "m":
		f *= 1e6
	}
	return normCurrency(cur) + ":" + strconv.FormatFloat(f, 'f', -1, 64)
}

func urlKey(u string) string {
	u = strings.ToLower(u)
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	u = strings.TrimPrefix(u, "www.")
	return strings.TrimRight(u, "/")
}

func glued(text string, start, end int) bool {
	if start > 0 {
		if r, _ := utf8.DecodeLastRuneInString(text[:start]); checkGlued(r) || r == '.' || r == ',' {
			return true
		}
	}
	if end < len(text) {
		if r, _ := utf8.DecodeRuneInString(text[end:]); checkGlued(r) {
			return true
		}
	}
	return false
}

// extractClaims lists the claim tokens of a text: URLs, emails, dates, phones, domains,
// money, percentages, versions ("Drupal 7", "D10", "7.x", "10.3") and bare numbers, in that
// order of precedence.
func extractClaims(text string) []claim {
	cs := &claimScan{buf: []byte(text)}
	cs.take(gURLRe, func(t string, m []int) (claim, bool) {
		raw := strings.TrimRight(t[m[0]:m[1]], ".,;:!?)]}'\"")
		return claim{kind: claimURL, vals: []string{urlKey(raw)}, raw: raw, start: m[0], end: m[0] + len(raw)}, true
	})
	cs.take(gEmailRe, func(t string, m []int) (claim, bool) {
		return claim{kind: claimEmail, vals: []string{strings.ToLower(t[m[0]:m[1]])}}, true
	})
	cs.take(gISODateRe, func(t string, m []int) (claim, bool) {
		d, ok := validDate(atoi(sub(t, m, 1)), atoi(sub(t, m, 2)), atoi(sub(t, m, 3)))
		return claim{kind: claimDate, vals: []string{d}}, ok
	})
	cs.take(gNumDateRe, func(t string, m []int) (claim, bool) {
		a, b, y := atoi(sub(t, m, 1)), atoi(sub(t, m, 2)), atoi(sub(t, m, 3))
		var vals []string
		if d, ok := validDate(y, b, a); ok { // day first
			vals = append(vals, d)
		}
		if d, ok := validDate(y, a, b); ok && !slices.Contains(vals, d) {
			vals = append(vals, d)
		}
		return claim{kind: claimDate, vals: vals}, len(vals) > 0
	})
	cs.take(gPhoneRe, func(t string, m []int) (claim, bool) {
		_, norm, ok := enrich.NormalizePhone(t[m[0]:m[1]])
		return claim{kind: claimPhone, vals: []string{norm}}, ok
	})
	cs.take(gDomainRe, func(t string, m []int) (claim, bool) {
		if m[0] > 0 && (t[m[0]-1] == '@' || t[m[0]-1] == '.') {
			return claim{}, false
		}
		return claim{kind: claimDomain, vals: []string{urlKey(t[m[0]:m[1]])}}, true
	})
	money := func(cur, amount, mult int) func(string, []int) (claim, bool) {
		return func(t string, m []int) (claim, bool) {
			return claim{kind: claimMoney, vals: []string{moneyVal(sub(t, m, cur), sub(t, m, amount), sub(t, m, mult))}}, true
		}
	}
	cs.take(gMoneyPreRe, money(1, 2, 3))
	cs.take(gMoneyPostRe, money(3, 1, 2))
	cs.take(gPctRe, func(t string, m []int) (claim, bool) {
		return claim{kind: claimPct, vals: []string{normNum(sub(t, m, 1))}}, true
	})
	cs.take(gDMYRe, func(t string, m []int) (claim, bool) {
		d, ok := validDate(atoi(sub(t, m, 3)), monthOf(sub(t, m, 2)), atoi(sub(t, m, 1)))
		return claim{kind: claimDate, vals: []string{d}}, ok
	})
	cs.take(gMDYRe, func(t string, m []int) (claim, bool) {
		d, ok := validDate(atoi(sub(t, m, 3)), monthOf(sub(t, m, 1)), atoi(sub(t, m, 2)))
		return claim{kind: claimDate, vals: []string{d}}, ok
	})
	cs.take(gMYRe, func(t string, m []int) (claim, bool) {
		return claim{kind: claimMonth, vals: []string{fmt.Sprintf("%04d-%02d", atoi(sub(t, m, 2)), monthOf(sub(t, m, 1)))}}, true
	})
	daymonth := func(day, mon int) func(string, []int) (claim, bool) {
		return func(t string, m []int) (claim, bool) {
			d, mo := atoi(sub(t, m, day)), monthOf(sub(t, m, mon))
			_, ok := validDate(2000, mo, d) // 2000 is a leap year: 29 Feb passes
			return claim{kind: claimDayMonth, vals: []string{fmt.Sprintf("%02d-%02d", mo, d)}}, ok
		}
	}
	cs.take(gDMRe, daymonth(1, 2))
	cs.take(gMDRe, daymonth(2, 1))
	cs.take(gDrupalVerRe, func(t string, m []int) (claim, bool) {
		return claim{kind: claimVersion, vals: []string{normVersion(sub(t, m, 1))}}, true
	})
	cs.take(gDotVerRe, func(t string, m []int) (claim, bool) {
		if glued(t, m[0], m[1]) {
			return claim{}, false
		}
		return claim{kind: claimVersion, vals: []string{normVersion(t[m[0]:m[1]])}}, true
	})
	cs.take(gNumRe, func(t string, m []int) (claim, bool) {
		if glued(t, m[0], m[1]) {
			return claim{}, false
		}
		raw := t[m[0]:m[1]]
		if n, err := strconv.Atoi(raw); err == nil && len(raw) == 4 && n >= 1900 && n <= 2100 {
			return claim{kind: claimYear, vals: []string{raw}}, true
		}
		return claim{kind: claimNum, vals: []string{normNum(raw)}}, true
	})
	cs.takeNumberWords()
	slices.SortFunc(cs.claims, func(a, b claim) int { return a.start - b.start })
	return cs.claims
}

// --- spelled-out numbers (G3, G9) ---

// numWord is one number word: its value and how it combines.
type numWord struct {
	val  int
	kind numKind
}

type numKind int

const (
	numSmall numKind = iota // 1-99: units, teens, tens
	numMult                 // hundred, thousand, million, dozen; Romanian sută/sute, mie/mii
	numOrd                  // an ordinal: ends its phrase
)

// numberWords are the spelled-out numbers read as claims, folded (fold). A spelled number is
// a claim wherever a digit is (G3 and G9 read digits everywhere), so the words that are
// ordinary English or Romanian far more often than counts are kept out on their own:
// "one" (one of, no one, the one), "first" and "second" (adverbs, a unit of time), "dozens",
// "hundreds", "thousands", "sute", "mii" (vague quantities: G9's boast words), the Romanian
// articles "un", "o", "una", and "noua" without its diacritic ("the new"). "one", "first",
// "second", a multiplier and "o"/"a" before a multiplier still count inside a number:
// "twenty-one", "one hundred", "twenty-first", "a dozen", "o sută".
var numberWords = func() map[string]numWord {
	m := map[string]numWord{}
	small := func(words string, from, step int) {
		for i, w := range strings.Fields(words) {
			m[w] = numWord{val: from + i*step}
		}
	}
	small("one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen", 1, 1)
	small("twenty thirty forty fifty sixty seventy eighty ninety", 20, 10)
	small("unu doi trei patru cinci sase sapte opt noua zece unsprezece doisprezece treisprezece paisprezece cincisprezece saisprezece saptesprezece optsprezece nouasprezece", 1, 1)
	small("douazeci treizeci patruzeci cincizeci saizeci saptezeci optzeci nouazeci", 20, 10)
	m["doua"] = numWord{val: 2}
	m["douasprezece"] = numWord{val: 12}
	for w, v := range map[string]int{"hundred": 100, "thousand": 1000, "million": 1000000, "dozen": 12, "suta": 100, "sute": 100, "mie": 1000, "mii": 1000} {
		m[w] = numWord{val: v, kind: numMult}
	}
	for i, w := range strings.Fields("first second third fourth fifth sixth seventh eighth ninth tenth eleventh twelfth thirteenth fourteenth fifteenth sixteenth seventeenth eighteenth nineteenth") {
		m[w] = numWord{val: i + 1, kind: numOrd}
	}
	for i, w := range strings.Fields("twentieth thirtieth fortieth fiftieth sixtieth seventieth eightieth ninetieth") {
		m[w] = numWord{val: 20 + i*10, kind: numOrd}
	}
	return m
}()

var (
	numTokenRe   = regexp.MustCompile(`\p{L}+`)
	drupalBefore = regexp.MustCompile(`(?i)\bdrupal\s+$`)
	optOutAfter  = regexp.MustCompile(`(?i)^[\s-]*(?:out|in)\b`)
	// after "nouă" these words make it "new" or "to us", not nine ("o versiune nouă de Drupal").
	nouaNotNine = wordSet("de si pentru ca in la cu din pe a al ale ai sau iar dar mai")
)

// fits says whether a small number can follow cur within one number: "twenty" + "one",
// "hundred" + "five", never "twenty" + "thirty" or "five" + "six".
func fits(cur, v int) bool {
	low := cur % 100
	if v < 10 {
		return low == 0 || low >= 20 && low%10 == 0
	}
	return low == 0
}

// takeNumberWords takes the spelled-out numbers left in the text, each as one claim: a num
// (forty-two, a dozen, douăzeci și cinci) or, right after "Drupal", a version ("Drupal six").
func (cs *claimScan) takeNumberWords() {
	text := string(cs.buf)
	toks := numTokenRe.FindAllStringIndex(text, -1)
	word := func(i int) string { return fold(text[toks[i][0]:toks[i][1]]) }
	joined := func(i int) bool { // toks[i-1] and toks[i] are joined by spaces or hyphens only
		gap := text[toks[i-1][1]:toks[i][0]]
		return gap != "" && strings.Trim(gap, " -") == ""
	}
	isNum := func(i int) (numWord, bool) {
		if i >= len(toks) {
			return numWord{}, false
		}
		nw, ok := numberWords[word(i)]
		return nw, ok
	}
	for i := 0; i < len(toks); {
		start := i
		if w := word(i); (w == "a" || w == "o") && i+1 < len(toks) && joined(i+1) {
			if nw, ok := isNum(i + 1); ok && nw.kind == numMult {
				i++ // the article of "a dozen", "o sută"
			}
		}
		total, cur, n, j := 0, 0, 0, i
		for ; j < len(toks); j++ {
			if j > i && !joined(j) {
				break
			}
			if n > 0 && (word(j) == "and" || word(j) == "si") { // "one hundred and twenty", "douăzeci și cinci"
				nx, ok := isNum(j + 1)
				if !ok || nx.kind == numMult || !joined(j+1) || !fits(cur, nx.val) {
					break
				}
				continue
			}
			nw, ok := isNum(j)
			if !ok {
				break
			}
			switch nw.kind {
			case numMult:
				if n == 0 && start == j && nw.val != 12 {
					ok = false // "hundred", "mie" ("to me") with no number before it
				} else if nw.val >= 1000 {
					total += max(cur, 1) * nw.val
					cur = 0
				} else if cur < 100 {
					cur = max(cur, 1) * nw.val
				} else {
					ok = false
				}
			default:
				if n > 0 && !fits(cur, nw.val) {
					ok = false
				} else {
					cur += nw.val
				}
			}
			if !ok {
				break
			}
			n++
			if nw.kind == numOrd {
				j++
				break
			}
		}
		if n == 0 {
			i = start + 1
			continue
		}
		i = j
		end := toks[j-1][1]
		raw := text[toks[start][0]:end]
		if n == 1 && start == j-1 { // one word on its own, no article
			switch w := word(j - 1); {
			case w == "one" || w == "first" || w == "second":
				continue
			case w == "opt" && optOutAfter.MatchString(text[end:]):
				continue
			case w == "noua":
				prev := ""
				if j-2 >= 0 {
					prev = word(j - 2)
				}
				if strings.ToLower(raw) != "nouă" || slices.Contains([]string{"o", "cea", "una", "aceasta"}, prev) ||
					j >= len(toks) || !joined(j) || nouaNotNine[word(j)] {
					continue
				}
			}
		}
		c := claim{kind: claimNum, vals: []string{strconv.Itoa(total + cur)}, raw: raw, start: toks[start][0], end: end}
		if loc := drupalBefore.FindStringIndex(text[:c.start]); loc != nil {
			c.kind, c.start = claimVersion, loc[0]
			c.raw = text[c.start:end]
		}
		for k := c.start; k < c.end; k++ {
			cs.buf[k] = ' '
		}
		cs.claims = append(cs.claims, c)
	}
}

// uncoveredClaims are the claims of text that no source covers.
func uncoveredClaims(text string, sources []string) []claim {
	var src []claim
	for _, s := range sources {
		src = append(src, extractClaims(s)...)
	}
	var out []claim
	for _, c := range extractClaims(text) {
		if !covered(c, src) {
			out = append(out, c)
		}
	}
	return out
}

func overlap(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// covered reports whether a source claim backs c: the same kind and value, or a looser
// reading that is still true (a year or month of a stated date, "Drupal 10" of 10.3.1, a
// bare number of a version or year, a homepage of a stated domain, a domain of a stated URL
// or email).
func covered(c claim, src []claim) bool {
	for _, s := range src {
		if s.kind == c.kind && overlap(s.vals, c.vals) {
			return true
		}
		for _, cv := range c.vals {
			for _, sv := range s.vals {
				if looseMatch(c.kind, cv, s.kind, sv) {
					return true
				}
			}
		}
	}
	return false
}

func looseMatch(ck, cv, sk, sv string) bool {
	switch ck {
	case claimYear:
		return (sk == claimDate || sk == claimMonth) && strings.HasPrefix(sv, cv+"-") || sk == claimNum && sv == cv
	case claimMonth:
		return sk == claimDate && strings.HasPrefix(sv, cv+"-")
	case claimDayMonth:
		return sk == claimDate && strings.HasSuffix(sv, "-"+cv)
	case claimVersion:
		return sk == claimVersion && strings.HasPrefix(sv, cv+".") || sk == claimNum && sv == cv
	case claimNum:
		return sk == claimVersion && sv == cv || sk == claimYear && sv == cv ||
			sk == claimMoney && strings.HasSuffix(sv, ":"+cv)
	case claimURL:
		return sk == claimDomain && sv == cv
	case claimDomain:
		if sk == claimURL {
			host, _, _ := strings.Cut(sv, "/")
			return host == cv
		}
		return sk == claimEmail && strings.HasSuffix(sv, "@"+cv)
	}
	return false
}

// --- names (G4, G4b) ---

var nameExempt = func() map[string]bool {
	m := map[string]bool{"I": true, "I'm": true, "I've": true, "I'll": true, "I'd": true}
	for _, w := range strings.Fields("January February March April May June July August September October November December " +
		"Jan Feb Mar Apr Jun Jul Aug Sep Sept Oct Nov Dec Monday Tuesday Wednesday Thursday Friday Saturday Sunday") {
		m[w] = true
	}
	return m
}()

// blankClaims replaces every claim span with spaces.
func blankClaims(text string) string {
	b := []byte(text)
	for _, c := range extractClaims(text) {
		for i := c.start; i < c.end; i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

// capitalRuns lists the runs of capitalised words that do not start a sentence (claim
// tokens taken out first: they are G3's).
func capitalRuns(text string) []string {
	var runs []string
	for _, line := range strings.Split(blankClaims(text), "\n") {
		start := true
		var run []string
		flush := func() {
			if len(run) > 0 {
				runs = append(runs, strings.Join(run, " "))
				run = nil
			}
		}
		for _, field := range strings.Fields(line) {
			core := strings.TrimLeft(field, "\"'([{“‘")
			core = strings.TrimRight(core, ".,;:!?\"')]}”’")
			t := strings.TrimRight(field, "\"')]}”’")
			ends := strings.HasSuffix(t, ".") || strings.HasSuffix(t, "!") || strings.HasSuffix(t, "?")
			for _, suf := range []string{"'s", "’s"} {
				core = strings.TrimSuffix(core, suf)
			}
			r, _ := utf8.DecodeRuneInString(core)
			capital := core != "" && unicode.IsUpper(r)
			switch {
			case !capital || start || nameExempt[core]:
				flush()
			default:
				run = append(run, core)
			}
			if strings.HasSuffix(field, ",") || strings.HasSuffix(field, ";") || strings.HasSuffix(field, ":") {
				flush()
			}
			start = ends
			if ends {
				flush()
			}
		}
		flush()
	}
	return runs
}

// nameAllowed: the run is in a source as written, or each of its capitalised parts is.
func nameAllowed(run string, src []string) bool {
	in := func(w string) bool {
		for _, s := range src {
			if containsWord(s, w) {
				return true
			}
		}
		return false
	}
	if in(run) {
		return true
	}
	for _, w := range strings.FieldsFunc(run, func(r rune) bool { return r == ' ' || r == '-' }) {
		if r, _ := utf8.DecodeRuneInString(w); unicode.IsUpper(r) && !in(w) {
			return false
		}
	}
	return true
}

var orgNameStop = map[string]bool{"the": true, "and": true, "ltd": true, "limited": true, "inc": true, "llc": true, "gmbh": true, "plc": true}

// orgNameMisuse returns how text misnames the org: the full name in another case, or a
// capitalised word of the name outside an exact use. "" when the name is used exactly or
// not at all. Words in allowed (copy.allowed_names) are the sender's own, never a misuse.
func orgNameMisuse(text, name string, allowed []string) string {
	rest := strings.ReplaceAll(text, name, " ")
	if containsWordFold(rest, name) {
		return name
	}
	for _, w := range strings.Fields(name) {
		w = strings.Trim(w, ".,&()'\"")
		if utf8.RuneCountInString(w) < 4 || orgNameStop[strings.ToLower(w)] || slices.Contains(allowed, w) {
			continue
		}
		for _, m := range wordRe(w).FindAllStringSubmatch(rest, -1) {
			if r, _ := utf8.DecodeRuneInString(m[1]); unicode.IsUpper(r) {
				return m[1]
			}
		}
	}
	return ""
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// containsWord reports whether w is in s as written, not inside a longer word.
func containsWord(s, w string) bool {
	if w == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before, _ := utf8.DecodeLastRuneInString(s[:j])
		after, _ := utf8.DecodeRuneInString(s[j+len(w):])
		if (j == 0 || !isWordRune(before)) && (j+len(w) == len(s) || !isWordRune(after)) {
			return true
		}
		i = j + 1
	}
}

// wordRes caches wordRe: Ground matches the same words on every item and every draft.
var wordRes sync.Map

// wordRe matches w as a whole word in any case, w itself captured; the same match as
// Check's checkWordRe, compiled once per word.
func wordRe(w string) *regexp.Regexp {
	w = strings.TrimSpace(w)
	if re, ok := wordRes.Load(w); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(` + regexp.QuoteMeta(w) + `)(?:$|[^\p{L}\p{N}_])`)
	wordRes.Store(w, re)
	return re
}

// containsWordFold is containsWord in any case.
func containsWordFold(s, w string) bool {
	return strings.TrimSpace(w) != "" && wordRe(w).MatchString(s)
}

// --- echo (G6) ---

// echoWords are the lower-case letter and digit runs of s.
func echoWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func shingles(words []string, n int) []string {
	var out []string
	for i := 0; i+n <= len(words); i++ {
		out = append(out, strings.Join(words[i:i+n], " "))
	}
	return out
}
