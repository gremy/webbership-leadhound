package mailbox

import (
	"bytes"
	"encoding/base64"
	"mime/quotedprintable"
	"strings"
	"testing"
)

// Every address and Message-ID here is a reserved name, built at run time.
var (
	ourID      = "lh-1a2b" + "@" + "sender.example"
	ourOtherID = "lh-9z8y" + "@" + "sender.example"
	leadAddr   = "contact" + "@" + "northwind.example"
	colleague  = "ana.pop" + "@" + "mail.northwind.example"
	stranger   = "someone" + "@" + "fabrikam.example"
	daemon     = "MAILER-DAEMON" + "@" + "mx.sender.example"
)

func ours(id string) bool { return id == ourID || id == ourOtherID }

func contacted(domain string) bool { return domain == "northwind.example" }

// hdr builds a raw header block from name/value pairs, CRLF-ended, with the blank line.
func hdr(kv ...string) []byte {
	var b bytes.Buffer
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(kv[i] + ": " + kv[i+1] + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// dsn builds a delivery-status report as Fetched would hold it: the raw header, the first
// 600 bytes of the text (all MIME preamble and the human-readable part here) and the two
// report parts peeked by section.
func dsn(t *testing.T) (raw, firstText []byte, report [][]byte) {
	t.Helper()
	raw = hdr(
		"From", "Mail Delivery System <"+daemon+">",
		"To", "outreach"+"@"+"sender.example",
		"Subject", "Undelivered Mail Returned to Sender",
		"Return-Path", "<>",
		"Content-Type", `multipart/report; report-type=delivery-status; boundary="b1"`,
	)
	human := "--b1\r\nContent-Type: text/plain\r\n\r\n" + strings.Repeat("This is the mail system. ", 40) + "\r\n"
	status := "Reporting-MTA: dns; mx.sender.example\r\n\r\n" +
		"Final-Recipient: rfc822; " + leadAddr + "\r\n" +
		"Action: failed\r\nStatus: 5.1.1\r\n"
	headers := "From: Glitch <outreach" + "@" + "sender.example>\r\n" +
		"To: " + leadAddr + "\r\n" +
		"Subject: Drupal 7\r\n" +
		"Message-ID: <" + ourID + ">\r\n\r\n"
	body := human + "--b1\r\nContent-Type: message/delivery-status\r\n\r\n" + status +
		"--b1\r\nContent-Type: text/rfc822-headers\r\n\r\n" + headers + "--b1--\r\n"
	if strings.Index(body, "Message-ID") <= 600 {
		t.Fatal("the fixture's headers part must sit past byte 600")
	}
	return raw, []byte(body[:600]), [][]byte{[]byte(status), []byte(headers)}
}

func TestClassify(t *testing.T) {
	inReply := []string{"In-Reply-To", "<" + ourID + ">", "References", "<" + ourOtherID + "> <" + ourID + ">"}
	from := func(addr string, kv ...string) []byte {
		return hdr(append([]string{"From", "Ana <" + addr + ">", "Subject", "Re: Drupal 7"}, kv...)...)
	}
	cases := []struct {
		name      string
		raw       []byte
		text      string
		want      MessageKind
		wantID    string
		wantDom   string
		wantFinal string
	}{
		{"reply by In-Reply-To", from(stranger, inReply...), "Sounds good, call me.", KindReply, ourID, "fabrikam.example", ""},
		{"reply by References only", from(stranger, "References", "<other"+"@"+"x.example> <"+ourOtherID+">"),
			"Yes.", KindReply, ourOtherID, "fabrikam.example", ""},
		{"reply from a colleague at the same domain", from(colleague), "My colleague forwarded this.",
			KindReply, "", "northwind.example", ""},
		{"nu mai is a reply, not an opt-out", from(leadAddr, inReply...), "Mulțumim, nu mai avem pe nimeni pe Drupal.",
			KindReply, ourID, "northwind.example", ""},
		{"stop inside the text is a reply", from(leadAddr, inReply...), "We will stop using Drupal 7 next year.",
			KindReply, ourID, "northwind.example", ""},
		{"opt-out: unsubscribe in the subject", hdr("From", leadAddr, "Subject", "unsubscribe"), "",
			KindOptOut, "", "northwind.example", ""},
		{"opt-out: remove me in the text", from(leadAddr, inReply...), "Please remove   me from your list.",
			KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: dezabonare", from(leadAddr, inReply...), "Dezabonare, vă rog.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: dezabona", from(leadAddr, inReply...), "Vă rog să mă dezabonați.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: abmelden", from(leadAddr, inReply...), "Bitte abmelden.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: afmelden", from(leadAddr, inReply...), "Graag afmelden.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: Q-encoded subject", hdr("From", leadAddr, "Subject", "=?utf-8?q?Dezabonare_v=C4=83_rog?="), "",
			KindOptOut, "", "northwind.example", ""},
		{"opt-out: quoted-printable soft break", hdr("From", leadAddr, "Subject", "Re: Drupal 7",
			"Content-Transfer-Encoding", "quoted-printable"), "Please unsub=\r\nscribe me.", KindOptOut, "", "northwind.example", ""},
		{"opt-out: bare stop as the whole subject", hdr("From", leadAddr, "Subject", " STOP "), "",
			KindOptOut, "", "northwind.example", ""},
		{"stop in a longer subject is a reply", hdr("From", leadAddr, "Subject", "Re: stop the Drupal 7 work?"), "",
			KindReply, "", "northwind.example", ""},
		{"opt-out with no match cannot act", hdr("From", stranger, "Subject", "unsubscribe"), "",
			KindUnmatched, "", "fabrikam.example", ""},
		{"out-of-office", from(leadAddr, append(inReply, "Auto-Submitted", "auto-replied")...),
			"I am out of the office. Please remove me... no, unsubscribe.", KindAutoReply, ourID, "northwind.example", ""},
		{"Auto-Submitted: no is not automatic", from(leadAddr, append(inReply, "Auto-Submitted", "No")...),
			"Thanks.", KindReply, ourID, "northwind.example", ""},
		{"X-Autoreply", from(leadAddr, "X-Autoreply", "yes"), "Away.", KindAutoReply, "", "northwind.example", ""},
		{"X-Autorespond", from(leadAddr, "X-Autorespond", "1"), "Away.", KindAutoReply, "", "northwind.example", ""},
		{"Precedence: bulk", from(stranger, "Precedence", "bulk"), "News.", KindAutoReply, "", "fabrikam.example", ""},
		{"Precedence: auto_reply", from(leadAddr, "Precedence", "auto_reply"), "", KindAutoReply, "", "northwind.example", ""},
		{"Precedence: junk", from(stranger, "Precedence", "junk"), "", KindAutoReply, "", "fabrikam.example", ""},
		{"Precedence: list is not automatic", from(stranger, "Precedence", "list"), "", KindUnmatched, "", "fabrikam.example", ""},
		{"null return path from postmaster is a bounce", hdr("From", "postmaster"+"@"+"mx.fabrikam.example",
			"Return-Path", "<>", "Subject", "Delivery failure"), "", KindBounce, "", "fabrikam.example", ""},
		{"null return path from a person is not a bounce", hdr("From", stranger, "Return-Path", "<>",
			"Subject", "Hello"), "", KindUnmatched, "", "fabrikam.example", ""},
		{"unmatched", hdr("From", stranger, "Subject", "A question"), "Hello there.", KindUnmatched, "", "fabrikam.example", ""},
		{"no From at all", hdr("Subject", "x"), "", KindUnmatched, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, err := ParseHeader(c.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := Classify(h, []byte(c.text), ours, contacted)
			want := Result{Kind: c.want, MatchedMessageID: c.wantID, FromDomain: c.wantDom, FinalRecipient: c.wantFinal}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestClassifyBounceReadsTheReportParts(t *testing.T) {
	raw, first, report := dsn(t)
	h, err := ParseHeader(raw, report)
	if err != nil {
		t.Fatal(err)
	}
	got := Classify(h, first, ours, contacted)
	want := Result{Kind: KindBounce, MatchedMessageID: ourID, FromDomain: "sender.example", FinalRecipient: leadAddr}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Without the parts the first 600 bytes alone cannot match it.
	h, _ = ParseHeader(raw, nil)
	if got := Classify(h, first, ours, contacted); got.MatchedMessageID != "" || got.FinalRecipient != "" || got.Kind != KindBounce {
		t.Fatalf("without the report parts: %+v", got)
	}

	// A report that is not ours keeps its final recipient and carries no match.
	h, _ = ParseHeader(raw, report)
	if got := Classify(h, first, func(string) bool { return false }, contacted); got.MatchedMessageID != "" || got.FinalRecipient != leadAddr {
		t.Fatalf("a foreign report: %+v", got)
	}
}

func TestClassifyBounceByContentTypeOnly(t *testing.T) {
	// A report from an ordinary-looking sender still classifies by its Content-Type.
	raw := hdr("From", stranger, "Subject", "Report",
		"Content-Type", `multipart/report; boundary=x; report-type="delivery-status"`)
	h, err := ParseHeader(raw, [][]byte{[]byte("Final-Recipient: RFC822;<" + leadAddr + ">\r\n")})
	if err != nil {
		t.Fatal(err)
	}
	got := Classify(h, nil, ours, contacted)
	if got.Kind != KindBounce || got.FinalRecipient != leadAddr {
		t.Fatalf("got %+v", got)
	}
	// Another report type (a read receipt) is not a bounce.
	raw = hdr("From", stranger, "Content-Type", `multipart/report; report-type=disposition-notification; boundary=x`)
	h, _ = ParseHeader(raw, nil)
	if got := Classify(h, nil, ours, contacted); got.Kind == KindBounce {
		t.Fatalf("a disposition notification classified as a bounce: %+v", got)
	}
}

func TestClassifyReturnsNoAddressOrText(t *testing.T) {
	raw := hdr("From", "Ana <"+leadAddr+">", "Subject", "Re: Drupal 7", "In-Reply-To", "<"+ourID+">")
	h, _ := ParseHeader(raw, nil)
	got := Classify(h, []byte("secret body words"), ours, contacted)
	for _, f := range []string{got.MatchedMessageID, got.FromDomain, got.FinalRecipient, string(got.Kind)} {
		if strings.Contains(f, "contact") || strings.Contains(f, "secret") || strings.Contains(f, "@northwind") {
			t.Fatalf("result carries an address or body text: %+v", got)
		}
	}
}

func TestParseHeaderRejectsGarbage(t *testing.T) {
	if _, err := ParseHeader([]byte("not a header line\r\n\r\n"), nil); err == nil {
		t.Fatal("want an error for a malformed header")
	}
}

// encodeQP writes s as quoted-printable with short lines, so soft breaks fall inside the
// fields that must be read.
func encodeQP(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Re-wrap at 20 characters with soft breaks.
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(b.String(), "\r\n") {
		for len(line) > 22 && !strings.HasPrefix(line[18:], "=") && !strings.Contains(line[17:20], "=") {
			out.WriteString(line[:18] + "=\r\n")
			line = line[18:]
		}
		out.WriteString(line)
	}
	return out.Bytes()
}

func encodeB64(s string) []byte {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var out strings.Builder
	for len(enc) > 76 {
		out.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	out.WriteString(enc + "\r\n")
	return []byte(out.String())
}

func TestClassifyEncodedDSN(t *testing.T) {
	raw, first, report := dsn(t)
	for _, c := range []struct {
		name     string
		encoding string
		encode   func(string) []byte
	}{
		{"base64", "base64", encodeB64},
		{"quoted-printable", "Quoted-Printable", func(s string) []byte { return encodeQP(t, s) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var parts [][]byte
			for _, p := range report {
				enc := c.encode(string(p))
				if bytes.Contains(enc, []byte(ourID)) && bytes.Contains(enc, []byte("Final-Recipient: rfc822; "+leadAddr)) {
					t.Fatal("the encoding must hide the fields")
				}
				parts = append(parts, decodePart(enc, c.encoding))
			}
			h, err := ParseHeader(raw, parts)
			if err != nil {
				t.Fatal(err)
			}
			got := Classify(h, first, ours, contacted)
			want := Result{Kind: KindBounce, MatchedMessageID: ourID, FromDomain: "sender.example", FinalRecipient: leadAddr}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestDecodePart(t *testing.T) {
	big := strings.Repeat("Final-Recipient: rfc822; x"+"@"+"y.example\r\n", MaxReportPart/20)
	if got := decodePart(encodeB64(big), "base64"); len(got) != MaxReportPart || !strings.HasPrefix(big, string(got)) {
		t.Fatalf("base64: %d bytes, want the first %d", len(got), MaxReportPart)
	}
	// A base64 part cut mid-quantum decodes as far as it goes.
	enc := encodeB64("Message-ID: <" + ourID + ">\r\n")
	if got := decodePart(enc[:len(enc)-5], "base64"); !strings.HasPrefix("Message-ID: <"+ourID+">\r\n", string(got)) || len(got) == 0 {
		t.Fatalf("cut base64: %q", got)
	}
	if got := decodePart([]byte("plain =41 text"), "7bit"); string(got) != "plain =41 text" {
		t.Fatalf("7bit changed: %q", got)
	}
	if got := decodePart([]byte("!!!! ####"), "base64"); len(got) != 0 {
		t.Fatalf("garbage base64: %q", got)
	}
}
