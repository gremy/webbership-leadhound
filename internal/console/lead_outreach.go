package console

import "net/http"

// The lead page's outreach writes and the Draft it fragment. Task 4.2 fills the writes in: each
// parses its form, refuses with a 422 that keeps what was typed (LeadDetailPage.Posted), and
// answers 303 to its card's anchor (#contacts, #draft-<mid>, #proposals). The two-segment ones
// are reached through leadAct, because POST /leads/{id}/<word> would conflict with
// POST /leads/settings/{section}.

// outreachNotBuilt answers a write that has no handler yet.
func outreachNotBuilt(w http.ResponseWriter) {
	http.Error(w, "This is not on this server yet.", http.StatusNotImplemented)
}

// leadContactAdd is POST /leads/{id}/contacts, through leadAct.
func (s *server) leadContactAdd(w http.ResponseWriter, _ *http.Request, _ int64) {
	outreachNotBuilt(w)
}

// leadDraftIt is POST /leads/{id}/draft-it, through leadAct.
func (s *server) leadDraftIt(w http.ResponseWriter, _ *http.Request, _ int64) {
	outreachNotBuilt(w)
}

// leadDraftDiscard is POST /leads/{id}/draft-discard, through leadAct.
func (s *server) leadDraftDiscard(w http.ResponseWriter, _ *http.Request, _ int64) {
	outreachNotBuilt(w)
}

// leadSector is POST /leads/{id}/sector, through leadAct.
func (s *server) leadSector(w http.ResponseWriter, _ *http.Request, _ int64) {
	outreachNotBuilt(w)
}

// leadProposalAdd is POST /leads/{id}/proposals, through leadAct.
func (s *server) leadProposalAdd(w http.ResponseWriter, _ *http.Request, _ int64) {
	outreachNotBuilt(w)
}

// leadContactEdit is POST /leads/{id}/contacts/{cid}/edit; {cid} is a ContactRef.
func (s *server) leadContactEdit(w http.ResponseWriter, _ *http.Request) { outreachNotBuilt(w) }

// leadContactRetire is POST /leads/{id}/contacts/{cid}/retire; {cid} is a ContactRef.
func (s *server) leadContactRetire(w http.ResponseWriter, _ *http.Request) { outreachNotBuilt(w) }

// leadDraftSave is POST /leads/{id}/drafts/{mid}/save.
func (s *server) leadDraftSave(w http.ResponseWriter, _ *http.Request) { outreachNotBuilt(w) }

// leadDraftSent is POST /leads/{id}/drafts/{mid}/sent; it posts the hash the card showed.
func (s *server) leadDraftSent(w http.ResponseWriter, _ *http.Request) { outreachNotBuilt(w) }

// leadProposalStatus is POST /leads/{id}/proposals/{pid}/status.
func (s *server) leadProposalStatus(w http.ResponseWriter, _ *http.Request) { outreachNotBuilt(w) }

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
