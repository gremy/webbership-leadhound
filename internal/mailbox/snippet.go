package mailbox

import (
	"mime"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// MaxSnippetRunes is the longest snippet ReplySnippet returns, in characters; it is
// leadstore.MaxReplySnippet (the mailbox_event_snippet_check), restated here because
// leadstore imports this package.
const MaxSnippetRunes = 300

var (
	// htmlSignatureMark: where Gmail's HTML signature block starts (lowered text).
	htmlSignatureMark = `gmail_signature`
	// mobileFooterRe: the one-line footers phone and webmail clients add under a reply,
	// en / ro / de / fr.
	mobileFooterRe = regexp.MustCompile(`(?i)^\s*(Sent from (my|Mail for|Yahoo Mail|Outlook)\b.*|Get Outlook for .*|Trimis de pe .*|Trimis din .*|Von meinem .* gesendet.*|Gesendet von .*|Envoyé de mon .*|Envoyé depuis .*)$`)
	// cutAttributionRe: a last line the peek may have cut inside an attribution ("On Mon, 26
	// Oct 2026 at 18:10, Sam <"): it starts like one and holds a digit.
	cutAttributionRe = regexp.MustCompile(`(?i)^\s*(On|În|In|Am|Op|Le) .*\d`)
)

// ReplySnippet returns a reply's opening words as plain text, at most MaxSnippetRunes
// characters, for the replies inbox. h is the message's top-level header (its Content-Type,
// Content-Transfer-Encoding and charset are read), firstText the peeked text (at most
// FirstTextBytes, possibly cut mid-part), and ownLines our own lines, taken out as the
// opt-out rule does (FO3). The text is decoded as OptOutIn decodes it (FO4: the first text
// part of a multipart, by its own transfer encoding); a single-part message in a declared
// legacy charset is converted to UTF-8, and any byte still not UTF-8 is dropped. Then:
//   - HTML is cut at its quote (FO2.1) or Gmail's signature block and its tags stripped;
//   - everything from the first attribution line (FO2.3, a wrapped pair included) or the
//     first unprefixed quote header (FO2.4) goes, and so does a last line the peek cut
//     inside an attribution;
//   - lines starting with ">" go (FO2.2);
//   - everything from a signature separator ("-- ", or "--" once the space is lost) or a
//     mobile footer ("Sent from my iPhone", "Trimis de pe …") goes;
//   - whitespace collapses to single spaces, trimmed, and a longer text is cut on a word
//     boundary where one is near the end, with "…" inside the limit.
//
// It returns "" when nothing is left. It never panics on any input.
func ReplySnippet(h Header, firstText []byte, ownLines []string) string {
	ctype, cte := "", ""
	if h.Fields != nil {
		ctype, cte = h.Fields.Get("Content-Type"), h.Fields.Get("Content-Transfer-Encoding")
	}
	body, html := firstTextPart(firstText, ctype, cte)
	if len(strings.TrimSpace(string(body))) == 0 {
		body, html = firstText, false
	}
	s := toUTF8(body, ctype)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if html || htmlStartRe.MatchString(s) {
		s = htmlOwnText(s)
	}

	lines := strings.Split(s, "\n")
	lines = cutAtAttribution(lines, len(firstText) >= FirstTextBytes)
	lines, _ = cutQuoteHeader(lines)
	lines = dropQuotedLines(lines)
	lines = cutAtSignature(lines)
	return clipSnippet(collapseSpace(removeOwnLines(strings.Join(lines, "\n"), ownLines)))
}

// toUTF8 returns body as valid UTF-8: as it is when it already is; converted from the
// top-level charset when that names a known one other than UTF-8 (a multipart's part charset is not
// read, so its stray bytes are dropped); else with the invalid bytes dropped.
func toUTF8(body []byte, ctype string) string {
	if utf8.Valid(body) {
		return string(body)
	}
	if _, params, err := mime.ParseMediaType(ctype); err == nil {
		if cs := strings.ToLower(params["charset"]); cs != "" && cs != "utf-8" && cs != "utf8" && cs != "us-ascii" {
			if enc, err := htmlindex.Get(cs); err == nil {
				if out, err := enc.NewDecoder().Bytes(body); err == nil && utf8.Valid(out) {
					return string(out)
				}
			}
		}
	}
	return strings.ToValidUTF8(string(body), "")
}

// htmlOwnText cuts HTML at its first quote mark (FO2.1) or at Gmail's signature block,
// whichever comes first, and returns the part above as plain text.
func htmlOwnText(s string) string {
	own, _ := stripHTMLQuote(s)
	lower := asciiLower(s)
	if i := strings.Index(lower, htmlSignatureMark); i >= 0 {
		if lt := strings.LastIndexByte(s[:i], '<'); lt >= 0 {
			i = lt
		}
		if sig := htmlToText(s[:i]); len(sig) < len(own) {
			own = sig
		}
	}
	return own
}

// cutAtAttribution drops every line from the first attribution on (FO2.3): a line that
// matches alone, or a wrapped pair that matches joined. When peekFull is true (the text
// may have been cut), a last line that starts like an attribution and holds a digit goes
// too: the peek most likely cut it before its "wrote:".
func cutAtAttribution(lines []string, peekFull bool) []string {
	for i, l := range lines {
		if attributionRe.MatchString(l) {
			return lines[:i]
		}
		if i+1 < len(lines) && attributionStart.MatchString(l) && attributionRe.MatchString(l+" "+lines[i+1]) {
			return lines[:i]
		}
	}
	if peekFull {
		last := len(lines) - 1
		for last >= 0 && strings.TrimSpace(lines[last]) == "" {
			last--
		}
		if last >= 0 && cutAttributionRe.MatchString(lines[last]) {
			return lines[:last]
		}
	}
	return lines
}

// cutAtSignature drops every line from a signature separator or a mobile footer on.
func cutAtSignature(lines []string) []string {
	for i, l := range lines {
		if l == "-- " || l == "--" || mobileFooterRe.MatchString(l) {
			return lines[:i]
		}
	}
	return lines
}

// collapseSpace makes every run of whitespace one space and trims both ends.
func collapseSpace(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// clipSnippet returns s when it fits MaxSnippetRunes; else it cuts it on a rune boundary,
// backs off to the last space when that keeps at least two thirds of the room, and appends
// "…" within the limit.
func clipSnippet(s string) string {
	r := []rune(s)
	if len(r) <= MaxSnippetRunes {
		return s
	}
	atWord := unicode.IsSpace(r[MaxSnippetRunes-1]) // the cut already falls between words
	r = r[:MaxSnippetRunes-1]
	for i := len(r) - 1; !atWord && i >= MaxSnippetRunes*2/3; i-- {
		if r[i] == ' ' {
			r = r[:i]
			break
		}
	}
	return strings.TrimRightFunc(string(r), unicode.IsSpace) + "…"
}
