package mail

import "testing"

func TestCheck(t *testing.T) {
	for addr, ok := range map[string]bool{
		"user@mail.example":                     true,
		"a@b":                                   true,
		"User <user@mail.example>":              false,
		"nu":                                    false,
		"user@mail.example, other@mail.example": false,
		"":                                      false,
		"user@mail.example\r\nBcc: x@mail.example": false,
	} {
		if got := Check(addr) == nil; got != ok {
			t.Errorf("Check(%q) ok = %v, want %v", addr, got, ok)
		}
	}
}
