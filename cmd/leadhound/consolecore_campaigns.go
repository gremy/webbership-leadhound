package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Campaigns is every campaign of the operator's account, each worded from its latest spec
// version. A campaign with no version yet shows the spec leads.yaml gives it when it is the
// legacy Drupal campaign, else its name alone. It never writes.
func (b *backend) Campaigns(ctx context.Context) ([]console.CampaignView, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return nil, err
	}
	cs, err := b.lq().Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	kinds, err := leadKinds(ctx, b, setup.leads)
	if err != nil {
		return nil, err
	}
	file := setup.file
	if file.Countries == nil {
		file = setup.leads
	}
	spent, _, err := claudeSpendToday(ctx, b.s.Q(), b.now().In(b.loc))
	if err != nil {
		return nil, err
	}
	spend := console.SpendOf(spent, b.cfg.ClaudeDailyCapMicros)
	out := make([]console.CampaignView, 0, len(cs))
	for _, c := range cs {
		source, err := latestCampaignSource(ctx, b, c.ID)
		if err != nil {
			return nil, err
		}
		v, err := campaignView(c, source, kinds, file)
		if err != nil {
			return nil, err
		}
		v.ClaudeSpendToday = spend
		out = append(out, v)
	}
	return out, nil
}

// campaignView words campaign c: its latest spec version decoded, or, with none, the legacy
// spec from cfg for the legacy campaign and the name alone for any other.
func campaignView(c leadstore.Campaign, source string, kinds []leadKind, cfg leadconfig.Config) (console.CampaignView, error) {
	v := console.CampaignView{Slug: c.Slug, Name: c.Name, Retired: c.RetiredAt != nil}
	var spec campaign.Spec
	switch {
	case len(c.Spec) > 0:
		if err := json.Unmarshal(c.Spec, &spec); err != nil {
			return console.CampaignView{}, fmt.Errorf("console: campaign %q: spec: %w", c.Slug, err)
		}
		v.SpecSHA, v.SpecSource = c.SpecSHA256, source
		if c.VersionAt != nil {
			v.SpecAt = *c.VersionAt
		}
	case c.Slug == campaign.LegacySlug:
		spec = campaign.Legacy(&cfg)
	default:
		return v, nil
	}
	for _, k := range spec.Kinds {
		v.Kinds = append(v.Kinds, kindLabel(kinds, k))
	}
	codes := slices.Sorted(maps.Keys(spec.Countries))
	for _, code := range codes {
		cc := spec.Countries[code]
		row := console.CampaignCountry{Code: code, Label: countryName(code), Email: cc.Email, Call: cc.Call}
		if cc.Lang != "" {
			row.Language = languageName(cc.Lang)
		}
		v.Countries = append(v.Countries, row)
	}
	for _, s := range spec.Sequence {
		v.Steps = append(v.Steps, console.CampaignStep{Step: s.Step, Type: s.Type, AfterBusinessDays: s.AfterBusinessDays, When: s.When})
	}
	v.Writer = spec.Writer
	return v, nil
}

// claudePurposeOrder is the order the dashboard names Claude purposes in; any other purpose
// follows in name order.
var claudePurposeOrder = []string{claude.PurposeDrafting, claude.PurposeResearch, claude.PurposeDraftsEval}

// claudeSpendToday is the operator's account's Claude spend on local's calendar day, in
// micro-dollars, in all and per purpose (purposes that spent only, claudePurposeOrder
// first). Jev rows never count.
func claudeSpendToday(ctx context.Context, q store.Queries, local time.Time) (int64, []console.PurposeSpend, error) {
	by, err := q.ClaudeSpendByPurpose(ctx, leadstore.DefaultAccountID, local)
	if err != nil {
		return 0, nil, err
	}
	var (
		total int64
		out   []console.PurposeSpend
	)
	for _, p := range claudePurposeOrder {
		if m, ok := by[p]; ok {
			out = append(out, console.PurposeSpend{Purpose: p, Micros: m})
			total += m
			delete(by, p)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(by)) {
		out = append(out, console.PurposeSpend{Purpose: p, Micros: by[p]})
		total += by[p]
	}
	return total, out, nil
}

// latestCampaignSource is where campaign id's latest spec version came from ("file" |
// "console"); "" with no version.
func latestCampaignSource(ctx context.Context, b *backend, id int64) (string, error) {
	var src string
	err := b.s.Pool().QueryRow(ctx,
		`SELECT source FROM campaign_version WHERE campaign_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&src)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("console: campaign source: %w", err)
	}
	return src, nil
}
