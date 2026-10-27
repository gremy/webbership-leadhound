package leadstoretest

import (
	"context"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// SeedMailbox adds a sending mailbox for address (a reserved name) through q, with
// placeholder sealed bytes that no key opens, servers on .invalid (ports 465 and 993), DKIM
// selector "lh", and its first check passed. It is for tests
// that need a mailbox to exist, never to send through.
func SeedMailbox(t testing.TB, q leadstore.Queries, address string) leadstore.Mailbox {
	t.Helper()
	ctx := context.Background()
	id, err := q.AddMailbox(ctx, leadstore.DefaultAccountID, leadstore.Mailbox{
		Address: address, Login: address,
		SMTPAddr: "smtp.seed.invalid:465", IMAPAddr: "imap.seed.invalid:993",
		DKIMSelector: "lh", PasswordSealed: []byte("placeholder, sealed by no key"), SealedBy: "placeholder",
	}, leadstore.ActorOperator)
	if err != nil {
		t.Fatalf("leadstoretest: seed mailbox %s: %v", address, err)
	}
	checks, err := q.RequestedChecks(ctx)
	if err != nil {
		t.Fatalf("leadstoretest: seed mailbox %s: %v", address, err)
	}
	yes := true
	for _, c := range checks {
		if c.MailboxID != id {
			continue
		}
		if err := q.FinishCheck(ctx, c.ID, leadstore.MailboxCheckResult{
			SMTPOK: &yes, IMAPOK: &yes, FoldersOK: &yes, SPF: &yes, DKIM: &yes, DMARC: &yes, Outcome: leadstore.CheckPassed,
		}, time.Now()); err != nil {
			t.Fatalf("leadstoretest: seed mailbox %s: %v", address, err)
		}
	}
	m, err := q.MailboxByID(ctx, id)
	if err != nil {
		t.Fatalf("leadstoretest: seed mailbox %s: %v", address, err)
	}
	return m
}
