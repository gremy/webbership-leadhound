package campaign

import (
	"slices"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// Detector names a spec may list. The Go detectors behind them run in the probe
// (internal/probe picks one per site: cafe@1 for a site a place import added, drupal@1 for
// the rest); this registry says which source each runs on and which lead kinds it makes.
const (
	DetectorDrupal = "drupal@1"     // the Drupal version probe
	DetectorAgency = "agency@1"     // the agency finding pattern
	DetectorCafe   = cafe.DerivedBy // cafe@1: digital loyalty, ordering, Instagram and language on a café's homepage
)

type detectorInfo struct {
	source string   // the source kind it runs on
	kinds  []string // the lead kinds it makes
}

var detectors = map[string]detectorInfo{
	DetectorAgency: {SourceSiteList, []string{leadconfig.KindAgencyPartner}},
	DetectorDrupal: {SourceSiteList, []string{leadconfig.KindUnsupported, leadconfig.KindD10}},
	DetectorCafe:   {SourceOverture, cafeKinds()},
}

func cafeKinds() []string {
	out := make([]string, len(cafe.Kinds))
	for i, k := range cafe.Kinds {
		out[i] = k.Kind
	}
	return out
}

// KnownDetector reports whether name is a registered detector.
func KnownDetector(name string) bool { _, ok := detectors[name]; return ok }

// Detectors returns the registered detector names, sorted.
func Detectors() []string {
	out := make([]string, 0, len(detectors))
	for n := range detectors {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// DetectorSource is the source kind detector name runs on; "" when it is unknown.
func DetectorSource(name string) string { return detectors[name].source }

// DetectorKinds are the lead kinds detector name makes; nil when it is unknown.
func DetectorKinds(name string) []string { return slices.Clone(detectors[name].kinds) }

// KnownKinds are every lead kind a campaign may name: the leads.yaml kinds (which have
// templates there) and every registered detector's kinds, sorted and without repeats.
func KnownKinds() []string {
	out := slices.Clone(leadconfig.Kinds)
	for _, d := range detectors {
		out = append(out, d.kinds...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
