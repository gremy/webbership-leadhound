package console

import (
	"net/http"
	"strings"
)

// leadsTodayPath is the Today tab.
const leadsTodayPath = "/leads/today"

// todayView is what the Today tab renders: the backend's view, the line from the last
// action, and how many emails due today are not listed.
type todayView struct {
	T           TodayView
	Message     string
	Error       bool
	MoreEmails  int // EmailsDue past the ones listed
	MoreWaiting int // CallsWaitingTotal past the ones listed
}

// leadsToday is GET /leads/today?campaign=S: the day's work, decision-first, for one
// campaign or every campaign. It never writes.
func (s *server) leadsToday(w http.ResponseWriter, r *http.Request) {
	campaign := strings.TrimSpace(r.URL.Query().Get("campaign"))
	t, err := s.b.LeadsToday(r.Context(), campaign)
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	v := todayView{T: t, MoreEmails: max(t.EmailsDue-len(t.Emails), 0), MoreWaiting: max(t.CallsWaitingTotal-len(t.CallsWaiting), 0)}
	v.Message = s.takeFlash(w, r)
	s.render(w, r, http.StatusOK, "leads_today", page{Title: "Today", Active: leadsPath, SignedIn: true, Data: v})
}
