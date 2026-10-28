package probe

import (
	"os"
	"reflect"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

// What the probe stores of a cafe@1 reading reads back into the same result, so the build
// classifies exactly as the probe saw the page, the own vendor's customer included.
func TestCafeSignalsRoundTrip(t *testing.T) {
	cfg, err := cafe.DefaultConfig().WithOwnVendor("own", []string{"loyalty-own.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"none.html", "loyalty_vendor.html", "own_customer.html", "romanian.html", "ordering.html", "instagram_only.html", "inline_widget.html"} {
		body, err := os.ReadFile("../cafe/testdata/" + page) // #nosec G304 -- the repository's own test pages
		if err != nil {
			t.Fatal(err)
		}
		r := cafe.Detect(cfg, cafe.Page{URL: "https://cafe.example/", Body: body})
		back := CafeResult(CafeSignals(r, 7))
		if !reflect.DeepEqual(cafe.Classify(cafe.Input{OwnWebsite: "cafe.example", HomepageRead: true, Detection: r}),
			cafe.Classify(cafe.Input{OwnWebsite: "cafe.example", HomepageRead: true, Detection: back})) {
			t.Errorf("%s: decision changed through the store:\n%+v\n%+v", page, r, back)
		}
		if back.Language != r.Language || back.LanguageVia != r.LanguageVia || back.OwnCustomer != r.OwnCustomer {
			t.Errorf("%s: language or customer changed: %+v vs %+v", page, back, r)
		}
	}
}
