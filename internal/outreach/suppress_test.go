package outreach

import "testing"

func TestSuppressed(t *testing.T) {
	set := NewSuppressionSet(
		[]string{"Office@Contoso.example", "  hello@fabrikam.example "},
		[]string{"northwind.example", "WWW.Tailspin.EXAMPLE."},
	)
	tests := []struct {
		name, addr, domain string
		want               bool
	}{
		{"address exact", "office@contoso.example", "", true},
		{"address case-folded", "OFFICE@CONTOSO.EXAMPLE", "", true},
		{"address trimmed in the set", "hello@fabrikam.example", "", true},
		{"other address on the same domain", "info@contoso.example", "contoso.example", false},
		{"domain of the address", "info@northwind.example", "", true},
		{"subdomain of the address", "info@mail.northwind.example", "", true},
		{"domain argument", "", "northwind.example", true},
		{"domain argument subdomain", "", "Shop.Northwind.Example", true},
		{"domain argument covers a clean address", "info@contoso.example", "northwind.example", true},
		{"set domain stored as subdomain covers the registrable", "info@tailspin.example", "", true},
		{"lookalike domain", "info@notnorthwind.example", "", false},
		{"clean", "info@adatum.example", "adatum.example", false},
		{"empty", "", "", false},
		{"not an address", "northwind.example", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Suppressed(tc.addr, tc.domain, set); got != tc.want {
				t.Errorf("Suppressed(%q, %q) = %v, want %v", tc.addr, tc.domain, got, tc.want)
			}
		})
	}
}

func TestSuppressedEmptySet(t *testing.T) {
	var set SuppressionSet
	if Suppressed("office@northwind.example", "northwind.example", set) {
		t.Error("the zero set suppressed an address")
	}
}

func TestSuppressionSetAdd(t *testing.T) {
	set := NewSuppressionSet(nil, nil)
	set.AddAddress("Office@Northwind.example")
	set.AddDomain("contoso.example")
	if !Suppressed("office@northwind.example", "", set) || !Suppressed("x@a.contoso.example", "", set) {
		t.Error("added entries not matched")
	}
}
