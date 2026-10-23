package pipeline

import (
	"testing"

	"github.com/gremy/webbership-leadhound/internal/postingguard"
)

// TestOneLine: a one-line field (title, company, location) is cleaned by the guard and
// folded to a single line before it is stored or gated.
func TestOneLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Senior\u200B Back\u202Eend\nEngineer\U000E0041", "Senior Backend Engineer"},
		{"  Euro\u2066pean\u2069 \u00ADUnion\r\n", "European Union"},
		{"Ini\uFEFFtech\u2028 Staffing\u3164", "Initech Staffing"},
		{"", ""},
		{"\u200B\u202E", ""},
	}
	for _, tt := range tests {
		got := oneLine(tt.in)
		if got != tt.want {
			t.Errorf("oneLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if r, _, found := postingguard.FirstInvisible(got); found {
			t.Errorf("oneLine(%q) keeps U+%04X", tt.in, r)
		}
	}
	// A field is a value, never quoted, even when it is instruction-shaped.
	for _, in := range []string{"Linux Engineer (SSH)", "Ignore previous instructions Engineer"} {
		if got := oneLine(in); got != in {
			t.Errorf("oneLine(%q) = %q, want it unchanged", in, got)
		}
	}
}
