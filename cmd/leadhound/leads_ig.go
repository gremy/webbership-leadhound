package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/igsignal"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["ig"] = runLeadsIG }

const igUsage = "usage: leadhound leads ig import FILE [--campaign SLUG] [--decider jev|fake] [--max-calls N] [--dry-run]" +
	" | report [--campaign SLUG] | lookups [--campaign SLUG] [--place-type roastery|unclear|any] [--name-word]" +
	" | review [--status new|dismissed|matched|wanted|all] | match HANDLE LEAD_ID | unmatch HANDLE | dismiss HANDLE | want HANDLE"

const (
	// commandLeadsIGImport is the run row of an import; its source is the decider (jev or fake).
	commandLeadsIGImport = "leads-ig-import"
	// defaultIGMaxCalls is the most Jev calls one import makes when --max-calls is not given.
	defaultIGMaxCalls = 100
	// igDefaultCampaign is the one café campaign an import matches against by default.
	igDefaultCampaign = "cafes-loyalty"
	// igLineErrorsShown is how many refused lines an import names, by number.
	igLineErrorsShown = 10
	igDeciderFake     = "fake"
	igDeciderJev      = "jev"
)

// runLeadsIG is `leads ig <verb>`: Instagram signals on café leads. import writes (and may
// call Jev); report, lookups and review only read, so make remote runs them free (its list
// matches a command's first three words); match, unmatch, dismiss and want are the
// operator's own calls. Nothing here changes a lead, an org or a place type.
func runLeadsIG(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("%s", igUsage)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "import":
		return igImportCmd(ctx, rest)
	case "report":
		return igReportCmd(ctx, rest)
	case "lookups":
		return igLookupsCmd(ctx, rest)
	case "review":
		return igReviewCmd(ctx, rest)
	case "match", "unmatch", "dismiss", "want":
		return igOperatorCmd(ctx, verb, rest)
	}
	return usageError("unknown ig verb %q; %s", verb, igUsage)
}

// igFake is the keyless stand-in for the Instagram questions: every answer is unclear, below
// leadrate.IGMinConfidence, so a fake import claims nothing about a café and a later Jev run
// (another decider, so another hash) asks again. A var so a test can script its own.
var igFake = func() ai.Decider {
	a := ai.Answer{Choice: leadrate.IGUnclear, Confidence: 0.5}
	return &ai.Fake{Answers: map[string]ai.Answer{leadrate.IGPlace: a, leadrate.IGLoyalty: a, leadrate.IGVendor: a}}
}

// igDecider resolves --decider for import: with no flag it is jev when LEADHOUND_LIVE=1 and
// the fake otherwise, so an unattended run never reaches Jev. jev needs LEADHOUND_LIVE=1 and
// TYPESAFE_API_KEY (pickDecider).
func igDecider(cfg config.Config, name string) (ai.Decider, string, error) {
	if name == "" {
		name = igDeciderFake
		if cfg.Live == config.LiveAll {
			name = igDeciderJev
		}
	}
	switch name {
	case igDeciderFake:
		return igFake(), name, nil
	case igDeciderJev:
		if cfg.Live != config.LiveAll {
			return nil, "", usageError("--decider jev needs LEADHOUND_LIVE=1 (%s)", liveOffMessage)
		}
		d, err := pickDecider(cfg, igDeciderJev, "leads ig import")
		return d, name, err
	}
	return nil, "", usageError("--decider must be jev or fake, not %q", name)
}

// igSummary is what one import did; it holds counts only.
type igSummary struct {
	Records, Walls, Refused                int
	ProfilesNew, ProfilesKnown             int
	Missing, Private                       int
	Lists, ListsPartial, Kept, FollowsNew  int
	Hints                                  int
	ByTier                                 map[string]int // the matcher's result: handle, domain, name, tie, none
	OperatorKept, MatchesWritten           int
	Calls, Unchanged, NoText, Failures     int
	Due                                    int // profiles with text Jev was not asked about: a stop, or a failed call
	StopReason                             string
	Queued, MarkedMatched, RoutesAdded     int
	RuleVendors                            int
	ProfilesWithText, WouldAsk, Unreadable int // dry run only
}

// igPending is a profile's match waiting for its review decision, made once every profile of
// the file has been judged.
type igPending struct {
	ownCustomer bool
	vendor      bool
}

// igImporter is one import's state. matches and status are the latest per handle, kept
// current as the import writes.
type igImporter struct {
	lq       leadstore.Queries
	db       store.DBTX
	cfg      *cafe.Config
	matcher  *igsignal.Matcher
	keys     map[int64]leadstore.IGLeadKey
	decider  ai.Decider // capped; nil on a dry run
	name     string     // the decider's own name, as the judgment hash reads it
	maxCalls int
	dry      bool

	sum     igSummary
	matches map[string]leadstore.IGMatch
	status  map[string]string
	pending map[string]igPending
	order   []string // pending handles in file order
	inARow  int
}

// igImportCmd is `leads ig import FILE`: it reads a capture file (ig-capture@1), stores every
// profile and following list in it, matches each to the campaign's café leads, asks Jev about
// each profile with text (one call, three questions, skipped when the input is unchanged),
// writes an instagram route on a strong match the lead's org lacks, and queues the review
// list. A record with a wall is counted and nothing of it is stored. --dry-run reads the
// file and the leads, writes nothing and calls nobody. A stop (--max-calls, the leads cap,
// an unavailable provider, failures in a row) leaves the rest due for the next import of the
// same file and exits 0.
func igImportCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads ig import", flag.ContinueOnError)
	slug := fs.String("campaign", igDefaultCampaign, "the café campaign whose leads profiles are matched to")
	deciderName := fs.String("decider", "", "jev or fake (default fake, or jev with LEADHOUND_LIVE=1)")
	maxCalls := fs.Int("max-calls", defaultIGMaxCalls, "most Jev calls in this import")
	dry := fs.Bool("dry-run", false, "read the file and the leads; write nothing, call nobody")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *maxCalls < 0 {
		return usageError("%s", igUsage)
	}
	raw, err := os.ReadFile(pos[0]) // #nosec G304 -- the operator names the capture file
	if err != nil {
		return usageError("read the capture file: %v", err)
	}
	recs, lineErrs := igsignal.Parse(bytes.NewReader(raw))

	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	var decider ai.Decider
	kind := cmp.Or(*deciderName, "none")
	if !*dry {
		if decider, kind, err = igDecider(cfg, *deciderName); err != nil {
			return err
		}
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return usageError("LEADHOUND_TZ: %v", err)
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
	ccfg, err := cafeConfig(setup.leads)
	if err != nil {
		return err
	}
	im, err := newIGImporter(ctx, lq, s.Pool(), ccfg, c.ID, recs)
	if err != nil {
		return err
	}
	im.sum.Refused = len(lineErrs)
	im.maxCalls, im.dry = *maxCalls, *dry
	if *dry {
		if err := im.dryRun(ctx, recs); err != nil {
			return err
		}
		printIGDryRun(stdout, c.Slug, im.sum, lineErrs)
		return nil
	}
	im.name = decider.Name()
	im.decider = &ai.Capped{Decider: decider, Store: s.Q(), DailyCalls: setup.jevLeadsDailyCap(), Loc: loc, Purpose: ai.PurposeLeads}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsIGImport, kind)
	if err != nil {
		return err
	}
	runErr := im.run(ctx, recs)
	result := store.RunResult{Requests: im.sum.Calls, Failures: im.sum.Failures, StopReason: im.sum.StopReason,
		Note: fmt.Sprintf("%d records, %d new profiles, %d unchanged, %d still due, %d review rows, %d routes",
			im.sum.Records, im.sum.ProfilesNew, im.sum.Unchanged, im.sum.Due, im.sum.Queued, im.sum.RoutesAdded)}
	switch {
	case runErr != nil:
		result.Note += "; " + runErr.Error()
	case ctx.Err() != nil:
		result.Note += "; interrupted"
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	printIGImport(stdout, runID, c.Slug, kind, im.maxCalls, setup.jevLeadsDailyCap(), im.sum, lineErrs)
	return ctx.Err()
}

// newIGImporter reads what an import compares against: the campaign's café leads (the
// matcher's keys) and the latest match and review status of every handle in recs.
func newIGImporter(ctx context.Context, lq leadstore.Queries, db store.DBTX, cfg *cafe.Config, campaignID int64, recs []igsignal.Record) (*igImporter, error) {
	keys, err := lq.IGLeadKeys(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	im := &igImporter{lq: lq, db: db, cfg: cfg, matcher: igsignal.NewMatcher(cfg, keys), keys: map[int64]leadstore.IGLeadKey{},
		status: map[string]string{}, pending: map[string]igPending{}, sum: igSummary{ByTier: map[string]int{}}}
	for _, k := range keys {
		im.keys[k.LeadID] = k
	}
	var handles []string
	for _, r := range recs {
		switch {
		case r.Profile != nil:
			handles = append(handles, r.Profile.Handle)
		case r.Following != nil:
			handles = append(handles, r.Following.Of)
			for _, k := range r.Following.Kept {
				handles = append(handles, k.Handle)
			}
		}
	}
	if im.matches, err = lq.IGMatches(ctx, handles); err != nil {
		return nil, err
	}
	review, err := lq.IGReview(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, it := range review {
		im.status[it.Handle] = it.Status
	}
	return im, nil
}

// run is one import: the profiles first (stored, judged, matched), their review decisions
// once every profile is judged, then the following lists, whose accounts a profile read
// (now or before) leaves to that profile.
func (im *igImporter) run(ctx context.Context, recs []igsignal.Record) error {
	var lists []igsignal.Record
	for _, r := range recs {
		im.sum.Records++
		if r.Wall() != "" {
			im.sum.Walls++
			continue
		}
		if r.Following != nil {
			lists = append(lists, r)
			continue
		}
		if err := im.profile(ctx, r); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	if err := im.reviewProfiles(ctx); err != nil {
		return err
	}
	for _, r := range lists {
		if err := im.following(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// igProfileRow is a capture's profile as ig_profile stores it: a private account is an ok
// read with is_private true.
func igProfileRow(p igsignal.Profile, sha []byte) leadstore.IGProfile {
	row := leadstore.IGProfile{Handle: p.Handle, Outcome: leadstore.IGOutcomeOK, IGUserID: p.IGUserID, FullName: p.FullName,
		Category: p.Category, Bio: p.Bio, ExternalURL: p.ExternalURL, Followers: p.Followers, Following: p.Following,
		Posts: p.Posts, IsPrivate: p.IsPrivate, Captions: p.Captions, Source: p.Source.Type, SourceOf: p.Source.Of,
		SourceLeadID: p.Source.LeadID, CaptureSHA: sha, CapturedAt: p.CapturedAt}
	switch p.Outcome {
	case igsignal.OutcomeMissing:
		row.Outcome = leadstore.IGOutcomeMissing
	case igsignal.OutcomePrivate:
		yes := true
		row.IsPrivate = &yes
	}
	return row
}

// profile stores one profile capture, writes its rule rows when the capture is new, asks Jev
// when it has text, and matches it. A missing handle is stored and judged by rule only: it
// matches nothing and is never reviewed.
func (im *igImporter) profile(ctx context.Context, rec igsignal.Record) error {
	wctx := context.WithoutCancel(ctx)
	p := rec.Profile
	row := igProfileRow(*p, rec.SHA256)
	id, isNew, err := im.lq.InsertIGProfile(wctx, row)
	if err != nil {
		return err
	}
	row.ID = id
	if isNew {
		im.sum.ProfilesNew++
	} else {
		im.sum.ProfilesKnown++
	}
	switch p.Outcome {
	case igsignal.OutcomeMissing:
		im.sum.Missing++
	case igsignal.OutcomePrivate:
		im.sum.Private++
	}
	if isNew {
		if err := im.rules(wctx, row, *p); err != nil {
			return err
		}
	}
	if err := im.judge(ctx, row); err != nil {
		return err
	}
	if p.Outcome == igsignal.OutcomeMissing {
		return nil
	}
	if err := im.hintFromSource(wctx, *p); err != nil {
		return err
	}
	r := im.matcher.Match(*p)
	if err := im.applyMatch(wctx, p.Handle, r); err != nil {
		return err
	}
	if _, seen := im.pending[p.Handle]; !seen {
		im.order = append(im.order, p.Handle)
	}
	im.pending[p.Handle] = igPending{ownCustomer: r.OwnCustomer, vendor: r.VendorAccount != ""}
	return nil
}

// rules writes a new capture's rule rows: the vendor its links or tags show (outranking Jev's
// answer), and unclear on every question Jev is not asked because the capture has no text
// (Decision 5).
func (im *igImporter) rules(ctx context.Context, row leadstore.IGProfile, p igsignal.Profile) error {
	vendor, via, ok := "", "", false
	if p.Outcome != igsignal.OutcomeMissing {
		vendor, via, ok = igsignal.Vendor(p, im.cfg)
	}
	if ok {
		im.sum.RuleVendors++
		if _, err := im.lq.AddIGJudgment(ctx, leadstore.IGJudgment{ProfileID: row.ID, Question: leadstore.IGVendor,
			Choice: vendor, Source: leadstore.IGFromRule, Detail: "from a " + via}); err != nil {
			return err
		}
	}
	if leadrate.IGHasText(row) {
		return nil
	}
	for _, q := range []string{leadstore.IGPlace, leadstore.IGLoyalty, leadstore.IGVendor} {
		if q == leadstore.IGVendor && ok {
			continue
		}
		if _, err := im.lq.AddIGJudgment(ctx, leadstore.IGJudgment{ProfileID: row.ID, Question: q,
			Choice: leadrate.IGUnclear, Source: leadstore.IGFromRule, Detail: "no text to judge"}); err != nil {
			return err
		}
	}
	return nil
}

// judge asks Jev the three questions about a profile with text, once per input: a profile of
// a handle whose latest Jev answer was asked on the same input (decider, questions, state,
// threshold, version) is unchanged and costs nothing. After a stop every further profile with
// text is due. Only a store error or a cancel is returned.
func (im *igImporter) judge(ctx context.Context, row leadstore.IGProfile) error {
	if !leadrate.IGHasText(row) {
		im.sum.NoText++
		return nil
	}
	state := leadrate.IGState(row)
	hash := leadrate.IGHash(im.name, *im.cfg, state)
	prev, err := im.lq.IGJudgmentHash(ctx, row.Handle, leadstore.IGPlace)
	if err != nil {
		return err
	}
	if bytes.Equal(prev, hash) {
		im.sum.Unchanged++
		return nil
	}
	if im.sum.StopReason != "" {
		im.sum.Due++
		return nil
	}
	if im.sum.Calls >= im.maxCalls {
		im.sum.StopReason = rateStopMax
		im.sum.Due++
		return nil
	}
	qs := leadrate.IGQuestions(*im.cfg)
	answers, usage, err := im.decider.Decide(ctx, state, qs)
	if !errors.Is(err, ai.ErrSpendCap) {
		im.sum.Calls++
	}
	if err != nil {
		switch {
		case errors.Is(err, ai.ErrUsageStore):
			return err
		case errors.Is(err, ai.ErrSpendCap):
			im.sum.StopReason = scoring.StopSpendCap
		case errors.Is(err, ai.ErrUnavailable):
			im.sum.Failures++
			im.sum.StopReason = scoring.StopThrottled
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			im.sum.Failures++
			if im.inARow++; im.inARow >= rateFailuresInARow {
				im.sum.StopReason = rateStopFailures
			}
		}
		im.sum.Due++
		return nil
	}
	for _, q := range qs {
		if _, ok := answers[q.Name]; !ok {
			im.sum.Failures++ // the decider checks its questions, so never expected
			im.sum.Due++
			return nil
		}
	}
	im.inARow = 0
	wctx := context.WithoutCancel(ctx)
	model := cutRunes(cmp.Or(usage.Model, im.name), rateModelRunes)
	for _, q := range qs {
		choice, raw, conf := leadrate.IGEffective(answers[q.Name])
		if _, err := im.lq.AddIGJudgment(wctx, leadstore.IGJudgment{ProfileID: row.ID, Question: q.Name, Choice: choice,
			RawChoice: raw, Confidence: &conf, Source: leadstore.IGFromJev, Model: model, InputSHA: hash}); err != nil {
			return err
		}
	}
	return nil
}

// tierWord is how a match result is counted: its tier, or tie for two or more leads.
func tierWord(r igsignal.Result) string {
	if len(r.LeadIDs) > 1 {
		return "tie"
	}
	return r.Tier
}

// applyMatch stores r as handle's match unless the operator's own call stands (the latest
// match is his, a match or an unmatch: it always wins), and writes the instagram route of the
// handle's strong match when the lead's org has none.
func (im *igImporter) applyMatch(ctx context.Context, handle string, r igsignal.Result) error {
	im.sum.ByTier[tierWord(r)]++
	latest, had := im.matches[handle]
	switch {
	case had && latest.Actor == leadstore.ActorGremy:
		im.sum.OperatorKept++
	case had && latest.Strong() && r.Strong() && latest.LeadID == r.LeadIDs[0]:
		// The same lead, strongly: a domain match turned handle match by the route this
		// import wrote is not a new match, so a re-import stores nothing.
	default:
		m := r.Store(handle)
		changed, err := im.lq.SetIGMatch(ctx, m)
		if err != nil {
			return err
		}
		if changed {
			im.sum.MatchesWritten++
			im.matches[handle] = m
		}
	}
	return im.ensureRoute(ctx, handle)
}

// ensureRoute writes handle as the instagram route of the lead its latest match strongly
// names, when that lead's org holds no live instagram route of any kind (the operator's own
// included, so his is never replaced). A lead outside the campaign is left alone.
func (im *igImporter) ensureRoute(ctx context.Context, handle string) error {
	m, ok := im.matches[handle]
	if !ok || !m.Strong() {
		return nil
	}
	k, ok := im.keys[m.LeadID]
	if !ok {
		return nil
	}
	added, err := igEnsureRoute(ctx, im.lq, k.OrgID, handle)
	if added {
		im.sum.RoutesAdded++
	}
	return err
}

// igEnsureRoute writes handle as org orgID's instagram route (DerivedByInstagram) unless the
// org already holds a live instagram route; added reports whether it wrote one.
func igEnsureRoute(ctx context.Context, lq leadstore.Queries, orgID int64, handle string) (bool, error) {
	routes, err := lq.RoutesFor(ctx, orgID)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(routes, func(r leadstore.Route) bool { return r.Kind == leadstore.RouteInstagram }) {
		return false, nil
	}
	return lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteInstagram, Value: handle,
		DerivedBy: leadstore.DerivedByInstagram})
}

// hintFromSource records that a loyalty vendor's own account follows the profile, when the
// profile was reached from that account's following list.
func (im *igImporter) hintFromSource(ctx context.Context, p igsignal.Profile) error {
	if p.Source.Type != igsignal.SourceFollowing {
		return nil
	}
	vendor := im.matcher.MatchFollow(igsignal.Follow{Handle: p.Source.Of}).VendorAccount
	if vendor == "" {
		return nil
	}
	return im.hint(ctx, p.Handle, vendor, p.CapturedAt)
}

func (im *igImporter) hint(ctx context.Context, handle, vendor string, at time.Time) error {
	added, err := im.lq.AddIGHint(ctx, leadstore.IGHint{Handle: handle, Kind: leadstore.IGHintFollowedByVendor, Value: vendor, CapturedAt: at})
	if added {
		im.sum.Hints++
	}
	return err
}

// igCoffeePlace reports whether an ig_place choice says a coffee place.
func igCoffeePlace(choice string) bool {
	return choice == leadrate.IGCafeSeating || choice == leadrate.IGRoasteryOrOnline
}

// reviewProfiles makes the review decision of every profile matched in this import
// (Decision 6): a weak or tied match waits for the operator with its candidates, and an
// unmatched coffee place (by its standing ig_place answer) is a café LeadHound may lack. A
// strong match marks a handle still new as matched. Own customers, vendors' accounts and a
// handle the operator unmatched are never queued; a handle reviewed before keeps its status.
func (im *igImporter) reviewProfiles(ctx context.Context) error {
	wctx := context.WithoutCancel(ctx)
	places, err := igStandingPlaces(ctx, im.db, im.order)
	if err != nil {
		return err
	}
	for _, h := range im.order {
		p := im.pending[h]
		if p.ownCustomer || p.vendor {
			continue
		}
		if err := im.review(wctx, h, igCoffeePlace(places[h])); err != nil {
			return err
		}
	}
	return nil
}

// review queues handle or marks it matched from its latest match; coffee says the handle is
// a coffee place with no lead to show it on (an unmatched one is queued only then).
func (im *igImporter) review(ctx context.Context, h string, coffee bool) error {
	m := im.matches[h]
	switch {
	case m.Strong():
		if im.status[h] != leadstore.IGReviewNew {
			return nil
		}
		changed, err := im.lq.SetIGReviewStatus(ctx, h, leadstore.IGReviewMatched, "", leadstore.ActorSystem)
		if changed {
			im.sum.MarkedMatched++
			im.status[h] = leadstore.IGReviewMatched
		}
		return err
	case m.Actor == leadstore.ActorGremy:
		return nil
	case m.Tier != leadstore.IGTierNone || coffee:
		queued, err := im.lq.QueueIGReview(ctx, h)
		if queued {
			im.sum.Queued++
			im.status[h] = leadstore.IGReviewNew
		}
		return err
	}
	return nil
}

// following stores one following list. Its accounts are matched by handle and name only, and
// only those never read as a profile: a read account's own profile decides its match. An
// account never read is a coffee name (the list's prefilter) and goes to the review list
// unless it matches strongly. Each account's name is kept only when it is matched or on the
// review list (Decision 9). A list a loyalty vendor's own account owns hints every account it
// keeps.
func (im *igImporter) following(ctx context.Context, rec igsignal.Record) error {
	wctx := context.WithoutCancel(ctx)
	f := rec.Following
	im.sum.Lists++
	if !f.Complete {
		im.sum.ListsPartial++
	}
	im.sum.Kept += len(f.Kept)
	handles := make([]string, len(f.Kept))
	for i, k := range f.Kept {
		handles[i] = k.Handle
	}
	read, err := igReadHandles(ctx, im.db, handles)
	if err != nil {
		return err
	}
	vendorOwner := im.matcher.MatchFollow(igsignal.Follow{Handle: f.Of}).VendorAccount
	rows := make([]leadstore.IGFollowRow, 0, len(f.Kept))
	for _, k := range f.Kept {
		keepName, err := im.followAccount(wctx, k, read[k.Handle])
		if err != nil {
			return err
		}
		row := leadstore.IGFollowRow{Handle: k.Handle}
		if keepName {
			row.FullName = k.FullName
		}
		rows = append(rows, row)
		if vendorOwner != "" {
			if err := im.hint(wctx, k.Handle, vendorOwner, f.CapturedAt); err != nil {
				return err
			}
		}
	}
	n, err := im.lq.InsertIGFollows(wctx, f.Of, f.CapturedAt, rec.SHA256, rows)
	im.sum.FollowsNew += int(n)
	return err
}

// followAccount matches and reviews one account of a following list and reports whether its
// name is kept.
func (im *igImporter) followAccount(ctx context.Context, k igsignal.Follow, read bool) (keepName bool, err error) {
	if !read {
		r := im.matcher.MatchFollow(k)
		if r.VendorAccount != "" {
			im.sum.ByTier[tierWord(r)]++
			return false, nil
		}
		if err := im.applyMatch(ctx, k.Handle, r); err != nil {
			return false, err
		}
		if err := im.review(ctx, k.Handle, true); err != nil {
			return false, err
		}
	}
	_, reviewed := im.status[k.Handle]
	return im.matches[k.Handle].Strong() || reviewed, nil
}

// dryRun counts what an import of recs would meet: the walls, the profiles with text and how
// many of them Jev would be asked about with the fake, and what each profile and following
// account matches now. It writes nothing and calls nobody.
func (im *igImporter) dryRun(ctx context.Context, recs []igsignal.Record) error {
	for _, r := range recs {
		im.sum.Records++
		switch {
		case r.Wall() != "":
			im.sum.Walls++
		case r.Profile != nil:
			p := r.Profile
			switch p.Outcome {
			case igsignal.OutcomeMissing:
				im.sum.Missing++
				continue
			case igsignal.OutcomePrivate:
				im.sum.Private++
			}
			row := igProfileRow(*p, r.SHA256)
			if leadrate.IGHasText(row) {
				im.sum.ProfilesWithText++
				prev, err := im.lq.IGJudgmentHash(ctx, p.Handle, leadstore.IGPlace)
				if err != nil {
					return err
				}
				if !bytes.Equal(prev, leadrate.IGHash(ai.FakeName, *im.cfg, leadrate.IGState(row))) {
					im.sum.WouldAsk++
				}
			}
			if _, _, ok := igsignal.Vendor(*p, im.cfg); ok {
				im.sum.RuleVendors++
			}
			im.sum.ByTier[tierWord(im.matcher.Match(*p))]++
		case r.Following != nil:
			im.sum.Lists++
			im.sum.Kept += len(r.Following.Kept)
			for _, k := range r.Following.Kept {
				im.sum.ByTier[tierWord(im.matcher.MatchFollow(k))]++
			}
		}
	}
	return nil
}

// igReadHandles is which of handles have been read as a profile, in any import.
func igReadHandles(ctx context.Context, db store.DBTX, handles []string) (map[string]bool, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT handle FROM ig_profile WHERE handle = ANY($1::text[])`, handles)
	if err != nil {
		return nil, fmt.Errorf("leads ig: profiles read: %w", err)
	}
	hs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("leads ig: profiles read: %w", err)
	}
	out := make(map[string]bool, len(hs))
	for _, h := range hs {
		out[h] = true
	}
	return out, nil
}

// igStandingPlaces is the standing ig_place choice of each of handles, read as leadstore's
// lead card reads it: from the newest capture holding an answer, a rule's before Jev's.
func igStandingPlaces(ctx context.Context, db store.DBTX, handles []string) (map[string]string, error) {
	rows, err := db.Query(ctx,
		`SELECT DISTINCT ON (p.handle) p.handle, j.choice
		 FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id
		 WHERE p.handle = ANY($1::text[]) AND j.question = '`+leadstore.IGPlace+`'
		 ORDER BY p.handle, j.profile_id DESC, (j.source = '`+leadstore.IGFromRule+`') DESC, j.id DESC`, handles)
	if err != nil {
		return nil, fmt.Errorf("leads ig: standing places: %w", err)
	}
	out := map[string]string{}
	var h, choice string
	if _, err := pgx.ForEachRow(rows, []any{&h, &choice}, func() error { out[h] = choice; return nil }); err != nil {
		return nil, fmt.Errorf("leads ig: standing places: %w", err)
	}
	return out, nil
}

// printIGImport prints an import's counts. Nothing personal is printed: no handle, no name.
func printIGImport(w io.Writer, runID int64, slug, decider string, maxCalls, dailyCap int, s igSummary, lineErrs []igsignal.LineError) {
	_, _ = fmt.Fprintf(w, "leads ig import: run %d · campaign %s · decider %s · %s · %s · %s refused\n",
		runID, slug, decider, plural(s.Records, "record", "records"), plural(s.Walls, "wall", "walls"), plural(s.Refused, "line", "lines"))
	printIGLineErrors(w, lineErrs)
	_, _ = fmt.Fprintf(w, "  profiles: %d new · %d already imported · %d missing · %d private\n",
		s.ProfilesNew, s.ProfilesKnown, s.Missing, s.Private)
	_, _ = fmt.Fprintf(w, "  following lists: %d (%d cut short) · %d accounts kept · %d rows new · %d hints new\n",
		s.Lists, s.ListsPartial, s.Kept, s.FollowsNew, s.Hints)
	_, _ = fmt.Fprintf(w, "  matches: handle %d · domain %d · name %d · tie %d · none %d · the operator's call kept %d · %d rows written\n",
		s.ByTier[leadstore.IGTierHandle], s.ByTier[leadstore.IGTierDomain], s.ByTier[leadstore.IGTierName], s.ByTier["tie"],
		s.ByTier[leadstore.IGTierNone], s.OperatorKept, s.MatchesWritten)
	_, _ = fmt.Fprintf(w, "  jev: calls %d · unchanged %d · no text %d · failed %d · still due %d · stop %s · vendor by rule %d\n",
		s.Calls, s.Unchanged, s.NoText, s.Failures, s.Due, cmp.Or(s.StopReason, "none"), s.RuleVendors)
	switch s.StopReason {
	case rateStopMax:
		_, _ = fmt.Fprintf(w, "  stopped at --max-calls %d: %s still due; import the same file again to ask them\n",
			maxCalls, plural(s.Due, "profile", "profiles"))
	case scoring.StopSpendCap:
		_, _ = fmt.Fprintf(w, "  stopped: the leads cap of %d calls a day is spent; %s still due\n", dailyCap, plural(s.Due, "profile", "profiles"))
	case scoring.StopThrottled:
		_, _ = fmt.Fprintf(w, "  stopped: Jev is unavailable; %s still due\n", plural(s.Due, "profile", "profiles"))
	case rateStopFailures:
		_, _ = fmt.Fprintf(w, "  stopped: %d failed calls in a row; %s still due\n", rateFailuresInARow, plural(s.Due, "profile", "profiles"))
	}
	_, _ = fmt.Fprintf(w, "  review: %d added · %d marked matched\n", s.Queued, s.MarkedMatched)
	_, _ = fmt.Fprintf(w, "  routes: %d added\n", s.RoutesAdded)
}

// printIGDryRun prints what a dry run counted.
func printIGDryRun(w io.Writer, slug string, s igSummary, lineErrs []igsignal.LineError) {
	_, _ = fmt.Fprintf(w, "leads ig import --dry-run: campaign %s · %s · %s · %s refused · nothing written, nobody called\n",
		slug, plural(s.Records, "record", "records"), plural(s.Walls, "wall", "walls"), plural(s.Refused, "line", "lines"))
	printIGLineErrors(w, lineErrs)
	_, _ = fmt.Fprintf(w, "  profiles: %d with text · %d Jev would be asked about (fake) · %d missing · %d private · vendor by rule %d\n",
		s.ProfilesWithText, s.WouldAsk, s.Missing, s.Private, s.RuleVendors)
	_, _ = fmt.Fprintf(w, "  following lists: %d · %d accounts kept\n", s.Lists, s.Kept)
	_, _ = fmt.Fprintf(w, "  would match: handle %d · domain %d · name %d · tie %d · none %d\n",
		s.ByTier[leadstore.IGTierHandle], s.ByTier[leadstore.IGTierDomain], s.ByTier[leadstore.IGTierName], s.ByTier["tie"], s.ByTier[leadstore.IGTierNone])
}

// printIGLineErrors names the refused lines by number and reason (fixed words, never a
// line's text), the first igLineErrorsShown of them.
func printIGLineErrors(w io.Writer, errs []igsignal.LineError) {
	for i, e := range errs {
		if i == igLineErrorsShown {
			_, _ = fmt.Fprintf(w, "    and %d more\n", len(errs)-i)
			return
		}
		_, _ = fmt.Fprintf(w, "    %s\n", e.Error())
	}
}

// igKeys opens the database and reads the café leads of the campaign named slug (every
// campaign's when slug is empty), with a label naming what was read.
func igKeys(ctx context.Context, slug string) (lq leadstore.Queries, db store.DBTX, keys []leadstore.IGLeadKey, label string, done func(), err error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return leadstore.Queries{}, nil, nil, "", nil, err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return leadstore.Queries{}, nil, nil, "", nil, err
	}
	lq = leadstore.NewQueries(s.Pool())
	var campaignID int64
	label = "all café campaigns"
	if slug != "" {
		c, err := activeCampaign(ctx, lq, slug)
		if err != nil {
			s.Close()
			return leadstore.Queries{}, nil, nil, "", nil, err
		}
		campaignID, label = c.ID, "campaign "+c.Slug
	}
	if keys, err = lq.IGLeadKeys(ctx, campaignID); err != nil {
		s.Close()
		return leadstore.Queries{}, nil, nil, "", nil, err
	}
	return lq, s.Pool(), keys, label, s.Close, nil
}

// igReportLine is one strong match as report prints it.
type igReportLine struct {
	key    leadstore.IGLeadKey
	ig     *leadstore.IGLead
	handle string
	place  string
	loyal  string
	vendor string
}

func (l igReportLine) head() string {
	return fmt.Sprintf("lead %d · %s · %s · @%s", l.key.LeadID, cmp.Or(l.key.OrgName, "-"), cmp.Or(l.key.Town, "-"), l.handle)
}

// igSwitchPitch reports whether a strong match shows a loyalty scheme of the café's own: Jev
// says it runs one, or a vendor or a paper card is named. The own vendor's customers are not
// a switch.
func (l igReportLine) switchPitch() bool {
	if l.vendor == cafe.OwnVendorID {
		return false
	}
	return l.loyal == leadrate.IGRunsScheme || !slices.Contains([]string{"", leadrate.IGNoVendor, leadrate.IGUnclear}, l.vendor)
}

// igCorrection is what Instagram's place call says against the lead's effective place type,
// or "" when they agree or Instagram cannot tell.
func (l igReportLine) correction() string {
	switch igPlaceDisagrees(l.place, l.key.PlaceType) {
	case leadstore.PlaceRoastery:
		return "Instagram reads a roastery or online shop; the lead is a coffee shop"
	case leadstore.PlaceCafe:
		return "Instagram reads a café with seating; the lead is a roastery"
	}
	return ""
}

// igPlaceDisagrees is the place type Instagram's place call igPlace points to when it
// disagrees with a lead's effective place type placeType (leadstore.PlaceRoastery or
// leadstore.PlaceCafe), or "" when they agree or Instagram cannot tell. The report's "to
// correct" list and the lead page's Instagram card both read it; neither changes the lead.
func igPlaceDisagrees(igPlace, placeType string) string {
	switch {
	case igPlace == leadrate.IGRoasteryOrOnline && placeType == leadstore.PlaceCafe:
		return leadstore.PlaceRoastery
	case igPlace == leadrate.IGCafeSeating && placeType == leadstore.PlaceRoastery:
		return leadstore.PlaceCafe
	}
	return ""
}

// igReportCmd is `leads ig report`: it writes nothing. It counts every handle's latest match
// by tier, the review list by status and the standing answers on the campaign's strongly
// matched leads, prints one line per strong match, then the two lists the operator acts on:
// the leads to correct (Instagram's place call disagrees with the lead's place type; nothing
// is changed for him) and the switch pitches (a café already running a scheme).
func igReportCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads ig report", flag.ContinueOnError)
	slug := fs.String("campaign", "", "only this campaign's leads (default: every café campaign)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", igUsage)
	}
	lq, db, keys, label, done, err := igKeys(ctx, *slug)
	if err != nil {
		return err
	}
	defer done()
	byLead := make(map[int64]leadstore.IGLeadKey, len(keys))
	for _, k := range keys {
		byLead[k.LeadID] = k
	}
	tiers, strongLeads, err := igLatestTiers(ctx, db)
	if err != nil {
		return err
	}
	review, err := lq.IGReview(ctx, "")
	if err != nil {
		return err
	}
	statuses := map[string]int{}
	for _, it := range review {
		statuses[it.Status]++
	}
	var lines []igReportLine
	for _, id := range strongLeads {
		k, ok := byLead[id]
		if !ok {
			continue
		}
		ig, err := lq.IGForLead(ctx, id)
		if err != nil {
			return err
		}
		if ig == nil {
			continue
		}
		lines = append(lines, igReportLine{key: k, ig: ig, handle: ig.Match.Handle, place: ig.Judgments[leadstore.IGPlace].Choice,
			loyal: ig.Judgments[leadstore.IGLoyalty].Choice, vendor: ig.Judgments[leadstore.IGVendor].Choice})
	}
	judged := map[string]map[string]int{leadstore.IGPlace: {}, leadstore.IGLoyalty: {}, leadstore.IGVendor: {}}
	for _, l := range lines {
		for q, c := range map[string]string{leadstore.IGPlace: l.place, leadstore.IGLoyalty: l.loyal, leadstore.IGVendor: l.vendor} {
			judged[q][cmp.Or(c, "not judged")]++
		}
	}
	w := stdout
	_, _ = fmt.Fprintf(w, "leads ig report: %s, %s with a strong Instagram match\n", label, plural(len(lines), "café lead", "café leads"))
	_, _ = fmt.Fprintf(w, "  latest match per handle: handle %d · domain %d · name %d · tie %d · yours %d · unmatched by you %d · none %d\n",
		tiers[leadstore.IGTierHandle], tiers[leadstore.IGTierDomain], tiers[leadstore.IGTierName], tiers["tie"],
		tiers[leadstore.IGTierOperator], tiers["unmatched"], tiers[leadstore.IGTierNone])
	for _, q := range []string{leadstore.IGPlace, leadstore.IGLoyalty, leadstore.IGVendor} {
		_, _ = fmt.Fprintf(w, "  %s on matched leads: %s\n", strings.TrimPrefix(q, "ig_"), countWords(judged[q]))
	}
	_, _ = fmt.Fprintf(w, "  review: new %d · dismissed %d · matched %d · wanted %d\n", statuses[leadstore.IGReviewNew],
		statuses[leadstore.IGReviewDismissed], statuses[leadstore.IGReviewMatched], statuses[leadstore.IGReviewWanted])
	for _, l := range lines {
		_, _ = fmt.Fprintf(w, "  %s · now %s · IG place %s · loyalty %s · vendor %s\n", l.head(), cmp.Or(l.key.PlaceType, "-"),
			cmp.Or(l.place, "-"), cmp.Or(l.loyal, "-"), cmp.Or(l.vendor, "-"))
	}
	var fix, pitch []string
	for _, l := range lines {
		if c := l.correction(); c != "" {
			fix = append(fix, fmt.Sprintf("    %s · %s", l.head(), c))
		}
		if l.switchPitch() {
			pitch = append(pitch, fmt.Sprintf("    %s · loyalty %s · vendor %s", l.head(), cmp.Or(l.loyal, "-"), cmp.Or(l.vendor, "-")))
		}
	}
	_, _ = fmt.Fprintf(w, "  to correct: %d (nothing was changed: correct the place type on the lead's page)\n", len(fix))
	for _, s := range fix {
		_, _ = fmt.Fprintln(w, s)
	}
	_, _ = fmt.Fprintf(w, "  switch pitch: %d (already running a loyalty scheme)\n", len(pitch))
	for _, s := range pitch {
		_, _ = fmt.Fprintln(w, s)
	}
	return nil
}

// countWords is counts as "a 2 · b 1", by name; "none" when empty.
func countWords(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	slices.Sort(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s %d", n, counts[n])
	}
	return strings.Join(parts, " · ")
}

// igLatestTiers counts every handle's latest match by tier (a tie, the operator's match and
// his unmatch counted apart) and lists the leads a latest strong match names.
func igLatestTiers(ctx context.Context, db store.DBTX) (map[string]int, []int64, error) {
	rows, err := db.Query(ctx,
		`SELECT m.tier, coalesce(m.lead_id, 0), cardinality(m.candidate_ids)
		 FROM (SELECT DISTINCT ON (handle) tier, lead_id, candidate_ids FROM ig_match ORDER BY handle, id DESC) m`)
	if err != nil {
		return nil, nil, fmt.Errorf("leads ig report: matches: %w", err)
	}
	tiers := map[string]int{}
	var leads []int64
	var tier string
	var lead int64
	var cands int
	if _, err := pgx.ForEachRow(rows, []any{&tier, &lead, &cands}, func() error {
		m := leadstore.IGMatch{Tier: tier, LeadID: lead}
		switch {
		case cands > 0:
			tiers["tie"]++
		case tier == leadstore.IGTierOperator && lead == 0:
			tiers["unmatched"]++
		default:
			tiers[tier]++
		}
		if m.Strong() && !slices.Contains(leads, lead) {
			leads = append(leads, lead)
		}
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("leads ig report: matches: %w", err)
	}
	slices.Sort(leads)
	return tiers, leads, nil
}

// igField is s fit for one tab-separated field: tabs and line breaks become spaces.
func igField(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\t' || r == '\n' || r == '\r' }), " ")
}

// igLookupsCmd is `leads ig lookups`: it writes nothing. One tab-separated line per café lead
// for the /ig-signals skill to look up: id, org name, town, website domain, the Instagram
// handles it holds (comma-joined, may be empty), its effective place type and Jev's latest
// place call (empty when never asked). --place-type keeps the roasteries (effective type) or
// Jev's unclear calls; --name-word keeps the leads whose names carry a roastery word, read as
// place-types report reads them.
func igLookupsCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads ig lookups", flag.ContinueOnError)
	slug := fs.String("campaign", "", "only this campaign's leads (default: every café campaign)")
	placeType := fs.String("place-type", "any", "roastery, unclear (Jev's call) or any")
	nameWord := fs.Bool("name-word", false, "only leads whose names carry a roastery word")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || !slices.Contains([]string{"roastery", "unclear", "any"}, *placeType) {
		return usageError("%s", igUsage)
	}
	_, _, keys, _, done, err := igKeys(ctx, *slug)
	if err != nil {
		return err
	}
	defer done()
	for _, k := range keys {
		switch {
		case *placeType == "roastery" && k.PlaceType != leadstore.PlaceRoastery,
			*placeType == "unclear" && k.JevPlaceType != leadstore.PlaceUnclear:
			continue
		}
		if *nameWord {
			if _, _, ok := placeNameMatch(leadstore.CafeLeadName{OrgName: k.OrgName, SiteName: k.SiteName}); !ok {
				continue
			}
		}
		_, _ = fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", k.LeadID, igField(k.OrgName), igField(k.Town), igField(k.Domain),
			strings.Join(k.Handles, ","), k.PlaceType, k.JevPlaceType)
	}
	return nil
}

// igReviewCmd is `leads ig review`: it writes nothing. The review list at one status, by
// handle: its name, category, Jev's place call and the leads it may be.
func igReviewCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads ig review", flag.ContinueOnError)
	status := fs.String("status", leadstore.IGReviewNew, "new, dismissed, matched, wanted or all")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	statuses := []string{leadstore.IGReviewNew, leadstore.IGReviewDismissed, leadstore.IGReviewMatched, leadstore.IGReviewWanted}
	if len(pos) != 0 || (*status != "all" && !slices.Contains(statuses, *status)) {
		return usageError("%s", igUsage)
	}
	lq, _, done, err := igOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	read := *status
	if read == "all" {
		read = ""
	}
	items, err := lq.IGReview(ctx, read)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "leads ig review: %s at %s\n", plural(len(items), "handle", "handles"), *status)
	for _, it := range items {
		name, category := it.FollowName, ""
		if it.Profile != nil {
			name, category = cmp.Or(it.Profile.FullName, name), it.Profile.Category
		}
		cands := make([]string, len(it.Candidates))
		for i, c := range it.Candidates {
			cands[i] = strconv.FormatInt(c, 10)
		}
		line := fmt.Sprintf("  @%s · %s · %s · place %s · candidates %s", it.Handle, cmp.Or(name, "-"), cmp.Or(category, "-"),
			cmp.Or(it.Place, "-"), cmp.Or(strings.Join(cands, ", "), "none"))
		if *status == "all" {
			line += " · " + it.Status
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	return nil
}

// igOpen opens the database for the review list and the operator's calls.
func igOpen(ctx context.Context) (leadstore.Queries, store.DBTX, func(), error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return leadstore.Queries{}, nil, nil, err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return leadstore.Queries{}, nil, nil, err
	}
	return leadstore.NewQueries(s.Pool()), s.Pool(), s.Close, nil
}

// igKnown reports whether handle has been seen in a capture: a profile or a following list.
func igKnown(ctx context.Context, db store.DBTX, handle string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ig_profile WHERE handle = $1) OR EXISTS (SELECT 1 FROM ig_follow WHERE handle = $1)`,
		handle).Scan(&ok); err != nil {
		return false, fmt.Errorf("leads ig: known handle: %w", err)
	}
	return ok, nil
}

// igOperatorCmd is the operator's own calls on a handle seen in a capture: match HANDLE
// LEAD_ID attaches it to a café lead (it outranks every later import's match, and writes the
// lead's instagram route when its org has none); unmatch HANDLE says it is no lead (no import
// attaches it again, and a handle marked matched goes back on the review list); dismiss and
// want set its review status. The console's buttons run the same igOperator* functions.
func igOperatorCmd(ctx context.Context, verb string, args []string) error {
	want := 1
	if verb == "match" {
		want = 2
	}
	if len(args) != want {
		return usageError("%s", igUsage)
	}
	if _, ok := enrich.InstagramHandle(args[0]); !ok {
		return usageError("%q is not an Instagram handle", args[0])
	}
	var leadID int64
	if verb == "match" {
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return usageError("LEAD_ID must be a lead's number, not %q", args[1])
		}
		leadID = id
	}
	lq, db, done, err := igOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	err = igOperatorPrint(ctx, lq, db, verb, args[0], leadID)
	var r igRefusal
	if errors.As(err, &r) {
		return usageError("%s", r)
	}
	return err
}

// igOperatorPrint runs one operator's call and prints what it did.
func igOperatorPrint(ctx context.Context, lq leadstore.Queries, db store.DBTX, verb, raw string, leadID int64) error {
	switch verb {
	case "match":
		handle, added, err := igOperatorMatch(ctx, lq, db, raw, leadID)
		if err != nil {
			return err
		}
		route := "its org already holds an Instagram route"
		if added {
			route = "Instagram route added"
		}
		_, _ = fmt.Fprintf(stdout, "leads ig match: @%s is lead %d now (%s)\n", handle, leadID, route)
		return nil
	case "unmatch":
		handle, back, err := igOperatorUnmatch(ctx, lq, db, raw)
		if err != nil {
			return err
		}
		tail := ""
		if back {
			tail = "; back on the review list"
		}
		_, _ = fmt.Fprintf(stdout, "leads ig unmatch: @%s matches no lead%s\n", handle, tail)
		return nil
	}
	status := leadstore.IGReviewDismissed
	if verb == "want" {
		status = leadstore.IGReviewWanted
	}
	handle, changed, err := igSetStatus(ctx, lq, db, raw, status)
	if err != nil {
		return err
	}
	if !changed {
		_, _ = fmt.Fprintf(stdout, "leads ig %s: @%s is already %s; nothing written\n", verb, handle, status)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "leads ig %s: @%s is %s\n", verb, handle, status)
	return nil
}

// igRefusal is an operator's call on a handle refused, in words the CLI prints as a usage error
// and the console shows as a refusal: one set of words for both.
type igRefusal string

func (e igRefusal) Error() string { return string(e) }

// igOperatorHandle is raw as a stored handle, refused unless it is a handle seen in an
// imported capture (a profile or a following list).
func igOperatorHandle(ctx context.Context, db store.DBTX, raw string) (string, error) {
	handle, ok := enrich.InstagramHandle(raw)
	if !ok {
		return "", igRefusal(fmt.Sprintf("%q is not an Instagram handle", raw))
	}
	known, err := igKnown(ctx, db, handle)
	if err != nil {
		return "", err
	}
	if !known {
		return "", igRefusal(fmt.Sprintf("@%s is in no imported capture; import it first", handle))
	}
	return handle, nil
}

// igOperatorMatch is the operator's "it's this lead": raw attaches to café lead leadID (it
// outranks every later import's match), the handle is marked matched, and the lead's org gets
// the handle as its instagram route when it holds none. It is `leads ig match` and the
// console's button alike.
func igOperatorMatch(ctx context.Context, lq leadstore.Queries, db store.DBTX, raw string, leadID int64) (handle string, routeAdded bool, err error) {
	if handle, err = igOperatorHandle(ctx, db, raw); err != nil {
		return "", false, err
	}
	keys, err := lq.IGLeadKeys(ctx, 0)
	if err != nil {
		return "", false, err
	}
	i := slices.IndexFunc(keys, func(k leadstore.IGLeadKey) bool { return k.LeadID == leadID })
	if i < 0 {
		return "", false, igRefusal(fmt.Sprintf("lead %d is not a café lead Instagram can be matched to", leadID))
	}
	if _, err := lq.SetIGMatch(ctx, leadstore.IGMatch{Handle: handle, LeadID: leadID, Tier: leadstore.IGTierOperator,
		Detail: "your call", Actor: leadstore.ActorGremy}); err != nil {
		return "", false, err
	}
	if _, err := lq.SetIGReviewStatus(ctx, handle, leadstore.IGReviewMatched, "", leadstore.ActorGremy); err != nil {
		return "", false, err
	}
	routeAdded, err = igEnsureRoute(ctx, lq, keys[i].OrgID, handle)
	return handle, routeAdded, err
}

// igOperatorUnmatch is the operator's "it is no lead": an operator's match with no lead, so no
// import attaches it again; a handle marked matched goes back on the review list (back).
func igOperatorUnmatch(ctx context.Context, lq leadstore.Queries, db store.DBTX, raw string) (handle string, back bool, err error) {
	if handle, err = igOperatorHandle(ctx, db, raw); err != nil {
		return "", false, err
	}
	if _, err := lq.SetIGMatch(ctx, leadstore.IGMatch{Handle: handle, Tier: leadstore.IGTierOperator,
		Detail: "unmatched by you", Actor: leadstore.ActorGremy}); err != nil {
		return "", false, err
	}
	items, err := lq.IGReview(ctx, leadstore.IGReviewMatched)
	if err != nil {
		return "", false, err
	}
	if !slices.ContainsFunc(items, func(it leadstore.IGReviewItem) bool { return it.Handle == handle }) {
		return handle, false, nil
	}
	_, err = lq.SetIGReviewStatus(ctx, handle, leadstore.IGReviewNew, "unmatched", leadstore.ActorGremy)
	return handle, err == nil, err
}

// igSetStatus is the operator's dismiss (not a lead) or want (wanted as a lead) on raw: its
// review status, by him; changed is false when it already had that status.
func igSetStatus(ctx context.Context, lq leadstore.Queries, db store.DBTX, raw, status string) (handle string, changed bool, err error) {
	if status != leadstore.IGReviewDismissed && status != leadstore.IGReviewWanted {
		return "", false, fmt.Errorf("leads ig: %q is not an operator's review status", status)
	}
	if handle, err = igOperatorHandle(ctx, db, raw); err != nil {
		return "", false, err
	}
	changed, err = lq.SetIGReviewStatus(ctx, handle, status, "", leadstore.ActorGremy)
	return handle, changed, err
}
