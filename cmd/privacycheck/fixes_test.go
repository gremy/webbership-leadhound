package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// Tests for the review fixes. Every blocked sample is assembled at runtime and every
// name is fictional, so this file passes `privacycheck tree` itself.

func publicDNS() string { return "8.8" + ".8.8" }

func hasRule(rules []string, want string) bool {
	for _, r := range rules {
		if r == want {
			return true
		}
	}
	return false
}

func TestNormalizedDenylistForms(t *testing.T) {
	c := newChecker([]string{fictionalName()}, nil)
	for _, form := range []string{
		"Ada.Placeholder",
		"ada-placeholder",
		"ADA  PLACEHOLDER",
		"ada_placeholder",
		"ada \t._- placeholder",
		"Ădâ Plăcehőlder",             // precomposed diacritics on the same letters
		"Ada\u0306 Pla\u0302ceholder", // decomposed (NFD) marks
	} {
		if !c.denyHit("x " + form + " y") {
			t.Errorf("form %q not matched", form)
		}
	}
	if c.denyHit("x adaplaceholder y") {
		t.Error("glued form must not match a spaced entry")
	}
	// The entry side is normalised too.
	c2 := newChecker([]string{"Adă.Pláceholder"}, nil)
	if !c2.denyHit("hello " + fictionalName()) {
		t.Error("diacritic entry did not match plain content")
	}
}

func TestNormalizeFoldsRomanian(t *testing.T) {
	got := normalize("ș ş ț ţ ă â î Ș Ţ Ă Î")
	if want := "s s t t a a i s t a i"; got != want {
		t.Fatalf("normalize = %q, want %q", got, want)
	}
}

func TestIPv4VersionContexts(t *testing.T) {
	c := newChecker(nil, nil)
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" +
		"124.0" + ".0.0" + " Safari/537.36"
	pass := []string{
		ua,
		"Safari/537.36",
		"Edg/" + "124.0" + ".2478.51",
		"version " + publicDNS(),
		"Version " + publicDNS(),
		"v" + publicDNS(),
	}
	for _, s := range pass {
		if hasRule(c.scanLine("a.txt", s), ruleIPv4) {
			t.Errorf("version string flagged as ipv4: %q", s)
		}
	}
	block := []string{
		publicDNS(),
		"dns " + publicDNS(),
		"http://" + publicDNS() + "/x",
		"https://" + publicDNS(),
	}
	for _, s := range block {
		if !hasRule(c.scanLine("a.txt", s), ruleIPv4) {
			t.Errorf("public address not flagged in %q", s)
		}
	}
}

func TestMsgScansEveryLine(t *testing.T) {
	_, deny := newRepo(t)
	dir := t.TempDir()
	msg := filepath.Join(dir, "COMMIT_EDITMSG")
	writeFile(t, msg, "feat: x\n# "+blockedEmail()+"\n")
	code, out := runIn(t, dir, envWith(deny), "msg", msg)
	if code != 1 || !strings.Contains(out, "commit message:2: "+ruleEmail) {
		t.Fatalf("comment line not scanned: exit %d\n%s", code, out)
	}
	writeFile(t, msg, "feat: x\n# ------------------------ >8 ------------------------\nbelow\n"+fictionalName()+"\n")
	code, out = runIn(t, dir, envWith(deny), "msg", msg)
	if code != 1 || !strings.Contains(out, "commit message:4: "+ruleDenylist) {
		t.Fatalf("scissors tail not scanned: exit %d\n%s", code, out)
	}
	assertNoEcho(t, out, fictionalName(), blockedEmail())
}

func utf16LE(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func utf16BE(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u>>8), byte(u))
	}
	return b
}

func TestBinaryContentIsScanned(t *testing.T) {
	repo, deny := newRepo(t)
	files := map[string]string{
		"blob.bin":    "\x00\x01\x02" + fictionalName() + "\x00\xff",
		"le.bin":      "\x00\x00" + string(utf16LE("mail "+blockedEmail())) + "\x00\x00",
		"be.bin":      "\x00\x01" + string(utf16BE(fictionalName())) + "\x00",
		"photo.png":   "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00Exif\x00Artist\x00" + fictionalName() + "\x00",
		"clean.png":   "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00IEND",
		"report.pdf":  "%PDF-1.4 plain text, still blocked by its extension\n",
		"dump.sql.gz": "x\n",
		"Letter.DOCX": "x\n",
		"card.vcf":    "x\n",
	}
	var names []string
	for name, body := range files {
		writeFile(t, filepath.Join(repo, name), body)
		names = append(names, name)
	}
	gitRun(t, repo, nil, append([]string{"add"}, names...)...)
	code, out := runIn(t, repo, envWith(deny), "tree")
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"blob.bin:binary: " + ruleDenylist,
		"le.bin:binary: " + ruleEmail,
		"be.bin:binary: " + ruleDenylist,
		"photo.png:binary: " + ruleDenylist,
		"report.pdf:0: " + ruleBinaryDocument,
		"dump.sql.gz:0: " + ruleBinaryDocument,
		"Letter.DOCX:0: " + ruleBinaryDocument,
		"card.vcf:0: " + ruleBinaryDocument,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "clean.png") {
		t.Errorf("clean image flagged:\n%s", out)
	}
	assertNoEcho(t, out, fictionalName(), blockedEmail())
}

func TestDiffPathsWithSpacesAndAnd(t *testing.T) {
	repo, deny := newRepo(t)
	writeFile(t, filepath.Join(repo, "seed.txt"), "seed\n")
	gitRun(t, repo, nil, "add", "seed.txt")
	gitRun(t, repo, nil, "commit", "-q", "-m", "seed")

	text := "testdata/a and b.txt"
	bin := "testdata/c and d.bin"
	writeFile(t, filepath.Join(repo, text), "ok\n<div class=\""+recruiterClass()+"\">\n")
	writeFile(t, filepath.Join(repo, bin), "\x00\x01<div class=\""+recruiterClass()+"\">\x00")
	gitRun(t, repo, nil, "add", text, bin)
	code, out := runIn(t, repo, envWith(deny), "staged")
	if code != 1 || !strings.Contains(out, text+":2: "+ruleRecruiter) || !strings.Contains(out, bin+":binary: "+ruleRecruiter) {
		t.Fatalf("staged: exit %d\n%s", code, out)
	}

	// A modified file with a space: only the added line is reported, at its own number.
	gitRun(t, repo, nil, "commit", "-q", "-m", "add pages")
	sha := gitRun(t, repo, nil, "rev-parse", "--short=7", "HEAD")
	writeFile(t, filepath.Join(repo, text), "ok\n<div class=\""+recruiterClass()+"\">\nmore\n"+recruiterClass()+"\n")
	gitRun(t, repo, nil, "add", text)
	_, out = runIn(t, repo, envWith(deny), "staged")
	if !strings.Contains(out, text+":4: "+ruleRecruiter) || strings.Contains(out, text+":2:") {
		t.Fatalf("modified file: want only line 4:\n%s", out)
	}
	gitRun(t, repo, nil, "reset", "-q")

	_, out = runIn(t, repo, envWith(deny), "history")
	for _, want := range []string{
		"commit " + sha + " " + text + ":2: " + ruleRecruiter,
		"commit " + sha + " " + bin + ":binary: " + ruleRecruiter,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("history missing %q in:\n%s", want, out)
		}
	}
}

func TestHistoryIdentityNames(t *testing.T) {
	repo, deny := newRepo(t)
	nameEnv := []string{"GIT_AUTHOR_NAME=" + fictionalName(), "GIT_COMMITTER_NAME=" + "ada.placeholder"}
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	gitRun(t, repo, nil, "add", "a.txt")
	gitRun(t, repo, nameEnv, "commit", "-q", "-m", "a")
	sha := gitRun(t, repo, nil, "rev-parse", "--short=7", "HEAD")

	code, out := runIn(t, repo, envWith(deny), "history")
	if code != 1 ||
		!strings.Contains(out, "commit "+sha+" author-name: "+ruleDenylist) ||
		!strings.Contains(out, "commit "+sha+" committer-name: "+ruleDenylist) {
		t.Fatalf("identity names not checked: exit %d\n%s", code, out)
	}
	assertNoEcho(t, out, fictionalName(), "ada.placeholder")

	t.Run("name: line exempts the identity only", func(t *testing.T) {
		allow := filepath.Join(filepath.Dir(deny), "allow.txt")
		writeFile(t, allow, "builder@leadhound.test\nname:ADA  PLACEHOLDER\n")
		// The same name in content and in a message stays blocked.
		writeFile(t, filepath.Join(repo, "b.txt"), fictionalName()+"\n")
		gitRun(t, repo, nil, "add", "b.txt")
		gitRun(t, repo, nameEnv, "commit", "-q", "-m", "thanks "+fictionalName())
		sha2 := gitRun(t, repo, nil, "rev-parse", "--short=7", "HEAD")
		_, out := runIn(t, repo, envWith(deny), "history")
		if strings.Contains(out, "-name:") {
			t.Fatalf("allowlisted name still flagged:\n%s", out)
		}
		for _, want := range []string{
			"commit " + sha2 + " b.txt:1: " + ruleDenylist,
			"commit " + sha2 + " message:1: " + ruleDenylist,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("content exempted by allow.txt; missing %q in:\n%s", want, out)
			}
		}
	})

	t.Run("a name: line does not allowlist an email", func(t *testing.T) {
		c := newChecker(nil, map[string]bool{"name:" + fictionalName(): true})
		if len(c.allow) != 0 {
			t.Fatalf("name entry leaked into the email allowlist: %v", c.allow)
		}
	})
}

func TestHistoryTagsAndStash(t *testing.T) {
	repo, deny := newRepo(t)
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	gitRun(t, repo, nil, "add", "a.txt")
	gitRun(t, repo, nil, "commit", "-q", "-m", "a")
	if code, out := runIn(t, repo, envWith(deny), "history"); code != 0 {
		t.Fatalf("clean repo: exit %d\n%s", code, out)
	}

	// A stash is never pushed, so its content is not history.
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n"+fictionalName()+"\n")
	gitRun(t, repo, nil, "stash", "-q")
	if code, out := runIn(t, repo, envWith(deny), "history"); code != 0 {
		t.Fatalf("stash walked: exit %d\n%s", code, out)
	}

	tagEnv := []string{"GIT_COMMITTER_NAME=" + fictionalName(), "GIT_COMMITTER_EMAIL=stranger@leadhound.test"}
	gitRun(t, repo, tagEnv, "tag", "-a", "v0.1.0", "-m", "release\n\nping "+blockedEmail())
	tagSha := gitRun(t, repo, nil, "rev-parse", "--short=7", "v0.1.0")
	code, out := runIn(t, repo, envWith(deny), "history")
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"tag " + tagSha + " tagger-email: " + ruleNotAllowlisted,
		"tag " + tagSha + " tagger-name: " + ruleDenylist,
		"tag " + tagSha + " message:3: " + ruleEmail,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	assertNoEcho(t, out, fictionalName(), blockedEmail(), "stranger")
}

func TestErrorsNeverShowPaths(t *testing.T) {
	_, deny := newRepo(t)
	secretDir := filepath.Join(t.TempDir(), "job-hunt-private")
	missing := filepath.Join(secretDir, "denylist.txt")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	code, out := runIn(t, dir, envWith(missing), "files", "a.txt")
	if code != 1 || !strings.Contains(out, "configured denylist unreadable") || strings.Contains(out, "job-hunt-private") {
		t.Fatalf("exit %d:\n%s", code, out)
	}

	notRepo := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(notRepo))
	code, out = runIn(t, notRepo, envWith(deny), "history")
	if code != 1 || !strings.Contains(out, "git command failed (rev-list)") || strings.Contains(out, notRepo) || strings.Contains(out, filepath.Base(notRepo)) {
		t.Fatalf("exit %d:\n%s", code, out)
	}

	code, out = runIn(t, dir, envWith(deny), "msg", filepath.Join(secretDir, "MSG"))
	if code != 1 || strings.Contains(out, "job-hunt-private") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(secretDir); err == nil {
		t.Fatal("test setup: dir should not exist")
	}
}
