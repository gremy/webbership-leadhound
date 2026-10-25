package brief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadbrief"
)

// leadsCases are the golden inputs with a leads block, one per block shape.
func leadsCases() map[string]Input {
	clean := func(f func(*Input)) Input {
		in := Input{Day: day, Sources: healthyLinkedIn(0), Scoring: ScoringOK, Recheck: RecheckNotRun}
		f(&in)
		return in
	}
	return map[string]Input{
		// Replies and opt-outs to read, a batch, manual items and an email to check, after
		// one job: the leads numbers go on from the jobs.
		"leads_replies": clean(func(in *Input) {
			in.Apply = applyJobs()[:1]
			in.Leads = &leadbrief.Section{
				Replies: []string{"alpha.example", "beta.example"}, Other: 1,
				Batch:         &leadbrief.BatchReady{ID: 12, Emails: 8, Forms: 2, LinkedIn: 1},
				ManualWaiting: 3, Sent: 5, ShadowSent: 2, Bounced: 1, OptedOut: 1, CheckByHand: 1,
				Probe: &leadbrief.ProbeLine{Checked: 120, Drupal: 14, ByMajor: map[int]int{10: 4, 7: 9, 0: 1}},
			}
		}),
		"leads_batch_only": clean(func(in *Input) {
			in.Leads = &leadbrief.Section{Batch: &leadbrief.BatchReady{ID: 3, Emails: 1}}
		}),
		"leads_shadow_batch": clean(func(in *Input) {
			in.Leads = &leadbrief.Section{ShadowSent: 2,
				Batch: &leadbrief.BatchReady{ID: 4, Shadow: true, Emails: 2, Forms: 1}}
		}),
		"leads_paused": clean(func(in *Input) {
			in.Leads = &leadbrief.Section{
				Paused: "dns: dmarc missing", CheckByHand: 2, Other: 3, Sent: 1,
				Probe: &leadbrief.ProbeLine{Checked: 40, ByMajor: map[int]int{}},
			}
		}),
		// Nothing to act on: the block is its health line alone.
		"leads_nothing": clean(func(in *Input) {
			in.Leads = &leadbrief.Section{Sent: 4, Bounced: 1,
				Probe: &leadbrief.ProbeLine{Checked: 1, Drupal: 1, ByMajor: map[int]int{11: 1}}}
		}),
	}
}

func TestLeadsGolden(t *testing.T) {
	for name, in := range leadsCases() {
		t.Run(name, func(t *testing.T) {
			got := render(in)
			path := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path) // #nosec G304 -- a fixed testdata path
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("%s differs from the golden file:\n%s", name, got)
			}
		})
	}
}

// TestLeadsBlockShape: the block comes after the jobs and before the health lines, its
// items in decision order, and a nil section writes no block at all.
func TestLeadsBlockShape(t *testing.T) {
	_, text, html := Compose(leadsCases()["leads_replies"])
	order := []string{"\n1. Senior Go Engineer", "\nLeads\n2. Read 2 replies in the outreach mailbox: alpha.example, beta.example",
		"\n3. Read 1 other message in the outreach mailbox (not matched to a lead)",
		"\n4. Approve batch 12 (8 emails, 2 forms, 1 LinkedIn note): leadhound leads batch show 12",
		"\n5. Send 3 contact forms and LinkedIn notes by hand", "\n6. Check by hand whether 1 email went out",
		"\nLeads health: 5 sent, 2 shadow, 1 bounced, 1 opted out yesterday · probe", "\nHealth: LinkedIn ok"}
	at := 0
	for _, w := range order {
		i := strings.Index(text[at:], w)
		if i < 0 {
			t.Fatalf("text lacks %q after position %d:\n%s", w, at, text)
		}
		at += i + len(w)
	}
	if !strings.Contains(html, "<h2>Leads</h2>\n<ol start=\"2\">\n<li>Read 2 replies") {
		t.Errorf("html block does not number on from the jobs:\n%s", html)
	}
	// The jobs footer still follows, and the leads numbers are not job numbers.
	if ids := leadsCases()["leads_replies"].JobIDs(); len(ids) != 1 {
		t.Errorf("JobIDs = %v, want the one job", ids)
	}

	_, text, html = Compose(leadsCases()["leads_nothing"])
	if strings.Contains(text, "\nLeads\n") || strings.Contains(html, "<h2>Leads") {
		t.Errorf("nothing to do still writes a heading:\n%s", text)
	}
	want := "Leads: nothing to do · 4 sent, 1 bounced, 0 opted out yesterday · probe checked 1 site yesterday, 1 on Drupal (1 on 11)"
	if !strings.Contains(text, "\n"+want+"\n") || !strings.Contains(html, "<p>"+want+"</p>") {
		t.Errorf("collapsed block is not one health line %q:\n%s", want, text)
	}
	// A day with no jobs and a leads block has no jobs footer.
	if strings.Contains(text, "Mark ") {
		t.Errorf("a leads-only day has the jobs footer:\n%s", text)
	}

	for name, in := range cases() {
		_, text, html := Compose(in)
		if strings.Contains(text, "Leads") || strings.Contains(html, "Leads") {
			t.Errorf("%s: a nil section writes a leads block", name)
		}
	}
}

// personLike are the fictional people of the leads fixtures; none may reach a brief.
var personLike = []string{"Ottoline", "Brackwater", "Ilinca", "Vasilescu", "Marten", "Oakhollow"}

// leadsForbidden returns every forbidden string in s: any "@" at all, a person-like token,
// or what the jobs sweep forbids.
func leadsForbidden(s string) []string {
	hits := forbidden(s)
	if strings.Contains(s, "@") {
		hits = append(hits, "@")
	}
	for _, w := range personLike {
		if strings.Contains(s, w) {
			hits = append(hits, w)
		}
	}
	return hits
}

// TestLeadsInvariantSweep: no brief with a leads block, text or HTML, carries an "@" or a
// person-like token.
func TestLeadsInvariantSweep(t *testing.T) {
	for name, in := range leadsCases() {
		if hits := leadsForbidden(render(in)); len(hits) > 0 {
			t.Errorf("%s carries forbidden strings %q", name, hits)
		}
	}
	// The sweep catches what is planted in the section.
	for name, plant := range map[string]func(*leadbrief.Section){
		"at":     func(s *leadbrief.Section) { s.Replies = []string{"info" + "@" + "alpha.example"} },
		"person": func(s *leadbrief.Section) { s.Paused = "Ilinca" + " said no" },
	} {
		in := leadsCases()["leads_replies"]
		plant(in.Leads)
		if len(leadsForbidden(render(in))) == 0 {
			t.Errorf("planted %s was not caught", name)
		}
	}
}

// TestLeadsPausedAndOther: a sending pause and other messages are items to act on, never
// "nothing to do" and never on the health line.
func TestLeadsPausedAndOther(t *testing.T) {
	for name, s := range map[string]leadbrief.Section{
		"paused": {Paused: "dns: dmarc missing"},
		"other":  {Other: 3},
	} {
		in := Input{Day: day, Sources: healthyLinkedIn(0), Scoring: ScoringOK, Leads: &s}
		_, text, _ := Compose(in)
		if strings.Contains(text, "nothing to do") || !strings.Contains(text, "\nLeads\n1. ") {
			t.Errorf("%s is not an item:\n%s", name, text)
		}
		for l := range strings.SplitSeq(text, "\n") {
			if strings.HasPrefix(l, "Leads health:") && (strings.Contains(l, "paused") || strings.Contains(l, "other message")) {
				t.Errorf("%s is on the health line: %s", name, l)
			}
		}
	}
	_, text, _ := Compose(leadsCases()["leads_paused"])
	for _, w := range []string{"1. Read 3 other messages in the outreach mailbox (not matched to a lead)\n",
		"2. Sending is paused (dns: dmarc missing): fix it, then run leadhound leads senders check\n"} {
		if !strings.Contains(text, w) {
			t.Errorf("paused brief lacks %q:\n%s", w, text)
		}
	}
}

// TestLeadsSubject: with no job listed, the subject names the first leads action; with a
// job, or no leads block, it is as before.
func TestLeadsSubject(t *testing.T) {
	for name, want := range map[string]string{
		"leads_replies":      "LeadHound · Fri 23 Oct · 1 to apply",
		"leads_batch_only":   "LeadHound · Fri 23 Oct · batch 3 to approve",
		"leads_shadow_batch": "LeadHound · Fri 23 Oct · shadow batch 4 to approve",
		"leads_paused":       "LeadHound · Fri 23 Oct · 3 other messages to read",
		"leads_nothing":      "LeadHound · Fri 23 Oct · nothing new",
	} {
		if got, _, _ := Compose(leadsCases()[name]); got != want {
			t.Errorf("%s: subject %q, want %q", name, got, want)
		}
	}
	in := leadsCases()["leads_replies"]
	in.Apply = nil
	if got, _, _ := Compose(in); got != "LeadHound · Fri 23 Oct · 2 replies to read" {
		t.Errorf("replies subject %q", got)
	}
	in.Leads = &leadbrief.Section{ManualWaiting: 1}
	if got, _, _ := Compose(in); got != "LeadHound · Fri 23 Oct · 1 manual item to send" {
		t.Errorf("manual subject %q", got)
	}
	in.Leads = &leadbrief.Section{CheckByHand: 2}
	if got, _, _ := Compose(in); got != "LeadHound · Fri 23 Oct · 2 emails to check" {
		t.Errorf("check subject %q", got)
	}
	_, text, _ := Compose(leadsCases()["leads_shadow_batch"])
	if !strings.Contains(text, "\n1. Read and approve shadow batch 4 (2 emails, 1 form): leadhound leads batch show 4\n") {
		t.Errorf("shadow batch item:\n%s", text)
	}
}

func TestLeadsHTMLEscaping(t *testing.T) {
	in := leadsCases()["leads_replies"]
	in.Leads.Replies = []string{"<b>x.example"}
	_, _, html := Compose(in)
	if strings.Contains(html, "<b>x") || !strings.Contains(html, "&lt;b&gt;x.example") {
		t.Errorf("html carries the domain unescaped:\n%s", html)
	}
	if !strings.Contains(html, "leadhound leads done &lt;message&gt;") {
		t.Errorf("html lacks the escaped done command:\n%s", html)
	}
}

// TestLeadsOperatorPaused: the operator's own pause is a numbered item with when it was set
// (the time alone on the brief's day, the date too from an earlier day), never "nothing to
// do" and never on the health line.
func TestLeadsOperatorPaused(t *testing.T) {
	for _, c := range []struct {
		at    time.Time
		since string
	}{
		{time.Date(2026, 10, 23, 7, 5, 0, 0, time.UTC), "since 07:05"},
		{time.Date(2026, 10, 22, 18, 5, 0, 0, time.UTC), "since Thu 22 Oct 18:05"},
	} {
		in := Input{Day: day, Sources: healthyLinkedIn(0), Scoring: ScoringOK, Leads: &leadbrief.Section{OperatorPaused: &c.at, Sent: 2}}
		subject, text, _ := Compose(in)
		want := "\nLeads\n1. Sending is paused (" + c.since + "): resume it in the console or with leadhound leads send resume\n"
		if !strings.Contains(text, want) || strings.Contains(text, "nothing to do") {
			t.Errorf("operator pause is not the leads item %q:\n%s", want, text)
		}
		if !strings.Contains(subject, "sending paused") {
			t.Errorf("subject %q does not name the pause", subject)
		}
		for l := range strings.SplitSeq(text, "\n") {
			if strings.HasPrefix(l, "Leads health:") && strings.Contains(l, "paused") {
				t.Errorf("the pause is on the health line: %s", l)
			}
		}
	}
}
