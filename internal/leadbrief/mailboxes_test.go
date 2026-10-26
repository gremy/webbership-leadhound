package leadbrief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mailboxCases are the golden inputs of the mailbox line, one per shape. The integration
// test reads the same golden files for the sections it loads.
var mailboxCases = map[string]MailboxHealth{
	"mailboxes_none":        {},
	"mailboxes_mixed":       {Ready: 2, Paused: 1, PausedWhy: []string{"2 hard bounces"}, NeedsCheck: 1},
	"mailboxes_key_missing": {Ready: 2, KeyMissing: true},
	"mailboxes_ready":       {Ready: 1},
	"mailboxes_none_ready":  {Paused: 2, PausedWhy: []string{"blocked by a receiving server", "by the operator"}, NeedsCheck: 2},
}

func TestMailboxLineGolden(t *testing.T) {
	for name, h := range mailboxCases {
		t.Run(name, func(t *testing.T) {
			got := h.Line() + "\n"
			want, err := os.ReadFile(filepath.Join("testdata", name+".golden")) //nolint:gosec // a fixed test path
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s = %q, want the golden %q", name, got, want)
			}
			if strings.Contains(got, "@") {
				t.Errorf("%s carries an address: %q", name, got)
			}
		})
	}
}

// TestPausedWhy: the system's reasons map to fixed words; the operator's own text never
// reaches the brief.
func TestPausedWhy(t *testing.T) {
	for _, c := range []struct{ scope, reason, want string }{
		{"mailbox", "2 hard bounces in its last 50 live sends", "2 hard bounces"},
		{"domain", "a receiving server blocked mail from alpha.example (5.7.1)", "blocked by a receiving server"},
		{"mailbox", "re-enter the password: it could not be opened", "password to re-enter"},
		{"domain", "Ilinca" + " asked to wait", "its domain paused by the operator"},
		{"mailbox", "Ilinca" + " asked to wait", "by the operator"},
		{"mailbox", "2 hard bounces in its last 50 live sends, said " + "Ilinca", "by the operator"},
	} {
		if got := pausedWhy(c.scope, c.reason); got != c.want {
			t.Errorf("pausedWhy(%q, %q) = %q, want %q", c.scope, c.reason, got, c.want)
		}
	}
}
