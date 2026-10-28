package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/report"
)

func init() { leadsCommands["report"] = runLeadsReport }

const leadsReportUsage = "usage: leadhound leads report LEAD --out FILE [--contact ADDRESS] (FILE outside the repository, never overwritten)"

// reportMagnet is the campaign offer.magnet that names the upgrade-risk report.
const reportMagnet = "upgrade_risk_report"

// reportStore is what a report reads from the lead store; leadstore.Queries is one.
type reportStore interface {
	LeadByID(ctx context.Context, id int64) (leadstore.Lead, error)
	OrgByID(ctx context.Context, id int64) (leadstore.Org, error)
	LeadEvidence(ctx context.Context, leadIDs []int64) (map[int64]leadstore.Evidence, error)
	Campaigns(ctx context.Context, accountID int64) ([]leadstore.Campaign, error)
}

var _ reportStore = leadstore.Queries{}

// runLeadsReport is `leadhound leads report LEAD --out FILE`: it writes the lead's free
// upgrade-risk report, one self-contained HTML page, to a new file at mode 0600. It refuses a
// path inside a checkout of this repository (the report names a real organisation) and never
// overwrites a file. The operator attaches it, or prints it to PDF, when the lead asks.
func runLeadsReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads report", flag.ContinueOnError)
	out := fs.String("out", "", "the file to write (outside the repository; never overwritten)")
	contact := fs.String("contact", "", "your contact address, shown under your name")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		return usageError(leadsReportUsage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id < 1 {
		return usageError("leads report: %q is not a lead id; %s", pos[0], leadsReportUsage)
	}
	path, err := reportOutPath(*out)
	if err != nil {
		return err
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := reportToFile(ctx, leadstore.NewQueries(s.Pool()), setup.leads, id, *contact, time.Now(), path); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "leads report: lead %d written to %s, mode 0600. Attach it to your reply, or open it and print to PDF.\n", id, path)
	return nil
}

// reportOutPath is out made absolute, refused when it lies inside a checkout of this
// repository or already exists.
func reportOutPath(out string) (string, error) {
	path, err := filepath.Abs(out)
	if err != nil {
		return "", usageError("--out %s: %v", out, err)
	}
	if root, inside := insideRepo(path); inside {
		return "", usageError("--out %s is inside the leadhound repository at %s; the report names a real organisation, so write it outside, e.g. under $LEADHOUND_DATA_DIR/reports/", path, root)
	}
	if _, err := os.Lstat(path); err == nil {
		return "", usageError("--out %s exists; a report never overwrites a file, so move it or choose another name", path)
	}
	return path, nil
}

// reportToFile renders lead leadID's report and writes it to path (checked again here, so
// no caller can skip the repository and overwrite rules).
func reportToFile(ctx context.Context, rs reportStore, cfg leadconfig.Config, leadID int64, contact string, now time.Time, path string) error {
	path, err := reportOutPath(path)
	if err != nil {
		return err
	}
	b, err := leadReportHTML(ctx, rs, cfg, leadID, contact, now)
	if err != nil {
		return err
	}
	if err := writeNew(path, b); err != nil {
		return fmt.Errorf("leads report: %w", errors.Unwrap(err))
	}
	return nil
}

// leadReportHTML is the report page for lead leadID, built from its stored facts. It is
// what the console's Report button serves too.
func leadReportHTML(ctx context.Context, rs reportStore, cfg leadconfig.Config, leadID int64, contact string, now time.Time) ([]byte, error) {
	lead, err := rs.LeadByID(ctx, leadID)
	if err != nil {
		return nil, err
	}
	spec, campaignRef, err := leadCampaignSpec(ctx, rs, cfg, lead.CampaignID)
	if err != nil {
		return nil, err
	}
	in, ev, err := sheetInputForLead(ctx, rs, cfg, lead, spec, campaignRef, outreach.LangEnglish)
	if err != nil {
		return nil, err
	}
	if ev.ProbeID == nil || in.Finding == nil || in.Finding.Major <= 0 {
		return nil, refused("Lead %d has no Drupal site finding, so it has no upgrade-risk report (the report is for site leads, not agency or hand-added leads).", leadID)
	}
	texts := report.DefaultMajorTexts() // until the campaign spec carries offer.report
	mt, ok := texts[in.Finding.Major]
	if !ok {
		return nil, refused("Lead %d runs Drupal %d, which has no report wording.", leadID, in.Finding.Major)
	}
	ri := report.Input{Sheet: in, ProbedAt: ev.ProbedAt, Major: mt, Now: now,
		ContactEmail: contact, ContactSource: outreach.Provenance{Table: "operator"}}
	if strings.TrimSpace(contact) == "" {
		// No --contact (the console's download): the contact line links the sender's site.
		ri.ContactURL = cfg.Sender.SiteURL
	}
	probe := outreach.Provenance{Table: slotProbe, ID: *ev.ProbeID}
	for _, sig := range ev.Signals {
		if w, ok := report.SignalWords(sig.Signal.Signal, sig.Value, ev.Major); ok {
			ri.Signals = append(ri.Signals, outreach.TextFact{Value: w, Source: probe})
		}
	}
	for _, pat := range spec.Copy.ForbidByKind[lead.Kind] {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("campaign %s: copy.forbid_by_kind.%s: %w", spec.Slug, lead.Kind, err)
		}
		ri.Forbid = append(ri.Forbid, re)
	}
	return report.UpgradeRisk(ri)
}

// leadCampaignSpec is the registered spec of campaign campaignID (what the console shows), or
// the legacy spec from cfg for the legacy campaign without a registered version.
func leadCampaignSpec(ctx context.Context, rs reportStore, cfg leadconfig.Config, campaignID int64) (campaign.Spec, outreach.Provenance, error) {
	ref := outreach.Provenance{Table: "campaign", ID: campaignID}
	cs, err := rs.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return campaign.Spec{}, ref, err
	}
	i := slices.IndexFunc(cs, func(c leadstore.Campaign) bool { return c.ID == campaignID })
	if i < 0 {
		return campaign.Legacy(&cfg), ref, nil
	}
	c := cs[i]
	if len(c.Spec) == 0 {
		return campaign.Legacy(&cfg), ref, nil
	}
	var spec campaign.Spec
	if err := json.Unmarshal(c.Spec, &spec); err != nil {
		return campaign.Spec{}, ref, fmt.Errorf("campaign %q: spec: %w", c.Slug, err)
	}
	return spec, ref, nil
}

// sheetInputForLead builds the fact-sheet input of one lead from its stored rows, the way
// leadSlots fills a template's slots: the org, the finding (a site lead's probe with its
// first version-naming signal worded in lang, or an agency lead's finding page), the
// end-of-life date from cfg, the sender from cfg and the campaign's proof line, and the offer
// (the free report) when the campaign's magnet is the upgrade-risk report. CP4's writer needs
// the same input; the evidence is returned for what only the report shows.
func sheetInputForLead(ctx context.Context, rs reportStore, cfg leadconfig.Config, lead leadstore.Lead,
	spec campaign.Spec, campaignRef outreach.Provenance, lang string) (outreach.SheetInput, leadstore.Evidence, error) {
	org, err := rs.OrgByID(ctx, lead.OrgID)
	if err != nil {
		return outreach.SheetInput{}, leadstore.Evidence{}, err
	}
	evs, err := rs.LeadEvidence(ctx, []int64{lead.ID})
	if err != nil {
		return outreach.SheetInput{}, leadstore.Evidence{}, err
	}
	ev := evs[lead.ID]
	orgSrc := outreach.Provenance{Table: slotOrg, ID: org.ID}
	in := outreach.SheetInput{
		Lang: lang,
		Org:  outreach.OrgFacts{Name: orgNameValue(org.Name), Country: org.Country, Domain: org.Domain, Source: orgSrc},
		Kind: lead.Kind,
		Sender: outreach.SenderFacts{
			Name: cfg.Sender.Name, IdentityLine: cfg.Sender.IdentityLine, ProofLine: spec.Offer.ProofLine,
			SiteURL: cmpOr(spec.Product.SiteURL, cfg.Sender.SiteURL), Source: outreach.ConfigSource,
		},
	}
	if spec.Offer.Magnet == reportMagnet {
		in.Offer = outreach.OfferFacts{Report: "a free one-page upgrade-risk report", Source: campaignRef}
	}
	switch {
	case ev.ProbeID != nil:
		probe := outreach.Provenance{Table: slotProbe, ID: *ev.ProbeID}
		f := &outreach.FindingFacts{Major: ev.Major, Exact: ev.ExactVersion, Source: probe}
		for _, sig := range ev.Signals {
			l, err := outreach.EvidenceLine(drupal.Signal{Name: sig.Signal.Signal, Value: sig.Value}, ev.Major, lang)
			if err == nil && sig.Fetch.ID > 0 {
				f.Signal, f.Evidence, f.URL = sig.Signal.Signal+" "+sig.Value, l, cmpOr(sig.Fetch.FinalURL, sig.Fetch.URL)
				break
			}
		}
		in.Finding = f
		if eol, ok := cfg.Facts.EOL(ev.Major); ok && !eol.IsZero() {
			in.EOL, in.EOLSource = eol.Time, outreach.ConfigSource
		}
	case ev.AgencyFindingID != nil:
		in.Finding = &outreach.FindingFacts{URL: ev.FindingURL, Evidence: ev.Phrase,
			Source: outreach.Provenance{Table: slotFinding, ID: *ev.AgencyFindingID}}
	}
	return in, ev, nil
}
