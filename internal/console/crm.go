package console

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/campaign"
)

// The CRM pages' paths.
const (
	leadsRepliesPath  = "/leads/replies"
	leadsPipelinePath = "/leads/pipeline"
	leadsCallsPath    = "/leads/calls"
	leadsReportsPath  = "/leads/reports"
)

// reportDays is how many days back the campaign report reads.
const reportDays = 30

// The CRM writes' refusals, in plain words. Each says nothing was saved.
const (
	crmOff          = "The CRM is not on this server yet; it arrives with the next update."
	badStage        = "Choose Talking, Proposal, Won or Lost. Nothing was saved."
	needLostReason  = "Say why it was lost, in a few words. Nothing was saved."
	lostTooLong     = "The reason is longer than 200 characters; shorten it. Nothing was saved."
	badTenantURL    = "The tenant link must be a full https:// address, like https://cafe.example.com. Nothing was saved."
	crmNoteTooLong  = "The note is longer than 2,000 characters; shorten it. Nothing was saved."
	nextTooLong     = "The next action is longer than 200 characters; shorten it. Nothing was saved."
	needNextAction  = "Say what the next action is, or clear its date. Nothing was saved."
	badDay          = "The date is not a day like 2026-11-03. Nothing was saved."
	badDealValue    = "The deal value is not an amount like 2400 or 2,400.50. Nothing was saved."
	dealTooBig      = "The deal value is over 100 million; check it. Nothing was saved."
	badCurrency     = "Choose GBP, EUR, RON or USD. Nothing was saved."
	badSnooze       = "Choose Snooze 1 day, Snooze 1 week or Clear snooze. Nothing was saved."
	callBackPast    = "The call-back date is before today; give today or a later day. Nothing was saved."
	callBackFar     = "The call-back date is more than a year away; pick a nearer day. Nothing was saved."
	callBackNoCRM   = "Call-back dates are not on this server yet; leave the date empty. Nothing was saved."
	callBackNotHere = "A call-back date does not fit that outcome; leave the date empty. Nothing was saved."
)

// stageWords are the stages in the buttons' own words.
var stageWords = map[string]string{
	StageTalking:  "Talking",
	StageProposal: "Proposal",
	StageWon:      "Won",
	StageLost:     "Lost",
}

// StageWords is a stage in the buttons' words; "" is "No stage".
func StageWords(stage string) string {
	if w, ok := stageWords[stage]; ok {
		return w
	}
	if stage == "" {
		return "No stage"
	}
	return strings.ReplaceAll(stage, "_", " ")
}

// currencySymbols are the currencies with a sign of their own; RON is written as a prefix.
var currencySymbols = map[string]string{"GBP": "£", "EUR": "€", "USD": "$"}

// MoneyWords words an amount in its currency: "£2,400", "€950.50", "RON 1,200". Whole
// amounts show no pence.
func MoneyWords(m Money) string {
	neg := m.Cents < 0
	c := m.Cents
	if neg {
		c = -c
	}
	s := thousands(int(c / 100))
	if frac := c % 100; frac != 0 {
		s += "." + strconv.FormatInt(frac/10, 10) + strconv.FormatInt(frac%10, 10)
	}
	if sym, ok := currencySymbols[m.Currency]; ok {
		s = sym + s
	} else {
		s = cmp.Or(m.Currency, "GBP") + " " + s
	}
	if neg {
		s = "-" + s
	}
	return s
}

// moneyOf words cents (an int64, or a *int64 where nil is "") in currency.
func moneyOf(c any, currency string) string {
	switch c := c.(type) {
	case int64:
		return MoneyWords(Money{Currency: currency, Cents: c})
	case *int64:
		if c != nil {
			return MoneyWords(Money{Currency: currency, Cents: *c})
		}
	}
	return ""
}

// moneyLine words several amounts, one per currency, "£2,400 · €950"; "" for none.
func moneyLine(ms []Money) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, MoneyWords(m))
	}
	return strings.Join(parts, " · ")
}

// sumMoney adds amount in currency into sums, keeping one entry per currency in the order of
// Currencies (an unknown currency after them, in the order met).
func sumMoney(sums []Money, currency string, cents int64) []Money {
	currency = cmp.Or(currency, Currencies[0])
	for i := range sums {
		if sums[i].Currency == currency {
			sums[i].Cents += cents
			return sums
		}
	}
	sums = append(sums, Money{Currency: currency, Cents: cents})
	rank := func(c string) int {
		if i := slices.Index(Currencies, c); i >= 0 {
			return i
		}
		return len(Currencies)
	}
	slices.SortStableFunc(sums, func(a, b Money) int { return rank(a.Currency) - rank(b.Currency) })
	return sums
}

// crmBackend is the backend's CRM part, when it has one.
func (s *server) crmBackend() (CRMBackend, bool) {
	c, ok := s.b.(CRMBackend)
	return c, ok
}

// today is the console's day now, at midnight in its zone.
func (s *server) today() time.Time {
	n := s.now().In(s.opt.Location)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, s.opt.Location)
}

// crmFlash is a flash read on a CRM page: the message, and whether it is a refusal.
func (s *server) crmFlash(w http.ResponseWriter, r *http.Request) (string, bool) {
	msg, isErr := strings.CutPrefix(s.takeFlash(w, r), errFlash)
	return msg, isErr
}

// repliesView is the replies inbox.
type repliesView struct {
	Off     bool
	Rows    []ReplyRow
	Message string
	Error   bool
}

// leadsReplies is GET /leads/replies: the leads that replied and wait on the operator, newest
// first, each with stage buttons and a snooze. It never writes.
func (s *server) leadsReplies(w http.ResponseWriter, r *http.Request) {
	v := repliesView{}
	v.Message, v.Error = s.crmFlash(w, r)
	if crm, ok := s.crmBackend(); ok {
		rows, err := crm.RepliesInbox(r.Context())
		if s.leadsRead(w, r, http.StatusOK, err) {
			return
		}
		v.Rows = rows
	} else {
		v.Off = true
	}
	s.render(w, r, http.StatusOK, "leads_replies", page{Title: "Replies", Active: leadsPath, SignedIn: true, Data: v})
}

// pipelineColumn is one stage's column: its cards, and their values summed per currency.
type pipelineColumn struct {
	Stage  string
	Cards  []PipelineCard
	Totals []Money
}

// pipelineView is the pipeline: a column per stage, and the open deals' figures above them.
type pipelineView struct {
	Off      bool
	Columns  []pipelineColumn
	Open     int     // cards talking or at proposal
	OpenSum  []Money // their values, per currency
	Expected []Money // their expected values (the backend's weights), per currency
	WonSum   []Money // the won cards' values, per currency
	Empty    bool    // no card at all
	Message  string
	Error    bool
}

// newPipelineView groups cards into the four columns, in Stages order, keeping the backend's
// order within each; a card of a stage the console does not know is left out.
func newPipelineView(cards []PipelineCard) pipelineView {
	v := pipelineView{Empty: true}
	for _, st := range Stages {
		col := pipelineColumn{Stage: st}
		for _, c := range cards {
			if c.Stage != st {
				continue
			}
			v.Empty = false
			col.Cards = append(col.Cards, c)
			if c.DealValueCents != nil {
				col.Totals = sumMoney(col.Totals, c.Currency, *c.DealValueCents)
			}
			switch st {
			case StageTalking, StageProposal:
				v.Open++
				if c.DealValueCents != nil {
					v.OpenSum = sumMoney(v.OpenSum, c.Currency, *c.DealValueCents)
					v.Expected = sumMoney(v.Expected, c.Currency, c.ExpectedCents)
				}
			case StageWon:
				if c.DealValueCents != nil {
					v.WonSum = sumMoney(v.WonSum, c.Currency, *c.DealValueCents)
				}
			}
		}
		v.Columns = append(v.Columns, col)
	}
	return v
}

// leadsPipeline is GET /leads/pipeline: the leads past their first reply, a column per stage.
// It never writes.
func (s *server) leadsPipeline(w http.ResponseWriter, r *http.Request) {
	v := pipelineView{Off: true}
	if crm, ok := s.crmBackend(); ok {
		cards, err := crm.Pipeline(r.Context())
		if s.leadsRead(w, r, http.StatusOK, err) {
			return
		}
		v = newPipelineView(cards)
	}
	v.Message, v.Error = s.crmFlash(w, r)
	s.render(w, r, http.StatusOK, "leads_pipeline", page{Title: "Pipeline", Active: leadsPath, SignedIn: true, Data: v})
}

// callsView is the call queue: today's calls and call-backs, with the outcome buttons.
type callsView struct {
	Off      bool
	Rows     []CallQueueRow
	Outcomes []string
	Today    string // YYYY-MM-DD, the call-back field's earliest day
	Message  string
	Error    bool
}

// leadsCalls is GET /leads/calls: the calls to make today and the call-backs due, each with
// the outcome buttons and a call-back date. It never writes.
func (s *server) leadsCalls(w http.ResponseWriter, r *http.Request) {
	v := callsView{Off: true, Outcomes: campaign.Outcomes, Today: s.today().Format(time.DateOnly)}
	if crm, ok := s.crmBackend(); ok {
		rows, err := crm.CallQueue(r.Context(), s.today())
		if s.leadsRead(w, r, http.StatusOK, err) {
			return
		}
		v.Off, v.Rows = false, rows
	}
	v.Message, v.Error = s.crmFlash(w, r)
	s.render(w, r, http.StatusOK, "leads_calls", page{Title: "Calls", Active: leadsPath, SignedIn: true, Data: v})
}

// reportView is the campaign report.
type reportView struct {
	Off   bool
	Rows  []CampaignReportRow
	Since time.Time
	Days  int
}

// leadsReports is GET /leads/reports: each campaign's funnel, revenue and Claude cost over
// the last 30 days. It never writes.
func (s *server) leadsReports(w http.ResponseWriter, r *http.Request) {
	since := s.today().AddDate(0, 0, -reportDays)
	v := reportView{Off: true, Since: since, Days: reportDays}
	if crm, ok := s.crmBackend(); ok {
		rows, err := crm.CampaignReport(r.Context(), since)
		if s.leadsRead(w, r, http.StatusOK, err) {
			return
		}
		v.Off, v.Rows = false, rows
	}
	s.render(w, r, http.StatusOK, "leads_reports", page{Title: "Reports", Active: leadsPath, SignedIn: true,
		Message: s.takeFlash(w, r), Data: v})
}

// replyRate words replied out of sent as a whole percentage; "—" with nothing sent.
func replyRate(replied, sent int) string {
	if sent <= 0 {
		return "—"
	}
	return pct(float64(replied) * 100 / float64(sent))
}

// costPer words a cost per lead or reply; "—" when there was nothing to divide by.
func costPer(micros int64, n int) string {
	if n <= 0 {
		return "—"
	}
	return USD(micros)
}

// crmForm is the lead page's CRM forms as they show: the stored values, or after a refusal
// the values posted to the refused section, with the refusal and the field at fault.
type crmForm struct {
	Section, Field, Refusal        string
	Stage, LostReason, TenantURL   string
	Note, NextAction, NextActionOn string
	Value, Currency                string
}

// crmPanel is what the lead page's CRM panels render.
type crmPanel struct {
	LeadID     int64
	C          LeadCRM
	Timeline   []TimelineItem
	F          crmForm
	Back       string // the lead page: where the panels' forms come back to
	Today      string // YYYY-MM-DD
	Snoozed    bool   // SnoozedUntil is after now
	Stages     []string
	Currencies []string
}

// crmFormOf is the forms filled from the stored values.
func crmFormOf(c LeadCRM, loc *time.Location) crmForm {
	f := crmForm{Stage: c.Stage, LostReason: c.LostReason, TenantURL: c.TenantURL, Note: c.Note, NextAction: c.NextAction,
		Currency: cmp.Or(c.Currency, Currencies[0])}
	if c.NextActionOn != nil {
		f.NextActionOn = c.NextActionOn.In(loc).Format(time.DateOnly)
	}
	if c.DealValueCents != nil {
		f.Value = centsInput(*c.DealValueCents)
	}
	return f
}

// centsInput is cents as the deal field takes it back: "2400" or "2400.50".
func centsInput(c int64) string {
	s := strconv.FormatInt(c/100, 10)
	if frac := c % 100; frac != 0 {
		s += "." + strconv.FormatInt(frac/10, 10) + strconv.FormatInt(frac%10, 10)
	}
	return s
}

// postedForm is the section's fields as posted.
func postedForm(section string, f url.Values) crmForm {
	return crmForm{Section: section,
		Stage: strings.TrimSpace(f.Get("stage")), LostReason: strings.TrimSpace(f.Get("lost_reason")), TenantURL: strings.TrimSpace(f.Get("tenant_url")),
		Note: strings.TrimSpace(f.Get("note")), NextAction: strings.TrimSpace(f.Get("next_action")), NextActionOn: strings.TrimSpace(f.Get("next_action_on")),
		Value: strings.TrimSpace(f.Get("value")), Currency: strings.TrimSpace(f.Get("currency"))}
}

// overlay is stored with the posted section's fields put over it, and the refusal.
func (stored crmForm) overlay(p *crmForm) crmForm {
	if p == nil {
		return stored
	}
	out := stored
	out.Section, out.Field, out.Refusal = p.Section, p.Field, p.Refusal
	switch p.Section {
	case "stage":
		out.Stage, out.LostReason, out.TenantURL = p.Stage, p.LostReason, p.TenantURL
	case "note":
		out.Note, out.NextAction, out.NextActionOn = p.Note, p.NextAction, p.NextActionOn
	case "deal":
		out.Value, out.Currency = p.Value, p.Currency
	}
	return out
}

// crmPanelFor reads lead id's CRM panels; nil (and no error) when the backend has no CRM.
func (s *server) crmPanelFor(r *http.Request, id int64, posted *crmForm) (*crmPanel, error) {
	crm, ok := s.crmBackend()
	if !ok {
		return nil, nil
	}
	c, err := crm.LeadCRM(r.Context(), id)
	if err != nil {
		return nil, err
	}
	tl, err := crm.LeadTimeline(r.Context(), id)
	if err != nil {
		return nil, err
	}
	return &crmPanel{LeadID: id, C: c, Timeline: tl, F: crmFormOf(c, s.opt.Location).overlay(posted),
		Back: leadsPath + "/" + strconv.FormatInt(id, 10), Today: s.today().Format(time.DateOnly),
		Snoozed: c.SnoozedUntil != nil && c.SnoozedUntil.After(s.now()), Stages: Stages, Currencies: Currencies}, nil
}

// parseDay reads an optional YYYY-MM-DD field as midnight in the console's zone; "" is nil.
func (s *server) parseDay(v string) (*time.Time, bool) {
	if v == "" {
		return nil, true
	}
	d, err := time.ParseInLocation(time.DateOnly, v, s.opt.Location)
	if err != nil {
		return nil, false
	}
	return &d, true
}

// parseDeal reads a deal value: digits with optional thousands commas and at most two
// decimals, "2400", "2,400" or "2400.50"; "" is nil. It refuses with a sentence.
func parseDeal(v string) (*int64, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, ""
	}
	whole, frac, dot := strings.Cut(v, ".")
	whole = strings.ReplaceAll(whole, ",", "")
	digits := func(s string) bool {
		for _, c := range s {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	if whole == "" || !digits(whole) || !digits(frac) || dot && frac == "" || len(frac) > 2 {
		return nil, badDealValue
	}
	if len(whole) > 9 {
		return nil, dealTooBig
	}
	w, _ := strconv.ParseInt(whole, 10, 64)
	frac += strings.Repeat("0", 2-len(frac))
	f, _ := strconv.ParseInt(frac, 10, 64)
	c := w*100 + f
	if c > MaxDealCents {
		return nil, dealTooBig
	}
	return &c, ""
}

// tenantURL reports whether v is an https address with a host and nothing a link should not
// carry, at most MaxTenantURL characters.
func tenantURL(v string) bool {
	if len(v) > MaxTenantURL {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && !strings.ContainsAny(v, " \t\r\n\\")
}

// crmRefusal is a CRM write the console refuses before the backend: the field at fault and
// the sentence.
type crmRefusal struct{ field, sentence string }

// checkCRM checks a posted section and builds its change; a refusal leaves the change unset.
func (s *server) checkCRM(id int64, f crmForm) (any, *crmRefusal) {
	switch f.Section {
	case "stage":
		if f.Stage != "" && !slices.Contains(Stages, f.Stage) {
			return nil, &crmRefusal{"stage", badStage}
		}
		c := StageChange{LeadID: id, Stage: f.Stage}
		if f.Stage == StageLost {
			switch {
			case f.LostReason == "":
				return nil, &crmRefusal{"lost_reason", needLostReason}
			case utf8.RuneCountInString(f.LostReason) > MaxLostReason:
				return nil, &crmRefusal{"lost_reason", lostTooLong}
			}
			c.LostReason = f.LostReason
		}
		if f.Stage == StageWon && f.TenantURL != "" {
			if !tenantURL(f.TenantURL) {
				return nil, &crmRefusal{"tenant_url", badTenantURL}
			}
			c.TenantURL = f.TenantURL
		}
		return c, nil
	case "note":
		switch {
		case utf8.RuneCountInString(f.Note) > MaxNote:
			return nil, &crmRefusal{"note", crmNoteTooLong}
		case utf8.RuneCountInString(f.NextAction) > MaxNextAction:
			return nil, &crmRefusal{"next_action", nextTooLong}
		}
		on, ok := s.parseDay(f.NextActionOn)
		if !ok {
			return nil, &crmRefusal{"next_action_on", badDay}
		}
		if on != nil && f.NextAction == "" {
			return nil, &crmRefusal{"next_action", needNextAction}
		}
		return NoteChange{LeadID: id, Note: f.Note, NextAction: f.NextAction, NextActionOn: on}, nil
	case "deal":
		v, refusal := parseDeal(f.Value)
		if refusal != "" {
			return nil, &crmRefusal{"value", refusal}
		}
		cur := cmp.Or(f.Currency, Currencies[0])
		if !slices.Contains(Currencies, cur) {
			return nil, &crmRefusal{"currency", badCurrency}
		}
		return DealChange{LeadID: id, ValueCents: v, Currency: cur}, nil
	}
	return nil, nil
}

// crmBack is where a CRM write goes back to: the form's back when it is a Leads page, the
// lead's own page when it names none.
func crmBack(posted string, id int64) string {
	if posted == "" {
		return leadsPath + "/" + strconv.FormatInt(id, 10)
	}
	return leadsBack(posted)
}

// onLeadPage reports whether back is lead id's own page (any fragment).
func onLeadPage(back string, id int64) bool {
	p, _, _ := strings.Cut(back, "#")
	return p == leadsPath+"/"+strconv.FormatInt(id, 10)
}

// leadCRM is POST /leads/{id}/crm (routed through leadAct): section is stage, note or deal,
// and that section's fields. A section the forms never post is refused (400) before the
// backend; a value the console can refuse is refused before it too. A refusal posted from the
// lead page shows that page again (422) with the sentence inside the section's panel and the
// values kept; from another page it goes back (303) with the sentence as an error. Saved, it
// goes back (303) to back with its line.
func (s *server) leadCRM(w http.ResponseWriter, r *http.Request, id int64) {
	const do = "crm"
	if !leadsForm(w, r) {
		return
	}
	crm, ok := s.crmBackend()
	if !ok {
		s.leadsLog(do, "off")
		http.Error(w, crmOff, http.StatusNotFound)
		return
	}
	section := r.PostForm.Get("section")
	if section != "stage" && section != "note" && section != "deal" {
		s.leadsLog(do, "refused")
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	back := crmBack(r.PostForm.Get("back"), id)
	f := postedForm(section, r.PostForm)
	change, refused := s.checkCRM(id, f)
	if refused != nil {
		s.leadsLog(do, "refused")
		f.Field, f.Refusal = refused.field, refused.sentence
		s.crmRefuse(w, r, id, back, f)
		return
	}
	var msg string
	var err error
	switch c := change.(type) {
	case StageChange:
		msg, err = crm.SetLeadStage(r.Context(), c)
		if c.Stage == "" {
			msg = cmp.Or(msg, "Stage cleared: it follows the replies again.")
		} else {
			msg = cmp.Or(msg, "Stage set to "+StageWords(c.Stage)+".")
		}
	case NoteChange:
		msg, err = crm.SetLeadNote(r.Context(), c)
		msg = cmp.Or(msg, "Note and next action saved.")
	case DealChange:
		msg, err = crm.SetLeadDeal(r.Context(), c)
		if c.ValueCents == nil {
			msg = cmp.Or(msg, "Deal value cleared.")
		} else {
			msg = cmp.Or(msg, "Deal value saved: "+MoneyWords(Money{c.Currency, *c.ValueCents})+".")
		}
	}
	if s.crmAnswer(w, r, do, err, func() {
		var cr *CRMRefused
		f.Refusal = refusalWords(err)
		if errors.As(err, &cr) {
			f.Field = cr.Field
		}
		s.crmRefuse(w, r, id, back, f)
	}) {
		return
	}
	s.setFlash(w, msg)
	http.Redirect(w, r, crmAnchor(back, id, section), http.StatusSeeOther) //nolint:gosec // G710: back is a Leads path rebuilt by leadsBack (crmBack), or the lead page
}

// crmAnchor is back pointed at the section's panel when back is the lead's own page without a
// fragment of its own.
func crmAnchor(back string, id int64, section string) string {
	if onLeadPage(back, id) && !strings.Contains(back, "#") {
		return back + "#crm-" + section
	}
	return back
}

// crmAnswer answers a CRM write that failed: not set up, a refusal (refuse), not found, or a
// 500. It reports whether err was one; a nil err is left to the caller.
func (s *server) crmAnswer(w http.ResponseWriter, r *http.Request, do string, err error, refuse func()) bool {
	switch {
	case err == nil:
		s.leadsLog(do, "ok")
		return false
	case errors.Is(err, ErrLeadsNotSetUp):
		s.leadsLog(do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
	case errors.Is(err, ErrUserFixable):
		s.leadsLog(do, "refused")
		refuse()
	case errors.Is(err, ErrNotFound):
		s.leadsLog(do, "not found")
		s.leadsRead(w, r, http.StatusNotFound, err)
	default:
		s.leadsLog(do, "failed")
		s.leadsFail(w, r, err)
	}
	return true
}

// crmRefuse shows a refused CRM write: the lead page again (422) with the form kept when back
// is that page, else back (303) with the sentence as an error.
func (s *server) crmRefuse(w http.ResponseWriter, r *http.Request, id int64, back string, f crmForm) {
	if onLeadPage(back, id) {
		s.renderDetailCRM(w, r, http.StatusUnprocessableEntity, id, "", false, false, &f)
		return
	}
	s.setFlash(w, errFlash+f.Refusal)
	http.Redirect(w, r, back, http.StatusSeeOther) //nolint:gosec // G710: back is a Leads path rebuilt by leadsBack (crmBack), or the lead page
}

// snoozeFor are the snoozes the buttons offer, by their form value; "clear" ends one.
var snoozeFor = map[string]time.Duration{"1d": 24 * time.Hour, "1w": 7 * 24 * time.Hour, "clear": 0}

// leadSnooze is POST /leads/{id}/snooze (routed through leadAct): for is 1d, 1w or clear. A
// value the buttons never post is refused before the backend, shown as leadCRM shows a
// refusal; saved, it goes back (303) with its line.
func (s *server) leadSnooze(w http.ResponseWriter, r *http.Request, id int64) {
	const do = "snooze"
	if !leadsForm(w, r) {
		return
	}
	crm, ok := s.crmBackend()
	if !ok {
		s.leadsLog(do, "off")
		http.Error(w, crmOff, http.StatusNotFound)
		return
	}
	back := crmBack(r.PostForm.Get("back"), id)
	d, ok := snoozeFor[r.PostForm.Get("for")]
	if !ok {
		s.leadsLog(do, "refused")
		s.snoozeRefuse(w, r, id, back, badSnooze)
		return
	}
	var until *time.Time
	msg := "Snooze cleared."
	if d > 0 {
		t := s.now().Add(d)
		until = &t
		msg = "Snoozed until " + t.In(s.opt.Location).Format("Mon 2 Jan 15:04") + "."
	}
	out, err := crm.SnoozeLead(r.Context(), id, until)
	if s.crmAnswer(w, r, do, err, func() { s.snoozeRefuse(w, r, id, back, refusalWords(err)) }) {
		return
	}
	s.setFlash(w, cmp.Or(out, msg))
	http.Redirect(w, r, crmAnchor(back, id, "stage"), http.StatusSeeOther) //nolint:gosec // G710: back is a Leads path rebuilt by leadsBack (crmBack), or the lead page
}

// snoozeRefuse shows a refused snooze: on the lead page above its panels (422), elsewhere
// back (303) with the sentence as an error.
func (s *server) snoozeRefuse(w http.ResponseWriter, r *http.Request, id int64, back, sentence string) {
	if onLeadPage(back, id) {
		s.renderDetailCRM(w, r, http.StatusUnprocessableEntity, id, sentence, true, false, nil)
		return
	}
	s.setFlash(w, errFlash+sentence)
	http.Redirect(w, r, back, http.StatusSeeOther) //nolint:gosec // G710: back is a Leads path rebuilt by leadsBack (crmBack), or the lead page
}

// callBackDay reads the outcome form's call-back date: "" is nil; a day before today, more
// than a year ahead, or with an outcome that ends calling (do not call, wrong number) is a
// refusal.
func (s *server) callBackDay(v, outcome string) (*time.Time, string) {
	d, ok := s.parseDay(strings.TrimSpace(v))
	switch {
	case !ok:
		return nil, "The call-back date is not a day like 2026-11-03. Nothing was saved."
	case d == nil:
		return nil, ""
	case outcome == campaign.OutcomeDoNotCall || outcome == campaign.OutcomeWrongNumber:
		return nil, callBackNotHere
	case d.Before(s.today()):
		return nil, callBackPast
	case d.After(s.today().AddDate(1, 0, 0)):
		return nil, callBackFar
	}
	return d, ""
}

// timelineDot is a timeline item's dot class, by its kind.
func timelineDot(kind string) string {
	switch kind {
	case TimelineReply:
		return "tl-reply"
	case TimelineCall:
		return "tl-call"
	case TimelineStage:
		return "tl-stage"
	case TimelineCheck:
		return "tl-check"
	case TimelineEmail:
		return "tl-email"
	}
	return "tl-event"
}

// stageBtnView is one lead's stage buttons: the lead, its effective stage, where to come
// back to, and its name for a screen reader. Move leaves out the current stage's button (a
// pipeline card, whose column already says its stage).
type stageBtnView struct {
	ID          int64
	Stage, Back string
	Name        string
	Move        bool
}

// hasCRM reports whether the backend has the CRM.
func (s *server) hasCRM() bool {
	_, ok := s.crmBackend()
	return ok
}
