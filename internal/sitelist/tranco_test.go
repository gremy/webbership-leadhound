package sitelist

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTranco(t *testing.T) {
	in := strings.Join([]string{
		"1,northwind.example",
		"2,www.contoso.example",
		"3,shop.northwind.example", // dup of rank 1: best rank kept
		"",                         // blank: skipped, not counted
		"not a line",               // bad: no comma
		"x,fabrikam.example",       // bad: rank not a number
		"0,fabrikam.example",       // bad: rank below 1
		"5,co" + ".uk",             // bad: a bare public suffix
		"6, tailspin.example ",
		"7,bücher.example\r",
		"8,woodgrove.invalid", // dropped by keep, not bad
	}, "\n")
	got, tally, err := ParseTranco(strings.NewReader(in), KeepTLDs([]string{"example"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Domain: "northwind.example", Rank: 1},
		{Domain: "contoso.example", Rank: 2},
		{Domain: "tailspin.example", Rank: 6},
		{Domain: "xn--bcher-kva.example", Rank: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if want := (Tally{Bad: 4, Filtered: 1}); tally != want {
		t.Errorf("tally = %+v, want %+v", tally, want)
	}
}

func TestParseTrancoBestRankWhenWorseComesFirst(t *testing.T) {
	got, _, err := ParseTranco(strings.NewReader("9,www.northwind.example\n4,northwind.example\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{{Domain: "northwind.example", Rank: 4}}; !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

func TestParseTrancoEmpty(t *testing.T) {
	got, tally, err := ParseTranco(strings.NewReader(""), nil)
	if err != nil || len(got) != 0 || tally != (Tally{}) {
		t.Errorf("empty reader: %v, %+v, %v", got, tally, err)
	}
}

func TestParseMajestic(t *testing.T) {
	// Columns reordered against the published file, plus an extra one: read by name.
	in := strings.Join([]string{
		"TLD,Domain,RefSubNets,GlobalRank,TldRank",
		"example,www.northwind.example,10,2,1",
		"example,contoso.example,9,1,2",
		"example,northwind.example,8,3,3",  // dup: rank 2 kept
		"example,fabrikam.example,7,abc,4", // bad rank
		"example,fabrikam.example",         // short row: bad
		"invalid,woodgrove.invalid,6,5,1",  // dropped by keep
	}, "\n")
	got, tally, err := ParseMajestic(strings.NewReader(in), KeepTLDs([]string{"example"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{Domain: "contoso.example", Rank: 1}, {Domain: "northwind.example", Rank: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if want := (Tally{Bad: 2, Filtered: 1}); tally != want {
		t.Errorf("tally = %+v, want %+v", tally, want)
	}
}

func TestParseMajesticMissingColumn(t *testing.T) {
	for _, header := range []string{"GlobalRank,TLD", "Domain,TLD", ""} {
		if _, _, err := ParseMajestic(strings.NewReader(header+"\n1,northwind.example\n"), nil); err == nil {
			t.Errorf("header %q: want an error", header)
		}
	}
}

func TestParseMajesticHeaderWithBOM(t *testing.T) {
	got, _, err := ParseMajestic(strings.NewReader("\uFEFFGlobalRank,Domain\n1,northwind.example\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{{Domain: "northwind.example", Rank: 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

func TestParseMajesticHeaderOnly(t *testing.T) {
	got, tally, err := ParseMajestic(strings.NewReader("GlobalRank,Domain\n"), nil)
	if err != nil || len(got) != 0 || tally != (Tally{}) {
		t.Errorf("header only: %v, %+v, %v", got, tally, err)
	}
}
