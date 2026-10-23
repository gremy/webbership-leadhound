package linkedin

import "testing"

func TestDescriptionText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"<p>One &amp; two.</p><p>Three.</p>", "One & two.\nThree."},
		{"<strong>Role</strong><br>You will <em>build</em> services.", "Role\nYou will build services."},
		{"<ul><li>Go</li><li>PostgreSQL</li></ul>", "- Go\n- PostgreSQL"},
		{"<p>  spaced\n\n  out </p><script>x()</script>", "spaced out"},
	}
	for _, tt := range tests {
		if got := descriptionText(tt.in); got != tt.want {
			t.Errorf("descriptionText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
