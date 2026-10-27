package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// TestClaudeCappedLiveOff: without LEADHOUND_LIVE=1, claudeCapped reads no key, builds
// nothing, prints one line and returns nil, nil (the command exits 0).
func TestClaudeCappedLiveOff(t *testing.T) {
	for _, v := range []string{"", "ats"} {
		t.Setenv("LEADHOUND_LIVE", v)
		t.Setenv(claude.KeyFileEnv, filepath.Join(t.TempDir(), "missing.key"))
		out := captureStdout(t)
		c, err := claudeCapped("leads drafts", store.Queries{}, claude.PurposeDrafting, 1, claudeCampaignCaps{})
		if c != nil || err != nil || !strings.Contains(out.String(), "leads drafts: live fetching is off here") {
			t.Errorf("LEADHOUND_LIVE=%q: %v, %v, stdout %q", v, c, err, out.String())
		}
	}
}
