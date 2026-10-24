package main

import (
	"errors"
	"strings"
	"testing"

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
	user := "outreach" + "@" + "Mail.Sender.example"
	if d, ok := sendingDomain(leadconfig.Env{User: config.Secret(user)}); !ok || d != "mail.sender.example" {
		t.Errorf("sendingDomain = %q %v", d, ok)
	}
	for _, bad := range []string{"", "outreach", "@sender.example", "outreach@"} {
		if _, ok := sendingDomain(leadconfig.Env{User: config.Secret(bad)}); ok {
			t.Errorf("sendingDomain(%q) ok", bad)
		}
	}
	setup := leadsSetup{env: leadconfig.Env{Outreach: leadconfig.OutreachLive, User: config.Secret(user)}}
	if got := strings.Join(liveMissing(setup), ","); got != "LEADHOUND_SMTP_ADDR,LEADHOUND_IMAP_ADDR,LEADHOUND_OUTREACH_PASSWORD" {
		t.Errorf("liveMissing = %s", got)
	}
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
	if dkimSelector(leadconfig.Config{}) != "spacemail" {
		t.Error("default selector")
	}
}
