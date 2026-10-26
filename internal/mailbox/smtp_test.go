package mailbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// The fake server's name and every address are reserved names, built at run time.
const fakeHost = "mail.example"

var (
	testUser = "outreach" + "@" + "sender.example"
	testPass = "pw-Zq81" + "-test-only"
	testTo   = "Contact" + "@" + "northwind.example"
)

// testCert is a self-signed certificate for fakeHost and a pool that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: fakeHost},
		DNSNames:              []string{fakeHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// fake is an in-process SMTP server on loopback. Each hook, when set, answers that step.
type fake struct {
	auth func(user, pass string) error
	mail func(from string) error
	rcpt func(to string) error
	data func(c *smtp.Conn, body []byte) error

	mu       sync.Mutex
	user     string
	pass     string
	from     string
	to       []string
	body     []byte
	sessions int
}

type fakeSession struct {
	f *fake
	c *smtp.Conn
}

func (s *fakeSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *fakeSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		s.f.mu.Lock()
		s.f.user, s.f.pass = user, pass
		s.f.mu.Unlock()
		if s.f.auth != nil {
			return s.f.auth(user, pass)
		}
		return nil
	}), nil
}

func (s *fakeSession) Mail(from string, _ *smtp.MailOptions) error {
	s.f.mu.Lock()
	s.f.from = from
	s.f.mu.Unlock()
	if s.f.mail != nil {
		return s.f.mail(from)
	}
	return nil
}

func (s *fakeSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.f.rcpt != nil {
		if err := s.f.rcpt(to); err != nil {
			return err
		}
	}
	s.f.mu.Lock()
	s.f.to = append(s.f.to, to)
	s.f.mu.Unlock()
	return nil
}

func (s *fakeSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.f.mu.Lock()
	s.f.body = body
	s.f.mu.Unlock()
	if s.f.data != nil {
		return s.f.data(s.c, body)
	}
	return nil
}

func (*fakeSession) Reset()        {}
func (*fakeSession) Logout() error { return nil }

// start serves f on loopback, with implicit TLS or STARTTLS, and returns a sender whose
// Addr names port 465 or 587 on fakeHost and whose dial reaches the fake server.
func (f *fake) start(t *testing.T, implicit bool) *SMTPSender {
	t.Helper()
	cert, pool := testCert(t)
	tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv := smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
		f.mu.Lock()
		f.sessions++
		f.mu.Unlock()
		return &fakeSession{f: f, c: c}, nil
	}))
	srv.Domain = fakeHost
	srv.ErrorLog = log.New(io.Discard, "", 0)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := "587"
	if implicit {
		l = tls.NewListener(l, tlsConf)
		port = "465"
	} else {
		srv.TLSConfig = tlsConf
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	listen := l.Addr().String()
	return &SMTPSender{
		Addr:     net.JoinHostPort(fakeHost, port),
		User:     config.Secret(testUser),
		Password: config.Secret(testPass),
		RootCAs:  pool,
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, listen)
		},
	}
}

// both runs fn once over implicit TLS (465) and once over STARTTLS (587).
func both(t *testing.T, fn func(t *testing.T, implicit bool)) {
	t.Run("465", func(t *testing.T) { fn(t, true) })
	t.Run("587", func(t *testing.T) { fn(t, false) })
}

var testRaw = []byte("From: a\r\nTo: b\r\nSubject: hello\r\n\r\nBody line.\r\n")

func send(t *testing.T, s *SMTPSender) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Send(ctx, testUser, testTo, testRaw)
}

func kindOf(t *testing.T, err error) *SendError {
	t.Helper()
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("error %v (%T) is not a *SendError", err, err)
	}
	return se
}

func TestSMTPSendSuccess(t *testing.T) {
	both(t, func(t *testing.T, implicit bool) {
		f := &fake{}
		reply, err := send(t, f.start(t, implicit))
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if reply == "" {
			t.Error("empty reply")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.user != testUser || f.pass != testPass {
			t.Errorf("auth got the wrong user or password")
		}
		if f.from != testUser || len(f.to) != 1 || f.to[0] != testTo {
			t.Errorf("envelope from=%q to=%q", f.from, f.to)
		}
		if string(f.body) != string(testRaw) {
			t.Errorf("body = %q, want %q", f.body, testRaw)
		}
	})
}

// At RCPT only an address-specific failure suppresses: a 550, 551 or 553 whose enhanced
// status is 5.1.x. Any other 5xx refuses the message alone.
func TestSMTPRefusedAtRcpt(t *testing.T) {
	for _, tc := range []struct {
		code   int
		enh    smtp.EnhancedCode
		bounce bool
	}{
		{550, smtp.EnhancedCode{5, 1, 1}, true}, {551, smtp.EnhancedCode{5, 1, 6}, true}, {553, smtp.EnhancedCode{5, 1, 3}, true},
		{550, smtp.EnhancedCode{5, 1, 10}, true},
		{550, smtp.EnhancedCode{5, 7, 1}, false}, // policy: not the address
		{550, smtp.EnhancedCode{5, 2, 1}, false}, // mailbox disabled for now
		{550, smtp.NoEnhancedCode, false},
		{552, smtp.EnhancedCode{5, 1, 1}, false}, {554, smtp.EnhancedCode{5, 1, 1}, false},
	} {
		t.Run(fmt.Sprintf("%d %v", tc.code, tc.enh), func(t *testing.T) {
			f := &fake{rcpt: func(string) error {
				return &smtp.SMTPError{Code: tc.code, EnhancedCode: tc.enh, Message: "no such user"}
			}}
			_, err := send(t, f.start(t, true))
			se := kindOf(t, err)
			if se.Kind != Refused || se.Code != tc.code || se.Bounce != tc.bounce || se.Stage != "rcpt" {
				t.Errorf("got %+v, want refused at rcpt, code %d, bounce %v", se, tc.code, tc.bounce)
			}
			if f.body != nil {
				t.Error("a body reached the server")
			}
		})
	}
}

// A refusal at AUTH (any code) or a 5xx at MAIL FROM is about the account, not the message.
func TestSMTPAccountFailures(t *testing.T) {
	for name, f := range map[string]*fake{
		"auth 535": {auth: func(string, string) error {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "bad credentials"}
		}},
		"auth 454": {auth: func(string, string) error {
			return &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "try later"}
		}},
		"mail 550": {mail: func(string) error {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "sender rejected"}
		}},
		"mail 554 quota": {mail: func(string) error {
			return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "daily quota exceeded"}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := send(t, f.start(t, true))
			se := kindOf(t, err)
			if se.Kind != Account || se.Bounce {
				t.Errorf("got %+v, want an account failure without bounce", se)
			}
			if f.body != nil {
				t.Error("a body reached the server")
			}
		})
	}
	// A 4xx at MAIL FROM is still only "try later".
	f := &fake{mail: func(string) error {
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "busy"}
	}}
	_, err := send(t, f.start(t, true))
	if se := kindOf(t, err); se.Kind != Retryable || se.Stage != "mail" {
		t.Errorf("mail 451: %+v", se)
	}
}

func TestSMTPRetryableBeforeMail(t *testing.T) {
	both(t, func(t *testing.T, implicit bool) {
		f := &fake{auth: func(string, string) error {
			return &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "service not available"}
		}}
		_, err := send(t, f.start(t, implicit))
		se := kindOf(t, err)
		if se.Kind != Retryable || se.Code != 421 || se.Bounce {
			t.Errorf("got %+v, want retryable 421", se)
		}
		if f.from != "" {
			t.Error("MAIL was sent after a failed AUTH")
		}
	})
}

func TestSMTPRetryableAtRcpt(t *testing.T) {
	f := &fake{rcpt: func(string) error {
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "greylisted"}
	}}
	_, err := send(t, f.start(t, false))
	if se := kindOf(t, err); se.Kind != Retryable || se.Stage != "rcpt" {
		t.Errorf("got %+v, want retryable at rcpt", se)
	}
}

func TestSMTPUncertainAfterData(t *testing.T) {
	t.Run("drop", func(t *testing.T) {
		both(t, func(t *testing.T, implicit bool) {
			f := &fake{data: func(c *smtp.Conn, _ []byte) error {
				_ = c.Conn().Close()
				return nil
			}}
			_, err := send(t, f.start(t, implicit))
			if se := kindOf(t, err); se.Kind != Uncertain || se.Stage != "data" {
				t.Errorf("got %+v, want uncertain at data", se)
			}
		})
	})
	t.Run("refused after the final dot", func(t *testing.T) {
		f := &fake{data: func(*smtp.Conn, []byte) error {
			return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "content rejected"}
		}}
		_, err := send(t, f.start(t, true))
		if se := kindOf(t, err); se.Kind != Uncertain || se.Code != 554 {
			t.Errorf("got %+v, want uncertain 554", se)
		}
	})
}

func TestSMTPDeadline(t *testing.T) {
	t.Run("before data", func(t *testing.T) {
		release := make(chan struct{})
		f := &fake{mail: func(string) error { <-release; return nil }}
		s := f.start(t, true)
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := s.Send(ctx, testUser, testTo, testRaw)
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("Send took %v after the deadline", el)
		}
		se := kindOf(t, err)
		if se.Kind != Retryable || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %+v, want retryable deadline", se)
		}
	})
	t.Run("during data", func(t *testing.T) {
		release := make(chan struct{})
		f := &fake{data: func(*smtp.Conn, []byte) error { <-release; return nil }}
		s := f.start(t, true)
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := s.Send(ctx, testUser, testTo, testRaw)
		se := kindOf(t, err)
		if se.Kind != Uncertain || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %+v, want uncertain deadline", se)
		}
	})
}

func TestSMTPUntrustedCertificate(t *testing.T) {
	both(t, func(t *testing.T, implicit bool) {
		f := &fake{}
		s := f.start(t, implicit)
		s.RootCAs = x509.NewCertPool() // trusts nothing
		_, err := send(t, s)
		if se := kindOf(t, err); se.Kind != Retryable {
			t.Errorf("got %+v, want retryable", se)
		}
		if f.user != "" {
			t.Error("credentials were sent over an unverified connection")
		}
	})
}

func TestSMTPPortAndConfig(t *testing.T) {
	for _, addr := range []string{fakeHost + ":25", fakeHost + ":2525", fakeHost, ""} {
		s := &SMTPSender{Addr: addr, User: config.Secret(testUser), Password: config.Secret(testPass)}
		if _, err := s.Send(context.Background(), testUser, testTo, testRaw); !errors.Is(err, ErrSMTPConfig) {
			t.Errorf("Addr %q: err = %v, want ErrSMTPConfig", addr, err)
		}
	}
	s := &SMTPSender{Addr: fakeHost + ":465"}
	if _, err := s.Send(context.Background(), testUser, testTo, testRaw); !errors.Is(err, ErrSMTPConfig) {
		t.Errorf("no credentials: err = %v, want ErrSMTPConfig", err)
	}
}

// Every error path, with a server that echoes the user, the password and the recipient
// (in other letter cases too) back in its replies: no error carries any of them.
func TestSMTPErrorsCarryNoSecrets(t *testing.T) {
	echo := func(code int) error {
		return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{code / 100, 0, 0},
			Message: "bad " + testTo + " " + strings.ToLower(testTo) + " user " + strings.ToUpper(testUser) + " pass " + testPass}
	}
	paths := map[string]*fake{
		"auth 535":  {auth: func(string, string) error { return echo(535) }},
		"auth 421":  {auth: func(string, string) error { return echo(421) }},
		"mail 550":  {mail: func(string) error { return echo(550) }},
		"rcpt 550":  {rcpt: func(string) error { return echo(550) }},
		"rcpt 451":  {rcpt: func(string) error { return echo(451) }},
		"data 554":  {data: func(*smtp.Conn, []byte) error { return echo(554) }},
		"data drop": {data: func(c *smtp.Conn, _ []byte) error { _ = c.Conn().Close(); return nil }},
	}
	secrets := []string{testPass, testUser, testTo, "outreach", "northwind"}
	for name, f := range paths {
		t.Run(name, func(t *testing.T) {
			_, err := send(t, f.start(t, true))
			if err == nil {
				t.Fatal("no error")
			}
			for _, msg := range []string{err.Error(), strings.ToLower(err.Error())} {
				for _, v := range secrets {
					if strings.Contains(msg, strings.ToLower(v)) || strings.Contains(msg, v) {
						t.Errorf("error %q contains %q", err, v)
					}
				}
			}
		})
	}
	// The untrusted-certificate and deadline paths name no secret either.
	f := &fake{}
	s := f.start(t, false)
	s.RootCAs = x509.NewCertPool()
	_, err := send(t, s)
	for _, v := range secrets {
		if err == nil || strings.Contains(strings.ToLower(err.Error()), strings.ToLower(v)) {
			t.Errorf("certificate error %v contains %q", err, v)
		}
	}
}

func TestSMTPSenderPrintsNoSecrets(t *testing.T) {
	s := SMTPSender{Addr: fakeHost + ":465", User: config.Secret(testUser), Password: config.Secret(testPass)}
	for _, out := range []string{fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
		if strings.Contains(out, testPass) || strings.Contains(out, testUser) {
			t.Errorf("printed sender %q leaks a credential", out)
		}
	}
}

// SendError carries the reply's parsed enhanced status code, so callers read a field, not text.
func TestSMTPSendErrorEnhanced(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fake
		want [3]int
	}{
		{"rcpt 5.7.1", &fake{rcpt: func(string) error {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "policy"}
		}}, [3]int{5, 7, 1}},
		{"rcpt 5.1.1", &fake{rcpt: func(string) error {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
		}}, [3]int{5, 1, 1}},
		{"auth 5.7.8", &fake{auth: func(string, string) error {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "bad credentials"}
		}}, [3]int{5, 7, 8}},
		{"no enhanced code", &fake{rcpt: func(string) error {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.NoEnhancedCode, Message: "no"}
		}}, [3]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := send(t, tc.f.start(t, true))
			if se := kindOf(t, err); se.Enhanced != tc.want {
				t.Errorf("Enhanced = %v, want %v", se.Enhanced, tc.want)
			}
		})
	}
	// A deadline has no reply, so no code.
	release := make(chan struct{})
	f := &fake{mail: func(string) error { <-release; return nil }}
	s := f.start(t, true)
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.Send(ctx, testUser, testTo, testRaw)
	if se := kindOf(t, err); se.Enhanced != [3]int{} {
		t.Errorf("deadline: Enhanced = %v", se.Enhanced)
	}
}
