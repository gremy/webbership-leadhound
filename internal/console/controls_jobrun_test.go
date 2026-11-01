package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// rowHTML is the Controls page's row for job: from its <li> to the </li> that closes it.
func rowHTML(t *testing.T, body, job string) string {
	t.Helper()
	i := strings.Index(body, `id="control-`+job+`"`)
	if i < 0 {
		t.Fatalf("no row for %s", job)
	}
	j := strings.Index(body[i:], "</li>")
	if j < 0 {
		t.Fatalf("the row for %s does not close", job)
	}
	return body[i : i+j]
}

// jobRunConfirm is the confirm sentence the tests put on a block.
const jobRunConfirm = "Checks the listed sites. It can take a while."

// TestJobRunNowBlocksOnlyOnTheirRows: a row with RunNow renders its own block (its own form
// path and checkbox id); rows without one render none, over a page where only some rows
// have it.
func TestJobRunNowBlocksOnlyOnTheirRows(t *testing.T) {
	with := []string{JobBatch, JobContacts} // the probe row is left without one on purpose
	b := controlsBackend()
	for i := range b.controls.Jobs {
		if slices.Contains(with, b.controls.Jobs[i].Job) {
			b.controls.Jobs[i].RunNow = &JobRunNow{State: "idle", CanAsk: true, Confirm: jobRunConfirm}
		}
	}
	h := newHarness(t, b, false)
	body := h.do(get("/controls", h.session(t))).Body.String()
	if n := strings.Count(body, `/run-now"`); n != len(with) {
		t.Errorf("%d Run now forms, want %d", n, len(with))
	}
	for _, job := range ControlJobs {
		row := rowHTML(t, body, job)
		has := slices.Contains(with, job)
		for _, want := range []string{
			`id="run-now-` + job + `"`,
			`action="/controls/` + job + `/run-now"`,
			`<input class="check" type="checkbox" id="run-confirm-` + job + `" name="confirm" value="yes" required>`,
			`for="run-confirm-` + job + `"`,
		} {
			if strings.Contains(row, want) != has {
				t.Errorf("%s: %q shown %t, want %t", job, want, !has, has)
			}
		}
	}
	if strings.Contains(rowHTML(t, body, "sending"), "run-now") {
		t.Error("the sending row has a Run now block")
	}
	if n := h.b.n("JobRunNow"); n != 0 {
		t.Errorf("GET called JobRunNow %d times", n)
	}
}

// TestJobRunNowBlockStates: CanAsk shows the form with its confirm sentence; Pending hides it
// and polls only its own block; an idle block never polls; stuck and bad are red.
func TestJobRunNowBlockStates(t *testing.T) {
	tests := []struct {
		name string
		rn   JobRunNow
	}{
		{"idle", JobRunNow{State: "idle", CanAsk: true}},
		{"waiting", JobRunNow{State: "waiting", Line: "Asked at 21:03, waiting to start.", Pending: true}},
		{"running", JobRunNow{State: "running", Line: "Running since 21:15: checking the sites.", Pending: true}},
		{"done", JobRunNow{State: "done", Line: "The run asked at 21:03 ended at 23:40.", CanAsk: true}},
		{"bad", JobRunNow{State: "bad", Line: "The last run did not finish (no end was recorded).", CanAsk: true}},
		{"stuck", JobRunNow{State: "stuck", Line: "Asked at 21:03 and not started.", CanAsk: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := controlsBackend()
			rn := tt.rn
			rn.Confirm = jobRunConfirm
			b.controls.Jobs[2].RunNow = &rn // the probe row
			h := newHarness(t, b, false)
			row := rowHTML(t, h.do(get("/controls", h.session(t))).Body.String(), JobProbe)

			form := strings.Contains(row, `action="/controls/probe/run-now"`)
			if form != rn.CanAsk || strings.Contains(row, jobRunConfirm) != rn.CanAsk {
				t.Errorf("form shown %t, want %t (CanAsk)", form, rn.CanAsk)
			}
			poll := strings.Contains(row, `hx-get="/controls"`) && strings.Contains(row, `hx-select="#run-now-probe"`) &&
				strings.Contains(row, `hx-trigger="every 30s"`)
			if poll != rn.Pending || strings.Contains(row, "hx-get") != rn.Pending {
				t.Errorf("polls %t, want %t (Pending)", poll, rn.Pending)
			}
			if strings.Contains(row, `role="status"`) != rn.Pending {
				t.Errorf("role=status shown %t, want %t", !rn.Pending, rn.Pending)
			}
			if rn.Line == "" {
				if strings.Contains(row, `<p class="hint text-bad"`) {
					t.Error("an idle block shows a line")
				}
				return
			}
			red := rn.State == "stuck" || rn.State == "bad"
			want := `<p class="hint">`
			if red {
				want = `<p class="hint text-bad">`
			}
			if rn.Pending {
				want = `<p class="hint" role="status">`
			}
			if !strings.Contains(row, want+rn.Line+"</p>") {
				t.Errorf("lacks %q\n%s", want+rn.Line+"</p>", row)
			}
		})
	}
}

// TestJobRunNowBlockSweep: whatever the state, a block offers the form exactly when it is not
// pending (the backend keeps CanAsk == !Pending; the page never shows both or neither).
func TestJobRunNowBlockSweep(t *testing.T) {
	for _, pending := range []bool{false, true} {
		b := controlsBackend()
		b.controls.Jobs[3].RunNow = &JobRunNow{State: "waiting", Line: "Asked.", Pending: pending, CanAsk: !pending, Confirm: jobRunConfirm}
		h := newHarness(t, b, false)
		row := rowHTML(t, h.do(get("/controls", h.session(t))).Body.String(), JobContacts)
		if strings.Contains(row, "<form method=\"post\" action=\"/controls/contacts/run-now\"") == pending {
			t.Errorf("pending %t: the form is shown %t", pending, pending)
		}
	}
}

// plannedAt is the planned run the tests show.
var plannedAt = testSent.Add(3 * time.Hour)

// TestPlannedOnControls: a planned run replaces the timer's next elapse; overdue is red; a
// disabled timer keeps its own words.
func TestPlannedOnControls(t *testing.T) {
	line := "Next run planned at 15:00 tomorrow, 24 hours after the run by hand at 15:02."
	late := "Overdue: planned at 10:00 and not started. Check journalctl -u leadhound-leads-contacts.service -g timer-guard."
	b := controlsBackend()
	b.controls.Jobs[0].Timer.Enabled = true
	b.controls.Jobs[0].Planned = &PlannedRun{At: plannedAt, Line: line}
	b.controls.Jobs[2].Planned = &PlannedRun{At: plannedAt, Line: late, Overdue: true}
	h := newHarness(t, b, false)
	body := h.do(get("/controls", h.session(t))).Body.String()

	batch := rowHTML(t, body, JobBatch)
	if !strings.Contains(batch, `timer enabled, <span class="">`+line+`</span>`) {
		t.Errorf("the batch row lacks its planned line\n%s", batch)
	}
	if strings.Contains(batch, "next run") {
		t.Error("the batch row still shows the timer's next elapse")
	}
	probe := rowHTML(t, body, JobProbe)
	if !strings.Contains(probe, `timer enabled, <span class="text-bad">`+late+`</span>`) {
		t.Errorf("the probe row's overdue line is not red\n%s", probe)
	}
	if sync := rowHTML(t, body, JobSync); !strings.Contains(sync, "timer enabled, next run") {
		t.Error("a row without a plan lost its timer's next run")
	}
	contacts := rowHTML(t, body, JobContacts) // its timer is not enabled
	if !strings.Contains(contacts, `<span class="text-bad">timer not enabled</span>`) {
		t.Error("a disabled timer lost its words")
	}
}

// TestPlannedOnJobsHome: the ATS and fetch rows of the Run now card show their planned line,
// red when overdue.
func TestPlannedOnJobsHome(t *testing.T) {
	b := richBackend("x")
	b.dash.RunNow = idleCard()
	b.dash.RunNow.Actions[1].Planned = &PlannedRun{At: plannedAt, Line: "Next run planned at 18:30, 12 hours after the run on schedule at 06:30."}
	b.dash.RunNow.Actions[3].Planned = &PlannedRun{At: plannedAt, Line: "Overdue: planned at 01:00 and not started.", Overdue: true}
	h := newHarness(t, b, false)
	body := h.do(get("/jobs/home", h.session(t))).Body.String()
	for _, want := range []string{
		`<p class="run-line">Next run planned at 18:30, 12 hours after the run on schedule at 06:30.</p>`,
		`<p class="run-line bad">Overdue: planned at 01:00 and not started.</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	if n := strings.Count(body, "planned at"); n != 2 {
		t.Errorf("%d planned lines, want 2", n)
	}
}

// TestJobRunNowPosts: a confirmed press of each Run now job reaches the backend once with
// that job, and returns to its row.
func TestJobRunNowPosts(t *testing.T) {
	for _, job := range RunNowJobs {
		for _, confirm := range []bool{true, false} {
			b := controlsBackend()
			b.jobRunOut = ControlOutcome{Message: "Asked."}
			h := newHarness(t, b, false)
			form := url.Values{}
			if confirm {
				form.Set("confirm", "yes")
			}
			w := h.do(post("/controls/"+job+"/run-now", form, h.session(t)))
			if want := "/controls#control-" + job; w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
				t.Errorf("%s: %d to %q, want 303 to %s", job, w.Code, w.Header().Get("Location"), want)
			}
			if n := h.b.n("JobRunNow"); n != 1 {
				t.Errorf("%s: JobRunNow called %d times", job, n)
			}
			if h.b.lastJobRun != job || h.b.jobRunConf != confirm {
				t.Errorf("%s confirm %t: backend got %q, %t", job, confirm, h.b.lastJobRun, h.b.jobRunConf)
			}
		}
	}
	h := newHarness(t, controlsBackend(), false)
	h.do(post("/controls/batch/run-now", url.Values{"confirm": {"on"}}, h.session(t)))
	if h.b.jobRunConf {
		t.Error("a confirm other than yes counted as confirmed")
	}
}

// TestJobRunNowRefusedAndUnknown: a refusal shows its words at 422; a job without a Run now
// block, or no job at all, is no page and asks nothing.
func TestJobRunNowRefusedAndUnknown(t *testing.T) {
	b := controlsBackend()
	b.jobRunErr = fmt.Errorf("%w: Tick the box to confirm: this run takes a while and spends credits.", ErrUserFixable)
	h := newHarness(t, b, false)
	w := h.do(post("/controls/probe/run-now", url.Values{}, h.session(t)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "Tick the box to confirm: this run takes a while and spends credits.") || !strings.Contains(body, "notice-error") {
		t.Error("the refusal is not on the page")
	}

	b = controlsBackend()
	b.jobRunErr = ErrLeadsNotSetUp
	h = newHarness(t, b, false)
	if w := h.do(post("/controls/contacts/run-now", url.Values{"confirm": {"yes"}}, h.session(t))); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("not set up: status %d, want 422", w.Code)
	}

	b = controlsBackend()
	b.jobRunErr = errors.New("database gone")
	h = newHarness(t, b, false)
	if w := h.do(post("/controls/batch/run-now", url.Values{"confirm": {"yes"}}, h.session(t))); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "database gone") {
		t.Errorf("a failure: status %d, or its error leaked", w.Code)
	}

	h = newHarness(t, controlsBackend(), false)
	for _, job := range []string{JobSync, JobResearch, JobVerify, "nope"} {
		if w := h.do(post("/controls/"+job+"/run-now", url.Values{"confirm": {"yes"}}, h.session(t))); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", job, w.Code)
		}
	}
	if n := h.b.n("JobRunNow"); n != 0 {
		t.Errorf("JobRunNow called %d times for jobs without Run now", n)
	}
}
