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

const selftestUsage = "usage: leadhound leads selftest --to ADDRESS (one of sender.operator_addresses in leads.yaml)"

// runLeadsSelftest is `leads selftest --to ADDRESS`: it proves the live loop on the
// operator's own address only (RA14, RA24). It runs the send order (lock, DNS, sync), then
// writes a selftest org, lead and one-message batch approved on this command line, and sends
// that one message: it bypasses the cap and counts toward it. It needs LEADHOUND_OUTREACH=live.
func runLeadsSelftest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads selftest", flag.ContinueOnError)
	to := fs.String("to", "", "the operator address to send to")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || strings.TrimSpace(*to) == "" {
		return usageError("%s", selftestUsage)
	}
	return leadsSendRun(ctx, stdout, sendOptions{selftestTo: strings.ToLower(strings.TrimSpace(*to))})
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

// createSelftest writes the selftest's rows in one transaction, its message due now.
func (r *sendRun) createSelftest(ctx context.Context, now time.Time) (leadstore.Selftest, error) {
	msgID, err := outreach.NewMessageID(outreach.MessageIDDomain(r.setup.env.User.Reveal()), rand.Reader)
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
		})
		return err
	})
	return st, err
}
