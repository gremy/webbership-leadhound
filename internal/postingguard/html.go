package postingguard

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// dropped elements are never rendered as text: they are removed and scanned, not listed.
// Upstream drops script, style, noscript, svg and template; iframe, noembed, noframes and
// title are added because the tokenizer reads their content as raw text, which a browser
// does not render either.
var dropped = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "template": true,
	"iframe": true, "noembed": true, "noframes": true, "title": true,
}

var voidElements = map[string]bool{
	"br": true, "img": true, "input": true, "meta": true, "link": true, "hr": true, "wbr": true, "source": true,
	"area": true, "base": true, "col": true, "embed": true, "param": true, "track": true,
}

// blockElements start and end on their own line.
var blockElements = map[string]bool{
	"p": true, "div": true, "li": true, "ul": true, "ol": true, "h1": true, "h2": true, "h3": true, "h4": true,
	"h5": true, "h6": true, "tr": true, "br": true, "section": true, "article": true, "header": true,
	"footer": true, "table": true, "thead": true, "tbody": true, "blockquote": true, "pre": true, "dd": true,
	"dt": true, "dl": true, "nav": true, "main": true, "aside": true, "figure": true, "form": true,
	"fieldset": true, "legend": true, "address": true, "hr": true, "td": true, "th": true, "option": true,
}

// impliedEnd elements are closed by an opening sibling of the same name.
var impliedEnd = map[string]bool{"p": true, "li": true, "dd": true, "dt": true, "tr": true, "td": true, "th": true, "option": true}

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// hiddenStyle holds upstream's HIDDEN_STYLE alternatives that need no lookahead.
	hiddenStyle = regexp.MustCompile(`(?i)display\s*:\s*none|visibility\s*:\s*hidden|color\s*:\s*transparent` +
		`|clip\s*:\s*rect\(\s*0|clip-path\s*:\s*inset\(\s*100%|(?:left|top|text-indent)\s*:\s*-\d{4,}px`)
	// opacity:0 (or .0) unless a non-zero digit follows ("opacity:0.5" is visible).
	opacityZero    = regexp.MustCompile(`(?i)opacity\s*:\s*\.?0`)
	opacityNonZero = regexp.MustCompile(`^[.\d]*[1-9]`)
	fontSizeZero   = regexp.MustCompile(`(?i)font-size\s*:\s*0`)
	// A box collapsed to nothing with its overflow clipped hides its content; either half
	// alone is everyday layout CSS, so both must be present.
	collapsedBox = regexp.MustCompile(`(?i)\b(?:max-)?(?:height|width)\s*:\s*0`)
	clipped      = regexp.MustCompile(`(?i)\boverflow(?:-[xy])?\s*:\s*hidden\b`)
	zeroDecimals = regexp.MustCompile(`^\.0+`)
	cssUnit      = regexp.MustCompile(`^(?i)(?:px|pt|em|rem|%)`)
	tagInDropped = regexp.MustCompile(`<[^>]+>`)

	// The rest go beyond upstream's HIDDEN_STYLE: other spellings of the same tricks.
	moreHidden = regexp.MustCompile(`(?i)visibility\s*:\s*collapse` +
		// A negative offset of three or more digits, in any unit, moves the text off-screen.
		`|\b(?:left|top|text-indent|margin(?:-left|-top)?)\s*:\s*-\d{3,}`)
	// The text colour with an alpha of zero: rgba(0,0,0,0), rgb(0 0 0 / 0%), hsla(...).
	colorFunc = regexp.MustCompile(`(?i)(?:^|[;\s])color\s*:\s*(?:rgba?|hsla?)\(([^)]*)\)`)
	alphaZero = regexp.MustCompile(`^(?:0*\.?0*%?)$`)
	// font-size of at most 1px.
	fontSizePx = regexp.MustCompile(`(?i)font-size\s*:\s*(\d*\.?\d+)px`)
	// scale(0), scaleX(0), scale(0, 0) anywhere in a transform, or the scale property.
	scaleZero = regexp.MustCompile(`(?i)(?:transform\s*:[^;]*\bscale(?:x|y|3d)?\(\s*|\bscale\s*:\s*)0(?:\.0*)?\s*(?:[,)]|$|;)`)
	// clip-path: circle(0), circle(0px at ...), circle(0%).
	circleZero = regexp.MustCompile(`(?i)clip-path\s*:\s*circle\(\s*0`)
)

// colorTransparent reports whether a color function in style has an alpha of zero.
func colorTransparent(style string) bool {
	for _, m := range colorFunc.FindAllStringSubmatch(style, -1) {
		args := m[1]
		var alpha string
		if i := strings.LastIndexByte(args, '/'); i >= 0 {
			alpha = args[i+1:]
		} else if parts := strings.Split(args, ","); len(parts) == 4 {
			alpha = parts[3]
		} else {
			continue // no alpha given: opaque
		}
		if a := strings.TrimSpace(alpha); a != "" && strings.ContainsRune(a, '0') && alphaZero.MatchString(a) {
			return true
		}
	}
	return false
}

// fontTiny reports a font-size of at most 1px.
func fontTiny(style string) bool {
	for _, m := range fontSizePx.FindAllStringSubmatch(style, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v <= 1 {
			return true
		}
	}
	return false
}

// zeroLength reports whether s, read just after a "0", completes a zero length: an optional
// ".0+" and an optional unit, then no further digit or dot (upstream's
// `0(?:\.0+)?(?:px|...)?(?![\d.])`, with every way the optional parts can match tried).
func zeroLength(s string) bool {
	ends := []int{0}
	if m := zeroDecimals.FindString(s); m != "" {
		ends = append(ends, len(m))
	}
	for _, e := range ends {
		if u := cssUnit.FindString(s[e:]); u != "" {
			ends = append(ends, e+len(u))
		}
	}
	for _, e := range ends {
		if e == len(s) || !strings.ContainsRune("0123456789.", rune(s[e])) {
			return true
		}
	}
	return false
}

func anyMatchThen(re *regexp.Regexp, s string, ok func(rest string) bool) bool {
	for _, loc := range re.FindAllStringIndex(s, -1) {
		if ok(s[loc[1]:]) {
			return true
		}
	}
	return false
}

// styleHides reads a style value the way a browser does (entities are already decoded by
// the tokenizer; CSS comments are removed) and reports whether it hides the element.
func styleHides(style string) bool {
	style = cssComment.ReplaceAllString(style, "")
	if hiddenStyle.MatchString(style) {
		return true
	}
	if anyMatchThen(opacityZero, style, func(rest string) bool { return !opacityNonZero.MatchString(rest) }) {
		return true
	}
	if anyMatchThen(fontSizeZero, style, zeroLength) {
		return true
	}
	if clipped.MatchString(style) && anyMatchThen(collapsedBox, style, zeroLength) {
		return true
	}
	return moreHidden.MatchString(style) || colorTransparent(style) || fontTiny(style) ||
		scaleZero.MatchString(style) || anyMatchThen(circleZero, style, zeroLength)
}

// hiddenAttrs reports whether a start tag's attributes hide it. A <dialog> without open is
// not rendered, so it hides its content too (beyond upstream).
func hiddenAttrs(name string, attrs []html.Attribute) bool {
	if name == "dialog" && !slices.ContainsFunc(attrs, func(a html.Attribute) bool { return a.Key == "open" }) {
		return true
	}
	for _, a := range attrs {
		switch a.Key {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimFunc(a.Val, isPySpace), "true") {
				return true
			}
		case "style":
			if styleHides(a.Val) {
				return true
			}
		}
	}
	return false
}

// walker turns markup into visible text, keeping a stack of open elements with HTML5
// implied end tags, so a stray closer cannot end a hidden element early and an unclosed
// <li> or <p> inside one cannot swallow the rest of the page.
type walker struct {
	visible, hiddenBuf strings.Builder
	stack              []string
	hiddenAt           int // stack index of the element that started the hidden run, or -1
	blocks             []string
	blockTexts         []string
	comments, drops    []string
	dropName           string // the dropped element being skipped, or ""
	dropDepth          int
	dropBuf            strings.Builder
}

func (w *walker) emit(s string) {
	if w.hiddenAt < 0 {
		w.visible.WriteString(s)
	} else {
		w.hiddenBuf.WriteString(s)
	}
}

func (w *walker) flushHidden() {
	block := fold(w.hiddenBuf.String())
	w.hiddenBuf.Reset()
	w.hiddenAt = -1
	if block != "" {
		rs := []rune(block)
		w.blocks = append(w.blocks, string(rs[:min(len(rs), excerptMax)]))
		w.blockTexts = append(w.blockTexts, block)
	}
}

func (w *walker) popTo(i int) {
	w.stack = w.stack[:i]
	if w.hiddenAt >= 0 && w.hiddenAt >= len(w.stack) {
		w.flushHidden()
	}
}

func (w *walker) lastIndex(name string) int {
	for i := len(w.stack) - 1; i >= 0; i-- {
		if w.stack[i] == name {
			return i
		}
	}
	return -1
}

// flushDrop records a dropped element's content for scanning: markup inside it becomes
// spaces and entities are decoded, as upstream collects it.
func (w *walker) flushDrop() {
	inner := tagInDropped.ReplaceAllString(w.dropBuf.String(), " ")
	w.drops = append(w.drops, html.UnescapeString(" "+inner+" "))
	w.dropBuf.Reset()
	w.dropName, w.dropDepth = "", 0
}

func (w *walker) startTag(z *html.Tokenizer, selfClosing bool) {
	tok := z.Token() // a repeated attribute keeps its first value, as browsers do
	name := tok.Data
	if w.dropName != "" {
		if name == w.dropName && !selfClosing {
			w.dropDepth++
		}
		w.dropBuf.WriteString(" ")
		return
	}
	if dropped[name] {
		if name == "svg" && selfClosing {
			return // an empty foreign element; nothing to hide
		}
		w.emit(" ")
		w.dropName, w.dropDepth = name, 1
		return
	}
	if blockElements[name] {
		w.emit("\n")
	}
	if voidElements[name] {
		return
	}
	// A "/>" on a non-void element is ignored, as browsers do.
	top := ""
	if len(w.stack) > 0 {
		top = w.stack[len(w.stack)-1]
	}
	switch {
	case impliedEnd[name] && top == name:
		w.popTo(len(w.stack) - 1)
	case blockElements[name] && name != "br" && top == "p":
		w.popTo(len(w.stack) - 1)
	}
	w.stack = append(w.stack, name)
	if w.hiddenAt < 0 && hiddenAttrs(name, tok.Attr) {
		w.hiddenAt = len(w.stack) - 1
	}
}

func (w *walker) endTag(name string) {
	if w.dropName != "" {
		if name == w.dropName {
			w.dropDepth--
			if w.dropDepth == 0 {
				w.flushDrop()
				return
			}
		}
		w.dropBuf.WriteString(" ")
		return
	}
	if blockElements[name] {
		w.emit("\n")
	}
	if i := w.lastIndex(name); i >= 0 {
		w.popTo(i)
	} // a closer with no matching opener is ignored
}

// cleanHTML returns the visible text, everything removed (for scanning) and excerpts of the
// hidden elements. Unterminated comments and dropped elements hide to the end of input, as
// browsers do.
func cleanHTML(markup string) (visible, hidden string, blocks []string) {
	w := &walker{hiddenAt: -1}
	z := html.NewTokenizer(strings.NewReader(markup))
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			// A strings.Reader only ends with io.EOF: the end of input.
			if w.dropName != "" {
				w.flushDrop()
			}
			if w.hiddenAt >= 0 {
				w.flushHidden()
			}
			parts := append(append(w.comments, w.drops...), w.blockTexts...)
			return fold(w.visible.String()), strings.Join(parts, "\n"), w.blocks
		case html.TextToken:
			if w.dropName != "" {
				w.dropBuf.Write(z.Raw())
			} else {
				w.emit(string(z.Text()))
			}
		case html.CommentToken:
			if strings.HasPrefix(string(z.Raw()), "<?") {
				continue // an XML prolog or processing instruction is not content
			}
			data := string(z.Text())
			w.comments = append(w.comments, html.UnescapeString(tagInDropped.ReplaceAllString(" "+data+" ", " ")))
			if w.dropName == "" {
				w.emit(" ")
			}
		case html.StartTagToken:
			w.startTag(z, false)
		case html.SelfClosingTagToken:
			w.startTag(z, true)
		case html.EndTagToken:
			name, _ := z.TagName()
			w.endTag(string(name))
		case html.DoctypeToken:
		}
	}
}
