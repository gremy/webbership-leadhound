package enrich

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// personProfile is a person's LinkedIn profile link, built at runtime so no file holds one;
// the fixtures name it PERSONPROFILE.
var personProfile = "https://www.linkedin.com/" + "in" + "/jane-doe-northwind/"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: tests read their own fixtures by name
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.ReplaceAll(string(b), "PERSONPROFILE", personProfile))
}

func phone(v, n string) Fact  { return Fact{Kind: KindPhone, Value: v, Normalized: n} }
func social(v, n string) Fact { return Fact{Kind: KindSocial, Value: v, Normalized: n} }
func desc(v string) Fact      { return Fact{Kind: KindDescription, Value: v, Normalized: v} }

func TestExtractHome(t *testing.T) {
	body := readFixture(t, "home.html")
	if !strings.Contains(string(body), personProfile) {
		t.Fatal("the fixture lost its person's profile link")
	}
	got := Extract(body, "https://northwind.example/", "northwind.example", false)
	want := []Fact{
		phone("+44 20 7946 0123", "+442079460123"),
		phone("+44 (0)20 7946 0456", "+442079460456"),
		desc("Northwind Traders roasts coffee and bakes bread in the old mill."),
		social("https://www.facebook.com/NorthwindTraders", "https://facebook.com/northwindtraders"),
		social("https://twitter.com/northwind", "https://x.com/northwind"),
		social("https://www.linkedin.com/company/northwind-traders", "https://linkedin.com/company/northwind-traders"),
		social("https://www.instagram.com/northwind.traders", "https://instagram.com/northwind.traders"),
		social("https://www.youtube.com/@northwindtraders", "https://youtube.com/@northwindtraders"),
		social("https://www.tiktok.com/@northwind", "https://tiktok.com/@northwind"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract =\n%s\nwant\n%s", dump(got), dump(want))
	}
}

func TestExtractJSONLD(t *testing.T) {
	got := Extract(readFixture(t, "jsonld.html"), "https://www.contoso.example/", "contoso.example", false)
	want := []Fact{
		phone("+44 20 7946 0234", "+442079460234"),
		phone("+44 20 7946 0235", "+442079460235"),
		phone("0044 20 7946 0236", "+442079460236"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract =\n%s\nwant\n%s (a person's node, a broken script, a plain script and an unlabelled page line give nothing)", dump(got), dump(want))
	}
}

func TestExtractJSONLDTooLong(t *testing.T) {
	pad := strings.Repeat(" ", MaxScript)
	body := `<script type="application/ld+json">{"telephone": "+44 20 7946 0237"` + pad + `}</script>` +
		`<script type="application/ld+json">{"telephone": "+44 20 7946 0238"}</script>`
	got := Extract([]byte(body), "https://contoso.example/", "contoso.example", false)
	want := []Fact{phone("+44 20 7946 0238", "+442079460238")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract = %s, want only the short script's number", dump(got))
	}
}

func TestExtractMicrodata(t *testing.T) {
	got := Extract(readFixture(t, "microdata.html"), "https://fabrikam.example/", "fabrikam.example", false)
	want := []Fact{
		phone("+44 20 7946 0345", "+442079460345"),
		phone("+44 20 7946 0346", "+442079460346"),
		phone("+44 20 7946 0347", "+442079460347"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract =\n%s\nwant\n%s (a person's scope gives nothing)", dump(got), dump(want))
	}
}

func TestExtractLabelledLines(t *testing.T) {
	body := readFixture(t, "contact.html")
	got := Extract(body, "https://tailspin.example/contact", "tailspin.example", true)
	want := []Fact{
		phone("+44 20 7946 0123", "+442079460123"),
		phone("+44 20 7946 0124", "+442079460124"),
		phone("020 7946 0125", "02079460125"),
		phone("+44 20 7946 0126", "+442079460126"),
		phone("+44 (0)20 7946 0127", "+442079460127"),
		desc("Contact Tailspin Toys."),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("contact page =\n%s\nwant\n%s", dump(got), dump(want))
	}
	// The same lines on a page that is not a contact page give no phone.
	got = Extract(body, "https://tailspin.example/", "tailspin.example", false)
	if want := []Fact{desc("Contact Tailspin Toys.")}; !reflect.DeepEqual(got, want) {
		t.Errorf("not a contact page = %s, want the description alone", dump(got))
	}
}

func TestExtractGermanDescription(t *testing.T) {
	got := Extract(readFixture(t, "home_de.html"), "https://wingtip.example/", "wingtip.example", false)
	want := []Fact{
		desc("Wir modernisieren Ihre Website – schnell, sicher und zuverlässig. Größere Projekte übernehmen wir gern."),
		social("https://de-de.facebook.com/wingtip.gmbh", "https://facebook.com/wingtip.gmbh"),
		social("https://x.com/wingtip_gmbh", "https://x.com/wingtip_gmbh"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract =\n%s\nwant\n%s", dump(got), dump(want))
	}
}

func TestExtractOffDomainPage(t *testing.T) {
	if got := Extract(readFixture(t, "home.html"), "https://hosting.example/northwind/", "northwind.example", false); got != nil {
		t.Errorf("a page off the org's domain gave %s", dump(got))
	}
	if got := Extract(readFixture(t, "home.html"), "ftp://northwind.example/", "northwind.example", false); got != nil {
		t.Errorf("a page that is not http(s) gave %s", dump(got))
	}
}

func TestDescriptionCut(t *testing.T) {
	long := strings.Repeat("ä", MaxRunes+50)
	got := Extract([]byte(`<meta name="description" content="`+long+`">`), "https://northwind.example/", "northwind.example", false)
	if len(got) != 1 || utf8.RuneCountInString(got[0].Value) != MaxRunes {
		t.Fatalf("Extract = %s, want one description of %d runes", dump(got), MaxRunes)
	}
	// A description holding an address is dropped whole.
	addr := "info" + "@" + "northwind.example"
	got = Extract([]byte(`<meta name="description" content="Write to `+addr+`">`), "https://northwind.example/", "northwind.example", false)
	if got != nil {
		t.Errorf("a description with an address gave %s", dump(got))
	}
}

func TestSocialAtMostEight(t *testing.T) {
	var b strings.Builder
	for i := range 12 {
		b.WriteString(`<a href="https://www.facebook.com/page` + string(rune('a'+i)) + `">x</a>`)
	}
	got := Extract([]byte(b.String()), "https://northwind.example/", "northwind.example", false)
	if len(got) != MaxSocial {
		t.Errorf("%d social links, want %d", len(got), MaxSocial)
	}
}

func TestNormalizePhone(t *testing.T) {
	for raw, want := range map[string]string{
		"+44 20 7946 0000":      "+442079460000",
		"+44 (0) 20 7946 0001":  "+442079460001",
		"0044-20-7946-0002":     "+442079460002",
		"020/7946.0003":         "02079460003",
		" +44 20 7946 0004":     "+442079460004",
		"1234567":               "",
		"+1234567890123456":     "",
		"+44 20 7946 0005 ext":  "",
		"call +44 20 7946 0006": "",
		"":                      "",
	} {
		_, got, ok := NormalizePhone(raw)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

// personOrEmail matches what may never be returned: an email-shaped string or a person's
// LinkedIn path.
var personOrEmail = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9\-]+(?:\.[a-z0-9\-]+)*\.[a-z]{2,}|/in/|/people/|profile\.php`)

// TestInvariantNoPersonOrEmail runs every fixture through Extract, as a contact page and as
// a homepage, and checks no value is an address or a person's profile, every value fits, and
// no number from a person's node or scope (0990-0999) is returned.
func TestInvariantNoPersonOrEmail(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "*.html"))
	if err != nil || len(names) < 5 {
		t.Fatalf("fixtures %v: %v", names, err)
	}
	for _, n := range names {
		body := readFixture(t, filepath.Base(n))
		for _, contact := range []bool{false, true} {
			for _, domain := range []string{"northwind.example", "contoso.example", "fabrikam.example", "tailspin.example", "wingtip.example"} {
				for _, f := range Extract(body, "https://"+domain+"/", domain, contact) {
					for _, s := range []string{f.Value, f.Normalized} {
						if personOrEmail.MatchString(s) {
							t.Errorf("%s: %s %q is an address or a person", n, f.Kind, s)
						}
						if s == "" || utf8.RuneCountInString(s) > MaxRunes {
							t.Errorf("%s: %s value %q is empty or too long", n, f.Kind, s)
						}
						if f.Kind == KindPhone && strings.HasPrefix(f.Normalized, "+4420794609") {
							t.Errorf("%s: phone %q came from a person, a script, a fax line or an unlabelled line", n, s)
						}
						if strings.HasPrefix(strings.ToLower(s), "javascript:") || strings.HasPrefix(strings.ToLower(s), "data:") {
							t.Errorf("%s: %q", n, s)
						}
					}
					switch f.Kind {
					case KindPhone, KindDescription, KindSocial:
					default:
						t.Errorf("%s: kind %q", n, f.Kind)
					}
				}
			}
		}
	}
}

func dump(fs []Fact) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  " + f.Kind + " " + f.Value + " | " + f.Normalized + "\n")
	}
	return b.String()
}
