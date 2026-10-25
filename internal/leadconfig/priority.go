package leadconfig

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// MaxPoints bounds every priority rule: points are integers from -MaxPoints to MaxPoints.
const MaxPoints = 100

// The Drupal majors a priority profile may weigh.
const (
	MinPriorityMajor = 6
	MaxPriorityMajor = 11
)

// PriorityChannels are the contact channels a priority profile may weigh: the channels a
// lead that can be offered takes (a channel none lead never is).
var PriorityChannels = []string{ChannelEmail, ChannelForm}

// Profile orders the leads a batch drafts from: a lead scores the sum of the points its
// kind, Drupal major, country, channel and site list rank band earn, plus ExactVersion when
// its exact version is known. A country in ExcludeCountries is never offered, on top of the
// country policy. leads.yaml's priority block holds the defaults; the operator's edits are
// versions in the database (leadstore), the latest of which wins.
type Profile struct {
	Kinds            map[string]int `yaml:"kinds" json:"kinds"`         // lead kind → points
	Majors           map[int]int    `yaml:"majors" json:"majors"`       // Drupal major (6-11) → points
	Countries        map[string]int `yaml:"countries" json:"countries"` // ISO code → points; unlisted 0
	Channels         map[string]int `yaml:"channels" json:"channels"`   // email or form → points
	RankBands        []RankBand     `yaml:"rank_bands" json:"rank_bands"`
	ExactVersion     int            `yaml:"exact_version" json:"exact_version"` // the exact version is known
	ExcludeCountries []string       `yaml:"exclude_countries" json:"exclude_countries"`
}

// RankBand gives Points to a site whose list rank is below Under. A site earns the band
// with the smallest Under it falls in, never more than one; an unranked site earns none.
type RankBand struct {
	Under  int `yaml:"under" json:"under"`
	Points int `yaml:"points" json:"points"`
}

// KindPoints maps a kinds_priority list into kind points, first listed highest (10 points
// a place), so ordering by score keeps the list's order and puts kinds not listed last.
func KindPoints(order []string) map[string]int {
	m := make(map[string]int, len(order))
	for i, k := range order {
		if _, ok := m[k]; !ok {
			m[k] = 10 * (len(order) - i)
		}
	}
	return m
}

// PriorityProfile is the file's priority profile: the priority block, with its kinds taken
// from kinds_priority (KindPoints) when the block has none; with no block, kind points from
// kinds_priority only, so an old file keeps its order.
func (cfg Config) PriorityProfile() Profile {
	var p Profile
	if cfg.Priority != nil {
		p = cfg.Priority.Clone()
	}
	if p.Kinds == nil {
		p.Kinds = KindPoints(cfg.KindsPriority)
	}
	return p
}

// Clone returns a deep copy of p.
func (p Profile) Clone() Profile {
	return Profile{
		Kinds: maps.Clone(p.Kinds), Majors: maps.Clone(p.Majors), Countries: maps.Clone(p.Countries),
		Channels: maps.Clone(p.Channels), RankBands: slices.Clone(p.RankBands),
		ExactVersion: p.ExactVersion, ExcludeCountries: slices.Clone(p.ExcludeCountries),
	}
}

// IsZero reports whether p has no rule at all.
func (p Profile) IsZero() bool {
	return len(p.Kinds) == 0 && len(p.Majors) == 0 && len(p.Countries) == 0 && len(p.Channels) == 0 &&
		len(p.RankBands) == 0 && p.ExactVersion == 0 && len(p.ExcludeCountries) == 0
}

// Rules counts p's rules, an exclusion included.
func (p Profile) Rules() int {
	n := len(p.Kinds) + len(p.Majors) + len(p.Countries) + len(p.Channels) + len(p.RankBands) + len(p.ExcludeCountries)
	if p.ExactVersion != 0 {
		n++
	}
	return n
}

// Validate reports every problem with p as one ErrInvalid error.
func (p Profile) Validate() error {
	if problems := p.Problems(); len(problems) > 0 {
		return fmt.Errorf("leadconfig: priority: %w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// Problems lists every problem with p, each naming its field under priority.
func (p Profile) Problems() []string {
	var out []string
	points := func(field string, v int) {
		if v < -MaxPoints || v > MaxPoints {
			out = append(out, fmt.Sprintf("%s: %d points is outside -%d to %d", field, v, MaxPoints, MaxPoints))
		}
	}
	for _, k := range sortedKeys(p.Kinds) {
		if !slices.Contains(Kinds, k) {
			out = append(out, fmt.Sprintf("priority.kinds: %q is not one of %s", k, strings.Join(Kinds, ", ")))
		}
		points("priority.kinds."+k, p.Kinds[k])
	}
	majors := slices.Sorted(maps.Keys(p.Majors))
	for _, m := range majors {
		if m < MinPriorityMajor || m > MaxPriorityMajor {
			out = append(out, fmt.Sprintf("priority.majors: %d is not a Drupal major from %d to %d", m, MinPriorityMajor, MaxPriorityMajor))
		}
		points("priority.majors."+strconv.Itoa(m), p.Majors[m])
	}
	for _, c := range sortedKeys(p.Countries) {
		if !countryCode.MatchString(c) {
			out = append(out, fmt.Sprintf("priority.countries: %q is not an ISO 3166-1 alpha-2 code (two capital letters, e.g. GB)", c))
		}
		points("priority.countries."+c, p.Countries[c])
	}
	for _, ch := range sortedKeys(p.Channels) {
		if !slices.Contains(PriorityChannels, ch) {
			out = append(out, fmt.Sprintf("priority.channels: %q is not one of %s", ch, strings.Join(PriorityChannels, ", ")))
		}
		points("priority.channels."+ch, p.Channels[ch])
	}
	for i, b := range p.RankBands {
		field := fmt.Sprintf("priority.rank_bands[%d]", i)
		switch {
		case b.Under < 1:
			out = append(out, field+".under must be at least 1")
		case i > 0 && b.Under <= p.RankBands[i-1].Under:
			out = append(out, field+".under must be above the band before it")
		}
		points(field+".points", b.Points)
	}
	points("priority.exact_version", p.ExactVersion)
	for i, c := range p.ExcludeCountries {
		if !countryCode.MatchString(c) {
			out = append(out, fmt.Sprintf("priority.exclude_countries: %q is not an ISO 3166-1 alpha-2 code", c))
		} else if slices.Index(p.ExcludeCountries, c) != i {
			out = append(out, fmt.Sprintf("priority.exclude_countries: %q appears twice", c))
		}
	}
	return out
}
