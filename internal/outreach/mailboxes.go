package outreach

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// MailboxLoad is what the sizing, assignment and send planning read about one sending
// mailbox. Every time in it, and every day passed with it, is in the operator's zone
// (LEADHOUND_TZ), never a recipient's.
type MailboxLoad struct {
	ID        int64
	Domain    string     // the sending domain, the part after the @ of its address
	FirstSend *time.Time // the mailbox's first live send; nil when none has gone
	SentToday int        // live sends today that count toward its cap
	Waiting   int        // live emails on it that may still go: approved, or to check by hand
	Ready     bool       // the readiness that applies to the caller (new leads, or sending)
}

// MailboxCap is the mailbox's own cap on day's date: the ramp counted from that mailbox's
// first live send (CapOn), never above leadconfig.HardDailyCeiling.
func MailboxCap(cfg leadconfig.Config, m MailboxLoad, day time.Time) int {
	return CapOn(cfg, m.FirstSend, day)
}

// DomainCap is the most live sends one sending domain may make in a day over all of its
// mailboxes: caps.domain_per_day (leadconfig.DefaultDomainPerDay when unset), never above
// leadconfig.HardDomainDailyCeiling.
func DomainCap(cfg leadconfig.Config) int {
	n := cfg.Caps.DomainPerDay
	if n <= 0 {
		n = leadconfig.DefaultDomainPerDay
	}
	return min(n, leadconfig.HardDomainDailyCeiling)
}

// RoomToday is how many more live sends each mailbox may make on day (the operator's
// today): its MailboxCap less SentToday, then capped so a domain's mailboxes together never
// exceed DomainCap less the domain's sends today; the domain's remainder is handed out in
// ascending mailbox id. A mailbox that is not Ready gets 0, though its sends still count
// toward its domain. Every mailbox given has an entry.
func RoomToday(cfg leadconfig.Config, mbs []MailboxLoad, day time.Time) map[int64]int {
	return mailboxRoomOn(cfg, mbs, day, true, func(m MailboxLoad) *time.Time { return m.FirstSend })
}

// RoomOver is each mailbox's room over the sizing window, as the batch sizing counts it:
// days business days starting at now (the operator's now, in the operator's zone), today
// being one of them only while a send could still go out today (its sending window, in
// some email country's zone, starts before the operator's day ends). Today counts RoomToday;
// every other day counts the caps whole, again capped per domain. Waiting is then taken off,
// never below 0. With caps.start_date first-send, a mailbox with no send yet is at day 0 on
// every day counted, as the batch sizing does.
func RoomOver(cfg leadconfig.Config, mbs []MailboxLoad, now time.Time, days int, bh leadconfig.BusinessHours) map[int64]int {
	first := func(m MailboxLoad) *time.Time {
		if m.FirstSend == nil && cfg.Caps.StartDate.IsFirstSend() {
			return &now
		}
		return m.FirstSend
	}
	out := map[int64]int{}
	for _, m := range mbs {
		out[m.ID] = 0
	}
	counted := 0
	for i := 0; counted < days && i < scheduleSearchDays; i++ {
		day := time.Date(now.Year(), now.Month(), now.Day()+i, 12, 0, 0, 0, now.Location())
		if i == 0 {
			if !mailboxSendableToday(cfg, now, bh) {
				continue
			}
		} else if !scheduleBusinessDay(bh, day) {
			continue
		}
		counted++
		for id, r := range mailboxRoomOn(cfg, mbs, day, i == 0, first) {
			out[id] += r
		}
	}
	for _, m := range mbs {
		out[m.ID] = max(0, out[m.ID]-m.Waiting)
	}
	return out
}

// Assign gives each lead, in the order given, to the mailbox with the most room left (ties:
// the lowest id), taking steps (the first email and its follow-ups) off that room; a lead
// that no mailbox has room for stays unassigned, and a lead given twice is assigned once.
// The room map is not changed.
func Assign(leads []int64, room map[int64]int, steps int) map[int64]int64 {
	steps = max(1, steps)
	left := maps.Clone(room)
	ids := slices.Sorted(maps.Keys(left))
	out := map[int64]int64{}
	for _, lead := range leads {
		if _, done := out[lead]; done {
			continue
		}
		best, found := int64(0), false
		for _, id := range ids {
			if left[id] >= steps && (!found || left[id] > left[best]) {
				best, found = id, true
			}
		}
		if !found {
			continue
		}
		out[lead] = best
		left[best] -= steps
	}
	return out
}

// PlanMailboxes is Plan over many mailboxes: the same order and the same not-due, hours and
// one-per-recipient-domain rules, with each item's mailbox room (room[MailboxID], 0 when
// absent) in place of the one cap. Room is used only by items actually picked, so a held
// item never uses up a mailbox's room; a selftest is picked whatever the room and still
// uses one place. With one mailbox and room = capToday - sentToday it returns exactly what
// Plan returns. The inputs are not changed.
func PlanMailboxes(due []PlanItem, room map[int64]int, now time.Time, bh leadconfig.BusinessHours) (send []PlanItem, held map[HoldReason]int) {
	return planMailboxes(due, room, now, windowOpen(bh, now))
}

// PlanMailboxesAnyHour is PlanMailboxes for a run that skips the sending window (leads send
// --now): every rule but the hours one, so nothing is held for hours. Not-due messages are
// still held (a follow-up waits for its due time), as are a second message to one domain and
// one past its mailbox's room.
func PlanMailboxesAnyHour(due []PlanItem, room map[int64]int, now time.Time) (send []PlanItem, held map[HoldReason]int) {
	return planMailboxes(due, room, now, anyHour)
}

func planMailboxes(due []PlanItem, room map[int64]int, now time.Time, open func(*time.Location) bool) (send []PlanItem, held map[HoldReason]int) {
	left := maps.Clone(room)
	if left == nil {
		left = map[int64]int{}
	}
	return planWalk(due, now, open,
		func(it PlanItem) bool { return left[it.MailboxID] > 0 },
		func(it PlanItem) { left[it.MailboxID]-- })
}

// mailboxRoomOn is the room of each mailbox on one day: its cap (from first's first send),
// less SentToday when today, then the domain's remainder in ascending id.
func mailboxRoomOn(cfg leadconfig.Config, mbs []MailboxLoad, day time.Time, today bool, first func(MailboxLoad) *time.Time) map[int64]int {
	order := slices.Clone(mbs)
	slices.SortStableFunc(order, func(a, b MailboxLoad) int { return cmp.Compare(a.ID, b.ID) })
	left := map[string]int{}
	for _, m := range order {
		d := mailboxDomain(m.Domain)
		if _, ok := left[d]; !ok {
			left[d] = DomainCap(cfg)
		}
		if today {
			left[d] -= m.SentToday
		}
	}
	out := map[int64]int{}
	for _, m := range order {
		out[m.ID] = 0
		if !m.Ready {
			continue
		}
		own := CapOn(cfg, first(m), day)
		if today {
			own -= m.SentToday
		}
		d := mailboxDomain(m.Domain)
		r := max(0, min(own, left[d]))
		left[d] -= r
		out[m.ID] = r
	}
	return out
}

// mailboxDomain is the key two mailboxes share a domain by: the domain without case.
func mailboxDomain(d string) string {
	return strings.ToLower(strings.TrimSpace(d))
}

// mailboxSendableToday reports whether a send could still go out on the operator's calendar
// day of now: whether the next business slot of any email country's zone starts before the
// operator's day ends; with no email country it is the operator's own zone. It mirrors the
// batch sizing's todayOpen.
func mailboxSendableToday(cfg leadconfig.Config, now time.Time, bh leadconfig.BusinessHours) bool {
	end := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	var locs []*time.Location
	for _, c := range cfg.Countries {
		if c.Channel == leadconfig.ChannelEmail {
			locs = append(locs, c.Location())
		}
	}
	if len(locs) == 0 {
		locs = []*time.Location{now.Location()}
	}
	for _, loc := range locs {
		if slot := NextSlot(loc, bh, now); !slot.IsZero() && slot.Before(end) {
			return true
		}
	}
	return false
}
