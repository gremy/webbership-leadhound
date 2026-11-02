package console

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The proof library's page: the proofs in use, a form to add one, and a retire form on each. The
// checks here say ahead of the store what its table would refuse (00029, the proof_* checks), so
// a refusal comes back as a plain sentence with what was typed still in the form.

// proofPath is the proof library's page.
const proofPath = "/leads/proof"

// proofForm is what the add form holds, as typed. Field names the input a refusal is about
// (title, line, url, sectors), "" when it is about none of them.
type proofForm struct {
	Title, Line, URL string
	Sectors          []string
	Public           bool
	Field            string
}

// Ticked is true when the form holds sector.
func (f proofForm) Ticked(sector string) bool { return slices.Contains(f.Sectors, sector) }

// proofView is what leads_proof renders: the library, the add form, and the retire form a refusal
// left open with its reason (RetireID 0 for none).
type proofView struct {
	ProofPage
	Form         proofForm
	RetireID     int64
	RetireReason string
}

// leadsProof is GET /leads/proof.
func (s *server) leadsProof(w http.ResponseWriter, r *http.Request) {
	s.renderProof(w, r, http.StatusOK, s.takeFlash(w, r), false, proofView{})
}

// renderProof shows the page with v's forms; it fills in the library.
func (s *server) renderProof(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool, v proofView) {
	p, err := s.b.Proofs(r.Context())
	if s.leadsRead(w, r, code, err) {
		return
	}
	v.ProofPage = p
	s.render(w, r, code, "leads_proof", page{Title: "Proof", Active: proofPath, SignedIn: true, Message: msg, Error: isErr, Data: v})
}

// proofRefusal says why the store would refuse f, and which input that is about, given the sectors
// a proof may name; "" when it would not. A link is checked with public off too, because it is
// stored all the same.
func proofRefusal(f proofForm, sectors []FilterOption) (words, field string) {
	title, line, link := strings.TrimSpace(f.Title), strings.TrimSpace(f.Line), strings.TrimSpace(f.URL)
	switch {
	case title == "":
		return "A proof needs a title.", "title"
	case strings.ContainsAny(title, "\r\n"):
		return "The title must be one line.", "title"
	case utf8.RuneCountInString(title) > MaxProofTitle:
		return fmt.Sprintf("The title is over %d characters.", MaxProofTitle), "title"
	case line == "":
		return "A proof needs a line.", "line"
	case strings.ContainsAny(line, "\r\n"):
		return "The line must be one line, with no line breaks.", "line"
	case utf8.RuneCountInString(line) > MaxProofLine:
		return fmt.Sprintf("The line is over %d characters.", MaxProofLine), "line"
	case link != "" && !proofLink(link):
		return fmt.Sprintf("The link must be a full https:// address, up to %d characters.", MaxOutreachURL), "url"
	case len(f.Sectors) == 0:
		return "A proof needs at least one sector.", "sectors"
	}
	for _, name := range f.Sectors {
		if !slices.ContainsFunc(sectors, func(o FilterOption) bool { return o.Value == name }) {
			return "One of those sectors is not on the list.", "sectors"
		}
	}
	return "", ""
}

// proofLink is true for an address the proof table takes: https, with a host and no login, no
// white space, at most MaxOutreachURL characters.
func proofLink(s string) bool {
	u, err := url.Parse(s)
	return err == nil && strings.HasPrefix(s, "https://") && u.Host != "" && u.User == nil &&
		!strings.ContainsFunc(s, unicode.IsSpace) && utf8.RuneCountInString(s) <= MaxOutreachURL
}

// addProof is POST /leads/proof. Done, it goes back to the page with what happened; a refusal shows
// the page again (422) with what was typed. A link typed with public off is stored as typed, and
// the line says it stays out of drafts.
func (s *server) addProof(w http.ResponseWriter, r *http.Request) {
	const do = "proof add"
	if !leadsForm(w, r) {
		return
	}
	f := proofForm{Title: r.PostFormValue("title"), Line: r.PostFormValue("line"), URL: r.PostFormValue("url"),
		Sectors: r.PostForm["sectors"], Public: r.PostFormValue("public") != ""}
	p, err := s.b.Proofs(r.Context())
	if err != nil {
		s.leadsLog(do, "failed")
		s.leadsRead(w, r, http.StatusUnprocessableEntity, err)
		return
	}
	refuse := func(words, field string) {
		f.Field = field
		s.renderProof(w, r, http.StatusUnprocessableEntity, words+" Nothing was saved.", true, proofView{Form: f})
	}
	if words, field := proofRefusal(f, p.Sectors); words != "" {
		s.leadsLog(do, "refused")
		refuse(words, field)
		return
	}
	n := NewProof{Title: strings.TrimSpace(f.Title), Line: strings.TrimSpace(f.Line), URL: strings.TrimSpace(f.URL), Public: f.Public}
	for _, o := range p.Sectors {
		if f.Ticked(o.Value) {
			n.Sectors = append(n.Sectors, o.Value)
		}
	}
	msg, err := s.b.AddProof(r.Context(), n)
	if s.crmAnswer(w, r, do, err, func() { refuse(refusalWords(err), "") }) {
		return
	}
	if msg == "" {
		msg = "Proof added."
	}
	if n.URL != "" && !n.Public {
		msg += " Spoken line only: its link is kept but never goes in a draft."
	}
	s.setFlash(w, msg)
	http.Redirect(w, r, proofPath, http.StatusSeeOther)
}

// retireProof is POST /leads/proof/{pid}/retire, with an optional reason.
func (s *server) retireProof(w http.ResponseWriter, r *http.Request) {
	const do = "proof retire"
	id, err := strconv.ParseInt(r.PathValue("pid"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	reason := r.PostFormValue("reason")
	refuse := func(words string) {
		s.renderProof(w, r, http.StatusUnprocessableEntity, words, true, proofView{RetireID: id, RetireReason: reason})
	}
	trimmed := strings.TrimSpace(reason)
	if utf8.RuneCountInString(trimmed) > MaxProofReason {
		s.leadsLog(do, "refused")
		refuse(fmt.Sprintf("The reason is over %d characters. Nothing was saved.", MaxProofReason))
		return
	}
	msg, err := s.b.RetireProof(r.Context(), id, trimmed)
	if s.crmAnswer(w, r, do, err, func() { refuse(refusalWords(err)) }) {
		return
	}
	if msg == "" {
		msg = "Proof retired. New drafts no longer use it."
	}
	s.setFlash(w, msg)
	http.Redirect(w, r, proofPath, http.StatusSeeOther)
}
