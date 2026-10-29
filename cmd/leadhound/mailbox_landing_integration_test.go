//go:build integration

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// landingTransport sends https://<answering>/ to srv and fails every other host, so
// mailbox.LandingURL runs for real (redirects, limits) without the network.
type landingTransport struct {
	srv       *httptest.Server
	answering map[string]bool
}

func (t landingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != strings.TrimPrefix(t.srv.URL, "https://") && !t.answering[r.URL.Host] {
		return nil, errors.New("no answer")
	}
	r2 := r.Clone(r.Context())
	r2.URL.Host = strings.TrimPrefix(t.srv.URL, "https://")
	r2.Host = ""
	return t.srv.Client().Transport.RoundTrip(r2)
}

// TestRunChecksLandingURL: a check stores where its domain's website lands, after the
// redirects; a site that does not answer stores NULL and a note in the job's output, never
// in the fix; and the outcome and fix are the same either way.
func TestRunChecksLandingURL(t *testing.T) {
	w := newCoreWorld(t)
	ctx := context.Background()
	const web = "web.example"
	dns := passingDNS()
	dns[web] = []string{"v=spf1 -all"}
	dns["sel1._domainkey."+web] = []string{"v=DKIM1; p=MIIB"}
	dns["_dmarc."+web] = []string{"v=DMARC1; p=none"}
	leadsMail.resolver = dns
	passA := seedMailbox(t, w.lq, "a@"+testSenderDomain)
	passB := seedMailbox(t, w.lq, "b@"+web)
	failA := seedMailbox(t, w.lq, "c@"+testSenderDomain, withSelector("nope"))
	failB := seedMailbox(t, w.lq, "d@"+web, withSelector("nope"))
	wrongKey := seedMailbox(t, w.lq, "key@"+web)
	ownerExec(t, `UPDATE sender_mailbox SET sealed_by = 'another-key' WHERE id = $1`, wrongKey.ID)
	all := []leadstore.Mailbox{passA, passB, failA, failB, wrongKey}

	// / redirects twice, to /en/.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(rw, r, "/home", http.StatusMovedPermanently)
		case "/home":
			http.Redirect(rw, r, "/en/", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)
	saved := landingURL
	t.Cleanup(func() { landingURL = saved })
	useLanding := func(answering ...string) {
		tr := landingTransport{srv: srv, answering: map[string]bool{}}
		for _, h := range answering {
			tr.answering[h] = true
		}
		client := &http.Client{Transport: tr}
		landingURL = func(ctx context.Context, domain string) (string, error) {
			return mailbox.LandingURL(ctx, client, domain)
		}
	}
	type seen struct {
		outcome, fix, note string
		landing            *string
	}
	run := func() map[int64]seen {
		t.Helper()
		for _, m := range all {
			if _, err := w.lq.RequestCheck(ctx, m.ID, leadstore.ActorOperator); err != nil {
				t.Fatal(err)
			}
		}
		done, err := runChecks(ctx, w.lq, w.key, w.now)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]seen{}
		for _, d := range done {
			lc := w.status(t, d.MailboxID).LastCheck
			if lc == nil || lc.ID != d.CheckID {
				t.Fatalf("mailbox %d: last check %+v, want check %d", d.MailboxID, lc, d.CheckID)
			}
			out[d.MailboxID] = seen{lc.Outcome, lc.Fix, d.Note, lc.LandingURL}
		}
		if len(out) != len(all) {
			t.Fatalf("%d mailboxes checked, want %d", len(out), len(all))
		}
		return out
	}

	useLanding(testSenderDomain, web)
	answered := run()
	useLanding()
	silent := run()
	useLanding(testSenderDomain)
	mixed := run()

	want := map[int64]string{passA.ID: leadstore.CheckPassed, passB.ID: leadstore.CheckPassed,
		failA.ID: leadstore.CheckFailed, failB.ID: leadstore.CheckFailed, wrongKey.ID: leadstore.CheckDidNotRun}
	final := srv.URL + "/en/"
	for _, m := range all {
		a, s, x := answered[m.ID], silent[m.ID], mixed[m.ID]
		if a.outcome != want[m.ID] || s.outcome != a.outcome || x.outcome != a.outcome {
			t.Errorf("%s: outcomes %s / %s / %s, want %s each", m.Address, a.outcome, s.outcome, x.outcome, want[m.ID])
		}
		if s.fix != a.fix || x.fix != a.fix || strings.Contains(s.fix, mailbox.LandingNote) {
			t.Errorf("%s: fixes %q / %q / %q, want the same, without the note", m.Address, a.fix, s.fix, x.fix)
		}
		if m.ID == wrongKey.ID {
			if a.landing != nil || s.landing != nil || a.note != "" || s.note != "" {
				t.Errorf("a check that did not run: %+v / %+v, want no landing and no note", a, s)
			}
			continue
		}
		if a.landing == nil || *a.landing != final || a.note != "" {
			t.Errorf("%s answered: landing %v note %q, want %q and no note", m.Address, a.landing, a.note, final)
		}
		if s.landing != nil || s.note != mailbox.LandingNote {
			t.Errorf("%s silent: landing %v note %q, want NULL and the note", m.Address, s.landing, s.note)
		}
		onWeb := strings.HasSuffix(m.Address, "@"+web)
		if (x.landing == nil) != onWeb {
			t.Errorf("%s mixed: landing %v, want NULL only for %s", m.Address, x.landing, web)
		}
	}
}
