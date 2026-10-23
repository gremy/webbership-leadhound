package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every blocked sample below is assembled at runtime so this file itself passes
// `privacycheck tree`. Allowed samples use reserved names only (RFC 2606, RFC 5737).

func blockedEmail() string    { return "someone" + "@" + "corp-mail" + ".com" }
func blockedIP() string       { return "93.184" + ".216.34" }
func blockedProfile() string  { return "https://www." + "linkedin.com/" + "in/" + "someone-1" }
func recruiterClass() string  { return "message-the-" + "recruiter" }
func fictionalName() string   { return "Ada " + "Placeholder" }
func allowlistedAddr() string { return "builder" + "@" + "corp-mail" + ".com" }

func TestScanLineRules(t *testing.T) {
	c := newChecker([]string{fictionalName(), "Placeholder"}, map[string]bool{allowlistedAddr(): true})
	cases := []struct {
		name string
		path string
		line string
		want string // rule expected; "" means no finding
	}{
		{"denylist blocks, any case", "a.txt", "hi " + strings.ToLower(fictionalName()) + " there", ruleDenylist},
		{"denylist lets the public module path through", "go.mod", "module github.com/gremy/webbership-leadhound", ""},
		{"email blocks a real-looking address", "a.txt", "write to " + blockedEmail(), ruleEmail},
		{"email allowlist does not exempt content", "a.txt", "author " + allowlistedAddr(), ruleEmail},
		{"email allows reserved TLD", "a.txt", "user@mail.example", ""},
		{"email allows reserved domain", "a.txt", "ops@example.org and ops@sub.example.com", ""},
		{"email allows noreply trailer", "a.txt", "Co-Authored-By: Bot <noreply@anthropic.com>", ""},
		{"email allows GitHub noreply", "a.txt", "1+bot@users.noreply.github.com", ""},
		{"email allows the ssh remote form", "a.txt", "git@github.com:gremy/webbership-leadhound.git", ""},
		{"email ignores retina asset names", "a.txt", "logo@2x.png", ""},
		{"email ignores module versions", "a.txt", "golang.org/x/net@v0.61.0", ""},
		{"linkedin profile blocks", "a.txt", "see " + blockedProfile(), ruleProfile},
		{"linkedin jobs URL passes", "a.txt", "https://www.linkedin.com/jobs/view/123", ""},
		{"ipv4 blocks a public address", "a.txt", "host=" + blockedIP() + ":22", ruleIPv4},
		{"ipv4 allows documentation range", "a.txt", "host=192.0.2.10", ""},
		{"ipv4 allows loopback", "a.txt", "127.0.0.1:5432", ""},
		{"ipv4 ignores v-prefixed versions", "a.txt", "v1.2.3.4", ""},
		{"ipv4 ignores five-part numbers", "a.txt", "1.2.3.4.5", ""},
		{"ipv4 ignores out-of-range octets", "a.txt", "999.1.1.1", ""},
		{"recruiter class blocks under testdata", "internal/x/testdata/p.html", `<div class="` + recruiterClass() + `">`, ruleRecruiter},
		{"recruiter class outside testdata passes", "internal/x/parse.go", `sel := "div.` + recruiterClass() + `"`, ""},
		{"testdata without the class passes", "internal/x/testdata/p.html", `<div class="job-title">`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.scanLine(tc.path, tc.line)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no finding, got %v", got)
				}
				return
			}
			found := false
			for _, r := range got {
				if r == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("want rule %q, got %v", tc.want, got)
			}
		})
	}
}

// gitEnv isolates every git call (ours and the code's) from the machine's config.
func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_DATE", "")
}

func gitRun(t *testing.T, dir string, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- test helper, fixed binary
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a temp repo whose author is builder@leadhound.test, plus a private
// list dir holding the fictional denylist and an allow.txt naming that author.
func newRepo(t *testing.T) (repo, denylist string) {
	t.Helper()
	gitEnv(t)
	repo = t.TempDir()
	lists := t.TempDir()
	denylist = filepath.Join(lists, "denylist.txt")
	writeFile(t, denylist, "# fictional\n\n"+fictionalName()+"\n")
	writeFile(t, filepath.Join(lists, "allow.txt"), "builder@leadhound.test\n")
	gitRun(t, repo, nil, "init", "-q", "-b", "main")
	gitRun(t, repo, nil, "config", "user.name", "Builder")
	gitRun(t, repo, nil, "config", "user.email", "builder@leadhound.test")
	gitRun(t, repo, nil, "config", "commit.gpgsign", "false")
	return repo, denylist
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envWith(denylist string) func(string) string {
	return func(k string) string {
		if k == envDenylist {
			return denylist
		}
		return ""
	}
}

func runIn(t *testing.T, dir string, getenv func(string) string, args ...string) (int, string) {
	t.Helper()
	t.Chdir(dir)
	var out, errb bytes.Buffer
	code := run(args, getenv, &out, &errb)
	return code, out.String() + errb.String()
}

func assertNoEcho(t *testing.T, out string, secrets ...string) {
	t.Helper()
	lo := strings.ToLower(out)
	for _, s := range secrets {
		if strings.Contains(lo, strings.ToLower(s)) {
			t.Fatalf("output echoes matched text:\n%s", out)
		}
	}
}

func TestFilesModeReportsWithoutEcho(t *testing.T) {
	_, deny := newRepo(t)
	dir := t.TempDir()
	body := "ok line\n" + fictionalName() + "\n" + blockedEmail() + "\n" + blockedIP() + "\n" + blockedProfile() + "\n"
	writeFile(t, filepath.Join(dir, "testdata", "page.html"), body+`<div class="`+recruiterClass()+`">`+"\n")
	writeFile(t, filepath.Join(dir, "clean.txt"), "user@mail.example 192.0.2.10\n")
	code, out := runIn(t, dir, envWith(deny), "files", "testdata/page.html", "clean.txt")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	for _, want := range []string{
		"testdata/page.html:2: " + ruleDenylist,
		"testdata/page.html:3: " + ruleEmail,
		"testdata/page.html:4: " + ruleIPv4,
		"testdata/page.html:5: " + ruleProfile,
		"testdata/page.html:6: " + ruleRecruiter,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "clean.txt") {
		t.Errorf("clean file reported:\n%s", out)
	}
	assertNoEcho(t, out, fictionalName(), blockedEmail(), blockedIP(), "someone-1")
}

func TestFileNameHitIsHidden(t *testing.T) {
	_, deny := newRepo(t)
	dir := t.TempDir()
	name := fictionalName() + ".txt"
	writeFile(t, filepath.Join(dir, name), "fine\n")
	code, out := runIn(t, dir, envWith(deny), "files", name)
	if code != 1 || !strings.Contains(out, hiddenName+":0: "+ruleDenylist) {
		t.Fatalf("exit %d, want hidden-name finding:\n%s", code, out)
	}
	assertNoEcho(t, out, "Placeholder")
}

func TestDenylistResolution(t *testing.T) {
	repo, deny := newRepo(t)

	t.Run("none runs generic checks only", func(t *testing.T) {
		writeFile(t, filepath.Join(repo, "a.txt"), fictionalName()+"\n"+blockedEmail()+"\n")
		code, out := runIn(t, repo, envWith("none"), "files", "a.txt")
		if code != 1 || strings.Contains(out, ruleDenylist) || !strings.Contains(out, "a.txt:2: "+ruleEmail) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("missing env and config fails closed with the hint", func(t *testing.T) {
		code, out := runIn(t, repo, envWith(""), "files", "a.txt")
		if code != 1 || !strings.Contains(out, "git config leadhound.denylist") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("missing file fails closed", func(t *testing.T) {
		code, out := runIn(t, repo, envWith(filepath.Join(t.TempDir(), "nope.txt")), "files", "a.txt")
		if code != 1 || !strings.Contains(out, "git config leadhound.denylist") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("repo-local git config is read", func(t *testing.T) {
		gitRun(t, repo, nil, "config", "leadhound.denylist", deny)
		code, out := runIn(t, repo, envWith(""), "files", "a.txt")
		if code != 1 || !strings.Contains(out, "a.txt:1: "+ruleDenylist) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("usage error exits 2", func(t *testing.T) {
		code, _ := runIn(t, repo, envWith(deny), "bogus")
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})
}

func TestStaged(t *testing.T) {
	repo, deny := newRepo(t)
	writeFile(t, filepath.Join(repo, "keep.txt"), "line one\n")
	gitRun(t, repo, nil, "add", "keep.txt")
	gitRun(t, repo, nil, "commit", "-q", "-m", "init")

	code, out := runIn(t, repo, envWith(deny), "staged")
	if code != 0 {
		t.Fatalf("nothing staged: exit %d\n%s", code, out)
	}

	writeFile(t, filepath.Join(repo, "keep.txt"), "line one\n"+fictionalName()+"\n")
	writeFile(t, filepath.Join(repo, "new.txt"), "fine\n"+blockedEmail()+"\n")
	gitRun(t, repo, nil, "add", "keep.txt", "new.txt")
	code, out = runIn(t, repo, envWith(deny), "staged")
	if code != 1 || !strings.Contains(out, "keep.txt:2: "+ruleDenylist) || !strings.Contains(out, "new.txt:2: "+ruleEmail) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertNoEcho(t, out, fictionalName(), blockedEmail())

	// A -diff attribute makes git call a text file binary; it is still read.
	gitRun(t, repo, nil, "reset", "-q")
	writeFile(t, filepath.Join(repo, ".gitattributes"), "*.dat -diff\n")
	writeFile(t, filepath.Join(repo, "hidden.dat"), "x\n"+fictionalName()+"\n")
	gitRun(t, repo, nil, "add", ".gitattributes", "hidden.dat")
	code, out = runIn(t, repo, envWith(deny), "staged")
	if code != 1 || !strings.Contains(out, "hidden.dat:2: "+ruleDenylist) {
		t.Fatalf("-diff file: exit %d:\n%s", code, out)
	}

	// A clean change passes; the already-committed first line is not re-reported.
	gitRun(t, repo, nil, "reset", "-q")
	writeFile(t, filepath.Join(repo, "keep.txt"), "line one\nline two\n")
	gitRun(t, repo, nil, "add", "keep.txt")
	if code, out = runIn(t, repo, envWith(deny), "staged"); code != 0 {
		t.Fatalf("clean change: exit %d\n%s", code, out)
	}
}

func TestMsg(t *testing.T) {
	_, deny := newRepo(t)
	dir := t.TempDir()
	msg := filepath.Join(dir, "COMMIT_EDITMSG")
	writeFile(t, msg, "feat: add parser\n\nCo-Authored-By: Bot <noreply@anthropic.com>\n# a comment line is scanned too\n")
	if code, out := runIn(t, dir, envWith(deny), "msg", msg); code != 0 {
		t.Fatalf("clean message: exit %d\n%s", code, out)
	}
	writeFile(t, msg, "fix: thanks to "+fictionalName()+"\n")
	code, out := runIn(t, dir, envWith(deny), "msg", msg)
	if code != 1 || !strings.Contains(out, "commit message:1: "+ruleDenylist) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertNoEcho(t, out, fictionalName())
}

func TestTreeScansTracked(t *testing.T) {
	repo, deny := newRepo(t)
	writeFile(t, filepath.Join(repo, "blob.bin"), "\x00\x01 clean binary")
	writeFile(t, filepath.Join(repo, "ok.txt"), "user@mail.example\n")
	writeFile(t, filepath.Join(repo, "untracked.txt"), fictionalName()+"\n")
	gitRun(t, repo, nil, "add", "blob.bin", "ok.txt")
	if code, out := runIn(t, repo, envWith(deny), "tree"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	writeFile(t, filepath.Join(repo, "ok.txt"), "x\n"+blockedIP()+"\n")
	code, out := runIn(t, repo, envWith(deny), "tree")
	if code != 1 || !strings.Contains(out, "ok.txt:2: "+ruleIPv4) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertNoEcho(t, out, blockedIP())
}

func TestHistory(t *testing.T) {
	repo, deny := newRepo(t)
	writeFile(t, filepath.Join(repo, "notes.txt"), "a\n"+fictionalName()+"\n")
	gitRun(t, repo, nil, "add", "notes.txt")
	gitRun(t, repo, nil, "commit", "-q", "-m", "add notes")
	bad := gitRun(t, repo, nil, "rev-parse", "--short=7", "HEAD")
	gitRun(t, repo, nil, "rm", "-q", "notes.txt")
	gitRun(t, repo, nil, "commit", "-q", "-m", "remove notes")

	if code, out := runIn(t, repo, envWith(deny), "tree"); code != 0 {
		t.Fatalf("tree should be clean now: exit %d\n%s", code, out)
	}
	code, out := runIn(t, repo, envWith(deny), "history")
	if code != 1 || !strings.Contains(out, "commit "+bad+" notes.txt:2: "+ruleDenylist) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "email") {
		t.Fatalf("allowlisted author flagged:\n%s", out)
	}
	assertNoEcho(t, out, fictionalName())

	t.Run("non-allowlisted author email", func(t *testing.T) {
		writeFile(t, filepath.Join(repo, "b.txt"), "b\n")
		gitRun(t, repo, nil, "add", "b.txt")
		gitRun(t, repo, []string{"GIT_AUTHOR_EMAIL=stranger@leadhound.test"}, "commit", "-q", "-m", "b")
		sha := gitRun(t, repo, nil, "rev-parse", "--short=7", "HEAD")
		code, out := runIn(t, repo, envWith(deny), "history")
		if code != 1 || !strings.Contains(out, "commit "+sha+" author-email: "+ruleNotAllowlisted) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if strings.Contains(out, "commit "+sha+" committer-email") {
			t.Fatalf("allowlisted committer flagged:\n%s", out)
		}
		assertNoEcho(t, out, "stranger")
	})

	t.Run("bad commit message", func(t *testing.T) {
		gitRun(t, repo, nil, "commit", "-q", "--allow-empty", "-m", "chore: ping "+blockedEmail())
		sha := gitRun(t, repo, nil, "rev-parse", "--short=7", "HEAD")
		_, out := runIn(t, repo, envWith(deny), "history")
		if !strings.Contains(out, "commit "+sha+" message:1: "+ruleEmail) {
			t.Fatalf("missing message finding:\n%s", out)
		}
		assertNoEcho(t, out, blockedEmail())
	})

	t.Run("none keeps the author check with an empty allowlist", func(t *testing.T) {
		_, out := runIn(t, repo, envWith("none"), "history")
		if !strings.Contains(out, "author-email: "+ruleNotAllowlisted) {
			t.Fatalf("author check skipped under none:\n%s", out)
		}
	})
}
