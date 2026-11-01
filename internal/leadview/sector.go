package leadview

import (
	"context"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The sectors of an org. The closed list is Sectors; the org_sector and proof checks in the
// store (migration 00029) hold the same names.
const (
	SectorEducation   = "education"
	SectorGovernment  = "government"
	SectorHealthcare  = "healthcare"
	SectorNonprofit   = "nonprofit"
	SectorAgency      = "agency"
	SectorHospitality = "hospitality"
	SectorRetail      = "retail"
	SectorMedia       = "media"
	SectorOther       = "other"
)

// Sector is one sector: its stored name and its plain words.
type Sector struct {
	Name  string
	Words string
}

// Sectors is the closed list of sectors, in display order.
var Sectors = []Sector{
	{SectorEducation, "Education"},
	{SectorGovernment, "Government"},
	{SectorHealthcare, "Healthcare"},
	{SectorNonprofit, "Nonprofit"},
	{SectorAgency, "Agency"},
	{SectorHospitality, "Hospitality"},
	{SectorRetail, "Retail"},
	{SectorMedia, "Media"},
	{SectorOther, "Other"},
}

// SectorWords is a sector's plain words, or "" for one not on the list.
func SectorWords(name string) string {
	i := slices.IndexFunc(Sectors, func(s Sector) bool { return s.Name == name })
	if i < 0 {
		return ""
	}
	return Sectors[i].Words
}

// GuessSector is the sector the rule gives a domain and lead kind, or "" when it gives none.
// The domain decides first: .edu, .ac.<cc>, .edu.<cc>, .sch.uk and .k12.* are education; .gov,
// .gov.<cc>, .gouv.fr and .gob.* are government; .nhs.uk is healthcare. Then the kind: an
// agency partner is agency, a café kind is hospitality. Each ending needs a name in front of
// it, so edu.example and k12.example are nothing.
func GuessSector(domain, kind string) string {
	if s := sectorOfDomain(domain); s != "" {
		return s
	}
	switch {
	case kind == leadstore.LeadAgencyPartner:
		return SectorAgency
	case slices.ContainsFunc(cafe.Kinds, func(k cafe.KindInfo) bool { return k.Kind == kind }):
		return SectorHospitality
	}
	return ""
}

// sectorOfDomain reads the domain half of GuessSector from the domain's last labels. A country
// code is any two-letter last label. .k12.* is k12 as the second- or third-last label, for
// .k12.tr and .k12.ca.us.
func sectorOfDomain(domain string) string {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), "."), ".")
	n := len(labels)
	if n < 2 {
		return ""
	}
	last, second := labels[n-1], labels[n-2]
	switch last {
	case "edu":
		return SectorEducation
	case "gov":
		return SectorGovernment
	}
	if n < 3 {
		return ""
	}
	cc := len(last) == 2
	switch {
	case (second == "ac" || second == "edu") && cc, second == "sch" && last == "uk", second == "k12",
		n >= 4 && labels[n-3] == "k12":
		return SectorEducation
	case second == "gov" && cc, second == "gouv" && last == "fr", second == "gob":
		return SectorGovernment
	case second == "nhs" && last == "uk":
		return SectorHealthcare
	}
	return ""
}

// OperatorSectors reads the operator's own sector of an org; leadstore.Queries is one.
type OperatorSectors interface {
	OperatorSector(ctx context.Context, orgID int64) (string, error)
}

// SectorOf is an org's sector: the operator's latest choice, else what GuessSector says of its
// domain and lead kind ("" when that is nothing). The rule is worked out on read, so no job
// stores it.
func SectorOf(ctx context.Context, q OperatorSectors, orgID int64, domain, kind string) (string, error) {
	s, err := q.OperatorSector(ctx, orgID)
	if err != nil || s != "" {
		return s, err
	}
	return GuessSector(domain, kind), nil
}
