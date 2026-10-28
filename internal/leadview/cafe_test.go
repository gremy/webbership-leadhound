package leadview

import (
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func TestFindingLineCafe(t *testing.T) {
	s := leadstore.FindingSubject{Kind: "cafe_no_digital_loyalty", Detector: "cafe@1", Reason: "no digital loyalty on the homepage", Language: "ro", LanguageVia: "text"}
	if got, want := FindingLine(s, leadconfig.Facts{}, time.Now()), "Café: no digital loyalty on the homepage · language ro, from the page's text"; got != want {
		t.Errorf("FindingLine = %q, want %q", got, want)
	}
	s = leadstore.FindingSubject{Kind: "cafe_no_website", Detector: "cafe@1", Reason: "no website of its own on the map listing"}
	if got, want := FindingLine(s, leadconfig.Facts{}, time.Now()), "Café: no website of its own on the map listing"; got != want {
		t.Errorf("FindingLine = %q, want %q", got, want)
	}
}
