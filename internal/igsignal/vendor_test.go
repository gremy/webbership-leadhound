package igsignal

import (
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

func TestVendor(t *testing.T) {
	email := "contact" + "@" + "cardora.ro" // built here so the line holds no address
	for _, c := range []struct {
		name    string
		p       Profile
		id, via string
	}{
		{"external link", Profile{ExternalURL: "https://stampino.ro/c/cafe"}, "stampino", VendorViaLink},
		{"link to the vendor's account", Profile{ExternalURL: "https://www.instagram.com/stampino.app/"}, "stampino", VendorViaLink},
		{"address in the bio", Profile{Bio: "Card: cardora.ro/c/example."}, "cardora", VendorViaBio},
		{"https address in the bio", Profile{Bio: "Puncte pe (https://pick.coupons/x)"}, "pick-coupons", VendorViaBio},
		{"tag in the bio", Profile{Bio: "Stamp card on @Stampino.app."}, "stampino", VendorViaTag},
		{"tag in a caption", Profile{Captions: []string{"nothing", "a 6-a cafea gratis cu @pick.coupons"}}, "pick-coupons", VendorViaTag},
		{"tag spelled as a host", Profile{Bio: "ask @cardora.ro"}, "cardora", VendorViaTag},
		{"link before bio before tag", Profile{ExternalURL: "https://pick.coupons/", Bio: "cardora.ro @stampino.app"}, "pick-coupons", VendorViaLink},
		{"bio address before tag", Profile{Bio: "@stampino.app and cardora.ro"}, "cardora", VendorViaBio},
		{"look-alike account", Profile{Bio: "@stampino.appfan"}, "", ""},
		{"an email address is no link and no tag", Profile{Bio: "write to " + email}, "", ""},
		{"ordering is not loyalty", Profile{ExternalURL: "https://glovoapp.com/ro/tm/cafe-exemplu/"}, "", ""},
		{"another account", Profile{ExternalURL: "https://www.instagram.com/cafe.exemplu/", Bio: "@cafe.exemplu"}, "", ""},
		{"nothing", Profile{Bio: "Specialty coffee ☕", Captions: []string{"latte art"}}, "", ""},
	} {
		id, via, ok := Vendor(c.p, nil)
		if id != c.id || via != c.via || ok != (c.id != "") {
			t.Errorf("%s: %q %q %v, want %q %q", c.name, id, via, ok, c.id, c.via)
		}
	}
}

// TestVendorOwnProductWins: the operator's own product, added with WithOwnVendor as the probe
// adds it, is seen, and outranks a rival seen first; the default list never knows it.
func TestVendorOwnProductWins(t *testing.T) {
	cfg, err := cafe.DefaultConfig().WithOwnVendor("own loyalty product", []string{"loyalty-own.example"})
	if err != nil {
		t.Fatal(err)
	}
	p := Profile{ExternalURL: "https://stampino.ro/", Bio: "Card: cafe.loyalty-own.example/join"}
	if id, via, ok := Vendor(p, cfg); !ok || id != cafe.OwnVendorID || via != VendorViaBio {
		t.Fatalf("own product: %q %q %v", id, via, ok)
	}
	if id, _, _ := Vendor(p, nil); id != "stampino" {
		t.Fatalf("the default list: %q", id)
	}
}
