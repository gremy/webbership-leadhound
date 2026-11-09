package console

import "testing"

// TestPluralGroupsThousands checks that a counted noun writes its number the way the chips
// and the step bar do, with a comma between each group of three digits.
func TestPluralGroupsThousands(t *testing.T) {
	for _, tt := range []struct {
		n    int
		want string
	}{
		{0, "0 leads"},
		{1, "1 lead"},
		{999, "999 leads"},
		{2267, "2,267 leads"},
		{1234567, "1,234,567 leads"},
	} {
		if got := plural(tt.n, "lead", "leads"); got != tt.want {
			t.Errorf("plural(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
