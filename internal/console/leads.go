package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// LeadsBackend is what the leads pages read and write. Every read and write loads the
// leads settings afresh; when they are missing or broken it returns ErrLeadsNotSetUp.
type LeadsBackend interface {
	Leads(ctx context.Context) (LeadsPage, error) // the Outreach page
	// LeadList is one page of the Leads list under the query's filter.
	LeadList(ctx context.Context, q LeadListQuery) (LeadListPage, error)
	// LeadDetail is everything stored about one lead; ErrNotFound for no such lead.
	LeadDetail(ctx context.Context, id int64) (LeadDetailPage, error)
	LeadBatch(ctx context.Context, id int64) (LeadBatch, error) // ErrNotFound for no such batch
	LeadsNext(ctx context.Context) (LeadsQueue, error)
	// LeadPriorities is the priority profile as a form with the top of the queue under it:
	// the profile in use when edit is nil, else edit (nothing is saved). An edit with
	// problems comes back with them and no preview.
	LeadPriorities(ctx context.Context, edit *PriorityForm) (PrioritiesPage, error)
	// LeadAct runs one write. A refusal the operator can fix wraps ErrUserFixable with
	// the words to show.
	LeadAct(ctx context.Context, a LeadAction) (LeadOutcome, error)
}

// ErrLeadsNotSetUp is what the leads backend returns when this server has no usable
// leads settings.
var ErrLeadsNotSetUp = errors.New("console: leads are not set up")

// leadsNotSetUp is the leads pages' line when ErrLeadsNotSetUp comes back.
const leadsNotSetUp = "Leads are not set up on this server yet."

// The writes LeadAction.Do names.
const (
	LeadApprove      = "approve"          // ID batch, Hash, Skip
	LeadDiscard      = "discard"          // ID batch
	LeadNewBatch     = "new-batch"        //
	LeadResolve      = "resolve"          // ID message, Sent
	LeadDone         = "done"             // ID message
	LeadSuppress     = "suppress"         // Kind, Value, Reason
	LeadLift         = "lift"             // ID, Value (the domain typed to confirm), Reason
	LeadContacted    = "contacted"        // Value (a domain), Note
	LeadPause        = "pause"            // Reason
	LeadResume       = "resume"           //
	LeadPin          = "pin"              // ID lead
	LeadDefer        = "defer"            // ID lead, Until
	LeadSkip         = "skip"             // ID lead
	LeadClear        = "clear"            // ID lead
	LeadSavePriority = "save-priorities"  // Form
	LeadResetPrio    = "reset-priorities" //
)

// LeadAction is one write from a leads page.
type LeadAction struct {
	Do                               string
	ID                               int64 // the batch, message or lead Do names
	Hash                             string
	Skip                             []int
	Kind, Value, Reason, Note, Until string
	Sent                             bool
	Form                             PriorityForm
}

// LeadOutcome is what a write did: the line to show, and the batch a new batch made.
type LeadOutcome struct {
	Message string
	BatchID int64
}

// LeadsSummary is the dashboard's leads card, read live.
type LeadsSummary struct {
	Paused                           *time.Time // when the operator paused sending; nil when not paused
	Stopped                          string     // why the latest send run stopped sending, or ""
	Batch                            *WaitingBatch
	Replies, ManualWaiting           int
	CheckByHand                      int
	SentYesterday, Bounced, OptedOut int
}

// LeadsPage is the leads overview.
type LeadsPage struct {
	Day          time.Time
	Sending      Sending
	Batches      []WaitingBatch // every batch waiting for approval, newest first
	Manual       []ManualItem   // approved contact forms and LinkedIn notes not yet used
	CheckByHand  []CheckItem    // live emails that may have gone out
	Replies      []string       // registrable domains of replies and opt-outs since yesterday
	Other        int            // unmatched messages in the mailbox since yesterday
	Checked      int            // sites probed yesterday
	Drupal       int            // of those, on Drupal
	Suppressions Suppressions
}

// Sending is the sending state: the operator's switch, today's sends against the ramp, and
// yesterday's health.
type Sending struct {
	SetUp                            bool // sending is set up, so a batch made here is live
	Paused                           bool
	PausedAt                         time.Time
	PauseReason                      string
	SentToday, CapToday              int
	SentYesterday, Bounced, OptedOut int
	Stopped                          string           // why the latest send run stopped sending, or ""
	DNS                              *SenderCheck     // the latest check of the sending domain; nil when none
	Mailboxes                        []SendingMailbox // each mailbox not retired, grouped by domain
}

// SenderCheck is one check of the sending domain's records.
type SenderCheck struct {
	At               time.Time
	SPF, DKIM, DMARC bool
}

// WaitingBatch is a batch waiting for approval.
type WaitingBatch struct {
	ID                             int64
	Shadow                         bool
	CreatedAt                      time.Time
	Leads, Emails, Forms, LinkedIn int
}

// ManualItem is an approved contact form or LinkedIn note to use by hand.
type ManualItem struct {
	MessageID, BatchID     int64
	Domain, Channel, Route string // Route: the form page, for a contact form
}

// CheckItem is a live email that may have gone out: maybe sent, or left sending.
type CheckItem struct {
	MessageID int64
	Domain    string
	Step      int
	State     string
	At        *time.Time // the attempt
}

// Suppressions is the suppression list: counts by kind and reason, addresses masked.
// Rows are the active ones, newest first, capped (More counts the rest); Lifted are the
// lifted ones, newest lift first (FO6).
type Suppressions struct {
	Counts []SuppressionCount
	Rows   []SuppressionRow
	More   int
	Lifted []SuppressionRow
}

// SuppressionRow is one suppression as the page lists it. Value is already masked by the
// backend; LiftedAt is nil while the suppression is active.
type SuppressionRow struct {
	ID                          int64
	Kind, Value, Reason, Source string
	At                          time.Time
	LiftedAt                    *time.Time
	LiftReason, LiftSource      string
}

// SuppressionCount is the suppressions of one kind and reason.
type SuppressionCount struct {
	Kind, Reason string
	N            int
}

// LeadBatch is one batch in full. HashPrefix is set only when the batch can be approved:
// waiting, and its messages still hash to what was drafted, as recomputed for this read.
type LeadBatch struct {
	ID                      int64
	Shadow                  bool
	CreatedAt               time.Time
	ApprovedAt, DiscardedAt *time.Time
	Changed                 bool
	HashPrefix              string
	Items                   []LeadItem
}

// LeadItem is one lead of a batch, numbered as the approval's skip list.
type LeadItem struct {
	N                            int
	LeadID                       int64
	Domain, Kind, Channel, Route string
	Finding                      []string // the finding and each piece of evidence, one line each
	Messages                     []LeadMessage
}

// LeadMessage is one drafted message.
type LeadMessage struct {
	ID                                int64
	Label, Channel, To, Subject, Body string
	From                              MessageFrom // an email's From, fixed when it was drafted
	State, CancelReason               string      // State "" while a draft
}

// LeadsQueue is the next leads, in the order batches draw them.
type LeadsQueue struct {
	DeferUntil string // the default push-back date, YYYY-MM-DD
	Leads      []QueueLead
}

// QueueLead is one lead of the queue with its score and the reasons for it.
type QueueLead struct {
	LeadID                                  int64
	Domain, Kind, Version, Country, Channel string
	Score                                   int
	Why                                     []string
	Pinned                                  bool
}

// PriorityForm is the priority profile as the form holds it. Points are keyed
// "kind.<kind>", "major.<n>" and "channel.<channel>"; a blank value is no rule.
// Countries and RankBands are "GB 20, DE 10" and "10000 15, 100000 8"; Exclude is "FR, IT".
type PriorityForm struct {
	Points                                      []PointField
	ExactVersion, Countries, RankBands, Exclude string
}

// PointField is one points input.
type PointField struct{ Key, Group, Label, Value string }

// PrioritiesPage is the priorities form and the queue's top under it.
type PrioritiesPage struct {
	FromDB   bool // the profile in use is the operator's saved one, not the file's defaults
	Edited   bool // the form shows an edit, not the profile in use
	Form     PriorityForm
	Problems []string
	Preview  []QueueLead
}

// The leads pages' paths.
const (
	leadsPath      = "/leads"          // the list
	leadsOutPath   = "/leads/outreach" // sending, batches, replies and suppressions
	leadsNextPath  = "/leads/next"     // GET goes to the list in the outreach order
	leadsOrderPath = "/leads?sort=order"
	leadsPrioPath  = "/leads/priorities"
	leadBatchPath  = "/leads/batches/"
	maxLeadsText   = 500 // the longest reason or note a form takes, as the inputs' maxlength
	approveLiveFmt = "Approve: these emails send in each recipient's business hours over the next days, within the warm-up ramp, a few minutes apart, and their follow-ups send automatically unless a reply stops them."
)

// leadsView is what a leads page renders: the backend's data and the line from the
// last action.
type leadsView struct {
	Data    any
	Message string
	Error   bool
}

// pathID reads the {name} path segment as a positive id, or reports false.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

// leadsLog writes one line for a leads action: its name and outcome, never what was
// submitted.
func (s *server) leadsLog(do, outcome string) {
	s.log.Info("console leads", "action", do, "outcome", outcome)
}

// leadsFail logs err under the route's pattern (a path holds submitted ids) and answers
// 500 with a plain line.
func (s *server) leadsFail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("console error", "route", r.Pattern, "err", err.Error())
	http.Error(w, serverFailed, http.StatusInternalServerError)
}

// leadsRead answers a leads read that failed: the not-set-up page, a 404, or a 500. It
// reports whether err was one.
func (s *server) leadsRead(w http.ResponseWriter, r *http.Request, code int, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrLeadsNotSetUp):
		s.render(w, r, code, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
	case errors.Is(err, ErrNotFound):
		s.render(w, r, http.StatusNotFound, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true,
			Message: "There is no such item.", Error: true, Data: true})
	default:
		s.leadsFail(w, r, err)
	}
	return true
}

// leadsOutreach is GET /leads/outreach: the page that was /leads before the list.
func (s *server) leadsOutreach(w http.ResponseWriter, r *http.Request) {
	s.renderLeads(w, r, http.StatusOK, s.takeFlash(w, r), false)
}

func (s *server) renderLeads(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	p, err := s.b.Leads(r.Context())
	if s.leadsRead(w, r, code, err) {
		return
	}
	s.render(w, r, code, "leads_outreach", page{Title: "Outreach", Active: leadsPath, SignedIn: true,
		Data: leadsView{Data: p, Message: msg, Error: isErr}})
}

// leadBatch is GET /leads/batches/{id}: the whole batch, and the approve form when it can
// be approved.
func (s *server) leadBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.renderBatch(w, r, http.StatusOK, id, s.takeFlash(w, r), false)
}

func (s *server) renderBatch(w http.ResponseWriter, r *http.Request, code int, id int64, msg string, isErr bool) {
	b, err := s.b.LeadBatch(r.Context(), id)
	if s.leadsRead(w, r, code, err) {
		return
	}
	s.render(w, r, code, "lead_batch", page{Title: "Batch " + strconv.FormatInt(id, 10), Active: leadsPath, SignedIn: true,
		Data: leadsView{Data: b, Message: msg, Error: isErr}})
}

// leadsNext is GET /leads/next: the list in the outreach order. The one-time message is
// left for the list to show.
func (s *server) leadsNext(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, leadsOrderPath, http.StatusSeeOther)
}

// renderNext shows the queue with a refusal from one of its POSTs.
func (s *server) renderNext(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	q, err := s.b.LeadsNext(r.Context())
	if s.leadsRead(w, r, code, err) {
		return
	}
	s.render(w, r, code, "leads_next", page{Title: "Next leads", Active: leadsPath, SignedIn: true,
		Data: leadsView{Data: q, Message: msg, Error: isErr}})
}

// leadPriorities is GET /leads/priorities.
func (s *server) leadPriorities(w http.ResponseWriter, r *http.Request) {
	s.renderPriorities(w, r, http.StatusOK, nil, s.takeFlash(w, r), false)
}

func (s *server) renderPriorities(w http.ResponseWriter, r *http.Request, code int, edit *PriorityForm, msg string, isErr bool) {
	p, err := s.b.LeadPriorities(r.Context(), edit)
	if s.leadsRead(w, r, code, err) {
		return
	}
	s.render(w, r, code, "leads_priorities", page{Title: "Lead priorities", Active: leadsPath, SignedIn: true,
		Data: leadsView{Data: p, Message: msg, Error: isErr}})
}

// leadsForm reads a leads POST form; a body over the cap or an unreadable form has been
// answered when it reports false.
func leadsForm(w http.ResponseWriter, r *http.Request) bool {
	ok, tooBig := parseForm(w, r)
	if tooBig {
		return false
	}
	if !ok {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return false
	}
	return true
}

// leadsAct runs a and answers: on success a redirect to next (or to the new batch) with
// the outcome in the one-time cookie; on a refusal the page back renders with it (422).
func (s *server) leadsAct(w http.ResponseWriter, r *http.Request, a LeadAction, next string,
	back func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool)) {
	out, err := s.b.LeadAct(r.Context(), a)
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.leadsLog(a.Do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.leadsLog(a.Do, "refused")
		back(w, r, http.StatusUnprocessableEntity, refusalWords(err), true)
		return
	case err != nil:
		s.leadsLog(a.Do, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.leadsLog(a.Do, "ok")
	if out.BatchID > 0 {
		next = leadBatchPath + strconv.FormatInt(out.BatchID, 10)
	}
	s.setFlash(w, out.Message)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// refusalWords are the words of an ErrUserFixable error, after its prefix.
func refusalWords(err error) string {
	return strings.TrimPrefix(strings.TrimPrefix(err.Error(), ErrUserFixable.Error()), ": ")
}

// backToLeads, backToNext re-render the Outreach page and the queue with a refusal.
func (s *server) backToLeads(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	s.renderLeads(w, r, code, msg, isErr)
}

func (s *server) backToNext(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	s.renderNext(w, r, code, msg, isErr)
}

// backToBatch re-renders batch id with a refusal.
func (s *server) backToBatch(id int64) func(http.ResponseWriter, *http.Request, int, string, bool) {
	return func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
		s.renderBatch(w, r, code, id, msg, isErr)
	}
}

// approveBatch is POST /leads/batches/{id}/approve: the hash the page showed and the items
// left out.
func (s *server) approveBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	a := LeadAction{Do: LeadApprove, ID: id, Hash: strings.TrimSpace(r.PostForm.Get("hash"))}
	for _, v := range r.PostForm["skip"] {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			s.leadsLog(a.Do, "refused")
			s.renderBatch(w, r, http.StatusUnprocessableEntity, id, "The items to leave out could not be read; nothing was approved.", true)
			return
		}
		a.Skip = append(a.Skip, n)
	}
	slices.Sort(a.Skip)
	s.leadsAct(w, r, a, leadBatchPath+strconv.FormatInt(id, 10), s.backToBatch(id))
}

// discardBatch is POST /leads/batches/{id}/discard.
func (s *server) discardBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	s.leadsAct(w, r, LeadAction{Do: LeadDiscard, ID: id}, leadsOutPath, s.backToBatch(id))
}

// newBatch is POST /leads/batches/new: the next batch, sized to the ramp; it goes to the
// new batch's page.
func (s *server) newBatch(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	s.leadsAct(w, r, LeadAction{Do: LeadNewBatch}, leadsOutPath, s.backToLeads)
}

// resolveMessage is POST /leads/messages/{id}/resolve with sent=sent|not-sent.
func (s *server) resolveMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	sent := r.PostForm.Get("sent")
	if sent != "sent" && sent != "not-sent" {
		s.leadsLog(LeadResolve, "refused")
		s.renderLeads(w, r, http.StatusUnprocessableEntity, "Say whether the email is in the Sent folder.", true)
		return
	}
	s.leadsAct(w, r, LeadAction{Do: LeadResolve, ID: id, Sent: sent == "sent"}, leadsOutPath, s.backToLeads)
}

// itemDone is POST /leads/items/{id}/done: a contact form or LinkedIn note was used.
func (s *server) itemDone(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	s.leadsAct(w, r, LeadAction{Do: LeadDone, ID: id}, leadsOutPath, s.backToLeads)
}

// suppress is POST /leads/suppress: kind, value and reason.
func (s *server) suppress(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	s.leadsAct(w, r, LeadAction{Do: LeadSuppress, Kind: f.Get("kind"), Value: strings.TrimSpace(f.Get("value")), Reason: f.Get("reason")},
		leadsOutPath, s.backToLeads)
}

// liftSuppression is POST /leads/suppressions/lift: the suppression's number, its domain
// typed to confirm, and why it was wrong (FO6). The backend checks the domain and reason.
func (s *server) liftSuppression(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	id, err := strconv.ParseInt(strings.TrimSpace(f.Get("id")), 10, 64)
	if err != nil || id < 1 {
		s.leadsLog(LeadLift, "refused")
		s.renderLeads(w, r, http.StatusUnprocessableEntity, "Give the number of the suppression to lift.", true)
		return
	}
	s.leadsAct(w, r, LeadAction{Do: LeadLift, ID: id, Value: strings.TrimSpace(f.Get("domain")), Reason: strings.TrimSpace(f.Get("reason"))},
		leadsOutPath, s.backToLeads)
}

// contacted is POST /leads/contacted: a domain reached by hand, with a note.
func (s *server) contacted(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	s.leadsAct(w, r, LeadAction{Do: LeadContacted, Value: strings.TrimSpace(f.Get("domain")), Note: strings.TrimSpace(f.Get("note"))},
		leadsOutPath, s.backToLeads)
}

// sending is POST /leads/sending: do=pause (with a reason) or do=resume.
func (s *server) sending(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	a := LeadAction{Do: LeadResume}
	switch r.PostForm.Get("do") {
	case "pause":
		a = LeadAction{Do: LeadPause, Reason: strings.TrimSpace(r.PostForm.Get("reason"))}
	case "resume":
	default:
		s.leadsLog("sending", "refused")
		s.renderLeads(w, r, http.StatusUnprocessableEntity, "Choose pause or resume.", true)
		return
	}
	s.leadsAct(w, r, a, leadsOutPath, s.backToLeads)
}

// steerLead is POST /leads/next/{lead}/{action}: pin, defer (until a date), skip or clear.
func (s *server) steerLead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "lead")
	do := r.PathValue("action")
	if !ok || !slices.Contains([]string{LeadPin, LeadDefer, LeadSkip, LeadClear}, do) {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	s.leadsAct(w, r, LeadAction{Do: do, ID: id, Until: strings.TrimSpace(r.PostForm.Get("until"))}, leadsNextPath, s.backToNext)
}

// savePriorities is POST /leads/priorities: do=preview shows the queue under the edited
// profile and saves nothing; do=save saves it as a new version; do=reset goes back to the
// file's defaults.
func (s *server) savePriorities(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	form := priorityForm(r.PostForm)
	switch r.PostForm.Get("do") {
	case "preview":
		s.leadsLog("preview-priorities", "ok")
		s.renderPriorities(w, r, http.StatusOK, &form, "", false)
	case "save":
		s.leadsAct(w, r, LeadAction{Do: LeadSavePriority, Form: form}, leadsPrioPath,
			func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
				s.renderPriorities(w, r, code, &form, msg, isErr)
			})
	case "reset":
		s.leadsAct(w, r, LeadAction{Do: LeadResetPrio}, leadsPrioPath,
			func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
				s.renderPriorities(w, r, code, nil, msg, isErr)
			})
	default:
		s.leadsLog("priorities", "refused")
		s.renderPriorities(w, r, http.StatusUnprocessableEntity, &form, "Choose preview, save or reset.", true)
	}
}

// priorityForm reads the priorities form: every points input by its key, and the text
// fields.
func priorityForm(f url.Values) PriorityForm {
	var out PriorityForm
	keys := make([]string, 0, len(f))
	for k := range f {
		if strings.HasPrefix(k, "kind.") || strings.HasPrefix(k, "major.") || strings.HasPrefix(k, "channel.") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		out.Points = append(out.Points, PointField{Key: k, Value: strings.TrimSpace(f.Get(k))})
	}
	out.ExactVersion = strings.TrimSpace(f.Get("exact_version"))
	out.Countries = strings.TrimSpace(f.Get("countries"))
	out.RankBands = strings.TrimSpace(f.Get("rank_bands"))
	out.Exclude = strings.TrimSpace(f.Get("exclude_countries"))
	return out
}

// approveWords is the approve button's text for a batch.
func approveWords(shadow bool) string {
	if shadow {
		return "Approve: a shadow batch, so nothing is sent."
	}
	return approveLiveFmt
}
