package deploy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDeployDataChecksWithServerBinary: make deploy-data runs the server's own binary on the
// staged files, as the leadhound user, before anything is installed, and a refusal stops it
// there. The local check (go run) reads this checkout's source, so it passes a key the live
// binary refuses (29 Oct: cafe_own_vendor_hosts), and every leads command on the server then
// refuses the installed file.
func TestDeployDataChecksWithServerBinary(t *testing.T) {
	mk, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make")
	}
	cmd := exec.Command(mk, "-n", "-C", "..", "deploy-data", "HOST=h.invalid", "CONFIRM=1") // #nosec G204 -- the test's own Makefile
	cmd.Env = append(os.Environ(), "MAKEFLAGS=")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n deploy-data: %v\n%s", err, b)
	}
	out := string(b)

	const server = "/opt/leadhound/bin/leadhound"
	install := strings.Index(out, "install -o root -g leadhound")
	if install < 0 {
		t.Fatalf("make deploy-data installs nothing:\n%s", out)
	}
	for _, check := range []string{"leads check-config", "score --check-config"} {
		at := strings.Index(out, "LEADHOUND_DATA_DIR=$stage "+server+" "+check)
		switch {
		case at < 0:
			t.Errorf("make deploy-data does not run the server's %s %s on the staged files", server, check)
		case at > install:
			t.Errorf("make deploy-data runs the server's %s after install", check)
		default:
			// The run is the leadhound user's, through systemd-run as make remote runs it.
			start := max(strings.LastIndex(out[:at], "systemd-run"), 0)
			if !strings.Contains(out[start:at], "--uid=leadhound") {
				t.Errorf("the server's %s does not run as the leadhound user: %q", check, out[start:at])
			}
		}
	}
	// A refusal ends the recipe before the install step.
	last := max(strings.LastIndex(out[:install], "leads check-config"), 0)
	if !strings.Contains(out[last:install], "exit 1") {
		t.Errorf("a refused server check does not stop make deploy-data before install:\n%s", out[last:install])
	}
}
