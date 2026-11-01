//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// Decision 7 (a lead's next action) and Decision 20 (look again with LinkedIn).

// reachMark writes a contact_mark on a route or fact column.
func reachMark(t *testing.T, f outreachFixture, column string, id int64, mark string) {
	t.Helper()
	f.ownerExec(t, `INSERT INTO contact_mark (`+column+`, mark, actor) VALUES ($1, $2, 'operator')`, id, mark)
}

// reachLinkedInRoute adds a LinkedIn company route to lead's org and returns it; the lead is
// put on it when pick is true.
func reachLinkedInRoute(t *testing.T, f outreachFixture, lead int64, slug string, pick bool) int64 {
	t.Helper()
	var id int64
	if err := f.owner.QueryRow(context.Background(),
		`INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by)
		 VALUES ($1, 'linkedin', 'https://linkedin.com/company/' || $2::text, $3, 'enrich@1') RETURNING id`,
		f.orgOf(t, lead), slug, f.fid).Scan(&id); err != nil {
		t.Fatalf("linkedin route: %v", err)
	}
	if pick {
		f.ownerExec(t, `UPDATE lead SET channel = 'linkedin', route_id = $2 WHERE id = $1`, lead, id)
	}
	return id
}

// reachForceState sets a lead's state directly, whatever road would reach it.
func reachForceState(t *testing.T, f outreachFixture, lead int64, state string) {
	t.Helper()
	f.ownerExec(t, `UPDATE lead SET state = $2 WHERE id = $1`, lead, state)
}

// reachFirstMessage returns lead's first message.
func reachFirstMessage(t *testing.T, f outreachFixture, lead int64) int64 {
	t.Helper()
	var id int64
	if err := f.owner.QueryRow(context.Background(),
		`SELECT id FROM outreach_message WHERE lead_id = $1 ORDER BY id LIMIT 1`, lead).Scan(&id); err != nil {
		t.Fatalf("message of lead %d: %v", lead, err)
	}
	return id
}

// reachSearch runs a research attempt for lead through the store's own road and ends it with
// status; ResearchQueued and ResearchLooking stop there. Every earlier attempt must have left
// the queue, which is why the fixture asks for the queued lead last.
func reachSearch(t *testing.T, f outreachFixture, lead int64, status string, want []string) int64 {
	t.Helper()
	ctx := context.Background()
	id, created, err := f.q.RequestResearch(ctx, lead, leadstore.ResearchCLI, want, op)
	if err != nil || !created {
		t.Fatalf("request research lead %d: %d %v %v", lead, id, created, err)
	}
	if status == leadstore.ResearchQueued {
		return id
	}
	f.claim(t, id)
	if status == leadstore.ResearchLooking {
		return id
	}
	verified := 0
	if status == leadstore.ResearchFound {
		verified = 1
	}
	if err := f.q.FinishResearch(ctx, id, status, "m", nil, verified, verified); err != nil {
		t.Fatalf("finish research %d: %v", id, err)
	}
	return id
}

// reachOff is the caller's country rule in the fixture: ZZ is off.
func reachOff(country string) bool { return country == "ZZ" }

// reachFixtureLeads builds a lead of every bucket and reason and returns what each should be.
func reachFixtureLeads(t *testing.T, f outreachFixture) map[string]struct {
	ID   int64
	Want leadstore.Reach
} {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	out := map[string]struct {
		ID   int64
		Want leadstore.Reach
	}{}
	add := func(name string, id int64, bucket, reason string) {
		out[name] = struct {
			ID   int64
			Want leadstore.Reach
		}{id, leadstore.Reach{Bucket: bucket, Reason: reason}}
	}
	// Domains take no underscore, and two states carry one.
	host := func(name string) string { return strings.ReplaceAll(name, "_", "-") + ".localhost" }
	email := func(name string) int64 { return f.siteLead(t, host(name), nil, 7, "info@"+host(name)) }
	none := func(name string) int64 { return f.siteLead(t, host(name), nil, 7, "") }

	// Contacted, by state.
	for _, s := range []string{leadstore.LeadContacted, leadstore.LeadReplied, leadstore.LeadBounced, leadstore.LeadOptedOut} {
		l := email("state-" + s)
		reachForceState(t, f, l, s)
		add("state "+s, l, leadstore.ReachContacted, "")
	}
	// Contacted, batched with a message out; a draft or a shadow message is not out.
	for _, s := range []string{leadstore.MsgSent, leadstore.MsgMaybeSent, leadstore.MsgDone} {
		l := email("batched-" + s)
		f.batch(t, false, l)
		f.setMsg(t, reachFirstMessage(t, f, l), s, &now, false)
		add("batched, message "+s, l, leadstore.ReachContacted, "")
	}
	batched := email("batched-draft")
	f.batch(t, false, batched)
	add("batched email, nothing out", batched, leadstore.ReachEmail, "")
	shadow := email("batched-shadow")
	f.batch(t, true, shadow)
	f.setMsg(t, reachFirstMessage(t, f, shadow), leadstore.MsgDone, &now, false)
	reachForceState(t, f, shadow, leadstore.LeadBatched)
	add("batched, only a shadow message done", shadow, leadstore.ReachEmail, "")

	add("new email", email("new-email"), leadstore.ReachEmail, "")

	// Manual.
	add("new form", f.formLead(t, "new-form.localhost"), leadstore.ReachManual, "")
	batchedForm := f.formLead(t, "batched-form.localhost")
	reachForceState(t, f, batchedForm, leadstore.LeadBatched)
	add("batched form", batchedForm, leadstore.ReachManual, "")
	li := none("new-linkedin")
	reachLinkedInRoute(t, f, li, "new-linkedin", true)
	add("new linkedin", li, leadstore.ReachManual, "")
	phone := none("new-phone")
	f.phoneFact(t, phone, "+15550100001")
	add("new, phone", phone, leadstore.ReachManual, "")
	batchedPhone := none("batched-phone")
	f.phoneFact(t, batchedPhone, "+15550100002")
	reachForceState(t, f, batchedPhone, leadstore.LeadBatched)
	add("batched, phone", batchedPhone, leadstore.ReachManual, "")

	// Marks: the highest id decides.
	retired := none("phone-retired")
	reachMark(t, f, "fact_id", f.phoneFact(t, retired, "+15550100003"), "retire")
	add("phone retired", retired, leadstore.ReachDead, leadstore.ReachNotSearched)
	back := none("phone-back")
	fact := f.phoneFact(t, back, "+15550100004")
	reachMark(t, f, "fact_id", fact, "retire")
	reachMark(t, f, "fact_id", fact, "claim")
	add("phone retired then claimed", back, leadstore.ReachManual, "")
	again := none("phone-again")
	fact = f.phoneFact(t, again, "+15550100005")
	reachMark(t, f, "fact_id", fact, "claim")
	reachMark(t, f, "fact_id", fact, "retire")
	add("phone claimed then retired", again, leadstore.ReachDead, leadstore.ReachNotSearched)
	second := none("phone-second")
	reachMark(t, f, "fact_id", f.phoneFact(t, second, "+15550100006"), "retire")
	f.phoneFact(t, second, "+15550100007")
	add("one phone retired, another live", second, leadstore.ReachManual, "")

	// Dead ends, one per reason. An attempt that ended earlier is covered by "latest".
	add("never searched", none("dead-never"), leadstore.ReachDead, leadstore.ReachNotSearched)
	for _, c := range []struct{ status, reason string }{
		{leadstore.ResearchNothing, leadstore.ReachNothing},
		{leadstore.ResearchFound, leadstore.ReachFound},
		{leadstore.ResearchError, leadstore.ReachNotSearched},
		{leadstore.ResearchCapped, leadstore.ReachNotSearched},
	} {
		l := none("dead-" + c.status)
		reachSearch(t, f, l, c.status, wantEmail)
		add("search "+c.status, l, leadstore.ReachDead, c.reason)
	}
	latest := none("dead-latest")
	first := reachSearch(t, f, latest, leadstore.ResearchNothing, wantEmail)
	f.ageResearch(t, first, 91)
	reachSearch(t, f, latest, leadstore.ResearchError, wantEmail)
	add("nothing, then an error", latest, leadstore.ReachDead, leadstore.ReachNotSearched)
	searching := none("dead-looking")
	reachSearch(t, f, searching, leadstore.ResearchLooking, wantEmail)
	add("search looking", searching, leadstore.ReachDead, leadstore.ReachLooking)
	queued := none("dead-queued")
	reachSearch(t, f, queued, leadstore.ResearchQueued, wantEmail)
	add("search queued", queued, leadstore.ReachDead, leadstore.ReachLooking)

	// The country rule wins over the research, and moves no lead out of its bucket.
	off := none("off-never")
	f.setCountry(t, off, "ZZ")
	add("off, never searched", off, leadstore.ReachDead, leadstore.ReachCountry)
	offNothing := none("off-nothing")
	f.setCountry(t, offNothing, "ZZ")
	if _, err := f.owner.Exec(ctx, `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor, status, started_at, finished_at)
		SELECT org_id, id, campaign_id, 'cli', ARRAY['email'], 'operator', 'nothing', now(), now() FROM lead WHERE id = $1`, offNothing); err != nil {
		t.Fatal(err)
	}
	add("off, found nothing", offNothing, leadstore.ReachDead, leadstore.ReachCountry)
	offEmail := email("off-email")
	f.setCountry(t, offEmail, "ZZ")
	add("off, email", offEmail, leadstore.ReachEmail, "")
	offPhone := none("off-phone")
	f.phoneFact(t, offPhone, "+15550100008")
	f.setCountry(t, offPhone, "ZZ")
	add("off, phone", offPhone, leadstore.ReachManual, "")

	// Other.
	for _, s := range []string{leadstore.LeadSkipped, leadstore.LeadSuppressed, leadstore.LeadClosed} {
		l := email("state-" + s)
		reachForceState(t, f, l, s)
		add("state "+s, l, leadstore.ReachOther, "")
	}
	stray := none("batched-nothing")
	reachForceState(t, f, stray, leadstore.LeadBatched)
	add("batched, no channel, no phone", stray, leadstore.ReachOther, "")
	return out
}

// TestReachContract drives ReachOf, the List's filter and row, the lead page's row and
// ReachCounts over one fixture of every bucket and reason, lead by lead.
func TestReachContract(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	fixture := reachFixtureLeads(t, f)
	lc := leadstore.ListContext{CountryOff: reachOff}

	wantIDs := map[string][]int64{}
	var counts leadstore.ReachCounts
	for name, l := range fixture {
		got, err := f.q.ReachOf(ctx, l.ID, reachOff)
		if err != nil {
			t.Fatalf("%s: reach of: %v", name, err)
		}
		if got != l.Want {
			t.Errorf("%s: ReachOf = %+v, want %+v", name, got, l.Want)
		}
		wantIDs[l.Want.Bucket] = append(wantIDs[l.Want.Bucket], l.ID)
		switch l.Want.Bucket {
		case leadstore.ReachContacted:
			counts.Contacted++
		case leadstore.ReachEmail:
			counts.Email++
		case leadstore.ReachManual:
			counts.Manual++
		case leadstore.ReachDead:
			counts.Dead++
		case leadstore.ReachOther:
			counts.Other++
		}
		// No rule passed: the country goes unjudged, and the bucket stays.
		plain, err := f.q.ReachOf(ctx, l.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if plain.Bucket != l.Want.Bucket || plain.Reason == leadstore.ReachCountry {
			t.Errorf("%s: ReachOf without a rule = %+v", name, plain)
		}
		// The lead page reads the same row.
		d, err := f.q.LeadDetail(ctx, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.Reach != plain.Bucket || d.ReachReason != plain.Reason {
			t.Errorf("%s: detail = %s/%s, ReachOf = %+v", name, d.Reach, d.ReachReason, plain)
		}
	}
	for _, b := range []string{leadstore.ReachContacted, leadstore.ReachEmail, leadstore.ReachManual, leadstore.ReachDead, leadstore.ReachOther} {
		if len(wantIDs[b]) == 0 {
			t.Errorf("the fixture has no %s lead", b)
		}
	}

	byID := map[int64]string{}
	for name, l := range fixture {
		byID[l.ID] = name
	}
	// The filter partitions the leads the same way, and each row names its own reach.
	listed := func(f2 leadstore.ListFilter) []leadstore.ListRow {
		t.Helper()
		f2.PerPage = leadstore.ListPerPageMax
		rows, total, err := f.q.LeadList(ctx, f2, lc)
		if err != nil {
			t.Fatalf("list %+v: %v", f2, err)
		}
		if total != len(rows) {
			t.Fatalf("list %+v: %d rows of %d: the fixture must fit one page", f2, len(rows), total)
		}
		return rows
	}
	for b, ids := range wantIDs {
		rows := listed(leadstore.ListFilter{Reach: b})
		got := []int64{}
		for _, r := range rows {
			got = append(got, r.LeadID)
			want := fixture[byID[r.LeadID]].Want
			if r.Reach != want.Bucket || r.ReachReason != want.Reason {
				t.Errorf("%s: list row = %s/%s, want %+v", byID[r.LeadID], r.Reach, r.ReachReason, want)
			}
		}
		slices.Sort(got)
		slices.Sort(ids)
		if !slices.Equal(got, ids) {
			t.Errorf("Reach %s lists %v, want %v", b, got, ids)
		}
	}
	got, err := f.q.ReachCounts(ctx, leadstore.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got != counts {
		t.Errorf("ReachCounts = %+v, want %+v", got, counts)
	}
	if got.All() != len(fixture) {
		t.Errorf("All = %d, want %d", got.All(), len(fixture))
	}
	if rows := listed(leadstore.ListFilter{Status: leadstore.ListAll}); len(rows) != len(fixture) {
		t.Errorf("status all lists %d, want %d", len(rows), len(fixture))
	}

	// ReachCounts ignores Reach, honours every other filter and agrees with the List under each.
	yes := true
	first := wantIDs[leadstore.ReachEmail][0]
	f.star(t, first)
	for name, flt := range map[string]leadstore.ListFilter{
		"status open":  {Status: leadstore.ListOpen},
		"status all":   {Status: leadstore.ListAll},
		"contacted":    {Status: leadstore.ListContacted},
		"one lead":     {LeadIDs: []int64{first}},
		"no lead":      {LeadIDs: []int64{}},
		"country ZZ":   {Countries: []string{"ZZ"}},
		"has a phone":  {HasPhone: true},
		"starred":      {Starred: &yes},
		"sort order":   {Sort: leadstore.SortOrder},
		"with a reach": {Reach: leadstore.ReachDead},
	} {
		c, err := f.q.ReachCounts(ctx, flt)
		if err != nil {
			t.Errorf("%s: counts: %v", name, err)
			continue
		}
		for b, n := range map[string]int{leadstore.ReachContacted: c.Contacted, leadstore.ReachEmail: c.Email,
			leadstore.ReachManual: c.Manual, leadstore.ReachDead: c.Dead, leadstore.ReachOther: c.Other} {
			one := flt
			one.Reach = b
			if rows := listed(one); len(rows) != n {
				t.Errorf("%s: %s counts %d, lists %d", name, b, n, len(rows))
			}
		}
	}

	// A blank Status with no Reach stays the open list.
	if rows := listed(leadstore.ListFilter{}); len(rows) == 0 || slices.ContainsFunc(rows, func(r leadstore.ListRow) bool { return r.State != leadstore.LeadNew }) {
		t.Errorf("the default list: %d rows, want only new leads", len(rows))
	}

	// Refused: an unknown reach, and in the counts an unknown status, sort or channel.
	if _, _, err := f.q.LeadList(ctx, leadstore.ListFilter{Reach: "soon"}, lc); !errors.Is(err, leadstore.ErrBadListFilter) {
		t.Errorf("unknown reach: %v", err)
	}
	for name, flt := range map[string]leadstore.ListFilter{"status": {Status: "x"}, "sort": {Sort: "x"}, "channel": {Channels: []string{"x"}}} {
		if _, err := f.q.ReachCounts(ctx, flt); !errors.Is(err, leadstore.ErrBadListFilter) {
			t.Errorf("counts, unknown %s: %v", name, err)
		}
	}
}

// TestReachOfNotFound: a selftest lead is not on the list, and neither is a missing one.
func TestReachOfNotFound(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	l := f.siteLead(t, "selftest-reach.localhost", nil, 7, "")
	f.selftest(t, l)
	for _, id := range []int64{l, 999999} {
		if _, err := f.q.ReachOf(ctx, id, nil); !errors.Is(err, leadstore.ErrLeadNotFound) {
			t.Errorf("lead %d: %v, want ErrLeadNotFound", id, err)
		}
	}
	if c, err := f.q.ReachCounts(ctx, leadstore.ListFilter{}); err != nil || c.All() != 0 {
		t.Errorf("counts = %+v %v, want none", c, err)
	}
}

// TestLeadListRetiredContacts: a retired route or phone no longer counts as the lead having one.
func TestLeadListRetiredContacts(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lc := leadstore.ListContext{}
	emailLead := f.siteLead(t, "retired-email.localhost", nil, 7, "info@retired-email.localhost")
	formLead := f.formLead(t, "retired-form.localhost")
	phoneLead := f.siteLead(t, "retired-phone.localhost", nil, 7, "")
	fact := f.phoneFact(t, phoneLead, "+15550100009")
	row := func(id int64) leadstore.ListRow {
		t.Helper()
		rows, _ := listRows(t, f.q, leadstore.ListFilter{LeadIDs: []int64{id}}, lc)
		if len(rows) != 1 {
			t.Fatalf("lead %d: %d rows", id, len(rows))
		}
		return rows[0]
	}
	if !row(emailLead).HasEmail || !row(formLead).HasForm || !row(phoneLead).HasPhone {
		t.Fatal("before any mark: each lead should have its contact")
	}
	routeOf := func(lead int64) int64 {
		var id int64
		if err := f.owner.QueryRow(ctx, `SELECT route_id FROM lead WHERE id = $1`, lead).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	reachMark(t, f, "route_id", routeOf(emailLead), "retire")
	reachMark(t, f, "route_id", routeOf(formLead), "retire")
	reachMark(t, f, "fact_id", fact, "retire")
	if row(emailLead).HasEmail || row(formLead).HasForm || row(phoneLead).HasPhone {
		t.Error("after a retire: the lead should no longer have the contact")
	}
	for _, ch := range []string{leadstore.ListChannelEmail, leadstore.ListChannelForm, leadstore.ListChannelPhone} {
		if ids, total := listIDs(t, f.q, leadstore.ListFilter{Channels: []string{ch}}, lc); total != 0 {
			t.Errorf("channel %s lists %v after the retire", ch, ids)
		}
	}
	reachMark(t, f, "fact_id", fact, "claim")
	if !row(phoneLead).HasPhone {
		t.Error("a claim after the retire should bring the phone back")
	}
}

// TestResearchLinkedInWindow is Decision 20: an attempt that did not look for LinkedIn does not
// hold back a request that wants it, once; from then on the window counts as usual.
func TestResearchLinkedInWindow(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	linkedIn := []string{leadstore.WantEmail, leadstore.WantLinkedIn}
	onlyLinkedIn := []string{leadstore.WantLinkedIn}
	button := func(lead int64, want []string) (int64, bool, error) {
		return f.q.RequestResearch(ctx, lead, leadstore.ResearchButton, want, op)
	}
	tooSoon := func(name string, lead int64, want []string) {
		t.Helper()
		if id, created, err := button(lead, want); !errors.Is(err, leadstore.ErrResearchTooSoon) {
			t.Errorf("%s: %d %v %v, want too soon", name, id, created, err)
		}
	}

	// Searched for email; LinkedIn is accepted at once, the same day; the next is refused.
	a := f.siteLead(t, "linkedin-a.localhost", nil, 7, "")
	emailOnly := reachSearch(t, f, a, leadstore.ResearchNothing, wantEmail)
	tooSoon("email again", a, wantEmail)
	r, err := f.q.ResearchForLead(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if r.NextAt == nil || r.NextAtLinkedIn != nil {
		t.Errorf("after an email search: NextAt %v, NextAtLinkedIn %v; want a date and none", r.NextAt, r.NextAtLinkedIn)
	}
	id, created, err := button(a, linkedIn)
	if err != nil || !created || id == emailOnly {
		t.Fatalf("linkedin after email: %d %v %v, want a new row", id, created, err)
	}
	var want []string
	if err := f.owner.QueryRow(ctx, `SELECT want FROM contact_research WHERE id = $1`, id).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(want, linkedIn) {
		t.Errorf("stored want = %v, want %v", want, linkedIn)
	}
	// While it is open the same row answers, not a second one.
	if open, created, err := button(a, linkedIn); err != nil || created || open != id {
		t.Errorf("while open: %d %v %v, want row %d", open, created, err, id)
	}
	f.claim(t, id)
	if err := f.q.FinishResearch(ctx, id, leadstore.ResearchNothing, "m", nil, 0, 0); err != nil {
		t.Fatal(err)
	}
	tooSoon("linkedin the same day", a, linkedIn)
	tooSoon("only linkedin the same day", a, onlyLinkedIn)
	tooSoon("email after a linkedin search", a, wantEmail)
	if got, created, err := f.q.RequestResearch(ctx, a, leadstore.ResearchStar, linkedIn, op); err != nil || created || got != id {
		t.Errorf("a star the same day: %d %v %v, want the last attempt %d", got, created, err, id)
	}
	r, err = f.q.ResearchForLead(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if r.NextAtLinkedIn == nil || r.NextAt == nil || !r.NextAtLinkedIn.Equal(*r.NextAt) {
		t.Errorf("after a linkedin search: NextAt %v, NextAtLinkedIn %v; want the same date", r.NextAt, r.NextAtLinkedIn)
	}
	// Past the window it may run again.
	f.ageResearch(t, id, 91)
	f.ageResearch(t, emailOnly, 91)
	if _, created, err := button(a, linkedIn); err != nil || !created {
		t.Errorf("past the window: %v %v", created, err)
	}

	// A capped LinkedIn attempt never ran, so it spends nothing.
	b := f.siteLead(t, "linkedin-b.localhost", nil, 7, "")
	f.ownerExec(t, `UPDATE contact_research SET status = 'nothing', started_at = now(), finished_at = now() WHERE org_id = $1 AND status = 'queued'`, f.orgOf(t, a))
	reachSearch(t, f, b, leadstore.ResearchNothing, wantEmail)
	reachSearch(t, f, b, leadstore.ResearchCapped, onlyLinkedIn)
	id, created, err = button(b, onlyLinkedIn)
	if err != nil || !created {
		t.Fatalf("linkedin after a capped linkedin: %d %v %v", id, created, err)
	}
	f.claim(t, id)
	if err := f.q.FinishResearch(ctx, id, leadstore.ResearchFound, "m", nil, 1, 1); err != nil {
		t.Fatal(err)
	}
	tooSoon("linkedin after one that ran", b, onlyLinkedIn)

	// An unknown want is still refused.
	if _, _, err := button(b, []string{"fax"}); !errors.Is(err, leadstore.ErrBadResearch) {
		t.Errorf("unknown want: %v", err)
	}
}
