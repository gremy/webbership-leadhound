package console

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// boardOnly are the board's own classes (its bar, contents page, swatches, proposals, the
// mockup's theme toggle): they never ship in console.css.
var boardOnly = regexp.MustCompile(`^(board-|screen-list|swatch|sw-|proposal|mock$|theme-toggle|label-(light|dark)$|icon-(moon|sun)$)`)

// TestCSSHasBoardComponents: every class the approved board styles has a rule in
// console.css, so a restyled page can use the board's class names as they are.
func TestCSSHasBoardComponents(t *testing.T) {
	board, err := os.ReadFile("board/board.css")
	if err != nil {
		t.Fatal(err)
	}
	css, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	class := regexp.MustCompile(`\.([a-zA-Z][\w-]*)`)
	have := map[string]bool{}
	for _, m := range class.FindAllStringSubmatch(string(css), -1) {
		have[m[1]] = true
	}
	for _, m := range class.FindAllStringSubmatch(string(board), -1) {
		if name := m[1]; !boardOnly.MatchString(name) && !have[name] {
			t.Errorf("console.css has no rule for the board's .%s", name)
			have[name] = true
		}
	}
}

// TestCSSBoardScoping: the board's rules for names the older pages used with another meaning
// reach only a .page, never the shell around it. The older pages' rules are gone: every page's
// content sits in a .page, so a rule that stopped at .page would reach nothing a page shows.
func TestCSSBoardScoping(t *testing.T) {
	b, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, name := range []string{"card", "grid", "page-head", "empty", "stats", "hint", "filters", "stack", "split", "pager"} {
		if !strings.Contains(css, "\n:where(.page) ."+name+" ") {
			t.Errorf("the board's .%s is not scoped to .page", name)
		}
	}
	if strings.Contains(css, ":where(:not(.page *))") {
		t.Error("an older rule that stops at .page is back; every page is in a .page now")
	}
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `{{define "content"}}`) && !strings.Contains(string(raw), `<div class="page">`) {
			t.Errorf("%s has a page that is not in a .page", e.Name())
		}
	}
	if !regexp.MustCompile(`\.table-wrap \{\s*position: relative;`).MatchString(css) {
		t.Error(".table-wrap lost position: relative (it keeps visually-hidden labels in the scroll box)")
	}
}
