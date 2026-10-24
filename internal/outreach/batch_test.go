package outreach

import (
	"slices"
	"testing"
	"time"
)

// planCoUK and planCoUK2 are fictional second-level .co.uk names, built at runtime.
var (
	planCoUK  = "leadhound-fixture-" + "zq" + ".co.uk"
	planCoUK2 = "leadhound-fixture-" + "zr" + ".co.uk"
)

// planNow is Wednesday 18 November 2026, 11:00 UTC: inside the window in London and Bucharest.
var planNow = time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)

func planItem(id, batch, lead int64, step int, domain string, loc *time.Location) PlanItem {
	return PlanItem{
		MessageID: id, BatchID: batch, LeadID: lead, Step: step,
		Domain: domain, Loc: loc, DueAt: planNow.Add(-time.Hour),
	}
}

func planIDs(items []PlanItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.MessageID)
	}
	return out
}

func TestPlan(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	tokyo := scheduleZone(t, "Asia/Tokyo") // 20:00 there at planNow: outside the window
	bh := scheduleHours()

	notYet := planItem(9, 1, 9, 0, "later.example", london)
	notYet.DueAt = planNow.Add(time.Minute)
	self := planItem(50, 0, 50, 0, "operator.example", london)
	self.Selftest = true
	selfNight := self
	selfNight.MessageID, selfNight.Loc = 51, tokyo

	tests := []struct {
		name      string
		due       []PlanItem
		sentToday int
		cap       int
		want      []int64
		held      map[HoldReason]int
	}{
		{
			name: "step 0 by batch then lead",
			due: []PlanItem{
				planItem(1, 2, 20, 0, "a.example", london),
				planItem(2, 1, 30, 0, "b.example", london),
				planItem(3, 1, 10, 0, "c.example", london),
			},
			cap:  10,
			want: []int64{3, 2, 1},
		},
		{
			name: "follow-ups before first emails, oldest due first",
			due: func() []PlanItem {
				f1 := planItem(4, 1, 40, 1, "f1.example", london)
				f1.DueAt = planNow.Add(-time.Minute)
				f2 := planItem(5, 3, 41, 2, "f2.example", london)
				f2.DueAt = planNow.Add(-48 * time.Hour)
				return []PlanItem{planItem(1, 1, 10, 0, "a.example", london), f1, f2}
			}(),
			cap:  10,
			want: []int64{5, 4, 1},
		},
		{
			name: "one per recipient domain per run",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "same.example", london),
				planItem(2, 1, 11, 0, "Same.Example", london),
				planItem(3, 1, 12, 0, "other.example", london),
			},
			cap:  10,
			want: []int64{1, 3},
			held: map[HoldReason]int{HoldDomain: 1},
		},
		{
			name: "subdomains share their registrable domain",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "x.example", london),
				planItem(2, 1, 11, 0, "mail.x.example", london),
				planItem(3, 1, 12, 0, "Shop.Mail.X.Example", london),
				planItem(4, 1, 13, 0, planCoUK, london),
				planItem(5, 1, 14, 0, "mail."+planCoUK, london),
				planItem(6, 1, 15, 0, planCoUK2, london),
			},
			cap:  10,
			want: []int64{1, 4, 6},
			held: map[HoldReason]int{HoldDomain: 3},
		},
		{
			name: "a domain that is not registrable is compared whole",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "192.0.2.1", london),
				planItem(2, 1, 11, 0, "192.0.2.1", london),
				planItem(3, 1, 12, 0, "192.0.2.2", london),
			},
			cap:  10,
			want: []int64{1, 3},
			held: map[HoldReason]int{HoldDomain: 1},
		},
		{
			name: "a follow-up takes the domain from a first email",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "same.example", london),
				planItem(2, 1, 11, 1, "same.example", london),
			},
			cap:  10,
			want: []int64{2},
			held: map[HoldReason]int{HoldDomain: 1},
		},
		{
			name: "cap minus sent today",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "a.example", london),
				planItem(2, 1, 11, 0, "b.example", london),
				planItem(3, 1, 12, 0, "c.example", london),
			},
			sentToday: 3,
			cap:       5,
			want:      []int64{1, 2},
			held:      map[HoldReason]int{HoldCap: 1},
		},
		{
			name:      "cap reached sends nothing",
			due:       []PlanItem{planItem(1, 1, 10, 0, "a.example", london)},
			sentToday: 5,
			cap:       5,
			want:      []int64{},
			held:      map[HoldReason]int{HoldCap: 1},
		},
		{
			name:      "over the cap already sends nothing",
			due:       []PlanItem{planItem(1, 1, 10, 0, "a.example", london)},
			sentToday: 7,
			cap:       5,
			want:      []int64{},
			held:      map[HoldReason]int{HoldCap: 1},
		},
		{
			name:      "selftest bypasses a reached cap",
			due:       []PlanItem{planItem(1, 1, 10, 0, "a.example", london), self},
			sentToday: 5,
			cap:       5,
			want:      []int64{50},
			held:      map[HoldReason]int{HoldCap: 1},
		},
		{
			name: "selftest counts toward the cap",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "a.example", london),
				planItem(2, 1, 11, 0, "b.example", london),
				self,
			},
			sentToday: 3,
			cap:       5,
			want:      []int64{50, 1},
			held:      map[HoldReason]int{HoldCap: 1},
		},
		{
			name: "outside business hours held whatever its due time",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "jp.example", tokyo),
				planItem(2, 1, 11, 0, "uk.example", london),
			},
			cap:  10,
			want: []int64{2},
			held: map[HoldReason]int{HoldHours: 1},
		},
		{
			name: "selftest still keeps business hours",
			due:  []PlanItem{selfNight},
			cap:  10,
			want: []int64{},
			held: map[HoldReason]int{HoldHours: 1},
		},
		{
			name: "not yet due held",
			due:  []PlanItem{notYet, planItem(2, 1, 11, 0, "uk.example", london)},
			cap:  10,
			want: []int64{2},
			held: map[HoldReason]int{HoldNotDue: 1},
		},
		{
			name: "no due time is not due",
			due: func() []PlanItem {
				it := planItem(1, 1, 10, 0, "a.example", london)
				it.DueAt = time.Time{}
				return []PlanItem{it}
			}(),
			cap:  10,
			want: []int64{},
			held: map[HoldReason]int{HoldNotDue: 1},
		},
		{
			name: "a held item does not take its domain",
			due: func() []PlanItem {
				night := planItem(1, 1, 10, 1, "shared.example", tokyo)
				return []PlanItem{night, planItem(2, 1, 11, 0, "shared.example", london)}
			}(),
			cap:  10,
			want: []int64{2},
			held: map[HoldReason]int{HoldHours: 1},
		},
		{
			name: "the domain rule is checked before the cap",
			due: []PlanItem{
				planItem(1, 1, 10, 0, "a.example", london),
				planItem(2, 1, 11, 0, "a.example", london),
				planItem(3, 1, 12, 0, "b.example", london),
			},
			cap:  1,
			want: []int64{1},
			held: map[HoldReason]int{HoldDomain: 1, HoldCap: 1},
		},
		{
			name: "nothing due",
			cap:  10,
			want: []int64{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := slices.Clone(tc.due)
			send, held := Plan(tc.due, tc.sentToday, tc.cap, planNow, bh)
			if got := planIDs(send); !slices.Equal(got, tc.want) {
				t.Errorf("send = %v, want %v", got, tc.want)
			}
			want := tc.held
			if want == nil {
				want = map[HoldReason]int{}
			}
			if len(held) != len(want) {
				t.Errorf("held = %v, want %v", held, want)
			}
			for r, n := range want {
				if held[r] != n {
					t.Errorf("held[%s] = %d, want %d (all: %v)", r, held[r], n, held)
				}
			}
			if !slices.EqualFunc(in, tc.due, func(a, b PlanItem) bool { return a.MessageID == b.MessageID }) {
				t.Error("Plan reordered its input")
			}
		})
	}
}
