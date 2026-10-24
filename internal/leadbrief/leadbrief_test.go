package leadbrief

import (
	"reflect"
	"testing"
)

func TestNoteWords(t *testing.T) {
	for _, c := range []struct {
		note string
		want map[string]bool
	}{
		{"live · sent 0 · refused 0 · maybe_sent 0 · retry 0 · cancelled 0 · skipped 0 · notes dns", map[string]bool{"dns": true}},
		{"live · sent 2 · held cap 1 · notes stuck-sending, smtp-account · error", map[string]bool{"stuck-sending": true, "smtp-account": true}},
		{"shadow · sent 1 · notes sync running", map[string]bool{"sync running": true}},
		{"shadow · sent 1", map[string]bool{}},
		{"", map[string]bool{}},
	} {
		if got := NoteWords(c.note); !reflect.DeepEqual(got, c.want) {
			t.Errorf("NoteWords(%q) = %v, want %v", c.note, got, c.want)
		}
	}
	// "sync running" is a wait, not a failed sync.
	if NoteWords("live · notes sync running")[noteSync] {
		t.Error("sync running read as a failed sync")
	}
}

func TestDNSPaused(t *testing.T) {
	for _, c := range []struct {
		spf, dkim, dmarc bool
		want             string
	}{
		{true, true, false, "dns: dmarc missing"},
		{false, true, false, "dns: spf, dmarc missing"},
		{false, false, false, "dns: spf, dkim, dmarc missing"},
		{true, true, true, ""},
	} {
		if got := DNSPaused(c.spf, c.dkim, c.dmarc); got != c.want {
			t.Errorf("DNSPaused(%v, %v, %v) = %q, want %q", c.spf, c.dkim, c.dmarc, got, c.want)
		}
	}
}

func TestTodo(t *testing.T) {
	if (Section{Sent: 3, ShadowSent: 1, Bounced: 1, OptedOut: 1, Probe: &ProbeLine{Checked: 1}}).Todo() {
		t.Error("health alone is something to do")
	}
	for name, s := range map[string]Section{
		"replies": {Replies: []string{"alpha.example"}},
		"batch":   {Batch: &BatchReady{ID: 1, Emails: 1}},
		"manual":  {ManualWaiting: 1},
		"check":   {CheckByHand: 1},
		"other":   {Other: 1},
		"paused":  {Paused: PausedSync},
	} {
		if !s.Todo() {
			t.Errorf("%s is not something to do", name)
		}
	}
}
