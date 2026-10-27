package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGit answers the three questions the Makefile asks git about the gate's stamp from
// FAKE_GIT_HEAD and FAKE_GIT_STATUS, and refuses anything else.
const fakeGit = `#!/bin/sh
case "$*" in
"rev-parse --verify -q HEAD") printf '%s\n' "$FAKE_GIT_HEAD" ;;
"rev-parse --short HEAD") printf '%s\n' "$FAKE_GIT_HEAD" | cut -c1-7 ;;
"status --porcelain") printf '%s' "$FAKE_GIT_STATUS" ;;
*) echo "fake git: $*" >&2; exit 1 ;;
esac
`

// gateCommands are the gate's steps as make -n prints them, in the order they must run.
var gateCommands = []string{
	"gofmt -s -l .",                          // check
	"go test -tags integration -p 1 ./...",   // test-integration
	"-run 'Scale'",                           // test-scale
	"go vet -tags 'integration scale' ./...", // vet with both tags
	"go test ./deploy/ -race -count=1",       // the deploy tests
}

// TestDeployGateStamp: make deploy runs the gate (check, test-integration, test-scale, vet
// with the tags, the deploy tests, in that order) unless the stamp names HEAD and the tree is
// clean, untracked files included; then it says so in one line and runs none of it. Either
// way it builds and copies as before, and a dry run never writes the stamp.
func TestDeployGateStamp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake git is a shell script")
	}
	mk, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o700); err != nil { // #nosec G306 -- the test's own fake git, run by make
		t.Fatal(err)
	}
	const head = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, stamp, status string
		skip                bool
	}{
		{"stamp is HEAD, clean", head + "\n", "", true},
		{"stamp is another commit", "fedcba9876543210fedcba9876543210fedcba98\n", "", false},
		{"no stamp", "", "", false},
		{"stamp is HEAD, a file changed", head + "\n", " M internal/store/jobs.go\n", false},
		{"stamp is HEAD, an untracked file", head + "\n", "?? deploy/leadhound-new.service\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stamp := filepath.Join(t.TempDir(), "gate-passed")
			if tc.stamp != "" {
				if err := os.WriteFile(stamp, []byte(tc.stamp), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-n", "-C", "..", "deploy", "HOST=h.invalid", "CONFIRM=1", "GATE_STAMP=" + stamp}
			cmd := exec.Command(mk, args...) // #nosec G204 -- the test's own Makefile
			cmd.Env = append(os.Environ(), "MAKEFLAGS=", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_GIT_HEAD="+head, "FAKE_GIT_STATUS="+tc.status)
			b, err := cmd.CombinedOutput()
			out := string(b)
			if err != nil {
				t.Fatalf("make %q: %v\n%s", args, err, out)
			}
			skipLine := "deploy: the gate passed on " + head[:7] + "; not running it again"
			if got := strings.Contains(out, skipLine); got != tc.skip {
				t.Errorf("the skip line is printed = %t, want %t:\n%s", got, tc.skip, out)
			}
			at := 0
			for _, c := range gateCommands {
				i := strings.Index(out[at:], c)
				switch {
				case tc.skip && strings.Contains(out, c):
					t.Errorf("the gate's %q runs although it passed on HEAD:\n%s", c, out)
				case !tc.skip && i < 0:
					t.Errorf("the gate's %q does not run, or not in order:\n%s", c, out)
				case !tc.skip:
					at += i + len(c)
				}
			}
			build := strings.Index(out, "go build -trimpath")
			if build < 0 || build < at || !strings.Contains(out[build:], "deploy/install.sh") {
				t.Errorf("deploy does not build after the gate and then copy:\n%s", out)
			}
			got, err := os.ReadFile(stamp) // #nosec G304 -- the test's own temp file
			if tc.stamp == "" && err == nil || tc.stamp != "" && string(got) != tc.stamp {
				t.Errorf("a dry run changed the stamp to %q (err %v), want %q", got, err, tc.stamp)
			}
		})
	}

	// CONFIRM=1 still guards deploy, with the stamp matching or not.
	cmd := exec.Command(mk, "-n", "-C", "..", "deploy", "HOST=h.invalid") // #nosec G204 -- the test's own Makefile
	cmd.Env = append(os.Environ(), "MAKEFLAGS=", "CONFIRM=")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "add CONFIRM=1") {
		t.Errorf("make deploy without CONFIRM=1: err %v, want the CONFIRM refusal:\n%s", err, out)
	}
}
