package main

import "testing"

func TestCafeSignalWords(t *testing.T) {
	for _, c := range [][4]string{
		{"loyalty", "loyalty:stampme:script:https://stampme.com/widget.js", "loyalty", "stampme, via script: https://stampme.com/widget.js"},
		{"lang", "ro:html-lang:ro-RO", "language", "ro, via html-lang (ro-RO)"},
		{"x-generator", "10", "x-generator", "10"},
	} {
		if n, v := cafeSignalWords(c[0], c[1]); n != c[2] || v != c[3] {
			t.Errorf("%s %q = %q %q, want %q %q", c[0], c[1], n, v, c[2], c[3])
		}
	}
}
