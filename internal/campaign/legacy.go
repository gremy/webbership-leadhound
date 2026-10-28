package campaign

import (
	"slices"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// The legacy campaign's slug and name: today's Drupal flow, campaign #1.
const (
	LegacySlug = "drupal-eol"
	LegacyName = "Drupal end-of-life"
)

// Legacy describes today's leads.yaml flow as a spec: the site lists with the Drupal and
// agency detectors, the leadconfig kinds, each country's channel (email on only for the email
// channel; calls off), the templates, writer template and the three-step all-email sequence
// spaced by schedule.followup_business_days. Max words are the upper bounds of first_words
// (step 0) and followup_words (steps 1 and 2). Links in step 0 are allowed (the finding's URL
// and the sender's site); a "!" is not (the subject-bang rule). Its templates_dir stays
// relative to the data directory, as in leads.yaml.
func Legacy(cfg *leadconfig.Config) Spec {
	countries := make(map[string]Country, len(cfg.Countries))
	for code, c := range cfg.Countries {
		countries[code] = Country{Email: c.Channel == leadconfig.ChannelEmail, Lang: c.Lang}
	}

	seq := LegacySequence(cfg.Schedule.FollowupBusinessDays)

	maxWords := map[int]int{}
	if len(cfg.Copy.FirstWords) == 2 {
		maxWords[0] = cfg.Copy.FirstWords[1]
	}
	if len(cfg.Copy.FollowupWords) == 2 {
		for i := 1; i < leadconfig.Steps; i++ {
			maxWords[i] = cfg.Copy.FollowupWords[1]
		}
	}

	return Spec{
		Slug:         LegacySlug,
		Name:         LegacyName,
		Product:      Product{Name: "Drupal upgrades", SiteURL: cfg.Sender.SiteURL},
		Source:       Source{Kind: SourceSiteList},
		Detectors:    []string{DetectorDrupal, DetectorAgency},
		Kinds:        slices.Clone(leadconfig.Kinds),
		Countries:    countries,
		Writer:       WriterTemplate,
		Copy:         Copy{BannedWords: slices.Clone(cfg.Copy.BannedWords), MaxWords: maxWords, LinksInStep0: true},
		Sequence:     seq,
		TemplatesDir: cfg.TemplatesDir,
	}
}
