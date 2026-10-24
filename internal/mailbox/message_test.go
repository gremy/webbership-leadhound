package mailbox

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden from Build")

// The fixture's addresses are reserved names; the opt-out mailbox is built at run time.
func fixtureMessage() Message {
	box := "outreach" + "@" + "sender.example"
	return Message{
		FromName:        "Ana Example",
		FromAddr:        box,
		To:              "contact@northwind.example",
		Subject:         "Întrebare despre northwind.example",
		Body:            "Bună ziua,\n\nAm văzut că pagina folosește Drupal 7 = o versiune veche, ieșită din 2025, iar acest rând este destul de lung ca să treacă de limita de șaptezeci și șase de caractere.\nUn rând cu spațiu la sfârșit \n\n--\nAna Example\nRăspundeți cu „dezabonare” și nu vă mai scriu.\n",
		MessageID:       "0123456789abcdef0123456789abcdef@sender.example",
		ListUnsubscribe: box,
		Date:            time.Date(2026, 10, 27, 9, 30, 0, 0, time.FixedZone("EET", 2*3600)),
	}
}

func followUp() Message {
	m := fixtureMessage()
	m.Subject = "Re: a question about northwind.example"
	m.Body = "Hello again,\r\njust a short follow-up.\r\n"
	m.MessageID = "fedcba9876543210fedcba9876543210@sender.example"
	m.InReplyTo = "0123456789abcdef0123456789abcdef@sender.example"
	m.References = []string{m.InReplyTo}
	return m
}

func TestBuildGolden(t *testing.T) {
	for name, m := range map[string]Message{"first.golden": fixtureMessage(), "followup.golden": followUp()} {
		t.Run(name, func(t *testing.T) {
			got, err := Build(m)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", name)
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) // #nosec G304 -- the test's own testdata
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Build =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// Every line ends CRLF, none is longer than 78 octets, the headers are exactly the ones
// named, and the body decodes back to the text.
func TestBuildShape(t *testing.T) {
	m := fixtureMessage()
	raw, err := Build(m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) ||
		bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\r")) {
		t.Error("a line ends with a bare LF or CR")
	}
	if !bytes.HasSuffix(raw, []byte("\r\n")) {
		t.Error("the message does not end with CRLF")
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 78 && !strings.HasPrefix(line, "Subject:") {
			t.Errorf("line over 78 octets: %q", line)
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for k := range msg.Header {
		names = append(names, k)
	}
	wantHeaders := []string{"From", "To", "Subject", "Date", "Message-Id", "List-Unsubscribe", "Mime-Version", "Content-Type", "Content-Transfer-Encoding"}
	if len(names) != len(wantHeaders) {
		t.Errorf("headers = %v, want %v", names, wantHeaders)
	}
	for _, h := range wantHeaders {
		if msg.Header.Get(h) == "" {
			t.Errorf("header %s missing", h)
		}
	}
	dec := new(mime.WordDecoder)
	if subj, err := dec.DecodeHeader(msg.Header.Get("Subject")); err != nil || subj != m.Subject {
		t.Errorf("subject decodes to %q (%v)", subj, err)
	}
	if !strings.HasPrefix(msg.Header.Get("Subject"), "=?utf-8?q?") {
		t.Errorf("non-ASCII subject not Q-encoded: %q", msg.Header.Get("Subject"))
	}
	from, err := msg.Header.AddressList("From")
	if err != nil || from[0].Name != m.FromName || from[0].Address != m.FromAddr {
		t.Errorf("from = %v (%v)", from, err)
	}
	if got := msg.Header.Get("List-Unsubscribe"); got != "<mailto:"+m.ListUnsubscribe+"?subject=unsubscribe>" {
		t.Errorf("List-Unsubscribe = %q", got)
	}
	if got := msg.Header.Get("Date"); got != "Tue, 27 Oct 2026 09:30:00 +0200" {
		t.Errorf("Date = %q", got)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.ReplaceAll(m.Body, "\n", "\r\n"); string(body) != want {
		t.Errorf("body decodes to\n%q\nwant\n%q", body, want)
	}
	for _, bad := range []string{"<img", "<html", "http://", "https://", "text/html", "X-"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Errorf("the message holds %q", bad)
		}
	}
}

func TestBuildThreadsAFollowUp(t *testing.T) {
	raw, err := Build(followUp())
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	first := "<" + fixtureMessage().MessageID + ">"
	if msg.Header.Get("In-Reply-To") != first || msg.Header.Get("References") != first {
		t.Errorf("In-Reply-To %q References %q", msg.Header.Get("In-Reply-To"), msg.Header.Get("References"))
	}
	if msg.Header.Get("Subject") != followUp().Subject {
		t.Errorf("an ASCII subject is written as is, got %q", msg.Header.Get("Subject"))
	}
}

func TestBuildRefuses(t *testing.T) {
	cases := map[string]func(*Message){
		"header injection in subject": func(m *Message) { m.Subject = "hi\r\nBcc: x" + "@" + "y.example" },
		"to with a name":              func(m *Message) { m.To = "Someone <contact@northwind.example>" },
		"to not an address":           func(m *Message) { m.To = "northwind.example" },
		"no message id":               func(m *Message) { m.MessageID = "" },
		"bracketed message id":        func(m *Message) { m.MessageID = "<a@b.example>" },
		"in-reply-to without refs":    func(m *Message) { m.InReplyTo = "a@b.example" },
		"no date":                     func(m *Message) { m.Date = time.Time{} },
		"empty body":                  func(m *Message) { m.Body = " \n" },
		"empty subject":               func(m *Message) { m.Subject = "" },
		"no unsubscribe address":      func(m *Message) { m.ListUnsubscribe = "" },
		"reference with CRLF": func(m *Message) {
			m.InReplyTo = "a@b.example"
			m.References = []string{"a@b.example\r\nBcc: x" + "@" + "y.example"}
		},
		"message id with LF": func(m *Message) { m.MessageID = "a@b.example\nX: y" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := fixtureMessage()
			mutate(&m)
			_, err := Build(m)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if strings.Contains(err.Error(), "northwind") {
				t.Errorf("the error repeats an address: %v", err)
			}
		})
	}
}
