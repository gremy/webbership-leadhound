package console

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The lead page's outreach writes and the Draft it fragment. Each parses its form, refuses with a
// 422 that keeps what was typed (LeadDetailPage.Posted), and answers 303 to its card's anchor
// (#contacts, #draft-<mid>, #drafts, #sector, #proposals). The two-segment ones are reached
// through leadAct, because POST /leads/{id}/<word> would conflict with
// POST /leads/settings/{section}.

// The outreach writes' refusals, in plain words. Each says nothing was saved.
const (
	badContactKind     = "Choose email, phone, contact form, LinkedIn, Instagram or person. Nothing was saved."
	needContactValue   = "Give the address, number or link. Nothing was saved."
	needPersonText     = "Give the person's name or role. Nothing was saved."
	contactTooLong     = "That is longer than 500 characters; shorten it. Nothing was saved."
	personNameTooLong  = "The name is longer than 120 characters; shorten it. Nothing was saved."
	personRoleTooLong  = "The role is longer than 60 characters; shorten it. Nothing was saved."
	contactNoteTooLong = "The note is longer than 200 characters; shorten it. Nothing was saved."
	badContactPerson   = "That person could not be read; reload. Nothing was saved."
	needDraftBody      = "The text is empty; write it or leave it as it was. Nothing was saved."
	draftBodyTooLong   = "The text is longer than 5,000 characters; shorten it. Nothing was saved."
	draftSubjectLong   = "The subject is longer than 200 characters; shorten it. Nothing was saved."
	draftSubjectLines  = "The subject must be one line. Nothing was saved."
	draftChanged       = "This draft changed; reload."
	badDraftChannel    = "Choose email, contact form, LinkedIn or Instagram. Nothing was saved."
	badDiscard         = "That draft could not be read; reload. Nothing was discarded."
	needSector         = "Choose a sector. Nothing was saved."
	needProposalTitle  = "Give the proposal a title. Nothing was saved."
	proposalTitleLong  = "The title is longer than 120 characters; shorten it. Nothing was saved."
	badProposalLink    = "The link must be a full https:// address, like https://cafe.example.com. Nothing was saved."
	badProposalStatus  = "Choose Sent, Accepted, Declined or Withdrawn. Nothing was saved."
	proposalNoteLong   = "The note is longer than 300 characters; shorten it. Nothing was saved."
)

// formText is a posted text as it is stored: trimmed, with a browser's line ends made plain, so a
// text is measured and later copied as the operator sees it.
func formText(v string) string {
	return strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
}

// tooLong reports whether v is over limit characters.
func tooLong(v string, limit int) bool { return utf8.RuneCountInString(v) > limit }

// pathPositive reads a path segment as a positive id, or reports false.
func pathPositive(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil && n > 0
}

// leadPathID reads the {id} and a second id segment; a path that is not both has been answered
// when it reports false.
func leadPathID(w http.ResponseWriter, r *http.Request, name string) (id, second int64, ok bool) {
	id, ok1 := pathID(r)
	second, ok2 := pathPositive(r, name)
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return 0, 0, false
	}
	return id, second, true
}

// leadAnchor is the lead's page pointed at one of its cards.
func leadAnchor(id int64, anchor string) string {
	return leadsPath + "/" + strconv.FormatInt(id, 10) + "#" + anchor
}

// postedValues is what the form posted of names, exactly as typed. A field it did not post is left
// out, so the form shows that field's stored value.
func postedValues(r *http.Request, names ...string) map[string]string {
	vals := make(map[string]string, len(names))
	for _, n := range names {
		if r.PostForm.Has(n) {
			vals[n] = r.PostForm.Get(n)
		}
	}
	return vals
}

// renderDetailPosted is the lead's page again (422) with a refused outreach form kept in its place.
func (s *server) renderDetailPosted(w http.ResponseWriter, r *http.Request, id int64, posted OutreachPosted) {
	const code = http.StatusUnprocessableEntity
	d, err := s.b.LeadDetail(r.Context(), id)
	if s.leadsRead(w, r, code, err) {
		return
	}
	d.Posted = posted
	seq, err := s.b.LeadSequence(r.Context(), id)
	if s.leadsRead(w, r, code, err) {
		return
	}
	crm, err := s.crmPanelFor(r, id, nil)
	if s.leadsRead(w, r, code, err) {
		return
	}
	title := cmp.Or(d.Name, siteDomain(d.Domain), "Lead "+strconv.FormatInt(id, 10))
	s.render(w, r, code, "lead_detail", page{Title: title, Active: leadsPath, SignedIn: true,
		Data: leadDetailView{Data: d, Fetches: signalFetches(d.Signals), Back: leadsPath + "/" + strconv.FormatInt(id, 10),
			Seq: s.sequenceRows(seq), CRM: crm}})
}

// outreachRefuse refuses a write before the backend: the page again, with the sentence on the form.
func (s *server) outreachRefuse(w http.ResponseWriter, r *http.Request, do string, id int64, posted OutreachPosted, sentence string) {
	s.leadsLog(do, "refused")
	posted.Refusal = sentence
	s.renderDetailPosted(w, r, id, posted)
}

// outreachAnswer finishes a write the backend ran: a failure as crmAnswer answers it, with a
// refusal on the form; saved, back (303) to the card with the backend's line, or line when it has
// none.
func (s *server) outreachAnswer(w http.ResponseWriter, r *http.Request, do string, id int64, anchor string, posted OutreachPosted, out, line string, err error) {
	if s.crmAnswer(w, r, do, err, func() {
		posted.Refusal = refusalWords(err)
		s.renderDetailPosted(w, r, id, posted)
	}) {
		return
	}
	s.setFlash(w, cmp.Or(out, line))
	http.Redirect(w, r, leadAnchor(id, anchor), http.StatusSeeOther) //nolint:gosec // G710: a Leads path built from a parsed id and a fixed anchor
}

// contactRefusal is the sentence refusing a contact's text, "" when it passes: a person needs a
// name or a role, anything else a value, and each field holds what the store holds.
func contactRefusal(person bool, value, name, role, note string) string {
	switch {
	case person && name == "" && role == "":
		return needPersonText
	case !person && value == "":
		return needContactValue
	case tooLong(value, MaxContactValue):
		return contactTooLong
	case tooLong(name, MaxPersonName):
		return personNameTooLong
	case tooLong(role, MaxPersonRole):
		return personRoleTooLong
	case tooLong(note, MaxContactNote):
		return contactNoteTooLong
	}
	return ""
}

// leadContactAdd is POST /leads/{id}/contacts, through leadAct: kind, value, name and role for a
// person (the value, when given, is the person's own LinkedIn or email route), note, and person_id tying an email or LinkedIn page to a person already on the org.
func (s *server) leadContactAdd(w http.ResponseWriter, r *http.Request, id int64) {
	const do = "contact add"
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	posted := OutreachPosted{Form: "contact-add", Values: postedValues(r, "kind", "value", "name", "role", "note", "person_id")}
	c := NewContact{LeadID: id, Kind: f.Get("kind"), Value: formText(f.Get("value")), Name: formText(f.Get("name")),
		Role: formText(f.Get("role")), Note: formText(f.Get("note"))}
	if !slices.Contains(ContactKinds, c.Kind) {
		s.outreachRefuse(w, r, do, id, posted, badContactKind)
		return
	}
	person := c.Kind == ContactPerson
	if !person {
		c.Name, c.Role = "", ""
	}
	if refusal := contactRefusal(person, c.Value, c.Name, c.Role, c.Note); refusal != "" {
		s.outreachRefuse(w, r, do, id, posted, refusal)
		return
	}
	if v := f.Get("person_id"); v != "" && (c.Kind == ContactEmail || c.Kind == ContactLinkedIn) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			s.outreachRefuse(w, r, do, id, posted, badContactPerson)
			return
		}
		c.PersonID = n
	}
	out, err := s.b.AddContact(r.Context(), c)
	s.outreachAnswer(w, r, do, id, "contacts", posted, out, "Contact added.", err)
}

// contactPath reads a contact write's {id} and {cid}; a path that is not both has been answered
// when it reports false.
func contactPath(w http.ResponseWriter, r *http.Request) (int64, ContactRef, bool) {
	id, ok := pathID(r)
	ref, refOK := ParseContactRef(r.PathValue("cid"))
	if !ok || !refOK {
		http.NotFound(w, r)
		return 0, ContactRef{}, false
	}
	return id, ref, true
}

// leadContactEdit is POST /leads/{id}/contacts/{cid}/edit; {cid} is a ContactRef. It takes value (for a
// person, a new LinkedIn or email route of theirs), name, role and note.
func (s *server) leadContactEdit(w http.ResponseWriter, r *http.Request) {
	const do = "contact edit"
	id, ref, ok := contactPath(w, r)
	if !ok || !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	posted := OutreachPosted{Form: "contact", ID: ref.String(), Values: postedValues(r, "value", "name", "role", "note")}
	c := ContactChange{LeadID: id, Ref: ref, Value: formText(f.Get("value")), Name: formText(f.Get("name")),
		Role: formText(f.Get("role")), Note: formText(f.Get("note"))}
	person := ref.PersonID > 0
	if refusal := contactRefusal(person, c.Value, c.Name, c.Role, c.Note); refusal != "" {
		s.outreachRefuse(w, r, do, id, posted, refusal)
		return
	}
	out, err := s.b.EditContact(r.Context(), c)
	s.outreachAnswer(w, r, do, id, "contacts", posted, out, "Contact saved.", err)
}

// leadContactRetire is POST /leads/{id}/contacts/{cid}/retire; {cid} is a ContactRef. It takes an
// optional note.
func (s *server) leadContactRetire(w http.ResponseWriter, r *http.Request) {
	const do = "contact retire"
	id, ref, ok := contactPath(w, r)
	if !ok || !leadsForm(w, r) {
		return
	}
	posted := OutreachPosted{Form: "contact-retire", ID: ref.String(), Values: postedValues(r, "note")}
	note := formText(r.PostForm.Get("note"))
	if tooLong(note, MaxContactNote) {
		s.outreachRefuse(w, r, do, id, posted, contactNoteTooLong)
		return
	}
	out, err := s.b.RetireContact(r.Context(), id, ref, note)
	s.outreachAnswer(w, r, do, id, "contacts", posted, out, "Contact retired.", err)
}

// draftCard is a draft's card on the lead's page.
func draftCard(mid int64) string { return "draft-" + strconv.FormatInt(mid, 10) }

// leadDraftSave is POST /leads/{id}/drafts/{mid}/save: subject (an email's) and body. The backend
// decides whether this message may still change and runs the text checks.
func (s *server) leadDraftSave(w http.ResponseWriter, r *http.Request) {
	const do = "draft save"
	id, mid, ok := leadPathID(w, r, "mid")
	if !ok || !leadsForm(w, r) {
		return
	}
	posted := OutreachPosted{Form: "draft", ID: strconv.FormatInt(mid, 10), Values: postedValues(r, "subject", "body")}
	e := DraftEdit{LeadID: id, MessageID: mid, Subject: formText(r.PostForm.Get("subject")), Body: formText(r.PostForm.Get("body"))}
	refusal := ""
	switch {
	case e.Body == "":
		refusal = needDraftBody
	case tooLong(e.Body, MaxDraftBody):
		refusal = draftBodyTooLong
	case tooLong(e.Subject, MaxDraftSubject):
		refusal = draftSubjectLong
	case strings.Contains(e.Subject, "\n"):
		refusal = draftSubjectLines
	}
	if refusal != "" {
		s.outreachRefuse(w, r, do, id, posted, refusal)
		return
	}
	out, err := s.b.SaveDraft(r.Context(), e)
	s.outreachAnswer(w, r, do, id, draftCard(mid), posted, out, "Draft saved.", err)
}

// leadDraftSent is POST /leads/{id}/drafts/{mid}/sent: hash is the start of the batch's hash as the
// card showed it, so a batch that changed since is refused, and a form without one is refused the
// same way.
func (s *server) leadDraftSent(w http.ResponseWriter, r *http.Request) {
	const do = "draft sent"
	id, mid, ok := leadPathID(w, r, "mid")
	if !ok || !leadsForm(w, r) {
		return
	}
	posted := OutreachPosted{Form: "draft", ID: strconv.FormatInt(mid, 10)}
	hash := strings.TrimSpace(r.PostForm.Get("hash"))
	if hash == "" {
		s.outreachRefuse(w, r, do, id, posted, draftChanged)
		return
	}
	out, err := s.b.MarkSent(r.Context(), MarkSentRequest{LeadID: id, MessageID: mid, Hash: hash})
	s.outreachAnswer(w, r, do, id, draftCard(mid), posted, out, "Marked sent.", err)
}

// draftChannels are the channels Draft it writes for.
var draftChannels = []string{ChannelEmail, ChannelForm, ChannelLinkedIn, ChannelInstagram}

// leadDraftIt is POST /leads/{id}/draft-it, through leadAct: channel is email, form, linkedin,
// instagram, or empty for the lead's own.
func (s *server) leadDraftIt(w http.ResponseWriter, r *http.Request, id int64) {
	const do = "draft it"
	if !leadsForm(w, r) {
		return
	}
	posted := OutreachPosted{Form: "draft-it", Values: postedValues(r, "channel")}
	channel := r.PostForm.Get("channel")
	if channel != "" && !slices.Contains(draftChannels, channel) {
		s.outreachRefuse(w, r, do, id, posted, badDraftChannel)
		return
	}
	out, err := s.b.RequestDraft(r.Context(), id, channel)
	s.outreachAnswer(w, r, do, id, "drafts", posted, out, "Drafting. It shows here when it is ready.", err)
}

// leadDraftDiscard is POST /leads/{id}/draft-discard, through leadAct: batch_id is
// LeadDetailPage.DiscardBatchID. A refusal shows on the Draft it form, beside the button.
func (s *server) leadDraftDiscard(w http.ResponseWriter, r *http.Request, id int64) {
	const do = "draft discard"
	if !leadsForm(w, r) {
		return
	}
	posted := OutreachPosted{Form: "draft-it"}
	batch, err := strconv.ParseInt(r.PostForm.Get("batch_id"), 10, 64)
	if err != nil || batch <= 0 {
		s.outreachRefuse(w, r, do, id, posted, badDiscard)
		return
	}
	out, err := s.b.DiscardDraft(r.Context(), id, batch)
	s.outreachAnswer(w, r, do, id, "drafts", posted, out, "Draft discarded.", err)
}

// leadSector is POST /leads/{id}/sector, through leadAct: sector is one of SectorOptions, which the
// backend holds.
func (s *server) leadSector(w http.ResponseWriter, r *http.Request, id int64) {
	const do = "sector"
	if !leadsForm(w, r) {
		return
	}
	posted := OutreachPosted{Form: "sector", Values: postedValues(r, "sector")}
	sector := strings.TrimSpace(r.PostForm.Get("sector"))
	if sector == "" {
		s.outreachRefuse(w, r, do, id, posted, needSector)
		return
	}
	out, err := s.b.SetSector(r.Context(), id, sector)
	s.outreachAnswer(w, r, do, id, "sector", posted, out, "Sector saved.", err)
}

// leadProposalAdd is POST /leads/{id}/proposals, through leadAct: title, url, value, currency,
// status and note.
func (s *server) leadProposalAdd(w http.ResponseWriter, r *http.Request, id int64) {
	const do = "proposal add"
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	posted := OutreachPosted{Form: "proposal-add", Values: postedValues(r, "title", "url", "value", "currency", "status", "note")}
	p := NewProposal{LeadID: id, Title: formText(f.Get("title")), URL: strings.TrimSpace(f.Get("url")),
		Currency: cmp.Or(f.Get("currency"), Currencies[0]), Status: cmp.Or(f.Get("status"), ProposalSent), Note: formText(f.Get("note"))}
	value, refusal := parseDeal(f.Get("value"))
	switch {
	case p.Title == "":
		refusal = needProposalTitle
	case tooLong(p.Title, MaxProposalTitle):
		refusal = proposalTitleLong
	case p.URL != "" && !tenantURL(p.URL):
		refusal = badProposalLink
	case refusal != "": // the value's own sentence, from parseDeal
	case !slices.Contains(Currencies, p.Currency):
		refusal = badCurrency
	case !slices.Contains(ProposalStatuses, p.Status):
		refusal = badProposalStatus
	case tooLong(p.Note, MaxProposalNote):
		refusal = proposalNoteLong
	}
	if refusal != "" {
		s.outreachRefuse(w, r, do, id, posted, refusal)
		return
	}
	p.ValueCents = value
	out, err := s.b.AddProposal(r.Context(), p)
	s.outreachAnswer(w, r, do, id, "proposals", posted, out, "Proposal added.", err)
}

// leadProposalStatus is POST /leads/{id}/proposals/{pid}/status: status and an optional note.
func (s *server) leadProposalStatus(w http.ResponseWriter, r *http.Request) {
	const do = "proposal status"
	id, pid, ok := leadPathID(w, r, "pid")
	if !ok || !leadsForm(w, r) {
		return
	}
	posted := OutreachPosted{Form: "proposal", ID: strconv.FormatInt(pid, 10), Values: postedValues(r, "status", "note")}
	c := ProposalStatusChange{LeadID: id, ProposalID: pid, Status: r.PostForm.Get("status"), Note: formText(r.PostForm.Get("note"))}
	switch {
	case !slices.Contains(ProposalStatuses, c.Status):
		s.outreachRefuse(w, r, do, id, posted, badProposalStatus)
		return
	case tooLong(c.Note, MaxProposalNote):
		s.outreachRefuse(w, r, do, id, posted, proposalNoteLong)
		return
	}
	out, err := s.b.SetProposalStatus(r.Context(), c)
	s.outreachAnswer(w, r, do, id, "proposals", posted, out, "Proposal marked "+strings.ToLower(ProposalStatusWords(c.Status))+".", err)
}

// draftStatusView is what the draft_status fragment renders (outreach_parts.html): the lead, for
// its hx-get, and its latest Draft it request, nil when it never had one. A request still open
// keeps the hx-trigger; one that ended leaves it out, which stops the polling.
type draftStatusView struct {
	LeadID  int64
	Request *LeadDraftRequest
}

// leadDraftStatus is GET /leads/draft-status/{id}: the Draft it line as an htmx fragment, with no
// layout. It writes nothing.
func (s *server) leadDraftStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	req, err := s.b.DraftStatus(r.Context(), id)
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	s.write(w, r, http.StatusOK, s.pages["outreach_parts"], "draft_status", draftStatusView{LeadID: id, Request: req})
}
