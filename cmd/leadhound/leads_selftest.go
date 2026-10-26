package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

func init() { leadsCommands["selftest"] = runLeadsSelftest }

// commandLeadsSelftest is the run row of `leads selftest`.
const commandLeadsSelftest = "leads-selftest"

const selftestUsage = "usage: leadhound leads selftest --to ADDRESS [--mailbox ID|ADDRESS] (--to: one of sender.operator_addresses in leads.yaml; --mailbox: the mailbox to test, needed when there is more than one)"

// runLeadsSelftest is `leads selftest --to ADDRESS [--mailbox ID|ADDRESS]`: it proves the
// live loop of one mailbox on the operator's own address only (RA14, RA24). It runs the send
// order (lock, key, DNS, sync), then writes a selftest org, lead and one-message batch from
// that mailbox approved on this command line, and sends that one message: it bypasses the
// cap and counts toward it. It needs LEADHOUND_OUTREACH=live; --mailbox is needed when more
// than one mailbox is not retired.
func runLeadsSelftest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads selftest", flag.ContinueOnError)
	to := fs.String("to", "", "the operator address to send to")
	mb := fs.String("mailbox", "", "the mailbox to send from: its id or address")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || strings.TrimSpace(*to) == "" {
		return usageError("%s", selftestUsage)
	}
	return leadsSendRun(ctx, stdout, sendOptions{selftestTo: strings.ToLower(strings.TrimSpace(*to)), selftestMailbox: *mb})
}

// selftestAllowed refuses a selftest that is not live or not to an operator address. The
// address is never echoed.
func selftestAllowed(setup leadsSetup, to string) error {
	if !setup.env.Live() {
		return usageError("a selftest sends one real message: it needs LEADHOUND_OUTREACH=live")
	}
	for _, a := range setup.leads.Sender.OperatorAddresses {
		if strings.EqualFold(strings.TrimSpace(a.Reveal()), to) {
			return nil
		}
	}
	return usageError("--to is not one of sender.operator_addresses in leads.yaml; a selftest only writes to your own address")
}

// createSelftest writes the selftest's rows in one transaction, its message from mb (its
// Message-ID on mb's domain, its From name mb's or sender.name) due now.
func (r *sendRun) createSelftest(ctx context.Context, now time.Time, mb *sendMailbox) (leadstore.Selftest, error) {
	msgID, err := outreach.NewMessageID(outreach.MessageIDDomain(mb.Address), rand.Reader)
	if err != nil {
		return leadstore.Selftest{}, err
	}
	var st leadstore.Selftest
	err = leadsInTx(ctx, r.s.Pool(), func(tq leadstore.Queries) error {
		var err error
		st, err = tq.CreateSelftest(ctx, leadstore.SelftestDraft{
			To:      r.opts.selftestTo,
			Subject: "Leadhound selftest",
			Body: fmt.Sprintf("Leadhound selftest, %s.\n\nReply to this message to test the reply sync; "+
				"a reply with the word unsubscribe tests the opt-out.\n", now.In(r.loc).Format("2 Jan 2006 15:04 MST")),
			MessageID: msgID,
			Due:       now,
			MailboxID: mb.ID,
			FromName:  cmpOr(mb.FromName, r.setup.leads.Sender.Name),
		})
		return err
	})
	return st, err
}
