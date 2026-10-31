package console

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// boardFS holds the static mockup board: board.css and one HTML page per screen.
// New mounts it at /board/ only when Options.Dev is set; in production /board/
// is an unknown path and gets the mux's plain 404.
//
//go:embed board
var boardFS embed.FS

// board serves GET /board/ (index.html) and GET /board/<file> from boardFS. A
// name with a slash is refused, so there is no listing and no way out of the
// folder. /board without the slash is redirected to /board/ by the mux.
func board(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/board/")
	if name == "" {
		name = "index.html"
	}
	b, err := fs.ReadFile(boardFS, "board/"+name)
	if strings.Contains(name, "/") || err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}
