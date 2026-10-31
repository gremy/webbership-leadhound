package console

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"testing"
)

// TestBoardOnlyInDev: outside dev /board/ is an unknown path, a plain 404 and no
// sign-in redirect.
func TestBoardOnlyInDev(t *testing.T) {
	h := newHarness(t, &fakeBackend{}, false)
	for _, p := range []string{"/board/", "/board/index.html", "/board/board.css", "/board"} {
		w := h.do(get(p))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s outside dev: %d, want 404 (Location %q)", p, w.Code, w.Header().Get("Location"))
		}
	}
}

// TestBoardServesEveryFile: in dev every embedded page and board.css is served with
// its content type and the security headers, and /board redirects to /board/.
func TestBoardServesEveryFile(t *testing.T) {
	h := newHarness(t, &fakeBackend{}, true)
	names, err := fs.Glob(boardFS, "board/*")
	if err != nil || len(names) == 0 {
		t.Fatalf("board files: %v %v", names, err)
	}
	paths := []string{"/board/"}
	for _, n := range names {
		paths = append(paths, "/"+n)
	}
	for _, p := range paths {
		w := h.do(get(p))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", p, w.Code)
			continue
		}
		want := "text/html"
		switch path.Ext(p) {
		case ".css":
			want = "text/css"
		case ".html", "":
		default:
			continue
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Errorf("GET %s: Content-Type %q, want %s", p, ct, want)
		}
		if w.Header().Get("Content-Security-Policy") != contentSecurityPolicy {
			t.Errorf("GET %s: CSP missing", p)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s: X-Frame-Options missing", p)
		}
	}
	if w := h.do(get("/board")); (w.Code != http.StatusMovedPermanently && w.Code != http.StatusTemporaryRedirect) || w.Header().Get("Location") != "/board/" {
		t.Errorf("GET /board: %d to %q, want a redirect to /board/", w.Code, w.Header().Get("Location"))
	}
	for _, p := range []string{"/board/nope.html", "/board/../server.go", "/board/x/index.html"} {
		if w := h.do(get(p)); w.Code == http.StatusOK {
			t.Errorf("GET %s: 200, want refused", p)
		}
	}
}
