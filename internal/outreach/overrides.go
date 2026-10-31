package outreach

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// OverrideCode names one volume limit the operator may override for a manual send, with one
// confirm. The set is closed: ParseOverrides refuses any other code. The hard ceilings
// (leadconfig.HardDailyCeiling per mailbox, leadconfig.HardDomainDailyCeiling per domain) and
// a mailbox that is not Ready are never codes and never overridable.
type OverrideCode string

const (
	OverrideWindow        OverrideCode = "window"         // send outside the sending window
	OverrideBusinessHours OverrideCode = "business_hours" // send outside the recipient's business hours
	OverrideRamp          OverrideCode = "ramp"           // a mailbox's own cap at the hard ceiling, not its ramp
	OverrideDomainDaily   OverrideCode = "domain_daily"   // the domain's cap at the hard ceiling, not DomainCap
	OverrideSpacingJitter OverrideCode = "spacing_jitter" // MinSpacing between sends, not the random spacing
	OverrideSchedule      OverrideCode = "schedule"       // send before its due time / outside the batch schedule
)

// MinSpacing is the least time between two sends of one run. With spacing_jitter overridden
// the run waits MinSpacing between sends instead of the random spacing, so the send unit's
// timeout and deliverability still hold.
const MinSpacing = 60 * time.Second

// AllOverrideCodes is the closed set of override codes, sorted.
func AllOverrideCodes() []OverrideCode {
	return []OverrideCode{OverrideBusinessHours, OverrideDomainDaily, OverrideRamp, OverrideSchedule, OverrideSpacingJitter, OverrideWindow}
}

// Overrides is the set of codes the operator confirmed; nil is none.
type Overrides map[OverrideCode]bool

// Has reports whether code is overridden.
func (o Overrides) Has(code OverrideCode) bool { return o[code] }

// Codes is the overridden codes as strings, sorted.
func (o Overrides) Codes() []string {
	out := []string{}
	for c, on := range o {
		if on {
			out = append(out, string(c))
		}
	}
	slices.Sort(out)
	return out
}

// ParseOverrides reads codes into Overrides, once each. An empty or unknown code is refused,
// named in the error, so nothing outside the closed set is ever overridden.
func ParseOverrides(codes []string) (Overrides, error) {
	out := Overrides{}
	for _, c := range codes {
		code := OverrideCode(c)
		if !slices.Contains(AllOverrideCodes(), code) {
			return nil, fmt.Errorf("unknown override code %q", c)
		}
		out[code] = true
	}
	return out, nil
}

// LimitWarning is one overridable volume limit a manual send would pass on one mailbox:
// Used of Allowed already gone today, and Over how many of the sends asked for go past it.
// For OverrideRamp Used is the mailbox's sends today and Allowed its ramp cap; for
// OverrideDomainDaily Used is the domain's sends today and Allowed DomainCap.
type LimitWarning struct {
	Code      OverrideCode
	MailboxID int64
	Domain    string
	Used      int
	Allowed   int
	Over      int
}

// LimitStop is a hard stop on one mailbox, which no override lifts. Reason is "not_ready"
// (the mailbox is not Ready, or not one of those given), "mailbox_ceiling" (past
// leadconfig.HardDailyCeiling: Used is its sends today) or "domain_ceiling" (past
// leadconfig.HardDomainDailyCeiling: Used is the domain's sends today).
type LimitStop struct {
	MailboxID int64
	Domain    string
	Reason    string
	Used      int
	Allowed   int
}

// CheckLimits is what a manual send of need[id] sends from each mailbox on day (the
// operator's today) runs into: the warnings the operator may override and the hard stops no
// override lifts. It is the one reasoning both the preview and the run use. A mailbox with
// a stop gets no warning. With ov holding the codes of warns, RoomTodayWith(ov) has room for
// need on every mailbox without a stop, and RoomToday already has room for it exactly on the
// mailboxes no warning or stop names. Warnings are in mailbox id then code order, stops in
// mailbox id order.
func CheckLimits(cfg leadconfig.Config, mbs []MailboxLoad, day time.Time, need map[int64]int) (warns []LimitWarning, stops []LimitStop) {
	byID := map[int64]MailboxLoad{}
	domainSent := map[string]int{}
	for _, m := range mbs {
		byID[m.ID] = m
		domainSent[mailboxDomain(m.Domain)] += m.SentToday
	}
	base := RoomToday(cfg, mbs, day)
	full := RoomTodayWith(cfg, mbs, day, Overrides{OverrideRamp: true, OverrideDomainDaily: true})
	ids := slices.Sorted(maps.Keys(need))

	var short []int64 // mailboxes past RoomToday with no stop
	ramp := false
	for _, id := range ids {
		n := need[id]
		if n <= 0 {
			continue
		}
		m, ok := byID[id]
		d := mailboxDomain(m.Domain)
		switch {
		case !ok || !m.Ready:
			stops = append(stops, LimitStop{MailboxID: id, Domain: d, Reason: "not_ready"})
		case n > full[id]:
			if n > max(0, leadconfig.HardDailyCeiling-m.SentToday) {
				stops = append(stops, LimitStop{MailboxID: id, Domain: d, Reason: "mailbox_ceiling", Used: m.SentToday, Allowed: leadconfig.HardDailyCeiling})
			} else {
				stops = append(stops, LimitStop{MailboxID: id, Domain: d, Reason: "domain_ceiling", Used: domainSent[d], Allowed: leadconfig.HardDomainDailyCeiling})
			}
		case n > base[id]:
			short = append(short, id)
			capOn := CapOn(cfg, m.FirstSend, day)
			if own := max(0, capOn-m.SentToday); n > own {
				ramp = true
				warns = append(warns, LimitWarning{Code: OverrideRamp, MailboxID: id, Domain: d, Used: m.SentToday, Allowed: capOn, Over: n - own})
			}
		}
	}
	// The domain limit binds wherever the room is still short once the ramp is lifted: the
	// lifted ramp of a lower id can use up a domain's remainder, so it is checked after.
	var ov Overrides
	if ramp {
		ov = Overrides{OverrideRamp: true}
	}
	room := RoomTodayWith(cfg, mbs, day, ov)
	for _, id := range short {
		if n := need[id]; n > room[id] {
			d := mailboxDomain(byID[id].Domain)
			warns = append(warns, LimitWarning{Code: OverrideDomainDaily, MailboxID: id, Domain: d, Used: domainSent[d], Allowed: DomainCap(cfg), Over: n - room[id]})
		}
	}
	slices.SortStableFunc(warns, func(a, b LimitWarning) int {
		return cmp.Or(cmp.Compare(a.MailboxID, b.MailboxID), strings.Compare(string(a.Code), string(b.Code)))
	})
	return warns, stops
}
