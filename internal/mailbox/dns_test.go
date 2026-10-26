package mailbox

import (
	"context"
	"net"
	"strings"
	"testing"
)

// fakeResolver answers TXT lookups from a map; a name it does not hold is not found, and
// a name in fail fails with a lookup error that names the queried host, as net's does.
type fakeResolver struct {
	txt  map[string][]string
	fail map[string]bool
}

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.fail[name] {
		return nil, &net.DNSError{Err: "server misbehaving", Name: name, IsTimeout: true}
	}
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

const dnsDomain = "sender.example"

func goodRecords() map[string][]string {
	return map[string][]string{
		dnsDomain:                      {"google-site-verification=abc", "v=spf1 include:spf.mail.example ~all"},
		"sel1._domainkey." + dnsDomain: {"v=DKIM1; k=rsa; p=MIIBIjANBgkq"},
		"_dmarc." + dnsDomain:          {"v=DMARC1; p=none"},
	}
}

func TestCheckDNS(t *testing.T) {
	cases := []struct {
		name             string
		edit             func(m map[string][]string)
		fail             string
		spf, dkim, dmarc bool
		note             string // a word the note must hold; "" means the note is empty
	}{
		{"all pass", func(map[string][]string) {}, "", true, true, true, ""},
		{"spf missing", func(m map[string][]string) { m[dnsDomain] = []string{"google-site-verification=abc"} }, "",
			false, true, true, "spf"},
		{"spf domain not found", func(m map[string][]string) { delete(m, dnsDomain) }, "", false, true, true, "not found"},
		{"dkim missing", func(m map[string][]string) { delete(m, "sel1._domainkey."+dnsDomain) }, "",
			true, false, true, "dkim"},
		{"dmarc missing", func(m map[string][]string) { delete(m, "_dmarc."+dnsDomain) }, "", true, true, false, "dmarc"},
		{"wrong prefixes", func(m map[string][]string) {
			m[dnsDomain] = []string{"spf1 v=spf1"}
			m["sel1._domainkey."+dnsDomain] = []string{"k=rsa; v=DKIM1"}
			m["_dmarc."+dnsDomain] = []string{"v=DMARC2; p=none"}
		}, "", false, false, false, "v=spf1"},
		{"split dkim txt", func(m map[string][]string) {
			m["sel1._domainkey."+dnsDomain] = []string{"v=DKIM1; k=rsa; p=MIIBIj", "ANBgkqhkiG9w0BAQEFAAOC"}
		}, "", true, true, true, ""},
		{"split dkim txt whose first string is only the version", func(m map[string][]string) {
			m["sel1._domainkey."+dnsDomain] = []string{"v=DKI", "M1; k=rsa; p=MIIB"}
		}, "", true, true, true, ""},
		{"lookup failure", func(map[string][]string) {}, "_dmarc." + dnsDomain, true, true, false, "lookup failed"},
		{"case and spaces", func(m map[string][]string) {
			m[dnsDomain] = []string{"  V=SPF1 -all"}
			m["_dmarc."+dnsDomain] = []string{"v=dmarc1;p=reject"}
		}, "", true, true, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := goodRecords()
			c.edit(m)
			r := fakeResolver{txt: m, fail: map[string]bool{c.fail: c.fail != ""}}
			got := CheckDNS(context.Background(), r, dnsDomain, "sel1")
			if got.SPF != c.spf || got.DKIM != c.dkim || got.DMARC != c.dmarc {
				t.Fatalf("got %+v, want spf=%v dkim=%v dmarc=%v", got, c.spf, c.dkim, c.dmarc)
			}
			if c.note == "" && got.Note != "" {
				t.Fatalf("want no note, got %q", got.Note)
			}
			if c.note != "" && !strings.Contains(got.Note, c.note) {
				t.Fatalf("note %q does not say %q", got.Note, c.note)
			}
			if strings.Contains(got.Note, "sender") || strings.Contains(got.Note, ".example") {
				t.Fatalf("the note names the domain: %q", got.Note)
			}
		})
	}
}

func TestCheckDNSBadInput(t *testing.T) {
	r := fakeResolver{txt: goodRecords()}
	for _, c := range []struct{ domain, selector string }{{"", "sel1"}, {dnsDomain, ""}, {dnsDomain, "a.b"}} {
		got := CheckDNS(context.Background(), r, c.domain, c.selector)
		if got.SPF || got.DKIM || got.DMARC || got.Note == "" || strings.Contains(got.Note, "sender") {
			t.Fatalf("CheckDNS(%q, %q) = %+v", c.domain, c.selector, got)
		}
	}
}

func TestCheckDNSCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := CheckDNS(ctx, fakeResolver{txt: goodRecords()}, dnsDomain, "sel1")
	if got.SPF || got.DKIM || got.DMARC || !strings.Contains(got.Note, context.Canceled.Error()) {
		t.Fatalf("got %+v", got)
	}
}

// The standard resolver is what production passes.
var _ Resolver = net.DefaultResolver

func TestCheckDNSNoteNeverHoldsTheLookupError(t *testing.T) {
	r := fakeResolver{txt: goodRecords(), fail: map[string]bool{dnsDomain: true}}
	got := CheckDNS(context.Background(), r, dnsDomain, "sel1")
	if got.SPF || strings.Contains(got.Note, "misbehaving") || strings.Contains(got.Note, "no such host") {
		t.Fatalf("note carries the resolver's error: %q", got.Note)
	}
}

// TestCheckDNSOrgDMARC: a host below its registrable domain whose own _dmarc name holds no
// DMARC record passes on the organisational domain's record (RFC 7489 §6.6.3), and the note
// says so; SPF and DKIM are still read on the host only.
func TestCheckDNSOrgDMARC(t *testing.T) {
	const host = "mail." + dnsDomain
	records := func() map[string][]string {
		return map[string][]string{
			host:                      {"v=spf1 -all"},
			"sel1._domainkey." + host: {"v=DKIM1; p=MIIB"},
			"_dmarc." + dnsDomain:     {"v=DMARC1; p=none"},
		}
	}
	t.Run("org record passes", func(t *testing.T) {
		got := CheckDNS(context.Background(), fakeResolver{txt: records()}, host, "sel1")
		if !got.OK() {
			t.Fatalf("got %+v, want all pass", got)
		}
		if !strings.Contains(got.Note, "organisational domain") {
			t.Fatalf("note %q does not say the record is at the organisational domain", got.Note)
		}
		if strings.Contains(got.Note, "sender") || strings.Contains(got.Note, ".example") {
			t.Fatalf("the note names the domain: %q", got.Note)
		}
	})
	t.Run("host record wins", func(t *testing.T) {
		m := records()
		m["_dmarc."+host] = []string{"v=DMARC1; p=reject"}
		got := CheckDNS(context.Background(), fakeResolver{txt: m}, host, "sel1")
		if !got.OK() || got.Note != "" {
			t.Fatalf("got %+v, want all pass with no note", got)
		}
	})
	t.Run("neither", func(t *testing.T) {
		m := records()
		delete(m, "_dmarc."+dnsDomain)
		got := CheckDNS(context.Background(), fakeResolver{txt: m}, host, "sel1")
		if got.DMARC || !strings.Contains(got.Note, "dmarc") {
			t.Fatalf("got %+v, want dmarc fail", got)
		}
	})
	t.Run("spf and dkim stay on the host", func(t *testing.T) {
		m := records()
		delete(m, host)
		delete(m, "sel1._domainkey."+host)
		m[dnsDomain] = []string{"v=spf1 -all"}
		m["sel1._domainkey."+dnsDomain] = []string{"v=DKIM1; p=MIIB"}
		got := CheckDNS(context.Background(), fakeResolver{txt: m}, host, "sel1")
		if got.SPF || got.DKIM || !got.DMARC {
			t.Fatalf("got %+v, want spf and dkim fail, dmarc pass", got)
		}
	})
	t.Run("registrable host has no fallback", func(t *testing.T) {
		m := goodRecords()
		delete(m, "_dmarc."+dnsDomain)
		got := CheckDNS(context.Background(), fakeResolver{txt: m}, dnsDomain, "sel1")
		if got.DMARC {
			t.Fatalf("got %+v", got)
		}
	})
}
