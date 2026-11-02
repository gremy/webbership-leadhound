//go:build integration

package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// outreachWorld is the drafts fixture with the backend the console runs on.
func outreachWorld(t *testing.T) (*backend, string) {
	t.Helper()
	dir, _ := draftsWorld(t)
	return leadsBackend(t), dir
}

func detailOf(t *testing.T, b *backend, lead int64) console.LeadDetailPage {
	t.Helper()
	d, err := b.LeadDetail(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// contactRefs are the refs of the lead's contacts still in use of kind.
func contactRefs(t *testing.T, b *backend, lead int64, kind string) []console.ContactRef {
	t.Helper()
	var out []console.ContactRef
	for _, c := range detailOf(t, b, lead).Contacts {
		if c.Kind == kind && !c.Retired {
			ref, ok := console.ParseContactRef(c.CID)
			if !ok {
				t.Fatalf("contact %+v: no ref", c)
			}
			out = append(out, ref)
		}
	}
	return out
}

// retireAll retires every contact of kind on the lead's org through the backend.
func retireAll(t *testing.T, b *backend, lead int64, kind string) {
	t.Helper()
	for _, ref := range contactRefs(t, b, lead, kind) {
		if _, err := b.RetireContact(context.Background(), lead, ref, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func deadEndLead(t *testing.T, b *backend) int64 {
	t.Helper()
	lead := leadOf(t, "litware.example")
	retireAll(t, b, lead, "email")
	retireAll(t, b, lead, "form")
	if got := detailOf(t, b, lead).Reach; got != console.ReachDead {
		t.Fatalf("reach %s after retiring its routes", got)
	}
	return lead
}

func timelineLines(t *testing.T, b *backend, lead int64) string {
	t.Helper()
	items, err := b.LeadTimeline(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, it.Line+" | "+it.Detail)
	}
	return strings.Join(lines, "\n")
}

func wantOutreachRefusal(t *testing.T, what string, err error, words string) {
	t.Helper()
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), words) {
		t.Fatalf("%s: %v, want a refusal saying %q", what, err, words)
	}
}

// TestOutreachContractDrift: ReachOf, the List's filter, its tab counts and the lead's page agree
// lead by lead, over every bucket and reason, through the console's own fill.
func TestOutreachContractDrift(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lq := leadstoretest.Q(t)

	formLead := leadOf(t, "adatum.example")
	if _, err := b.AddContact(ctx, console.NewContact{LeadID: formLead, Kind: "form", Value: "https://adatum.example/contact"}); err != nil {
		t.Fatal(err)
	}
	retireAll(t, b, formLead, "email")
	emailLead := leadOf(t, "northwind.example")
	linkedIn, _ := linkedInLead(t, "contoso.example")
	phoneLead := leadOf(t, "tailspin.example")
	retireAll(t, b, phoneLead, "email")
	dead := deadEndLead(t, b)

	agree := func(t *testing.T, want map[int64][2]string) {
		t.Helper()
		w, err := b.leadsWeighing(ctx)
		if err != nil {
			t.Fatal(err)
		}
		all, err := b.LeadList(ctx, console.LeadListQuery{Status: leadstore.ListAll})
		if err != nil {
			t.Fatal(err)
		}
		for _, bucket := range slices.Concat(console.ReachTabs, []string{console.ReachOther}) {
			page, err := b.LeadList(ctx, console.LeadListQuery{Reach: bucket})
			if err != nil {
				t.Fatal(err)
			}
			if page.Filter.Status != "" || page.Filter.Reach != bucket {
				t.Errorf("%s echoed %+v", bucket, page.Filter)
			}
			if page.Total != page.ReachCounts.Of(bucket) || page.Total != all.ReachCounts.Of(bucket) {
				t.Errorf("%s: %d rows, tab says %d, all says %d", bucket, page.Total, page.ReachCounts.Of(bucket), all.ReachCounts.Of(bucket))
			}
			for _, r := range page.Rows {
				if r.Reach != bucket {
					t.Errorf("lead %d under %s says %s", r.LeadID, bucket, r.Reach)
				}
				of, err := lq.ReachOf(ctx, r.LeadID, w.countryOff)
				if err != nil {
					t.Fatal(err)
				}
				d := detailOf(t, b, r.LeadID)
				if of.Bucket != bucket || of.Reason != r.ReachReason || d.Reach != bucket || d.ReachReason != r.ReachReason {
					t.Errorf("lead %d: list %s/%s, ReachOf %+v, page %s/%s", r.LeadID, bucket, r.ReachReason, of, d.Reach, d.ReachReason)
				}
				if exp, ok := want[r.LeadID]; ok && (exp[0] != bucket || exp[1] != r.ReachReason) {
					t.Errorf("lead %d: %s/%s, want %v", r.LeadID, bucket, r.ReachReason, exp)
				}
				delete(want, r.LeadID)
			}
		}
		for id, exp := range want {
			t.Errorf("lead %d: never listed, want %v", id, exp)
		}
	}
	agree(t, map[int64][2]string{
		emailLead: {console.ReachEmail, ""},
		formLead:  {console.ReachManual, ""},
		linkedIn:  {console.ReachManual, ""},
		phoneLead: {console.ReachManual, ""},
		dead:      {console.ReachDead, console.ReachNotSearched},
	})

	// The lead that was Manual, once its note is marked sent, is Contacted on a Reach tab too: the
	// tab must not read open.
	batch := draftIt(t, linkedIn)
	rows := batchRows(t, batch)
	if _, err := b.MarkSent(ctx, console.MarkSentRequest{LeadID: linkedIn, MessageID: rows[0].ID, Hash: detailOf(t, b, linkedIn).Drafts[0].BatchHash}); err != nil {
		t.Fatal(err)
	}
	agree(t, map[int64][2]string{linkedIn: {console.ReachContacted, ""}})

	org, err := leadOrgID(ctx, lq, dead)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ status, reason string }{
		{"queued", console.ReachLooking},
		{"nothing", console.ReachNothing},
		{"found", console.ReachFound},
		{"error", console.ReachNotSearched},
	} {
		ownerExec(t, `UPDATE contact_research SET status = 'nothing', started_at = coalesce(started_at, now()), finished_at = now() WHERE org_id = $1 AND finished_at IS NULL`, org)
		ownerExec(t, `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor, status, started_at, finished_at, candidates, verified)
			SELECT l.org_id, l.id, l.campaign_id, 'cli', '{email}', 'operator', $2::text,
			  CASE WHEN $2::text = 'queued' THEN NULL ELSE now() END, CASE WHEN $2::text = 'queued' THEN NULL ELSE now() END,
			  CASE WHEN $2::text = 'found' THEN 1 ELSE 0 END, CASE WHEN $2::text = 'found' THEN 1 ELSE 0 END
			FROM lead l WHERE l.id = $1`, dead, tc.status)
		agree(t, map[int64][2]string{dead: {console.ReachDead, tc.reason}})
	}
	// A country that is off wins the reason, in the list and on the page alike; the bucket stays.
	ownerExec(t, `UPDATE org SET country = 'ZZ' WHERE id = $1`, org)
	agree(t, map[int64][2]string{dead: {console.ReachDead, console.ReachCountry}})

	ownerExec(t, `UPDATE lead SET state = 'skipped' WHERE id = $1`, dead)
	agree(t, map[int64][2]string{dead: {console.ReachOther, ""}})

	// An unknown next action is ignored with a note, as the list's other bad filters are.
	p, err := b.LeadList(ctx, console.LeadListQuery{Reach: "nonsense"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Filter.Reach != "" || p.Filter.Status != leadstore.ListOpen || len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "nonsense") {
		t.Fatalf("unknown reach: %+v, notes %q", p.Filter, p.Notes)
	}
	// The plain list's tabs count what they link to, not the open leads alone.
	p, err = b.LeadList(ctx, console.LeadListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if p.ReachCounts.Contacted == 0 {
		t.Fatalf("the Contacted tab reads %+v on the plain list", p.ReachCounts)
	}
}

// TestOutreachDraftItAsDesigned: a Draft it batch never blocks the morning batch (UnapprovedBatch),
// the List's Send now stops at it naming it, and its lead's page offers Discard and no second one.
func TestOutreachDraftItAsDesigned(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	batch := draftIt(t, ids[0])
	if id, ok, err := b.lq().UnapprovedBatch(ctx, leadstore.DefaultAccountID, false); err != nil || ok {
		t.Fatalf("UnapprovedBatch sees %d %v %v", id, ok, err)
	}
	_, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err == nil || !strings.Contains(err.Error(), "Send it from its draft") {
		t.Fatalf("the List's Send now: %v", err)
	}
	d := detailOf(t, b, ids[0])
	if d.DiscardBatchID != batch || !strings.Contains(d.DraftItBlock, "already in batch") {
		t.Fatalf("page: discard %d, block %q", d.DiscardBatchID, d.DraftItBlock)
	}
	_, err = b.RequestDraft(ctx, ids[0], "")
	wantOutreachRefusal(t, "Draft it on a batched lead", err, "already in batch")
	if _, err := b.DiscardDraft(ctx, ids[1], batch); !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("another lead's discard: %v", err)
	}
	if _, err := b.DiscardDraft(ctx, ids[0], batch); err != nil {
		t.Fatal(err)
	}
	d = detailOf(t, b, ids[0])
	if d.State != leadStateWords(leadstore.LeadNew) || d.DiscardBatchID != 0 || d.DraftItBlock != "" || len(d.Drafts) != 0 {
		t.Fatalf("after discard: %+v", d)
	}
}

// TestOutreachSharedContact: two leads on one org share a contact and both change list, whichever
// lead's page it was added on.
func TestOutreachSharedContact(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lq := leadstoretest.Q(t)
	first := leadOf(t, "litware.example")
	l, err := lq.LeadByID(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	var otherCampaign int64
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`INSERT INTO campaign (account_id, slug, name) VALUES (1, 'second', 'Second') RETURNING id`).Scan(&otherCampaign); err != nil {
		t.Fatal(err)
	}
	second, _, err := lq.UpsertLead(ctx, leadstore.LeadRow{OrgID: l.OrgID, Kind: l.Kind, ProbeID: l.ProbeID, Channel: l.Channel, RouteID: l.RouteID, CampaignID: otherCampaign})
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("the second campaign did not make a second lead")
	}
	retireAll(t, b, first, "email")
	for _, id := range []int64{first, second} {
		if got := detailOf(t, b, id); got.Reach != console.ReachDead || got.Channel != "none" {
			t.Fatalf("lead %d after retiring: %s on %s", id, got.Reach, got.Channel)
		}
	}
	// The address was retired, so adding it brings it back.
	line, err := b.AddContact(ctx, console.NewContact{LeadID: second, Kind: "email", Value: "Office@Litware.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "Back in use") {
		t.Fatalf("line %q", line)
	}
	for _, id := range []int64{first, second} {
		got := detailOf(t, b, id)
		if got.Reach != console.ReachEmail || got.Channel != "email" {
			t.Fatalf("lead %d after adding on the other: %s on %s", id, got.Reach, got.Channel)
		}
		if !slices.ContainsFunc(got.Contacts, func(c console.LeadContact) bool { return c.Chosen && c.Operator && c.Value == "office@litware.example" }) {
			t.Fatalf("lead %d contacts: %+v", id, got.Contacts)
		}
	}
}

// TestOutreachHandContactMovesList: a contact added by hand moves a dead-end lead to Email-ready at
// once, retiring moves it back, and both are on the timeline; a pasted LinkedIn address is
// normalised and a bad one refused in plain words.
func TestOutreachHandContactMovesList(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lead := deadEndLead(t, b)

	if _, err := b.AddContact(ctx, console.NewContact{LeadID: lead, Kind: "email", Value: "hello@litware.example", Note: "from their card"}); err != nil {
		t.Fatal(err)
	}
	d := detailOf(t, b, lead)
	if d.Reach != console.ReachEmail || d.Channel != "email" {
		t.Fatalf("after add: %s on %s", d.Reach, d.Channel)
	}
	i := slices.IndexFunc(d.Contacts, func(c console.LeadContact) bool { return c.Value == "hello@litware.example" })
	if i < 0 || d.Contacts[i].Provenance != console.ProvenanceOperator || d.Contacts[i].Note != "from their card" || d.Contacts[i].MailServer != "" {
		t.Fatalf("contacts %+v", d.Contacts)
	}
	ref, _ := console.ParseContactRef(d.Contacts[i].CID)
	if _, err := b.RetireContact(ctx, lead, ref, "they left"); err != nil {
		t.Fatal(err)
	}
	if d = detailOf(t, b, lead); d.Reach != console.ReachDead || d.Channel != "none" {
		t.Fatalf("after retire: %s on %s", d.Reach, d.Channel)
	}
	lines := timelineLines(t, b, lead)
	for _, want := range []string{"Contact added · Email | hello@litware.example · from their card", "Contact retired · Email | hello@litware.example · they left"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("timeline lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "marked as yours · Email | hello@litware.example") {
		t.Fatalf("one add shows twice:\n%s", lines)
	}

	_, err := b.AddContact(ctx, console.NewContact{LeadID: lead, Kind: "linkedin", Value: "https://example.org/in/nope"})
	wantOutreachRefusal(t, "a bad LinkedIn address", err, "not a LinkedIn")
	if !strings.HasSuffix(err.Error(), "Nothing was saved.") {
		t.Fatalf("refusal: %v", err)
	}
	if got := detailOf(t, b, lead).Reach; got != console.ReachDead {
		t.Fatalf("a refusal moved the lead: %s", got)
	}
	if _, err := b.AddContact(ctx, console.NewContact{LeadID: lead, Kind: "linkedin", Value: "www.linkedin.com/company/Harbour-Lane/about?trk=x"}); err != nil {
		t.Fatal(err)
	}
	d = detailOf(t, b, lead)
	if d.Reach != console.ReachManual || d.Channel != "linkedin" || !d.HasLinkedIn {
		t.Fatalf("LinkedIn: %s on %s", d.Reach, d.Channel)
	}
	if !slices.ContainsFunc(d.Contacts, func(c console.LeadContact) bool {
		return c.Chosen && c.Value == "https://linkedin.com/company/harbour-lane"
	}) {
		t.Fatalf("contacts %+v", d.Contacts)
	}

	// A contact on another org, or another lead's ref, is not found.
	if _, err := b.RetireContact(ctx, leadOf(t, "northwind.example"), ref, ""); !errors.Is(err, console.ErrNotFound) {
		t.Fatalf("another org's contact: %v", err)
	}
}

// TestOutreachWarnings: a country off with his own route, and a personal address where individuals
// are treated differently, each show one line and block nothing. No raw place id reaches the page.
func TestOutreachWarnings(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lq := leadstoretest.Q(t)
	lead := deadEndLead(t, b)
	org, err := leadOrgID(ctx, lq, lead)
	if err != nil {
		t.Fatal(err)
	}
	warned := func(c console.LeadContact) bool { return c.Warning != "" }

	ownerExec(t, `UPDATE org SET country = 'ZZ', domain = 'overture:08f1', name = '' WHERE id = $1`, org)
	ownerExec(t, `UPDATE site_probe SET site_name = '' WHERE id = (SELECT probe_id FROM lead WHERE id = $1)`, lead)
	if _, err := b.AddContact(ctx, console.NewContact{LeadID: lead, Kind: "linkedin", Value: "https://linkedin.com/company/harbour-lane"}); err != nil {
		t.Fatal(err)
	}
	d := detailOf(t, b, lead)
	if d.Reach != console.ReachManual || d.Channel != "linkedin" {
		t.Fatalf("own route in a country that is off: %s on %s", d.Reach, d.Channel)
	}
	i := slices.IndexFunc(d.Contacts, warned)
	if i < 0 || !strings.Contains(d.Contacts[i].Warning, "off in your settings") {
		t.Fatalf("contacts %+v", d.Contacts)
	}
	if strings.HasPrefix(d.Name, leadstore.PlaceOrgPrefix) || d.Sector != "" && d.SectorByRule && d.SectorWords == "" {
		t.Fatalf("page name %q, sector %q", d.Name, d.Sector)
	}

	code := ""
	table := compliance.Default()
	for _, c := range table.Codes() {
		if row, _ := table.Lookup(c); row.Individuals.Email != compliance.IndividualsSame {
			code = c
			break
		}
	}
	if code == "" {
		t.Fatal("no country treats individuals differently")
	}
	ownerExec(t, `UPDATE org SET country = $2 WHERE id = $1`, org, code)
	if _, err := b.AddContact(ctx, console.NewContact{LeadID: lead, Kind: "email", Value: "jo.bloggs@harbour.example"}); err != nil {
		t.Fatalf("a personal address is never blocked: %v", err)
	}
	if _, err := b.AddContact(ctx, console.NewContact{LeadID: lead, Kind: "email", Value: "info@harbour.example"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range detailOf(t, b, lead).Contacts {
		looks := strings.Contains(c.Warning, "looks personal")
		if looks != (c.Value == "jo.bloggs@harbour.example") {
			t.Errorf("%s: warning %q", c.Value, c.Warning)
		}
	}
}

// TestOutreachMarkSent: Mark sent moves the lead to Contacted at the hash he was shown, and the
// LinkedIn message after they accept can still be edited and marked sent (C3).
func TestOutreachMarkSent(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lead, url := linkedInLead(t, "contoso.example")
	draftIt(t, lead)
	d := detailOf(t, b, lead)
	if len(d.Drafts) != 2 || !d.Drafts[0].Note || d.Drafts[1].Note {
		t.Fatalf("drafts %+v", d.Drafts)
	}
	note, message := d.Drafts[0], d.Drafts[1]
	if note.Label != "Connection note" || message.Label != "After they accept" || note.TargetURL != url ||
		!note.CanMarkSent || !note.Editable || note.Origin != console.OriginDraftIt || note.Writer != console.WriterClaude {
		t.Fatalf("note %+v", note)
	}
	if d.NoteMax != 200 {
		t.Fatalf("note max %d", d.NoteMax)
	}

	_, err := b.MarkSent(ctx, console.MarkSentRequest{LeadID: lead, MessageID: note.MessageID, Hash: "000000000000"})
	wantOutreachRefusal(t, "a hash no longer the batch's", err, "This draft changed; reload.")
	_, err = b.MarkSent(ctx, console.MarkSentRequest{LeadID: lead, MessageID: note.MessageID, Hash: "short"})
	wantOutreachRefusal(t, "a short hash", err, "This draft changed; reload.")
	if _, err := b.MarkSent(ctx, console.MarkSentRequest{LeadID: leadOf(t, "northwind.example"), MessageID: note.MessageID, Hash: note.BatchHash}); !errors.Is(err, console.ErrNotFound) {
		t.Fatalf("another lead's message: %v", err)
	}
	if _, err := b.SaveDraft(ctx, console.DraftEdit{LeadID: lead, MessageID: note.MessageID, Body: note.Body + " " + strings.Repeat("Long ", 60)}); !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("a note over the limit: %v", err)
	}
	const edited = "I build sites for organisations like yours."
	if _, err := b.SaveDraft(ctx, console.DraftEdit{LeadID: lead, MessageID: note.MessageID, Body: edited}); err != nil {
		t.Fatal(err)
	}
	d = detailOf(t, b, lead)
	if got := d.Drafts[0]; strings.TrimSpace(got.Body) != edited || !got.Edited || got.OriginalBody != note.Body || got.BatchHash != note.BatchHash {
		t.Fatalf("after the edit %+v", got)
	}

	if _, err := b.MarkSent(ctx, console.MarkSentRequest{LeadID: lead, MessageID: note.MessageID, Hash: note.BatchHash}); err != nil {
		t.Fatal(err)
	}
	d = detailOf(t, b, lead)
	if d.Reach != console.ReachContacted {
		t.Fatalf("after Mark sent: %s", d.Reach)
	}
	if got := d.Drafts[0]; got.CanMarkSent || got.Editable || got.State != "Marked sent" {
		t.Fatalf("the note %+v", got)
	}
	if got := d.Drafts[1]; !got.CanMarkSent || !got.Editable || !got.DueNow {
		t.Fatalf("the message after they accept %+v", got)
	}
	if !strings.Contains(d.DraftItBlock, "Already contacted") {
		t.Fatalf("block %q", d.DraftItBlock)
	}
	if _, err := b.SaveDraft(ctx, console.DraftEdit{LeadID: lead, MessageID: message.MessageID, Body: message.Body + "\n\nHappy to talk."}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.MarkSent(ctx, console.MarkSentRequest{LeadID: lead, MessageID: message.MessageID, Hash: d.Drafts[1].BatchHash}); err != nil {
		t.Fatal(err)
	}
	lines := timelineLines(t, b, lead)
	for _, want := range []string{"Drafted · LinkedIn, step 1", "You edited LinkedIn, step 1", "Marked sent · LinkedIn, step 1", "Marked sent · LinkedIn, step 2"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("timeline lacks %q:\n%s", want, lines)
		}
	}
	_, err = b.MarkSent(ctx, console.MarkSentRequest{LeadID: lead, MessageID: message.MessageID, Hash: d.Drafts[1].BatchHash})
	wantOutreachRefusal(t, "marked twice", err, "already be marked sent")
}

// TestOutreachRequestDraft: Draft it writes the row and the request file, a second press shows the
// time of the first, and the page polls.
func TestOutreachRequestDraft(t *testing.T) {
	ctx := context.Background()
	b, dir := outreachWorld(t)
	lead := leadOf(t, "contoso.example")
	if _, err := b.RequestDraft(ctx, lead, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run", draftItAction)); err != nil {
		t.Fatalf("no request file: %v", err)
	}
	req, err := b.DraftStatus(ctx, lead)
	if err != nil || req == nil || !req.Open() {
		t.Fatalf("status %+v %v", req, err)
	}
	at := req.RequestedAt.In(b.loc).Format("15:04")
	_, err = b.RequestDraft(ctx, lead, "")
	wantOutreachRefusal(t, "a second press", err, at)
	d := detailOf(t, b, lead)
	if d.DraftItBlock != "Asked for at "+at || d.DraftRequest == nil {
		t.Fatalf("block %q, request %+v", d.DraftItBlock, d.DraftRequest)
	}
	dead := deadEndLead(t, b)
	_, err = b.RequestDraft(ctx, dead, "")
	wantOutreachRefusal(t, "no way to reach it", err, "no way to reach it")
	if _, err := b.RequestDraft(ctx, 999999, ""); !errors.Is(err, console.ErrNotFound) {
		t.Fatalf("no such lead: %v", err)
	}
}

// TestOutreachProposalsAndSector: a proposal added as sent sets the stage, and its two rows read as
// one on the timeline; a sector is off the closed list or refused, and the rule says it guessed.
func TestOutreachProposalsAndSector(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lead := leadOf(t, "northwind.example")
	other := leadOf(t, "tailspin.example")
	if _, err := b.AddProposal(ctx, console.NewProposal{LeadID: lead, Title: "Harbour Lane rebuild", Currency: "EUR", Status: console.ProposalSent}); err != nil {
		t.Fatal(err)
	}
	crm, err := b.LeadCRM(ctx, lead)
	if err != nil || crm.Stage != leadstore.StageProposal {
		t.Fatalf("stage %+v %v", crm, err)
	}
	d := detailOf(t, b, lead)
	if len(d.Proposals) != 1 || d.Proposals[0].Status != console.ProposalSent {
		t.Fatalf("proposals %+v", d.Proposals)
	}
	id := d.Proposals[0].ID
	lines := timelineLines(t, b, lead)
	if !strings.Contains(lines, "Proposal added · Sent | Harbour Lane rebuild") || strings.Contains(lines, "Proposal · Sent") {
		t.Fatalf("timeline:\n%s", lines)
	}
	if _, err := b.SetProposalStatus(ctx, console.ProposalStatusChange{LeadID: other, ProposalID: id, Status: console.ProposalAccepted}); !errors.Is(err, console.ErrNotFound) {
		t.Fatalf("another lead's proposal: %v", err)
	}
	if _, err := b.SetProposalStatus(ctx, console.ProposalStatusChange{LeadID: lead, ProposalID: id, Status: console.ProposalAccepted, Note: "they said yes"}); err != nil {
		t.Fatal(err)
	}
	if crm, _ = b.LeadCRM(ctx, lead); crm.Stage != leadstore.StageProposal {
		t.Fatalf("accepted forced the stage: %+v", crm)
	}
	if d = detailOf(t, b, lead); d.Proposals[0].Status != console.ProposalAccepted || d.Proposals[0].StatusNote != "they said yes" {
		t.Fatalf("proposals %+v", d.Proposals)
	}

	if d.Sector != "" || d.SectorByRule || len(d.SectorOptions) == 0 {
		t.Fatalf("sector %q %v", d.Sector, d.SectorByRule)
	}
	_, err = b.SetSector(ctx, lead, "nonsense")
	wantOutreachRefusal(t, "a sector off the list", err, "not on the list")
	if _, err := b.SetSector(ctx, lead, "media"); err != nil {
		t.Fatal(err)
	}
	if d = detailOf(t, b, lead); d.Sector != "media" || d.SectorWords != "Media" || d.SectorByRule {
		t.Fatalf("set sector %q %q %v", d.Sector, d.SectorWords, d.SectorByRule)
	}
	if !strings.Contains(timelineLines(t, b, lead), "Sector set · Media") {
		t.Fatal("no sector on the timeline")
	}
	ownerExec(t, `UPDATE org SET domain = 'tailspin.ac.uk' WHERE domain = 'tailspin.example'`)
	if d = detailOf(t, b, other); d.Sector != "education" || !d.SectorByRule {
		t.Fatalf("rule sector %q %v", d.Sector, d.SectorByRule)
	}
}

// TestOutreachProof: the page gets the closed list, a link is stored even when not public, and a
// sector off the list is refused.
func TestOutreachProof(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	p, err := b.Proofs(ctx)
	if err != nil || len(p.Proofs) != 0 || len(p.Sectors) == 0 {
		t.Fatalf("empty library %+v %v", p, err)
	}
	if _, err := b.AddProof(ctx, console.NewProof{Title: "Harbour Lane", Line: "We rebuilt their site.", Sectors: []string{"nonsense"}}); !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("a sector off the list: %v", err)
	}
	if _, err := b.AddProof(ctx, console.NewProof{Title: "", Line: "x", Sectors: []string{"education"}}); !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("no title: %v", err)
	}
	if _, err := b.AddProof(ctx, console.NewProof{Title: "Harbour Lane", Line: "We rebuilt their site.", URL: "https://work.example/harbour", Sectors: []string{"education", "media"}}); err != nil {
		t.Fatal(err)
	}
	p, err = b.Proofs(ctx)
	if err != nil || len(p.Proofs) != 1 {
		t.Fatalf("proofs %+v %v", p, err)
	}
	got := p.Proofs[0]
	if got.URL != "https://work.example/harbour" || got.Public || len(got.Sectors) != 2 || got.Sectors[0].Label != "Education" {
		t.Fatalf("proof %+v", got)
	}
	if _, err := b.RetireProof(ctx, got.ID, "old"); err != nil {
		t.Fatal(err)
	}
	if p, err = b.Proofs(ctx); err != nil || len(p.Proofs) != 0 {
		t.Fatalf("after retire %+v %v", p, err)
	}
	if _, err := b.RetireProof(ctx, 999999, ""); !errors.Is(err, console.ErrNotFound) {
		t.Fatalf("no such proof: %v", err)
	}
}

// TestOutreachCLI: the CLI's verbs write what the console's actions write, through the same functions.
func TestOutreachCLI(t *testing.T) {
	b, _ := outreachWorld(t)
	lead := deadEndLead(t, b)
	id := strconv.FormatInt(lead, 10)

	wantLines(t, "add", mustRun(t, "leads", "contacts", "add", id, "--kind", "email", "--value", "hello@litware.example", "--note", "by hand"), "Contact added.")
	if got := detailOf(t, b, lead); got.Reach != console.ReachEmail {
		t.Fatalf("after the CLI add: %s", got.Reach)
	}
	list := mustRun(t, "leads", "contacts", "list", id)
	wantLines(t, "list", list, "hello@litware.example", "added by you", "by hand")
	ref := contactRefs(t, b, lead, "email")[0].String()
	wantLines(t, "edit", mustRun(t, "leads", "contacts", "edit", id, ref, "--value", "office2@litware.example"), "The old one stays")
	refs := contactRefs(t, b, lead, "email")
	wantLines(t, "retire", mustRun(t, "leads", "contacts", "retire", id, refs[0].String()), "Contact retired.")
	if got := detailOf(t, b, lead); got.Reach != console.ReachDead {
		t.Fatalf("after the CLI retire: %s", got.Reach)
	}
	code, _, errOut := leadsRun(t, "leads", "contacts", "add", id, "--kind", "linkedin", "--value", "nope")
	wantRefused(t, "a bad LinkedIn address", code, errOut, "not a LinkedIn")

	wantLines(t, "sector", mustRun(t, "leads", "sector", "set", id, "media"), "sector saved")
	code, _, errOut = leadsRun(t, "leads", "sector", "set", id, "nonsense")
	wantRefused(t, "a sector off the list", code, errOut, "not on the list")

	wantLines(t, "proof", mustRun(t, "leads", "proof", "add", "--title", "Harbour Lane", "--line", "We rebuilt it.", "--sector", "media"), "added")
	wantLines(t, "proof list", mustRun(t, "leads", "proof", "list"), "Harbour Lane", "spoken line only")

	wantLines(t, "proposal", mustRun(t, "leads", "proposals", "add", id, "--title", "Rebuild", "--value-cents", "500000", "--currency", "EUR"), "added")
	wantLines(t, "proposal list", mustRun(t, "leads", "proposals", "list", id), "Rebuild", "sent")
	if crm, err := b.LeadCRM(context.Background(), lead); err != nil || crm.Stage != leadstore.StageProposal {
		t.Fatalf("stage %+v %v", crm, err)
	}
	p := detailOf(t, b, lead).Proposals[0]
	wantLines(t, "proposal status", mustRun(t, "leads", "proposals", "status", strconv.FormatInt(p.ID, 10), "declined"), "declined")
}

// TestOutreachPagesRender: the real backend through the real templates: the lead page shows its
// cards with no raw place id, a form posted through the console moves the lead, and the Reach tab
// lists a contacted lead.
func TestOutreachPagesRender(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lq := leadstoretest.Q(t)
	lead := deadEndLead(t, b)
	org, err := leadOrgID(ctx, lq, lead)
	if err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE org SET domain = 'overture:08f1', name = 'Harbour Lane Studio' WHERE id = $1`, org)
	ownerExec(t, `UPDATE site_probe SET site_name = '' WHERE id = (SELECT probe_id FROM lead WHERE id = $1)`, lead)
	c := newConsoleClient(t, b, nil)
	id := strconv.FormatInt(lead, 10)

	page := func(path string) string {
		t.Helper()
		w := c.get(path, false)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	body := page("/leads/" + id)
	for _, want := range []string{`id="next-step"`, `id="drafts"`, `id="contacts"`, `id="proposals"`, `id="sector"`, "Harbour Lane Studio", "Not searched yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("the lead page lacks %q", want)
		}
	}
	if strings.Contains(body, "overture:") {
		t.Error("a raw place id reached the page")
	}

	if w := c.post("/leads/"+id+"/contacts", url.Values{"kind": {"email"}, "value": {"hello@harbour.example"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("add a contact: %d", w.Code)
	}
	body = page("/leads/" + id)
	if !strings.Contains(body, "hello@harbour.example") || !strings.Contains(body, "badge-good") {
		t.Fatal("the lead page did not move after the add")
	}
	w := c.post("/leads/"+id+"/contacts", url.Values{"kind": {"linkedin"}, "value": {"not a link"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "not a LinkedIn") || !strings.Contains(w.Body.String(), "not a link") {
		t.Fatalf("a bad LinkedIn address: %d", w.Code)
	}
	body = page("/leads?reach=email")
	if !strings.Contains(body, "Harbour Lane Studio") || strings.Contains(body, "overture:") {
		t.Fatal("the Email-ready tab")
	}
	page("/leads/proof")
}
