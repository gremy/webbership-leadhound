package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

func TestSendRunNote(t *testing.T) {
	r := &sendReport{live: true, sent: []string{"a.example"}, refused: []string{"message 3 (tampered)"}, maybe: []int64{4},
		retry: 1, cancelled: 2, held: map[outreach.HoldReason]int{outreach.HoldCap: 5, outreach.HoldNotDue: 9}, overMax: 1}
	r.note(noteTampered)
	r.note(noteTampered)
	got := r.runNote(errors.New("boom"))
	want := "live · sent 1 · refused 1 · maybe_sent 1 · retry 1 · cancelled 2 · skipped 0 · held cap 5, not_due 9, max 1 · notes tampered · error"
	if got != want {
		t.Errorf("note\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "a.example") || strings.Contains(got, "boom") {
		t.Error("the note names a domain or an error text")
	}
	if n := (&sendReport{held: map[outreach.HoldReason]int{}}).runNote(nil); n != "shadow · sent 0 · refused 0 · maybe_sent 0 · retry 0 · cancelled 0 · skipped 0" {
		t.Errorf("empty note %q", n)
	}
}

func TestMatcher(t *testing.T) {
	refs := []leadstore.SentRef{
		{ID: 1, MessageID: "a1@sender.example", LeadID: 10, ToAddress: "info@acme.example", OrgDomain: "acme.example", LeadKind: leadstore.LeadUnsupported},
		{ID: 2, MessageID: "a2@sender.example", LeadID: 10, ToAddress: "info@acme.example", OrgDomain: "acme.example", LeadKind: leadstore.LeadUnsupported},
		{ID: 3, MessageID: "s1@sender.example", LeadID: 11, ToAddress: "me@operator.example", OrgDomain: "selftest-1", LeadKind: leadstore.LeadSelftest},
	}
	m := newMatcher(refs)
	if !m.ourID("a1@sender.example") || m.ourID("x@sender.example") {
		t.Error("ourID")
	}
	if !m.contacted("acme.example") || m.contacted("operator.example") {
		t.Error("contacted: a selftest's domain must not match (RA24)")
	}
	if r := m.resolve(mailbox.Result{Kind: mailbox.KindReply, MatchedMessageID: "a1@sender.example"}); r == nil || r.ID != 1 {
		t.Errorf("by id: %+v", r)
	}
	if r := m.resolve(mailbox.Result{Kind: mailbox.KindReply, FromDomain: "acme.example"}); r == nil || r.ID != 2 {
		t.Errorf("by domain, latest: %+v", r)
	}
	if r := m.resolve(mailbox.Result{Kind: mailbox.KindBounce, FinalRecipient: "info@acme.example"}); r == nil || r.ID != 2 {
		t.Errorf("bounce by final recipient: %+v", r)
	}
	if r := m.resolve(mailbox.Result{Kind: mailbox.KindBounce, FromDomain: "acme.example"}); r != nil {
		t.Errorf("a bounce matched by domain: %+v", r)
	}
	if r := m.resolve(mailbox.Result{Kind: mailbox.KindReply, FromDomain: "operator.example"}); r != nil {
		t.Errorf("a selftest matched by domain: %+v", r)
	}
}

func TestClassifyFetched(t *testing.T) {
	m := newMatcher([]leadstore.SentRef{{ID: 1, MessageID: "a1@sender.example", LeadID: 10, ToAddress: "info@acme.example", OrgDomain: "acme.example"}})
	in, domain := classifyFetched(mailbox.Fetched{UID: 5, Header: []byte("From: A <a@mail.acme.example>\r\nSubject: Re: hi\r\nIn-Reply-To: <a1@sender.example>\r\n\r\n"),
		FirstText: []byte("Please remove me from your list")}, m)
	if in.Kind != mailbox.KindOptOut || in.Matched == nil || in.FromDomain != "acme.example" || domain != "acme.example" || in.UID != 5 {
		t.Errorf("opt-out: %+v %q", in, domain)
	}
	// A reply quoting our email, our opt-out line included, is a reply (FO2, FO3).
	ownLine := `Not relevant? Reply "unsubscribe" and I won't write again.`
	m.ownLines = []string{ownLine, mailbox.SelftestLine}
	in, _ = classifyFetched(mailbox.Fetched{UID: 8, Header: []byte("From: A <a" + "@" + "mail.acme.example>\r\nSubject: Re: hi\r\n" +
		"In-Reply-To: <a1" + "@" + "sender.example>\r\n\r\n"),
		FirstText: []byte("Thanks, call me Tuesday.\r\n\r\n" + "From: Sam\r\nSent: Monday, 26 October 2026 18:10\r\n\r\nHello,\r\n" + ownLine + "\r\n")}, m)
	if in.Kind != mailbox.KindReply || in.Matched == nil {
		t.Errorf("quoted reply: %+v", in)
	}
	in, _ = classifyFetched(mailbox.Fetched{UID: 6, Header: []byte("From: Daemon <mailer-daemon@mx.other.example>\r\nReturn-Path: <>\r\n\r\n")}, m)
	if in.Kind != mailbox.KindUnmatched || in.Matched != nil {
		t.Errorf("a bounce of nothing of ours: %+v", in)
	}
	in, _ = classifyFetched(mailbox.Fetched{UID: 7, Header: []byte("not a header")}, m)
	if in.Kind != mailbox.KindUnmatched {
		t.Errorf("a broken header: %+v", in)
	}
}

func TestSendSetupHelpers(t *testing.T) {
	setup := leadsSetup{env: leadconfig.Env{Outreach: leadconfig.OutreachLive}}
	op := "me" + "@" + "operator.example"
	setup.leads.Sender.OperatorAddresses = []config.Secret{config.Secret(op)}
	if err := selftestAllowed(setup, op); err != nil {
		t.Errorf("operator refused: %v", err)
	}
	other := "x" + "@" + "operator.example"
	if err := selftestAllowed(setup, other); !errors.Is(err, errUsage) || strings.Contains(err.Error(), other) {
		t.Errorf("stranger: %v", err)
	}
	setup.env.Outreach = ""
	if err := selftestAllowed(setup, op); !errors.Is(err, errUsage) {
		t.Errorf("shadow selftest: %v", err)
	}
	if dkimSelector(leadconfig.Config{}) != leadconfig.DefaultDKIMSelector {
		t.Error("default selector")
	}
}

// TestSendMailboxWords: the reasons a mailbox is paused for, and the run note and report of
// messages held for their mailbox and of a missing key.
func TestSendMailboxWords(t *testing.T) {
	for stage, want := range map[string]string{
		"auth": "the server refused the login", "mail": "the server refused mail from this address", "data": "the server refused the account at data",
	} {
		if got := accountReason(stage); got != want {
			t.Errorf("accountReason(%q) = %q, want %q", stage, got, want)
		}
	}
	if got := policyReason("sender.example", [3]int{5, 7, 1}); got != "a receiving server blocked mail from sender.example (5.7.1)" {
		t.Errorf("policyReason = %q", got)
	}
	r := &sendReport{live: true, held: map[outreach.HoldReason]int{holdMailbox: 2}}
	r.note(noteKeyMissing)
	if got, want := r.runNote(nil), "live · sent 0 · refused 0 · maybe_sent 0 · retry 0 · cancelled 0 · skipped 0 · held mailbox 2 · notes mailbox key missing"; got != want {
		t.Errorf("note\n got %q\nwant %q", got, want)
	}
	var b strings.Builder
	r.print(&b)
	if !strings.Contains(b.String(), "Held for a later run: its mailbox cannot send now 2.") {
		t.Errorf("report:\n%s", b.String())
	}
}

// TestSelftestBodyHasNoOptOutWord (FO3): once our own lines are taken out, the selftest body
// holds no opt-out word, so a reply quoting it, in any client's quote shape, is a reply.
func TestSelftestBodyHasNoOptOutWord(t *testing.T) {
	var cfg leadconfig.Config
	cfg.Sender.OptOutLines = map[string]string{"en": `Not relevant? Reply "unsubscribe" and I won't write again.`,
		"ro": "Nu vă interesează? Răspundeți cu „dezabonare” și nu vă mai scriu."}
	lines := optOutLinesOf(cfg)
	if !slices.Equal(lines[:2], []string{cfg.Sender.OptOutLines["en"], cfg.Sender.OptOutLines["ro"]}) ||
		!slices.Contains(lines, mailbox.SelftestLine) || !slices.Contains(lines, mailbox.OldSelftestLine) {
		t.Fatalf("optOutLinesOf = %q", lines)
	}
	body := selftestBody(time.Date(2026, 10, 26, 18, 10, 0, 0, time.UTC))
	if !strings.Contains(body, mailbox.SelftestLine) {
		t.Fatalf("the selftest body does not carry mailbox.SelftestLine: %q", body)
	}
	own, tail := mailbox.OwnText([]byte(body), false, lines)
	if mailbox.HasOptOutWord(string(own)) || mailbox.HasOptOutWord(string(tail)) {
		t.Fatalf("the selftest body holds an opt-out word outside our lines: %q", own)
	}
}
