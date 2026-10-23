package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Kind says what sort of page is being scrubbed.
type Kind string

// The page kinds scrubfixture understands.
const (
	KindSearch  Kind = "search"
	KindDetail  Kind = "detail"
	KindCompany Kind = "company"
)

// firstFakeID is the first synthetic job ID; every fake ID has ten digits.
const firstFakeID = 1000000001

// Options change one scrub.
type Options struct {
	Kind Kind
	// Lang is the language of the description text. Empty means detect it.
	Lang Lang
	// Closed inserts the documented closed-job banner into the top card.
	Closed bool
	// RenameTitleClass renames the search card title class, so a parser that needs it breaks.
	RenameTitleClass bool
}

// Scrubber holds the keys shared by every page scrubbed in one run, so the same real job
// or company gets the same synthetic one in every output.
type Scrubber struct {
	ids       map[string]int // real number -> synthetic job number (1-based)
	fakes     map[string]bool
	companies map[string]int // real company key -> index into companyNames
	detect    func(string) (Lang, bool)

	// per-page state, reset by Scrub
	lang     Lang
	sentence int
	filler   int
	heading  int
	pageJob  int
}

// NewScrubber returns a scrubber. detect finds the language of a description; nil means
// every page is treated as English.
func NewScrubber(detect func(string) (Lang, bool)) *Scrubber {
	return &Scrubber{ids: map[string]int{}, fakes: map[string]bool{}, companies: map[string]int{}, detect: detect}
}

var (
	jobURNRe      = regexp.MustCompile(`jobPosting:(\d+)`)
	jobViewRe     = regexp.MustCompile(`/jobs/view/(?:[^/?#"]*-)?(\d{6,})`)
	longDigitsRe  = regexp.MustCompile(`\d{8,}`)
	companyHrefRe = regexp.MustCompile(`linkedin\.com/company/([^/?#"&\s]+)`)
	emailLikeRe   = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}`)
	ipv4LikeRe    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	postedRe      = regexp.MustCompile(`^(?:Reposted )?(?:\d+|an?) (?:second|minute|hour|day|week|month|year)s? ago$`)
	applicantsRe  = regexp.MustCompile(`^(?:Over \d+ applicants|\d+ applicants?|Be among the first \d+ applicants)$`)
)

// profileMarker is split so the source does not trip the profile-link rule it enforces.
const profileMarker = "/" + "in/"

// recruiterClass is the recruiter card's class; the scrubbed output never contains it.
const recruiterClass = "message-the-" + "recruiter"

// fakeID returns the synthetic ten-digit ID for a real number, assigning the next one on
// first sight.
func (s *Scrubber) fakeID(real string) string {
	n, ok := s.ids[real]
	if !ok {
		n = len(s.ids) + 1
		s.ids[real] = n
	}
	fake := strconv.Itoa(firstFakeID - 1 + n)
	s.fakes[fake] = true
	return fake
}

// jobNumber returns the synthetic job number (1-based) for a real job ID.
func (s *Scrubber) jobNumber(real string) int {
	s.fakeID(real)
	return s.ids[real]
}

// companyName returns the fictional name for a real company key.
func (s *Scrubber) companyName(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	i, ok := s.companies[key]
	if !ok {
		i = len(s.companies)
		s.companies[key] = i
	}
	name := companyNames[i%len(companyNames)]
	if r := i / len(companyNames); r > 0 {
		name += " " + strconv.Itoa(r+1)
	}
	return name
}

// Scrub turns one raw page into a scrubbed one.
func (s *Scrubber) Scrub(raw []byte, opt Options) ([]byte, error) {
	doc, nodes, full, err := parse(raw)
	if err != nil {
		return nil, err
	}
	s.sentence, s.filler, s.heading, s.pageJob = 0, 0, 0, 0
	s.lang = opt.Lang
	if s.lang == "" {
		s.lang = LangEnglish
		if s.detect != nil {
			if l, ok := s.detect(descriptionText(nodes)); ok {
				s.lang = l
			}
		}
	}
	if opt.Kind == KindDetail {
		if m := jobURNRe.FindSubmatch(raw); m != nil {
			s.pageJob = s.jobNumber(string(m[1]))
		} else if m := jobViewRe.FindSubmatch(raw); m != nil {
			s.pageJob = s.jobNumber(string(m[1]))
		}
	}
	// Card job numbers in search order, before anything else claims a number.
	for _, n := range nodes {
		walk(n, func(e *html.Node) {
			if v := attr(e, "data-entity-urn"); v != "" {
				if m := jobURNRe.FindStringSubmatch(v); m != nil {
					s.jobNumber(m[1])
				}
			}
		})
	}

	var kept []*html.Node
	for _, n := range nodes {
		if s.removeNode(n, opt.Kind) {
			continue
		}
		s.prune(n, opt.Kind)
		kept = append(kept, n)
	}
	for _, n := range kept {
		s.annotate(n)
	}
	for _, n := range kept {
		s.rewrite(n, opt)
	}
	for _, n := range kept {
		walk(n, func(e *html.Node) {
			if e.Type == html.ElementNode {
				delAttr(e, keyAttr)
				delAttr(e, jobAttr)
			}
		})
	}
	if opt.Closed {
		if err := insertClosedBanner(kept); err != nil {
			return nil, err
		}
	}
	if opt.RenameTitleClass {
		for _, n := range kept {
			walk(n, func(e *html.Node) {
				if hasClass(e, "base-search-card__title") {
					setAttr(e, "class", strings.ReplaceAll(attr(e, "class"), "base-search-card__title", "base-search-card__heading"))
				}
			})
		}
	}

	var buf bytes.Buffer
	if full {
		if err := html.Render(&buf, doc); err != nil {
			return nil, err
		}
	} else {
		for _, n := range kept {
			if err := html.Render(&buf, n); err != nil {
				return nil, err
			}
		}
	}
	out := buf.Bytes()
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return out, nil
}

// parse reads a whole document (when the input has an <html> tag) or a fragment, which is
// what LinkedIn's guest endpoints return.
func parse(raw []byte) (doc *html.Node, nodes []*html.Node, full bool, err error) {
	if bytes.Contains(bytes.ToLower(raw), []byte("<html")) {
		doc, err = html.Parse(bytes.NewReader(raw))
		if err != nil {
			return nil, nil, false, err
		}
		var top []*html.Node
		for c := doc.FirstChild; c != nil; c = c.NextSibling {
			top = append(top, c)
		}
		return doc, top, true, nil
	}
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err = html.ParseFragment(bytes.NewReader(raw), ctx)
	if err != nil {
		return nil, nil, false, err
	}
	// ParseFragment returns detached nodes; give them one parent so they can be removed.
	root := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	return root, nodes, false, nil
}

// removedTags never survive a scrub: scripts, JSON blobs, images, metadata.
var removedTags = map[string]bool{
	"script": true, "noscript": true, "code": true, "img": true, "meta": true, "link": true,
	"iframe": true, "style": true, "template": true, "picture": true, "source": true, "video": true,
}

// removeNode reports whether a node goes entirely.
func (s *Scrubber) removeNode(n *html.Node, kind Kind) bool {
	if n.Type == html.CommentNode {
		return true
	}
	if n.Type != html.ElementNode {
		return false
	}
	if removedTags[n.Data] {
		return true
	}
	if strings.Contains(attr(n, "class"), recruiterClass) {
		return true
	}
	if kind == KindCompany && isPeopleBlock(n) {
		return true
	}
	return false
}

// isPeopleBlock finds employee and people lists on a company page: a section or list whose
// class names employees or people, one headed "Employees at …", or any card linking a
// personal profile.
func isPeopleBlock(n *html.Node) bool {
	cls := strings.ToLower(attr(n, "class"))
	if strings.Contains(cls, "employee") || strings.Contains(cls, "people") {
		return true
	}
	if n.Data == "section" || n.Data == "div" {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.Data == "h2" || c.Data == "h3") &&
				strings.HasPrefix(strings.TrimSpace(textOf(c)), "Employees at") {
				return true
			}
		}
	}
	if n.Data == "li" || strings.Contains(cls, "base-card") || strings.Contains(cls, "base-main-card") {
		found := false
		walk(n, func(e *html.Node) {
			if strings.Contains(attr(e, "href"), profileMarker) {
				found = true
			}
		})
		return found
	}
	return false
}

// prune removes unwanted descendants depth-first.
func (s *Scrubber) prune(n *html.Node, kind Kind) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if s.removeNode(c, kind) {
			n.RemoveChild(c)
		} else {
			s.prune(c, kind)
		}
		c = next
	}
}

// rewrite replaces attribute values and text throughout a subtree.
func (s *Scrubber) rewrite(n *html.Node, opt Options) {
	walk(n, func(e *html.Node) {
		switch e.Type {
		case html.ElementNode:
			s.rewriteAttrs(e)
		case html.TextNode:
			s.rewriteText(e, opt)
		}
	})
}

// Attribute groups: random tokens, human-readable labels and URLs. Every other attribute
// carries fixed interface values and passes through the generic value checks only.
var tokenAttrs = map[string]bool{"data-tracking-id": true, "data-reference-id": true}
var labelAttrs = map[string]bool{"aria-label": true, "alt": true, "title": true, "placeholder": true}
var urlAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "data-delayed-url": true, "data-ghost-url": true,
	"data-url": true, "data-apply-url": true, "srcset": true, "poster": true,
}

func (s *Scrubber) rewriteAttrs(e *html.Node) {
	var out []html.Attribute
	for _, a := range e.Attr {
		key := strings.ToLower(a.Key)
		val := a.Val
		if strings.HasPrefix(key, scratchPref) {
			out = append(out, a)
			continue
		}
		switch {
		case tokenAttrs[key]:
			val = "AAAAAAAAAAAAAAAAAAAAAA=="
		case labelAttrs[key]:
			if !uiText[strings.TrimSpace(val)] {
				val = "Label"
			}
		case urlAttrs[key]:
			var ok bool
			if val, ok = s.rewriteURL(val); !ok {
				continue
			}
		case key == "value":
			if looksLikeURL(val) {
				var ok bool
				if val, ok = s.rewriteURL(val); !ok {
					continue
				}
			} else {
				val = ""
			}
		case key == "datetime":
			val = shiftDate(val)
		case key == "data-entity-urn" || key == "data-semaphore-content-urn":
			val = s.rewriteNumbers(val)
		default:
			if looksLikeURL(val) {
				var ok bool
				if val, ok = s.rewriteURL(val); !ok {
					continue
				}
			}
		}
		if strings.Contains(val, profileMarker) || strings.Contains(strings.ToLower(val), recruiterClass) {
			continue
		}
		val = s.rewriteNumbers(val)
		val = emailLikeRe.ReplaceAllString(val, "someone@example.invalid")
		val = ipv4LikeRe.ReplaceAllString(val, "192.0.2.1")
		out = append(out, html.Attribute{Namespace: a.Namespace, Key: a.Key, Val: val})
	}
	e.Attr = out
}

// rewriteNumbers maps every run of eight or more digits (job IDs, URNs, tracking numbers)
// to a synthetic ten-digit ID.
func (s *Scrubber) rewriteNumbers(v string) string {
	return longDigitsRe.ReplaceAllStringFunc(v, func(d string) string {
		if s.fakes[d] {
			return d
		}
		return s.fakeID(d)
	})
}

func looksLikeURL(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "//")
}

// rewriteText replaces one text node by what its place in the page calls for.
func (s *Scrubber) rewriteText(t *html.Node, opt Options) {
	core := strings.TrimSpace(t.Data)
	if core == "" {
		return
	}
	lead := t.Data[:strings.Index(t.Data, core)]
	trail := t.Data[len(lead)+len(core):]
	t.Data = lead + s.replacement(t, core, opt) + trail
}

// replacement decides the synthetic text for a non-empty text node from its nearest
// meaningful ancestor.
func (s *Scrubber) replacement(t *html.Node, core string, opt Options) string {
	for a := t.Parent; a != nil; a = a.Parent {
		if a.Type != html.ElementNode {
			continue
		}
		switch {
		case a.Data == "title" || a.Data == "button" || hasClass(a, "show-more-less-html__button"):
			return s.generic(core)
		case hasClass(a, "base-search-card__title") || hasClass(a, "top-card-layout__title"):
			if opt.Kind == KindCompany {
				return s.companyName(core)
			}
			return syntheticTitle(s.jobFor(a))
		case hasClass(a, "sr-only") && cardOf(a) != nil:
			return syntheticTitle(s.jobFor(a))
		case hasClass(a, "base-search-card__subtitle") || hasClass(a, "topcard__org-name-link"):
			return s.companyName(companyKey(t.Parent, core))
		case hasClass(a, "num-applicants__caption"):
			if applicantsRe.MatchString(core) {
				return perturbCount(core)
			}
			return "Over 200 applicants"
		case hasClass(a, "posted-time-ago__text") || hasClassPrefix(a, "job-search-card__listdate"):
			if postedRe.MatchString(core) {
				return core
			}
			return "2 days ago"
		case hasClass(a, "job-search-card__location") ||
			(hasClass(a, "topcard__flavor--bullet") && !hasClass(a, "num-applicants__figure")):
			return syntheticLocation
		case hasClass(a, "job-posting-benefits__text"):
			if benefitTexts[core] {
				return core
			}
			return "Actively Hiring"
		case hasClass(a, "description__job-criteria-subheader"):
			if uiText[core] {
				return core
			}
			return "Industries"
		case hasClass(a, "description__job-criteria-text"):
			return criteriaValue(criteriaLabel(a), core)
		case hasClass(a, "compensation__salary"):
			return "€60,000.00/yr - €80,000.00/yr"
		case hasClass(a, "show-more-less-html__markup"):
			return s.description(t, a, core)
		}
	}
	return s.generic(core)
}

// generic keeps fixed interface wording and fills anything else with words from the
// page's phrase table, about as many as the original had.
func (s *Scrubber) generic(core string) string {
	if uiText[core] {
		return core
	}
	want := len(strings.Fields(core))
	want = max(1, min(want, 12))
	set := phrases[s.lang]
	words := strings.Fields(set.sentences[s.filler%len(set.sentences)])
	s.filler++
	if want < len(words) {
		w := strings.Join(words[:want], " ")
		return strings.TrimRight(w, ",.")
	}
	return strings.Join(words, " ")
}

// description fills description text with our own sentences in the page's language,
// headings for emphasised runs and one to four sentences for paragraphs and items.
func (s *Scrubber) description(t, markup *html.Node, core string) string {
	set := phrases[s.lang]
	for a := t.Parent; a != nil && a != markup; a = a.Parent {
		switch a.Data {
		case "strong", "b", "h1", "h2", "h3", "h4", "h5", "h6", "u":
			h := set.headings[s.heading%len(set.headings)]
			s.heading++
			return h
		}
	}
	n := max(1, min(4, (len(core)+60)/120))
	var out []string
	for range n {
		out = append(out, s.nextSentence())
	}
	return strings.Join(out, " ")
}

func (s *Scrubber) nextSentence() string {
	i := s.sentence
	s.sentence++
	if s.lang == LangEnglish {
		if i < len(englishMarkers) {
			return englishMarkers[i]
		}
		i -= len(englishMarkers)
	}
	set := phrases[s.lang]
	return set.sentences[i%len(set.sentences)]
}

// jobFor returns the synthetic job number for a title: its card's, or the page's.
func (s *Scrubber) jobFor(n *html.Node) int {
	if c := cardOf(n); c != nil {
		if v, err := strconv.Atoi(attr(c, jobAttr)); err == nil {
			return v
		}
	}
	if s.pageJob > 0 {
		return s.pageJob
	}
	return 1
}

// cardOf finds the search card (the element carrying data-entity-urn) around n.
func cardOf(n *html.Node) *html.Node {
	for a := n; a != nil; a = a.Parent {
		if a.Type == html.ElementNode && attr(a, "data-entity-urn") != "" {
			return a
		}
	}
	return nil
}

// companyKey keys a company by the slug of the nearest company link, else by its name.
// The link's href is rewritten before its text, so the original slug is read from the
// scratch attribute annotate left on the link.
func companyKey(n *html.Node, name string) string {
	for a := n; a != nil; a = a.Parent {
		if a.Type != html.ElementNode {
			continue
		}
		if k := attr(a, keyAttr); k != "" {
			return k
		}
	}
	return name
}

// Scratch attributes hold original keys during a scrub; they are removed before rendering.
const (
	keyAttr     = "data-scrubfixture-key"
	jobAttr     = "data-scrubfixture-job"
	scratchPref = "data-scrubfixture-"
)

// annotate records, before anything is rewritten, each card's synthetic job number and
// each company link's original slug.
func (s *Scrubber) annotate(n *html.Node) {
	walk(n, func(e *html.Node) {
		if e.Type != html.ElementNode {
			return
		}
		if m := jobURNRe.FindStringSubmatch(attr(e, "data-entity-urn")); m != nil {
			setAttr(e, jobAttr, strconv.Itoa(s.jobNumber(m[1])))
		}
		if m := companyHrefRe.FindStringSubmatch(attr(e, "href")); m != nil {
			setAttr(e, keyAttr, companySlugKey(m[1]))
		}
	})
}

func criteriaLabel(n *html.Node) string {
	for a := n; a != nil; a = a.Parent {
		if hasClass(a, "description__job-criteria-item") {
			var label string
			walk(a, func(e *html.Node) {
				if label == "" && hasClass(e, "description__job-criteria-subheader") {
					label = strings.TrimSpace(textOf(e))
				}
			})
			return label
		}
	}
	return ""
}

func criteriaValue(label, core string) string {
	if allowed, ok := criteriaValues[label]; ok && allowed[core] {
		return core
	}
	if d, ok := criteriaDefault[label]; ok {
		return d
	}
	return "Software Development"
}

// closedBanner is LinkedIn's documented closed-job banner (from the upstream scraper's
// selectors), not markup observed on a live page.
const closedBanner = `<figure class="closed-job closed-job__flavor topcard__flavor-row"><figcaption>No longer accepting applications</figcaption></figure>`

// ErrNoTopCard means a closed banner was asked for on a page without a top card.
var ErrNoTopCard = errors.New("no top card to put the closed banner in")

func insertClosedBanner(nodes []*html.Node) error {
	var target *html.Node
	for _, n := range nodes {
		walk(n, func(e *html.Node) {
			if target == nil && hasClass(e, "top-card-layout__second-subline") {
				target = e
			}
		})
	}
	if target == nil {
		return ErrNoTopCard
	}
	frag, err := html.ParseFragment(strings.NewReader(closedBanner), target)
	if err != nil {
		return fmt.Errorf("closed banner: %w", err)
	}
	target.AppendChild(&html.Node{Type: html.CommentNode,
		Data: " scrubfixture: the closed-job banner below is built from upstream-documented markup, not observed on a live page "})
	for _, f := range frag {
		target.AppendChild(f)
	}
	return nil
}

// descriptionText is the text of the first description block, for language detection.
func descriptionText(nodes []*html.Node) string {
	var out string
	for _, n := range nodes {
		walk(n, func(e *html.Node) {
			if out == "" && hasClass(e, "show-more-less-html__markup") {
				out = textOf(e)
			}
		})
	}
	if out == "" {
		for _, n := range nodes {
			out += textOf(n)
		}
	}
	return out
}

// --- small DOM helpers ---

func walk(n *html.Node, f func(*html.Node)) {
	f(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, f)
	}
}

func attr(n *html.Node, key string) string {
	if n == nil || n.Type != html.ElementNode {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i, a := range n.Attr {
		if a.Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func delAttr(n *html.Node, key string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != key {
			out = append(out, a)
		}
	}
	n.Attr = out
}

func hasClass(n *html.Node, class string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

func hasClassPrefix(n *html.Node, prefix string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	walk(n, func(e *html.Node) {
		if e.Type == html.TextNode {
			b.WriteString(e.Data)
			b.WriteByte(' ')
		}
	})
	return b.String()
}

// dateShift moves every real date attribute by a fixed number of days, so a fixture's
// dates cannot be matched against the day the probe ran. Fixed, so output stays
// deterministic.
const dateShift = -3

// shiftDate moves a YYYY-MM-DD date by dateShift days, keeping any time part; a value
// that does not start with a date becomes a fixed synthetic date.
func shiftDate(v string) string {
	if len(v) < 10 {
		return "2026-01-01"
	}
	d, err := time.Parse("2006-01-02", v[:10])
	if err != nil {
		return "2026-01-01"
	}
	return d.AddDate(0, 0, dateShift).Format("2006-01-02") + v[10:]
}

// exactApplicantsRe is the one applicant shape that carries an exact real count.
var exactApplicantsRe = regexp.MustCompile(`^(\d+)( applicants?)$`)

// perturbCount moves an exact "N applicants" count to a nearby different number that
// stays under the "Over 200" bucket; the bucketed shapes come back unchanged.
func perturbCount(core string) string {
	m := exactApplicantsRe.FindStringSubmatch(core)
	if m == nil {
		return core
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return core
	}
	if n+7 < 200 {
		n += 7
	} else {
		n -= 7
	}
	if n < 2 {
		n = 2
	}
	return strconv.Itoa(n) + " applicants"
}
