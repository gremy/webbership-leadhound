package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/probe"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["places"] = runLeadsPlaces }

// The run row of a place import.
const (
	commandLeadsPlacesImport = "leads-places-import"
	sourcePlaces             = "places"
)

const placesUsage = "usage: leadhound leads places import overture --campaign SLUG --release RELEASE FILE"

// runLeadsPlaces is `leads places import overture …`.
func runLeadsPlaces(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "import" || args[1] != places.Source {
		return usageError(placesUsage)
	}
	return runPlacesImport(ctx, args[2:])
}

// runPlacesImport is `leads places import overture --campaign C --release R FILE`: it reads
// the operator's NDJSON export of Overture places with the campaign's source filters
// (categories, countries, min_confidence), counts every kept place's name and website domain
// with the places already stored for the release (another file of it) into one chain index
// (cafe.NewChainIndex with the spec's chains), checks each kept place against it, and stores
// the import in one transaction: an independent café gets its site (probed by cafe@1) and
// its org with its region, a chain only its place row, and a place whose website's domain is
// suppressed nothing. It prints the counts; malformed lines are named by number only. The
// campaign's spec must be its own file, registered as it is now, with source kind overture.
func runPlacesImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads places import overture", flag.ContinueOnError)
	slug := fs.String("campaign", "", "the campaign's short name (its spec's source is overture)")
	release := fs.String("release", "", "the Overture release the file was exported from, e.g. 2026-10-21.0")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *slug == "" || *release == "" {
		return usageError(placesUsage)
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	c, err := activeCampaign(ctx, lq, *slug)
	if err != nil {
		return err
	}
	spec, err := registeredSpec(setup.dataDir, c)
	if err != nil {
		return err
	}
	if spec.Source.Kind != campaign.SourceOverture {
		return usageError("campaign %s reads source %q; leads places import is for an %q source", c.Slug, spec.Source.Kind, campaign.SourceOverture)
	}
	f, err := os.Open(pos[0]) // #nosec G304 -- the operator names the export file
	if err != nil {
		return usageError("cannot read %s: %v", pos[0], err)
	}
	defer func() { _ = f.Close() }()
	kept, st, err := places.Read(f, placeOptions(spec))
	if err != nil {
		return err
	}

	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsPlacesImport, sourcePlaces)
	if err != nil {
		return err
	}
	var res leadstore.PlaceImported
	runErr := leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		stored, err := tq.ReleasePlaces(ctx, *release)
		if err != nil {
			return err
		}
		rows := chainRows(spec, stored, kept)
		res, err = tq.ImportPlaces(ctx, leadstore.PlaceImport{
			CampaignID: c.ID, Release: *release, FileName: filepath.Base(pos[0]), Stats: st, Actor: leadstore.ActorGremy,
		}, rows)
		return err
	})
	switch {
	case errors.Is(runErr, leadstore.ErrBadRelease):
		runErr = usageError("--release %q: %v", *release, runErr)
	case errors.Is(runErr, leadstore.ErrBadPlaceFile):
		runErr = usageError("%s: %v", pos[0], runErr)
	}
	result := store.RunResult{Note: fmt.Sprintf("%d lines, %d kept, %d stored, %d chains, %d suppressed", st.Lines, st.Kept, res.Stored, res.Chains, res.Suppressed)}
	if runErr != nil {
		result.Note = runErr.Error()
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil && runErr == nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	printPlacesImport(stdout, c.Slug, *release, st, res)
	return nil
}

// registeredSpec is campaign c's spec from its own file in dataDir, checked, and still what
// was registered (as a batch reads it, resolveBatchPlan).
func registeredSpec(dataDir string, c leadstore.Campaign) (campaign.Spec, error) {
	path, ok, err := campaignFile(dataDir, c.Slug)
	if err != nil {
		return campaign.Spec{}, err
	}
	if !ok {
		return campaign.Spec{}, usageError("Campaign %s has no spec file in the data directory (%s).", c.Slug, path)
	}
	spec, err := campaign.Load(path)
	if err != nil {
		return campaign.Spec{}, usageError("Campaign %s: %s", c.Slug, err.Error())
	}
	if err := spec.Check(campaign.CheckOptions{KnownKinds: campaign.KnownKinds(), DraftsTrusted: true}); err != nil {
		return campaign.Spec{}, usageError("Campaign %s does not pass its checks: %s", c.Slug, strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	_, sha, err := campaign.Canonical(spec)
	if err != nil {
		return campaign.Spec{}, err
	}
	if c.SpecSHA256 == "" || sha != c.SpecSHA256 {
		return campaign.Spec{}, usageError("Campaign %s changed since it was registered (or never was): run leadhound leads campaigns register first.", c.Slug)
	}
	return spec, nil
}

// placeOptions are the spec's source filters as places.Read takes them.
func placeOptions(spec campaign.Spec) places.Options {
	return places.Options{Categories: spec.Source.Categories, Countries: spec.Source.Countries, MinConfidence: spec.Source.MinConfidence}
}

// chainConfig is the spec's chains as the chain check takes them (the defaults when none).
func chainConfig(spec campaign.Spec) cafe.ChainConfig {
	if spec.Chains == nil {
		return cafe.ChainConfig{}
	}
	return cafe.ChainConfig{MaxLocations: spec.Chains.MaxLocations, Names: spec.Chains.Names, KeepBranded: spec.Chains.KeepBranded}
}

// releaseChains is one chain index over a release's places, each counted once by source id:
// stored, the places already stored for the release (ReleasePlaces), then the kept places of
// a file not among them. entries maps each counted place's source id to its entry.
func releaseChains(spec campaign.Spec, stored, kept []places.Place) (*cafe.ChainIndex, map[string]cafe.ChainEntry) {
	entries := map[string]cafe.ChainEntry{}
	var all []cafe.ChainEntry
	for _, set := range [][]places.Place{stored, kept} {
		for _, p := range set {
			if _, ok := entries[p.SourceID]; ok {
				continue
			}
			e := cafe.EntryOf(p)
			entries[p.SourceID] = e
			all = append(all, e)
		}
	}
	return cafe.NewChainIndex(chainConfig(spec), all), entries
}

// chainRows checks every kept place against one chain index over the release: the places
// already stored for it (stored, from earlier files) and the kept ones, so a chain whose
// places are split across export files is told apart as one.
func chainRows(spec campaign.Spec, stored, kept []places.Place) []leadstore.PlaceRow {
	ix, _ := releaseChains(spec, stored, kept)
	rows := make([]leadstore.PlaceRow, len(kept))
	for i, p := range kept {
		v := ix.Check(cafe.EntryOf(p))
		rows[i] = leadstore.PlaceRow{Place: p, ChainRule: v.Rule, ChainDetail: v.Detail}
	}
	return rows
}

// buildChains is the café build's chain check: an org whose latest place row is a chain, or
// whose latest place the chain index over that place's whole release now finds a chain (a
// later file of the release added more of its places), gets no lead. Indexes are read once
// per release.
type buildChains struct {
	lq      leadstore.Queries
	spec    campaign.Spec
	byRel   map[string]*cafe.ChainIndex
	entries map[string]map[string]cafe.ChainEntry
}

func newBuildChains(lq leadstore.Queries, spec campaign.Spec) *buildChains {
	return &buildChains{lq: lq, spec: spec, byRel: map[string]*cafe.ChainIndex{}, entries: map[string]map[string]cafe.ChainEntry{}}
}

// rule is o's chain rule ("" for an independent café).
func (b *buildChains) rule(ctx context.Context, o leadstore.CafeBuildOrg) (string, error) {
	if o.PlaceChainRule != "" {
		return o.PlaceChainRule, nil
	}
	ix, ok := b.byRel[o.PlaceRelease]
	if !ok {
		stored, err := b.lq.ReleasePlaces(ctx, o.PlaceRelease)
		if err != nil {
			return "", err
		}
		ix, b.entries[o.PlaceRelease] = releaseChains(b.spec, stored, nil)
		b.byRel[o.PlaceRelease] = ix
	}
	e, ok := b.entries[o.PlaceRelease][o.PlaceSourceID]
	if !ok {
		return "", nil
	}
	return ix.Check(e).Rule, nil
}

func printPlacesImport(w io.Writer, slug, release string, st places.Stats, res leadstore.PlaceImported) {
	_, _ = fmt.Fprintf(w, "leads places import: campaign %s, release %s, import %d\n", slug, release, res.ImportID)
	_, _ = fmt.Fprintf(w, "  read %s: %d kept · skipped %d (blank %d · malformed %d · not a café %d · low confidence %d · other country %d · no name %d)\n",
		plural(st.Lines, "line", "lines"), st.Kept, st.Skipped(), st.Blank, st.Malformed, st.NotCafe, st.LowConfidence, st.OtherCountry, st.NoName)
	if len(st.MalformedLines) > 0 {
		nums := make([]string, len(st.MalformedLines))
		for i, n := range st.MalformedLines {
			nums[i] = fmt.Sprint(n)
		}
		more := ""
		if st.Malformed > len(st.MalformedLines) {
			more = " …"
		}
		_, _ = fmt.Fprintf(w, "  malformed lines: %s%s\n", strings.Join(nums, ", "), more)
	}
	_, _ = fmt.Fprintf(w, "  stored %d places (%d already in this release) · %d chains skipped (no lead) · %d orgs and %d sites made\n",
		res.Stored, res.Already, res.Chains, res.Orgs, res.Sites)
	if res.Suppressed > 0 {
		_, _ = fmt.Fprintf(w, "  not stored: %s whose website's domain is suppressed (forgotten or opted out)\n", plural(res.Suppressed, "place", "places"))
	}
	if res.Sites > 0 {
		_, _ = fmt.Fprintln(w, "  next: leadhound leads sites probe reads each new café's homepage (cafe@1), then leads contacts, then leads build --campaign "+slug)
	}
}

// cafeConfig is the cafe@1 signal list with the operator's own vendor added from leads.yaml
// (probe.cafe_own_vendor_hosts, private); the default list when none are set.
func cafeConfig(cfg leadconfig.Config) (*cafe.Config, error) {
	hosts := cfg.Probe.CafeOwnVendorHosts
	if len(hosts) == 0 {
		return cafe.DefaultConfig(), nil
	}
	c, err := cafe.DefaultConfig().WithOwnVendor("own loyalty product", hosts)
	if err != nil {
		return nil, fmt.Errorf("leads.yaml probe.cafe_own_vendor_hosts: %w", err)
	}
	return c, nil
}

// buildCafeLeads is `leads build` for a campaign whose source is overture: every org a place
// of the campaign points at is classified with cafe.Classify from its site's latest cafe@1
// probe (read back from the store, probe.CafeResult), or from the place alone when it has
// no website of its own. An org whose latest place is a chain (buildChains: stored as one,
// or one now that a later file of its release is in) makes no lead, nor does a Skip
// (already a customer, the homepage not read yet) or a kind the spec does not list (off). A lead rests on a cafe@1
// finding whose subject holds the Decision's reason and the language source, and whose
// evidence is the probe's signals. A lead still new is refreshed or skipped as in the
// Drupal build; one past new is left as it is.
func buildCafeLeads(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, spec campaign.Spec, campaignID int64) (buildSummary, error) {
	sum := buildSummary{ByKind: map[string]map[string]int{}, NoLead: map[string]int{}}
	orgs, err := lq.CafeOrgsForBuild(ctx, campaignID)
	if err != nil {
		return sum, err
	}
	noLead := func(o leadstore.CafeBuildOrg, reason string) error {
		sum.NoLead[reason]++
		if o.LeadID == nil {
			return nil
		}
		moved, err := lq.SkipNewLead(ctx, *o.LeadID, "build: "+reason)
		if err != nil {
			return err
		}
		if moved {
			sum.Skipped++
		} else {
			sum.Kept++
			sum.Leads++
			sum.count(o.LeadKind, o.LeadChannel)
		}
		return nil
	}
	chains := newBuildChains(lq, spec)
	for _, o := range orgs {
		sum.Orgs++
		if o.LeadID != nil && o.LeadState != leadstore.LeadNew {
			sum.Kept++
			sum.Leads++
			sum.count(o.LeadKind, o.LeadChannel)
			continue
		}
		rule, err := chains.rule(ctx, o)
		if err != nil {
			return sum, err
		}
		if rule != "" {
			if err := noLead(o, "a chain ("+rule+")"); err != nil {
				return sum, err
			}
			continue
		}
		if outreach.PublicBody(o.Domain, cfg.PublicBodySuffixes) {
			if err := noLead(o, outreach.ReasonPublicBody); err != nil {
				return sum, err
			}
			continue
		}
		in := cafe.Input{}
		var probeID *int64
		if o.SiteID != nil {
			in.OwnWebsite = o.Domain
			if o.ProbeID != nil && o.DerivedBy == cafe.DerivedBy && o.ProbeOutcome == leadstore.OutcomeOK {
				in.HomepageRead = true
				in.Detection = probe.CafeResult(o.Signals)
				probeID = o.ProbeID
			}
		}
		d := cafe.Classify(in)
		if d.Skip {
			if err := noLead(o, d.Reason); err != nil {
				return sum, err
			}
			continue
		}
		if !slices.Contains(spec.Kinds, d.Kind) {
			if err := noLead(o, "kind off: "+d.Kind); err != nil {
				return sum, err
			}
			continue
		}
		stored, err := lq.RoutesFor(ctx, o.OrgID)
		if err != nil {
			return sum, err
		}
		routes := make([]outreach.Route, len(stored))
		for i, r := range stored {
			routes[i] = outreach.Route{ID: r.ID, Kind: r.Kind, Value: r.Value, MXOK: r.MXOK}
		}
		channel, routeID := outreach.PickChannel(cfg.Policy(o.Country).Channel, routes)
		fid, err := lq.EnsureCafeFinding(ctx, cafeFinding(o, d, in, probeID))
		if err != nil {
			return sum, err
		}
		row := leadstore.LeadRow{OrgID: o.OrgID, Kind: d.Kind, ProbeID: probeID, Channel: channel, RouteID: routeID,
			CampaignID: campaignID, FindingID: &fid}
		if o.LeadID != nil {
			changed, err := lq.RefreshLead(ctx, row)
			if err != nil {
				return sum, err
			}
			if changed {
				sum.Refreshed++
			}
		} else if _, created, err := lq.UpsertLead(ctx, row); err != nil {
			return sum, err
		} else if created {
			sum.Created++
		}
		sum.Leads++
		if outreach.ReservedDomain(o.Domain) {
			sum.Reserved++
		}
		sum.count(d.Kind, channel)
	}
	return sum, nil
}

// cafeFinding is the finding a café lead rests on: the Decision's reason and the language
// source as its subject, each stored signal as its evidence.
func cafeFinding(o leadstore.CafeBuildOrg, d cafe.Decision, in cafe.Input, probeID *int64) leadstore.CafeFinding {
	r := in.Detection
	subject := map[string]any{"reason": d.Reason, "website": in.OwnWebsite}
	if r.Language != "" {
		subject["language"], subject["language_via"], subject["language_note"] = r.Language, r.LanguageVia, r.LanguageNote
	}
	var ev []leadstore.FindingDetail
	if probeID != nil {
		for _, s := range o.Signals {
			ev = append(ev, leadstore.FindingDetail{Name: s.Signal, Value: s.Value, FetchID: s.FetchID})
		}
	}
	return leadstore.CafeFinding{OrgID: o.OrgID, Kind: d.Kind, ProbeID: probeID, PlaceID: o.PlaceID,
		Detector: cafe.DerivedBy, Subject: subject, Evidence: ev}
}
