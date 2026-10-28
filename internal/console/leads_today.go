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
	MoreEmails  int            // EmailsDue past the ones listed
	MoreWaiting int            // CallsWaitingTotal past the ones listed
	Num         map[string]int // each shown section's number, in display order (todaySections)
}

// todaySections numbers the Today tab's sections in the order they show, decision-first:
// the calls to make now, then beside them the calls due today that cannot be made now
// (shown only when there are some), the overdue calls, replies, emails, forms and LinkedIn
// notes, and last the calls not due yet. Every shown section has a number and the numbers run without a gap.
func todaySections(t TodayView) map[string]int {
	order := []string{"calls-now", "calls-due", "calls-overdue", "replies", "emails", "manual", "calls-waiting"}
	num := make(map[string]int, len(order))
	for _, id := range order {
		if id == "calls-due" && len(t.CallsDue) == 0 {
			continue
		}
		num[id] = len(num) + 1
	}
	return num
}

// leadsToday is GET /leads/today?campaign=S: the day's work, decision-first, for one
// campaign or every campaign. It never writes.
func (s *server) leadsToday(w http.ResponseWriter, r *http.Request) {
	campaign := strings.TrimSpace(r.URL.Query().Get("campaign"))
	t, err := s.b.LeadsToday(r.Context(), campaign)
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	v := todayView{T: t, MoreEmails: max(t.EmailsDue-len(t.Emails), 0), MoreWaiting: max(t.CallsWaitingTotal-len(t.CallsWaiting), 0),
		Num: todaySections(t)}
	v.Message = s.takeFlash(w, r)
	s.render(w, r, http.StatusOK, "leads_today", page{Title: "Today", Active: leadsPath, SignedIn: true, Data: v})
}
