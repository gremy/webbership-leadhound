// Package intent classifies a café lead's loyalty intent from its Instagram and website
// signals: the rules intent@1 (plan D6). It is pure: no store, no clock of its own (Now is
// an input) and no imports beyond the standard library, so it stays a leaf the store and
// the commands can both use.
package intent

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// Rules names the rule set; it is stored beside every class it decides.
const Rules = "intent@1"

// The classes.
const (
	Shopping  = "shopping"
	Paper     = "paper"
	JustChose = "just_chose"
	Switch    = "switch"
	Curious   = "curious"
	Cold      = "cold"
	Out       = "out"
)

// Touch kinds.
const (
	KindComment = "comment"
	KindUsertag = "usertag" // the vendor tagged the café in a photo
	KindMention = "mention" // the vendor named @café in a caption
	KindTagged  = "tagged"  // the café's post sits on the vendor's Tagged tab
)

// Jev answers this package reads (their strings live in leadrate, which a leaf cannot import).
const (
	jevNotCoffee  = "not_coffee"
	jevRunsScheme = "runs_scheme"
	jevPaperCard  = "paper_card"
	jevUnnamedApp = "unnamed_app_or_qr"
)

const (
	day          = 24 * time.Hour
	maxAge       = 365 * day // older dated signals are ignored
	doubleAge    = 30 * day  // dated signals this fresh weigh double
	askingAge    = 90 * day  // an asking comment this fresh is Shopping
	choseAge     = 60 * day  // a tag this fresh is Just chose
	chosenHold   = 60 * day  // how long Just chose holds
	maxReason    = 300
	maxQuoteRune = 60
)

// Dated is a vendor signal with its date (a follow's or hint's capture date).
type Dated struct {
	Vendor string
	At     time.Time
}

// Touch is a vendor's touch on the lead: Kind is comment, usertag, mention or tagged.
// Text is the comment's text; an empty Text is never asking.
type Touch struct {
	Vendor, Kind string
	At           time.Time
	Text         string
}

// Inputs are everything the rules read for one café lead.
type Inputs struct {
	Now                             time.Time
	PlaceType                       string
	OwnVendor                       string
	Follows, FollowedBy             []Dated
	Touches                         []Touch
	JevPlace, JevLoyalty, JevVendor string   // real Jev only, "" when none or unclear
	QuestionCues                    []string // from signals.yaml instagram.question_cues; Touch.Text "" = not asking
	RuleVendor                      string
	SiteVendors                     []string
	SiteWord, Prepaid               bool
	Call, CallNote                  string // Call "" = no call
	// CallHeldUntil is a just_chose call's own stored held_until; nil means Now + 60 days.
	CallHeldUntil *time.Time
	VendorName    func(id string) string
}

// Result is the decided class. Held says the class is one intent holds from sends:
// just_chose (until HeldUntil) and an out from the call or rules 2-3; a roastery out is not.
// Rule is the D6 rule that fired (1-11).
type Result struct {
	Class          string
	Overlap, Score int
	Reason         string
	SignalAt       *time.Time
	HeldUntil      *time.Time
	Held           bool
	Rule           int
}

// HeldAt reports whether the result holds the lead from sends at now.
func (r Result) HeldAt(now time.Time) bool {
	if !r.Held {
		return false
	}
	return r.HeldUntil == nil || now.Before(*r.HeldUntil)
}

// Rank orders classes: shopping 1, paper 2, switch 3, curious 4, cold 5, just_chose 6,
// out 7; "" (no row) and anything unknown rank as cold.
func Rank(class string) int {
	switch class {
	case Shopping:
		return 1
	case Paper:
		return 2
	case Switch:
		return 3
	case Curious:
		return 4
	case JustChose:
		return 6
	case Out:
		return 7
	}
	return 5
}

func validClass(c string) bool {
	switch c {
	case Shopping, Paper, JustChose, Switch, Curious, Cold, Out:
		return true
	}
	return false
}

// prepared holds the normalised, sorted, filtered signals.
type prepared struct {
	follows, hints []Dated // distinct vendors, latest capture date, own vendor dropped
	comments       []Touch // fresh (≤365 days) comments, newest first
	tags           []Touch // fresh tag/mention/tagged touches, newest first
	ownTouch       *Touch
	named          []namedSource
}

type namedSource struct{ vendor, source string }

func prepare(in Inputs) prepared {
	var p prepared
	p.follows = distinct(in.Follows, in.OwnVendor)
	p.hints = distinct(in.FollowedBy, in.OwnVendor)

	touches := append([]Touch(nil), in.Touches...)
	sort.Slice(touches, func(i, j int) bool { return touchLess(touches[i], touches[j]) })
	seen := map[Touch]bool{}
	for _, t := range touches {
		if seen[t] || t.Vendor == "" || age(in.Now, t.At) > maxAge {
			continue
		}
		seen[t] = true
		switch {
		case t.Kind == KindComment:
			if t.Vendor != in.OwnVendor {
				p.comments = append(p.comments, t)
			}
		case isTag(t.Kind):
			if in.OwnVendor != "" && t.Vendor == in.OwnVendor {
				if p.ownTouch == nil {
					tc := t
					p.ownTouch = &tc
				}
				continue
			}
			p.tags = append(p.tags, t)
		}
	}

	if namedJev(in.JevVendor) && in.JevVendor != in.OwnVendor {
		p.named = append(p.named, namedSource{in.JevVendor, "Instagram"})
	}
	if in.RuleVendor != "" && in.RuleVendor != in.OwnVendor {
		p.named = append(p.named, namedSource{in.RuleVendor, "profile"})
	}
	sites := append([]string(nil), in.SiteVendors...)
	sort.Strings(sites)
	for i, v := range sites {
		if v == "" || v == in.OwnVendor || (i > 0 && sites[i-1] == v) {
			continue
		}
		p.named = append(p.named, namedSource{v, "website"})
	}
	return p
}

func isTag(kind string) bool {
	return kind == KindUsertag || kind == KindMention || kind == KindTagged
}

func namedJev(v string) bool {
	switch v {
	case "", jevPaperCard, jevUnnamedApp, "none", "unclear":
		return false
	}
	return true
}

// touchLess sorts newest first, then by every field so equal dates order the same way.
func touchLess(a, b Touch) bool {
	if !a.At.Equal(b.At) {
		return a.At.After(b.At)
	}
	if a.Vendor != b.Vendor {
		return a.Vendor < b.Vendor
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Text < b.Text
}

// distinct keeps one entry per vendor with its latest date, sorted by vendor id.
func distinct(in []Dated, own string) []Dated {
	latest := map[string]time.Time{}
	for _, d := range in {
		if d.Vendor == "" || (own != "" && d.Vendor == own) {
			continue
		}
		if at, ok := latest[d.Vendor]; !ok || d.At.After(at) {
			latest[d.Vendor] = d.At
		}
	}
	out := make([]Dated, 0, len(latest))
	for v, at := range latest {
		out = append(out, Dated{v, at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Vendor < out[j].Vendor })
	return out
}

func age(now, at time.Time) time.Duration {
	if a := now.Sub(at); a > 0 {
		return a
	}
	return 0
}

func latestOf(ds []Dated) *time.Time {
	var best *time.Time
	for i := range ds {
		if best == nil || ds[i].At.After(*best) {
			t := ds[i].At
			best = &t
		}
	}
	return best
}

func ptr(t time.Time) *time.Time { return &t }

// Classify applies intent@1: the first rule that fires decides the class.
func Classify(in Inputs) Result {
	p := prepare(in)
	name := func(id string) string {
		if in.VendorName != nil {
			if n := strings.TrimSpace(in.VendorName(id)); n != "" {
				return n
			}
		}
		return id
	}
	cues := foldCues(in.QuestionCues)
	asking := func(t Touch) bool { return isAsking(t.Text, cues) }

	derived := derive(in, p, name, asking)
	derived.Overlap = len(p.follows)
	derived.Score = score(in, p)

	if validClass(in.Call) {
		r := Result{Class: in.Call, Overlap: derived.Overlap, Score: derived.Score, SignalAt: derived.SignalAt, Rule: 1}
		r.Reason = "your call"
		if note := oneLine(in.CallNote); note != "" {
			r.Reason += ": " + note
		}
		r.Reason = clip(r.Reason, maxReason)
		switch in.Call {
		case JustChose:
			if in.CallHeldUntil != nil {
				r.HeldUntil = ptr(*in.CallHeldUntil)
			} else {
				r.HeldUntil = ptr(in.Now.Add(chosenHold))
			}
			r.Held = true
		case Out:
			r.Held = true
		}
		return r
	}
	derived.Reason = clip(oneLine(derived.Reason), maxReason)
	return derived
}

// derive runs rules 2-11.
func derive(in Inputs, p prepared, name func(string) string, asking func(Touch) bool) Result {
	own := in.OwnVendor != "" && (in.RuleVendor == in.OwnVendor || in.JevVendor == in.OwnVendor ||
		contains(in.SiteVendors, in.OwnVendor) || p.ownTouch != nil)
	if own {
		r := Result{Class: Out, Reason: "already a Webbership customer", Held: true, Rule: 2}
		if p.ownTouch != nil {
			r.SignalAt = ptr(p.ownTouch.At)
		}
		return r
	}
	if in.JevPlace == jevNotCoffee {
		return Result{Class: Out, Reason: "Instagram reads it as not a coffee place", Held: true, Rule: 3}
	}
	if in.PlaceType == "roastery" {
		return Result{Class: Out, Reason: "a roastery", Rule: 4}
	}
	for _, c := range p.comments {
		if age(in.Now, c.At) <= askingAge && asking(c) {
			return Result{Class: Shopping, SignalAt: ptr(c.At), Rule: 5,
				Reason: "asked " + name(c.Vendor) + " on " + date(c.At) + ": «" + quote(c.Text) + "»"}
		}
	}
	for _, t := range p.tags {
		if age(in.Now, t.At) <= choseAge {
			return Result{Class: JustChose, SignalAt: ptr(t.At), HeldUntil: ptr(t.At.Add(chosenHold)),
				Held: true, Rule: 6, Reason: tagPhrase(t, name)}
		}
	}
	if len(p.follows) >= 2 {
		return Result{Class: Shopping, SignalAt: latestOf(p.follows), Rule: 7,
			Reason: "follows " + vendorList(p.follows, name)}
	}
	// Every tag left is 61-365 days old: a named vendor in use.
	named := append([]namedSource(nil), p.named...)
	for _, t := range p.tags {
		named = append(named, namedSource{t.Vendor, tagSource(t)})
	}
	switch {
	case in.JevVendor == jevPaperCard:
		return Result{Class: Paper, Rule: 8, Reason: "Instagram shows a paper stamp card"}
	case in.JevVendor == jevUnnamedApp:
		return Result{Class: Paper, Rule: 8, Reason: "Instagram shows an app or QR scheme with no vendor named"}
	case in.JevLoyalty == jevRunsScheme && len(named) == 0:
		return Result{Class: Paper, Rule: 8, Reason: "Instagram shows a loyalty scheme with no vendor named"}
	case in.SiteWord && len(named) == 0:
		return Result{Class: Paper, Rule: 8, Reason: "its website mentions loyalty with no vendor named"}
	}
	if len(named) > 0 {
		r := Result{Class: Switch, Rule: 9, Reason: "runs " + namedList(named, name)}
		if len(p.tags) > 0 {
			r.SignalAt = ptr(p.tags[0].At)
		}
		return r
	}
	var why []string
	var at *time.Time
	if len(p.follows) == 1 {
		why = append(why, "follows "+name(p.follows[0].Vendor))
		at = ptr(p.follows[0].At)
	}
	if len(p.comments) > 0 {
		c := p.comments[0]
		why = append(why, "commented on "+name(c.Vendor)+" on "+date(c.At))
		if at == nil || c.At.After(*at) {
			at = ptr(c.At)
		}
	}
	if in.Prepaid {
		why = append(why, "sells prepaid coffee")
	}
	if len(why) > 0 {
		return Result{Class: Curious, SignalAt: at, Rule: 10, Reason: strings.Join(why, "; ")}
	}
	return Result{Class: Cold, Rule: 11, Reason: "no loyalty signal yet"}
}

// score: follows 2 each, comment 3, tag/mention/tagged 2, hint 1, prepaid 1, each
// named-vendor source 1; dated signals ×2 at 30 days or less.
func score(in Inputs, p prepared) int {
	s := 2*len(p.follows) + len(p.hints) + len(p.named)
	if in.Prepaid {
		s++
	}
	dated := func(at time.Time, w int) int {
		if age(in.Now, at) <= doubleAge {
			return 2 * w
		}
		return w
	}
	for _, c := range p.comments {
		s += dated(c.At, 3)
	}
	for _, t := range p.tags {
		s += dated(t.At, 2)
	}
	return s
}

func tagPhrase(t Touch, name func(string) string) string {
	switch t.Kind {
	case KindMention:
		return name(t.Vendor) + " mentioned it on " + date(t.At)
	case KindTagged:
		return "it tagged " + name(t.Vendor) + " on " + date(t.At)
	}
	return name(t.Vendor) + " tagged it on " + date(t.At)
}

func tagSource(t Touch) string {
	return "tag on " + date(t.At)
}

// namedList renders "Cardora (website)" or "Cardora (Instagram, website) and Stampino (tag on 2 Jun)",
// vendors in first-seen order, each with its sources.
func namedList(ns []namedSource, name func(string) string) string {
	var order []string
	srcs := map[string][]string{}
	for _, n := range ns {
		if _, ok := srcs[n.vendor]; !ok {
			order = append(order, n.vendor)
		}
		if !contains(srcs[n.vendor], n.source) {
			srcs[n.vendor] = append(srcs[n.vendor], n.source)
		}
	}
	parts := make([]string, len(order))
	for i, v := range order {
		parts[i] = name(v) + " (" + strings.Join(srcs[v], ", ") + ")"
	}
	return joinAnd(parts)
}

func vendorList(ds []Dated, name func(string) string) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = name(d.Vendor)
	}
	return joinAnd(parts)
}

func joinAnd(parts []string) string {
	if len(parts) <= 1 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func date(t time.Time) string { return t.Format("2 Jan") }

// quote returns the first 60 characters of a comment on one line, "…" marking a cut.
func quote(s string) string {
	s = oneLine(s)
	r := []rune(s)
	if len(r) <= maxQuoteRune {
		return s
	}
	return strings.TrimSpace(string(r[:maxQuoteRune])) + "…"
}

// oneLine folds every run of whitespace (newlines included) to one space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip cuts s to at most n bytes on a rune boundary, "…" marking a cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	cut := n - len(ell)
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// isAsking: a "?" or a whole folded word among the cues.
func isAsking(text string, cues map[string]bool) bool {
	if text == "" {
		return false
	}
	if strings.Contains(text, "?") {
		return true
	}
	for _, w := range words(text) {
		if cues[w] {
			return true
		}
	}
	return false
}

func foldCues(cues []string) map[string]bool {
	m := make(map[string]bool, len(cues))
	for _, c := range cues {
		for _, w := range words(c) {
			m[w] = true
		}
	}
	return m
}

// words folds Romanian diacritics, lowercases and splits on anything not a letter or digit.
func words(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'ă', 'â':
			r = 'a'
		case 'î':
			r = 'i'
		case 'ș', 'ş':
			r = 's'
		case 'ț', 'ţ':
			r = 't'
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}
