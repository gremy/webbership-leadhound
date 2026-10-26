package mailbox

import (
	"strings"
	"testing"
)

// gmailAlternative builds a multipart/alternative body the way Gmail sends non-ASCII text:
// a base64 text/plain part, then a text/html part.
func gmailAlternative(plain, html string) string {
	return "--b1\r\nContent-Type: text/plain; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		string(encodeB64(plain)) +
		"\r\n--b1\r\nContent-Type: text/html; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		string(encodeB64(html)) + "\r\n--b1--\r\n"
}

func TestFirstTextPart(t *testing.T) {
	const alt = `multipart/alternative; boundary="b1"`
	cases := []struct {
		name, text, ctype, cte string
		want                   string
		wantHTML               bool
	}{
		{"Gmail base64 text/plain", gmailAlternative("Dezabonare, vă rog.\r\n", "<div>Dezabonare, vă rog.</div>"), alt, "",
			"Dezabonare, vă rog.\r\n", false},
		{"quoted-printable part with soft breaks", "--b1\r\nContent-Type: text/plain; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: quoted-printable\r\n\r\n" + "V=C4=83 rog s=C4=83 m=C4=83 dezabo=\r\nna=C8=9Bi de pe list=C4=83." + "\r\n--b1--\r\n",
			alt, "", "Vă rog să mă dezabonați de pe listă.", false},
		{"nested mixed > alternative > plain", "--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
			"--inner\r\nContent-Type: text/plain\r\n\r\nPlease remove me.\r\n--inner\r\nContent-Type: text/html\r\n\r\n<p>x</p>\r\n--inner--\r\n" +
			"--outer\r\nContent-Type: application/pdf\r\n\r\nJVBERi0x\r\n--outer--\r\n",
			"multipart/mixed; boundary=outer", "", "Please remove me.\r", false},
		{"HTML-only part", "--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>unsubscribe</p>\r\n--b1--\r\n", alt, "",
			"<p>unsubscribe</p>\r", true},
		{"preamble ignored", "This is a multi-part message in MIME format. unsubscribe\r\n\r\n--b1\r\nContent-Type: text/plain\r\n\r\nThanks.\r\n--b1--\r\n",
			alt, "", "Thanks.\r", false},
		{"transport padding after the boundary", "--b1   \r\nContent-Type: text/plain\r\n\r\nThanks.\r\n--b1--\r\n", alt, "", "Thanks.\r", false},
		{"LF line ends", "--b1\nContent-Type: text/plain\n\nThanks.\n--b1--\n", alt, "", "Thanks.", false},
		{"a part with no headers is text/plain", "--b1\r\n\r\nThanks.\r\n--b1--\r\n", alt, "", "Thanks.\r", false},
		{"attachment before any text part, cut inside it", "--b1\r\nContent-Type: application/pdf\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK", "multipart/mixed; boundary=b1", "", "", false},
		{"text attachment skipped", "--b1\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=a.txt\r\n\r\nunsubscribe\r\n" +
			"--b1\r\nContent-Type: text/plain\r\n\r\nThanks.\r\n--b1--\r\n", "multipart/mixed; boundary=b1", "", "Thanks.\r", false},
		{"header block never ends", "--b1\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Enc", alt, "", "", false},
		{"no boundary in the peek", "Content-Type: text/plain\r\n\r\nunsubscribe", alt, "",
			"Content-Type: text/plain\r\n\r\nunsubscribe", false},
		{"top-level base64 single part", string(encodeB64("Dezabonare, vă rog.")), "text/plain; charset=utf-8", "base64",
			"Dezabonare, vă rog.", false},
		{"top-level quoted-printable", "Please unsub=\r\nscribe me.", "text/plain", "quoted-printable", "Please unsubscribe me.", false},
		{"top-level html", "<p>Hi</p>", "text/html", "", "<p>Hi</p>", true},
		{"no Content-Type at all", "Thanks.", "", "", "Thanks.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, html := firstTextPart([]byte(c.text), c.ctype, c.cte)
			if string(got) != c.want || html != c.wantHTML {
				t.Fatalf("got %q html=%v, want %q html=%v", got, html, c.want, c.wantHTML)
			}
		})
	}
}

func TestFirstTextPartCutMidBase64(t *testing.T) {
	full := "Vă rog să mă dezabonați de pe lista dumneavoastră de e-mail, mulțumesc frumos.\r\n"
	body := gmailAlternative(full, "<p>x</p>")
	cutAt := strings.Index(body, "\r\n--b1\r\nContent-Type: text/html") - 7
	got, _ := firstTextPart([]byte(body[:cutAt]), `multipart/alternative; boundary=b1`, "")
	if len(got) == 0 || !strings.HasPrefix(full, string(got)) || len(got) == len(full) {
		t.Fatalf("want a strict decodable prefix, got %q", got)
	}
	if !strings.Contains(string(got), "dezabona") {
		t.Fatalf("the prefix should reach the word: %q", got)
	}
}

func FuzzFirstTextPart(f *testing.F) {
	f.Add([]byte(gmailAlternative("Dezabonare, vă rog.", "<p>x</p>")), `multipart/alternative; boundary="b1"`, "")
	f.Add([]byte("--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\n\r\nx\r\n--inner--\r\n--outer--\r\n"),
		"multipart/mixed; boundary=outer", "")
	f.Add([]byte("--b1\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nunsub=\r\nscribe =E2=80"), "multipart/alternative; boundary=b1", "")
	f.Add([]byte("--b1\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Enc"), "multipart/alternative; boundary=b1", "")
	f.Add([]byte("--b\n--b\n--b--\n"), "multipart/mixed; boundary=b", "")
	f.Add([]byte("UGxlYXNl"), "text/plain", "base64")
	f.Add([]byte(""), "", "")
	f.Fuzz(func(t *testing.T, text []byte, ctype, cte string) {
		body, _ := firstTextPart(text, ctype, cte)
		if len(body) > len(text)+MaxReportPart {
			t.Fatalf("body larger than its source: %d > %d", len(body), len(text))
		}
	})
}
