package agencies

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLoadFileGood(t *testing.T) {
	got, skipped, problems := LoadFile(filepath.Join("testdata", "good.yaml"))
	if len(problems) != 0 {
		t.Fatalf("problems = %q, want none", problems)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the row without a website)", skipped)
	}
	if len(got) != 2 {
		t.Fatalf("got %d agencies, want 2: %+v", len(got), got)
	}
	first := got[0]
	if first.Name != "Northwind Digital" || first.Country != "GB" || first.DrupalOrgSlug != "northwind" ||
		first.EvidenceURL != "https://www.northwind.example/services/upgrades" {
		t.Errorf("first = %+v", first)
	}
	if first.Domain != "northwind.example" {
		t.Errorf("first.Domain = %q, want northwind.example", first.Domain)
	}
	if got[1].Domain != "contoso.example" {
		t.Errorf("second.Domain = %q, want contoso.example", got[1].Domain)
	}
	for _, a := range got {
		if a.Name == "Fabrikam Studio" {
			t.Errorf("a row without a website was returned: %+v", a)
		}
	}
}

func TestLoadFileUnknownKeyRefused(t *testing.T) {
	got, _, problems := LoadFile(filepath.Join("testdata", "unknown_key.yaml"))
	if len(got) != 0 {
		t.Errorf("got %d agencies from a refused file, want 0", len(got))
	}
	joined := strings.Join(problems, "\n")
	for _, key := range []string{"homepage", "phone"} {
		if !strings.Contains(joined, key) {
			t.Errorf("problems %q do not name the unknown key %q", problems, key)
		}
	}
}

func TestLoadFileEveryProblemAtOnce(t *testing.T) {
	got, _, problems := LoadFile(filepath.Join("testdata", "bad_rows.yaml"))
	if len(got) != 0 {
		t.Errorf("got %d agencies from a file with problems, want 0", len(got))
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{
		"Bad Scheme", "No Host", "Bad Evidence", "row 4: name is missing",
		"Bad Country", "Same Domain",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems do not mention %q:\n%s", want, joined)
		}
	}
	if len(problems) != 6 {
		t.Errorf("got %d problems, want 6:\n%s", len(problems), joined)
	}
}

func TestLoadFileMissingAndEmpty(t *testing.T) {
	if _, _, problems := LoadFile(filepath.Join("testdata", "absent.yaml")); len(problems) != 1 {
		t.Errorf("missing file: problems = %q, want one", problems)
	}
	empty := filepath.Join(t.TempDir(), "agencies.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, problems := LoadFile(empty); len(problems) != 1 {
		t.Errorf("empty file: problems = %q, want one", problems)
	}
}

var upgradeRe = regexp.MustCompile(`(?i)drupal 7 to drupal 1[01]`)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- test data
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFindingPhraseScriptOnlyNoMatch(t *testing.T) {
	if phrase, ok := FindingPhrase(readFixture(t, "script_only.html"), upgradeRe); ok {
		t.Errorf("matched %q inside <script>, want no match", phrase)
	}
	style := regexp.MustCompile(`upgrade-to-drupal`)
	if phrase, ok := FindingPhrase(readFixture(t, "script_only.html"), style); ok {
		t.Errorf("matched %q inside <style>, want no match", phrase)
	}
}

func TestFindingPhraseVisible(t *testing.T) {
	phrase, ok := FindingPhrase(readFixture(t, "visible.html"), upgradeRe)
	if !ok {
		t.Fatal("no match in visible text")
	}
	if n := utf8.RuneCountInString(phrase); n > 120 {
		t.Errorf("phrase is %d runes, want <= 120: %q", n, phrase)
	}
	if !strings.Contains(phrase, "Drupal 7 to Drupal 11") {
		t.Errorf("phrase %q does not contain the match", phrase)
	}
	if strings.Contains(phrase, "  ") || strings.ContainsAny(phrase, "\n\t") {
		t.Errorf("phrase %q keeps unfolded whitespace", phrase)
	}
	if !strings.Contains(phrase, "&") || strings.Contains(phrase, "&amp;") {
		t.Errorf("phrase %q should carry decoded entities", phrase)
	}
	if strings.Contains(phrase, "nothing here") {
		t.Errorf("phrase %q carries script text", phrase)
	}
}

func TestFindingPhraseShortPageKeepsAll(t *testing.T) {
	body := []byte("<p>We upgrade\n Drupal 7 to Drupal 10.</p>")
	phrase, ok := FindingPhrase(body, upgradeRe)
	if !ok || phrase != "We upgrade Drupal 7 to Drupal 10." {
		t.Errorf("FindingPhrase = %q, %v", phrase, ok)
	}
}

func TestFindingPhraseInvalidUTF8(t *testing.T) {
	body := []byte("<p>\xff\xfe broken bytes, then: we move Drupal 7 to Drupal 11 \xc3\x28 fast</p>")
	phrase, ok := FindingPhrase(body, upgradeRe)
	if !ok {
		t.Fatal("no match on a page with invalid UTF-8")
	}
	if !utf8.ValidString(phrase) {
		t.Errorf("phrase %q is not valid UTF-8", phrase)
	}
	if !strings.Contains(phrase, "Drupal 7 to Drupal 11") {
		t.Errorf("phrase %q does not contain the match", phrase)
	}
}

func TestFindingPhraseNoMatchAndNilPattern(t *testing.T) {
	if _, ok := FindingPhrase([]byte("<p>plain page</p>"), upgradeRe); ok {
		t.Error("matched a page without the words")
	}
	if _, ok := FindingPhrase([]byte("<p>Drupal 7 to Drupal 11</p>"), nil); ok {
		t.Error("matched with a nil pattern")
	}
	if _, ok := FindingPhrase([]byte("<p>anything</p>"), regexp.MustCompile(`x*`)); ok {
		t.Error("an empty match counted as a finding")
	}
}

func TestFindingPhraseLongMatchCapped(t *testing.T) {
	long := strings.Repeat("drupal ", 40)
	phrase, ok := FindingPhrase([]byte("<p>"+long+"</p>"), regexp.MustCompile(`(drupal )+`))
	if !ok {
		t.Fatal("no match")
	}
	if n := utf8.RuneCountInString(phrase); n > 120 {
		t.Errorf("phrase is %d runes, want <= 120", n)
	}
}

func TestIsDrupalOrg(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://drupal.org/acme":                true,
		"https://www.drupal.org/acme":            true,
		"https://new.drupal.org/drupal-services": true,
		"https://WWW.Drupal.ORG/x":               true,
		"https://northwind.example/":             false,
		"https://drupal.org.example/":            false,
		"not a url":                              false,
		"":                                       false,
	} {
		if got := IsDrupalOrg(raw); got != want {
			t.Errorf("IsDrupalOrg(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestLoadFileDrupalOrgWebsiteRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agencies.yaml")
	src := "- name: Northwind\n  website: https://www.drupal.org/northwind\n" +
		"- name: Contoso\n  website: https://new.drupal.org/contoso\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, problems := LoadFile(path)
	if len(got) != 0 || len(problems) != 2 {
		t.Fatalf("got %d agencies, problems %q; want 0 and 2", len(got), problems)
	}
	for _, p := range problems {
		if !strings.Contains(p, "website is a drupal.org page; use the organisation's own site") {
			t.Errorf("problem %q does not say why", p)
		}
	}
}
