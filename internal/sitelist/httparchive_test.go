package sitelist

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseHTTPArchive(t *testing.T) {
	in := strings.Join([]string{
		"client,page,rank",
		"mobile,https://www.northwind.example/,1000",
		"desktop,https://northwind.example/about,1000", // same registrable: deduped
		"mobile,http://shop.contoso.example:8080/x,5000",
		"mobile,not a url,1", // bad
		"mobile,mailto:x,1",  // bad: no host
		`mobile,"https://bücher.example/",1`,
	}, "\n")
	got, tally, err := ParseHTTPArchive(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{Domain: "contoso.example"}, {Domain: "northwind.example"}, {Domain: "xn--bcher-kva.example"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if want := (Tally{Bad: 2}); tally != want {
		t.Errorf("tally = %+v, want %+v", tally, want)
	}
}

func TestParseHTTPArchiveMissingColumn(t *testing.T) {
	if _, _, err := ParseHTTPArchive(strings.NewReader("client,url\nmobile,https://northwind.example/\n")); err == nil {
		t.Error("want an error without a page column")
	}
}

func TestParseHTTPArchiveEmpty(t *testing.T) {
	if _, _, err := ParseHTTPArchive(strings.NewReader("")); err == nil {
		t.Error("an empty file has no header: want an error")
	}
}
