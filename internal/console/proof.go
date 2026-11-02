package console

import "net/http"

// The proof library's page: Task 4.5 fills in the writes and the template.

// leadsProof is GET /leads/proof.
func (s *server) leadsProof(w http.ResponseWriter, r *http.Request) {
	p, err := s.b.Proofs(r.Context())
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	s.render(w, r, http.StatusOK, "leads_proof", page{Title: "Proof", Active: "/leads/proof", SignedIn: true, Message: s.takeFlash(w, r), Data: p})
}

// addProof is POST /leads/proof.
func (s *server) addProof(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "This is not on this server yet.", http.StatusNotImplemented)
}

// retireProof is POST /leads/proof/{pid}/retire.
func (s *server) retireProof(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "This is not on this server yet.", http.StatusNotImplemented)
}
