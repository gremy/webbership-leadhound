package outreach

import (
	"maps"
	"slices"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// mailboxFirstSendConfig is a first-send ramp config with the given domain_per_day.
func mailboxFirstSendConfig(t *testing.T, domainPerDay int, ramp ...leadconfig.Ramp) leadconfig.Config {
	t.Helper()
	cfg := leadconfig.Config{}
	if err := yaml.Unmarshal([]byte("start_date: "+leadconfig.FirstSend+"\n"), &cfg.Caps); err != nil {
		t.Fatal(err)
	}
	cfg.Caps.Ramp = ramp
	cfg.Caps.DomainPerDay = domainPerDay
	cfg.Schedule.BusinessHours = scheduleHours()
	return cfg
}

func mailboxRamp() []leadconfig.Ramp {
	return []leadconfig.Ramp{{FromDay: 0, PerDay: 5}, {FromDay: 7, PerDay: 12}, {FromDay: 14, PerDay: 25}, {FromDay: 21, PerDay: 60}}
}

// TestMailboxCapDrift is the contract-drift test (MB-R7): with one mailbox, MailboxCap and
// RoomToday equal the old single-mailbox CapOn (less what today has sent) on ramp days 0..30.
func TestMailboxCapDrift(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	first := time.Date(2026, 11, 2, 15, 0, 0, 0, london)
	configs := map[string]leadconfig.Config{
		"first-send": mailboxFirstSendConfig(t, leadconfig.DefaultDomainPerDay, mailboxRamp()...),
		"fixed date": func() leadconfig.Config {
			c := scheduleCapConfig(mailboxRamp()...)
			c.Caps.DomainPerDay = leadconfig.DefaultDomainPerDay
			return c
		}(),
		"unset domain cap": mailboxFirstSendConfig(t, 0, mailboxRamp()...),
	}
	for name, cfg := range configs {
		for d := 0; d <= 30; d++ {
			day := time.Date(2026, 11, 2+d, 10, 0, 0, 0, london)
			for _, sent := range []int{0, 3, 50} {
				m := MailboxLoad{ID: 7, Domain: "send.example", FirstSend: &first, SentToday: sent, Ready: true}
				old := CapOn(cfg, &first, day)
				if got := MailboxCap(cfg, m, day); got != old {
					t.Errorf("%s day %d: MailboxCap = %d, want CapOn %d", name, d, got, old)
				}
				if got, want := RoomToday(cfg, []MailboxLoad{m}, day)[7], max(0, old-sent); got != want {
					t.Errorf("%s day %d sent %d: RoomToday = %d, want %d", name, d, sent, got, want)
				}
			}
			none := MailboxLoad{ID: 1, Domain: "send.example", Ready: true}
			if got, want := MailboxCap(cfg, none, day), CapOn(cfg, nil, day); got != want {
				t.Errorf("%s day %d nil FirstSend: MailboxCap = %d, want %d", name, d, got, want)
			}
		}
	}
}

func TestDomainCap(t *testing.T) {
	tests := []struct {
		name string
		set  int
		want int
	}{
		{"unset is the default", 0, leadconfig.DefaultDomainPerDay},
		{"negative is the default", -3, leadconfig.DefaultDomainPerDay},
		{"below the ceiling", 30, 30},
		{"at the ceiling", leadconfig.HardDomainDailyCeiling, leadconfig.HardDomainDailyCeiling},
		{"above the ceiling is clamped", 500, leadconfig.HardDomainDailyCeiling},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := leadconfig.Config{Caps: leadconfig.Caps{DomainPerDay: tc.set}}
			if got := DomainCap(cfg); got != tc.want {
				t.Errorf("DomainCap = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRoomToday(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	day := time.Date(2026, 12, 2, 10, 0, 0, 0, london)
	long := day.AddDate(0, -2, 0) // day 61: the top ramp step, clamped to 40
	cfg := mailboxFirstSendConfig(t, 50, mailboxRamp()...)
	mb := func(id int64, domain string, sent int, ready bool) MailboxLoad {
		return MailboxLoad{ID: id, Domain: domain, FirstSend: &long, SentToday: sent, Ready: ready}
	}
	tests := []struct {
		name string
		cfg  leadconfig.Config
		mbs  []MailboxLoad
		want map[int64]int
	}{
		{"one mailbox at the ceiling", cfg, []MailboxLoad{mb(1, "a.example", 0, true)}, map[int64]int{1: 40}},
		{"two on a domain share its cap in id order", cfg,
			[]MailboxLoad{mb(9, "a.example", 0, true), mb(2, "a.example", 0, true)}, map[int64]int{2: 40, 9: 10}},
		{"today's sends on the domain count", cfg,
			[]MailboxLoad{mb(1, "a.example", 30, true), mb(2, "a.example", 15, true)}, map[int64]int{1: 5, 2: 0}},
		{"domains are compared without case", cfg,
			[]MailboxLoad{mb(1, "A.Example", 0, true), mb(2, "a.example ", 0, true)}, map[int64]int{1: 40, 2: 10}},
		{"other domains are independent", cfg,
			[]MailboxLoad{mb(1, "a.example", 0, true), mb(2, "b.example", 0, true)}, map[int64]int{1: 40, 2: 40}},
		{"not ready gets no room but its sends count", cfg,
			[]MailboxLoad{mb(1, "a.example", 20, false), mb(2, "a.example", 0, true)}, map[int64]int{1: 0, 2: 30}},
		{"sent over the cap is no room", cfg, []MailboxLoad{mb(1, "a.example", 45, true)}, map[int64]int{1: 0}},
		{"nil FirstSend is day 0", cfg,
			[]MailboxLoad{{ID: 3, Domain: "a.example", Ready: true}}, map[int64]int{3: 5}},
		{"no mailboxes", cfg, nil, map[int64]int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoomToday(tc.cfg, tc.mbs, day); !maps.Equal(got, tc.want) {
				t.Errorf("RoomToday = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRoomTodayDomainCeiling: however many mailboxes share a domain, their room together
// never exceeds DomainCap less the domain's sends today.
func TestRoomTodayDomainCeiling(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	day := time.Date(2026, 12, 2, 10, 0, 0, 0, london)
	long := day.AddDate(0, -2, 0)
	for _, perDay := range []int{1, 10, 50, 79, 100, 400} {
		cfg := mailboxFirstSendConfig(t, perDay, mailboxRamp()...)
		for n := 1; n <= 5; n++ {
			for sent := 0; sent <= 60; sent += 7 {
				var mbs []MailboxLoad
				total := 0
				for i := range n {
					s := sent / n
					total += s
					mbs = append(mbs, MailboxLoad{ID: int64(i + 1), Domain: "d.example", FirstSend: &long, SentToday: s, Ready: true})
				}
				sum := 0
				for _, r := range RoomToday(cfg, mbs, day) {
					sum += r
				}
				if limit := max(0, DomainCap(cfg)-total); sum > limit {
					t.Errorf("domain_per_day %d, %d mailboxes, sent %d: room %d over %d", perDay, n, total, sum, limit)
				}
			}
		}
	}
}

func TestRoomOver(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	bh := scheduleHours()
	// Wednesday 2 December 2026.
	wedOpen := time.Date(2026, 12, 2, 11, 0, 0, 0, london)
	wedShut := time.Date(2026, 12, 2, 17, 0, 0, 0, london)
	friShut := time.Date(2026, 12, 4, 17, 0, 0, 0, london)
	long := wedOpen.AddDate(0, -2, 0)
	cfg := mailboxFirstSendConfig(t, 50, mailboxRamp()...)
	step := mailboxFirstSendConfig(t, 50, leadconfig.Ramp{FromDay: 0, PerDay: 4}, leadconfig.Ramp{FromDay: 1, PerDay: 10})
	yesterday := wedOpen.AddDate(0, 0, -1)
	mb := func(id int64, domain string, sent, waiting int) MailboxLoad {
		return MailboxLoad{ID: id, Domain: domain, FirstSend: &long, SentToday: sent, Waiting: waiting, Ready: true}
	}
	tests := []struct {
		name string
		cfg  leadconfig.Config
		mbs  []MailboxLoad
		now  time.Time
		want map[int64]int
	}{
		// Today counts with its sends taken off, then Thursday and Friday whole.
		{"today open", cfg, []MailboxLoad{mb(1, "a.example", 10, 0)}, wedOpen, map[int64]int{1: 30 + 40 + 40}},
		// Today's window is over: Thursday, Friday and Monday.
		{"today over", cfg, []MailboxLoad{mb(1, "a.example", 10, 0)}, wedShut, map[int64]int{1: 120}},
		{"weekend skipped", cfg, []MailboxLoad{mb(1, "a.example", 0, 0)}, friShut, map[int64]int{1: 120}},
		{"waiting taken off", cfg, []MailboxLoad{mb(1, "a.example", 0, 25)}, wedOpen, map[int64]int{1: 95}},
		{"waiting over the room is 0", cfg, []MailboxLoad{mb(1, "a.example", 0, 500)}, wedOpen, map[int64]int{1: 0}},
		{"domain cap each day", cfg,
			[]MailboxLoad{mb(1, "a.example", 0, 0), mb(2, "a.example", 0, 0)}, wedOpen, map[int64]int{1: 120, 2: 30}},
		{"not ready gets no room", cfg,
			[]MailboxLoad{{ID: 1, Domain: "a.example", FirstSend: &long, Ready: false}}, wedOpen, map[int64]int{1: 0}},
		// No send yet: today is day 0 and the later days ramp on from it (4 + 10 + 10), as the
		// batch sizing counts, not each day its own day 0 (4 + 4 + 4).
		{"nil FirstSend: today is day 0", step,
			[]MailboxLoad{{ID: 1, Domain: "a.example", Ready: true}}, wedOpen, map[int64]int{1: 24}},
		{"first send yesterday ramps up", step,
			[]MailboxLoad{{ID: 1, Domain: "a.example", FirstSend: &yesterday, Ready: true}}, wedOpen, map[int64]int{1: 30}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoomOver(tc.cfg, tc.mbs, tc.now, 3, bh); !maps.Equal(got, tc.want) {
				t.Errorf("RoomOver = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssign(t *testing.T) {
	tests := []struct {
		name  string
		leads []int64
		room  map[int64]int
		steps int
		want  map[int64]int64
	}{
		{"balanced over equal room", []int64{10, 11, 12, 13}, map[int64]int{1: 6, 2: 6}, 3,
			map[int64]int64{10: 1, 11: 2, 12: 1, 13: 2}},
		{"most room first", []int64{10, 11, 12}, map[int64]int{1: 3, 2: 9}, 3,
			map[int64]int64{10: 2, 11: 2, 12: 1}},
		{"tie goes to the lowest id", []int64{10}, map[int64]int{5: 3, 2: 3, 9: 3}, 3, map[int64]int64{10: 2}},
		{"no room left: unassigned", []int64{10, 11, 12}, map[int64]int{1: 3, 2: 3}, 3,
			map[int64]int64{10: 1, 11: 2}},
		{"room short of a whole sequence", []int64{10}, map[int64]int{1: 2}, 3, map[int64]int64{}},
		{"a lead given twice is assigned once", []int64{10, 10}, map[int64]int{1: 6}, 3, map[int64]int64{10: 1}},
		{"no mailboxes", []int64{10}, nil, 3, map[int64]int64{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := maps.Clone(tc.room)
			got := Assign(tc.leads, tc.room, tc.steps)
			if !maps.Equal(got, tc.want) {
				t.Errorf("Assign = %v, want %v", got, tc.want)
			}
			if !maps.Equal(tc.room, in) {
				t.Errorf("Assign changed its room map: %v, was %v", tc.room, in)
			}
			for range 5 {
				if again := Assign(tc.leads, tc.room, tc.steps); !maps.Equal(again, got) {
					t.Fatalf("Assign is not deterministic: %v then %v", got, again)
				}
			}
		})
	}
}

// TestPlanMailboxesDrift is the contract-drift test (MB-R7): with one mailbox and room =
// the old capToday - sentToday, PlanMailboxes returns exactly what Plan returns.
func TestPlanMailboxesDrift(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	tokyo := scheduleZone(t, "Asia/Tokyo")
	bh := scheduleHours()
	notYet := planItem(9, 1, 9, 0, "later.example", london)
	notYet.DueAt = planNow.Add(time.Minute)
	self := planItem(50, 0, 50, 0, "operator.example", london)
	self.Selftest = true
	selfNight := self
	selfNight.MessageID, selfNight.Loc = 51, tokyo
	follow := planItem(6, 3, 60, 1, "f.example", london)
	follow.DueAt = planNow.Add(-48 * time.Hour)
	due := []PlanItem{
		planItem(1, 2, 20, 0, "a.example", london),
		planItem(2, 1, 30, 0, "b.example", london),
		planItem(3, 1, 10, 0, "mail.b.example", london),
		planItem(4, 1, 11, 0, "c.example", tokyo),
		planItem(5, 1, 12, 0, "d.example", nil),
		notYet, self, selfNight, follow,
	}
	for i := range due {
		due[i].MailboxID = 4
	}
	for _, capToday := range []int{0, 1, 2, 3, 5, 40} {
		for _, sentToday := range []int{0, 1, 2, 4, 41} {
			for n := 0; n <= len(due); n++ {
				items := due[:n]
				wantSend, wantHeld := Plan(items, sentToday, capToday, planNow, bh)
				gotSend, gotHeld := PlanMailboxes(items, map[int64]int{4: capToday - sentToday}, planNow, bh)
				if !slices.Equal(gotSend, wantSend) || !maps.Equal(gotHeld, wantHeld) {
					t.Errorf("cap %d sent %d items %d: PlanMailboxes = %v %v, Plan = %v %v",
						capToday, sentToday, n, planIDs(gotSend), gotHeld, planIDs(wantSend), wantHeld)
				}
			}
		}
	}
}

func TestPlanMailboxes(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	tokyo := scheduleZone(t, "Asia/Tokyo")
	bh := scheduleHours()
	on := func(it PlanItem, mailbox int64) PlanItem {
		it.MailboxID = mailbox
		return it
	}
	notYet := on(planItem(2, 1, 11, 0, "b.example", london), 1)
	notYet.DueAt = planNow.Add(time.Minute)
	self := on(planItem(50, 0, 50, 0, "operator.example", london), 1)
	self.Selftest = true
	tests := []struct {
		name string
		due  []PlanItem
		room map[int64]int
		want []int64
		held map[HoldReason]int
	}{
		{
			name: "each mailbox its own room",
			due: []PlanItem{
				on(planItem(1, 1, 10, 0, "a.example", london), 1),
				on(planItem(2, 1, 11, 0, "b.example", london), 1),
				on(planItem(3, 1, 12, 0, "c.example", london), 2),
				on(planItem(4, 1, 13, 0, "d.example", london), 2),
			},
			room: map[int64]int{1: 1, 2: 2},
			want: []int64{1, 3, 4},
			held: map[HoldReason]int{HoldCap: 1},
		},
		{
			name: "a domain-held item uses no room",
			due: []PlanItem{
				on(planItem(1, 1, 10, 0, "x.example", london), 1),
				on(planItem(2, 1, 11, 0, "x.example", london), 2),
				on(planItem(3, 1, 12, 0, "y.example", london), 2),
			},
			room: map[int64]int{1: 1, 2: 1},
			want: []int64{1, 3},
			held: map[HoldReason]int{HoldDomain: 1},
		},
		{
			name: "not-due and out-of-hours items use no room",
			due: []PlanItem{
				notYet,
				on(planItem(3, 1, 12, 0, "c.example", tokyo), 1),
				on(planItem(4, 1, 13, 0, "d.example", london), 1),
			},
			room: map[int64]int{1: 1},
			want: []int64{4},
			held: map[HoldReason]int{HoldNotDue: 1, HoldHours: 1},
		},
		{
			name: "a mailbox with no room entry holds its items",
			due:  []PlanItem{on(planItem(1, 1, 10, 0, "a.example", london), 7)},
			room: map[int64]int{1: 5},
			want: []int64{},
			held: map[HoldReason]int{HoldCap: 1},
		},
		{
			name: "a selftest bypasses the room and uses a place",
			due:  []PlanItem{self, on(planItem(1, 1, 10, 0, "a.example", london), 1)},
			room: map[int64]int{1: 1},
			want: []int64{50},
			held: map[HoldReason]int{HoldCap: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := slices.Clone(tc.due)
			room := maps.Clone(tc.room)
			send, held := PlanMailboxes(tc.due, tc.room, planNow, bh)
			if got := planIDs(send); !slices.Equal(got, tc.want) {
				t.Errorf("send = %v, want %v", got, tc.want)
			}
			if !maps.Equal(held, tc.held) {
				t.Errorf("held = %v, want %v", held, tc.held)
			}
			if !slices.Equal(tc.due, in) || !maps.Equal(tc.room, room) {
				t.Error("PlanMailboxes changed its input")
			}
		})
	}
}

// TestPlanMailboxesAnyHour (Send now): with every recipient outside the window, PlanMailboxes
// holds them all for hours and PlanMailboxesAnyHour holds none for hours, yet keeps the
// not-due, one-per-domain and room rules exactly as PlanMailboxes applies them in hours.
func TestPlanMailboxesAnyHour(t *testing.T) {
	tokyo := scheduleZone(t, "Asia/Tokyo") // planNow is night in Tokyo
	bh := scheduleHours()
	notYet := planItem(9, 1, 9, 0, "later.example", tokyo)
	notYet.DueAt = planNow.Add(time.Minute)
	due := []PlanItem{
		planItem(1, 1, 10, 0, "a.example", tokyo),
		planItem(2, 1, 11, 0, "mail.a.example", tokyo),
		planItem(3, 1, 12, 0, "b.example", tokyo),
		planItem(4, 1, 13, 0, "c.example", tokyo),
		notYet,
	}
	for i := range due {
		due[i].MailboxID = 4
	}
	room := map[int64]int{4: 1}
	if send, held := PlanMailboxes(due, room, planNow, bh); len(send) != 0 || held[HoldHours] != 4 {
		t.Fatalf("in the window's rules: send %v held %v, want all held for hours", send, held)
	}
	send, held := PlanMailboxesAnyHour(due, room, planNow)
	if len(send) != 1 || send[0].MessageID != 1 {
		t.Errorf("send %v, want message 1 alone (room 1)", send)
	}
	want := map[HoldReason]int{HoldNotDue: 1, HoldDomain: 1, HoldCap: 2}
	if !maps.Equal(held, want) {
		t.Errorf("held %v, want %v", held, want)
	}
}
