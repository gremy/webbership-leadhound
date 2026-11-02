//go:build integration

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// linkedInOf adds the operator's LinkedIn company page to domain's org and returns its URL and id.
func linkedInOf(t *testing.T, domain string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	l, err := lq.LeadByID(ctx, leadOf(t, domain))
	if err != nil {
		t.Fatal(err)
	}
	url := "https://linkedin.com/company/" + strings.TrimSuffix(domain, ".example")
	added, err := lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: l.OrgID, Kind: leadstore.RouteLinkedIn, Value: url})
	if err != nil {
		t.Fatal(err)
	}
	return url, added.ID
}

// linkedInLead leaves domain's lead only its LinkedIn page, as a lead the build would have put on
// channel linkedin, and returns the lead and the page.
func linkedInLead(t *testing.T, domain string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	id := leadOf(t, domain)
	l, err := lq.LeadByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	url, route := linkedInOf(t, domain)
	routes, err := lq.RoutesFor(ctx, l.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.ID != route {
			if err := lq.MarkContact(ctx, leadstore.Mark{OrgID: l.OrgID, RouteID: r.ID, Mark: leadstore.MarkRetire}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := lq.SetNewLeadRoute(ctx, id, "linkedin", route); err != nil {
		t.Fatal(err)
	}
	return id, url
}

// draftIt runs Draft it on lead with the fake decider and returns its batch.
func draftIt(t *testing.T, lead int64) int64 {
	t.Helper()
	mustRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
	return latestBatch(t, lead)
}

func latestBatch(t *testing.T, lead int64) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT max(batch_id) FROM outreach_message WHERE lead_id = $1`, lead).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// draftMsg is one row of a batch, for the tests' reading.
type draftMsg struct {
	ID            int64
	Step          int
	Channel, To   string
	State, Body   string
	Writer, Why   string
	GenerationRow int64
}

func batchRows(t *testing.T, batch int64) []draftMsg {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT id, step, channel, to_address, state, body_text, slots FROM outreach_message WHERE batch_id = $1 ORDER BY lead_id, step`, batch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []draftMsg
	for rows.Next() {
		var m draftMsg
		var slots []byte
		if err := rows.Scan(&m.ID, &m.Step, &m.Channel, &m.To, &m.State, &m.Body, &slots); err != nil {
			t.Fatal(err)
		}
		m.Writer, m.Why = draftOrigin(slots)
		var s claudeSlots
		if json.Unmarshal(slots, &s) == nil {
			m.GenerationRow = s.GenerationID
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func draftLeadState(t *testing.T, lead int64) string {
	t.Helper()
	l, err := leadstoretest.Q(t).LeadByID(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	return l.State
}

func storedHash(t *testing.T, batch int64) []byte {
	t.Helper()
	var sum []byte
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT content_sha256 FROM outreach_batch WHERE id = $1`, batch).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

func editCount(t *testing.T, msg int64) int {
	t.Helper()
	return batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM message_edit WHERE message_id = %d`, msg))
}

// editDraft runs leads drafts edit with body, returning its exit code and stderr.
func editDraft(t *testing.T, msg int64, body string) (int, string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	code, _, errOut := leadsRun(t, "leads", "drafts", "edit", fmt.Sprint(msg), "--body-file", f)
	return code, errOut
}

func wantRefused(t *testing.T, what string, code int, errOut, words string) {
	t.Helper()
	if code != 2 || !strings.Contains(errOut, words) {
		t.Errorf("%s: exit %d %q, want refused with %q", what, code, errOut, words)
	}
}

// recCaller counts and keeps every request and answers as the fake, or with answer when set, under
// a real model's name, so a batch may reuse what it wrote.
type recCaller struct {
	mu     sync.Mutex
	system []string
	calls  int
	answer []byte
}

func (c *recCaller) Write(ctx context.Context, r claude.WriteRequest) (claude.WriteResult, claude.Usage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.system = append(c.system, r.System)
	if c.answer != nil {
		return claude.WriteResult{JSON: c.answer}, claude.Usage{Model: claude.ModelSonnet}, nil
	}
	res, _, err := draftFake{}.Write(ctx, r)
	return res, claude.Usage{Model: claude.ModelSonnet}, err
}

// TestDraftItColdStartLinkedIn: the first Draft it on a lead with no batch is a live, manual
// draft_it batch: the note at step 0 and the message at step 1, each naming the page, written by
// Claude's word on the row, its generation on the linkedin channel and out of the gate and the
// Drafts tab. It does not hold up the morning batch.
func TestDraftItColdStartLinkedIn(t *testing.T) {
	_, cid := draftsWorld(t)
	ctx := context.Background()
	lead, url := linkedInLead(t, "contoso.example")
	batch := draftIt(t, lead)
	b, err := leadstoretest.Q(t).BatchByID(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if b.Origin != leadstore.BatchOriginDraftIt || !b.Manual || b.Shadow || b.ApprovedAt != nil {
		t.Fatalf("batch %+v", b)
	}
	rows := batchRows(t, batch)
	if len(rows) != 2 || rows[0].Step != 0 || rows[1].Step != 1 || rows[0].To != url || rows[1].To != url {
		t.Fatalf("rows %+v", rows)
	}
	for _, r := range rows {
		if r.Writer != campaign.WriterClaude || r.Channel != "linkedin" || r.GenerationRow == 0 {
			t.Fatalf("row %+v", r)
		}
	}
	if n := len([]rune(rows[0].Body)); n > 200 {
		t.Fatalf("the note is %d characters", n)
	}
	if got := draftLeadState(t, lead); got != leadstore.LeadBatched {
		t.Fatalf("lead %s", got)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM draft_generation WHERE channel = 'linkedin'`); n != 1 {
		t.Fatalf("%d linkedin generations", n)
	}
	page, err := testBackend(t).DraftSamples(ctx, campaign.LegacySlug, 0)
	if err != nil || len(page.Samples) != 0 || page.Gate.Trusted || !strings.Contains(page.Gate.Summary, "0 of 20") {
		t.Fatalf("drafts tab %+v %v", page, err)
	}
	if _, ok, err := leadstoretest.Q(t).UnapprovedBatch(ctx, leadstore.DefaultAccountID, false); err != nil || ok {
		t.Fatalf("a Draft it batch blocks the morning batch: %v %v", ok, err)
	}
	_ = cid
}

// TestManualGenerationsStayOutOfTheGate: a reviewed manual generation moves neither the gate, its
// examples nor the Drafts tab; it is an example of its own channel alone.
func TestManualGenerationsStayOutOfTheGate(t *testing.T) {
	_, cid := draftsWorld(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	lead := leadOf(t, "contoso.example")
	cost := int64(1)
	id, err := lq.RecordGeneration(ctx, leadstore.GenerationRow{LeadID: lead, CampaignID: cid, Attempt: 1, Model: claude.ModelSonnet,
		Output: []byte(`{"note":{"paragraphs":[{"text":"I write websites.","cites":["S1"]}]}}`), CheckOK: true, CostMicros: &cost,
		Purpose: leadstore.PurposeSample, Channel: "linkedin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lq.RecordReview(ctx, id, leadstore.VerdictApprove, nil, "", leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	st, err := draftGate(ctx, lq, cid)
	if err != nil || strings.Contains(st.Summary(), "1 of 20") {
		t.Fatalf("gate %q %v", st.Summary(), err)
	}
	ex, approved, err := approvedExamples(ctx, lq, cid)
	if err != nil || len(ex) != 0 || len(approved) != 0 {
		t.Fatalf("email examples %v %v %v", ex, approved, err)
	}
	if ex, err := approvedManualExamples(ctx, lq, cid, "linkedin"); err != nil || len(ex) != 1 {
		t.Fatalf("linkedin examples %v %v", ex, err)
	}
	if ex, err := approvedManualExamples(ctx, lq, cid, "form"); err != nil || len(ex) != 0 {
		t.Fatalf("form examples %v %v", ex, err)
	}
	page, err := testBackend(t).DraftSamples(ctx, campaign.LegacySlug, 0)
	if err != nil || len(page.Samples) != 0 {
		t.Fatalf("drafts tab %+v %v", page.Samples, err)
	}
}

// TestDraftItFormLeadSteps (Decision 10): a form lead gets its form text at step 0, and the LinkedIn
// note and message at steps 1 and 2 only with a LinkedIn route that is not retired; each names its
// own target.
func TestDraftItFormLeadSteps(t *testing.T) {
	draftsWorld(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	lead := leadOf(t, "contoso.example")
	batch := draftIt(t, lead)
	if rows := batchRows(t, batch); len(rows) != 1 || rows[0].Channel != "form" || rows[0].To != "https://contoso.example/contact" {
		t.Fatalf("no LinkedIn route: %+v", rows)
	}
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(batch))
	if got := draftLeadState(t, lead); got != leadstore.LeadNew {
		t.Fatalf("discard left the lead %s", got)
	}
	url, route := linkedInOf(t, "contoso.example")
	batch = draftIt(t, lead)
	rows := batchRows(t, batch)
	if len(rows) != 3 || rows[1].Channel != "linkedin" || rows[1].Step != 1 || rows[2].Step != 2 || rows[1].To != url || rows[2].To != url {
		t.Fatalf("with a route: %+v", rows)
	}
	wantLines(t, "show", mustRun(t, "leads", "batch", "show", fmt.Sprint(batch)), "LinkedIn note · message", "LinkedIn message after they accept · message")
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(batch))
	l, err := lq.LeadByID(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if err := lq.MarkContact(ctx, leadstore.Mark{OrgID: l.OrgID, RouteID: route, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	if rows := batchRows(t, draftIt(t, lead)); len(rows) != 1 {
		t.Fatalf("a retired route: %+v", rows)
	}
}

// TestBatchNewManualLeads: the morning batch drafts what CandidateLeads offers: a LinkedIn lead (its
// note and message) and a form lead with a page, none of it from "channel has no drafts"; it counts
// one note per lead and its template rows say why.
func TestBatchNewManualLeads(t *testing.T) {
	leadsScene(t)
	lead, url := linkedInLead(t, "tailspin.example")
	formURL, _ := linkedInOf(t, "contoso.example")
	out := mustRun(t, "leads", "batch", "new")
	if strings.Contains(out, "has no drafts") {
		t.Fatalf("batch new:\n%s", out)
	}
	wantLines(t, "batch new", out, "1 contact form, 2 LinkedIn notes", "from templates: Claude drafting off for this run (2)")
	batch := latestBatch(t, lead)
	var mine, form []draftMsg
	for _, r := range batchRows(t, batch) {
		switch {
		case r.To == url:
			mine = append(mine, r)
		case r.To == formURL || strings.HasPrefix(r.To, "https://contoso.example"):
			form = append(form, r)
		}
	}
	if len(mine) != 2 || mine[0].Step != 0 || mine[1].Step != 1 || mine[0].Why != fbOff {
		t.Fatalf("linkedin lead rows %+v", mine)
	}
	if len(form) != 3 || form[0].Channel != "form" || form[2].Step != 2 || form[2].Why != fbOff {
		t.Fatalf("form lead rows %+v", form)
	}
}

// TestBatchNewNoteOverLimit: a template note over the limit neither stores the note nor stops the
// batch: the LinkedIn lead is dropped with the length named, and the form lead keeps its form text.
func TestBatchNewNoteOverLimit(t *testing.T) {
	dir := leadsScene(t)
	lead, _ := linkedInLead(t, "tailspin.example")
	formURL, _ := linkedInOf(t, "contoso.example")
	setNoteMax(t, dir, 10)
	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new", out, "tailspin.example", "the note is", "at most 10")
	if got := draftLeadState(t, lead); got != leadstore.LeadNew {
		t.Fatalf("the dropped lead is %s", got)
	}
	form := leadOf(t, "contoso.example")
	rows := batchRows(t, latestBatch(t, form))
	for _, r := range rows {
		if r.Channel == "linkedin" || r.To == formURL {
			t.Fatalf("a LinkedIn text was stored over the limit: %+v", rows)
		}
	}
	if !slices.ContainsFunc(rows, func(r draftMsg) bool { return r.Channel == "form" }) {
		t.Fatalf("the form lead lost its form text: %+v", rows)
	}
}

// TestBatchReusesManualByChannel: a discarded batch's manual text is reused on its own channel,
// unbilled, and Draft it asks anew; the email channel's rows are not touched by it.
func TestBatchReusesManualByChannel(t *testing.T) {
	leadsScene(t)
	ctx := context.Background()
	c := &recCaller{}
	useCaller(t, c)
	lead := leadOf(t, "contoso.example")
	l, err := leadstoretest.Q(t).LeadByID(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	cid := l.CampaignID
	mustRun(t, "leads", "batch", "new")
	batch := latestBatch(t, lead)
	if c.calls != 1 {
		t.Fatalf("%d calls for one form lead on a template campaign", c.calls)
	}
	gen := batchRows(t, batch)
	first := int64(0)
	for _, r := range gen {
		if r.Channel == "form" {
			first = r.GenerationRow
		}
	}
	if first == 0 {
		t.Fatalf("the form row names no generation: %+v", gen)
	}
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(batch))
	mustRun(t, "leads", "batch", "new")
	if c.calls != 1 {
		t.Fatalf("the reuse called Claude: %d", c.calls)
	}
	gens, err := leadstoretest.Q(t).GenerationsForLead(ctx, lead, cid, "email")
	if err != nil || len(gens) != 0 {
		t.Fatalf("email generations %v %v", gens, err)
	}
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(latestBatch(t, lead)))
	mustRun(t, "leads", "drafts", "make", fmt.Sprint(lead))
	if c.calls != 2 {
		t.Fatalf("Draft it reused: %d calls", c.calls)
	}
}

// TestDraftItTemplateBecause: Claude refusing twice on a LinkedIn lead gives the templates' note,
// every attempt recorded on its channel; the row says why for the lead page. With live calls off
// the reason is that.
func TestDraftItTemplateBecause(t *testing.T) {
	draftsWorld(t)
	lead, _ := linkedInLead(t, "contoso.example")
	saved := newDraftCaller
	mustRun(t, "leads", "drafts", "make", fmt.Sprint(lead)) // live calls off
	batch := latestBatch(t, lead)
	rows := batchRows(t, batch)
	if len(rows) != 2 || rows[0].Writer != campaign.WriterTemplate || rows[0].Why != fbOff {
		t.Fatalf("live off: %+v", rows)
	}
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(batch))

	bad := &recCaller{answer: []byte(`{"note":{"paragraphs":[{"text":"Hello there.","cites":[],"pain_id":""}]},"message":{"paragraphs":[{"text":"Hello again.","cites":[],"pain_id":""}]}}`)}
	useCaller(t, bad)
	out := mustRun(t, "leads", "drafts", "make", fmt.Sprint(lead))
	wantLines(t, "make", out, "written from the templates", fbRefused)
	rows = batchRows(t, latestBatch(t, lead))
	if len(rows) != 2 || rows[0].Why != fbRefused || !strings.Contains(rows[0].Body, "Drupal") {
		t.Fatalf("refused: %+v", rows)
	}
	if bad.calls != 2 {
		t.Fatalf("%d calls, want the try and the retry", bad.calls)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM draft_generation WHERE channel = 'linkedin' AND NOT check_ok`); n != 2 {
		t.Fatalf("%d failed attempts recorded", n)
	}
	_ = saved
}

// TestDraftsFromRequest: a run claims at most five requests and finishes each with its batch or a
// reason: a suppressed, not-new or already-batched lead fails and frees its open slot, and a full
// claim asks for the next run, which takes the rest.
func TestDraftsFromRequest(t *testing.T) {
	dir, _ := draftsWorld(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	batched := leadOf(t, "contoso.example")
	draftIt(t, batched)
	mustRun(t, "leads", "mark-contacted", "tailspin.example", "--note", "called them")
	mustRun(t, "leads", "suppress", "add", "address", "office@litware.example", "--reason", "opt_out")
	ids := map[string]int64{}
	for _, d := range []string{"contoso.example", "tailspin.example", "litware.example", "northwind.example", "wingtip.example", "adatum.example"} {
		id, err := lq.RequestDraft(ctx, leadOf(t, d), "")
		if err != nil {
			t.Fatal(err)
		}
		ids[d] = id
	}
	mustRun(t, "leads", "drafts", "make", "--from-request", "--decider", "fake")
	if n := batchCount(t, `SELECT count(*)::int FROM draft_request WHERE status = 'queued'`); n != 1 {
		t.Fatalf("%d left queued, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, runRequestDir, draftItAction)); err != nil {
		t.Fatalf("a full claim did not ask for the next run: %v", err)
	}
	mustRun(t, "leads", "drafts", "make", "--from-request", "--decider", "fake")
	for d, words := range map[string]string{"contoso.example": "already in batch", "tailspin.example": "Already contacted", "litware.example": "suppressed"} {
		r, err := lq.DraftRequestFor(ctx, leadOf(t, d))
		if err != nil {
			t.Fatal(err)
		}
		if r.ID != ids[d] || r.Status != leadstore.DraftRequestFailed || !strings.Contains(r.Reason, words) || r.BatchID != nil {
			t.Errorf("%s: %+v, want failed with %q", d, r, words)
		}
		if _, err := lq.RequestDraft(ctx, leadOf(t, d), ""); err != nil {
			t.Errorf("%s still holds the open slot: %v", d, err)
		}
	}
	r, err := lq.DraftRequestFor(ctx, leadOf(t, "northwind.example"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != leadstore.DraftRequestDone || r.BatchID == nil || *r.BatchID != latestBatch(t, leadOf(t, "northwind.example")) {
		t.Fatalf("northwind: %+v", r)
	}
}

// TestDraftGridOpenRequest: an open request refuses Draft it, naming its time.
func TestDraftGridOpenRequest(t *testing.T) {
	draftsWorld(t)
	lead := leadOf(t, "contoso.example")
	if _, err := leadstoretest.Q(t).RequestDraft(context.Background(), lead, ""); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
	wantRefused(t, "open request", code, errOut, "already asked for at ")
}

// TestDraftGridEmailUnapproved: an email in an unapproved Draft it batch edits and re-hashes, the new
// hash approves and the old does not; Mark sent is refused for an email; Draft it is refused while it
// is open; Discard works and the first Draft it after that is fine. Then approved: nothing edits.
func TestDraftGridEmailUnapproved(t *testing.T) {
	draftsWorld(t)
	lead := leadOf(t, "northwind.example")
	batch := draftIt(t, lead)
	rows := batchRows(t, batch)
	first := rows[0]
	if first.Channel != "email" || first.Writer != campaign.WriterTemplate {
		t.Fatalf("rows %+v", rows)
	}
	code, _, errOut := leadsRun(t, "leads", "done", fmt.Sprint(first.ID))
	wantRefused(t, "done on an email", code, errOut, "is an email")
	code, _, errOut = leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
	wantRefused(t, "second Draft it", code, errOut, "already in batch")

	old := hex.EncodeToString(storedHash(t, batch))[:12]
	body := strings.Replace(first.Body, "Hello", "Good morning", 1)
	if code, errOut := editDraft(t, first.ID, body); code != 0 {
		t.Fatalf("edit: %d %s", code, errOut)
	}
	if got := batchRows(t, batch)[0].Body; got != body {
		t.Fatalf("the row kept %q", got)
	}
	now := hex.EncodeToString(storedHash(t, batch))[:12]
	if now == old || editCount(t, first.ID) != 1 {
		t.Fatalf("hash %s (was %s), %d edits", now, old, editCount(t, first.ID))
	}
	code, _, errOut = leadsRun(t, "leads", "batch", "approve", fmt.Sprint(batch), "--hash", old)
	wantRefused(t, "old hash", code, errOut, "does not match")
	mustRun(t, "leads", "batch", "approve", fmt.Sprint(batch), "--hash", now)

	code, errOut = editDraft(t, first.ID, body+" More.")
	wantRefused(t, "edit after approval", code, errOut, "approved")
	code, _, errOut = leadsRun(t, "leads", "done", fmt.Sprint(first.ID))
	wantRefused(t, "done after approval", code, errOut, "is an email")
	code, _, errOut = leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
	wantRefused(t, "Draft it after approval", code, errOut, "already in batch")
}

// TestDraftGridDiscardAndAgain: Discard returns an unapproved email Draft it to new, and Draft it
// works again.
func TestDraftGridDiscardAndAgain(t *testing.T) {
	draftsWorld(t)
	lead := leadOf(t, "northwind.example")
	batch := draftIt(t, lead)
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(batch))
	if got := draftLeadState(t, lead); got != leadstore.LeadNew {
		t.Fatalf("lead %s", got)
	}
	if again := draftIt(t, lead); again == batch {
		t.Fatal("no new batch")
	}
}

// TestDraftGridManualFollowUp is C3's walk: a note in an unapproved Draft it batch edits and marks
// sent, which approves the batch and moves the lead to contacted; the message after they accept is
// now approved, edits, and marks sent on the contacted lead; done, it refuses both; Draft it says
// already contacted. A replied lead refuses everything.
func TestDraftGridManualFollowUp(t *testing.T) {
	draftsWorld(t)
	lead, _ := linkedInLead(t, "contoso.example")
	batch := draftIt(t, lead)
	rows := batchRows(t, batch)
	note, message := rows[0], rows[1]

	code, errOut := editDraft(t, note.ID, note.Body+" "+strings.Repeat("Long ", 60))
	wantRefused(t, "note over the limit", code, errOut, "at most 200")
	if editCount(t, note.ID) != 0 {
		t.Fatal("a refused edit wrote")
	}
	before := storedHash(t, batch)
	if code, errOut := editDraft(t, note.ID, "I build sites for organisations like yours."); code != 0 {
		t.Fatalf("note edit: %d %s", code, errOut)
	}
	if string(storedHash(t, batch)) != string(before) || batchRows(t, batch)[0].Body != note.Body || editCount(t, note.ID) != 1 {
		t.Fatal("a manual edit moved the row or the hash")
	}

	wantLines(t, "done", mustRun(t, "leads", "done", fmt.Sprint(note.ID)), "lead "+fmt.Sprint(lead)+" is now contacted")
	if got := draftLeadState(t, lead); got != leadstore.LeadContacted {
		t.Fatalf("lead %s", got)
	}
	rows = batchRows(t, batch)
	if rows[0].State != "done" || rows[1].State != leadstore.MsgApproved {
		t.Fatalf("after the note: %+v", rows)
	}
	code, errOut = editDraft(t, note.ID, "Too late.")
	wantRefused(t, "edit a done note", code, errOut, "can no longer be edited")
	code, _, errOut = leadsRun(t, "leads", "done", fmt.Sprint(note.ID))
	wantRefused(t, "done twice", code, errOut, "already be marked sent")

	if code, errOut := editDraft(t, message.ID, message.Body+"\n\nHappy to talk."); code != 0 {
		t.Fatalf("message edit on a contacted lead: %d %s", code, errOut)
	}
	code, _, errOut = leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
	wantRefused(t, "Draft it, contacted", code, errOut, "Already contacted")
	wantLines(t, "done message", mustRun(t, "leads", "done", fmt.Sprint(message.ID)), "stays contacted")
	if got := batchRows(t, batch)[1].State; got != "done" {
		t.Fatalf("message %s", got)
	}
}

// TestDraftGridAnswered: replied, bounced and opted out refuse Edit, Mark sent and Draft it, even on
// a message still approved.
func TestDraftGridAnswered(t *testing.T) {
	for _, state := range []string{leadstore.LeadReplied, leadstore.LeadBounced, leadstore.LeadOptedOut} {
		t.Run(state, func(t *testing.T) {
			draftsWorld(t)
			lead, _ := linkedInLead(t, "contoso.example")
			batch := draftIt(t, lead)
			rows := batchRows(t, batch)
			mustRun(t, "leads", "done", fmt.Sprint(rows[0].ID))
			ownerExec(t, `UPDATE lead SET state = $2 WHERE id = $1`, lead, state)
			code, errOut := editDraft(t, rows[1].ID, "New words.")
			wantRefused(t, "edit", code, errOut, "replied, bounced or opted out")
			code, _, errOut = leadsRun(t, "leads", "done", fmt.Sprint(rows[1].ID))
			wantRefused(t, "done", code, errOut, "replied, bounced or opted out")
			code, _, errOut = leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
			wantRefused(t, "Draft it", code, errOut, "Already contacted")
		})
	}
}

// TestDraftGridShadowBatch: in a shadow batch Edit works, Mark sent is refused and Discard works.
func TestDraftGridShadowBatch(t *testing.T) {
	leadsScene(t)
	ctx := context.Background()
	lead := leadOf(t, "contoso.example")
	mustRun(t, "leads", "batch", "new")
	batch := latestBatch(t, lead)
	var form draftMsg
	for _, r := range batchRows(t, batch) {
		if r.Channel == "form" {
			form = r
		}
	}
	if code, errOut := editDraft(t, form.ID, form.Body+"\n\nThank you."); code != 0 {
		t.Fatalf("edit: %d %s", code, errOut)
	}
	b := testBackend(t)
	_, err := markManualSent(ctx, b.s, form.ID, hex.EncodeToString(storedHash(t, batch))[:12])
	if !errors.Is(err, leadstore.ErrShadowMarkSent) || manualSentWords(err) == "" {
		t.Fatalf("mark sent in a shadow batch: %v", err)
	}
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(batch))
	if got := draftLeadState(t, lead); got != leadstore.LeadNew {
		t.Fatalf("lead %s", got)
	}
}

// TestDraftGridApprovedMorningBatch: a form text in an approved live morning batch carrying emails
// edits with the batch hash untouched, marks sent, and refuses a new Draft it.
func TestDraftGridApprovedMorningBatch(t *testing.T) {
	manualScene(t)
	lead := leadOf(t, "contoso.example")
	mustRun(t, "leads", "batch", "new")
	batch := latestBatch(t, lead)
	sum := storedHash(t, batch)
	mustRun(t, "leads", "batch", "approve", fmt.Sprint(batch), "--hash", hex.EncodeToString(sum)[:12])
	var form draftMsg
	emails := 0
	for _, r := range batchRows(t, batch) {
		switch r.Channel {
		case "form":
			form = r
		case "email":
			emails++
		}
	}
	if emails == 0 || form.State != leadstore.MsgApproved {
		t.Fatalf("batch: %d emails, form %+v", emails, form)
	}
	if code, errOut := editDraft(t, form.ID, form.Body+"\n\nThank you."); code != 0 {
		t.Fatalf("edit: %d %s", code, errOut)
	}
	if string(storedHash(t, batch)) != string(sum) || batchRows(t, batch)[0].Body == form.Body+"\n\nThank you." {
		t.Fatal("the edit touched the batch")
	}
	show := mustRun(t, "leads", "batch", "show", fmt.Sprint(batch))
	if strings.Contains(show, "Batch changed") {
		t.Fatalf("the batch reads as tampered:\n%s", show)
	}
	code, _, errOut := leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
	wantRefused(t, "Draft it", code, errOut, "already in batch")
	wantLines(t, "done", mustRun(t, "leads", "done", fmt.Sprint(form.ID)), "is now contacted")
}

// TestDraftEditRefusals: the first edit on a template draft has no generation row and works; a text
// with an opt-out word, a banned word, an unlisted link or nothing at all is refused and nothing is
// written, on each channel. A text unchanged writes nothing.
func TestDraftEditRefusals(t *testing.T) {
	dir, _ := draftsWorld(t)
	_ = dir
	lead := leadOf(t, "contoso.example")
	mustRun(t, "leads", "drafts", "make", fmt.Sprint(lead)) // live calls off: the templates
	form := batchRows(t, latestBatch(t, lead))[0]
	if form.GenerationRow != 0 || form.Why == "" {
		t.Fatalf("not a template draft: %+v", form)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM draft_generation`); n != 0 {
		t.Fatalf("%d generation rows", n)
	}
	for what, tc := range map[string]struct{ body, words string }{
		"opt-out word": {form.Body + "\n\nReply unsubscribe.", "opt-out word"},
		"unlisted":     {strings.TrimRight(form.Body, "\n") + "\n\nSee https://nowhere.example/page", "nowhere.example"},
		"empty":        {"  ", "empty"},
	} {
		code, errOut := editDraft(t, form.ID, tc.body)
		wantRefused(t, what, code, errOut, tc.words)
		if editCount(t, form.ID) != 0 {
			t.Fatalf("%s wrote an edit", what)
		}
	}
	wantLines(t, "same text", mustRun(t, "leads", "drafts", "edit", fmt.Sprint(form.ID), "--body-file", writeTemp(t, form.Body)), "nothing changed")
	if code, errOut := editDraft(t, form.ID, form.Body+"\n\nThank you."); code != 0 {
		t.Fatalf("first edit: %d %s", code, errOut)
	}
	eff, err := leadstoretest.Q(t).EffectiveText(context.Background(), form.ID)
	if err != nil || !eff.Edited || eff.OriginalBody != form.Body || !strings.Contains(eff.Body, "Thank you.") {
		t.Fatalf("effective %+v %v", eff, err)
	}
	code, _, errOut := leadsRun(t, "leads", "drafts", "edit", "999999", "--body-file", writeTemp(t, "x"))
	wantRefused(t, "no message", code, errOut, "no message 999999")
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return f
}

// TestOperatorVersionsScrubbed (Decision 16, G4): an edit naming its own org, domain and person
// reaches another lead's prompt only as "<org>" and "<name>", the latest edit alone, and never
// reaches its draft; a blanked edit reaches nothing.
func TestOperatorVersionsScrubbed(t *testing.T) {
	draftsWorld(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	lead, _ := linkedInLead(t, "contoso.example")
	other, _ := linkedInLead(t, "tailspin.example")
	l, err := lq.LeadByID(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE org SET name = 'Contoso Trading' WHERE id = $1`, l.OrgID)
	if _, err := lq.AddPerson(ctx, leadstore.PersonRow{OrgID: l.OrgID, Name: "Ana Pop", Role: "Head of web"}); err != nil {
		t.Fatal(err)
	}
	batch := draftIt(t, lead)
	message := batchRows(t, batch)[1]
	if code, errOut := editDraft(t, message.ID, "First words for Contoso Trading."); code != 0 {
		t.Fatalf("edit: %d %s", code, errOut)
	}
	if code, errOut := editDraft(t, message.ID, "Hello Ana Pop. Contoso Trading on contoso.example is worth an hour, Ana."); code != 0 {
		t.Fatalf("edit: %d %s", code, errOut)
	}

	c := &recCaller{}
	useCaller(t, c)
	mustRun(t, "leads", "drafts", "make", fmt.Sprint(other))
	if len(c.system) != 1 {
		t.Fatalf("%d calls", len(c.system))
	}
	sys := c.system[0]
	if !strings.Contains(sys, "Hello <name>. <org> on <org> is worth an hour, <name>.") {
		t.Fatalf("the version is not in the prompt, scrubbed:\n%s", sys)
	}
	for _, leak := range []string{"Contoso", "contoso", "Ana", "Pop", "First words"} {
		if strings.Contains(sys, leak) {
			t.Fatalf("%q reached another lead's prompt:\n%s", leak, sys)
		}
	}
	for _, r := range batchRows(t, latestBatch(t, other)) {
		if strings.Contains(r.Body, "Contoso") || strings.Contains(r.Body, "Ana") {
			t.Fatalf("it reached the draft: %q", r.Body)
		}
	}

	ownerExec(t, `UPDATE message_edit SET body_after = '' WHERE message_id = $1`, message.ID)
	vs, err := operatorVersions(ctx, lq, l.CampaignID, "linkedin")
	if err != nil || len(vs) != 0 {
		t.Fatalf("a blanked edit: %+v %v", vs, err)
	}
}

// TestDraftItProofBySector: the sheet carries the sector's proofs and no person's name; a public
// proof's page may be written into an edit of that lead's draft and no other page may; a proof of
// another sector stays out.
func TestDraftItProofBySector(t *testing.T) {
	draftsWorld(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	lead := leadOf(t, "contoso.example")
	l, err := lq.LeadByID(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if err := lq.SetSector(ctx, l.OrgID, "education", leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	for _, p := range []leadstore.Proof{
		{Title: "Government", Line: "I work with a ministry.", Sectors: []string{"government"}},
		{Title: "College", Line: "I moved a college to a current version.", URL: "https://secret.example/college", Sectors: []string{"education"}},
		{Title: "University", Line: "I rebuilt a university site.", URL: "https://work.example/university", Sectors: []string{"education"}, Public: true},
	} {
		p.Actor = leadstore.ActorOperator
		if _, err := lq.AddProof(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lq.AddPerson(ctx, leadstore.PersonRow{OrgID: l.OrgID, Name: "Ana Pop", Role: "Head of web"}); err != nil {
		t.Fatal(err)
	}
	batch := draftIt(t, lead)
	var facts string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT facts::text FROM draft_generation WHERE channel = 'form' ORDER BY id DESC LIMIT 1`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"I rebuilt a university site.", "I moved a college", "https://work.example/university"} {
		if !strings.Contains(facts, want) {
			t.Errorf("the sheet lacks %q: %s", want, facts)
		}
	}
	for _, leak := range []string{"ministry", "secret.example", "Ana Pop"} {
		if strings.Contains(facts, leak) {
			t.Errorf("%q is on the sheet: %s", leak, facts)
		}
	}
	form := batchRows(t, batch)[0]
	if code, errOut := editDraft(t, form.ID, form.Body+"\n\nSee https://work.example/university"); code != 0 {
		t.Errorf("the public page: %d %s", code, errOut)
	}
	code, errOut := editDraft(t, form.ID, form.Body+"\n\nSee https://secret.example/college")
	wantRefused(t, "the private page", code, errOut, "secret.example")
	// Another lead's check does not get this lead's page.
	other := leadOf(t, "northwind.example")
	otherForm := batchRows(t, draftIt(t, other))[0]
	code, errOut = editDraft(t, otherForm.ID, otherForm.Body+"\n\nSee https://work.example/university")
	wantRefused(t, "another lead's page", code, errOut, "work.example")
}

// TestDraftItSurvivesSendNow (Decision 8): a Draft it batch older than the confirm window survives
// the sweep and another lead's Send now; choosing its lead in the List refuses, naming its batch and
// page, and discards nothing; its own batch page's Send now plans and confirms.
func TestDraftItSurvivesSendNow(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	batch := draftIt(t, ids[0])
	ownerExec(t, `UPDATE outreach_batch SET created_at = $2 WHERE id = $1`, batch, leadsNow().Add(-31*time.Minute))
	alive := func() {
		t.Helper()
		got, err := leadstoretest.Q(t).BatchByID(ctx, batch)
		if err != nil || got.DiscardedAt != nil {
			t.Fatalf("the Draft it batch: %+v %v", got, err)
		}
	}
	if _, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[1:2]}); err != nil {
		t.Fatal(err)
	}
	alive()
	_, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	words := "Send it from its draft: batch " + strconv.FormatInt(batch, 10) + ", /leads/batches/" + strconv.FormatInt(batch, 10)
	if err == nil || !strings.Contains(err.Error(), words) {
		t.Fatalf("chosen in the List: %v, want %q", err, words)
	}
	alive()
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: []int64{ids[0], ids[1]}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(planLead(p, ids[0]).Stop, words) {
		t.Fatalf("beside another lead: %+v", planLead(p, ids[0]))
	}
	alive()
	own, err := b.SendNowPreview(ctx, console.ManualSendRequest{BatchID: batch})
	if err != nil {
		t.Fatal(err)
	}
	if got := planLead(own, ids[0]); !got.OK || got.Stop != "" {
		t.Fatalf("its own Send now: %+v", got)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(own)); err != nil {
		t.Fatal(err)
	}
}
