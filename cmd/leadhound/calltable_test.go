package main

import (
	"testing"

	"github.com/gremy/webbership-leadhound/internal/compliance"
)

// callFirstTable makes the shipped country table, with Romania back on call-first and Austria
// and Poland off as on 28 Oct 2026, the one every caller reads until the test ends. Since the operator's
// 29 Oct decision no shipped country takes those paths, so their tests run on this table.
func callFirstTable(t *testing.T) *compliance.Table {
	t.Helper()
	tab := compliance.Default()
	ro, _ := tab.Lookup("RO")
	ro.Group, ro.Email = "call_first", compliance.ChannelRule{Verdict: compliance.VerdictConsent}
	tab = tab.WithCountry("RO", ro)
	off := compliance.ChannelRule{Verdict: compliance.VerdictOff}
	for _, code := range []string{"AT", "PL"} {
		c, _ := tab.Lookup(code)
		c.Group, c.Email, c.Call = "off", off, off
		tab = tab.WithCountry(code, c)
	}
	t.Cleanup(compliance.SetDefault(tab))
	return tab
}
