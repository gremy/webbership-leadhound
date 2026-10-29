package cafe

import (
	"slices"
	"testing"
)

// TestDetectAddedLoyaltyVendors: each competitor is found from a homepage linking its host,
// with the exact id the private campaign keys its links on.
func TestDetectAddedLoyaltyVendors(t *testing.T) {
	for _, tc := range []struct{ id, href string }{
		{"stampino", "https://stampino.ro/"},
		{"loyalzoo", "https://www.loyalzoo.com/"},
		{"loyalzoo", "https://start.mylty.co/?id=abc"},
		{"rewardly", "https://rewardly.me/"},
		{"rewardly", "https://rewardly.app/"},
		{"cardora", "https://cardora.ro/card/1"},
		{"tap2", "https://tap2.ai/"},
		{"tap2", "https://tap2.ch/"},
		{"tap2", "https://tap2.es/"},
		{"tap2", "https://tap2pay.ai/"},
		{"pick-coupons", "https://pick.coupons/"},
		{"pick-coupons", "https://www.instagram.com/pick.coupons/"},
	} {
		body := `<html><body><a href="` + tc.href + `">card</a></body></html>`
		r := Detect(nil, Page{URL: "https://cafe.example/", Body: []byte(body)})
		if !r.HasLoyalty || !slices.Equal(r.Vendors, []string{tc.id}) {
			t.Errorf("%s: loyalty %v vendors %v, want [%s]", tc.href, r.HasLoyalty, r.Vendors, tc.id)
		}
		if DefaultConfig().Vendor(tc.id) == nil {
			t.Errorf("%s: not in the default list", tc.id)
		}
	}
}

// TestDetectStampinoInstagram: a link to Stampino's Instagram is Stampino loyalty (every
// matching vendor fires on one link), and Instagram as well; another account is only Instagram.
func TestDetectStampinoInstagram(t *testing.T) {
	r := Detect(nil, Page{URL: "https://cafe.example/", Body: []byte(`<a href="https://www.instagram.com/stampino.app/">x</a>`)})
	if !r.HasLoyalty || !slices.Equal(r.Vendors, []string{"stampino"}) || !r.HasInstagram {
		t.Fatalf("stampino instagram: %+v", r)
	}
	r = Detect(nil, Page{URL: "https://cafe.example/", Body: []byte(`<a href="https://www.instagram.com/stampino.appfan/">x</a>`)})
	if r.HasLoyalty || !r.HasInstagram {
		t.Fatalf("look-alike account: %+v", r)
	}
}
