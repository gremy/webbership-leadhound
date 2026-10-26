package outreach

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// PlanItem is one approved email message a send run may pick.
type PlanItem struct {
	MessageID int64
	BatchID   int64
	LeadID    int64
	Step      int            // 0 is the first email, 1 and 2 the follow-ups
	Domain    string         // the recipient's domain
	Loc       *time.Location // the recipient country's zone; it sets business hours only
	DueAt     time.Time      // the zero time is never due
	Selftest  bool           // to the operator's own address: bypasses the cap, counts toward it
	MailboxID int64          // the sending mailbox, set at batch creation; PlanMailboxes reads it
}

// HoldReason says why Plan left a due message for a later run.
type HoldReason string

// Reasons Plan holds a message, checked in this order; a message is counted once, under the
// first that applies.
const (
	HoldNotDue HoldReason = "not_due" // due_at is in the future or unset
	HoldHours  HoldReason = "hours"   // the recipient's local time is outside business hours (RA8)
	HoldDomain HoldReason = "domain"  // a message to the same domain goes in this run
	HoldCap    HoldReason = "cap"     // today's cap is used up
)

// Plan picks the messages this run sends, in send order, and counts the rest by reason.
//
// The order is: selftest messages first, then follow-ups by due_at (oldest first), then
// first emails by batch, then lead; message id breaks every tie. Follow-ups go before first
// emails because their timing was promised N business days after the previous email and they
// continue a sequence already open, while a first email loses nothing by waiting a day; so a
// tight cap finishes open sequences before it starts new ones. Selftests go first so they are
// never displaced and their count is taken before anything else.
//
// At most cap−sentToday non-selftest messages are picked; a selftest is picked whatever the
// cap and still uses up one of its places (RA24). No two picked messages share a recipient
// registrable domain, so mail.x.example and x.example count as one (a domain that is not
// registrable is compared whole, without case). A message outside business hours in its
// recipient's zone at now is held whatever its due_at (RA8); selftests keep that rule too.
// Held messages claim no domain. The input slice is not changed.
func Plan(due []PlanItem, sentToday, capToday int, now time.Time, bh leadconfig.BusinessHours) (send []PlanItem, held map[HoldReason]int) {
	count := sentToday
	return planWalk(due, now, bh,
		func(PlanItem) bool { return count < capToday },
		func(PlanItem) { count++ })
}

// planWalk is Plan's walk: it orders due as Plan says and applies the not-due, hours and
// one-per-domain rules, then asks room whether a non-selftest message still fits; take is
// called for every message picked (selftests included), and only for those, so a held
// message never uses up a place. The input slice is not changed.
func planWalk(due []PlanItem, now time.Time, bh leadconfig.BusinessHours, room func(PlanItem) bool, take func(PlanItem)) (send []PlanItem, held map[HoldReason]int) {
	order := slices.Clone(due)
	slices.SortStableFunc(order, planCompare)
	held = map[HoldReason]int{}
	send = []PlanItem{}
	domains := map[string]bool{}
	for _, it := range order {
		domain := planDomain(it.Domain)
		loc := it.Loc
		if loc == nil {
			loc = time.UTC
		}
		switch {
		case it.DueAt.IsZero() || it.DueAt.After(now):
			held[HoldNotDue]++
		case !InBusinessHours(loc, bh, now):
			held[HoldHours]++
		case domains[domain]:
			held[HoldDomain]++
		case !it.Selftest && !room(it):
			held[HoldCap]++
		default:
			send = append(send, it)
			domains[domain] = true
			take(it)
		}
	}
	return send, held
}

// planDomain is the key the one-per-domain rule compares: the registrable domain, or the
// lower-cased domain when it has none.
func planDomain(d string) string {
	if r, err := sitelist.Registrable(d); err == nil {
		return r
	}
	return strings.ToLower(strings.TrimSpace(d))
}

// planRank places selftests first, follow-ups second and first emails last.
func planRank(it PlanItem) int {
	switch {
	case it.Selftest:
		return 0
	case it.Step > 0:
		return 1
	}
	return 2
}

func planCompare(a, b PlanItem) int {
	if c := cmp.Compare(planRank(a), planRank(b)); c != 0 {
		return c
	}
	if planRank(a) == 1 {
		if c := a.DueAt.Compare(b.DueAt); c != 0 {
			return c
		}
	}
	return cmp.Or(
		cmp.Compare(a.BatchID, b.BatchID),
		cmp.Compare(a.LeadID, b.LeadID),
		cmp.Compare(a.MessageID, b.MessageID),
	)
}
