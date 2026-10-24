package outreach

import (
	"math/rand/v2"
	"slices"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// scheduleSearchDays bounds the search for a business day; the config allows at least one
// weekday, so a week always holds one, and the margin covers a calendar oddity.
const scheduleSearchDays = 15

// CapToday is today's send cap for the one mailbox: the ramp step for the whole local days
// since caps.start_date, counted in loc (the operator's LEADHOUND_TZ, never the recipient's),
// and never above leadconfig.HardDailyCeiling. Before the start date it is 0.
func CapToday(cfg leadconfig.Config, now time.Time, loc *time.Location) int {
	sd := cfg.Caps.StartDate.Time
	if sd.IsZero() {
		return 0
	}
	start := time.Date(sd.Year(), sd.Month(), sd.Day(), 0, 0, 0, 0, time.UTC)
	t := now.In(loc)
	today := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if today.Before(start) {
		return 0
	}
	days := int(today.Sub(start) / (24 * time.Hour))
	perDay := 0
	for _, r := range cfg.Caps.Ramp {
		if r.FromDay <= days {
			perDay = r.PerDay
		}
	}
	return max(0, min(perDay, leadconfig.HardDailyCeiling))
}

// scheduleWindow returns the business window's open and close on the calendar day of d in
// loc, and whether that day is a business day. ok is false when the hours do not parse or
// the window is empty.
func scheduleWindow(loc *time.Location, bh leadconfig.BusinessHours, d time.Time) (open, shut time.Time, business, ok bool) {
	start, errS := time.Parse("15:04", bh.Start)
	end, errE := time.Parse("15:04", bh.End)
	if errS != nil || errE != nil || !end.After(start) {
		return time.Time{}, time.Time{}, false, false
	}
	d = d.In(loc)
	open = time.Date(d.Year(), d.Month(), d.Day(), start.Hour(), start.Minute(), 0, 0, loc)
	shut = time.Date(d.Year(), d.Month(), d.Day(), end.Hour(), end.Minute(), 0, 0, loc)
	return open, shut, scheduleBusinessDay(bh, d), true
}

func scheduleBusinessDay(bh leadconfig.BusinessHours, d time.Time) bool {
	return slices.Contains(bh.Days, d.Weekday().String()[:3])
}

// scheduleDay is noon on the calendar day i days after t's day in loc; noon keeps clear of
// every DST change, which happens in the small hours.
func scheduleDay(loc *time.Location, t time.Time, i int) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day()+i, 12, 0, 0, 0, loc)
}

// InBusinessHours reports whether t falls inside the business window in loc: a business day,
// at or after the start and before the end (RA8).
func InBusinessHours(loc *time.Location, bh leadconfig.BusinessHours, t time.Time) bool {
	open, shut, business, ok := scheduleWindow(loc, bh, t)
	return ok && business && !t.Before(open) && t.Before(shut)
}

// NextSlot is the earliest instant at or after `after` inside the business window in loc:
// `after` itself when it is inside, else the next window's start. It is the zero time when
// the hours do not parse or no business day falls in the next two weeks.
func NextSlot(loc *time.Location, bh leadconfig.BusinessHours, after time.Time) time.Time {
	for i := range scheduleSearchDays {
		open, shut, business, ok := scheduleWindow(loc, bh, scheduleDay(loc, after, i))
		switch {
		case !ok:
			return time.Time{}
		case !business || !after.Before(shut):
			continue
		case after.Before(open):
			return open
		default:
			return after
		}
	}
	return time.Time{}
}

// FollowupDue is when a follow-up falls due: businessDays business days after the calendar
// day prevSent fell on in loc, at prevSent's local clock time, moved to NextSlot when that
// clock time is outside the window; the day counting skips every non-business day, so one
// business day after a Saturday is the Monday. businessDays below 1 gives NextSlot(prevSent).
func FollowupDue(prevSent time.Time, businessDays int, loc *time.Location, bh leadconfig.BusinessHours) time.Time {
	if businessDays < 1 {
		return NextSlot(loc, bh, prevSent)
	}
	if _, _, _, ok := scheduleWindow(loc, bh, prevSent); !ok || len(bh.Days) == 0 {
		return time.Time{}
	}
	day := scheduleDay(loc, prevSent, 0)
	for n := 0; n < businessDays; {
		day = scheduleDay(loc, day, 1)
		if scheduleBusinessDay(bh, day) {
			n++
		}
	}
	p := prevSent.In(loc)
	at := time.Date(day.Year(), day.Month(), day.Day(), p.Hour(), p.Minute(), p.Second(), p.Nanosecond(), loc)
	return NextSlot(loc, bh, at)
}

// Spacing is a random pause between two sends, uniform over [minutes[0], minutes[1]] to the
// second; the bounds may come in either order, and a negative bound counts as 0.
func Spacing(r *rand.Rand, minutes [2]int) time.Duration {
	lo, hi := max(0, minutes[0]), max(0, minutes[1])
	if hi < lo {
		lo, hi = hi, lo
	}
	span := int64(hi-lo) * 60
	return time.Duration(int64(lo)*60+r.Int64N(span+1)) * time.Second
}
