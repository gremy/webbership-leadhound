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

// TestCSSBoardScoping: the board's rules for names the older pages use with another
// meaning reach only a .page, and the older rules stop at .page, so no current page
// changes before it is restyled.
func TestCSSBoardScoping(t *testing.T) {
	b, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	const old = ":where(:not(.page *))"
	for _, name := range []string{"card", "grid", "page-head", "empty", "stats", "hint", "filters", "stack", "split", "pager"} {
		if !strings.Contains(css, "\n:where(.page) ."+name+" ") {
			t.Errorf("the board's .%s is not scoped to .page", name)
		}
		if !strings.Contains(css, "."+name+old) {
			t.Errorf("the older .%s does not stop at .page", name)
		}
	}
	for _, el := range []string{"h1", "h2", "h3", "p", "code", "button", "summary", "label", "input", "select"} {
		if !strings.Contains(css, "\n"+el+old) {
			t.Errorf("the older %s rule does not stop at .page", el)
		}
	}
	if !regexp.MustCompile(`\.table-wrap \{\s*position: relative;`).MatchString(css) {
		t.Error(".table-wrap lost position: relative (it keeps visually-hidden labels in the scroll box)")
	}
}
