package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MorningCounts is the work waiting at each step of the morning loop. ToReview is the open
// leads not yet reviewed; BatchID is the newest waiting batch, 0 when none waits, and
// BatchItems the leads in all waiting batches; ByHand, Calls and Replies are Today's own
// counts of forms and notes to send by hand, calls due and overdue, and replies waiting.
type MorningCounts struct {
	ToReview                           int
	BatchID                            int64
	BatchItems, ByHand, Calls, Replies int
}

// StepsBackend is the backend's morning counts, when it has them. Like the leads backend,
// MorningCounts returns ErrLeadsNotSetUp when this server has no usable leads settings.
type StepsBackend interface {
	MorningCounts(ctx context.Context) (MorningCounts, error)
}

// step is one step of the morning bar. Next marks the first step with work, and Clear a
// step with nothing waiting; the static form, which has no counts, marks neither.
type step struct {
	Num                  int
	Label, Path          string
	Count                int
	Current, Next, Clear bool
}

// stepsView is the morning bar. Counted is false for the static form the layout renders,
// which carries Fetch, the fragment's own URL; Nothing says no step past Review has work.
type stepsView struct {
	Steps   []step
	Counted bool
	Fetch   string
	Nothing bool
}

// barPage reports whether p is a page that shows the bar.
func barPage(p string) bool {
	switch p {
	case "/leads/home", "/leads", "/leads/outreach", "/leads/today", "/leads/calls", "/leads/replies":
		return true
	}
	return idUnder(p, "/leads/batches/") || idUnder(p, "/leads/calls/")
}

// idUnder reports whether p is prefix followed by one positive whole number, written plainly.
func idUnder(p, prefix string) bool {
	rest, ok := strings.CutPrefix(p, prefix)
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == rest
}

// stepAt is the number of the step the page at p belongs to, or 0 for none. Today holds
// three of the steps, so it marks none of them.
func stepAt(p string) int {
	switch {
	case p == "/leads":
		return 1
	case p == "/leads/outreach", idUnder(p, "/leads/batches/"):
		return 2
	case p == "/leads/calls", idUnder(p, "/leads/calls/"):
		return 3
	case p == "/leads/replies":
		return 4
	}
	return 0
}

// morningSteps is the bar's five steps for the page at path, in Today's decision-first
// order after Review and Approve. With c nil it is the static form: labels and links only.
// Otherwise the first of steps 2 to 5 with work is next, and with none Review is; Review is
// never clear, because there is always a list to read.
func morningSteps(c *MorningCounts, path string) []step {
	approve := "/leads/outreach"
	if c != nil && c.BatchID != 0 {
		approve = "/leads/batches/" + strconv.FormatInt(c.BatchID, 10)
	}
	steps := []step{
		{Num: 1, Label: "Review", Path: "/leads"},
		{Num: 2, Label: "Approve", Path: approve},
		{Num: 3, Label: "Calls", Path: "/leads/today#today-calls-now"},
		{Num: 4, Label: "Replies", Path: "/leads/today#today-replies"},
		{Num: 5, Label: "By hand", Path: "/leads/today#today-manual"},
	}
	at := stepAt(path)
	for i := range steps {
		steps[i].Current = steps[i].Num == at
	}
	if c == nil {
		return steps
	}
	counts := []int{c.ToReview, c.BatchItems, c.Calls, c.Replies, c.ByHand}
	work := []bool{true, c.BatchID != 0, c.Calls > 0, c.Replies > 0, c.ByHand > 0}
	next := 0
	for i := range steps {
		steps[i].Count = counts[i]
		if i == 0 {
			continue
		}
		if work[i] && next == 0 {
			next = i
		}
		steps[i].Clear = !work[i]
	}
	steps[next].Next = true
	return steps
}

// stepsBar is the bar for the page at path, counted when c is set.
func stepsBar(c *MorningCounts, path string) *stepsView {
	steps := morningSteps(c, path)
	v := &stepsView{Steps: steps, Counted: c != nil}
	if c == nil {
		v.Fetch = "/leads/steps?at=" + url.QueryEscape(path)
	} else {
		v.Nothing = steps[0].Next
	}
	return v
}

// leadSteps is GET /leads/steps?at=<path>: the bar with its counts, and nothing else. It
// only reads. A backend without the counts, leads not set up, or a failed read answer 204,
// so the bar the page already shows keeps its plain links. An at that is not a bar page
// marks no step and is never written back.
func (s *server) leadSteps(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.b.(StepsBackend)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	c, err := sb.MorningCounts(r.Context())
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.log.Info("console steps", "route", r.Pattern, "read", "morning counts", "err", "leads are not set up")
		w.WriteHeader(http.StatusNoContent)
		return
	case err != nil:
		s.log.Error("console error", "route", r.Pattern, "read", "morning counts", "err", err.Error())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	at := r.URL.Query().Get("at")
	if !barPage(at) {
		at = ""
	}
	s.write(w, r, http.StatusOK, s.pages["steps"], "steps", stepsBar(&c, at))
}
