//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// draftsWorld is the example drupal-eol campaign registered with writer: template, the batch
// fixture's leads built, tailspin with a phone (so one lead has the call step), and the clock
// on a Tuesday morning. It returns the data dir and the campaign id.
func draftsWorld(t *testing.T) (string, int64) {
	t.Helper()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })
	if err := os.CopyFS(filepath.Join(dir, campaign.Dir), os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}
	batchFixture(t)
	orgPhone(t, "tailspin.example", "+441632960002")
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	c, err := leadstoretest.Q(t).CampaignBySlug(context.Background(), leadstore.DefaultAccountID, campaign.LegacySlug)
	if err != nil {
		t.Fatal(err)
	}
	return dir, c.ID
}

// setWriter rewrites the campaign file's writer.
func setWriter(t *testing.T, dir, writer string) {
	t.Helper()
	path := filepath.Join(dir, campaign.Dir, campaign.LegacySlug, campaign.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(raw), "writer: template", "writer: "+writer, 1)
	if out == string(raw) {
		t.Fatal("campaign writer not rewritten")
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

func leadOf(t *testing.T, domain string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = $1`, domain).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedReviews stores n reviewed Claude sample drafts on leads (round robin), approved of them
// approved and the rest rejected, newest last.
func seedReviews(t *testing.T, campaignID int64, leads []int64, n, approved int) {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	sheet := outreach.FactSheet{Facts: []outreach.Fact{{ID: "S1", Label: outreach.LabelSenderName, Value: "Demo Sender", Group: outreach.GroupSender}}}
	cost := int64(100)
	for i := range n {
		id, err := lq.RecordGeneration(ctx, leadstore.GenerationRow{LeadID: leads[i%len(leads)], CampaignID: campaignID, Attempt: 1,
			Model: claude.ModelSonnet, Facts: sheet, Output: []byte(`{"steps":[],"talking_points":[]}`), CheckOK: true,
			CostMicros: &cost, Purpose: leadstore.PurposeSample})
		if err != nil {
			t.Fatal(err)
		}
		verdict, reasons := leadstore.VerdictApprove, []string(nil)
		if i >= approved {
			verdict, reasons = leadstore.VerdictReject, []string{"wrong_tone"}
		}
		if _, err := lq.RecordReview(ctx, id, verdict, reasons, "", leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
}

// trustGate opens the gate: 20 reviewed drafts, all approved, and a passing Claude eval.
func trustGate(t *testing.T, campaignID int64, leads []int64) {
	t.Helper()
	seedReviews(t, campaignID, leads, 20, 20)
	if _, err := leadstoretest.Q(t).RecordEval(context.Background(), leadstore.DraftEvalRow{CampaignID: campaignID, Runs: 20,
		Reviewed: 20, FirstTryPass: 0.95, AfterRetryPass: 1, Decider: leadstore.DeciderClaude}); err != nil {
		t.Fatal(err)
	}
}

// useCaller makes every drafts command and batch write with c (nil: live calls off).
func useCaller(t *testing.T, c outreach.Caller) {
	t.Helper()
	saved := newDraftCaller
	newDraftCaller = func(string, store.Queries, string) (outreach.Caller, error) { return c, nil }
	t.Cleanup(func() { newDraftCaller = saved })
}

// claudeWorld is draftsWorld with the gate trusted and writer: claude registered.
func claudeWorld(t *testing.T) (string, int64) {
	t.Helper()
	dir, cid := draftsWorld(t)
	trustGate(t, cid, []int64{leadOf(t, "northwind.example")})
	setWriter(t, dir, campaign.WriterClaude)
	if out := mustRun(t, "leads", "campaigns", "register"); !strings.Contains(out, "new version") {
		t.Fatalf("register with a trusted gate: %s", out)
	}
	return dir, cid
}

type msgRow struct {
	id, lead, batch, campaign int64
	step                      int
	channel, subject, body    string
	slots                     []byte
}

func batchMessages(t *testing.T, batch int64) []msgRow {
	t.Helper()
	rs, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT m.id, m.lead_id, m.batch_id, b.campaign_id, m.step, m.channel, m.subject, m.body_text, m.slots
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE $1 = 0 OR m.batch_id = $1 ORDER BY m.lead_id, m.step`, batch)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []msgRow
	for rs.Next() {
		var m msgRow
		if err := rs.Scan(&m.id, &m.lead, &m.batch, &m.campaign, &m.step, &m.channel, &m.subject, &m.body, &m.slots); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func isClaude(m msgRow) (claudeSlots, bool) {
	var s claudeSlots
	if json.Unmarshal(m.slots, &s) != nil || s.Writer != campaign.WriterClaude {
		return s, false
	}
	return s, true
}

// claudeSweep is the CP4 invariant over every message row: a row with writer: claude names a
// generation of the same lead and campaign that passed the check, and its text is that
// generation's text: the subject (or "Re: " and the first subject) and every paragraph (or
// talking point) in order. It returns how many Claude rows it checked.
func claudeSweep(t *testing.T) int {
	t.Helper()
	lq := leadstoretest.Q(t)
	n := 0
	for _, m := range batchMessages(t, 0) {
		s, ok := isClaude(m)
		if !ok {
			continue
		}
		n++
		g, err := lq.GenerationByID(context.Background(), s.GenerationID)
		if err != nil {
			t.Fatalf("message %d: generation %d: %v", m.id, s.GenerationID, err)
		}
		if !g.CheckOK || g.LeadID != m.lead || g.CampaignID != m.campaign {
			t.Fatalf("message %d: generation %+v does not back it", m.id, g)
		}
		d, err := outreach.DecodeWrittenDraft(g.Output)
		if err != nil {
			t.Fatalf("message %d: %v", m.id, err)
		}
		var texts []string
		if m.channel == leadstore.ChannelPhone {
			for _, tp := range d.TalkingPoints {
				texts = append(texts, strings.Join(strings.Fields(tp.Text), " "))
			}
		} else {
			first, firstAt := "", -1
			found := false
			for _, st := range d.Steps {
				if firstAt < 0 || st.Step < firstAt {
					first, firstAt = st.Subject, st.Step
				}
			}
			for _, st := range d.Steps {
				if st.Step != m.step {
					continue
				}
				found = true
				if want := cmpOr(st.Subject, "Re: "+first); m.subject != want {
					t.Fatalf("message %d: subject %q, generation %q", m.id, m.subject, want)
				}
				for _, p := range st.Paragraphs {
					texts = append(texts, p.Text)
				}
			}
			if !found {
				t.Fatalf("message %d: step %d is not in generation %d", m.id, m.step, g.ID)
			}
		}
		at := 0
		for _, tx := range texts {
			i := strings.Index(m.body[at:], tx)
			if i < 0 {
				t.Fatalf("message %d: %q is not in its body (in order)", m.id, tx)
			}
			at += i + len(tx)
		}
	}
	return n
}

// draftFakeCounting is the fake decider, counting calls per lead.
type draftFakeCounting struct {
	mu    sync.Mutex
	calls map[int64]int
	// capAfter: once this many leads were answered, every call for another lead is refused
	// by the daily cap (0: never).
	capAfter int
	// refuse: this lead's every call stops with a refusal, billed.
	refuse int64
}

func (f *draftFakeCounting) Write(ctx context.Context, r claude.WriteRequest) (claude.WriteResult, claude.Usage, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[int64]int{}
	}
	if f.capAfter > 0 && f.calls[r.LeadID] == 0 && len(f.calls) >= f.capAfter {
		f.calls[-1]++ // refused calls, no lead answered
		f.mu.Unlock()
		return claude.WriteResult{}, claude.Usage{}, fmt.Errorf("claude: drafting: $3.0000 spent of $3.0000 on 2026-10-27, next call up to $0.0900: %w", ai.ErrSpendCap)
	}
	f.calls[r.LeadID]++
	f.mu.Unlock()
	if r.LeadID == f.refuse {
		return claude.WriteResult{}, claude.Usage{Model: claude.ModelSonnet, InputTokens: 800, OutputTokens: 12}, fmt.Errorf("claude: stop reason refusal: %w", claude.ErrRefused)
	}
	res, _, err := draftFake{}.Write(ctx, r)
	// billed as Claude, so a batch may reuse what it wrote
	return res, claude.Usage{Model: claude.ModelSonnet, InputTokens: 900, OutputTokens: 400}, err
}

func (f *draftFakeCounting) answered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.calls {
		if k > 0 {
			n++
		}
	}
	return n
}

// TestBatchClaudeDraftsInvariant: with the gate trusted and writer: claude, a batch's email
// leads are written by Claude (every attempt a batch generation) and pass the invariant
// sweep; the call card comes from the talking points. Discarded, the next batch with live
// calls off drafts from templates and says why, with the same render tail; the batch after
// that reuses the first batch's passing generations without a call.
func TestBatchClaudeDraftsInvariant(t *testing.T) {
	_, cid := claudeWorld(t)
	fake := &draftFakeCounting{}
	useCaller(t, fake)
	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new", out, "Claude wrote ")
	if strings.Contains(out, "drafted from templates") {
		t.Fatalf("no lead should fall back:\n%s", out)
	}
	n := claudeSweep(t)
	if n == 0 || fake.answered() == 0 {
		t.Fatalf("%d Claude rows, %d leads answered", n, fake.answered())
	}
	var gens, phones int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT count(*) FROM draft_generation WHERE purpose = 'batch' AND campaign_id = $1 AND check_ok`, cid).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if gens != fake.answered() {
		t.Fatalf("%d passing batch generations, %d leads answered", gens, fake.answered())
	}
	b1 := batchMessages(t, 1)
	for _, m := range b1 {
		if _, ok := isClaude(m); ok && m.channel == leadstore.ChannelPhone {
			phones++
		}
	}
	if phones != 1 {
		t.Fatalf("%d Claude call cards, want 1 (tailspin)", phones)
	}

	mustRun(t, "leads", "batch", "discard", "1")
	useCaller(t, nil)
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new (live off)", out, "drafted from templates: "+fbOff)
	b2 := batchMessages(t, 2)
	tails := 0
	for _, m := range b2 {
		if _, ok := isClaude(m); ok {
			t.Fatalf("message %d is Claude's with live calls off", m.id)
		}
		for _, c := range b1 {
			if c.lead == m.lead && c.step == m.step && c.channel == "email" {
				if common := commonSuffix(c.body, m.body); !strings.Contains(common, "Demo Ltd, 1 Example Street") || !strings.Contains(common, "unsubscribe") {
					t.Fatalf("lead %d step %d: the Claude and template tails differ:\n%s", m.lead, m.step, common)
				}
				tails++
			}
		}
	}
	if tails == 0 {
		t.Fatal("no lead to compare tails on")
	}

	mustRun(t, "leads", "batch", "discard", "2")
	again := &draftFakeCounting{}
	useCaller(t, again)
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new (reuse)", out, "Claude wrote ")
	if again.answered() != 0 {
		t.Fatalf("the reuse batch called Claude for %d leads", again.answered())
	}
	if claudeSweep(t) == n {
		t.Fatal("the reuse batch holds no Claude rows")
	}
}

// TestBatchUntrustedUsesTemplates: a writer: claude campaign whose gate closed after it was
// registered (the last 20 reviews rejected) drafts every email lead from templates, says so,
// calls nothing, and the sweep finds no Claude row.
func TestBatchUntrustedUsesTemplates(t *testing.T) {
	_, cid := claudeWorld(t)
	seedReviews(t, cid, []int64{leadOf(t, "northwind.example")}, 20, 0)
	fake := &draftFakeCounting{}
	useCaller(t, fake)
	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new", out, "drafted from templates: "+fbUntrusted)
	if fake.answered() != 0 || claudeSweep(t) != 0 {
		t.Fatalf("untrusted batch: %d leads called, Claude rows present", fake.answered())
	}
}

// TestBatchClaudeCapMidBatch: the daily cap refuses after two leads: those two keep Claude's
// drafts, the rest fall back to templates, the batch says how many and why, and no call is
// tried after the cap.
func TestBatchClaudeCapMidBatch(t *testing.T) {
	claudeWorld(t)
	fake := &draftFakeCounting{capAfter: 2}
	useCaller(t, fake)
	out := mustRun(t, "leads", "batch", "new")
	leads := map[int64]bool{}
	tmpl := map[int64]bool{}
	for _, m := range batchMessages(t, 1) {
		if m.channel != "email" {
			continue
		}
		if _, ok := isClaude(m); ok {
			leads[m.lead] = true
		} else {
			tmpl[m.lead] = true
		}
	}
	if len(leads) != 2 || len(tmpl) == 0 {
		t.Fatalf("Claude leads %d (want 2), template leads %d:\n%s", len(leads), len(tmpl), out)
	}
	wantLines(t, "batch new", out, fmt.Sprintf("%s drafted from templates: %s (%d)", plural(len(tmpl), "lead", "leads"), fbCap, len(tmpl)))
	fake.mu.Lock()
	refused := fake.calls[-1]
	fake.mu.Unlock()
	if refused != 1 {
		t.Fatalf("%d calls refused by the cap, want 1 (no call after the cap)", refused)
	}
	claudeSweep(t)
}

// TestBatchClaudeRefusalFallback: a refusal stop is billed, retried once, recorded as two
// failed generations with the stop named, then that lead falls back to templates, counted.
func TestBatchClaudeRefusalFallback(t *testing.T) {
	_, cid := claudeWorld(t)
	lead := leadOf(t, "litware.example")
	fake := &draftFakeCounting{refuse: lead}
	useCaller(t, fake)
	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new", out, "1 lead drafted from templates: "+fbRefused+" (1)")
	fake.mu.Lock()
	calls := fake.calls[lead]
	fake.mu.Unlock()
	if calls != 2 {
		t.Fatalf("refused lead called %d times, want 2 (the retry)", calls)
	}
	gens, err := leadstoretest.Q(t).GenerationsForLead(context.Background(), lead, cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 2 {
		t.Fatalf("%d generations for the refused lead, want 2", len(gens))
	}
	for _, g := range gens {
		if g.CheckOK || g.Purpose != leadstore.PurposeBatch || g.CostMicros == nil || *g.CostMicros <= 0 ||
			len(g.Failures) != 1 || g.Failures[0].Code != failStop || g.Failures[0].Token != outreach.StopRefusal {
			t.Fatalf("refused generation %+v", g)
		}
	}
	for _, m := range batchMessages(t, 1) {
		if _, ok := isClaude(m); ok && m.lead == lead {
			t.Fatalf("the refused lead has a Claude message %d", m.id)
		}
	}
	claudeSweep(t)
}

// TestDraftsSampleReviewGrid is the review status × operation grid through the commands and
// the console backend: a fake sample writes sample generations (nothing sendable, no batch);
// approve then reject, the latest wins; a refused generation cannot be reviewed; a fake
// draft never counts toward the gate.
func TestDraftsSampleReviewGrid(t *testing.T) {
	_, cid := draftsWorld(t)
	ctx := context.Background()
	out := mustRun(t, "leads", "drafts", "sample", "--campaign", campaign.LegacySlug, "--n", "2", "--decider", "fake")
	wantLines(t, "sample", out, "Estimate: 2 leads with the fake decider", "2 leads drafted, 2 passed the check, nothing sendable")
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 0 {
		t.Fatalf("a sample made %d batches", n)
	}
	again := mustRun(t, "leads", "drafts", "sample", "--campaign", campaign.LegacySlug, "--n", "2", "--decider", "fake")
	wantLines(t, "second sample", again, "2 leads drafted") // two other leads: a sampled lead is not sampled twice

	b := testBackend(t)
	page, err := b.DraftSamples(ctx, campaign.LegacySlug, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Samples) != 4 || !page.Samples[0].Fake || !page.Samples[0].Reviewable || len(page.Samples[0].Steps) == 0 ||
		len(page.Samples[0].Steps[0].Paragraphs[0].Cites) == 0 || page.Samples[0].Steps[0].Paragraphs[0].Cites[0].Value == "" {
		t.Fatalf("drafts page %+v", page)
	}
	if page.Gate.Trusted || !strings.HasPrefix(page.Gate.Summary, "Claude drafts: not trusted yet: 0 of 20 reviewed") {
		t.Fatalf("gate line %q", page.Gate.Summary)
	}
	g := page.Samples[0].GenerationID
	if err := b.ReviewDraft(ctx, g, leadstore.VerdictApprove, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.ReviewDraft(ctx, g, leadstore.VerdictReject, []string{"too_long"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.ReviewDraft(ctx, g, leadstore.VerdictReject, nil, ""); !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("a reject with no reason: %v", err)
	}
	page, err = b.DraftSamples(ctx, campaign.LegacySlug, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := page.Samples[0].Review; r == nil || r.Verdict != leadstore.VerdictReject {
		t.Fatalf("latest review %+v, want the reject", r)
	}
	if !strings.HasPrefix(page.Gate.Summary, "Claude drafts: not trusted yet: 0 of 20 reviewed") {
		t.Fatalf("a fake draft's review counted toward the gate: %q", page.Gate.Summary)
	}

	lead := page.Samples[0].LeadID
	refusedID, err := leadstoretest.Q(t).RecordGeneration(ctx, leadstore.GenerationRow{LeadID: lead, CampaignID: cid, Attempt: 1,
		Model: claude.ModelSonnet, Facts: outreach.FactSheet{}, CheckOK: false, Purpose: leadstore.PurposeSample,
		Failures: []outreach.Failure{{Code: outreach.CodeClaim, Token: "2019", Msg: "2019 is not in a cited fact"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReviewDraft(ctx, refusedID, leadstore.VerdictApprove, nil, ""); !errors.Is(err, console.ErrUserFixable) ||
		!strings.Contains(err.Error(), "refused by the check") {
		t.Fatalf("review of a refused draft: %v", err)
	}
	gates, err := b.DraftGates(ctx)
	if err != nil || gates[campaign.LegacySlug].Trusted {
		t.Fatalf("gates %+v err %v", gates, err)
	}
}

// addEmailSites adds n more GB Drupal 7 sites with an email route each (built into leads by
// the next leads build).
func addEmailSites(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-sites-probe", "sites-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		domain := fmt.Sprintf("site%02d.example", i)
		home, err := core.InsertFetch(ctx, runID, "sites-fixture", fetch.Record{Kind: fetch.KindSite, URL: "https://" + domain + "/", Status: 200, Body: []byte("<html></html>"), ContentType: "text/html"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{{Domain: domain, Country: "GB"}}); err != nil {
			t.Fatal(err)
		}
		siteID, err := lq.SiteIDByDomain(ctx, domain)
		if err != nil {
			t.Fatal(err)
		}
		probeID, err := lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true,
			Major: 7, Signals: []leadstore.Signal{{Signal: "meta-generator", Value: "7", FetchID: home}}, FetchIDs: []int64{home}, DerivedBy: "drupal@1"})
		if err != nil {
			t.Fatal(err)
		}
		orgID, _, err := lq.UpsertSiteOrg(ctx, probeID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteEmail, Value: "info@" + domain, FetchID: home, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDraftsEvalCountsReviewedLeads: a Claude eval with 19 reviewed leads is refused with the
// count before any call; with 20 it runs, reports and is recorded; a fake eval is never
// recorded.
func TestDraftsEvalCountsReviewedLeads(t *testing.T) {
	_, cid := draftsWorld(t)
	addEmailSites(t, 20)
	mustRun(t, "leads", "build")
	var leads []int64
	for i := range 20 {
		leads = append(leads, leadOf(t, fmt.Sprintf("site%02d.example", i)))
	}
	fake := &draftFakeCounting{}
	useCaller(t, fake)
	seedReviews(t, cid, leads[:19], 19, 19)
	code, _, errOut := leadsRun(t, "leads", "drafts", "eval", "--decider", "claude")
	if code == 0 || !strings.Contains(errOut, "19 of 20 reviewed leads") || fake.answered() != 0 {
		t.Fatalf("eval with 19: exit %d, %q, %d leads called", code, errOut, fake.answered())
	}
	out := mustRun(t, "leads", "drafts", "eval")
	wantLines(t, "fake eval", out, "decider fake · 19 reviewed leads · 19 runs", "Not recorded: only a Claude eval counts")

	seedReviews(t, cid, leads[19:], 1, 1)
	out = mustRun(t, "leads", "drafts", "eval", "--decider", "claude", "--runs", "1")
	wantLines(t, "claude eval", out, "decider claude · 20 reviewed leads · 20 runs", "First try: 20 of 20 (100%)", "Recorded as eval ")
	e, ok, err := leadstoretest.Q(t).LatestEval(context.Background(), cid)
	if err != nil || !ok || e.Reviewed != 20 || e.Runs != 20 || e.FirstTryPass != 1 || e.MeanWords["0"] <= 0 {
		t.Fatalf("eval %+v ok %v err %v", e, ok, err)
	}
	if n := batchCount(t, `SELECT count(*) FROM draft_generation WHERE purpose = 'eval'`); n != 20 {
		t.Fatalf("%d eval generations, want 20", n)
	}
}

// TestDraftsGoldenExport: the export refuses a path inside the repository and an existing
// file, and writes every reviewed Claude draft at mode 0600.
func TestDraftsGoldenExport(t *testing.T) {
	_, cid := draftsWorld(t)
	seedReviews(t, cid, []int64{leadOf(t, "northwind.example")}, 3, 2)
	code, _, errOut := leadsRun(t, "leads", "drafts", "golden-export", "--out", "golden.jsonl")
	if code == 0 || !strings.Contains(errOut, "inside the leadhound repository") {
		t.Fatalf("repo path: exit %d %q", code, errOut)
	}
	path := filepath.Join(t.TempDir(), "golden.jsonl")
	out := mustRun(t, "leads", "drafts", "golden-export", "--out", path)
	wantLines(t, "export", out, "3 reviewed drafts written to")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("export file %v %v", info, err)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 3 || !strings.Contains(string(raw), `"verdict":"reject"`) {
		t.Fatalf("export holds %d lines:\n%s", lines, raw)
	}
	if code, _, errOut := leadsRun(t, "leads", "drafts", "golden-export", "--out", path); code == 0 || !strings.Contains(errOut, "exists") {
		t.Fatalf("overwrite: exit %d %q", code, errOut)
	}
}

// TestCampaignsCheckDraftGate: writer: claude is refused by campaigns check with the gate's
// reasons while it is closed, and passes once it opens.
func TestCampaignsCheckDraftGate(t *testing.T) {
	dir, cid := draftsWorld(t)
	setWriter(t, dir, campaign.WriterClaude)
	seedReviews(t, cid, []int64{leadOf(t, "northwind.example")}, 14, 12)
	code, _, errOut := leadsRun(t, "leads", "campaigns", "check")
	if code == 0 || !strings.Contains(errOut, "writer \"claude\" is refused until the draft gate passes") ||
		!strings.Contains(errOut, "drupal-eol: Claude drafts: not trusted yet: 14 of 20 reviewed, 85% approved; no eval recorded yet") {
		t.Fatalf("check, gate closed: exit %d\n%s", code, errOut)
	}
	if code, _, _ := leadsRun(t, "leads", "campaigns", "register"); code == 0 {
		t.Fatal("register passed writer: claude with the gate closed")
	}
	trustGate(t, cid, []int64{leadOf(t, "northwind.example")})
	out := mustRun(t, "leads", "campaigns", "check")
	wantLines(t, "check, gate open", out, "drupal-eol: ok", "writer claude")
}

// TestDraftsEvalCountsEvaluatedLeads: a reviewed lead whose input cannot be built is left
// out of the eval, named with why, and not counted: 20 reviewed with one left out is 19
// evaluated, so a Claude eval is refused before any call, and a fake eval reports 19.
func TestDraftsEvalCountsEvaluatedLeads(t *testing.T) {
	_, cid := draftsWorld(t)
	addEmailSites(t, 20)
	mustRun(t, "leads", "build")
	var leads []int64
	for i := range 20 {
		leads = append(leads, leadOf(t, fmt.Sprintf("site%02d.example", i)))
	}
	fake := &draftFakeCounting{}
	useCaller(t, fake)
	seedReviews(t, cid, leads, 20, 20)
	// site07's stored signal no longer names its major: its input cannot be built.
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`UPDATE site_probe SET major = 9 WHERE id = (SELECT probe_id FROM lead WHERE id = $1)`, leads[7]); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := leadsRun(t, "leads", "drafts", "eval", "--decider", "claude", "--runs", "1")
	if code == 0 || !strings.Contains(errOut, "19 of 20 reviewed leads can be evaluated") || fake.answered() != 0 {
		t.Fatalf("claude eval with one left out: exit %d, %q, %d leads called", code, errOut, fake.answered())
	}
	wantLines(t, "claude eval refused", out, fmt.Sprintf("lead %d site07.example: left out: ", leads[7]),
		"Left out: 1 of 20 reviewed leads (named above); the eval counts the other 19.")
	out = mustRun(t, "leads", "drafts", "eval")
	wantLines(t, "fake eval", out, "decider fake · 19 reviewed leads · 19 runs")
	if _, ok, err := leadstoretest.Q(t).LatestEval(context.Background(), cid); err != nil || ok {
		t.Fatalf("an eval was recorded (%v %v)", ok, err)
	}
}
