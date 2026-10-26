package mailbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

func probe(t *testing.T, s *SMTPSender) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Probe(ctx)
}

// A probe logs in and leaves: no MAIL FROM, no RCPT, no body.
func TestProbeSMTPLoginOnly(t *testing.T) {
	both(t, func(t *testing.T, implicit bool) {
		f := &fake{}
		if err := probe(t, f.start(t, implicit)); err != nil {
			t.Fatalf("Probe: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.user != testUser || f.pass != testPass {
			t.Error("auth got the wrong user or password")
		}
		if f.from != "" || len(f.to) != 0 || f.body != nil {
			t.Errorf("a probe went past AUTH: from=%q to=%q body=%q", f.from, f.to, f.body)
		}
	})
}

func TestProbeSMTPFailures(t *testing.T) {
	t.Run("auth 535", func(t *testing.T) {
		f := &fake{auth: func(string, string) error {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "bad credentials " + testPass}
		}}
		err := probe(t, f.start(t, true))
		se := kindOf(t, err)
		if se.Kind != Account || se.Stage != "auth" {
			t.Fatalf("got %+v, want account at auth", se)
		}
		if f.from != "" {
			t.Error("MAIL was sent")
		}
		if got := FixFor(StepSMTP, err, fakeHost); !strings.Contains(got, "refused the login") {
			t.Errorf("fix = %q", got)
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		both(t, func(t *testing.T, implicit bool) {
			f := &fake{}
			s := f.start(t, implicit)
			s.RootCAs = x509.NewCertPool()
			err := probe(t, s)
			if se := kindOf(t, err); se.Kind != Retryable {
				t.Fatalf("got %+v, want retryable", se)
			}
			if f.user != "" {
				t.Error("credentials were sent over an unverified connection")
			}
			want := "The server's certificate did not check out for " + fakeHost + "."
			if got := FixFor(StepSMTP, err, fakeHost); got != want {
				t.Errorf("fix = %q, want %q", got, want)
			}
		})
	})
	t.Run("nothing listening", func(t *testing.T) {
		s := &fake{}
		sender := s.start(t, true)
		sender.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:1")
		}
		err := probe(t, sender)
		if se := kindOf(t, err); se.Kind != Retryable || se.Stage != "connect" {
			t.Fatalf("got %+v", se)
		}
		want := "Could not reach " + fakeHost + ". Check the server name and port."
		if got := FixFor(StepSMTP, err, fakeHost); got != want {
			t.Errorf("fix = %q, want %q", got, want)
		}
	})
	t.Run("config", func(t *testing.T) {
		s := &SMTPSender{Addr: fakeHost + ":25", User: "u", Password: "p"}
		if err := s.Probe(context.Background()); !errors.Is(err, ErrSMTPConfig) {
			t.Fatalf("err = %v, want ErrSMTPConfig", err)
		}
	})
}

// A 5.7.x at RCPT is a policy block; a 5.1.x is a bounce and not a block.
func TestIsPolicyBlock(t *testing.T) {
	rcpt := func(code int, enh smtp.EnhancedCode) error {
		f := &fake{rcpt: func(string) error { return &smtp.SMTPError{Code: code, EnhancedCode: enh, Message: "no"} }}
		_, err := send(t, f.start(t, true))
		return err
	}
	err := rcpt(550, smtp.EnhancedCode{5, 7, 1})
	if se := kindOf(t, err); !IsPolicyBlock(err) || se.Enhanced != [3]int{5, 7, 1} || se.Bounce {
		t.Errorf("550 5.7.1: block=%v %+v", IsPolicyBlock(err), se)
	}
	err = rcpt(550, smtp.EnhancedCode{5, 1, 1})
	if se := kindOf(t, err); IsPolicyBlock(err) || !se.Bounce {
		t.Errorf("550 5.1.1: block=%v %+v", IsPolicyBlock(err), se)
	}
	if err := rcpt(451, smtp.EnhancedCode{4, 7, 1}); IsPolicyBlock(err) {
		t.Error("a 4.7.1 greylist is a block")
	}
	// 5.7.x anywhere but RCPT is not a block of the domain.
	f := &fake{mail: func(string) error {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "sender rejected"}
	}}
	if _, err := send(t, f.start(t, true)); IsPolicyBlock(err) {
		t.Error("a 5.7.1 at MAIL is a domain block")
	}
	if IsPolicyBlock(nil) || IsPolicyBlock(errors.New("x")) {
		t.Error("non-SendError is a block")
	}
}

// probeIMAP is a loopback TLS IMAP server that accepts LOGIN and lists the given folders,
// each line as the attributes and name after "* LIST ".
func probeIMAP(t *testing.T, list []string) (addr string, roots *x509.CertPool) {
	t.Helper()
	cert, roots := testCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveProbeIMAP(conn, list)
		}
	}()
	return ln.Addr().String(), roots
}

func serveProbeIMAP(conn net.Conn, list []string) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	w := bufio.NewWriter(conn)
	say := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
	say("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] fake ready")
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		cmd, _, _ := strings.Cut(rest, " ")
		switch strings.ToUpper(cmd) {
		case "CAPABILITY":
			say("* CAPABILITY IMAP4rev1 AUTH=PLAIN")
			say(tag + " OK done")
		case "LOGIN":
			say(tag + " OK [CAPABILITY IMAP4rev1] logged in")
		case "LIST":
			for _, l := range list {
				say("* LIST " + l)
			}
			say(tag + " OK listed")
		case "LOGOUT":
			say("* BYE")
			say(tag + " OK bye")
			return
		default:
			say(tag + " BAD unknown")
		}
	}
}

func TestProbeIMAP(t *testing.T) {
	run := func(t *testing.T, list []string) (Mailboxes, error) {
		t.Helper()
		addr, roots := probeIMAP(t, list)
		r := testReader(fakeHost + ":993")
		redirect(r, addr)
		r.RootCAs = roots
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return ProbeIMAP(ctx, r)
	}
	t.Run("folders found", func(t *testing.T) {
		got, err := run(t, []string{`(\HasNoChildren) "/" INBOX`, `(\Sent) "/" "Sent Items"`, `(\Junk) "/" Spam`})
		if err != nil {
			t.Fatalf("ProbeIMAP: %v", err)
		}
		if got != (Mailboxes{Inbox: "INBOX", Sent: "Sent Items", Junk: "Spam"}) {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("junk missing", func(t *testing.T) {
		got, err := run(t, []string{`() "/" INBOX`, `(\Sent) "/" Sent`})
		if !errors.Is(err, ErrFoldersMissing) {
			t.Fatalf("err = %v, want ErrFoldersMissing", err)
		}
		if got.Sent != "Sent" || got.Junk != "" {
			t.Errorf("got %+v", got)
		}
		want := "Signed in, but the Sent or Junk folder was not found; replies in Junk would be missed."
		if fix := FixFor(StepIMAP, err, fakeHost); fix != want {
			t.Errorf("fix = %q", fix)
		}
	})
	t.Run("login refused", func(t *testing.T) {
		addr, roots := fakeIMAP(t) // refuses LOGIN and echoes the credentials
		r := testReader(fakeHost + ":993")
		redirect(r, addr)
		r.RootCAs = roots
		_, err := ProbeIMAP(context.Background(), r)
		if fix := FixFor(StepIMAP, err, fakeHost); !strings.Contains(fix, "refused the login") {
			t.Errorf("fix = %q (err %v)", fix, err)
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		addr, _ := probeIMAP(t, nil)
		r := testReader(fakeHost + ":993")
		redirect(r, addr)
		r.RootCAs = x509.NewCertPool()
		_, err := ProbeIMAP(context.Background(), r)
		want := "The server's certificate did not check out for " + fakeHost + "."
		if fix := FixFor(StepIMAP, err, fakeHost); fix != want {
			t.Errorf("fix = %q (err %v)", fix, err)
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		r := testReader(fakeHost + ":993")
		redirect(r, "127.0.0.1:1")
		_, err := ProbeIMAP(context.Background(), r)
		want := "Could not reach " + fakeHost + ". Check the server name and port."
		if fix := FixFor(StepIMAP, err, fakeHost); fix != want {
			t.Errorf("fix = %q (err %v)", fix, err)
		}
	})
}

func TestFixForDNS(t *testing.T) {
	const domain, sel = "sender.example", "s1"
	if (DNSResult{SPF: true, DKIM: true, DMARC: true}).Err(domain, sel) != nil {
		t.Fatal("a passing result is an error")
	}
	err := DNSResult{DKIM: true}.Err(domain, sel)
	fix := FixFor(StepDNS, err, domain)
	for _, want := range []string{"v=spf1", "at " + domain, "_dmarc." + domain} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix %q lacks %q", fix, want)
		}
	}
	if strings.Contains(fix, "_domainkey") {
		t.Errorf("fix %q names DKIM, which passed", fix)
	}
	fix = FixFor(StepDNS, DNSResult{SPF: true, DMARC: true}.Err(domain, sel), domain)
	if !strings.Contains(fix, sel+"._domainkey."+domain) || strings.Contains(fix, "spf1") || strings.Contains(fix, "_dmarc") {
		t.Errorf("dkim fix = %q", fix)
	}
	if FixFor(StepDNS, nil, domain) != "" {
		t.Error("no error, but a fix")
	}
}

// No fix, for any failure, carries the password or the user, even when the server echoes
// them back in its replies.
func TestFixForCarriesNoSecrets(t *testing.T) {
	echo := &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "bad " + testUser + " " + testPass}
	var errs []error
	for _, f := range []*fake{
		{auth: func(string, string) error { return echo }},
		{auth: func(string, string) error {
			return &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: testPass}
		}},
	} {
		errs = append(errs, probe(t, f.start(t, true)))
	}
	f := &fake{rcpt: func(string) error {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: testPass}
	}}
	_, err := send(t, f.start(t, true))
	errs = append(errs, err)
	addr, roots := fakeIMAP(t)
	r := testReader(fakeHost + ":993")
	redirect(r, addr)
	r.RootCAs = roots
	_, err = ProbeIMAP(context.Background(), r)
	errs = append(errs, err, errors.New("raw "+testPass), &IMAPError{Stage: "list", Text: testPass},
		&SendError{Kind: Refused, Stage: "rcpt", Code: 550, Text: testPass})
	for _, step := range []string{StepSMTP, StepIMAP, StepDNS} {
		for i, err := range errs {
			fix := FixFor(step, err, fakeHost)
			if fix == "" {
				t.Errorf("%s case %d: empty fix for %v", step, i, err)
			}
			for _, v := range []string{testPass, testUser, "pw-Zq81", "outreach"} {
				if strings.Contains(strings.ToLower(fix), strings.ToLower(v)) {
					t.Errorf("%s case %d: fix %q carries %q", step, i, fix, v)
				}
			}
		}
	}
}
