package leadview

import (
	"context"
	"errors"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func TestGuessSectorRules(t *testing.T) {
	cases := []struct{ domain, kind, want string }{
		// .edu, .ac.<cc>, .edu.<cc>, .sch.uk, .k12.*
		{"harbour-lane.edu", "", SectorEducation},
		{"www.harbour-lane.edu", "", SectorEducation},
		{"harbour-lane.ac.uk", "", SectorEducation},
		{"harbour-lane.ac.nz", "", SectorEducation},
		{"harbour-lane.edu.au", "", SectorEducation},
		{"harbour-lane.sch.uk", "", SectorEducation},
		{"harbour-lane.k12.ca.us", "", SectorEducation},
		{"harbour-lane.k12.tr", "", SectorEducation},
		// .gov, .gov.<cc>, .gouv.fr, .gob.*
		{"harbour-lane.gov", "", SectorGovernment},
		{"harbour-lane.gov.uk", "", SectorGovernment},
		{"harbour-lane.gouv.fr", "", SectorGovernment},
		{"harbour-lane.gob.mx", "", SectorGovernment},
		{"harbour-lane.gob.example", "", SectorGovernment},
		// .nhs.uk
		{"harbour-lane.nhs.uk", "", SectorHealthcare},
		// Case, spaces and a final dot do not matter.
		{" Harbour-Lane.AC.UK. ", "", SectorEducation},
		// The kinds.
		{"harbour-lane.example", leadstore.LeadAgencyPartner, SectorAgency},
		{"harbour-lane.example", cafe.KindNoDigitalLoyalty, SectorHospitality},
		// The domain goes first.
		{"harbour-lane.edu", leadstore.LeadAgencyPartner, SectorEducation},
		{"harbour-lane.gov.uk", cafe.KindHasLoyalty, SectorGovernment},
		// Nothing: another kind, or an ending with no name in front or the wrong end.
		{"harbour-lane.example", "", ""},
		{"harbour-lane.example", leadstore.LeadUnsupported, ""},
		{"harbour-lane.example", leadstore.LeadD10, ""},
		{"", "", ""},
		{"edu", "", ""},
		{"ac.uk", "", ""},
		{"edu.example", "", ""},
		{"gov.example", "", ""},
		{"k12.example", "", ""},
		{"k12.ca.us", "", ""},
		{"gob.example", "", ""},
		{"sch.uk", "", ""},
		{"nhs.uk", "", ""},
		{"gouv.fr", "", ""},
		{"harbour-lane.ac.example", "", ""},
		{"harbour-lane.edu.example", "", ""},
		{"harbour-lane.gov.example", "", ""},
		{"harbour-lane.sch.example", "", ""},
		{"harbour-lane.gouv.example", "", ""},
		{"harbour-lane.nhs.example", "", ""},
		{"edu.harbour-lane.example", "", ""},
		{"harbour-edu.example", "", ""},
		{"k12.harbour-lane.studio.example", "", ""},
	}
	for _, tc := range cases {
		if got := GuessSector(tc.domain, tc.kind); got != tc.want {
			t.Errorf("GuessSector(%q, %q) = %q, want %q", tc.domain, tc.kind, got, tc.want)
		}
		if tc.want != "" && SectorWords(tc.want) == "" {
			t.Errorf("GuessSector(%q, %q) = %q, which is not on the list", tc.domain, tc.kind, tc.want)
		}
	}
	for _, k := range cafe.Kinds {
		if got := GuessSector("harbour-lane.example", k.Kind); got != SectorHospitality {
			t.Errorf("café kind %s = %q, want hospitality", k.Kind, got)
		}
	}
}

func TestSectorsClosedList(t *testing.T) {
	want := []string{"education", "government", "healthcare", "nonprofit", "agency", "hospitality", "retail", "media", "other"}
	if len(Sectors) != len(want) {
		t.Fatalf("Sectors has %d, want %d", len(Sectors), len(want))
	}
	for i, s := range Sectors {
		if s.Name != want[i] || s.Words == "" || SectorWords(s.Name) != s.Words {
			t.Errorf("Sectors[%d] = %+v, want name %s with its words", i, s, want[i])
		}
	}
	if w := SectorWords("farming"); w != "" {
		t.Errorf("SectorWords of one not on the list = %q", w)
	}
}

// sectorT14Store answers OperatorSector with a fixed row.
type sectorT14Store struct {
	sector string
	err    error
}

func (s sectorT14Store) OperatorSector(context.Context, int64) (string, error) {
	return s.sector, s.err
}

func TestSectorOfOperatorBeatsRule(t *testing.T) {
	ctx := context.Background()
	got, err := SectorOf(ctx, sectorT14Store{sector: SectorRetail}, 1, "harbour-lane.edu", "")
	if err != nil || got != SectorRetail {
		t.Errorf("operator row: %q, %v; want retail", got, err)
	}
	got, err = SectorOf(ctx, sectorT14Store{}, 1, "harbour-lane.edu", "")
	if err != nil || got != SectorEducation {
		t.Errorf("no row: %q, %v; want the rule's education", got, err)
	}
	got, err = SectorOf(ctx, sectorT14Store{}, 1, "harbour-lane.example", "")
	if err != nil || got != "" {
		t.Errorf("no row, no rule: %q, %v; want none", got, err)
	}
	boom := errors.New("boom")
	got, err = SectorOf(ctx, sectorT14Store{err: boom}, 1, "harbour-lane.edu", "")
	if !errors.Is(err, boom) || got != "" {
		t.Errorf("a failed read: %q, %v; want the error and no guess", got, err)
	}
}
