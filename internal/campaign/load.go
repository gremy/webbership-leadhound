package campaign

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// Dir is the campaigns folder inside the data directory; FileName is each campaign's spec.
const (
	Dir      = "campaigns"
	FileName = "campaign.yaml"
)

// Load reads one spec file strictly. It does not check the spec; LoadAll does.
func Load(path string) (Spec, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own data directory
	if err != nil {
		return Spec{}, fmt.Errorf("campaign: %w", err)
	}
	s, err := Decode(b)
	if err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// LoadAll reads and checks every <dataDir>/campaigns/*/campaign.yaml, sorted by slug. Each
// slug must match its folder's name. The legacy Drupal campaign, built from cfg, is included
// unless a file campaign has its slug; with no campaigns folder it is the only campaign.
// The checks run with the leadconfig kinds and with the draft gate closed.
func LoadAll(dataDir string, cfg *leadconfig.Config) ([]Spec, error) {
	return LoadAllWith(dataDir, cfg, LoadOptions{})
}

// LoadOptions are what LoadAllWith asks outside the files.
type LoadOptions struct {
	// DraftsTrusted reports whether the draft gate trusts Claude drafts for campaign slug; it
	// is asked only for a spec whose writer is claude. nil keeps the gate closed.
	DraftsTrusted func(slug string) bool
}

// LoadAllWith is LoadAll with the draft gate as o gives it.
func LoadAllWith(dataDir string, cfg *leadconfig.Config, o LoadOptions) ([]Spec, error) {
	root := filepath.Join(dataDir, Dir)
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return legacyOnly(cfg)
	case err != nil:
		return nil, fmt.Errorf("campaign: %w", err)
	case !info.IsDir():
		return nil, fmt.Errorf("campaign: %s is not a folder", root)
	}

	paths, err := filepath.Glob(filepath.Join(root, "*", FileName))
	if err != nil {
		return nil, fmt.Errorf("campaign: %w", err)
	}
	opts := CheckOptions{KnownKinds: KnownKinds()}
	var specs []Spec
	var errs []error
	for _, p := range paths {
		s, err := Load(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if folder := filepath.Base(filepath.Dir(p)); s.Slug != folder {
			errs = append(errs, fmt.Errorf("campaign: %s: %w: slug %q does not match its folder %q", p, ErrInvalid, s.Slug, folder))
			continue
		}
		so := opts
		if s.Writer == WriterClaude && o.DraftsTrusted != nil {
			so.DraftsTrusted = o.DraftsTrusted(s.Slug)
		}
		if err := s.Check(so); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		specs = append(specs, s)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if !slices.ContainsFunc(specs, func(s Spec) bool { return s.Slug == LegacySlug }) {
		legacy, err := legacyOnly(cfg)
		if err != nil {
			return nil, err
		}
		specs = append(specs, legacy...)
	}
	slices.SortFunc(specs, func(a, b Spec) int { return strings.Compare(a.Slug, b.Slug) })
	return specs, nil
}

func legacyOnly(cfg *leadconfig.Config) ([]Spec, error) {
	if cfg == nil {
		return nil, fmt.Errorf("campaign: %w: no leads config to build the %q campaign from", ErrInvalid, LegacySlug)
	}
	s := Legacy(cfg)
	if err := s.Check(CheckOptions{KnownKinds: KnownKinds()}); err != nil {
		return nil, fmt.Errorf("campaign: legacy from %s: %w", leadconfig.FileName, err)
	}
	return []Spec{s}, nil
}
