package mailbox

import (
	"bufio"
	"bytes"
	"mime"
	"net/textproto"
	"strings"
)

// maxPartDepth is how deep firstTextPart descends into nested multiparts (FO4).
const maxPartDepth = 3

// firstTextPart (FO4) returns the first text of a message as its sender wrote it. The peek
// of a multipart message (BODY[TEXT]) is raw MIME: boundary lines, part headers and each
// part's own transfer encoding (Gmail sends non-ASCII text as base64). So for a top-level
// multipart/* with a boundary it finds the first --boundary line in the peek, reads that
// part's headers, descends into a nested multipart (at most maxPartDepth deep) and returns
// the first text/plain or text/html part decoded by its own Content-Transfer-Encoding, with
// html true for text/html. The peek is cut at 600 bytes: a truncated part decodes as far as
// it goes, and a part header block that never ends gives an empty body. No boundary line in
// the peek gives the text as it came. Any other message is decoded by its top-level cte.
func firstTextPart(text []byte, contentType, cte string) (body []byte, html bool) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt, params = "text/plain", nil
	}
	if strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" {
		body, html, found := firstPartOf(text, params["boundary"], 1)
		if !found {
			return text, false
		}
		return body, html
	}
	return decodePart(text, cte), mt == "text/html"
}

// firstPartOf reads the parts of one multipart body delimited by boundary. found is false
// when no --boundary line is in text at all; once one is, the answer is the first text
// part's decoded body, or empty when none is reached inside the peek.
func firstPartOf(text []byte, boundary string, depth int) (body []byte, html, found bool) {
	delim := []byte("--" + boundary)
	pos := boundaryLine(text, delim, 0)
	if pos < 0 {
		return nil, false, false
	}
	for pos >= 0 {
		rest := text[pos:]
		hdrEnd, bodyAt := headerEnd(rest)
		if hdrEnd < 0 {
			return nil, false, true // the peek ends inside the part's header block
		}
		hdr, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(append([]byte{}, rest[:hdrEnd]...), "\r\n\r\n"...)))).ReadMIMEHeader()
		if err != nil {
			hdr = textproto.MIMEHeader{}
		}
		partBody := rest[bodyAt:]
		next := -1
		if i := bytes.Index(partBody, append([]byte("\n"), delim...)); i >= 0 {
			next = bodyAt + i + 1
			partBody = partBody[:i]
		}
		mt, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
		if err != nil {
			mt, params = "text/plain", nil // RFC 2046 §5.1: the default inside a multipart
		}
		attachment := strings.HasPrefix(strings.ToLower(strings.TrimSpace(hdr.Get("Content-Disposition"))), "attachment")
		switch {
		case strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" && depth < maxPartDepth:
			if b, h, ok := firstPartOf(partBody, params["boundary"], depth+1); ok && len(b) > 0 {
				return b, h, true
			}
		case (mt == "text/plain" || mt == "text/html") && !attachment:
			return decodePart(partBody, hdr.Get("Content-Transfer-Encoding")), mt == "text/html", true
		}
		if next < 0 {
			return nil, false, true
		}
		pos = boundaryLine(text, delim, next)
	}
	return nil, false, true
}

// boundaryLine returns the offset just after the first line at or after from that is the
// delimiter (trailing spaces allowed, RFC 2046 transport padding), or -1. A close delimiter
// (--boundary--) ends the parts.
func boundaryLine(text, delim []byte, from int) int {
	for i := from; i < len(text); {
		end := bytes.IndexByte(text[i:], '\n')
		line := text[i:]
		next := len(text)
		if end >= 0 {
			line, next = text[i:i+end], i+end+1
		}
		line = bytes.TrimRight(line, " \t\r")
		if bytes.Equal(line, delim) {
			if end < 0 {
				return -1 // the peek ends on the delimiter line itself
			}
			return next
		}
		if bytes.HasPrefix(line, delim) && bytes.Equal(line[len(delim):], []byte("--")) {
			return -1
		}
		i = next
	}
	return -1
}

// headerEnd finds the blank line ending a part's header block: hdrEnd is where the header
// lines stop and bodyAt where the body starts; hdrEnd is -1 when the block never ends.
func headerEnd(b []byte) (hdrEnd, bodyAt int) {
	if bytes.HasPrefix(b, []byte("\r\n")) {
		return 0, 2
	}
	if bytes.HasPrefix(b, []byte("\n")) {
		return 0, 1
	}
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	lf := bytes.Index(b, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lf < 0 || crlf <= lf):
		return crlf, crlf + 4
	case lf >= 0:
		return lf, lf + 2
	}
	return -1, -1
}
