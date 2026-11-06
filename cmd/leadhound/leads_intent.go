package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/igsignal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["intent"] = runLeadsIntent }

const intentUsage = "usage: leadhound leads intent sync [--campaign SLUG] [--dry-run] | report [--campaign SLUG]" +
	" | set LEAD_ID CLASS [--note TEXT] | clear LEAD_ID"

// intentCatchUp is the line an import, or an operator's call, prints when the loyalty intent
// could not follow it.
const intentCatchUp = "  loyalty intent: not synced; run leads intent sync"

// runLeadsIntent is `leads intent <verb>`: each café lead's loyalty intent class (intent@1),
// derived from what is stored, never asked of a model. sync writes the derived classes; report
// writes nothing (two verbs, because make remote's free list matches a command's first words);
// set and clear are the operator's own call, which wins over the derived class.
func runLeadsIntent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("%s", intentUsage)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "sync":
		return intentSyncCmd(ctx, rest)
	case "report":
		return intentReportCmd(ctx, rest)
	case "set", "clear":
		return intentCallCmd(ctx, verb, rest)
	}
	return usageError("unknown intent verb %q; %s", verb, intentUsage)
}

// intentConfigFor is the IntentConfig every sync builds from leads.yaml: the cafe@1 signal list
// with the own vendor added exactly as the importer builds it (cafeConfig), and igsignal's rules
// as functions, because leadstore cannot import igsignal.
func intentConfigFor(cfg leadconfig.Config) (leadstore.IntentConfig, error) {
	ccfg, err := cafeConfig(cfg)
	if err != nil {
		return leadstore.IntentConfig{}, err
	}
	m := igsignal.NewMatcher(ccfg, nil)
	prof := func(p leadstore.IGProfile) igsignal.Profile {
		return igsignal.Profile{Handle: p.Handle, FullName: p.FullName, Bio: p.Bio, ExternalURL: p.ExternalURL, Captions: p.Captions}
	}
	return leadstore.IntentConfig{
		Cafe:          ccfg,
		VendorAccount: func(h string) string { return m.MatchFollow(igsignal.Follow{Handle: h}).VendorAccount },
		RuleVendor: func(p leadstore.IGProfile) string {
			id, _, _ := igsignal.Vendor(prof(p), ccfg)
			return id
		},
		Prepaid: func(p leadstore.IGProfile) bool { return igsignal.Prepaid(prof(p), ccfg) },
	}, nil
}

// syncIntent is the one intent sync every trigger runs (the verb, the import, match, unmatch,
// the console's Match, set and clear): campaignID 0 syncs every café campaign.
func syncIntent(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, campaignID int64, dry bool) (leadstore.IntentSync, error) {
	icfg, err := intentConfigFor(cfg)
	if err != nil {
		return leadstore.IntentSync{}, err
	}
	return lq.SyncIntent(ctx, icfg, campaignID, leadsNow(), dry)
}

// intentSyncLine is what one sync left, in rank order: "intent: shopping 3, paper 5, … (changed n)";
// a dry run says "would change" and that nothing was written.
func intentSyncLine(s leadstore.IntentSync, dry bool) string {
	parts := make([]string, 0, len(leadstore.IntentClasses))
	for _, c := range leadstore.IntentClasses {
		parts = append(parts, fmt.Sprintf("%s %d", c, s.Classes[c]))
	}
	tail := fmt.Sprintf("(changed %d)", len(s.Changes))
	if dry {
		tail = fmt.Sprintf("(would change %d; nothing written)", len(s.Changes))
	}
	return "intent: " + strings.Join(parts, ", ") + " " + tail
}

// intentCampaign parses a sync or report's flags, opens the store and resolves --campaign.
func intentCampaign(ctx context.Context, verb, slug string, args []string, dry *bool) (st *store.Store, setup leadsSetup, c leadstore.Campaign, err error) {
	fs := flag.NewFlagSet("leads intent "+verb, flag.ContinueOnError)
	s := fs.String("campaign", slug, "the café campaign")
	if dry != nil {
		fs.BoolVar(dry, "dry-run", false, "say what would be written; write nothing")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, setup, c, err
	}
	if len(pos) != 0 {
		return nil, setup, c, usageError("%s", intentUsage)
	}
	if st, setup, err = leadsOpen(ctx, leadsForProbe); err != nil {
		return nil, setup, c, err
	}
	if c, err = activeCampaign(ctx, leadstore.NewQueries(st.Pool()), *s); err != nil {
		st.Close()
		return nil, setup, c, err
	}
	return st, setup, c, nil
}

// intentSyncCmd is `leads intent sync`: every café lead of the campaign gets its derived class
// written where it changed. A rerun writes nothing; --dry-run writes nothing.
func intentSyncCmd(ctx context.Context, args []string) error {
	var dry bool
	st, setup, c, err := intentCampaign(ctx, "sync", igDefaultCampaign, args, &dry)
	if err != nil {
		return err
	}
	defer st.Close()
	lq := leadstore.NewQueries(st.Pool())
	s, err := syncIntent(ctx, lq, setup.leads, c.ID, dry)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "leads intent sync: campaign %s, %s; no model called\n", c.Slug, plural(s.Leads, "café lead", "café leads"))
	_, _ = fmt.Fprintf(stdout, "  %s\n", intentSyncLine(s, dry))
	return nil
}

// intentReportLead is one café lead as the report names it.
type intentReportLead struct {
	ID                 int64
	Name, Town, Handle string
}

// intentReportHandlesSQL is each lead of $1's Instagram handle: of the handles whose latest
// match is a strong or operator one naming the lead, the most recently matched (as the lead
// card, IGForLead, names it).
const intentReportHandlesSQL = `SELECT DISTINCT ON (x.lead_id) x.lead_id, x.handle FROM (
	   SELECT DISTINCT ON (handle) handle, id, coalesce(lead_id, 0) AS lead_id, tier FROM ig_match
	   WHERE handle IN (SELECT handle FROM ig_match WHERE lead_id = ANY($1::bigint[])) ORDER BY handle, id DESC) x
	 WHERE x.lead_id = ANY($1::bigint[]) AND x.tier IN ('` + leadstore.IGTierHandle + `', '` + leadstore.IGTierDomain + `', '` + leadstore.IGTierOperator + `')
	 ORDER BY x.lead_id, x.id DESC`

// intentReportCmd is `leads intent report`: it writes nothing. The count of the campaign's café
// leads per effective class, then one block per class in rank order, the highest score first.
func intentReportCmd(ctx context.Context, args []string) error {
	st, _, c, err := intentCampaign(ctx, "report", igDefaultCampaign, args, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	lq := leadstore.NewQueries(st.Pool())
	keys, err := lq.IGLeadKeys(ctx, c.ID)
	if err != nil {
		return err
	}
	leads := make([]intentReportLead, 0, len(keys))
	ids := make([]int64, 0, len(keys))
	for _, k := range keys {
		leads = append(leads, intentReportLead{ID: k.LeadID, Name: cmp.Or(k.OrgName, k.PlaceName, k.SiteName, "no name"), Town: cmp.Or(k.Town, "no town")})
		ids = append(ids, k.LeadID)
	}
	handles := map[int64]string{}
	if len(ids) > 0 {
		rows, err := st.Pool().Query(ctx, intentReportHandlesSQL, ids)
		if err != nil {
			return fmt.Errorf("leads intent report: handles: %w", err)
		}
		for rows.Next() {
			var id int64
			var h string
			if err := rows.Scan(&id, &h); err != nil {
				rows.Close()
				return fmt.Errorf("leads intent report: handles: %w", err)
			}
			handles[id] = h
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("leads intent report: handles: %w", err)
		}
	}
	intents, err := lq.LatestIntents(ctx, ids)
	if err != nil {
		return err
	}
	byClass := map[string][]intentReportLead{}
	for _, l := range leads {
		l.Handle = handles[l.ID]
		cl := intents[l.ID].Class
		byClass[cl] = append(byClass[cl], l)
	}
	_, _ = fmt.Fprintf(stdout, "leads intent report: campaign %s, %s\n", c.Slug, plural(len(leads), "café lead", "café leads"))
	for _, cl := range leadstore.IntentClasses {
		_, _ = fmt.Fprintf(stdout, "  %s: %d\n", cl, len(byClass[cl]))
	}
	for _, cl := range leadstore.IntentClasses {
		ls := byClass[cl]
		if len(ls) == 0 {
			continue
		}
		slices.SortStableFunc(ls, func(a, b intentReportLead) int {
			return cmp.Or(cmp.Compare(intents[b.ID].Score, intents[a.ID].Score), cmp.Compare(a.ID, b.ID))
		})
		_, _ = fmt.Fprintf(stdout, "\n%s (%d)\n", cl, len(ls))
		for _, l := range ls {
			_, _ = fmt.Fprintf(stdout, "  %s\n", intentReportLine(l, intents[l.ID]))
		}
	}
	return nil
}

// intentReportLine is one lead of the report:
// "lead <id> · <name> · <town> · @<handle> · overlap <n> · <reason> · <date>". Under the
// operator's call the reason is "your call" with its note; the date is the latest signal's,
// else the last sync's.
func intentReportLine(l intentReportLead, r leadstore.IntentRow) string {
	handle := "no Instagram"
	if l.Handle != "" {
		handle = "@" + l.Handle
	}
	reason := cmp.Or(r.Reason, "no loyalty signal yet") // cold with no row: a sync writes no plain cold
	if r.Yours {
		reason = "your call"
		if r.Note != "" {
			reason += ": " + r.Note
		}
	}
	date := "no date"
	switch {
	case r.SignalAt != nil:
		date = r.SignalAt.Format("2 Jan 2006")
	case r.SyncedAt != nil:
		date = r.SyncedAt.Format("2 Jan 2006")
	}
	return fmt.Sprintf("lead %d · %s · %s · %s · overlap %d · %s · %s", l.ID, l.Name, l.Town, handle, r.Overlap, reason, date)
}

// intentCallCmd is `leads intent set LEAD_ID CLASS [--note TEXT]` and `leads intent clear
// LEAD_ID`: the operator's call on a café lead's class, then a sync of the lead's campaign, so
// the class shown is the one the batch reads. It refuses an unknown class, an unknown lead and
// a lead that is not a café lead.
func intentCallCmd(ctx context.Context, verb string, args []string) error {
	fs := flag.NewFlagSet("leads intent "+verb, flag.ContinueOnError)
	note := fs.String("note", "", "why, in a few words (set only)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	want := 2
	if verb == "clear" {
		want = 1
		if *note != "" {
			return usageError("--note goes with set, not clear; %s", intentUsage)
		}
	}
	if len(pos) != want {
		return usageError("%s", intentUsage)
	}
	leadID, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || leadID <= 0 {
		return usageError("LEAD_ID must be a lead's number, not %q", pos[0])
	}
	var class *string
	if verb == "set" {
		if !slices.Contains(leadstore.IntentClasses, pos[1]) {
			return usageError("%q is not an intent class; say one of %s", pos[1], strings.Join(leadstore.IntentClasses, ", "))
		}
		class = &pos[1]
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	switch err := lq.SetIntentCall(ctx, leadID, class, *note); {
	case errors.Is(err, leadstore.ErrLeadNotFound):
		return usageError("there is no lead %d", leadID)
	case errors.Is(err, leadstore.ErrNotCafeLead):
		return usageError("lead %d is not a café lead; only café leads have a loyalty intent", leadID)
	case errors.Is(err, leadstore.ErrUnknownIntentClass):
		return usageError("%q is not an intent class; say one of %s", *class, strings.Join(leadstore.IntentClasses, ", "))
	case errors.Is(err, leadstore.ErrBadIntentNote):
		return usageError("the note is over %d characters; say it shorter", leadstore.IntentMaxNote)
	case err != nil:
		return err
	}
	lead, err := lq.LeadByID(ctx, leadID)
	if err != nil {
		return err
	}
	sync, err := syncIntent(ctx, lq, setup.leads, lead.CampaignID, false)
	if err != nil {
		return fmt.Errorf("lead %d's call is written, but the intent is not synced yet (run leads intent sync): %w", leadID, err)
	}
	intents, err := lq.LatestIntents(ctx, []int64{leadID})
	if err != nil {
		return err
	}
	r := intents[leadID]
	whose := "derived"
	if r.Yours {
		whose = "your call"
	}
	_, _ = fmt.Fprintf(stdout, "leads intent %s: lead %d is %s now (%s)\n", verb, leadID, r.Class, whose)
	_, _ = fmt.Fprintf(stdout, "  %s\n", intentSyncLine(sync, false))
	return nil
}
