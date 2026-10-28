package outreach

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// MaxSubjectRunes is the longest subject Check passes.
const MaxSubjectRunes = 70

// Rules a Problem names.
const (
	RuleStep          = "step"           // the email step is outside the sequence
	RuleWords         = "words"          // the copy above the footer is outside the word bounds
	RulePlaceholder   = "placeholder"    // "{{" or "<no value>" is left in the text
	RuleEmptySlot     = "empty-slot"     // a slot the text uses is empty
	RuleSource        = "source"         // a slot the text uses names no stored row
	RuleBanned        = "banned-word"    // a copy.banned_words entry appears
	RuleOptOut        = "opt-out"        // the opt-out line is missing from the email
	RuleOptOutWord    = "opt-out-word"   // an opt-out word appears outside the opt-out line
	RuleIdentity      = "identity"       // the identity line is missing from the email
	RuleNumber        = "number"         // a number traces to neither the template nor a slot
	RuleSubjectLength = "subject-length" // the subject is over MaxSubjectRunes
	RuleSubjectBang   = "subject-bang"   // the subject holds "!"
	RuleURL           = "url"            // a URL other than the finding's or the sender's site
	RuleForbidden     = "forbidden"      // a pattern the campaign forbids for the lead's kind matches
	RuleExclamation   = "exclamation"    // the campaign allows no "!" and the email holds one
)

// stepList is "0, 1, 3, 4".
func stepList(steps []int) string {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = strconv.Itoa(s)
	}
	return strings.Join(parts, ", ")
}

// Problem is one reason a draft may not go out.
type Problem struct {
	Rule   string
	Detail string
}

func (p Problem) String() string { return p.Rule + ": " + p.Detail }

// Check fact-checks a rendered draft against the drafting rules. step is the email's place
// in the sequence (0 is the first email); it is ignored for a form or LinkedIn text.
//
// Every text gets the placeholder, empty-slot, source, banned-word, opt-out-word, number and
// URL rules.
// An email also gets the step, word-bound, subject and footer (opt-out and identity) rules;
// a form or LinkedIn text has no subject or footer and is not held to an email's length.
//
// The number rule: a number is a run of digits, with any further digit runs joined to it
// by a single ".", ":", "/" or "-" (10, 10.6.14, 2026-12-09, 09:30), that touches no
// letter, digit or "_" on either side (so web2 or h2o is a name, not a number); URLs are
// taken out first, as the URL rule owns them. Each number in the subject and body must
// be a number of the template's own text (FixedText), or the whole value of a slot (so
// Major "10" allows 10, while a name such as "Example, since 1998" allows nothing), or a
// number inside a slot leads.yaml writes (SenderName, IdentityLine, OptOutLine, EOLDate),
// which is the operator's own text like the template's, or a number inside EvidenceLine
// that equals the value of the stored signal it was built from (Slots.EvidenceSignal), so
// "at version 10.6.14" passes when the signal is core-asset-v 10.6.14 and fails otherwise.
//
// The URL rule: a URL is "http://" or "https://" up to white space, or a bare "www." name,
// with trailing punctuation dropped. It must be the FindingURL slot, the sender's site_url
// (a trailing "/" ignored either way), or, when bare, the Host or Domain slot; in an email's
// footer it may also be the sender's privacy_url (the privacy line).
func Check(d Draft, step int, cfg leadconfig.Config) []Problem {
	return CheckIn(d, step, cfg, StepRules{})
}

// StepRules are a campaign sequence's rules for Check. The zero value is the legacy
// three-step sequence: email steps 0 to leadconfig.Steps-1 held to copy.first_words (step 0)
// and copy.followup_words (the rest), minimum and maximum.
type StepRules struct {
	// EmailSteps are the sequence's email steps; an email at any other step is refused.
	// nil: the legacy steps.
	EmailSteps []int
	// MaxWords is the campaign's copy.max_words: the most words above the footer per step,
	// with no minimum. A step it leaves out takes the legacy maximum (first_words or
	// followup_words). Used only when EmailSteps is set.
	MaxWords map[int]int
	// BannedWords are the campaign's own banned words, checked beside copy.banned_words.
	BannedWords []string
	// Forbid are patterns the draft's subject and body must not match (the campaign's
	// copy.forbid_by_kind for the lead's kind), as Forbid's text says.
	Forbid []*regexp.Regexp
	// NoLinksInStep0 refuses any URL above the footer of the first email (copy.links_in_step0
	// off): the lowest of EmailSteps (step 2 on a call_first sequence), step 0 when EmailSteps
	// is nil. NoExclamation refuses "!" anywhere in an email (copy.exclamation off).
	NoLinksInStep0, NoExclamation bool
}

// FirstEmail is the step of the lead's first email: the lowest of EmailSteps, 0 when nil.
func (r StepRules) FirstEmail() int {
	if len(r.EmailSteps) == 0 {
		return 0
	}
	return slices.Min(r.EmailSteps)
}

// CheckIn is Check under a campaign sequence's rules (see StepRules).
func CheckIn(d Draft, step int, cfg leadconfig.Config, rules StepRules) []Problem {
	var p []Problem
	add := func(rule, format string, a ...any) {
		p = append(p, Problem{Rule: rule, Detail: fmt.Sprintf(format, a...)})
	}
	text := d.Subject + "\n" + d.Body

	if d.Kind == DraftEmail {
		p = append(p, checkEmail(d, step, cfg, rules)...)
	}
	for _, re := range rules.Forbid {
		if re.MatchString(text) {
			add(RuleForbidden, "the text matches %q, which this campaign forbids for the lead's kind", re.String())
		}
	}

	for _, bad := range []string{"{{", "<no value>"} {
		if strings.Contains(text, bad) {
			add(RulePlaceholder, "the text holds %q", bad)
		}
	}
	for _, name := range d.Used {
		s := d.Slots.field(name)
		if s == nil {
			add(RuleSource, "the text uses %s, which is not a slot", name)
			continue
		}
		if strings.TrimSpace(s.Value) == "" {
			add(RuleEmptySlot, "%s is empty", name)
		}
		switch {
		case s.Source.Table == "":
			add(RuleSource, "%s names no source", name)
		case s.Source != ConfigSource && s.Source.ID <= 0:
			add(RuleSource, "%s names table %s but no row", name, s.Source.Table)
		}
	}
	for _, w := range append(slices.Clone(cfg.Copy.BannedWords), rules.BannedWords...) {
		if checkWordRe(w).MatchString(text) {
			add(RuleBanned, "the text uses %q", w)
		}
	}

	// FO2.4: a reply carries "Re: <our subject>" and may answer inline under our quoted
	// body, which the sync reads as the sender's own words; so our text may hold an opt-out
	// word only in the opt-out line, or a reply would read as an opt-out.
	body := d.Body
	if line := d.Slots.OptOutLine.Value; strings.TrimSpace(line) != "" {
		body = strings.ReplaceAll(body, line, " ")
	}
	switch {
	case mailbox.HasOptOutWord(d.Subject):
		add(RuleOptOutWord, "the subject holds an opt-out word")
	case mailbox.HasOptOutWord(body):
		add(RuleOptOutWord, "the text holds an opt-out word outside the opt-out line")
	}

	allowed := map[string]bool{}
	for _, n := range checkNumbers(d.FixedText) {
		allowed[n] = true
	}
	for _, ns := range d.Slots.named() {
		if ns.name == "Footer" || ns.slot.Value == "" {
			continue
		}
		allowed[ns.slot.Value] = true
		switch ns.name {
		case "SenderName", "IdentityLine", "OptOutLine", "EOLDate":
			for _, n := range checkNumbers(ns.slot.Value) {
				allowed[n] = true
			}
		case "EvidenceLine":
			for _, n := range checkNumbers(ns.slot.Value) {
				if n == d.Slots.EvidenceSignal.Value {
					allowed[n] = true
				}
			}
		}
	}
	for _, n := range checkNumbers(text) {
		if !allowed[n] {
			add(RuleNumber, "%s is in neither the template's text nor a slot", n)
			allowed[n] = true // report each number once
		}
	}

	okURL := map[string]bool{}
	for _, u := range []string{d.Slots.FindingURL.Value, cfg.Sender.SiteURL} {
		if u != "" {
			okURL[checkURLKey(u)] = true
		}
	}
	for _, h := range []string{d.Slots.Host.Value, d.Slots.Domain.Value} {
		if h != "" && !strings.Contains(h, "://") {
			okURL[checkURLKey(h)] = true
		}
	}
	// The privacy notice's URL belongs in the footer only (Slots.PrivacyLine).
	above, footer := text, ""
	if d.Kind == DraftEmail && strings.HasPrefix(d.Body, d.BodyAboveFooter) {
		above, footer = d.Subject+"\n"+d.BodyAboveFooter, strings.TrimPrefix(d.Body, d.BodyAboveFooter)
	}
	privacy := ""
	if cfg.Sender.PrivacyURL != "" {
		privacy = checkURLKey(cfg.Sender.PrivacyURL)
	}
	for _, part := range []struct {
		text   string
		footer bool
	}{{above, false}, {footer, true}} {
		for _, u := range checkURLs(part.text) {
			k := checkURLKey(u)
			if !okURL[k] && (!part.footer || k != privacy) {
				add(RuleURL, "%s is neither the finding's URL nor the sender's site (the privacy notice's URL goes in the footer only)", u)
			}
		}
	}
	return p
}

// checkEmail holds the rules only an email gets.
func checkEmail(d Draft, step int, cfg leadconfig.Config, rules StepRules) []Problem {
	var p []Problem
	add := func(rule, format string, a ...any) {
		p = append(p, Problem{Rule: rule, Detail: fmt.Sprintf(format, a...)})
	}
	bounds, which := cfg.Copy.FollowupWords, "follow-up"
	if step == 0 {
		bounds, which = cfg.Copy.FirstWords, "first email"
	}
	if rules.NoLinksInStep0 && step == rules.FirstEmail() && len(checkURLs(d.BodyAboveFooter)) > 0 {
		add(RuleURL, "the first email holds a link above the footer; this campaign allows none there")
	}
	if rules.NoExclamation && strings.Contains(d.Body, "!") {
		add(RuleExclamation, "the email holds \"!\"; this campaign allows none")
	}
	words := len(strings.Fields(d.BodyAboveFooter))
	switch {
	case rules.EmailSteps != nil && !slices.Contains(rules.EmailSteps, step):
		add(RuleStep, "this email is not one the sequence asks for")
	case rules.EmailSteps != nil:
		most, ok := rules.MaxWords[step]
		if !ok && len(bounds) == 2 {
			most, ok = bounds[1], true
		}
		switch {
		case !ok:
			add(RuleWords, "there is no word limit for this email")
		case words > most:
			add(RuleWords, "%d words above the footer; this email allows at most %d", words, most)
		}
	case step < 0 || step >= leadconfig.Steps:
		add(RuleStep, "step %d is outside 0 to %d", step, leadconfig.Steps-1)
	case len(bounds) != 2:
		add(RuleWords, "copy has no %s word bounds", which)
	default:
		if words < bounds[0] || words > bounds[1] {
			add(RuleWords, "%d words above the footer, a %s needs %d to %d", words, which, bounds[0], bounds[1])
		}
	}
	if n := utf8.RuneCountInString(d.Subject); n > MaxSubjectRunes {
		add(RuleSubjectLength, "the subject is %d characters, at most %d", n, MaxSubjectRunes)
	}
	if strings.Contains(d.Subject, "!") {
		add(RuleSubjectBang, "the subject holds \"!\"")
	}
	for _, l := range []struct {
		rule, name, value string
	}{
		{RuleOptOut, "opt-out", d.Slots.OptOutLine.Value},
		{RuleIdentity, "identity", d.Slots.IdentityLine.Value},
	} {
		switch {
		case strings.TrimSpace(l.value) == "":
			add(l.rule, "the %s line is empty", l.name)
		case !strings.Contains(d.Body, l.value):
			add(l.rule, "the %s line is not in the email", l.name)
		}
	}
	return p
}

// checkWordRe matches w as a whole word, any case.
func checkWordRe(w string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])` + regexp.QuoteMeta(strings.TrimSpace(w)) + `(?:$|[^\p{L}\p{N}_])`)
}

var (
	checkURLRe    = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>"']+`)
	checkNumberRe = regexp.MustCompile(`\d+(?:[.:/-]\d+)*`)
)

// checkURLs lists the URLs in text, trailing punctuation dropped.
func checkURLs(text string) []string {
	var out []string
	for _, u := range checkURLRe.FindAllString(text, -1) {
		out = append(out, strings.TrimRight(u, ".,;:!?)]}"))
	}
	return out
}

func checkURLKey(u string) string { return strings.ToLower(strings.TrimRight(u, "/")) }

// checkNumbers lists the numbers in text, in order (see Check for the definition).
func checkNumbers(text string) []string {
	text = checkURLRe.ReplaceAllStringFunc(text, func(u string) string { return strings.Repeat(" ", len(u)) })
	var out []string
	for _, loc := range checkNumberRe.FindAllStringIndex(text, -1) {
		if loc[0] > 0 {
			if r, _ := utf8.DecodeLastRuneInString(text[:loc[0]]); checkGlued(r) {
				continue
			}
		}
		if loc[1] < len(text) {
			if r, _ := utf8.DecodeRuneInString(text[loc[1]:]); checkGlued(r) {
				continue
			}
		}
		out = append(out, text[loc[0]:loc[1]])
	}
	return out
}

func checkGlued(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
