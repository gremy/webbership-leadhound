package main

import (
	"strings"
	"testing"
)

func TestMaskAddress(t *testing.T) {
	for in, want := range map[string]string{
		"jane@northwind.example": "j***@northwind.example",
		"x@contoso.example":      "x***@contoso.example",
		"@contoso.example":       "***",
		"no-at":                  "***",
	} {
		if got := maskAddress(in); got != want {
			t.Errorf("maskAddress(%q) = %q, want %q", in, got, want)
		}
	}
	if got := shown("domain", "contoso.example"); got != "contoso.example" {
		t.Errorf("a domain is shown whole, got %q", got)
	}
	if got := shown("address", "jane@northwind.example"); strings.Contains(got, "jane") {
		t.Errorf("an address is shown unmasked: %q", got)
	}
}

func TestSuppressUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"add"}, {"add", "person", "x"}, {"add", "domain"}, {"check"}, {"list", "x"}, {"lift", "x"}} {
		if err := runLeadsSuppress(t.Context(), args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: err = %v, want a usage error", args, err)
		}
	}
}
