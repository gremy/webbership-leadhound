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
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/igsignal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["ig"] = runLeadsIG }

const igUsage = "usage: leadhound leads ig import FILE (ig-capture@1|@2) [--campaign SLUG] [--decider jev|fake] [--max-calls N] [--dry-run]" +
	" | report [--campaign SLUG] | lookups [--campaign SLUG] [--place-type roastery|unclear|any] [--name-word]" +
	" | review [--status new|dismissed|matched|wanted|all] | match HANDLE LEAD_ID | unmatch HANDLE | dismiss HANDLE | want HANDLE" +
	" | place-types [--dry-run]"

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
// operator's own calls; place-types writes Instagram's place read as each lead's instagram
// place type row, and import, match and unmatch end with the same sync. Nothing here changes
// a lead or an org; the place type changes only through those instagram rows, which the
// operator's own call always outranks.
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
	case "place-types":
		return igPlaceTypesCmd(ctx, rest)
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

// igJev is the real decider for import's --decider jev (pickDecider: TYPESAFE_API_KEY). A var
// so a test can stand in a scripted decider under a real model's name, never reaching Jev.
var igJev = func(cfg config.Config) (ai.Decider, error) { return pickDecider(cfg, igDeciderJev, "leads ig import") }

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
		d, err := igJev(cfg)
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
	NotCoffee, Held                        int // strong matches not attached: a not_coffee place call, or no call yet
	SiteWeak                               int // handle matches from a website alone under another name: weak, for review
	RoutesTaken                            int // routes an earlier import wrote, taken back (retired)
	Calls, Unchanged, NoText, Failures     int
	RealKept                               int // profiles the fake left alone: real Jev has answered their handle
	Due                                    int // profiles with text Jev was not asked about: a stop, or a failed call
	StopReason                             string
	Queued, MarkedMatched, RoutesAdded     int
	RuleVendors                            int
	FollowerLists, FollowerListsPartial    int
	FollowersNew, TouchesNew               int
	Touches                                map[string]int // touches read, by kind (leadstore.IGTouch*)
	ProfilesWithText, WouldAsk, Unreadable int            // dry run only
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
	db       *pgxpool.Pool
	leads    leadconfig.Config // leads.yaml: a route taken back re-picks the channel by its policy
	cfg      *cafe.Config
	matcher  *igsignal.Matcher
	keys     map[int64]leadstore.IGLeadKey
	decider  ai.Decider // capped; nil on a dry run
	name     string     // the decider's own name, as the judgment hash reads it
	maxCalls int
	dry      bool

	sum       igSummary
	matches   map[string]leadstore.IGMatch
	status    map[string]string
	places    map[string]string // the standing ig_place call of each profile read in this import
	notCoffee map[string]string // handles whose strong match was held back (not coffee, or a site's handle under another name): the note
	pending   map[string]igPending
	order     []string // pending handles in file order
	inARow    int
}

// igImportCmd is `leads ig import FILE`: it reads a capture file (ig-capture@1 or @2), stores
// every profile, following and followers list, comment and tag in it (D9), matches each to the campaign's café leads, asks Jev about
// each profile with text (one call, three questions, skipped when the input is unchanged),
// writes an instagram route on a strong match the lead's org lacks, and queues the review
// list. A record with a wall is counted and nothing of it is stored. Then it syncs each café
// lead's place type with Instagram's read (SyncIGPlaceTypes), even after a stop, then the
// campaign's loyalty intent (leads intent sync); an interrupt skips both syncs and says so. --dry-run reads the file and the leads, writes nothing and calls
// nobody; it says what a sync of what is stored now would write. A stop (--max-calls, the leads
// cap, an unavailable provider, failures in a row) leaves the rest due for the next import of
// the same file and exits 0.
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
	im, err := newIGImporter(ctx, lq, s.Pool(), setup.leads, ccfg, c.ID, recs)
	if err != nil {
		return err
	}
	im.sum.Refused = len(lineErrs)
	im.maxCalls, im.dry = *maxCalls, *dry
	if *dry {
		if err := im.dryRun(ctx, recs); err != nil {
			return err
		}
		sync, err := lq.SyncIGPlaceTypes(ctx, true)
		if err != nil {
			return err
		}
		printIGDryRun(stdout, c.Slug, im.sum, lineErrs)
		printIGPlaceSync(stdout, "  ", sync, true, " (from what is stored now, before this file)")
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
	// The place types follow the matches and answers this import left, a stop included (what
	// it wrote stands). An interrupt skips it: the operator asked to stop, and place-types or the
	// next import syncs. The sync itself is never cut short, as the import's own writes are not.
	var sync leadstore.IGPlaceSync
	synced := false
	if runErr == nil && ctx.Err() == nil {
		sync, runErr = lq.SyncIGPlaceTypes(context.WithoutCancel(ctx), false)
		synced = runErr == nil
	}
	// The loyalty intent follows the place types (intent@1's rule 3 reads the place type), on
	// the same guard: only after a place sync that ran. Its error is its own, so what the
	// import and the place sync wrote is still reported, with the catch-up line.
	var isync leadstore.IntentSync
	var intentErr error
	intentSynced := false
	if synced {
		isync, intentErr = syncIntent(context.WithoutCancel(ctx), lq, setup.leads, c.ID, false)
		intentSynced = intentErr == nil
	}
	result := store.RunResult{Requests: im.sum.Calls, Failures: im.sum.Failures, StopReason: im.sum.StopReason,
		Note: fmt.Sprintf("%d records, %d new profiles, %d unchanged, %d still due, %d review rows, %d routes",
			im.sum.Records, im.sum.ProfilesNew, im.sum.Unchanged, im.sum.Due, im.sum.Queued, im.sum.RoutesAdded)}
	switch {
	case runErr != nil:
		result.Note += "; " + runErr.Error() + "; place types not synced"
	case ctx.Err() != nil:
		result.Note += "; interrupted; place types not synced"
	case synced:
		result.Note += fmt.Sprintf("; place types from Instagram: roastery %d, café %d, silent %d", sync.Roastery, sync.Cafe, sync.Silent)
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		// What the run wrote before it stopped stands, so a roastery read it stored may not hold
		// its lead yet: say how to catch up (an interrupt during a Jev call lands here too).
		_, _ = fmt.Fprintln(stdout, "  place types from Instagram: not synced; run leads ig place-types")
		_, _ = fmt.Fprintln(stdout, intentCatchUp)
		return runErr
	}
	printIGImport(stdout, runID, c.Slug, kind, im.maxCalls, setup.jevLeadsDailyCap(), im.sum, lineErrs)
	switch {
	case !synced:
		_, _ = fmt.Fprintln(stdout, "  place types from Instagram: not synced (interrupted); run leads ig place-types")
		_, _ = fmt.Fprintln(stdout, intentCatchUp)
	default:
		printIGPlaceSync(stdout, "  ", sync, false, "")
		if !intentSynced {
			_, _ = fmt.Fprintln(stdout, intentCatchUp)
			return intentErr
		}
		_, _ = fmt.Fprintf(stdout, "  %s\n", intentSyncLine(isync, false))
	}
	return ctx.Err()
}

// igPlaceTypesCmd is `leads ig place-types`: it syncs every café lead's place type with
// Instagram's read as stored (SyncIGPlaceTypes), as an import ends, and prints what changed.
// It calls no model and reads no capture, so it is the backfill after a deploy; --dry-run says
// what it would write and writes nothing. A rerun writes nothing.
func igPlaceTypesCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads ig place-types", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "say what would be written; write nothing")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", igUsage)
	}
	lq, done, err := igOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	sync, err := lq.SyncIGPlaceTypes(ctx, *dry)
	if err != nil {
		return err
	}
	head := "leads ig place-types: synced from what is stored; no model called, no capture read"
	if *dry {
		head = "leads ig place-types --dry-run: nothing written; no model called, no capture read"
	}
	_, _ = fmt.Fprintln(stdout, head)
	printIGPlaceSync(stdout, "  ", sync, *dry, "")
	return nil
}

// printIGPlaceSync prints what one SyncIGPlaceTypes wrote (dry: would write), each line after
// indent: the rows by type, then each lead released to café outreach (the one road a roastery
// could be pitched, so never below the rest), then each lead held as a roastery. Lead ids
// only, as the import prints: no handle, no name.
func printIGPlaceSync(w io.Writer, indent string, s leadstore.IGPlaceSync, dry bool, tail string) {
	wrote, release, hold := "", "released", "held"
	if dry {
		wrote, release, hold = "would write ", "would release", "would hold"
	}
	_, _ = fmt.Fprintf(w, "%splace types from Instagram: %sroastery %d · café %d · silent %d%s\n", indent, wrote, s.Roastery, s.Cafe, s.Silent, tail)
	for _, c := range s.Released {
		_, _ = fmt.Fprintf(w, "%s  %s: %s\n", indent, release, igPlaceChangeLine(c))
	}
	for _, c := range s.Held {
		_, _ = fmt.Fprintf(w, "%s  %s: %s\n", indent, hold, igPlaceChangeLine(c))
	}
}

// igPlaceChangeLine is one lead a sync moved, with what decided it after and before:
// "lead 12: café from Instagram, was a roastery from its name". A lead Instagram fell silent
// on (an unmatch, a call that no longer decides) says so.
func igPlaceChangeLine(c leadstore.IGPlaceChange) string {
	line := fmt.Sprintf("lead %d: %s %s, was a %s %s", c.LeadID, igPlaceTypeWord(c.After.Type), igPlaceSourceWords(c.After.Source),
		igPlaceTypeWord(c.Before.Type), igPlaceSourceWords(c.Before.Source))
	if c.After.Source != leadstore.PlaceFromInstagram {
		line += " (Instagram is silent now)"
	}
	return line
}

// igPlaceChanges are a sync's moved leads, the released first.
func igPlaceChanges(s leadstore.IGPlaceSync) []leadstore.IGPlaceChange {
	return append(slices.Clone(s.Released), s.Held...)
}

// igPlaceChangeWords is one lead an operator's call moved, as a sentence without its full stop:
// "lead 12 is now a roastery, from Instagram's read", or "lead 12 is a café again" when
// Instagram fell silent and the next source (or the café default) speaks.
func igPlaceChangeWords(c leadstore.IGPlaceChange) string {
	fromIG := c.After.Source == leadstore.PlaceFromInstagram
	switch {
	case c.After.Type == leadstore.PlaceRoastery && fromIG:
		return fmt.Sprintf("lead %d is now a roastery, from Instagram's read", c.LeadID)
	case c.After.Type == leadstore.PlaceRoastery:
		return fmt.Sprintf("lead %d is a roastery again, %s", c.LeadID, igPlaceSourceWords(c.After.Source))
	case fromIG:
		return fmt.Sprintf("lead %d is now a café, from Instagram's read", c.LeadID)
	}
	return fmt.Sprintf("lead %d is a café again", c.LeadID)
}

// igPlaceTypeWord is a place type as the sync's lines say it.
func igPlaceTypeWord(placeType string) string {
	if placeType == leadstore.PlaceCafe {
		return "café"
	}
	return placeType
}

// igPlaceSourceWords is what decided a place type, in words: "by default" when no source spoke.
func igPlaceSourceWords(source string) string {
	switch source {
	case leadstore.PlaceFromOperator:
		return "from your call"
	case leadstore.PlaceFromInstagram:
		return "from Instagram"
	case leadstore.PlaceFromJev:
		return "from Jev's website read"
	case leadstore.PlaceFromName:
		return "from its name"
	case "":
		return "by default"
	}
	return "from " + source
}

// newIGImporter reads what an import compares against: the campaign's café leads (the
// matcher's keys) and the latest match and review status of every handle in recs.
func newIGImporter(ctx context.Context, lq leadstore.Queries, db *pgxpool.Pool, leads leadconfig.Config, cfg *cafe.Config, campaignID int64,
	recs []igsignal.Record) (*igImporter, error) {
	keys, err := lq.IGLeadKeys(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	im := &igImporter{lq: lq, db: db, leads: leads, cfg: cfg, matcher: igsignal.NewMatcher(cfg, keys), keys: map[int64]leadstore.IGLeadKey{},
		status: map[string]string{}, notCoffee: map[string]string{}, pending: map[string]igPending{}, sum: igSummary{ByTier: map[string]int{}, Touches: map[string]int{}}}
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
		case r.Followers != nil:
			for _, k := range r.Followers.Kept {
				handles = append(handles, k.Handle)
			}
		case r.Comments != nil:
			for _, p := range r.Comments.Posts {
				for _, c := range p.Comments {
					handles = append(handles, c.Handle)
				}
				for _, tg := range p.Tags {
					handles = append(handles, tg.Handle)
				}
			}
		case r.Tagged != nil:
			for _, k := range r.Tagged.Kept {
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

// run is one import: the profiles first (stored and judged), then matched once every one is
// judged (a strong match attaches only on the place call this import leaves standing), their
// review decisions, then the following lists, whose accounts a profile read (now or before)
// leaves to that profile, then the vendors' followers lists, comments and tagged posts (D9),
// whose accounts are matched the same way.
func (im *igImporter) run(ctx context.Context, recs []igsignal.Record) error {
	var lists, vendorReads, read []igsignal.Record
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
		if r.Profile == nil {
			vendorReads = append(vendorReads, r)
			continue
		}
		if err := im.profile(ctx, r); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if r.Profile.Outcome != igsignal.OutcomeMissing {
			read = append(read, r)
		}
	}
	if err := im.matchProfiles(ctx, read); err != nil {
		return err
	}
	if err := im.reviewProfiles(ctx); err != nil {
		return err
	}
	for _, r := range lists {
		if err := im.following(ctx, r); err != nil {
			return err
		}
	}
	for _, r := range vendorReads {
		var err error
		switch {
		case r.Followers != nil:
			err = im.followers(ctx, r)
		case r.Comments != nil:
			err = im.comments(ctx, r)
		case r.Tagged != nil:
			err = im.tagged(ctx, r)
		}
		if err != nil {
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

// profile stores one profile capture, writes its rule rows when the capture is new and asks
// Jev when it has text; matchProfiles matches it after. A missing handle is stored and judged
// by rule only: it matches nothing and is never reviewed.
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
	return im.judge(ctx, row)
}

// matchProfiles matches every profile read in this import, once all of them are judged, on the
// place call each one's judgment left standing, and records each for its review decision. A
// strong match held for want of a place call (Jev was not reached) waits for the next import.
func (im *igImporter) matchProfiles(ctx context.Context, read []igsignal.Record) error {
	wctx := context.WithoutCancel(ctx)
	handles := make([]string, len(read))
	for i, r := range read {
		handles[i] = r.Profile.Handle
	}
	places, err := igStandingPlaces(ctx, im.lq, handles)
	if err != nil {
		return err
	}
	im.places = places
	for _, rec := range read {
		p := rec.Profile
		if err := im.hintFromSource(wctx, *p); err != nil {
			return err
		}
		r := im.siteCheck(im.matcher.Match(*p), p.Handle, p.FullName, p.ExternalURL)
		held, err := im.applyMatch(wctx, p.Handle, r, places[p.Handle], true)
		if err != nil {
			return err
		}
		if held {
			continue
		}
		if _, seen := im.pending[p.Handle]; !seen {
			im.order = append(im.order, p.Handle)
		}
		im.pending[p.Handle] = igPending{ownCustomer: r.OwnCustomer, vendor: r.VendorAccount != ""}
	}
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
// a handle whose latest answer from the same kind of decider (the fake, or real Jev) was asked
// on the same input (decider, questions, state, threshold, version) is unchanged and costs
// nothing. The fake never answers a handle real Jev has answered (its answers stand). After a
// stop every further profile with text is due. Only a store error or a cancel is returned.
func (im *igImporter) judge(ctx context.Context, row leadstore.IGProfile) error {
	if !leadrate.IGHasText(row) {
		im.sum.NoText++
		return nil
	}
	fake := im.name == ai.FakeName
	if kept, err := im.realKept(ctx, row.Handle, fake); err != nil || kept {
		return err
	}
	state := leadrate.IGState(row)
	hash := leadrate.IGHash(im.name, *im.cfg, state)
	prev, err := im.lq.IGJudgmentHash(ctx, row.Handle, leadstore.IGPlace, fake)
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
	if fake {
		model = leadstore.IGFakeModel // what tells the fake's rows from real Jev's
	}
	for _, q := range qs {
		choice, raw, conf := leadrate.IGEffective(answers[q.Name])
		if _, err := im.lq.AddIGJudgment(wctx, leadstore.IGJudgment{ProfileID: row.ID, Question: q.Name, Choice: choice,
			RawChoice: raw, Confidence: &conf, Source: leadstore.IGFromJev, Model: model, InputSHA: hash}); err != nil {
			return err
		}
	}
	return nil
}

// realKept reports, for the fake decider (fake), whether real Jev has answered handle, counting
// it: the fake then neither asks nor writes, so real answers stay standing.
func (im *igImporter) realKept(ctx context.Context, handle string, fake bool) (bool, error) {
	if !fake {
		return false, nil
	}
	answered, err := im.lq.IGRealJudged(ctx, handle)
	if answered {
		im.sum.RealKept++
	}
	return answered, err
}

// tierWord is how a match result is counted: its tier, or tie for two or more leads.
func tierWord(r igsignal.Result) string {
	if len(r.LeadIDs) > 1 {
		return "tie"
	}
	return r.Tier
}

// igNotCoffeeWords end the note of a strong match held back because the profile's place call
// is not_coffee: the lead's website links someone else's account (a theme author's, another
// café's or roaster's).
const igNotCoffeeWords = "but not a coffee account"

// igNotCoffeeNote is the review note (and the stored match's detail) of a strong match at tier
// held back as not coffee.
func igNotCoffeeNote(tier string) string {
	if tier == leadstore.IGTierDomain {
		return "links to the lead's site, " + igNotCoffeeWords
	}
	return "linked from the lead's site, " + igNotCoffeeWords
}

// applyMatch stores r as handle's match unless the operator's own call stands (the latest
// match is his, a match or an unmatch: it always wins), and writes the instagram route of the
// handle's strong match when the lead's org has none. For a profile read (judged), place is
// its standing ig_place call: a strong match attaches only when that call is known and not
// not_coffee. A not_coffee one is stored as a weak match naming the lead (the operator's
// "it's this lead" still attaches it) with the not-coffee note; with no call yet nothing is
// stored and held is true. A following-list account never read has no call and attaches.
// Once stored, a route an earlier import wrote for the handle on any org the match no longer
// strongly names is taken back (igTakeBack).
func (im *igImporter) applyMatch(ctx context.Context, handle string, r igsignal.Result, place string, judged bool) (held bool, err error) {
	im.sum.ByTier[tierWord(r)]++
	latest, had := im.matches[handle]
	if r.Detail == igSiteHandleNote && (!had || latest.Actor != leadstore.ActorGremy) {
		im.sum.SiteWeak++
		im.notCoffee[handle] = igSiteHandleNote
	}
	switch {
	case had && latest.Actor == leadstore.ActorGremy:
		im.sum.OperatorKept++
		return false, im.ensureRoute(ctx, handle)
	case judged && r.Strong() && place == "":
		im.sum.Held++
		return true, nil
	case judged && r.Strong() && place == leadrate.IGNotCoffee:
		im.sum.NotCoffee++
		note := igNotCoffeeNote(r.Tier)
		im.notCoffee[handle] = note
		m := leadstore.IGMatch{Handle: handle, LeadID: r.LeadIDs[0], Tier: leadstore.IGTierName, Detail: note, Actor: leadstore.ActorSystem}
		changed, err := im.lq.SetIGMatch(ctx, m)
		if err != nil {
			return false, err
		}
		if changed {
			im.sum.MatchesWritten++
			im.matches[handle] = m
		}
		return false, im.takeBack(ctx, handle)
	case had && latest.Strong() && r.Strong() && latest.LeadID == r.LeadIDs[0]:
		// The same lead, strongly: a domain match turned handle match by the route this
		// import wrote is not a new match, so a re-import stores nothing.
	default:
		m := r.Store(handle)
		changed, err := im.lq.SetIGMatch(ctx, m)
		if err != nil {
			return false, err
		}
		if changed {
			im.sum.MatchesWritten++
			im.matches[handle] = m
		}
	}
	if err := im.takeBack(ctx, handle); err != nil {
		return false, err
	}
	return false, im.ensureRoute(ctx, handle)
}

// igSiteHandleNote is the review note (and the stored match's detail) of a handle match demoted
// by siteCheck.
const igSiteHandleNote = "linked from the lead's site, under another name"

// siteCheck demotes r, a handle match on one lead, to a weak match naming that lead (it waits
// for review) when the lead holds the handle from a website alone (not from the operator nor
// its map listing: IGLeadKey.TrustedHandles), the profile's link is not the lead's own domain,
// and the profile's name shares no word with the lead's names (igSharesNameWord): a site can
// link a theme author's or another café's account.
func (im *igImporter) siteCheck(r igsignal.Result, handle, fullName, link string) igsignal.Result {
	if r.Tier != leadstore.IGTierHandle || len(r.LeadIDs) != 1 {
		return r
	}
	k, ok := im.keys[r.LeadIDs[0]]
	h, _ := enrich.InstagramHandle(handle)
	if !ok || slices.Contains(k.TrustedHandles, h) || (k.Domain != "" && im.matcher.Domain(link) == k.Domain) ||
		igSharesNameWord(fullName, k.OrgName, k.SiteName, k.PlaceName) {
		return r
	}
	r.Tier, r.Detail = leadstore.IGTierName, igSiteHandleNote
	return r
}

// igFillerWords are words two unrelated names share without saying they are one business.
var igFillerWords = map[string]bool{"the": true, "and": true, "din": true, "for": true, "from": true, "shop": true, "bar": true}

// igSharesNameWord reports whether profileName and one of leadNames share a word, as NameKey
// folds them: three letters or more, not a coffee word (igsignal.CoffeeName: every café's name
// may hold one) and not a filler word.
func igSharesNameWord(profileName string, leadNames ...string) bool {
	words := func(s string) []string {
		var out []string
		for _, w := range strings.Fields(igsignal.NameKey(s)) {
			if utf8.RuneCountInString(w) >= 3 && !igFillerWords[w] && !igsignal.CoffeeName("", w) {
				out = append(out, w)
			}
		}
		return out
	}
	mine := words(profileName)
	for _, n := range leadNames {
		for _, w := range words(n) {
			if slices.Contains(mine, w) {
				return true
			}
		}
	}
	return false
}

// takeBack retires the routes an earlier import wrote for handle on every org its latest match
// does not strongly name (igTakeBack), counting them. A dry run never gets here.
func (im *igImporter) takeBack(ctx context.Context, handle string) error {
	var keep int64
	if m := im.matches[handle]; m.Strong() {
		keep = im.keys[m.LeadID].OrgID
	}
	n, err := igTakeBack(ctx, im.db, im.leads, handle, keep)
	im.sum.RoutesTaken += n
	return err
}

// igTakeBackNote is the note on a route an import wrote and took back.
const igTakeBackNote = "taken back: Instagram no longer matches this account to the lead"

// igTakeBack retires every route an Instagram import wrote for handle (IGImportRoutes: never one
// the operator typed or claimed, never one already marked) on an org other than keep (0: none
// kept), the way `leads contacts retire` does (a contact_mark retire, by the system, its unsent
// messages cancelled), and re-picks each such org's new leads' channels. n is how many it retired.
func igTakeBack(ctx context.Context, pool *pgxpool.Pool, cfg leadconfig.Config, handle string, keep int64) (n int, err error) {
	routes, err := leadstore.NewQueries(pool).IGImportRoutes(ctx, handle)
	if err != nil {
		return 0, err
	}
	for _, r := range routes {
		if r.OrgID == keep {
			continue
		}
		if err := contactTx(ctx, pool, cfg, r.OrgID, func(tq leadstore.Queries) error {
			return tq.MarkContact(ctx, leadstore.Mark{OrgID: r.OrgID, RouteID: r.ID, Mark: leadstore.MarkRetire, Note: igTakeBackNote, System: true})
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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
// org already holds a live instagram route, or handle is a loyalty or ordering vendor's own
// account (never a café's route); added reports whether it wrote one.
func igEnsureRoute(ctx context.Context, lq leadstore.Queries, orgID int64, handle string) (bool, error) {
	if leadstore.IsVendorIGHandle(handle) {
		return false, nil
	}
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
	for _, h := range im.order {
		p := im.pending[h]
		if p.ownCustomer || p.vendor {
			continue
		}
		if err := im.review(wctx, h, igCoffeePlace(im.places[h])); err != nil {
			return err
		}
	}
	return nil
}

// review queues handle or marks it matched from its latest match; coffee says the handle is
// a coffee place with no lead to show it on (an unmatched one is queued only then). A strong
// match held back as not coffee goes on the list with its note, new or back from a matched
// status the import gave it; a dismissed, wanted or operator's status stands.
func (im *igImporter) review(ctx context.Context, h string, coffee bool) error {
	m := im.matches[h]
	switch {
	case im.notCoffee[h] != "":
		if st := im.status[h]; st != "" && st != leadstore.IGReviewMatched {
			return nil
		}
		changed, err := im.lq.SetIGReviewStatus(ctx, h, leadstore.IGReviewNew, im.notCoffee[h], leadstore.ActorSystem)
		if changed {
			im.sum.Queued++
			im.status[h] = leadstore.IGReviewNew
		}
		return err
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
	vendorOwner := im.matcher.MatchFollow(igsignal.Follow{Handle: f.Of}).VendorAccount
	rows, err := im.followRows(ctx, f.Kept, vendorOwner, f.CapturedAt)
	if err != nil {
		return err
	}
	n, err := im.lq.InsertIGFollows(wctx, f.Of, leadstore.IGFollowing, f.CapturedAt, rec.SHA256, rows)
	im.sum.FollowsNew += int(n)
	return err
}

// followRows matches and reviews every account of a list (followAccount) and returns its rows,
// each name kept only when followAccount keeps it. A non-empty hintVendor (a loyalty vendor's
// own account owns a following list) hints every account.
func (im *igImporter) followRows(ctx context.Context, kept []igsignal.Follow, hintVendor string, at time.Time) ([]leadstore.IGFollowRow, error) {
	wctx := context.WithoutCancel(ctx)
	handles := make([]string, len(kept))
	for i, k := range kept {
		handles[i] = k.Handle
	}
	read, err := igReadHandles(ctx, im.db, handles)
	if err != nil {
		return nil, err
	}
	rows := make([]leadstore.IGFollowRow, 0, len(kept))
	for _, k := range kept {
		keepName, err := im.followAccount(wctx, k, read[k.Handle])
		if err != nil {
			return nil, err
		}
		row := leadstore.IGFollowRow{Handle: k.Handle}
		if keepName {
			row.FullName = k.FullName
		}
		rows = append(rows, row)
		if hintVendor != "" {
			if err := im.hint(wctx, k.Handle, hintVendor, at); err != nil {
				return nil, err
			}
		}
	}
	return rows, nil
}

// followers stores one followers list of a vendor's account (D9): its accounts are matched
// and reviewed as a following list's are, with no followed_by_vendor hint (the direction is
// the other way; the follow row itself is the signal).
func (im *igImporter) followers(ctx context.Context, rec igsignal.Record) error {
	f := rec.Followers
	im.sum.FollowerLists++
	if !f.Complete {
		im.sum.FollowerListsPartial++
	}
	rows, err := im.followRows(ctx, f.Kept, "", f.CapturedAt)
	if err != nil {
		return err
	}
	n, err := im.lq.InsertIGFollows(context.WithoutCancel(ctx), f.Of, leadstore.IGFollowers, f.CapturedAt, rec.SHA256, rows)
	im.sum.FollowersNew += int(n)
	return err
}

// igTouchIn is one touch as read, before followAccount decides what of it is kept: the
// account's name and a comment's text.
type igTouchIn struct {
	touch      leadstore.IGTouch
	name, text string
}

// igCommentTouches is a comments record's touches: a comment touch per comment (ref its id, at
// its created_at) and a usertag or mention touch per tag (ref the post code, at its taken_at).
func igCommentTouches(c *igsignal.Comments) []igTouchIn {
	var out []igTouchIn
	for _, p := range c.Posts {
		for _, cm := range p.Comments {
			out = append(out, igTouchIn{touch: leadstore.IGTouch{Handle: cm.Handle, Kind: leadstore.IGTouchComment, Ref: cm.ID,
				PostCode: p.Code, At: cm.CreatedAt}, name: cm.FullName, text: cm.Text})
		}
		for _, tg := range p.Tags {
			kind := leadstore.IGTouchUsertag
			if tg.Via == igsignal.ViaMention {
				kind = leadstore.IGTouchMention
			}
			out = append(out, igTouchIn{touch: leadstore.IGTouch{Handle: tg.Handle, Kind: kind, Ref: p.Code, PostCode: p.Code,
				At: p.TakenAt}, name: tg.FullName})
		}
	}
	return out
}

// igTaggedTouches is a tagged record's touches: a tagged touch per kept post (ref and post code
// its code, at its taken_at).
func igTaggedTouches(tg *igsignal.Tagged) []igTouchIn {
	out := make([]igTouchIn, 0, len(tg.Kept))
	for _, k := range tg.Kept {
		out = append(out, igTouchIn{touch: leadstore.IGTouch{Handle: k.Handle, Kind: leadstore.IGTouchTagged, Ref: k.Code,
			PostCode: k.Code, At: k.TakenAt}, name: k.FullName})
	}
	return out
}

func (im *igImporter) comments(ctx context.Context, rec igsignal.Record) error {
	return im.touches(ctx, rec, rec.Comments.Of, rec.Comments.CapturedAt, igCommentTouches(rec.Comments))
}

func (im *igImporter) tagged(ctx context.Context, rec igsignal.Record) error {
	return im.touches(ctx, rec, rec.Tagged.Of, rec.Tagged.CapturedAt, igTaggedTouches(rec.Tagged))
}

// touches stores one record's touches of vendor's account. Every touch account is matched and
// reviewed as a follow account is (followAccount), and the touch is stored whatever the match,
// so a later match finds it by handle. Its name is kept only when followAccount keeps it; a
// comment's text only when its account's match is strong at this import (D9 privacy: coffee
// words do not mean "not a person"), else the touch keeps the handle and the date alone.
func (im *igImporter) touches(ctx context.Context, rec igsignal.Record, vendor string, at time.Time, ins []igTouchIn) error {
	wctx := context.WithoutCancel(ctx)
	handles := make([]string, len(ins))
	for i, in := range ins {
		handles[i] = in.touch.Handle
	}
	read, err := igReadHandles(ctx, im.db, handles)
	if err != nil {
		return err
	}
	rows := make([]leadstore.IGTouch, 0, len(ins))
	for _, in := range ins {
		t := in.touch
		keepName, err := im.followAccount(wctx, igsignal.Follow{Handle: t.Handle, FullName: in.name}, read[t.Handle])
		if err != nil {
			return err
		}
		if keepName {
			t.FullName = in.name
		}
		if t.Kind == leadstore.IGTouchComment && im.matches[t.Handle].Strong() {
			t.Text = in.text
		}
		im.sum.Touches[t.Kind]++
		rows = append(rows, t)
	}
	n, err := im.lq.InsertIGTouches(wctx, vendor, at, rec.SHA256, rows)
	im.sum.TouchesNew += int(n)
	return err
}

// followAccount matches and reviews one account of a following list and reports whether its
// name is kept.
func (im *igImporter) followAccount(ctx context.Context, k igsignal.Follow, read bool) (keepName bool, err error) {
	if !read {
		r := im.siteCheck(im.matcher.MatchFollow(k), k.Handle, k.FullName, "")
		if r.VendorAccount != "" {
			im.sum.ByTier[tierWord(r)]++
			return false, nil
		}
		if _, err := im.applyMatch(ctx, k.Handle, r, "", false); err != nil {
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
				kept, err := im.realKept(ctx, p.Handle, true)
				if err != nil {
					return err
				}
				prev, err := im.lq.IGJudgmentHash(ctx, p.Handle, leadstore.IGPlace, true)
				if err != nil {
					return err
				}
				if !kept && !bytes.Equal(prev, leadrate.IGHash(ai.FakeName, *im.cfg, leadrate.IGState(row))) {
					im.sum.WouldAsk++
				}
			}
			if _, _, ok := igsignal.Vendor(*p, im.cfg); ok {
				im.sum.RuleVendors++
			}
			im.sum.ByTier[tierWord(im.siteCheck(im.matcher.Match(*p), p.Handle, p.FullName, p.ExternalURL))]++
		case r.Following != nil:
			im.sum.Lists++
			im.sum.Kept += len(r.Following.Kept)
			for _, k := range r.Following.Kept {
				im.sum.ByTier[tierWord(im.siteCheck(im.matcher.MatchFollow(k), k.Handle, k.FullName, ""))]++
			}
		case r.Followers != nil:
			im.sum.FollowerLists++
			if !r.Followers.Complete {
				im.sum.FollowerListsPartial++
			}
			for _, k := range r.Followers.Kept {
				im.sum.ByTier[tierWord(im.siteCheck(im.matcher.MatchFollow(k), k.Handle, k.FullName, ""))]++
			}
		case r.Comments != nil, r.Tagged != nil:
			var ins []igTouchIn
			if r.Comments != nil {
				ins = igCommentTouches(r.Comments)
			} else {
				ins = igTaggedTouches(r.Tagged)
			}
			for _, in := range ins {
				im.sum.Touches[in.touch.Kind]++
				h := in.touch.Handle
				im.sum.ByTier[tierWord(im.siteCheck(im.matcher.MatchFollow(igsignal.Follow{Handle: h, FullName: in.name}), h, in.name, ""))]++
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
// lead card reads it (IGStandingJudgments).
func igStandingPlaces(ctx context.Context, lq leadstore.Queries, handles []string) (map[string]string, error) {
	js, err := lq.IGStandingJudgments(ctx, handles)
	if err != nil {
		return nil, fmt.Errorf("leads ig: standing places: %w", err)
	}
	out := map[string]string{}
	for h, byQ := range js {
		if j, ok := byQ[leadstore.IGPlace]; ok {
			out[h] = j.Choice
		}
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
	_, _ = fmt.Fprintf(w, "  %s · %d follower rows new · %d touch rows new\n", igVendorReadsLine(s), s.FollowersNew, s.TouchesNew)
	_, _ = fmt.Fprintf(w, "  matches: handle %d · domain %d · name %d · tie %d · none %d · the operator's call kept %d · %d rows written"+
		" · not attached: not coffee %d, no place call yet %d · a site's handle under another name %d\n",
		s.ByTier[leadstore.IGTierHandle], s.ByTier[leadstore.IGTierDomain], s.ByTier[leadstore.IGTierName], s.ByTier["tie"],
		s.ByTier[leadstore.IGTierNone], s.OperatorKept, s.MatchesWritten, s.NotCoffee, s.Held, s.SiteWeak)
	_, _ = fmt.Fprintf(w, "  jev: calls %d · unchanged %d · no text %d · failed %d · still due %d · stop %s · vendor by rule %d"+
		" · real Jev's answers kept from the fake %d\n",
		s.Calls, s.Unchanged, s.NoText, s.Failures, s.Due, cmp.Or(s.StopReason, "none"), s.RuleVendors, s.RealKept)
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
	_, _ = fmt.Fprintf(w, "  routes: %d added · %d taken back\n", s.RoutesAdded, s.RoutesTaken)
}

// igVendorReadsLine counts the vendors' followers lists and touches read (D9).
func igVendorReadsLine(s igSummary) string {
	t := s.Touches
	return fmt.Sprintf("followers lists %d (partial %d), touches %d (comment %d, tag %d, mention %d, tagged %d)",
		s.FollowerLists, s.FollowerListsPartial,
		t[leadstore.IGTouchComment]+t[leadstore.IGTouchUsertag]+t[leadstore.IGTouchMention]+t[leadstore.IGTouchTagged],
		t[leadstore.IGTouchComment], t[leadstore.IGTouchUsertag], t[leadstore.IGTouchMention], t[leadstore.IGTouchTagged])
}

// printIGDryRun prints what a dry run counted.
func printIGDryRun(w io.Writer, slug string, s igSummary, lineErrs []igsignal.LineError) {
	_, _ = fmt.Fprintf(w, "leads ig import --dry-run: campaign %s · %s · %s · %s refused · nothing written, nobody called\n",
		slug, plural(s.Records, "record", "records"), plural(s.Walls, "wall", "walls"), plural(s.Refused, "line", "lines"))
	printIGLineErrors(w, lineErrs)
	_, _ = fmt.Fprintf(w, "  profiles: %d with text · %d Jev would be asked about (fake) · %d missing · %d private · vendor by rule %d\n",
		s.ProfilesWithText, s.WouldAsk, s.Missing, s.Private, s.RuleVendors)
	_, _ = fmt.Fprintf(w, "  following lists: %d · %d accounts kept\n", s.Lists, s.Kept)
	_, _ = fmt.Fprintf(w, "  %s\n", igVendorReadsLine(s))
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

// disagreement is what Instagram's place call says against the lead's effective place type,
// with whose the type is (yours: the operator's call; otherwise "the lead is"), or "" when
// they agree or Instagram cannot tell.
func (l igReportLine) disagreement(yours bool) string {
	is := "the lead is"
	if yours {
		is = "you called it"
	}
	switch igPlaceDisagrees(l.place, l.key.PlaceType) {
	case leadstore.PlaceRoastery:
		return "Instagram reads a roastery or online shop; " + is + " a coffee shop"
	case leadstore.PlaceCafe:
		return "Instagram reads a café with seating; " + is + " a roastery"
	}
	return ""
}

// igPlaceDisagrees is the place type Instagram's place call igPlace points to when it
// disagrees with a lead's effective place type placeType (leadstore.PlaceRoastery or
// leadstore.PlaceCafe), or "" when they agree or Instagram cannot tell. The report and the
// lead page's Instagram card read it; neither writes. Once synced, Instagram's real read is
// the lead's type unless the operator's call outranks it, so a disagreement is then his call
// against Instagram's (or the fake decider's answer, which never sets a type).
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
// matched leads, prints one line per strong match, then the lists the operator acts on: the
// leads released by Instagram (its café read overrides a roastery read from Jev or the name:
// the one road a roastery could be pitched, so first), every other lead whose type Instagram
// set, the leads to correct (Instagram disagrees with his own call, which stands), the leads
// whose read set no type (the fake decider's, or not synced yet) and the switch pitches (a
// café already running a scheme). A lead where Instagram and Jev's website read disagree says so.
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
	ids := make([]int64, len(lines))
	for i, l := range lines {
		ids[i] = l.key.LeadID
	}
	types, err := lq.PlaceTypes(ctx, ids)
	if err != nil {
		return err
	}
	under, err := igUnderReads(ctx, db, ids)
	if err != nil {
		return err
	}
	judged := map[string]map[string]int{leadstore.IGPlace: {}, leadstore.IGLoyalty: {}, leadstore.IGVendor: {}}
	for _, l := range lines {
		for q, c := range map[string]string{leadstore.IGPlace: l.place, leadstore.IGLoyalty: l.loyal, leadstore.IGVendor: l.vendor} {
			judged[q][cmp.Or(c, "not judged")]++
		}
	}
	w := stdout
	_, _ = fmt.Fprintf(w, "leads ig report: %s, %s with a strong Instagram match\n", label, plural(len(lines), "café lead", "café leads"))
	_, _ = fmt.Fprintf(w, "  latest match per handle: handle %d · domain %d · name %d · tie %d · yours %d · unmatched by you %d · none %d"+
		" · not coffee, not attached %d\n",
		tiers[leadstore.IGTierHandle], tiers[leadstore.IGTierDomain], tiers[leadstore.IGTierName], tiers["tie"],
		tiers[leadstore.IGTierOperator], tiers["unmatched"], tiers[leadstore.IGTierNone], tiers["not coffee"])
	for _, q := range []string{leadstore.IGPlace, leadstore.IGLoyalty, leadstore.IGVendor} {
		_, _ = fmt.Fprintf(w, "  %s on matched leads: %s\n", strings.TrimPrefix(q, "ig_"), countWords(judged[q]))
	}
	_, _ = fmt.Fprintf(w, "  review: new %d · dismissed %d · matched %d · wanted %d\n", statuses[leadstore.IGReviewNew],
		statuses[leadstore.IGReviewDismissed], statuses[leadstore.IGReviewMatched], statuses[leadstore.IGReviewWanted])
	for _, l := range lines {
		now := cmp.Or(l.key.PlaceType, "-")
		if src := types[l.key.LeadID].Source; src != "" {
			now += " (" + src + ")"
		}
		_, _ = fmt.Fprintf(w, "  %s · now %s · IG place %s · loyalty %s · vendor %s\n", l.head(), now,
			cmp.Or(l.place, "-"), cmp.Or(l.loyal, "-"), cmp.Or(l.vendor, "-"))
	}
	var released, set, fix, unapplied, pitch []string
	for _, l := range lines {
		pt, below := types[l.key.LeadID], under[l.key.LeadID]
		switch {
		case pt.Source == leadstore.PlaceFromInstagram && pt.Type == leadstore.PlaceCafe && below.Type == leadstore.PlaceRoastery:
			released = append(released, fmt.Sprintf("    %s · café from Instagram, overriding a roastery %s", l.head(), igPlaceSourceWords(below.Source)))
		case pt.Source == leadstore.PlaceFromInstagram:
			set = append(set, fmt.Sprintf("    %s · %s%s", l.head(), igPlaceTypeWord(pt.Type), igJevSays(l.key.JevPlaceType, pt.Type)))
		case pt.Source == leadstore.PlaceFromOperator:
			if d := l.disagreement(true); d != "" {
				fix = append(fix, fmt.Sprintf("    %s · %s", l.head(), d))
			}
		default:
			if d := l.disagreement(false); d != "" {
				unapplied = append(unapplied, fmt.Sprintf("    %s · %s", l.head(), d))
			}
		}
		if l.switchPitch() {
			pitch = append(pitch, fmt.Sprintf("    %s · loyalty %s · vendor %s", l.head(), cmp.Or(l.loyal, "-"), cmp.Or(l.vendor, "-")))
		}
	}
	for _, list := range []struct {
		head  string
		lines []string
	}{
		{fmt.Sprintf("released by Instagram: %d (its café read overrides a roastery read: these get café outreach)", len(released)), released},
		{fmt.Sprintf(`set by Instagram: %d (wrong? "It's a coffee shop" / "It's a roastery" on the lead's page)`, len(set)), set},
		{fmt.Sprintf("to correct: %d (Instagram disagrees with your call; nothing was changed)", len(fix)), fix},
		{fmt.Sprintf("not applied: %d (Instagram's read sets no type: the fake decider's answer, or not synced yet: run leads ig place-types)",
			len(unapplied)), unapplied},
		{fmt.Sprintf("switch pitch: %d (already running a loyalty scheme)", len(pitch)), pitch},
	} {
		_, _ = fmt.Fprintf(w, "  %s\n", list.head)
		for _, s := range list.lines {
			_, _ = fmt.Fprintln(w, s)
		}
	}
	return nil
}

// igJevSays marks a lead Instagram typed placeType where Jev's website read (jevType, its latest
// row) says the other type: "" when Jev agrees, was never asked or could not tell.
func igJevSays(jevType, placeType string) string {
	if jevType == "" || jevType == leadstore.PlaceUnclear || jevType == placeType {
		return ""
	}
	return " · Jev's website read says " + igPlaceTypeWord(jevType)
}

// igUnderReads is, for each of ids, the place type the sources under Instagram give: Jev's
// website read when its latest row decides (it outranks the name), else the name's latest row,
// as placeTypeWinnerSQL ranks them; a lead neither decides is not in the map. It is what
// Instagram's read overrides, for the report's released list.
func igUnderReads(ctx context.Context, db store.DBTX, ids []int64) (map[int64]leadstore.PlaceTypeRow, error) {
	rows, err := db.Query(ctx,
		`SELECT DISTINCT ON (w.lead_id) w.lead_id, w.place_type, w.source, w.detail FROM (
		     SELECT DISTINCT ON (t.lead_id, t.source) t.lead_id, t.place_type, t.source, t.detail
		     FROM lead_place_type t WHERE t.lead_id = ANY($1::bigint[]) AND t.source = ANY($2::text[])
		     ORDER BY t.lead_id, t.source, t.id DESC
		   ) w
		 WHERE w.place_type <> $3
		 ORDER BY w.lead_id, (w.source = $4) DESC`,
		ids, []string{leadstore.PlaceFromJev, leadstore.PlaceFromName}, leadstore.PlaceUnclear, leadstore.PlaceFromJev)
	if err != nil {
		return nil, fmt.Errorf("leads ig report: place reads: %w", err)
	}
	reads, err := pgx.CollectRows(rows, pgx.RowToStructByName[leadstore.PlaceTypeRow])
	if err != nil {
		return nil, fmt.Errorf("leads ig report: place reads: %w", err)
	}
	out := make(map[int64]leadstore.PlaceTypeRow, len(reads))
	for _, r := range reads {
		out[r.LeadID] = r
	}
	return out, nil
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

// igLatestTiers counts every handle's latest match by tier (a tie, the operator's match, his
// unmatch and a strong match held back as not coffee counted apart) and lists the leads a
// latest strong match names.
func igLatestTiers(ctx context.Context, db store.DBTX) (map[string]int, []int64, error) {
	rows, err := db.Query(ctx,
		`SELECT m.tier, coalesce(m.lead_id, 0), cardinality(m.candidate_ids), m.detail
		 FROM (SELECT DISTINCT ON (handle) tier, lead_id, candidate_ids, detail FROM ig_match ORDER BY handle, id DESC) m`)
	if err != nil {
		return nil, nil, fmt.Errorf("leads ig report: matches: %w", err)
	}
	tiers := map[string]int{}
	var leads []int64
	var tier, detail string
	var lead int64
	var cands int
	if _, err := pgx.ForEachRow(rows, []any{&tier, &lead, &cands, &detail}, func() error {
		m := leadstore.IGMatch{Tier: tier, LeadID: lead}
		switch {
		case cands > 0:
			tiers["tie"]++
		case tier == leadstore.IGTierName && strings.HasSuffix(detail, igNotCoffeeWords):
			tiers["not coffee"]++
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
	lq, done, err := igOpen(ctx)
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
		if it.Note != "" {
			line += " · " + it.Note
		}
		if *status == "all" {
			line += " · " + it.Status
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	return nil
}

// igOpen opens the database for the review list and the operator's calls.
func igOpen(ctx context.Context) (leadstore.Queries, func(), error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return leadstore.Queries{}, nil, err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return leadstore.Queries{}, nil, err
	}
	return leadstore.NewQueries(s.Pool()), s.Close, nil
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
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	err = igOperatorPrint(ctx, leadstore.NewQueries(s.Pool()), s.Pool(), setup.leads, verb, args[0], leadID)
	var r igRefusal
	if errors.As(err, &r) {
		return usageError("%s", r)
	}
	return err
}

// igOperatorPrint runs one operator's call and prints what it did.
func igOperatorPrint(ctx context.Context, lq leadstore.Queries, db *pgxpool.Pool, cfg leadconfig.Config, verb, raw string, leadID int64) error {
	switch verb {
	case "match":
		handle, added, sync, err := igOperatorMatch(ctx, lq, db, cfg, raw, leadID)
		if err != nil {
			return err
		}
		route := "its org already holds an Instagram route"
		if added {
			route = "Instagram route added"
		}
		_, _ = fmt.Fprintf(stdout, "leads ig match: @%s is lead %d now (%s)\n", handle, leadID, route)
		printIGPlaceChanges(sync)
		return igOperatorIntent(ctx, lq, cfg)
	case "unmatch":
		handle, back, sync, err := igOperatorUnmatch(ctx, lq, db, cfg, raw)
		if err != nil {
			return err
		}
		tail := ""
		if back {
			tail = "; back on the review list"
		}
		_, _ = fmt.Fprintf(stdout, "leads ig unmatch: @%s matches no lead%s\n", handle, tail)
		printIGPlaceChanges(sync)
		return igOperatorIntent(ctx, lq, cfg)
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

// igOperatorIntent is the loyalty intent following an operator's match or unmatch, after the
// place types it moved: every café campaign (scope 0), as the handle may leave one lead for
// another's. The call itself stands when the sync fails, and the line says how to catch up.
func igOperatorIntent(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config) error {
	isync, err := syncIntent(ctx, lq, cfg, 0, false)
	if err != nil {
		_, _ = fmt.Fprintln(stdout, intentCatchUp)
		return err
	}
	_, _ = fmt.Fprintf(stdout, "  %s\n", intentSyncLine(isync, false))
	return nil
}

// printIGPlaceChanges prints each lead an operator's call moved, one line each, the released
// first.
func printIGPlaceChanges(sync leadstore.IGPlaceSync) {
	for _, c := range igPlaceChanges(sync) {
		_, _ = fmt.Fprintf(stdout, "  %s\n", igPlaceChangeWords(c))
	}
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
// outranks every later import's match), the handle is marked matched, the lead's org gets the
// handle as its instagram route when it holds none (a retired one of that handle is claimed back
// as his), and a route an import wrote for the handle on another org (the lead it matched before)
// is taken back. Then the place types follow the handle (SyncIGPlaceTypes): the lead takes
// Instagram's read of it, and the lead it leaves falls back to its next source; sync says which
// moved. It is `leads ig match` and the console's button alike.
func igOperatorMatch(ctx context.Context, lq leadstore.Queries, db *pgxpool.Pool, cfg leadconfig.Config, raw string,
	leadID int64) (handle string, routeAdded bool, sync leadstore.IGPlaceSync, err error) {
	if handle, err = igOperatorHandle(ctx, db, raw); err != nil {
		return "", false, sync, err
	}
	keys, err := lq.IGLeadKeys(ctx, 0)
	if err != nil {
		return "", false, sync, err
	}
	i := slices.IndexFunc(keys, func(k leadstore.IGLeadKey) bool { return k.LeadID == leadID })
	if i < 0 {
		return "", false, sync, igRefusal(fmt.Sprintf("lead %d is not a café lead Instagram can be matched to", leadID))
	}
	if _, err := lq.SetIGMatch(ctx, leadstore.IGMatch{Handle: handle, LeadID: leadID, Tier: leadstore.IGTierOperator,
		Detail: "your call", Actor: leadstore.ActorGremy}); err != nil {
		return "", false, sync, err
	}
	if _, err := lq.SetIGReviewStatus(ctx, handle, leadstore.IGReviewMatched, "", leadstore.ActorGremy); err != nil {
		return "", false, sync, err
	}
	org := keys[i].OrgID
	if _, err := igTakeBack(ctx, db, cfg, handle, org); err != nil {
		return "", false, sync, err
	}
	if routeAdded, err = igEnsureRoute(ctx, lq, org, handle); err != nil {
		return "", false, sync, err
	}
	if !routeAdded {
		if routeAdded, err = igClaimRetiredRoute(ctx, db, cfg, org, handle); err != nil {
			return "", false, sync, err
		}
	}
	if sync, err = lq.SyncIGPlaceTypes(ctx, false); err != nil {
		return "", false, sync, fmt.Errorf("@%s is matched to lead %d, but its place type is not synced yet (run leads ig place-types): %w", handle, leadID, err)
	}
	return handle, routeAdded, sync, nil
}

// igClaimRetiredRoute claims org orgID's retired instagram route of handle back as the
// operator's (a contact_mark claim, as `leads contacts` claims one), when the org holds no live
// instagram route: his "it's this lead" undoes a take-back. It re-picks the org's new leads and
// reports whether it claimed one.
func igClaimRetiredRoute(ctx context.Context, pool *pgxpool.Pool, cfg leadconfig.Config, orgID int64, handle string) (bool, error) {
	if leadstore.IsVendorIGHandle(handle) {
		return false, nil
	}
	lq := leadstore.NewQueries(pool)
	routes, err := lq.RoutesFor(ctx, orgID)
	if err != nil || slices.ContainsFunc(routes, func(r leadstore.Route) bool { return r.Kind == leadstore.RouteInstagram }) {
		return false, err
	}
	var id int64
	err = pool.QueryRow(ctx, `SELECT id FROM contact_route WHERE org_id = $1 AND kind = $2 AND value = $3`,
		orgID, leadstore.RouteInstagram, enrich.InstagramURL(handle)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("leads ig: retired route: %w", err)
	}
	err = contactTx(ctx, pool, cfg, orgID, func(tq leadstore.Queries) error {
		return tq.MarkContact(ctx, leadstore.Mark{OrgID: orgID, RouteID: id, Mark: leadstore.MarkClaim, Note: "your Instagram match"})
	})
	return err == nil, err
}

// igOperatorUnmatch is the operator's "it is no lead": an operator's match with no lead, so no
// import attaches it again; every route an import wrote for the handle is taken back
// (igTakeBack); a handle marked matched goes back on the review list (back). Then the place
// types follow (SyncIGPlaceTypes): the lead the handle typed falls back to its next source,
// and sync says which moved.
func igOperatorUnmatch(ctx context.Context, lq leadstore.Queries, db *pgxpool.Pool, cfg leadconfig.Config,
	raw string) (handle string, back bool, sync leadstore.IGPlaceSync, err error) {
	if handle, err = igOperatorHandle(ctx, db, raw); err != nil {
		return "", false, sync, err
	}
	if _, err := lq.SetIGMatch(ctx, leadstore.IGMatch{Handle: handle, Tier: leadstore.IGTierOperator,
		Detail: "unmatched by you", Actor: leadstore.ActorGremy}); err != nil {
		return "", false, sync, err
	}
	if _, err := igTakeBack(ctx, db, cfg, handle, 0); err != nil {
		return "", false, sync, err
	}
	items, err := lq.IGReview(ctx, leadstore.IGReviewMatched)
	if err != nil {
		return "", false, sync, err
	}
	if slices.ContainsFunc(items, func(it leadstore.IGReviewItem) bool { return it.Handle == handle }) {
		if _, err := lq.SetIGReviewStatus(ctx, handle, leadstore.IGReviewNew, "unmatched", leadstore.ActorGremy); err != nil {
			return "", false, sync, err
		}
		back = true
	}
	if sync, err = lq.SyncIGPlaceTypes(ctx, false); err != nil {
		return "", false, sync, fmt.Errorf("@%s is unmatched, but place types are not synced yet (run leads ig place-types): %w", handle, err)
	}
	return handle, back, sync, nil
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
