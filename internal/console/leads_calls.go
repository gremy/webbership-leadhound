package console

import (
	"cmp"
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/campaign"
)

// leadCallPath is the start of a call card's path: /leads/calls/{message}.
const leadCallPath = "/leads/calls/"

// The outcome form's refusals, in plain words.
const (
	badOutcome     = "Choose what happened on the call: one of the six buttons. Nothing was saved."
	noteTooLong    = "The note is longer than 500 characters; shorten it. Nothing was saved."
	badConsentAddr = "That email address does not look right; check it or leave it empty to use theirs. Nothing was saved."
	callsOffWords  = "Calls are off."
	maxAddress     = 254
)

// callOutcomeWords are the six outcome buttons' words, keyed by campaign.Outcomes.
var callOutcomeWords = map[string]string{
	campaign.OutcomeConnected:   "Got through",
	campaign.OutcomeNoAnswer:    "No answer",
	campaign.OutcomeVoicemail:   "Left a voicemail",
	campaign.OutcomeWrongNumber: "Wrong number",
	campaign.OutcomeDoNotCall:   "Do not call",
	campaign.OutcomeSkipped:     "Skipped",
}

// callOutcomeHelp is what each outcome does next, in plain words, for the card.
var callOutcomeHelp = map[string]string{
	campaign.OutcomeConnected:   "The sequence ends; add a one-line note on how it went.",
	campaign.OutcomeNoAnswer:    "The \"tried to reach you\" email goes the next business day.",
	campaign.OutcomeVoicemail:   "The \"tried to reach you\" email goes the next business day.",
	campaign.OutcomeWrongNumber: "No more calls to this number; the last email still goes.",
	campaign.OutcomeDoNotCall:   "Their number is never called again and the sequence ends, even in a shadow batch.",
	campaign.OutcomeSkipped:     "No call this time; the last email still goes.",
}

// OutcomeWords is a call outcome in the outcome buttons' own words ("Got through"); one the
// console does not know is shown with its underscores as spaces. It is the one source of
// these words: the card, the saved line, Today and the lead page all use it.
func OutcomeWords(o string) string {
	if w, ok := callOutcomeWords[o]; ok {
		return w
	}
	return strings.ReplaceAll(o, "_", " ")
}

// cancelWords is why a step was cancelled, in words.
func cancelWords(reason string) string {
	switch reason {
	case campaign.CancelCondition:
		return "not needed after the step before"
	case campaign.CancelCallConnected:
		return "the call got through, so the sequence ended"
	case campaign.CancelWrongNumber:
		return "the number was wrong"
	case campaign.CancelCallSkipped:
		return "the call was skipped"
	case campaign.CancelDoNotCall:
		return "they asked not to be called, so the sequence ended"
	case campaign.CancelNoConsent:
		return "the call got through but they did not agree to an email"
	case "opt_out", "opt-out":
		return "they opted out"
	case "bounce", "bounced":
		return "an email bounced"
	case "reply", "replied", "replied_no":
		return "they replied"
	case "hand-contacted":
		return "you contacted them by hand"
	case "batch discarded":
		return "the batch was discarded"
	case "manual", "suppressed":
		return "they are on the do-not-contact list"
	case "":
		return "no reason on file"
	}
	return strings.ReplaceAll(reason, "_", " ")
}

// lineKindWords is a phone line's kind in words.
func lineKindWords(k string) string {
	switch k {
	case "direct":
		return "a direct line"
	case "decision-maker":
		return "the decision-maker's own line"
	case "switchboard":
		return "the switchboard"
	case "":
		return "not known"
	}
	return k
}

// telHref is the card's dial number (CallCard.Dial, the stored normalised number) as a tel:
// link takes it, when it is one: an optional leading + then digits only. Anything else gives
// "", so the card shows the number as text with no link. It never rebuilds a number from the
// text as printed: "+44 (0)20 ..." keeps the national 0 there, and dialling it is wrong.
func telHref(dial string) string {
	digits := strings.TrimPrefix(dial, "+")
	if digits == "" {
		return ""
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return dial
}

// seqRow is one step of a lead's Sequence section, worded.
type seqRow struct {
	Step  int
	Kind  string // "an email" | "a phone call"
	Words string
	Card  string // the call card's path, for a call row; ""
}

// sequenceRows words a lead's steps; times in the console's zone.
func (s *server) sequenceRows(steps []SequenceStep) []seqRow {
	at := func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return ""
		}
		return " " + t.In(s.opt.Location).Format("Mon 2 Jan 15:04")
	}
	out := make([]seqRow, 0, len(steps))
	for i, st := range steps {
		r := seqRow{Step: st.Step, Kind: "an email"}
		if st.Type == "call" {
			r.Kind = "a phone call"
			if st.MessageID > 0 {
				r.Card = leadCallPath + strconv.FormatInt(st.MessageID, 10)
			}
		}
		switch st.State {
		case SeqDraft:
			r.Words = "drafted, waiting for the batch to be approved"
		case SeqDue:
			r.Words = "due" + at(st.At)
		case SeqWaiting:
			r.Words = "waiting"
			if p, ok := waitsOn(steps, i); ok {
				r.Words = "waiting on " + WaitsOnWords(p.Type, p.Step, st.Step)
			}
		case SeqSent:
			r.Words = "sent" + at(st.At)
			if st.Shadow {
				r.Words = "sent in shadow" + at(st.At) + " (nothing left the server)"
			}
		case SeqDone:
			r.Words = "called" + at(st.At)
			if st.Outcome != "" {
				r.Words += ": " + strings.ToLower(OutcomeWords(st.Outcome))
			}
			if n := shortNote(st.CallNote); n != "" {
				r.Words += ` · "` + n + `"`
			}
		case SeqCancelled:
			r.Words = "cancelled: " + cancelWords(st.Reason)
		case SeqNoNumber:
			r.Words = "skipped because there was no number we may call"
		case SeqCheck:
			r.Words = "may have gone out: check it on the Outreach page"
		default:
			r.Words = strings.ReplaceAll(st.State, "_", " ")
		}
		if st.Note != "" {
			r.Words += " · " + st.Note
		}
		out = append(out, r)
	}
	return out
}

// seqNoteMax is how many characters of the operator's call note the Sequence section shows.
const seqNoteMax = 120

// shortNote is note on one line, cut to seqNoteMax characters with "…" when longer. The
// template escapes it.
func shortNote(note string) string {
	n := strings.Join(strings.Fields(note), " ")
	if utf8.RuneCountInString(n) <= seqNoteMax {
		return n
	}
	r := []rune(n)
	return strings.TrimSpace(string(r[:seqNoteMax-1])) + "…"
}

// waitsOn is the step steps[i] waits on: the nearest earlier step that has a row and was
// not cancelled (a step skipped for no number, or cancelled, is passed over).
func waitsOn(steps []SequenceStep, i int) (SequenceStep, bool) {
	for j := i - 1; j >= 0; j-- {
		if p := steps[j]; p.MessageID > 0 && p.State != SeqCancelled && p.State != SeqNoNumber {
			return p, true
		}
	}
	return SequenceStep{}, false
}

// WaitsOnWords names the step a step waits on: "the call" or "the email before it" when it
// is the step just before, else with its 1-based step number ("the email at step 2").
func WaitsOnWords(prevType string, prevStep, step int) string {
	what := "the email"
	if prevType == "call" {
		what = "the call"
	}
	if prevStep == step-1 {
		if prevType == "call" {
			return what
		}
		return what + " before it"
	}
	return what + " at step " + strconv.Itoa(prevStep+1)
}

// callView is what the call card renders: the backend's card, the outcomes in order, the
// line from the last action and the note the form keeps after a refusal.
type callView struct {
	C        CallCard
	Outcomes []string
	Back     string
	Message  string
	Error    bool
	Note     string
	CallBack bool   // the form offers a call-back date (the backend has the CRM)
	Today    string // YYYY-MM-DD, the call-back field's earliest day
}

// messageID reads the {message} path segment as a positive id, or reports false.
func messageID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("message"), 10, 64)
	return id, err == nil && id > 0
}

// leadCall is GET /leads/calls/{message}: the call card. It never writes.
func (s *server) leadCall(w http.ResponseWriter, r *http.Request) {
	id, ok := messageID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	msg, isErr := strings.CutPrefix(s.takeFlash(w, r), errFlash)
	s.renderCall(w, r, http.StatusOK, id, msg, isErr, "")
}

// errFlash marks a flash that is a refusal, shown as an error. A control character, so no
// message a backend words can start with it.
const errFlash = "\x1ferr\x1f"

func (s *server) renderCall(w http.ResponseWriter, r *http.Request, code int, id int64, msg string, isErr bool, note string) {
	c, err := s.b.LeadCall(r.Context(), id)
	if s.leadsRead(w, r, code, err) {
		return
	}
	if !c.Callable && c.BlockedReason == "" {
		c.BlockedReason = callsOffWords
	}
	title := cmp.Or(c.OrgName, "Call "+strconv.FormatInt(id, 10))
	s.render(w, r, code, "lead_call", page{Title: "Call " + title, Active: leadsPath, SignedIn: true,
		Data: callView{C: c, Outcomes: campaign.Outcomes, Back: leadCallPath + strconv.FormatInt(id, 10),
			Message: msg, Error: isErr, Note: note, CallBack: s.hasCRM(), Today: s.today().Format(time.DateOnly)}})
}

// leadCallOutcome is POST /leads/calls/{message}/outcome: outcome (one of the six), an
// optional note (at most 500 characters) and, for connected, consent and consent_address.
// An outcome the form never offers is refused (400) before the backend; a long note or a
// bad address shows the card again with the refusal (422), keeping the note. A refusal of
// the backend's ("not due yet") goes back (303) to the card, which shows it as an error.
// Saved, it goes back (303) to the page back names (the card when none) with its line.
func (s *server) leadCallOutcome(w http.ResponseWriter, r *http.Request) {
	const do = "call-outcome"
	id, ok := messageID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	outcome := strings.TrimSpace(r.PostForm.Get("outcome"))
	if !slices.Contains(campaign.Outcomes, outcome) {
		s.leadsLog(do, "refused")
		http.Error(w, badOutcome, http.StatusBadRequest)
		return
	}
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if utf8.RuneCountInString(note) > maxLeadsText {
		s.leadsLog(do, "refused")
		s.renderCall(w, r, http.StatusUnprocessableEntity, id, noteTooLong, true, note)
		return
	}
	var consent bool
	var address string
	if outcome == campaign.OutcomeConnected {
		consent = r.PostForm.Get("consent") == "yes"
		if consent {
			address = strings.TrimSpace(r.PostForm.Get("consent_address"))
			if address != "" && !plainAddress(address) {
				s.leadsLog(do, "refused")
				s.renderCall(w, r, http.StatusUnprocessableEntity, id, badConsentAddr, true, note)
				return
			}
		}
	}
	callBack, refusal := s.callBackDay(r.PostForm.Get("call_back_on"), outcome)
	crm, hasCRM := s.crmBackend()
	if refusal == "" && callBack != nil && !hasCRM {
		refusal = callBackNoCRM
	}
	if refusal != "" {
		s.leadsLog(do, "refused")
		s.renderCall(w, r, http.StatusUnprocessableEntity, id, refusal, true, note)
		return
	}
	back := leadCallPath + strconv.FormatInt(id, 10)
	if b := r.PostForm.Get("back"); b != "" {
		back = leadsBack(b)
	}
	var msg string
	var err error
	if hasCRM {
		msg, err = crm.LeadCallOutcomeOn(r.Context(), CallOutcomeRequest{MessageID: id, Outcome: outcome, Note: note,
			Consent: consent, ConsentAddress: address, CallBackOn: callBack})
	} else {
		msg, err = s.b.LeadCallOutcome(r.Context(), id, outcome, note, consent, address)
	}
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.leadsLog(do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		// Back to the card (post, redirect, get), so a reload never posts again.
		s.leadsLog(do, "refused")
		s.setFlash(w, errFlash+refusalWords(err))
		http.Redirect(w, r, leadCallPath+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	case errors.Is(err, ErrNotFound):
		s.leadsLog(do, "not found")
		s.leadsRead(w, r, http.StatusNotFound, err)
		return
	case err != nil:
		s.leadsLog(do, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.leadsLog(do, "ok")
	if msg == "" && callBack != nil {
		msg = "Saved: " + OutcomeWords(outcome) + ". Call back on " + callBack.Format("Mon 2 Jan") + "."
	}
	s.setFlash(w, cmp.Or(msg, "Saved: "+OutcomeWords(outcome)+"."))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// plainAddress reports whether a is one bare email address, at most 254 characters.
func plainAddress(a string) bool {
	if len(a) > maxAddress {
		return false
	}
	p, err := mail.ParseAddress(a)
	return err == nil && p.Name == "" && p.Address == a && strings.Contains(a, "@")
}

// AfterColon is a sentence as it reads after a colon ("Not callable: not screened"): its
// first letter lower-cased, unless the first word is an acronym or a name in capitals
// ("TPS", "UK") or the text is empty.
func AfterColon(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 || !unicode.IsUpper(r) {
		return s
	}
	if r2, _ := utf8.DecodeRuneInString(s[n:]); unicode.IsUpper(r2) {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}
