//go:build integration

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// consentSeq is a call, then two emails only on their yes.
var consentSeq = []campaign.Step{
	{Step: 0, Type: campaign.StepCall, When: campaign.WhenAlways},
	{Step: 1, Type: campaign.StepEmail, When: campaign.WhenEmailConsent},
	{Step: 2, Type: campaign.StepEmail, AfterBusinessDays: 4, When: campaign.WhenEmailConsent},
}

// consentBatch drafts consentSeq for lead (the call dials fact; each email's stored
// recipient is to, "" as the batch drafts it now) in a live batch, approves it with the call
// due now, and returns the message ids by step.
func (w *sendWorld) consentBatch(t *testing.T, lead int64, domain string, fact int64, to string) map[int]int64 {
	t.Helper()
	ctx := context.Background()
	var drafts []leadstore.DraftRow
	first := ""
	for _, st := range consentSeq {
		d := leadstore.DraftRow{LeadID: lead, Step: st.Step, Channel: "email", To: to, Condition: st.When,
			Subject: fmt.Sprintf("As promised, note %d", st.Step), Body: fmt.Sprintf("Hello %s, note %d.\n", domain, st.Step),
			MessageID: fmt.Sprintf("consent%dl%ds%d@%s", len(drafts), lead, st.Step, testSenderDomain), MailboxID: w.mailbox.ID, FromName: w.fromName}
		switch {
		case st.Type == campaign.StepCall:
			d.Channel, d.To, d.MessageID, d.PhoneFactID = leadstore.ChannelPhone, "", "", fact
			d.Subject, d.Body = "Call "+domain, "Who I am\nWhy I call\nThe free report"
		case first == "":
			first = d.MessageID
		default:
			d.InReplyTo = first
		}
		drafts = append(drafts, d)
	}
	tx, err := storetest.AppConn(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := leadstore.NewQueries(tx)
	id, sum, err := q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{campaign.SequenceDefault: consentSeq}}, false, drafts, nil)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	now := w.clock.now()
	if _, err := q.ApproveBatch(ctx, id, hex.EncodeToString(sum), nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return now.Add(-time.Minute)
		}
		return time.Time{}
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	ms, err := q.BatchMessages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	out := map[int]int64{}
	for _, m := range ms {
		out[m.Step] = m.ID
	}
	return out
}

// TestConsentAddress: an email_consent step goes to the address the person gave on the
// call, and nowhere else: the address on file when they confirm it, the other one when they
// give another; no consent cancels the emails; an opt-out of the consented address after
// the call cancels them too; a row drafted with a recipient other than the consent is
// refused at send.
func TestConsentAddress(t *testing.T) {
	cases := []struct {
		name    string
		to      string // the stored recipient of the email rows
		consent string // "" connected without consent
		optOut  bool   // the consented address opts out before the email goes
		want    []string
		refused bool
	}{
		{name: "consent on file", consent: "info@consent-a.example", want: []string{"info@consent-a.example"}},
		{name: "consent to another address", consent: "owner@elsewhere.example", want: []string{"owner@elsewhere.example"}},
		{name: "no consent", consent: "", want: nil},
		{name: "consent then opt-out", consent: "owner@elsewhere.example", optOut: true, want: nil},
		{name: "stored recipient differs from the consent", to: "info@consent-e.example", consent: "owner@elsewhere.example", want: nil, refused: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := newSendWorld(t, true, 10)
			domain := fmt.Sprintf("consent-%c.example", 'a'+i)
			lead := w.lead(t, domain)
			fact := w.phoneFact(t, lead, cp3Number)
			ids := w.consentBatch(t, lead, domain, fact, tc.to)
			args := []string{"leads", "calls", "outcome", fmt.Sprint(ids[0]), "connected"}
			if tc.consent != "" {
				args = append(args, "--consent-address", tc.consent)
			}
			w.mustRun(t, args...)
			if tc.optOut {
				w.mustRun(t, "leads", "suppress", "add", "address", tc.consent, "--reason", "opt_out")
				if r := w.row(t, ids[1]); r.state != leadstore.MsgCancelled {
					t.Errorf("email step right after the opt-out = %+v, want cancelled (the suppression reaches the consent)", r)
				}
			}
			for range 3 {
				w.clock.set(w.clock.now().Add(24 * time.Hour))
				w.mustRun(t, "leads", "send")
			}
			got := w.smtp.recipients()
			if !slices.Equal(got, tc.want) {
				t.Errorf("SMTP recipients = %v, want %v", got, tc.want)
			}
			r := w.row(t, ids[1])
			switch {
			case tc.refused:
				if r.state != leadstore.MsgRefused {
					t.Errorf("email step = %+v, want refused", r)
				}
			case len(tc.want) == 0:
				if r.state != leadstore.MsgCancelled {
					t.Errorf("email step = %+v, want cancelled", r)
				}
			default:
				if r.state != leadstore.MsgSent {
					t.Errorf("email step = %+v, want sent", r)
				}
				refs, err := w.lq.SentRefs(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, ref := range refs {
					if ref.ID == ids[1] && ref.ToAddress != tc.consent {
						t.Errorf("sent ref to %q, want %q (what a bounce or an opt-out suppresses)", ref.ToAddress, tc.consent)
					}
				}
			}
		})
	}
}
