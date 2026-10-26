package mailbox

import (
	"bytes"
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SelftestLine is the instruction sentence of a selftest body (FO3). It holds no opt-out
// word, and the sync removes it as one of our own lines, so a reply quoting it is a reply.
const SelftestLine = "Reply to this message to test the reply sync; to test the stop path, reply with the word your emails ask people to send."

// OldSelftestLine is the selftest sentence sent before FO3. Replies to those selftests
// still quote it, so the sync keeps removing it as one of our own lines.
const OldSelftestLine = "Reply to this message to test the reply sync; a reply with the word unsubscribe tests the opt-out."

// minCutLine is the shortest end-of-text prefix of one of our lines that FO3 removes: the
// 600-byte peek can cut our line, and a shorter prefix is too likely to be the sender's.
const minCutLine = 12

var (
	// FO2.1: where an HTML reply's quote starts (lower case; matched against lowered text).
	htmlQuoteMarks = []string{`<blockquote`, `<div class="gmail_quote`, `<div id="appendonsend"`, `<div id="divrplyfwdmsg"`, `<hr`}
	htmlDropBlock  = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<style\b.*?</style\s*>`),
		regexp.MustCompile(`(?is)<script\b.*?</script\s*>`),
		regexp.MustCompile(`(?is)<head\b.*?</head\s*>`),
	}
	htmlLineTag = regexp.MustCompile(`(?i)<\s*/?\s*(br|p|div|li|tr|blockquote|hr|h[1-6]|table|ul|ol)\b[^>]*>`)
	htmlAnyTag  = regexp.MustCompile(`<[^>]*(>|$)`)

	// FO2.3: attribution lines, en / ro / de / nl / fr.
	attributionRe = regexp.MustCompile(`(?i)^\s*(On .+ wrote:|(În|In) .+ a scris:|Am .+ schrieb .*:|Op .+ schreef .*:|Le .+ a écrit\s*:)\s*$`)
	// attributionStart: a line that may be the first half of a wrapped attribution.
	attributionStart = regexp.MustCompile(`(?i)^\s*(On|În|In|Am|Op|Le) `)

	// FO2.4: an unprefixed forwarded/Outlook quote header.
	quoteSeparatorRe = regexp.MustCompile(`(?i)^\s*-{2,}\s*(Original Message|Mesaj original|Ursprüngliche Nachricht|Oorspronkelijk bericht|Message d'origine)\s*-{2,}`)
	quoteRuleRe      = regexp.MustCompile(`^\s*_{8,}\s*$`)
	quoteFromRe      = regexp.MustCompile(`(?i)^\s*\*?(From|De la|De|Von|Van|Od):\*?\s.+`)
	quoteSentRe      = regexp.MustCompile(`(?i)^\s*\*?(Sent|Date|Trimis|Data|Gesendet|Datum|Verzonden|Envoyé|Wysłano):\*?\s`)
)

// HasOptOutWord (FO1) says whether s holds one of the opt-out words in any of our
// languages.
func HasOptOutWord(s string) bool { return optOutWords.MatchString(s) }

// OwnText (FO2, FO3) turns already-decoded text into the sender's own words. own is the
// text above any quote header (FO2.4) or HTML quote (FO2.1), with quoted lines, attribution
// lines and our own lines taken out; tail is the text from that header on, with its quoted
// lines and our own lines taken out. tail is kept because a sender may answer inline under
// our quoted email, so the caller counts an opt-out word in either. html says the text is
// text/html; text starting with "<" is taken as HTML too. Both are returned with LF line
// ends; our lines are replaced by a space, never by nothing, so no two words are glued.
func OwnText(text []byte, html bool, ownLines []string) (own, tail []byte) {
	s := strings.ReplaceAll(string(text), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	var htmlTail string
	if html || strings.HasPrefix(strings.TrimLeftFunc(s, unicode.IsSpace), "<") {
		s, htmlTail = stripHTMLQuote(s)
	}

	lines := dropAttributions(dropQuotedLines(strings.Split(s, "\n")))
	ownLinesOut, tailLines := cutQuoteHeader(lines)

	tailText := strings.Join(dropQuotedLines(tailLines), "\n")
	if htmlTail != "" {
		tailText += "\n" + strings.Join(dropQuotedLines(strings.Split(htmlTail, "\n")), "\n")
	}
	return []byte(removeOwnLines(strings.Join(ownLinesOut, "\n"), ownLines)),
		[]byte(removeOwnLines(tailText, ownLines))
}

// stripHTMLQuote (FO2.1) cuts an HTML text at its first quote mark (a blockquote, Gmail's
// gmail_quote, Outlook's appendonsend or divRplyFwdMsg, an <hr>), and returns both halves
// as plain text: tags stripped (a block tag becomes a line break) and entities unescaped.
func stripHTMLQuote(s string) (own, tail string) {
	lower := asciiLower(s)
	at := -1
	for _, m := range htmlQuoteMarks {
		if i := strings.Index(lower, m); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		return htmlToText(s), ""
	}
	return htmlToText(s[:at]), htmlToText(s[at:])
}

// htmlToText strips tags (a cut tag at the end included) and unescapes entities.
func htmlToText(s string) string {
	for _, re := range htmlDropBlock {
		s = re.ReplaceAllString(s, " ")
	}
	s = htmlLineTag.ReplaceAllString(s, "\n")
	s = htmlAnyTag.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

// asciiLower lowers ASCII letters only, so byte offsets stay those of s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// dropQuotedLines (FO2.2) drops every line whose first non-space character is ">". A ">"
// later in a line ("a > b") is the sender's.
func dropQuotedLines(lines []string) []string {
	out := lines[:0:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimLeftFunc(l, unicode.IsSpace), ">") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// dropAttributions (FO2.3) drops "On … wrote:" lines and their ro/de/nl/fr forms. A line
// is tested on its own first; a line is joined with the next (a wrapped attribution) only
// when neither matches alone and the first starts like an attribution, and then both go
// only if the pair matches. A candidate that holds an opt-out word is never dropped: a real
// attribution is a date and a name. Text after an attribution stays.
func dropAttributions(lines []string) []string {
	out := lines[:0:0]
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if isAttribution(l) {
			continue
		}
		if i+1 < len(lines) && attributionStart.MatchString(l) && !attributionRe.MatchString(l) &&
			!attributionRe.MatchString(lines[i+1]) && isAttribution(l+" "+lines[i+1]) {
			i++
			continue
		}
		out = append(out, l)
	}
	return out
}

// isAttribution: an attribution line holding no opt-out word.
func isAttribution(l string) bool {
	return attributionRe.MatchString(l) && !HasOptOutWord(l)
}

// cutQuoteHeader (FO2.4) splits the lines at the first unprefixed quote header: an
// "Original Message" separator, a line of underscores, or a From: line with a Sent:/Date:
// line within the next three. The lines above are the sender's own; the header and what
// follows are returned as the tail. No header: every line is own.
func cutQuoteHeader(lines []string) (own, tail []string) {
	for i, l := range lines {
		if quoteSeparatorRe.MatchString(l) || quoteRuleRe.MatchString(l) {
			return lines[:i], lines[i:]
		}
		if quoteFromRe.MatchString(l) {
			for j := i + 1; j <= i+3 && j < len(lines); j++ {
				if quoteSentRe.MatchString(lines[j]) {
					return lines[:i], lines[i:]
				}
			}
		}
	}
	return lines, nil
}

// isQuoteMark: the quote marks FO3 ignores when comparing our lines.
func isQuoteMark(r rune) bool {
	switch r {
	case '"', '\'', '„', '”', '“', '’', '‘', '«', '»':
		return true
	}
	return false
}

// normOptOut (FO3) normalises s for comparing our lines: lower case, the quote marks
// removed, every run of whitespace one space, trimmed. A broken UTF-8 byte (the 600-byte
// peek can cut a character) is dropped.
func normOptOut(s string) string {
	runes, _, _ := normMapped(s)
	return strings.TrimSpace(string(runes))
}

// normMapped is normOptOut with, for each rune of the result, the byte span of s it came
// from, so a match in the normalised text can be removed from s itself.
func normMapped(s string) (norm []rune, start, end []int) {
	space := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		j := i + size
		switch {
		case r == utf8.RuneError || isQuoteMark(r):
		case unicode.IsSpace(r):
			if space {
				end[len(end)-1] = j
			} else {
				norm, start, end = append(norm, ' '), append(start, i), append(end, j)
				space = true
			}
		default:
			norm, start, end = append(norm, unicode.ToLower(r)), append(start, i), append(end, j)
			space = false
		}
		i = j
	}
	return norm, start, end
}

// removeOwnLines (FO3) replaces each of our own lines, wherever it appears in text, by one
// space, comparing both sides normalised (normOptOut). When text ends with a prefix of one
// of our lines at least minCutLine normalised characters long (the peek cut it), that end
// goes too.
func removeOwnLines(text string, ownLines []string) string {
	if text == "" || len(ownLines) == 0 {
		return text
	}
	norm, start, end := normMapped(text)
	type span struct{ a, b int } // rune indexes into norm, [a, b)
	var spans []span
	n := len(norm)
	for n > 0 && norm[n-1] == ' ' {
		n--
	}
	for _, line := range ownLines {
		ln := []rune(normOptOut(line))
		if len(ln) == 0 {
			continue
		}
		for i := 0; i+len(ln) <= len(norm); {
			if slices.Equal(norm[i:i+len(ln)], ln) {
				spans = append(spans, span{i, i + len(ln)})
				i += len(ln)
				continue
			}
			i++
		}
		for k := len(ln) - 1; k >= minCutLine; k-- {
			if k <= n && slices.Equal(norm[n-k:n], ln[:k]) {
				spans = append(spans, span{n - k, n})
				break
			}
		}
	}
	if len(spans) == 0 {
		return text
	}
	// Mark the bytes to remove, then rebuild: each removed run becomes one space.
	cutByte := make([]bool, len(text))
	for _, sp := range spans {
		for b := start[sp.a]; b < end[sp.b-1]; b++ {
			cutByte[b] = true
		}
	}
	var out bytes.Buffer
	for i := 0; i < len(text); i++ {
		if cutByte[i] {
			if i == 0 || !cutByte[i-1] {
				out.WriteByte(' ')
			}
			continue
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

// isJustStop (FO1): the sender's own text, trimmed of whitespace and punctuation, is just
// "stop".
func isJustStop(own []byte) bool {
	t := strings.TrimFunc(string(own), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
	return strings.EqualFold(t, "stop")
}
