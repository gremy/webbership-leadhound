// Package brief composes the morning email from the day's ranked jobs. It is pure: the
// caller maps the ranking, the source health and the spend cap into an Input, and Compose
// turns it into a subject, a plain-text body and a minimal HTML body.
package brief

import (
	"cmp"
	"fmt"
	"html"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/leadbrief"
)

// Source states, as the collector's run outcome names them.
const (
	StateOK          = "ok"
	StateParserBroke = "parser_broke"
	StateThrottled   = "throttled"
	StateBlocked     = "blocked"
	StateNotRun      = "not_run" // no run today
)

// Scoring states, as the day's latest score run reads.
const (
	ScoringOK        = "ok"
	ScoringSpendCap  = "spend_cap" // stopped at the daily Jev cap
	ScoringThrottled = "throttled" // the AI provider stayed rate-limited or overloaded
	ScoringFailed    = "failed"    // ended in an error or never finished
	ScoringNotRun    = "not_run"   // no score run today
)

// Recheck states, as the day's latest recheck run reads; any other value is the stop
// reason the recheck ended on, such as parser_broke or throttled.
const (
	RecheckOK     = "ok"
	RecheckFailed = "failed"  // ended in an error or never finished
	RecheckNotRun = "not_run" // no recheck today: it writes no run when nothing is due
)

// Canary outcomes; an empty Canary means it did not run.
const (
	CanaryOK     = "ok"
	CanaryFailed = "failed"
)

// BoardStaleDays is how many days before the brief's day an ATS job may have gone unseen
// on its board and still be listed: past that it is left out (its board may be failing or
// gone) and counted on its source's health line.
const BoardStaleDays = 2

// maxEvidence is how many evidence snippets a job line quotes, and maxSnippetRunes the
// longest one, ellipsis included.
const (
	maxEvidence     = 2
	maxSnippetRunes = 120
)

// Input is everything one brief shows. Apply and YourCall are already ranked and capped.
type Input struct {
	// Day is the brief's calendar day; its location is the one "posted today" is read in.
	Day      time.Time
	Apply    []Job
	YourCall []Job
	RunnerUp *RunnerUp
	// Skipped counts today's jobs left out, by top reason (the scoring skip reasons).
	Skipped map[string]int
	Sources []Source
	// Scoring is the day's scoring state; empty reads as ScoringNotRun.
	Scoring string
	// Recheck is the day's recheck state; empty reads as RecheckNotRun.
	Recheck string
	// RecheckOff says the recheck was skipped because live fetching is off.
	RecheckOff bool
	// Unreachable and Unscored count every job, whatever day it was seen, that would be
	// listed but whose listing could not be reached or whose criteria are not all decided.
	Unreachable, Unscored int
	// ConsoleURL is the console's base URL; the footer links its jobs page. Empty names
	// the track commands instead.
	ConsoleURL string
	// Leads is the leads block, shown after the jobs; nil shows no block.
	Leads *leadbrief.Section
}

// Job is one listed job.
type Job struct {
	ID             int64
	Title          string
	Company        string
	Location       string
	URL            string
	ApplicantCount *int
	PostedAt       *time.Time
	ApplyType      string // ats, offsite, onsite or simple (the effective apply type)
	Criteria       []Criterion
	// Evidence is up to two short lines of the posting that back the verdicts.
	Evidence []string
	// Unclear names what a Your-call job needs judged: each flagged gate and unclear criterion.
	Unclear []Point
}

// Criterion is one criterion's short verdict, such as stack pass (Go, Postgres).
type Criterion struct{ Name, Verdict, Note string }

// Point is one unclear or flagged item, with the snippet it rests on when there is one.
type Point struct{ Name, Snippet string }

// RunnerUp is the best job left out by exactly one failed criterion.
type RunnerUp struct {
	Job
	FailedCriterion string
	Note            string // what that criterion found, such as associate
}

// Source is one source's health today.
type Source struct {
	Name   string // as the member reads it, such as LinkedIn
	State  string
	New    int    // new jobs collected today
	Canary string // CanaryOK, CanaryFailed or empty when it did not run
	// NoCanary marks a source that has no canary, such as an ATS: its line names none.
	NoCanary bool
	// NotSeen counts the source's jobs that would be listed but were left out because
	// their board has not listed them for BoardStaleDays.
	NotSeen int
}

// JobIDs maps the brief's numbers to job ids: number n is JobIDs()[n-1]. Numbering runs
// through Apply and then Your call, so this is what brief_log.job_ids stores.
func (in Input) JobIDs() []int64 {
	ids := make([]int64, 0, len(in.Apply)+len(in.YourCall))
	for _, j := range in.Apply {
		ids = append(ids, j.ID)
	}
	for _, j := range in.YourCall {
		ids = append(ids, j.ID)
	}
	return ids
}

// healthy reports whether every source ran clean, scoring and the recheck finished, and
// no job is waiting on either. A recheck that did not run is no fault: it runs only when
// jobs are due.
func (in Input) healthy() bool {
	if in.Scoring != ScoringOK || in.Unreachable > 0 || in.Unscored > 0 || len(in.Sources) == 0 {
		return false
	}
	if !in.RecheckOff && in.Recheck != RecheckOK && in.Recheck != RecheckNotRun && in.Recheck != "" {
		return false
	}
	for _, s := range in.Sources {
		if s.State != StateOK || s.NotSeen > 0 {
			return false
		}
	}
	return true
}

// Compose renders the brief. Numbers are positions in the email; JobIDs maps them back.
func Compose(in Input) (subject, text, htmlBody string) {
	subject = composeSubject(in)
	var t, h strings.Builder
	h.WriteString("<!doctype html>\n<html><body>\n")
	fmt.Fprintf(&t, "%s\n\n", subject)
	fmt.Fprintf(&h, "<p><strong>%s</strong></p>\n", esc(subject))

	n := 1
	writeSection := func(title string, jobs []Job, yourCall bool) {
		if len(jobs) == 0 {
			return
		}
		fmt.Fprintf(&t, "%s\n", title)
		fmt.Fprintf(&h, "<h2>%s</h2>\n<ol start=\"%d\">\n", esc(title), n)
		for _, j := range jobs {
			writeJob(&t, &h, n, j, in.Day, yourCall)
			n++
		}
		t.WriteString("\n")
		h.WriteString("</ol>\n")
	}
	writeSection("Apply", in.Apply, false)
	writeSection("Needs your call", in.YourCall, true)

	if n == 1 {
		line := "Nothing listed today, and not everything was checked: see Health below."
		if in.healthy() {
			line = "Nothing new worth applying to today."
		}
		fmt.Fprintf(&t, "%s\n\n", line)
		fmt.Fprintf(&h, "<p>%s</p>\n", esc(line))
	}
	if in.Leads != nil {
		writeLeads(&t, &h, n, *in.Leads)
	}

	var tail []string
	if ru := in.RunnerUp; ru != nil {
		line := fmt.Sprintf("Runner-up: %s · %s · failed only on %s", ru.Title, ru.Company, words(ru.FailedCriterion))
		if ru.Note != "" {
			line += " (" + ru.Note + ")"
		}
		fmt.Fprintf(&t, "%s\n", line)
		fmt.Fprintf(&h, "<p>%s", esc(line))
		if ru.URL != "" {
			fmt.Fprintf(&t, "   Link: %s\n", ru.URL)
			fmt.Fprintf(&h, " · %s", link(ru.URL, "link"))
		}
		h.WriteString("</p>\n")
	}
	if s := skippedLine(in.Skipped); s != "" {
		tail = append(tail, s)
	}
	tail = append(tail, HealthLines(in)...)
	for _, l := range tail {
		fmt.Fprintf(&t, "%s\n", l)
		fmt.Fprintf(&h, "<p>%s</p>\n", esc(l))
	}
	if n > 1 {
		writeFooter(&t, &h, in.ConsoleURL)
	}
	h.WriteString("</body></html>\n")
	return subject, t.String(), h.String()
}

func composeSubject(in Input) string {
	head := "LeadHound · " + in.Day.Format("Mon 2 Jan") + " · "
	var parts []string
	if a := len(in.Apply); a > 0 {
		parts = append(parts, fmt.Sprintf("%d to apply", a))
	}
	switch c := len(in.YourCall); {
	case c == 1:
		parts = append(parts, "1 needs your call")
	case c > 1:
		parts = append(parts, fmt.Sprintf("%d need your call", c))
	}
	switch {
	case len(parts) > 0:
		return head + strings.Join(parts, ", ")
	case in.Leads != nil && in.Leads.Todo():
		// No job to show: the subject names the first leads action instead.
		_, first := leadsItems(*in.Leads)
		return head + first
	case in.healthy():
		return head + "nothing new"
	default:
		return head + "nothing listed, see health"
	}
}

func writeJob(t, h *strings.Builder, n int, j Job, day time.Time, yourCall bool) {
	fields := []string{j.Title, j.Company}
	if j.Location != "" {
		fields = append(fields, j.Location)
	}
	if j.ApplicantCount != nil {
		if *j.ApplicantCount == 1 {
			fields = append(fields, "1 applicant")
		} else {
			fields = append(fields, fmt.Sprintf("%d applicants", *j.ApplicantCount))
		}
	}
	if j.PostedAt != nil {
		fields = append(fields, posted(*j.PostedAt, day))
	}
	if a := applyWording(j.ApplyType); a != "" {
		fields = append(fields, a)
	}
	if yourCall && len(j.Unclear) > 0 {
		var ps []string
		for _, p := range j.Unclear {
			if p.Snippet != "" {
				ps = append(ps, fmt.Sprintf("%s (%q)", words(p.Name), snippet(p.Snippet)))
			} else {
				ps = append(ps, words(p.Name))
			}
		}
		fields = append(fields, "unclear: "+strings.Join(ps, ", "))
	}
	line := strings.Join(fields, " · ")
	fmt.Fprintf(t, "%d. %s\n", n, line)
	fmt.Fprintf(h, "<li>%s", esc(line))

	if why := whyLine(j.Criteria); why != "" {
		fmt.Fprintf(t, "   Why: %s\n", why)
		fmt.Fprintf(h, "<br>Why: %s", esc(why))
	}
	if ev := evidenceLine(j.Evidence); ev != "" {
		fmt.Fprintf(t, "        %s\n", ev)
		fmt.Fprintf(h, "<br>%s", esc(ev))
	}
	if j.URL != "" {
		fmt.Fprintf(t, "   Link: %s\n", j.URL)
		fmt.Fprintf(h, "<br>Link: %s", link(j.URL, j.URL))
	}
	rec, mgr := referralURLs(j.Company)
	fmt.Fprintf(t, "   Find a referral: %s · %s\n", rec, mgr)
	fmt.Fprintf(h, "<br>Find a referral: %s · %s</li>\n", link(rec, "recruiters"), link(mgr, "engineering managers"))
}

// writeLeads writes the leads block decision-first, numbered on from the jobs (leadsItems),
// then one health line. With nothing to act on it is the health line alone.
func writeLeads(t, h *strings.Builder, n int, s leadbrief.Section) {
	health := leadsHealth(s)
	if !s.Todo() {
		line := "Leads: nothing to do · " + health
		fmt.Fprintf(t, "%s\n\n", line)
		fmt.Fprintf(h, "<p>%s</p>\n", esc(line))
		return
	}
	items, _ := leadsItems(s)
	t.WriteString("Leads\n")
	fmt.Fprintf(h, "<h2>Leads</h2>\n<ol start=\"%d\">\n", n)
	for i, it := range items {
		fmt.Fprintf(t, "%d. %s\n", n+i, it)
		fmt.Fprintf(h, "<li>%s</li>\n", esc(it))
	}
	h.WriteString("</ol>\n")
	line := "Leads health: " + health
	fmt.Fprintf(t, "%s\n\n", line)
	fmt.Fprintf(h, "<p>%s</p>\n", esc(line))
}

// leadsItems are the leads actions in decision order: replies to read, other messages to
// read, a sending pause to fix, the batch to approve, the manual items, the emails to check
// by hand. first is the first one in a few words, for the subject.
func leadsItems(s leadbrief.Section) (items []string, first string) {
	add := func(item, short string) {
		if first == "" {
			first = short
		}
		items = append(items, item)
	}
	if n := len(s.Replies); n > 0 {
		add(fmt.Sprintf("Read %s in the outreach mailbox: %s", plural(n, "reply", "replies"), strings.Join(s.Replies, ", ")),
			plural(n, "reply", "replies")+" to read")
	}
	if s.Other > 0 {
		add(fmt.Sprintf("Read %s in the outreach mailbox (not matched to a lead)", plural(s.Other, "other message", "other messages")),
			plural(s.Other, "other message", "other messages")+" to read")
	}
	if s.Paused != "" {
		add(fmt.Sprintf("Sending is paused (%s): fix it, then run leadhound leads senders check", s.Paused), "sending paused")
	}
	if b := s.Batch; b != nil {
		var parts []string
		for _, c := range []struct {
			n         int
			one, many string
		}{{b.Emails, "email", "emails"}, {b.Forms, "form", "forms"}, {b.LinkedIn, "LinkedIn note", "LinkedIn notes"}} {
			if c.n > 0 {
				parts = append(parts, plural(c.n, c.one, c.many))
			}
		}
		verb, short := "Approve batch", fmt.Sprintf("batch %d to approve", b.ID)
		if b.Shadow {
			verb, short = "Read and approve shadow batch", fmt.Sprintf("shadow batch %d to approve", b.ID)
		}
		add(fmt.Sprintf("%s %d (%s): leadhound leads batch show %d", verb, b.ID, strings.Join(parts, ", "), b.ID), short)
	}
	if s.ManualWaiting > 0 {
		add(fmt.Sprintf("Send %s by hand, then mark each: leadhound leads done <message>",
			plural(s.ManualWaiting, "contact form or LinkedIn note", "contact forms and LinkedIn notes")),
			plural(s.ManualWaiting, "manual item", "manual items")+" to send")
	}
	if s.CheckByHand > 0 {
		add(fmt.Sprintf("Check by hand whether %s went out (never resent)", plural(s.CheckByHand, "email", "emails")),
			plural(s.CheckByHand, "email", "emails")+" to check")
	}
	return items, first
}

// leadsHealth is the leads health line: yesterday's sends, bounces and opt-outs and
// yesterday's probe numbers.
func leadsHealth(s leadbrief.Section) string {
	sent := fmt.Sprintf("%d sent", s.Sent)
	if s.ShadowSent > 0 {
		sent += fmt.Sprintf(", %d shadow", s.ShadowSent)
	}
	parts := []string{fmt.Sprintf("%s, %d bounced, %d opted out yesterday", sent, s.Bounced, s.OptedOut)}
	if p := s.Probe; p != nil {
		l := fmt.Sprintf("probe checked %s yesterday", plural(p.Checked, "site", "sites"))
		if p.Drupal == 0 {
			l += ", none on Drupal"
		} else {
			l += fmt.Sprintf(", %d on Drupal", p.Drupal)
			majors := make([]int, 0, len(p.ByMajor))
			for m, c := range p.ByMajor {
				if c > 0 {
					majors = append(majors, m)
				}
			}
			slices.SortFunc(majors, func(a, b int) int {
				// The unknown major (0) comes last.
				if (a == 0) != (b == 0) {
					return cmp.Compare(b, a)
				}
				return cmp.Compare(a, b)
			})
			var by []string
			for _, m := range majors {
				if m == 0 {
					by = append(by, fmt.Sprintf("%d version unknown", p.ByMajor[m]))
				} else {
					by = append(by, fmt.Sprintf("%d on %d", p.ByMajor[m], m))
				}
			}
			if len(by) > 0 {
				l += " (" + strings.Join(by, ", ") + ")"
			}
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, " · ")
}

// plural is "1 reply" or "2 replies".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// whyLine is each criterion's short verdict: stack pass (Go, Postgres) · location pass.
func whyLine(cs []Criterion) string {
	var parts []string
	for _, c := range cs {
		p := words(c.Name) + " " + c.Verdict
		if c.Note != "" {
			p += " (" + c.Note + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

// evidenceLine quotes up to maxEvidence snippets.
func evidenceLine(ev []string) string {
	var parts []string
	for _, e := range ev {
		if len(parts) == maxEvidence {
			break
		}
		if e = strings.TrimSpace(e); e != "" {
			parts = append(parts, `"`+snippet(e)+`"`)
		}
	}
	return strings.Join(parts, " · ")
}

// snippet folds whitespace and shortens s to maxSnippetRunes.
func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxSnippetRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxSnippetRunes-1]) + "…"
}

// posted is the posting day relative to the brief's day, in the brief day's location.
func posted(t, day time.Time) string {
	y, m, d := t.In(day.Location()).Date()
	py := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	by, bm, bd := day.Date()
	days := int(time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC).Sub(py).Hours() / 24)
	switch {
	case days <= 0:
		return "posted today"
	case days == 1:
		return "posted yesterday"
	default:
		return fmt.Sprintf("posted %d days ago", days)
	}
}

func applyWording(t string) string {
	switch t {
	case "ats", "offsite":
		return "apply on their site"
	case "simple":
		return "Easy Apply on LinkedIn"
	case "onsite":
		return "apply on LinkedIn"
	}
	return ""
}

// referralURLs are two LinkedIn people searches built from the company name alone: its
// recruiters and its engineering managers. The member opens them himself.
func referralURLs(company string) (recruiter, manager string) {
	q := func(s string) string {
		// QueryEscape writes a space as "+" and a literal "+" as "%2B", so this is exact.
		return "https://www.linkedin.com/search/results/people/?keywords=" + strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	return q(company + " recruiter"), q(company + " engineering manager")
}

// skipLabels name the scoring skip reasons in plain words.
var skipLabels = map[string]string{
	"status":      "already handled",
	"closed":      "closed",
	"unreachable": "unreachable (retried next brief)",
	"rule":        "your rules",
	"language":    "language",
	"location":    "location",
	"workplace":   "workplace",
	"work_rights": "work rights",
	"criterion":   "criteria",
	"unscored":    "not scored yet",
}

// skippedLine is "Skipped today: 14 (9 language, 3 location, 2 your rules)", largest first,
// or "" when nothing was skipped.
func skippedLine(m map[string]int) string {
	type kv struct {
		label string
		n     int
	}
	var all []kv
	total := 0
	for k, n := range m {
		if n <= 0 {
			continue
		}
		label, ok := skipLabels[k]
		if !ok {
			label = words(k)
		}
		all = append(all, kv{label, n})
		total += n
	}
	if total == 0 {
		return ""
	}
	slices.SortFunc(all, func(a, b kv) int {
		if c := cmp.Compare(b.n, a.n); c != 0 {
			return c
		}
		return cmp.Compare(a.label, b.label)
	})
	parts := make([]string, len(all))
	for i, e := range all {
		parts[i] = fmt.Sprintf("%d %s", e.n, e.label)
	}
	return fmt.Sprintf("Skipped today: %d (%s)", total, strings.Join(parts, ", "))
}

// writeFooter tells the reader how to mark a listed job: on the console's jobs page when
// its URL is set, else with the track commands.
func writeFooter(t, h *strings.Builder, consoleURL string) {
	if consoleURL == "" {
		line := "Mark a job with leadhound track N applied, leadhound track N skip --reason TEXT or leadhound track N maybe, N being its number."
		fmt.Fprintf(t, "%s\n", line)
		fmt.Fprintf(h, "<p>%s</p>\n", esc(line))
		return
	}
	jobs := strings.TrimSuffix(consoleURL, "/") + "/jobs"
	fmt.Fprintf(t, "Mark jobs in the console: %s\n", jobs)
	if p, err := url.Parse(jobs); err != nil || (p.Scheme != "http" && p.Scheme != "https") {
		fmt.Fprintf(h, "<p>Mark jobs in the console: %s</p>\n", esc(jobs))
		return
	}
	fmt.Fprintf(h, "<p>Mark jobs in the console: <a href=\"%s\" rel=\"noreferrer\">%s</a></p>\n", esc(jobs), esc(jobs))
}

// HealthLines is the brief's "Health:" lines, as Compose writes them.
func HealthLines(in Input) []string { return healthLines(in) }

// healthLines is one "Health:" line per source, with the jobs left out as not seen on
// their board, then scoring and recheck.
func healthLines(in Input) []string {
	var out []string
	if len(in.Sources) == 0 {
		out = append(out, "Health: no source ran today")
	}
	for _, s := range in.Sources {
		var l string
		switch s.State {
		case StateOK:
			l = fmt.Sprintf("%s ok · %d new", s.Name, s.New)
			if !s.NoCanary {
				canary := "not run"
				if s.Canary != "" {
					canary = s.Canary
				}
				l += " · canary " + canary
			}
		case StateParserBroke:
			l = fmt.Sprintf("%s parser broke: no %s jobs today until it is fixed", s.Name, s.Name)
		case StateThrottled, StateBlocked:
			l = fmt.Sprintf("%s paused today (%s)", s.Name, s.State)
		case StateNotRun:
			l = s.Name + " did not run"
		default:
			l = fmt.Sprintf("%s did not finish (%s)", s.Name, words(s.State))
		}
		switch {
		case s.NotSeen == 1:
			l += fmt.Sprintf(" · 1 job left out, not seen on its board for %d days", BoardStaleDays)
		case s.NotSeen > 1:
			l += fmt.Sprintf(" · %d jobs left out, not seen on their board for %d days", s.NotSeen, BoardStaleDays)
		}
		out = append(out, "Health: "+l)
	}
	return append(out, scoringLine(in), recheckLine(in))
}

// scoringLine is the "Health:" line for scoring, with the jobs still waiting on it.
func scoringLine(in Input) string {
	var l string
	switch in.Scoring {
	case ScoringOK:
		l = "Scoring ok"
	case ScoringSpendCap:
		l = "Scoring paused: daily Jev cap reached"
	case ScoringThrottled:
		l = "Scoring paused: the AI provider was busy (throttled)"
	case ScoringFailed:
		l = "Scoring failed: jobs it did not reach are scored on the next run"
	default:
		l = "Scoring did not run today"
	}
	if in.Unscored > 0 {
		l += fmt.Sprintf(" · %d not scored yet", in.Unscored)
	}
	return "Health: " + l
}

// recheckLine is the "Health:" line for the recheck, with the jobs it could not reach.
func recheckLine(in Input) string {
	var l string
	switch {
	case in.RecheckOff:
		l = "recheck off (not live)"
	case in.Recheck == RecheckOK:
		l = "recheck ok"
	case in.Recheck == RecheckFailed:
		l = "recheck failed"
	case in.Recheck == RecheckNotRun || in.Recheck == "":
		l = "recheck not run"
	default:
		l = fmt.Sprintf("recheck stopped (%s)", words(in.Recheck))
	}
	if in.Unreachable > 0 {
		l += fmt.Sprintf(" · %d unreachable, retried next brief", in.Unreachable)
	}
	return "Health: " + l
}

// words turns an identifier such as work_rights into plain words.
func words(s string) string { return strings.ReplaceAll(s, "_", " ") }

func esc(s string) string { return html.EscapeString(s) }

// link is an escaped anchor for an http or https URL, or the escaped text alone otherwise.
func link(u, label string) string {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") {
		return esc(label)
	}
	return fmt.Sprintf(`<a href="%s">%s</a>`, esc(u), esc(label))
}
