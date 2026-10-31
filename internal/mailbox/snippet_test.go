package mailbox

import (
	"net/mail"
	"strings"
	"testing"
	"unicode/utf8"
)

// snipHdr is a top-level header with the given Content-Type and transfer encoding.
func snipHdr(ctype, cte string) Header {
	f := mail.Header{}
	if ctype != "" {
		f["Content-Type"] = []string{ctype}
	}
	if cte != "" {
		f["Content-Transfer-Encoding"] = []string{cte}
	}
	return Header{Fields: f}
}

func TestReplySnippet(t *testing.T) {
	plain := "text/plain; charset=UTF-8"
	ownLine := `Not relevant? Reply "unsubscribe" and I won't write again.`
	cases := []struct {
		name string
		h    Header
		text string
		own  []string
		want string
	}{
		{"plain reply", snipHdr(plain, ""), "Hi,\r\n\r\nYes, call me   Tuesday.\r\n", nil, "Hi, Yes, call me Tuesday."},
		{"quoted lines", snipHdr(plain, ""), "Sounds good.\r\n> Hello,\r\n  > our offer\r\nThanks", nil, "Sounds good. Thanks"},
		{"en attribution", snipHdr(plain, ""), "Sure, Thursday works.\r\n\r\nOn Mon, 26 Oct 2026 at 18:10, Sam Example wrote:\r\nHello, unquoted history\r\n", nil, "Sure, Thursday works."},
		{"wrapped attribution", snipHdr(plain, ""), "Sure.\r\nOn Mon, 26 Oct 2026 at 18:10, Sam Example <sam\r\nat sender.example> wrote:\r\nhistory", nil, "Sure."},
		{"ro attribution, diacritics kept", snipHdr(plain, ""), "Bună ziua, mulțumim! Să vorbim joi.\r\nÎn lun., 26 oct. 2026, 18:10 Sam Example a scris:\r\nistoric", nil, "Bună ziua, mulțumim! Să vorbim joi."},
		{"de attribution", snipHdr(plain, ""), "Gerne, Freitag passt.\r\nAm 26.10.2026 um 18:10 schrieb Sam Example:\r\nVerlauf", nil, "Gerne, Freitag passt."},
		{"fr attribution", snipHdr(plain, ""), "Avec plaisir.\r\nLe lun. 26 oct. 2026 à 18:10, Sam Example a écrit :\r\nhistorique", nil, "Avec plaisir."},
		{"outlook original message", snipHdr(plain, ""), "Send me a price.\r\n-----Original Message-----\r\nFrom: Sam\r\nhistory", nil, "Send me a price."},
		{"outlook from/sent block", snipHdr(plain, ""), "Send me a price.\r\n\r\nFrom: Sam Example\r\nSent: Monday, 26 October 2026 18:10\r\nTo: Info\r\n\r\nHello", nil, "Send me a price."},
		{"signature", snipHdr(plain, ""), "Interested, tell me more.\r\n-- \r\nAna Example\r\nAcme Cafe", nil, "Interested, tell me more."},
		{"signature without its space", snipHdr(plain, ""), "Interested.\r\n--\r\nAna Example", nil, "Interested."},
		{"iphone footer", snipHdr(plain, ""), "OK, Tuesday.\r\n\r\nSent from my iPhone\r\n", nil, "OK, Tuesday."},
		{"ro mobile footer", snipHdr(plain, ""), "Da, marți.\r\nTrimis de pe iPhone-ul meu", nil, "Da, marți."},
		{"outlook mobile footer", snipHdr(plain, ""), "Yes.\r\nGet Outlook for Android", nil, "Yes."},
		{"html with gmail quote", snipHdr("text/html; charset=UTF-8", ""),
			`<div dir="ltr">Thanks,&nbsp;<b>call</b> me.<br>Ana</div><div class="gmail_quote"><blockquote>old</blockquote></div>`, nil, "Thanks, call me. Ana"},
		{"html gmail signature", snipHdr("text/html; charset=UTF-8", ""),
			`<div>Yes please.</div><div><br></div>-- <br><div dir="ltr" class="gmail_signature">Ana Example</div>`, nil, "Yes please."},
		{"undeclared html", snipHdr("", ""), "<div>Hi there</div><div>more</div>", nil, "Hi there more"},
		{"quoted-printable", snipHdr(plain, "quoted-printable"), "Mul=C8=9Bumesc, s=C4=83 vorbim m=C3=A2ine.=\r\n Gata.", nil, "Mulțumesc, să vorbim mâine. Gata."},
		{"base64", snipHdr(plain, "base64"), "QnVuxIMsIGRhLg==", nil, "Bună, da."},
		{"multipart first part", snipHdr("multipart/alternative; boundary=b1", ""),
			"--b1\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nDa, =C3=AEmi place.\r\n--b1\r\nContent-Type: text/html\r\n\r\n<p>x</p>\r\n--b1--\r\n", nil, "Da, îmi place."},
		{"windows-1250", snipHdr("text/plain; charset=windows-1250", "8bit"), "Mul\x9dumesc, s\xe3 vorbim.", nil, "Mulťumesc, să vorbim."},
		{"stray invalid bytes dropped", snipHdr(plain, ""), "ok \xff\xfe done", nil, "ok done"},
		{"own line taken out", snipHdr(plain, ""), "Yes.\r\n" + ownLine, []string{ownLine}, "Yes."},
		{"only quoted history", snipHdr(plain, ""), "> Hello\r\n> there", nil, ""},
		{"empty", snipHdr("", ""), "", nil, ""},
		{"nil header", Header{}, "Plain words", nil, "Plain words"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReplySnippet(c.h, []byte(c.text), c.own); got != c.want {
				t.Errorf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestReplySnippetCutAttribution: a full peek whose last line is an attribution cut before
// its "wrote:" drops that line; a short message keeps a like line (it was not cut).
func TestReplySnippetCutAttribution(t *testing.T) {
	head := "Yes, Tuesday.\r\n"
	pad := strings.Repeat("Words here. ", (FirstTextBytes-len(head))/12+1)
	cut := head + pad + "\r\nOn Mon, 26 Oct 2026 at 18:10, Sam Ex"
	got := ReplySnippet(snipHdr("text/plain; charset=UTF-8", ""), []byte(cut), nil)
	if strings.Contains(got, "On Mon") {
		t.Errorf("cut attribution kept: %q", got)
	}
	short := "In 2027 we open a second shop."
	if got := ReplySnippet(snipHdr("", ""), []byte(short), nil); got != short {
		t.Errorf("short text: %q", got)
	}
}

// TestReplySnippetClip: a long text is cut on a word boundary with "…", at most 300 runes,
// and a multi-byte rune is never split.
func TestReplySnippetClip(t *testing.T) {
	long := strings.Repeat("ăîșț word ", 60)
	got := ReplySnippet(snipHdr("", ""), []byte(long), nil)
	if n := utf8.RuneCountInString(got); n > MaxSnippetRunes || n < MaxSnippetRunes*2/3 {
		t.Fatalf("length %d", n)
	}
	if !strings.HasSuffix(got, "word…") || !utf8.ValidString(got) {
		t.Errorf("not cut on a word: %q", got[len(got)-20:])
	}
	// No space at all: a hard cut on a rune boundary.
	got = ReplySnippet(snipHdr("", ""), []byte(strings.Repeat("ș", 400)), nil)
	if n := utf8.RuneCountInString(got); n != MaxSnippetRunes || !strings.HasSuffix(got, "ș…") {
		t.Errorf("hard cut: %d runes", n)
	}
	exact := strings.Repeat("a", MaxSnippetRunes)
	if got := ReplySnippet(snipHdr("", ""), []byte(exact), nil); got != exact {
		t.Error("a text of exactly the limit was cut")
	}
}

func FuzzReplySnippet(f *testing.F) {
	f.Add([]byte("Hi,\r\n> quoted\r\nOn Mon, 26 Oct 2026 Sam wrote:\r\n"), "text/plain; charset=UTF-8", "")
	f.Add([]byte("<div>Hi<div class=\"gmail_quote\">x"), "text/html", "quoted-printable")
	f.Add([]byte("--b1\r\nContent-Type: text/plain\r\n\r\nDa\r\n--b1--"), "multipart/mixed; boundary=b1", "")
	f.Add([]byte("Mul\x9dumesc \xff"), "text/plain; charset=windows-1250", "base64")
	f.Fuzz(func(t *testing.T, text []byte, ctype, cte string) {
		got := ReplySnippet(snipHdr(ctype, cte), text, []string{"Reply stop to stop."})
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8: %q", got)
		}
		if n := utf8.RuneCountInString(got); n > MaxSnippetRunes {
			t.Fatalf("%d runes", n)
		}
		if got != strings.TrimSpace(got) {
			t.Fatalf("not trimmed: %q", got)
		}
	})
}
