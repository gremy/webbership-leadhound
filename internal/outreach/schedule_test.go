package outreach

import (
	"math/rand/v2"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func scheduleZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func scheduleHours() leadconfig.BusinessHours {
	return leadconfig.BusinessHours{Start: "09:30", End: "16:30", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri"}}
}

func scheduleCapConfig(ramp ...leadconfig.Ramp) leadconfig.Config {
	return leadconfig.Config{Caps: leadconfig.Caps{
		StartDate: leadconfig.Date{Time: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)},
		Ramp:      ramp,
	}}
}

func scheduleAt(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return v
}

func TestCapToday(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	bucharest := scheduleZone(t, "Europe/Bucharest")
	normal := scheduleCapConfig(
		leadconfig.Ramp{FromDay: 0, PerDay: 5},
		leadconfig.Ramp{FromDay: 7, PerDay: 12},
		leadconfig.Ramp{FromDay: 14, PerDay: 25},
	)
	over := scheduleCapConfig(
		leadconfig.Ramp{FromDay: 0, PerDay: 5},
		leadconfig.Ramp{FromDay: 3, PerDay: 90},
	)
	tests := []struct {
		name string
		cfg  leadconfig.Config
		now  string
		loc  *time.Location
		want int
	}{
		{"day before start", normal, "2026-11-15T12:00:00Z", london, 0},
		{"last second before start", normal, "2026-11-15T23:59:59Z", london, 0},
		{"start day", normal, "2026-11-16T00:00:00Z", london, 5},
		{"day 6", normal, "2026-11-22T12:00:00Z", london, 5},
		{"day 7 switch", normal, "2026-11-23T00:00:00Z", london, 12},
		{"day 14 switch", normal, "2026-11-30T09:00:00Z", london, 25},
		{"long after start", normal, "2027-06-01T09:00:00Z", london, 25},
		{"ramp over the ceiling is clamped", over, "2026-11-20T09:00:00Z", london, leadconfig.HardDailyCeiling},
		{"ramp under the ceiling before it", over, "2026-11-18T09:00:00Z", london, 5},
		// 22:30 UTC on day 6 is already 00:30 on day 7 in Bucharest: the cap's zone decides.
		{"midnight rollover in the cap zone", normal, "2026-11-22T22:30:00Z", bucharest, 12},
		{"same instant in London is still day 6", normal, "2026-11-22T22:30:00Z", london, 5},
		{"start day begins at local midnight", normal, "2026-11-15T22:30:00Z", bucharest, 5},
		{"empty ramp", scheduleCapConfig(), "2026-11-20T09:00:00Z", london, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapToday(tc.cfg, scheduleAt(t, tc.now), tc.loc); got != tc.want {
				t.Errorf("CapToday = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCapOn: with caps.start_date first-send, day 0 is the first live send's local date, or
// the day asked about when nothing has gone yet; a fixed date keeps its meaning whatever
// the first send.
func TestCapOn(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	bucharest := scheduleZone(t, "Europe/Bucharest")
	ramp := []leadconfig.Ramp{{FromDay: 0, PerDay: 4}, {FromDay: 7, PerDay: 12}, {FromDay: 14, PerDay: 20}}
	firstSend := leadconfig.Config{Caps: leadconfig.Caps{Ramp: ramp}}
	if err := yaml.Unmarshal([]byte("start_date: "+leadconfig.FirstSend+"\n"), &firstSend.Caps); err != nil {
		t.Fatal(err)
	}
	firstSend.Caps.Ramp = ramp
	fixed := scheduleCapConfig(ramp...)
	at := func(s string, loc *time.Location) *time.Time {
		v := scheduleAt(t, s).In(loc)
		return &v
	}
	tests := []struct {
		name  string
		cfg   leadconfig.Config
		first *time.Time
		day   time.Time
		want  int
	}{
		{"no send yet: day 0 today", firstSend, nil, *at("2026-12-01T10:00:00Z", london), 4},
		{"no send yet, months on: still day 0", firstSend, nil, *at("2027-03-01T10:00:00Z", london), 4},
		{"first send 8 days ago: week 2", firstSend, at("2026-12-01T15:00:00Z", london), *at("2026-12-09T09:00:00Z", london), 12},
		{"first send 6 days ago: week 1", firstSend, at("2026-12-01T15:00:00Z", london), *at("2026-12-07T23:00:00Z", london), 4},
		{"first send 15 days ago: week 3", firstSend, at("2026-12-01T15:00:00Z", london), *at("2026-12-16T09:00:00Z", london), 20},
		// 22:30 UTC on 30 Nov is 1 Dec in Bucharest: the dates are read in the zone given.
		{"first send's local date", firstSend, at("2026-11-30T22:30:00Z", bucharest), *at("2026-12-08T09:00:00Z", bucharest), 12},
		{"day before the first send", firstSend, at("2026-12-01T15:00:00Z", london), *at("2026-11-30T09:00:00Z", london), 0},
		{"fixed date ignores the first send", fixed, at("2026-12-20T10:00:00Z", london), *at("2026-11-24T10:00:00Z", london), 12},
		{"fixed date before start", fixed, nil, *at("2026-11-15T10:00:00Z", london), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapOn(tc.cfg, tc.first, tc.day); got != tc.want {
				t.Errorf("CapOn = %d, want %d", got, tc.want)
			}
		})
	}
	if got := CapToday(firstSend, scheduleAt(t, "2027-03-01T10:00:00Z"), london); got != 4 {
		t.Errorf("CapToday with first-send = %d, want day 0's 4", got)
	}
}

func TestNextSlot(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	bucharest := scheduleZone(t, "Europe/Bucharest")
	tests := []struct {
		name  string
		loc   *time.Location
		after string
		want  string
	}{
		{"inside the window is itself", london, "2026-11-18T11:00:00Z", "2026-11-18T11:00:00Z"},
		{"exactly at open", london, "2026-11-18T09:30:00Z", "2026-11-18T09:30:00Z"},
		{"before open the same day", london, "2026-11-18T07:00:00Z", "2026-11-18T09:30:00Z"},
		{"at close moves on", london, "2026-11-18T16:30:00Z", "2026-11-19T09:30:00Z"},
		{"Friday evening to Monday", london, "2026-11-20T18:00:00Z", "2026-11-23T09:30:00Z"},
		{"Saturday to Monday 09:30 London", london, "2026-11-21T10:00:00Z", "2026-11-23T09:30:00Z"},
		{"Saturday to Monday 09:30 Bucharest", bucharest, "2026-11-21T10:00:00Z", "2026-11-23T07:30:00Z"},
		// Last Sunday of March 2026 is the 29th: clocks go forward over that weekend.
		{"spring DST London", london, "2026-03-28T12:00:00Z", "2026-03-30T08:30:00Z"},
		{"spring DST Bucharest", bucharest, "2026-03-28T12:00:00Z", "2026-03-30T06:30:00Z"},
		{"spring DST inside the gap", london, "2026-03-29T01:30:00Z", "2026-03-30T08:30:00Z"},
		// Last Sunday of October 2026 is the 25th: clocks go back over that weekend.
		{"autumn DST London", london, "2026-10-24T12:00:00Z", "2026-10-26T09:30:00Z"},
		{"autumn DST Bucharest", bucharest, "2026-10-24T12:00:00Z", "2026-10-26T07:30:00Z"},
		{"autumn DST in the repeated hour", bucharest, "2026-10-25T00:30:00Z", "2026-10-26T07:30:00Z"},
		{"Friday before autumn DST is summer time", london, "2026-10-23T07:00:00Z", "2026-10-23T08:30:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NextSlot(tc.loc, scheduleHours(), scheduleAt(t, tc.after))
			if want := scheduleAt(t, tc.want); !got.Equal(want) {
				t.Errorf("NextSlot = %s, want %s", got.UTC().Format(time.RFC3339), tc.want)
			}
		})
	}
}

func TestNextSlotNoWindow(t *testing.T) {
	now := scheduleAt(t, "2026-11-18T11:00:00Z")
	for name, bh := range map[string]leadconfig.BusinessHours{
		"bad start": {Start: "9h", End: "16:30", Days: []string{"Mon"}},
		"no days":   {Start: "09:30", End: "16:30"},
		"end first": {Start: "16:30", End: "09:30", Days: []string{"Mon"}},
	} {
		if got := NextSlot(time.UTC, bh, now); !got.IsZero() {
			t.Errorf("%s: NextSlot = %s, want zero", name, got)
		}
		if InBusinessHours(time.UTC, bh, now) {
			t.Errorf("%s: InBusinessHours = true, want false", name)
		}
	}
}

func TestInBusinessHours(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	bucharest := scheduleZone(t, "Europe/Bucharest")
	tests := []struct {
		name string
		loc  *time.Location
		at   string
		want bool
	}{
		{"mid morning", london, "2026-11-18T10:00:00Z", true},
		{"at open", london, "2026-11-18T09:30:00Z", true},
		{"a second before open", london, "2026-11-18T09:29:59Z", false},
		{"at close is out", london, "2026-11-18T16:30:00Z", false},
		{"Saturday", london, "2026-11-21T10:00:00Z", false},
		{"same instant, earlier zone", bucharest, "2026-11-18T15:00:00Z", false},
		{"summer time Friday", london, "2026-10-23T08:45:00Z", true},
		{"winter time Monday, same UTC clock", london, "2026-10-26T08:45:00Z", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := InBusinessHours(tc.loc, scheduleHours(), scheduleAt(t, tc.at)); got != tc.want {
				t.Errorf("InBusinessHours = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFollowupDue(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	bucharest := scheduleZone(t, "Europe/Bucharest")
	tests := []struct {
		name string
		loc  *time.Location
		prev string
		days int
		want string
	}{
		{"same week", london, "2026-11-16T10:00:00Z", 2, "2026-11-18T10:00:00Z"},
		{"Thursday plus 4 crosses a weekend", london, "2026-11-19T10:00:00Z", 4, "2026-11-25T10:00:00Z"},
		{"Friday plus 1 is Monday", london, "2026-11-20T14:00:00Z", 1, "2026-11-23T14:00:00Z"},
		{"plus 7 crosses two weekends", london, "2026-11-20T14:00:00Z", 7, "2026-12-01T14:00:00Z"},
		{"sent before open lands at open", london, "2026-11-16T08:00:00Z", 1, "2026-11-17T09:30:00Z"},
		{"sent on Saturday plus 1 is Monday", london, "2026-11-21T10:00:00Z", 1, "2026-11-23T10:00:00Z"},
		// Thursday 22 Oct 10:00 BST; four business days on is Wednesday 28 Oct, now GMT.
		{"across autumn DST keeps the local clock", london, "2026-10-22T09:00:00Z", 4, "2026-10-28T10:00:00Z"},
		{"across spring DST Bucharest", bucharest, "2026-03-26T08:00:00Z", 2, "2026-03-30T07:00:00Z"},
		{"zero days is the next slot", london, "2026-11-20T18:00:00Z", 0, "2026-11-23T09:30:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FollowupDue(scheduleAt(t, tc.prev), tc.days, tc.loc, scheduleHours())
			if want := scheduleAt(t, tc.want); !got.Equal(want) {
				t.Errorf("FollowupDue = %s, want %s", got.UTC().Format(time.RFC3339), tc.want)
			}
		})
	}
}

func TestSpacing(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- a fixed seed for the test
	seenLo, seenHi := false, false
	for range 2000 {
		d := Spacing(r, [2]int{3, 9})
		if d < 3*time.Minute || d > 9*time.Minute {
			t.Fatalf("Spacing = %s, outside 3m..9m", d)
		}
		seenLo = seenLo || d < 4*time.Minute
		seenHi = seenHi || d > 8*time.Minute
	}
	if !seenLo || !seenHi {
		t.Errorf("Spacing does not spread over the range: low %v, high %v", seenLo, seenHi)
	}
	if d := Spacing(r, [2]int{5, 5}); d != 5*time.Minute {
		t.Errorf("Spacing(5,5) = %s, want 5m", d)
	}
	if d := Spacing(r, [2]int{9, 3}); d < 3*time.Minute || d > 9*time.Minute {
		t.Errorf("Spacing(9,3) = %s, want within 3m..9m", d)
	}
	if d := Spacing(r, [2]int{-4, 0}); d != 0 {
		t.Errorf("Spacing(-4,0) = %s, want 0", d)
	}
}
