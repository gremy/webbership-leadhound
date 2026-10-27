package campaign

import "slices"

// Detector names a spec may list. CP1 knows the names only; the registry of Go detectors that
// run behind them arrives in CP7.
const (
	DetectorDrupal = "drupal@1" // the Drupal version probe
	DetectorAgency = "agency@1" // the agency finding pattern
)

var knownDetectors = []string{DetectorAgency, DetectorDrupal}

// KnownDetector reports whether name is a registered detector.
func KnownDetector(name string) bool { return slices.Contains(knownDetectors, name) }

// Detectors returns the registered detector names, sorted.
func Detectors() []string { return slices.Clone(knownDetectors) }
