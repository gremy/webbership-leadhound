//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// roWorld is draftsWorld with northwind.example in Romania (email needs their yes on a call)
// with a phone, RO on in leads.yaml and in the example campaign, whose spec defines no
// call_first of its own (so its RO leads follow campaign.DefaultCallFirst and its example
// templates). It returns the data dir.
func roWorld(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })

	rewrite(t, filepath.Join(dir, "leads.yaml"), "  DE: {channel: form,",
		"  RO: {channel: email, lang: en, tz: Europe/Bucharest, footer: footer-en}\n  DE: {channel: form,")
	cdir := filepath.Join(dir, campaign.Dir)
	if err := os.CopyFS(cdir, os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}
	rewrite(t, filepath.Join(cdir, "drupal-eol", campaign.FileName), "  DE: {email: off, call: off}",
		"  RO: {email: on, call: on, lang: en}\n  DE: {email: off, call: off}")

	batchFixture(t)
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE org SET country = 'RO' WHERE domain = 'northwind.example'`); err != nil {
		t.Fatal(err)
	}
	orgPhone(t, "northwind.example", "+40215550100")
	orgPhone(t, "tailspin.example", "+441632960002")
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	return dir
}

// rewrite replaces old with new in the file at path, failing when old is not there.
func rewrite(t *testing.T, path, old, new string) {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(raw), old, new, 1)
	if out == string(raw) {
		t.Fatalf("%s: %q not found", path, old)
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

type leadRow struct {
	step                              int
	channel, condition, name, subject string
	body                              string
	slots                             []byte
}

func leadRows(t *testing.T, domain string, batch int64) []leadRow {
	t.Helper()
	rs, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT m.step, m.channel, m.condition, coalesce(m.sequence_name, ''), m.subject, m.body_text, m.slots
		 FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 WHERE o.domain = $1 AND m.batch_id = $2 ORDER BY m.step`, domain, batch)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []leadRow
	for rs.Next() {
		var r leadRow
		if err := rs.Scan(&r.step, &r.channel, &r.condition, &r.name, &r.subject, &r.body, &r.slots); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func shapeOf(rows []leadRow) string {
	s := ""
	for _, r := range rows {
		s += fmt.Sprintf("%d:%s:%s:%s ", r.step, r.channel, r.condition, r.name)
	}
	return s
}

const callFirstShape = "0:phone:always:call_first 1:phone:call_not_connected:call_first 2:email:email_consent:call_first 3:email:email_consent:call_first "

// systemRecorder is the fake decider through draftFakeCounting, keeping each lead's system
// prompt and the email steps it was asked for.
type systemRecorder struct {
	draftFakeCounting
	mu     sync.Mutex
	system map[int64]string
	user   map[int64]string
	steps  map[int64][]int
}

func (r *systemRecorder) Write(ctx context.Context, req claude.WriteRequest) (claude.WriteResult, claude.Usage, error) {
	_, _, steps, _, err := parseUserTurn(req.Prompt)
	if err != nil {
		return claude.WriteResult{}, claude.Usage{}, err
	}
	r.mu.Lock()
	if r.system == nil {
		r.system, r.user, r.steps = map[int64]string{}, map[int64]string{}, map[int64][]int{}
	}
	r.system[req.LeadID], r.user[req.LeadID], r.steps[req.LeadID] = req.System, req.Prompt, steps
	r.mu.Unlock()
	return r.draftFakeCounting.Write(ctx, req)
}

// TestCallFirstDefaultSequenceTemplatesAndClaude: a campaign with no call_first of its own
// and an RO lead (email needs their yes on a call) drafts that lead on the default
// call_first sequence from the example call_first templates: step 2 thanks them for the call
// and brings the details as promised, step 3 is a short follow-up, and neither says "tried to
// call". Written by Claude (gate trusted), the same lead is asked for call_first's email
// steps (2 and 3), told the first email follows their yes on a call, and its rows are
// Claude's on the call_first sequence; the GB lead keeps the default sequence.
func TestCallFirstDefaultSequenceTemplatesAndClaude(t *testing.T) {
	dir := roWorld(t)
	if out := mustRun(t, "leads", "campaigns", "check"); !strings.Contains(out, "drupal-eol: warning: "+privacyMissing) {
		t.Fatalf("check does not warn about the missing privacy notice:\n%s", out)
	}
	out := mustRun(t, "leads", "batch", "new")
	ro := leadRows(t, "northwind.example", 1)
	if got := shapeOf(ro); got != callFirstShape {
		t.Fatalf("RO lead rows = %q\nwant %q\nbatch new said:\n%s", got, callFirstShape, out)
	}
	if !strings.Contains(ro[2].body, "Thanks for the call. As promised, the details.") ||
		!strings.Contains(ro[3].body, "A short follow-up to my note after our call.") ||
		ro[3].subject != "Re: "+ro[2].subject {
		t.Fatalf("call_first emails not from the call_first templates:\n%s: %s\n---\n%s: %s", ro[2].subject, ro[2].body, ro[3].subject, ro[3].body)
	}
	for _, r := range ro {
		if strings.Contains(r.body, "tried to call") {
			t.Fatalf("call_first step %d says it tried to call:\n%s", r.step, r.body)
		}
	}
	for _, r := range leadRows(t, "tailspin.example", 1) {
		if r.name != "" {
			t.Fatalf("GB lead step %d on sequence %q, want the default", r.step, r.name)
		}
	}

	mustRun(t, "leads", "batch", "discard", "1")
	c, err := leadstoretest.Q(t).CampaignBySlug(context.Background(), leadstore.DefaultAccountID, campaign.LegacySlug)
	if err != nil {
		t.Fatal(err)
	}
	trustGate(t, c.ID, []int64{leadOf(t, "tailspin.example")})
	setWriter(t, dir, campaign.WriterClaude)
	mustRun(t, "leads", "campaigns", "register")
	rec := &systemRecorder{}
	useCaller(t, rec)
	out = mustRun(t, "leads", "batch", "new")
	ro = leadRows(t, "northwind.example", 2)
	if got := shapeOf(ro); got != callFirstShape {
		t.Fatalf("Claude batch: RO lead rows = %q\nwant %q\nbatch new said:\n%s", got, callFirstShape, out)
	}
	for _, r := range ro {
		var s claudeSlots
		if json.Unmarshal(r.slots, &s) != nil || s.Writer != campaign.WriterClaude {
			t.Fatalf("RO step %d is not Claude's (slots %s)\nbatch new said:\n%s", r.step, r.slots, out)
		}
	}
	lead := leadOf(t, "northwind.example")
	if got := fmt.Sprint(rec.steps[lead]); got != "[2 3]" {
		t.Fatalf("RO lead asked for email steps %s, want [2 3]", got)
	}
	// the role of each step is the user turn's; the system prompt is the campaign's, the same
	// for every lead (one cache entry)
	if u := rec.user[lead]; !strings.Contains(u, "- Step 2: the first email after a call") || strings.Contains(u, "after a call attempt") {
		t.Fatalf("RO lead's user turn does not follow call_first:\n%s", u)
	}
	gb := leadOf(t, "tailspin.example")
	if got := fmt.Sprint(rec.steps[gb]); got != "[0 1 3 4]" {
		t.Fatalf("GB lead asked for email steps %s, want [0 1 3 4]", got)
	}
	if u := rec.user[gb]; !strings.Contains(u, "- Step 3: the follow-up after a call attempt") {
		t.Fatalf("GB lead's user turn has no tried-to-call step:\n%s", u)
	}
	if rec.system[gb] != rec.system[lead] {
		t.Fatal("the RO and GB leads of one campaign got different system prompts")
	}
	if claudeSweep(t) == 0 {
		t.Fatal("no Claude rows swept")
	}
}

// TestDraftsSampleNeverUsesWrongNumber: a sampled Claude draft is asked for the call step's
// talking points only when the lead has a number the batch would dial: a number marked wrong
// is never picked (UsableOrgFacts), so tailspin, whose only number is marked wrong, is
// sampled without talking points and without the call-dependent step 3.
func TestDraftsSampleNeverUsesWrongNumber(t *testing.T) {
	_, cid := draftsWorld(t)
	ctx := context.Background()
	var factID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT f.id FROM org_fact f JOIN org o ON o.id = f.org_id WHERE o.domain = 'tailspin.example' AND f.kind = 'phone'`).Scan(&factID); err != nil {
		t.Fatal(err)
	}
	if _, err := leadstoretest.Q(t).RecordPhoneStatus(ctx, factID, leadstore.PhoneBad, leadstore.PhoneReasonOperator, nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "drafts", "sample", "--campaign", campaign.LegacySlug, "--n", "10", "--decider", "fake")
	gens, err := leadstoretest.Q(t).GenerationsForLead(ctx, leadOf(t, "tailspin.example"), cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) == 0 {
		t.Fatal("tailspin was not sampled")
	}
	for _, g := range gens {
		d, err := outreach.DecodeWrittenDraft(g.Output)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.TalkingPoints) != 0 {
			t.Fatalf("generation %d has %d talking points: the wrong number was picked", g.ID, len(d.TalkingPoints))
		}
		for _, st := range d.Steps {
			if st.Step == 3 {
				t.Fatalf("generation %d holds the call_not_connected step 3: the wrong number was picked", g.ID)
			}
		}
	}
}

// TestCampaignsCheckNamesMissingCallFirstTemplates: a data dir whose campaign has a
// call-first country and no call_first of its own, but lacks the default call_first's email
// templates, is refused by `campaigns check` naming each missing file, never drafted with
// its call-first leads silently dropped.
func TestCampaignsCheckNamesMissingCallFirstTemplates(t *testing.T) {
	dir := roWorld(t)
	if err := os.RemoveAll(filepath.Join(dir, campaign.Dir, "drupal-eol", "templates", "d10", "en", "call_first")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCmdErr(t, "leads", "campaigns", "check")
	if code == 0 {
		t.Fatalf("check passed without the call_first templates:\n%s", out)
	}
	for _, want := range []string{"template d10/en/call_first/2.txt is missing", "template d10/en/call_first/3.txt is missing"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("check does not name %q:\n%s", want, errOut)
		}
	}
}
