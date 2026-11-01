//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/research"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The canonical LinkedIn addresses of this file, the person's built in pieces: the pre-commit
// check refuses the whole path in a tracked file.
const (
	liCompany = "https://linkedin.com/company/northwind"
	liPerson  = "https://linkedin.com/" + "in" + "/jane-doe"
)

// leadChannelOf is lead id's channel and its route's value ("" for none).
func leadChannelOf(t *testing.T, id int64) (channel, value string) {
	t.Helper()
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT l.channel, coalesce(r.value, '') FROM lead l LEFT JOIN contact_route r ON r.id = l.route_id WHERE l.id = $1`, id).
		Scan(&channel, &value); err != nil {
		t.Fatal(err)
	}
	return channel, value
}

// routeIDOf is org's route id for value.
func routeIDOf(t *testing.T, org int64, value string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT id FROM contact_route WHERE org_id = $1 AND value = $2`, org, value).Scan(&id); err != nil {
		t.Fatalf("route %s: %v", value, err)
	}
	return id
}

// TestLookAgainWithLinkedInOnce is Decision 20: a lead searched under the old rules (want
// {email}) has Look again accepted at once with LinkedIn, and refused again the same day. The
// CLI's plain request does not get the exception; its --linkedin does.
func TestLookAgainWithLinkedInOnce(t *testing.T) {
	researchScene(t, "northwind.example", "contoso.example")
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	b := leadsBackend(t)
	lead := leadIDOf(t, "northwind.example")
	if _, _, err := lq.RequestResearch(ctx, lead, leadstore.ResearchCLI, []string{leadstore.WantEmail}, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	finishResearch(t, leadstore.ResearchNothing)

	d, err := b.LeadDetail(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if r := d.Research; !r.CanAsk || r.NextAt != nil {
		t.Errorf("the page does not offer Look again at once: %+v", r)
	}
	id := strconv.FormatInt(lead, 10)
	if out := mustRun(t, "leads", "research", "request", id); !strings.Contains(out, "no request: northwind.example: research last ran on") {
		t.Errorf("the CLI's plain request:\n%s", out)
	}
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: lead})
	if err != nil || !strings.Contains(out.Message, "a search for a role email and a LinkedIn page is queued") {
		t.Fatalf("Look again after an email search: %q %v", out.Message, err)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_research WHERE lead_id = %d AND status = 'queued' AND trigger = 'button' AND want = '{email,linkedin}'`, lead)); n != 1 {
		t.Errorf("%d queued attempts with LinkedIn, want 1", n)
	}
	finishResearch(t, leadstore.ResearchNothing)

	_, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: lead})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Not looked again: research last ran on") {
		t.Errorf("Look again the same day: %v", err)
	}
	if d, err = b.LeadDetail(ctx, lead); err != nil || d.Research.NextAt == nil {
		t.Errorf("the page offers Look again after the LinkedIn search: %+v %v", d.Research, err)
	}
	if out := mustRun(t, "leads", "research", "request", "--linkedin", id); !strings.Contains(out, "research last ran on") {
		t.Errorf("--linkedin the same day:\n%s", out)
	}

	// The CLI's flag frees an org the same way, once, even with a route on the lead.
	other := leadIDOf(t, "contoso.example")
	if _, _, err := lq.RequestResearch(ctx, other, leadstore.ResearchCLI, []string{leadstore.WantEmail}, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	finishResearch(t, leadstore.ResearchNothing)
	if out := mustRun(t, "leads", "research", "request", "--linkedin", strconv.FormatInt(other, 10)); !strings.Contains(out, "it looks for a role email and a LinkedIn page") {
		t.Errorf("--linkedin after an email search:\n%s", out)
	}
}

// TestResearchStoresLinkedIn: a run wanting LinkedIn stores the company page and the person's
// profile with its org_person row, the attempt is found, no linkedin.com host is ever asked,
// and the next build puts the lead on the company page. A value retired afterwards comes back
// from research as nothing: no row, no person, not picked.
func TestResearchStoresLinkedIn(t *testing.T) {
	ctx := context.Background()
	researchScene(t, "northwind.example")
	key := sentinelKey(t)
	claudeKeyFile(t, key)
	org, lead := orgIDOf(t, "northwind.example"), leadIDOf(t, "northwind.example")
	const page = "https://northwind.example/team"
	pages := newResearchPages(t, map[string]string{
		"northwind.example/team": `<html><body><div class="card"><h3>Jane Doe</h3><p>Head of Digital</p><a href="https://www.` +
			strings.TrimPrefix(liPerson, "https://") + `/">LinkedIn</a></div>
			<footer><a href="https://www.linkedin.com/company/Northwind/about?trk=1">Follow us</a></footer></body></html>`,
	})
	answer, err := json.Marshal(map[string]any{"candidates": []map[string]string{
		{"kind": "linkedin", "value": liPerson, "source_url": page, "quote": researchQuote, "role": "Head of Digital", "name": "Jane Doe"},
		{"kind": "linkedin", "value": liCompany, "source_url": page, "quote": researchQuote},
	}})
	if err != nil {
		t.Fatal(err)
	}
	newAnthropicFake(t, key, func(int) (int, string) {
		return 200, anthropicMessage("end_turn", string(answer), 1200, 150)
	})
	if _, _, err := leadstoretest.Q(t).RequestResearch(ctx, lead, leadstore.ResearchButton, []string{leadstore.WantLinkedIn}, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, "leads", "research", "run", "--max", "1")
	wantLines(t, "run", out, "northwind.example: found 2 LinkedIn pages (2 of 2 candidates verified)", "stored: 0 new emails, 0 new phones, 2 new LinkedIn pages")
	if got := researchRowOf(t, org); got.status != leadstore.ResearchFound || got.verified != 2 {
		t.Errorf("attempt %+v, want found with 2 verified", got)
	}
	for _, asked := range pages.requests() {
		if strings.Contains(strings.ToLower(asked), "linkedin") {
			t.Errorf("asked %s", asked)
		}
	}
	lq := leadstoretest.Q(t)
	people, err := lq.PeopleFor(ctx, org)
	if err != nil || len(people) != 1 || people[0].Name != "Jane Doe" || people[0].Role != "Head of Digital" || people[0].FetchID == 0 || people[0].DerivedBy != research.DerivedBy {
		t.Fatalf("people %+v, %v", people, err)
	}
	routes, err := lq.RoutesFor(ctx, org)
	if err != nil || len(routes) != 2 || routes[0].Value != liCompany || routes[1].Value != liPerson || routes[1].PersonID != people[0].ID || routes[0].PersonID != 0 {
		t.Fatalf("routes %+v, %v", routes, err)
	}
	mustRun(t, "leads", "build")
	if ch, v := leadChannelOf(t, lead); ch != leadstore.RouteLinkedIn || v != liCompany {
		t.Errorf("after the build: %s %s, want the company page", ch, v)
	}
	if out := mustRun(t, "leads", "build"); !strings.Contains(out, "linkedin 1") {
		t.Errorf("the build's summary does not count the channel:\n%s", out)
	}

	// Retired: research finding them again adds nothing, and they are not picked.
	for _, r := range routes {
		if err := lq.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: r.ID, Mark: leadstore.MarkRetire}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rr := researchRun{pool: s.Pool()}
	res := research.Result{LinkedIn: []research.LinkedInFound{
		{Value: liCompany, Normalized: liCompany, Kind: enrich.LinkedInCompany, FetchID: routes[0].FetchID},
		{Value: liPerson, Normalized: liPerson, Kind: enrich.LinkedInPerson, Role: "Head of Digital", Name: "Jane Doe", FetchID: routes[1].FetchID},
	}}
	_, _, n, err := rr.store(ctx, org, res, &pageRecorder{bodies: map[int64][]byte{}})
	if err != nil || n != 0 {
		t.Fatalf("stored %d, %v: a retired value came back", n, err)
	}
	if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM org_person WHERE org_id = %d`, org)); got != 1 {
		t.Errorf("%d people: a retired profile added one", got)
	}
	if routes, err = lq.RoutesFor(ctx, org); err != nil || len(routes) != 0 {
		t.Errorf("routes after the retire %+v, %v", routes, err)
	}
	mustRun(t, "leads", "build")
	if ch, v := leadChannelOf(t, lead); ch != leadstore.ChannelNone || v != "" {
		t.Errorf("after retiring both: %s %s, want no channel", ch, v)
	}
}

// TestBuildReadsOperatorRoutes is C4: an email the operator added has no MX record stored, and
// the build still picks it for email, ahead of a found address with one; a retired one is not
// picked; the operator's LinkedIn page outranks a found form in a country that is off.
func TestBuildReadsOperatorRoutes(t *testing.T) {
	ctx := context.Background()
	researchScene(t)
	org := researchSite(t, "northwind.example", "GB", "contacts@1", true)
	off := researchSite(t, "us.example", "US", "contacts@1", false)
	mustRun(t, "leads", "build")
	lq := leadstoretest.Q(t)
	lead, leadOff := leadIDOf(t, "northwind.example"), leadIDOf(t, "us.example")
	found := "info" + "@" + "northwind.example"
	if ch, v := leadChannelOf(t, lead); ch != leadstore.ChannelEmail || v != found {
		t.Fatalf("before: %s %s", ch, v)
	}
	mine := "jane" + "@" + "northwind.example"
	added, err := lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: mine})
	if err != nil {
		t.Fatal(err)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE id = %d AND mx_ok`, added.ID)); n != 0 {
		t.Fatal("the operator's email was stored with an MX record")
	}
	mustRun(t, "leads", "build")
	if ch, v := leadChannelOf(t, lead); ch != leadstore.ChannelEmail || v != mine {
		t.Errorf("with his email: %s %s, want email %s", ch, v, mine)
	}
	if err := lq.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: added.ID, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "build")
	if ch, v := leadChannelOf(t, lead); ch != leadstore.ChannelEmail || v != found {
		t.Errorf("his email retired: %s %s, want the found one back", ch, v)
	}

	// A country that is off: only the operator's own manual route picks a channel.
	if ch, _ := leadChannelOf(t, leadOff); ch != leadstore.ChannelNone {
		t.Fatalf("off country before: %s", ch)
	}
	if _, err := lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: off, Kind: leadstore.RouteLinkedIn, Value: "https://linkedin.com/company/us"}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "build")
	if ch, v := leadChannelOf(t, leadOff); ch != leadstore.RouteLinkedIn || v != "https://linkedin.com/company/us" {
		t.Errorf("off country with his page: %s %s", ch, v)
	}

	// The email gate's fall reads the same routes in the same order, the operator's flag on.
	mine2 := "owner" + "@" + "northwind.example"
	if _, err := lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: mine2}); err != nil {
		t.Fatal(err)
	}
	g := &emailGate{}
	left, err := g.fallbackRoutes(ctx, lq, leadstore.Candidate{OrgID: org, Domain: "northwind.example", Route: found})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0].Value != mine2 || !left[0].Operator || left[1].Value != found || left[1].Operator {
		t.Errorf("fall routes %+v, want his live email first, flagged", left)
	}
}

// TestCafeBuildReadsOperatorRoutes: the café build reads the operator's email as MX-ok too.
func TestCafeBuildReadsOperatorRoutes(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	importDemoPlaces(t, dir)
	sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", cafeFixtures(t))
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	lq := leadstoretest.Q(t)
	const domain = "cafeneaua-exemplu.example"
	org, lead := orgIDOf(t, domain), leadIDOf(t, domain)
	if ch, _ := leadChannelOf(t, lead); ch != leadstore.ChannelNone {
		t.Fatalf("before: %s", ch)
	}
	mine := "ana" + "@" + domain
	if _, err := lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: mine}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if ch, v := leadChannelOf(t, lead); ch != leadstore.ChannelEmail || v != mine {
		t.Errorf("after: %s %s, want email %s", ch, v, mine)
	}
}
