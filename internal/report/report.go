// Package report builds the free Drupal upgrade-risk report: one self-contained, print-ready
// A4 HTML page about one lead's site, made only from that lead's stored facts (the same fact
// sheet the writer drafts from) and the operator's fixed wording. Every line names its
// source, and the rendered text runs through the same claim check as a draft
// (outreach.CheckText, G3 + G8), so the page can never carry a number, date, version or URL
// that is not a stored fact.
//
// The report is offered in the first email and sent by the operator when a lead replies; it
// has no public endpoint and is never linked or attached in a cold email (decision 3).
package report

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// Fact labels the report adds to the sheet it renders from, beside outreach's own.
const (
	LabelProbedAt    = "probed_at"    // the probe's date, YYYY-MM-DD
	LabelSenderEmail = "sender_email" // the operator's contact address, when given
	LabelContactURL  = "contact_url"  // the sender's site to reach them at, without an address
)

// Title is the report's name, as the emails offer it.
const Title = "Drupal upgrade-risk report"

// MajorText is the fixed wording for what an update or migration involves for one Drupal
// major: the campaign's offer.report text (DefaultMajorTexts until the spec carries it). It
// is the operator's own wording, so its numbers ("Drupal 11", "PHP 8.3") are allowed in the
// claim check; nothing else on the page may carry a number that is not a fact.
type MajorText struct {
	Heading string   // "What a migration involves"
	Paras   []string // one or more short paragraphs
	Source  string   // where the wording comes from, shown in its source line
}

// DefaultSource is the source line of the default per-major wording: the sender's own
// summary, as the reader sees it.
const DefaultSource = "the sender's own summary of the work"

// The reader-facing source words. The page names its sources in plain public words; the
// stored rows behind them (probe, org, fetch ids) never reach the page.
const (
	sourceEOL    = "Drupal's published support schedule"
	sourceSender = "the sender"
	sourceOrg    = "the organisation's public details"
)

// DefaultMajorTexts is the built-in wording per major: Drupal 6 and 7 → a migration;
// 8 and 9 → the upgrade path; 10 → the update to 11, never a migration.
func DefaultMajorTexts() map[int]MajorText {
	migration := func(m string) MajorText {
		return MajorText{
			Heading: "What a migration involves",
			Paras: []string{
				"Drupal " + m + " cannot be updated in place to a supported version. Moving to Drupal 10 or 11 is a migration: a new site is built on current Drupal, and the content, users and files are carried across with Drupal's own migration tools.",
				"Custom modules and the theme are rebuilt for the new version, and each contributed module is matched to its current release or a replacement. The old site stays live until the new one has been checked, so the switch can be planned for a quiet day.",
			},
			Source: DefaultSource,
		}
	}
	upgrade := func(m, path string) MajorText {
		return MajorText{
			Heading: "What the upgrade path involves",
			Paras: []string{
				"Drupal " + m + " upgrades in place: the site, its content and its theme stay, with no rebuild. The path runs one major version at a time, " + path + ".",
				"Each step updates contributed modules to releases that support the next version, removes deprecated code from custom modules and the theme (the Upgrade Status module lists what needs changing), then updates core with Composer and runs the database updates.",
			},
			Source: DefaultSource,
		}
	}
	return map[int]MajorText{
		6: migration("6"),
		7: migration("7"),
		8: upgrade("8", "from Drupal 8 to 9, then to 10 and 11"),
		9: upgrade("9", "from Drupal 9 to 10, then to 11"),
		10: {
			Heading: "What the update to Drupal 11 involves",
			Paras: []string{
				"Drupal 10 to 11 is an update on the same site, not a rebuild: the content, theme and configuration stay where they are.",
				"The work is to bring contributed modules to releases that support Drupal 11, remove deprecated code from custom modules and the theme (the Upgrade Status module lists what needs changing), meet Drupal 11's hosting requirements such as PHP 8.3, then update core with Composer and run the database updates.",
			},
			Source: DefaultSource,
		},
	}
}

// Input is everything one report is made from, as plain values: the same SheetInput the
// writer drafts from, plus what only the report shows.
type Input struct {
	Sheet         outreach.SheetInput // Finding (with Major) and EOL are required
	ProbedAt      time.Time           // when the finding's probe ran
	Signals       []outreach.TextFact // each stored signal in plain words (SignalWords), with its source
	ContactEmail  string              // the operator's address; "" leaves it out
	ContactSource outreach.Provenance
	// ContactURL is where to reach the sender when there is no ContactEmail (leads.yaml's
	// sender.site_url): the contact line shows it as a link. "" leaves it out.
	ContactURL string
	Major      MajorText        // the fixed text for the lead's major
	Forbid     []*regexp.Regexp // the kind's copy.forbid_by_kind patterns
	Now        time.Time        // the report's date; days left counts from its calendar day
}

// migratRe is always forbidden on a Drupal 10 report: 10 to 11 is an update, not a migration.
var migratRe = regexp.MustCompile(`(?i)migrat`)

//go:embed templates/upgrade_risk.html
var pageSource string

var pageTmpl = template.Must(template.New("upgrade_risk").Parse(pageSource))

// UpgradeRisk renders the report for one lead. It returns an error, and no page, when a
// required fact is missing or when the rendered text holds a claim no stored fact backs or a
// phrase the lead's kind forbids.
func UpgradeRisk(in Input) ([]byte, error) { return render(in, pageTmpl) }

// line is one fact on the page with its source.
type line struct {
	Text   string
	Source string
}

type view struct {
	Title       string
	Org         line // the org's name
	Domain      string
	Prepared    line
	Version     line
	EOL         line
	DaysNum     string
	DaysCaption string
	Days        line
	EOLPassed   bool
	EOLToday    bool
	Major       string // "7"
	EOLText     string
	Probe       line   // the probe's date
	ProbeURL    string // shown as plain text, never a link
	Signals     []line
	MajorText   MajorText
	Sender      []string // the sender's lines; one source note for the block
	SenderSrc   string
	SiteURL     string
	Email       string
	ContactURL  string // the contact line's link when there is no Email
}

func render(in Input, tmpl *template.Template) ([]byte, error) {
	f := in.Sheet.Finding
	switch {
	case f == nil || f.Major <= 0:
		return nil, errors.New("report: the lead has no Drupal finding with a major version")
	case in.Sheet.EOL.IsZero():
		return nil, fmt.Errorf("report: no end-of-life date for Drupal %d", f.Major)
	case in.ProbedAt.IsZero():
		return nil, errors.New("report: the finding has no probe date")
	case in.Now.IsZero():
		return nil, errors.New("report: no report date")
	case strings.TrimSpace(in.Sheet.Sender.Name) == "":
		return nil, errors.New("report: no sender name")
	case len(in.Major.Paras) == 0 || strings.TrimSpace(in.Major.Heading) == "":
		return nil, fmt.Errorf("report: no fixed text for Drupal %d", f.Major)
	}

	si := in.Sheet
	si.LeadExtra = append(append([]outreach.TextFact(nil), si.LeadExtra...),
		outreach.TextFact{Label: LabelProbedAt, Value: outreach.SheetDate(in.ProbedAt, si.Lang), Source: f.Source})
	for _, s := range in.Signals {
		si.LeadExtra = append(si.LeadExtra, outreach.TextFact{Label: outreach.LabelEvidence, Value: s.Value, Source: s.Source})
	}
	if e := strings.TrimSpace(in.ContactEmail); e != "" {
		si.Sender.Extra = append(append([]outreach.TextFact(nil), si.Sender.Extra...),
			outreach.TextFact{Label: LabelSenderEmail, Value: e, Source: in.ContactSource})
	} else if u := strings.TrimSpace(in.ContactURL); u != "" {
		si.Sender.Extra = append(append([]outreach.TextFact(nil), si.Sender.Extra...),
			outreach.TextFact{Label: LabelContactURL, Value: u, Source: outreach.ConfigSource})
	}
	sheet := outreach.BuildFactSheet(si)
	domain := strings.TrimSpace(si.Org.Domain)
	probedAt := longDate(civil(in.ProbedAt))

	// allowed: the operator's fixed wording and the report's date, the values not stored as
	// facts that the page may carry.
	allowed := append([]string{in.Major.Heading}, in.Major.Paras...)
	// src words a provenance for the reader: the scan of the site and its date, the
	// organisation's public details, or the sender; never a stored row's reference.
	src := func(p outreach.Provenance) string {
		switch p.Table {
		case "site_probe", "http_fetch", "fetch", "agency_finding":
			if domain == "" {
				return "a scan of the site on " + probedAt
			}
			return "a scan of " + domain + " on " + probedAt
		case "org":
			return sourceOrg
		}
		return sourceSender
	}
	first := func(label string) (outreach.Fact, bool) {
		fs := sheet.WithLabel(label)
		if len(fs) == 0 {
			return outreach.Fact{}, false
		}
		return fs[0], true
	}
	fl := func(fct outreach.Fact) line { return line{Text: fct.Value, Source: src(fct.Source)} }

	v := view{Title: Title, Major: strconv.Itoa(f.Major)}
	if o, ok := first(outreach.LabelOrgName); ok {
		v.Org = fl(o)
	}
	if d, ok := first(outreach.LabelOrgDomain); ok {
		v.Domain = d.Value
		if v.Org.Text == "" || v.Org.Text == d.Value {
			v.Org, v.Domain = fl(d), "" // the domain is the heading: not printed twice
		}
	}
	if v.Org.Text == "" {
		return nil, errors.New("report: the lead has no organisation name or domain")
	}

	nowDay := civil(in.Now)
	reportDate := nowDay.Format(time.DateOnly)
	allowed = append(allowed, reportDate)
	v.Prepared = line{Text: longDate(nowDay), Source: "the date this report was made"}

	major, _ := first(outreach.LabelMajor)
	if exact, ok := first(outreach.LabelVersion); ok {
		v.Version = line{Text: "Drupal " + exact.Value, Source: src(exact.Source)}
	} else {
		v.Version = line{Text: major.Value + " (the exact version is not shown on the site)", Source: src(major.Source)}
	}

	if _, ok := first(outreach.LabelEOLDate); !ok {
		return nil, fmt.Errorf("report: no end-of-life date for Drupal %d", f.Major)
	}
	eol := civil(si.EOL)
	v.EOLText = longDate(eol)
	v.EOL = line{Text: v.EOLText, Source: sourceEOL}
	days := int(eol.Sub(nowDay).Hours() / 24)
	switch {
	case days > 0:
		v.DaysNum = strconv.Itoa(days)
		v.DaysCaption = plural(days, "day left", "days left")
	case days == 0:
		v.DaysNum, v.DaysCaption, v.EOLToday = "Today", "end of life", true
	default:
		v.DaysNum, v.EOLPassed = strconv.Itoa(-days), true
		v.DaysCaption = plural(-days, "day since end of life", "days since end of life")
	}
	if days != 0 {
		allowed = append(allowed, v.DaysNum)
	}
	v.Days = line{Source: "worked out from the end-of-life date above and this report's date, " + v.Prepared.Text}

	pd, ok := first(LabelProbedAt)
	if !ok {
		return nil, errors.New("report: the finding has no probe date")
	}
	v.Probe = line{Text: probedAt, Source: src(pd.Source)}
	if u, ok := first(outreach.LabelFindingURL); ok {
		v.ProbeURL = u.Value
	}
	seen := map[string]bool{}
	for _, e := range sheet.WithLabel(outreach.LabelEvidence) {
		if seen[e.Value] {
			continue
		}
		seen[e.Value] = true
		v.Signals = append(v.Signals, line{Text: upperFirst(e.Value), Source: src(e.Source)})
	}
	if len(v.Signals) == 0 {
		return nil, errors.New("report: the finding has no signal in plain words")
	}
	v.MajorText = in.Major
	if v.MajorText.Source == "" {
		v.MajorText.Source = DefaultSource
	}

	for _, label := range []string{outreach.LabelSenderName, outreach.LabelSenderRole, outreach.LabelIdentityLine, outreach.LabelProofLine} {
		if fct, ok := first(label); ok {
			v.Sender = append(v.Sender, fct.Value)
		}
	}
	v.SenderSrc = sourceSender
	if s, ok := first(outreach.LabelSenderSite); ok {
		v.SiteURL = s.Value
	}
	if e, ok := first(LabelSenderEmail); ok {
		v.Email = e.Value
	} else if u, ok := first(LabelContactURL); ok {
		v.ContactURL = u.Value
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return nil, fmt.Errorf("report: render: %w", err)
	}
	text, err := VisibleText(buf.Bytes())
	if err != nil {
		return nil, err
	}
	forbid := in.Forbid
	if f.Major >= 10 {
		forbid = append(append([]*regexp.Regexp(nil), forbid...), migratRe)
	}
	if fails := outreach.CheckText(text, sheet, allowed, forbid); len(fails) > 0 {
		msgs := make([]string, len(fails))
		for i, fl := range fails {
			msgs[i] = fl.Code + " " + fl.Msg
		}
		return nil, fmt.Errorf("report: refused, the page would carry what no stored fact backs: %s", strings.Join(msgs, "; "))
	}
	return buf.Bytes(), nil
}

// VisibleText is the text a reader sees on page b, one text node per line, plus every
// link target (a link is a claim too). Styles and scripts are skipped.
func VisibleText(b []byte) (string, error) {
	z := html.NewTokenizer(bytes.NewReader(b))
	var out []string
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				return strings.Join(out, "\n"), nil
			}
			return "", fmt.Errorf("report: read page: %w", z.Err())
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if tok.Data == "style" || tok.Data == "script" {
				skip++
			}
			for _, a := range tok.Attr {
				if a.Key == "href" || a.Key == "src" {
					out = append(out, strings.TrimPrefix(a.Val, "mailto:"))
				}
			}
		case html.EndTagToken:
			if n, _ := z.TagName(); (string(n) == "style" || string(n) == "script") && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				if t := strings.TrimSpace(string(z.Text())); t != "" {
					out = append(out, t)
				}
			}
		}
	}
}

// SignalWords states one stored Drupal signal in plain words for the report: the version
// signals as the drafts word them (outreach.EvidenceLine), the others as what was seen. ok
// is false for a signal that says nothing about Drupal (a bot challenge, say).
func SignalWords(name, value string, major int) (string, bool) {
	if l, err := outreach.EvidenceLine(drupal.Signal{Name: name, Value: value}, major, outreach.LangEnglish); err == nil {
		return l, true
	}
	w, ok := plainSignals[name]
	return w, ok
}

// plainSignals words the signals that name no version. None holds a number, so none can
// carry a claim.
var plainSignals = map[string]string{
	drupal.SigXDrupalCache:   "the site answers with Drupal's X-Drupal-Cache header",
	drupal.SigXDrupalDynamic: "the site answers with Drupal's dynamic page cache header",
	drupal.SigExpires1978:    "the site's Expires header carries the fixed past date Drupal sets",
	drupal.SigDrupalSettings: "the page carries a drupalSettings block, as Drupal pages do",
	drupal.SigCorePaths:      "the page loads files from Drupal's core folder",
	drupal.SigD7Paths:        "the page loads files from the misc and modules folders older Drupal versions use",
	drupal.SigAjaxPageState:  "the page carries Drupal's ajaxPageState settings",
	drupal.SigSystemBaseCSS:  "the page loads Drupal's system base stylesheet",
	drupal.SigMiscDrupalJS:   "the page loads Drupal's core JavaScript file from the misc folder",
	drupal.SigD7Settings:     "the page carries a Drupal settings block",
}

// civil is t's calendar day, as midnight UTC.
func civil(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func longDate(t time.Time) string { return t.Format("2 January 2006") }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
