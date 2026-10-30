package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// MailboxesBackend is what the Mailboxes pages read and write. A mailbox's password never
// comes back from it: the console seals a new one on its way in and can never read one.
// Like the leads backend, it returns ErrLeadsNotSetUp when this server has no usable leads
// settings.
type MailboxesBackend interface {
	Mailboxes(ctx context.Context) (MailboxesPage, error)
	Mailbox(ctx context.Context, id int64) (MailboxPage, error)               // ErrNotFound for no such mailbox
	MailboxCheckCard(ctx context.Context, id int64) (MailboxCheckView, error) // ErrNotFound for no such mailbox
	MailboxAct(ctx context.Context, a MailboxAction) (MailboxOutcome, error)  // a refusal wraps ErrUserFixable
}

// The writes MailboxAction.Do names.
const (
	MailboxAdd          = "add"           // Input
	MailboxEdit         = "edit"          // ID, Input (Address and Preset unused)
	MailboxCheck        = "check"         // ID
	MailboxPause        = "pause"         // ID, Reason
	MailboxResume       = "resume"        // ID
	MailboxRetire       = "retire"        // ID, Confirm
	MailboxPauseDomain  = "pause_domain"  // Domain, Reason
	MailboxResumeDomain = "resume_domain" // Domain
)

// MailboxAction is one write from a Mailboxes page.
type MailboxAction struct {
	Do      string
	ID      int64  // the mailbox
	Domain  string // the registrable domain, for a domain pause or resume
	Input   MailboxInput
	Reason  string
	Confirm bool // the retire form's box
}

// MailboxInput is a mailbox as the operator types it in the add or edit form. On add, a
// blank login is the address and blank servers and selector come from Preset; on edit, a
// blank Password keeps the saved one. Password is a config.Secret, so it prints as
// "[redacted]" wherever the action is printed or logged.
type MailboxInput struct {
	Address, Login, SMTPAddr, IMAPAddr, Selector, FromName, Preset string
	Password                                                       config.Secret
}

// MailboxOutcome is what a write did: the line to show, and the mailbox an add made.
type MailboxOutcome struct {
	Message   string
	MailboxID int64
}

// The states MailboxRow.State and SendingMailbox.State take; mailboxState says each in
// plain words.
const (
	MailboxReady      = "ready"       // active, its latest check passed, its records passed today
	MailboxNeedsCheck = "needs_check" // active, but no passed check or no record pass today
	MailboxChecking   = "checking"    // a check waits or runs
	MailboxPaused     = "paused"      // Reason says why
	MailboxRetired    = "retired"
)

// The outcomes MailboxCheckView.Outcome takes; "" is no check yet.
const (
	MailboxCheckRequested = "requested"
	MailboxCheckPassed    = "passed"
	MailboxCheckFailed    = "failed"
	MailboxCheckDidNotRun = "did_not_run"
)

// MailboxesPage is every mailbox, grouped by its registrable domain, with what the page
// must say: the old env lines still set but no longer read, whether the mailbox key is
// missing (no add then), and the presets for the add form.
type MailboxesPage struct {
	Groups     []MailboxDomain
	RetiredEnv []string // the old mailbox variables still set in the environment
	KeyMissing bool     // the public key is missing or broken: no password can be sealed
	Presets    []MailboxPreset
}

// MailboxDomain is one registrable domain's mailboxes: the limits and pause count here,
// while DNS is checked on each address's own host.
type MailboxDomain struct {
	Domain                  string
	DNS                     []HostCheck // today's records, per host and selector of its active mailboxes
	SentToday, DomainLimit  int         // today's live sends from the domain, and caps.domain_per_day
	Mailboxes, MailboxLimit int         // its mailboxes not retired, and caps.mailboxes_per_domain
	Paused                  bool        // the domain is paused as a whole
	PauseReason             string
	Rows                    []MailboxRow
}

// HostCheck is today's record check of one host and DKIM selector; At is nil when none ran
// today.
type HostCheck struct {
	Host, Selector   string
	At               *time.Time
	SPF, DKIM, DMARC bool
}

// MailboxRow is one mailbox in the list.
type MailboxRow struct {
	ID                  int64
	Address             string
	State, Reason       string // one of the Mailbox* states; Reason for a pause
	DomainPause         bool   // paused with its whole domain
	SentToday, CapToday int    // today's live sends, and today's ramp limit
	RampDay             int    // days since its first live send, from 1, as the ramp counts them; 0 before it
	// RampStart is caps.start_date when it is a date still ahead (RampDay 0 then): the warm-up
	// starts on that day, not with the mailbox's first email. Nil otherwise.
	RampStart *time.Time
	LastCheck *time.Time // when its latest check finished; nil when none has
}

// MailboxPreset is a mail provider's public servers and DKIM selector, which fill the add
// form.
type MailboxPreset struct {
	Name, SMTP, IMAP, Selector string
}

// MailboxPage is one mailbox in full, never with its password.
type MailboxPage struct {
	Row                                           MailboxRow
	Domain                                        string // registrable: the group its limits and pauses count in
	Login, SMTPAddr, IMAPAddr, Selector, FromName string
	KeyMissing                                    bool // a new password cannot be saved
	Check                                         MailboxCheckView
}

// MailboxCheckView is a mailbox's latest check, as its card shows it. A step that never ran
// is nil; Fix is the plain fix a failed check stored.
type MailboxCheckView struct {
	MailboxID                                   int64
	Outcome                                     string // one of the MailboxCheck* outcomes, or "" for none yet
	RequestedAt, CheckedAt                      *time.Time
	SMTPOK, IMAPOK, FoldersOK, SPF, DKIM, DMARC *bool
	Fix                                         string
	Retired                                     bool // no new check can be asked for
	// NotStarted: the check was asked for more than CheckNotStartedAfter ago and has still
	// not started (its request was missed), so the card stops polling and offers Check now.
	NotStarted bool
}

// CheckNotStartedAfter is how long a requested check may wait before its card says it has
// not started; a check normally starts within seconds and ends within a minute.
const CheckNotStartedAfter = 5 * time.Minute

// SendingMailbox is one mailbox on the Leads page's sending card.
type SendingMailbox struct {
	Address, Domain     string
	State, Reason       string // one of the Mailbox* states
	SentToday, CapToday int
	DNS                 *SenderCheck // today's records for its host and selector; nil when not checked today
}

// MessageFrom is the From a drafted email goes out under.
type MessageFrom struct{ Name, Address string }

// mailboxState is a mailbox state in the words the operator reads.
func mailboxState(state, reason string) string {
	switch state {
	case MailboxReady:
		return "Ready"
	case MailboxNeedsCheck:
		return "Needs a check"
	case MailboxChecking:
		return "Checking…"
	case MailboxPaused:
		if reason != "" {
			return "Paused: " + reason
		}
		return "Paused"
	case MailboxRetired:
		return "Retired"
	}
	return state
}

const (
	mailboxesPath = "/mailboxes"
	mailboxPath   = "/mailboxes/"
	maxMailbox    = 320 // the longest address, login, server or name a field takes
)

// mailboxesView is the list with the line from the last action.
type mailboxesView struct {
	Page    MailboxesPage
	Message string
	Error   bool
}

// mailboxView is one mailbox with the line from the last action, and its sending card (nil
// when the backend has none for it).
type mailboxView struct {
	Page    MailboxPage
	Sending *MailboxSendingView
	Message string
	Error   bool
}

// LandingWords is where a mailbox's domain sends a visitor, in words, for its card.
func (v MailboxSendingView) LandingWords() string {
	switch v.Landing.State {
	case LandingFound:
		return v.Landing.URL
	case LandingNoAnswer:
		return "the domain's website didn't answer"
	}
	return "not checked yet"
}

// mailboxFormView is the add or edit form. It has no password field: a password typed in
// is never written back into a page.
type mailboxFormView struct {
	Edit                                         bool
	ID                                           int64
	Address, Login, SMTPAddr, IMAPAddr, Selector string
	FromName, Preset                             string
	Presets                                      []MailboxPreset
	KeyMissing                                   bool
	Message                                      string
	Error                                        bool
}

// mailboxCheckView is the check card as the template draws it, with the line from the last
// press.
type mailboxCheckView struct {
	Card    MailboxCheckView
	Message string
	Error   bool
}

// Polling reports whether the check waits or runs, so the card asks for itself again; a
// check that has not started in time stops it.
func (v mailboxCheckView) Polling() bool {
	return v.Card.Outcome == MailboxCheckRequested && !v.Card.NotStarted
}

// checkTick is one step of a finished check: ok, bad or none (never ran).
type checkTick struct{ Label, State string }

// Ticks are the check's steps in the order they run.
func (v mailboxCheckView) Ticks() []checkTick {
	c := v.Card
	steps := []struct {
		label string
		ok    *bool
	}{
		{"SMTP sign-in", c.SMTPOK}, {"IMAP sign-in", c.IMAPOK}, {"Sent and Junk folders", c.FoldersOK},
		{"SPF", c.SPF}, {"DKIM", c.DKIM}, {"DMARC", c.DMARC},
	}
	out := make([]checkTick, len(steps))
	for i, s := range steps {
		state := "none"
		if s.ok != nil && *s.ok {
			state = "ok"
		} else if s.ok != nil {
			state = "bad"
		}
		out[i] = checkTick{Label: s.label, State: state}
	}
	return out
}

// mailboxLog writes one line for a mailbox action: its name and outcome, never what was
// submitted.
func (s *server) mailboxLog(do, outcome string) {
	s.log.Info("console mailboxes", "action", do, "outcome", outcome)
}

// mailboxRead answers a mailbox read that failed: the not-set-up page, a 404, or a 500. It
// reports whether err was one.
func (s *server) mailboxRead(w http.ResponseWriter, r *http.Request, code int, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrLeadsNotSetUp):
		s.render(w, r, code, "leads_off", page{Title: "Mailboxes", Active: mailboxesPath, SignedIn: true})
	case errors.Is(err, ErrNotFound):
		s.render(w, r, http.StatusNotFound, "leads_off", page{Title: "Mailboxes", Active: mailboxesPath, SignedIn: true,
			Message: "There is no such mailbox.", Error: true, Data: true})
	default:
		s.leadsFail(w, r, err)
	}
	return true
}

// mailboxes is GET /mailboxes.
func (s *server) mailboxes(w http.ResponseWriter, r *http.Request) {
	s.renderMailboxes(w, r, http.StatusOK, s.takeFlash(w, r), false)
}

func (s *server) renderMailboxes(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	p, err := s.b.Mailboxes(r.Context())
	if s.mailboxRead(w, r, code, err) {
		return
	}
	s.render(w, r, code, "mailboxes", page{Title: "Mailboxes", Active: mailboxesPath, SignedIn: true,
		Data: mailboxesView{Page: p, Message: msg, Error: isErr}})
}

// newMailbox is GET /mailboxes/new?preset=NAME: the add form, its servers and selector
// filled from the preset when one is named. It reads only.
func (s *server) newMailbox(w http.ResponseWriter, r *http.Request) {
	s.renderAddForm(w, r, http.StatusOK, mailboxFormView{Preset: r.URL.Query().Get("preset")}, true)
}

// renderAddForm writes the add form f. With fill, a known preset fills the servers and
// selector; an unknown one is dropped.
func (s *server) renderAddForm(w http.ResponseWriter, r *http.Request, code int, f mailboxFormView, fill bool) {
	p, err := s.b.Mailboxes(r.Context())
	if s.mailboxRead(w, r, code, err) {
		return
	}
	f.Presets, f.KeyMissing = p.Presets, p.KeyMissing
	preset := f.Preset
	f.Preset = ""
	for _, pr := range p.Presets {
		if strings.EqualFold(pr.Name, strings.TrimSpace(preset)) {
			f.Preset = pr.Name
			if fill {
				f.SMTPAddr, f.IMAPAddr, f.Selector = pr.SMTP, pr.IMAP, pr.Selector
			}
		}
	}
	s.render(w, r, code, "mailbox_form", page{Title: "Add a mailbox", Active: mailboxesPath, SignedIn: true, Data: f})
}

// mailboxInput reads the add or edit form. Every field but the password is trimmed; the
// password is taken exactly as typed.
func mailboxInput(f url.Values, add bool) MailboxInput {
	in := MailboxInput{
		Login:    strings.TrimSpace(f.Get("login")),
		SMTPAddr: strings.TrimSpace(f.Get("smtp_addr")),
		IMAPAddr: strings.TrimSpace(f.Get("imap_addr")),
		Selector: strings.TrimSpace(f.Get("selector")),
		FromName: strings.TrimSpace(f.Get("from_name")),
		Password: config.Secret(f.Get("password")),
	}
	if add {
		in.Address = strings.TrimSpace(f.Get("address"))
		in.Preset = strings.TrimSpace(f.Get("preset"))
	}
	return in
}

// formOf is the form as typed, without the password.
func formOf(in MailboxInput) mailboxFormView {
	return mailboxFormView{Address: in.Address, Login: in.Login, SMTPAddr: in.SMTPAddr, IMAPAddr: in.IMAPAddr,
		Selector: in.Selector, FromName: in.FromName, Preset: in.Preset}
}

// addMailbox is POST /mailboxes: done, it goes to the new mailbox, whose check card shows
// its first check; refused, the form comes back with every field but the password.
func (s *server) addMailbox(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	in := mailboxInput(r.PostForm, true)
	s.mailboxAct(w, r, MailboxAction{Do: MailboxAdd, Input: in}, mailboxesPath,
		func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
			f := formOf(in)
			f.Message, f.Error = msg, isErr
			s.renderAddForm(w, r, code, f, false)
		})
}

// mailbox is GET /mailboxes/{id}.
func (s *server) mailbox(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.renderMailbox(w, r, http.StatusOK, id, s.takeFlash(w, r), false)
}

func (s *server) renderMailbox(w http.ResponseWriter, r *http.Request, code int, id int64, msg string, isErr bool) {
	p, err := s.b.Mailbox(r.Context(), id)
	if s.mailboxRead(w, r, code, err) {
		return
	}
	v := mailboxView{Page: p, Message: msg, Error: isErr}
	// A mailbox gone between the two reads keeps its page without the sending card.
	switch snd, err := s.b.MailboxSending(r.Context(), id); {
	case err == nil:
		v.Sending = &snd
	case !errors.Is(err, ErrNotFound) && s.mailboxRead(w, r, code, err):
		return
	}
	s.render(w, r, code, "mailbox", page{Title: p.Row.Address, Active: mailboxesPath, SignedIn: true, Data: v})
}

// backToMailbox re-renders mailbox id with a refusal.
func (s *server) backToMailbox(id int64) func(http.ResponseWriter, *http.Request, int, string, bool) {
	return func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
		s.renderMailbox(w, r, code, id, msg, isErr)
	}
}

// editMailboxForm is GET /mailboxes/{id}/edit: the form holding the mailbox's settings.
func (s *server) editMailboxForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.renderEditForm(w, r, http.StatusOK, id, nil, "", false)
}

// renderEditForm writes the edit form for mailbox id: its saved settings, or typed when
// a refused edit comes back.
func (s *server) renderEditForm(w http.ResponseWriter, r *http.Request, code int, id int64, typed *MailboxInput, msg string, isErr bool) {
	p, err := s.b.Mailbox(r.Context(), id)
	if s.mailboxRead(w, r, code, err) {
		return
	}
	f := mailboxFormView{Login: p.Login, SMTPAddr: p.SMTPAddr, IMAPAddr: p.IMAPAddr, Selector: p.Selector, FromName: p.FromName}
	if typed != nil {
		f = formOf(*typed)
	}
	f.Edit, f.ID, f.Address, f.KeyMissing, f.Message, f.Error = true, id, p.Row.Address, p.KeyMissing, msg, isErr
	s.render(w, r, code, "mailbox_form", page{Title: "Edit " + p.Row.Address, Active: mailboxesPath, SignedIn: true, Data: f})
}

// editMailbox is POST /mailboxes/{id}: a blank password keeps the saved one.
func (s *server) editMailbox(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	in := mailboxInput(r.PostForm, false)
	s.mailboxAct(w, r, MailboxAction{Do: MailboxEdit, ID: id, Input: in}, mailboxPath+strconv.FormatInt(id, 10),
		func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
			s.renderEditForm(w, r, code, id, &in, msg, isErr)
		})
}

// checkMailbox is POST /mailboxes/{id}/check. A press from the card (htmx) gets the card
// back in place, with the outcome or the refusal on it; a plain form goes back to the
// mailbox.
func (s *server) checkMailbox(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	a := MailboxAction{Do: MailboxCheck, ID: id}
	if !isHX(r) {
		s.mailboxAct(w, r, a, mailboxPath+strconv.FormatInt(id, 10), s.backToMailbox(id))
		return
	}
	out, err := s.b.MailboxAct(r.Context(), a)
	switch {
	case errors.Is(err, ErrUserFixable):
		s.mailboxLog(a.Do, "refused")
		s.renderCheckCard(w, r, id, refusalWords(err), true)
	case errors.Is(err, ErrLeadsNotSetUp):
		s.mailboxLog(a.Do, "not set up")
		s.setFlash(w, leadsNotSetUp)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	case err != nil:
		s.mailboxLog(a.Do, "failed")
		s.leadsFail(w, r, err)
	default:
		s.mailboxLog(a.Do, "ok")
		s.renderCheckCard(w, r, id, out.Message, false)
	}
}

// mailboxCheck is GET /mailboxes/{id}/check: the check card alone, which the card polls
// while the check waits or runs. A plain visit goes to the mailbox.
func (s *server) mailboxCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !isHX(r) {
		http.Redirect(w, r, mailboxPath+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	s.renderCheckCard(w, r, id, "", false)
}

// renderCheckCard writes the card fragment, read fresh, with msg on it.
func (s *server) renderCheckCard(w http.ResponseWriter, r *http.Request, id int64, msg string, isErr bool) {
	c, err := s.b.MailboxCheckCard(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrLeadsNotSetUp):
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		s.leadsFail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, s.pages["mailbox_check"], "mailbox_check", mailboxCheckView{Card: c, Message: msg, Error: isErr})
}

// pauseMailbox is POST /mailboxes/{id}/pause, with an optional reason.
func (s *server) pauseMailbox(w http.ResponseWriter, r *http.Request) {
	s.mailboxSwitch(w, r, MailboxPause)
}

// resumeMailbox is POST /mailboxes/{id}/resume.
func (s *server) resumeMailbox(w http.ResponseWriter, r *http.Request) {
	s.mailboxSwitch(w, r, MailboxResume)
}

// retireMailbox is POST /mailboxes/{id}/retire, with the confirm box; the backend refuses
// it without the box.
func (s *server) retireMailbox(w http.ResponseWriter, r *http.Request) {
	s.mailboxSwitch(w, r, MailboxRetire)
}

func (s *server) mailboxSwitch(w http.ResponseWriter, r *http.Request, do string) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	a := MailboxAction{Do: do, ID: id}
	switch do {
	case MailboxPause:
		a.Reason = strings.TrimSpace(r.PostForm.Get("reason"))
	case MailboxRetire:
		a.Confirm = r.PostForm.Get("confirm") == "yes"
	}
	s.mailboxAct(w, r, a, mailboxPath+strconv.FormatInt(id, 10), s.backToMailbox(id))
}

// pauseDomain is POST /mailboxes/domains/{domain}/pause: every mailbox of the domain.
func (s *server) pauseDomain(w http.ResponseWriter, r *http.Request) {
	s.domainSwitch(w, r, MailboxPauseDomain)
}

// resumeDomain is POST /mailboxes/domains/{domain}/resume.
func (s *server) resumeDomain(w http.ResponseWriter, r *http.Request) {
	s.domainSwitch(w, r, MailboxResumeDomain)
}

func (s *server) domainSwitch(w http.ResponseWriter, r *http.Request, do string) {
	domain := strings.TrimSpace(r.PathValue("domain"))
	if domain == "" {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	a := MailboxAction{Do: do, Domain: domain}
	if do == MailboxPauseDomain {
		a.Reason = strings.TrimSpace(r.PostForm.Get("reason"))
	}
	s.mailboxAct(w, r, a, mailboxesPath, func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
		s.renderMailboxes(w, r, code, msg, isErr)
	})
}

// mailboxAct runs a and answers: on success a redirect to next (or to the mailbox an add
// made) with the outcome in the one-time cookie; on a refusal back renders with it (422).
func (s *server) mailboxAct(w http.ResponseWriter, r *http.Request, a MailboxAction, next string,
	back func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool)) {
	out, err := s.b.MailboxAct(r.Context(), a)
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.mailboxLog(a.Do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Mailboxes", Active: mailboxesPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.mailboxLog(a.Do, "refused")
		back(w, r, http.StatusUnprocessableEntity, refusalWords(err), true)
		return
	case err != nil:
		s.mailboxLog(a.Do, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.mailboxLog(a.Do, "ok")
	if a.Do == MailboxAdd && out.MailboxID > 0 {
		next = mailboxPath + strconv.FormatInt(out.MailboxID, 10)
	}
	s.setFlash(w, out.Message)
	http.Redirect(w, r, next, http.StatusSeeOther)
}
