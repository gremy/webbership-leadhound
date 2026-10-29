package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["sites"] = runLeadsSites }

// The run row of a site probe.
const (
	commandLeadsSitesProbe = "leads-sites-probe"
	sourceSites            = "sites"
	sourceSitesFixture     = "sites-fixture"
)

// defaultProbeMax is how many due sites one probe run takes without --max.
const defaultProbeMax = 1000

const leadsSitesUsage = "usage: leadhound leads sites import majestic|tranco|httparchive FILE | " +
	"leads sites import manual DOMAIN... | leads sites probe [--max N] [--domain D]... [--dry-run --fixtures DIR] | " +
	"leads sites eval --labels FILE"

// runLeadsSites is `leadhound leads sites import|probe|eval ...`.
func runLeadsSites(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsSitesUsage)
	}
	switch args[0] {
	case "import":
		return runSitesImport(ctx, args[1:])
	case "probe":
		return runSitesProbe(ctx, args[1:])
	case "eval":
		return runSitesEval(ctx, args[1:])
	}
	return usageError("unknown sites subcommand %q; %s", args[0], leadsSitesUsage)
}

// runSitesImport is `leads sites import majestic|tranco|httparchive FILE` or `leads sites
// import manual DOMAIN...`. The ranked lists keep the configured TLDs only; every domain is
// reduced to its registrable domain and given its country by TLD.
func runSitesImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads sites import", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	const use = "usage: leadhound leads sites import majestic|tranco|httparchive FILE, or leads sites import manual DOMAIN..."
	if len(pos) < 2 {
		return usageError(use)
	}
	kind := pos[0]
	origin := map[string]string{
		"majestic": leadstore.OriginMajestic, "tranco": leadstore.OriginTranco,
		"httparchive": leadstore.OriginHTTPArchive, "manual": leadstore.OriginManual,
	}[kind]
	if origin == "" || (kind != "manual" && len(pos) != 2) {
		return usageError(use)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	setup, err := leadsEnv(cfg, leadsForProbe)
	if err != nil {
		return err
	}

	var entries []sitelist.Entry
	var tally sitelist.Tally
	if kind == "manual" {
		entries, tally = manualEntries(pos[1:])
	} else {
		entries, tally, err = parseSiteList(kind, pos[1], setup.leads.SiteLists.TLDs)
		if err != nil {
			return err
		}
	}
	rows := make([]leadstore.SiteImport, len(entries))
	for i, e := range entries {
		rows[i] = leadstore.SiteImport{Domain: e.Domain, Country: sitelist.CountryOf(e.Domain)}
		if e.Rank > 0 {
			rank := e.Rank
			rows[i].Rank = &rank
		}
	}

	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	if over, why, err := storageCheck(cfg, s.Q())(ctx); err != nil {
		return err
	} else if over {
		return fmt.Errorf("leads sites import %s: storage budget reached (%s); nothing imported", kind, why)
	}
	t, err := leadstore.NewQueries(s.Pool()).ImportSites(ctx, origin, rows)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("leads sites import %s: added %d · known %d · skipped %d (bad lines)", kind, t.Added, t.Known, tally.Bad)
	if kind == "majestic" || kind == "tranco" {
		line += fmt.Sprintf(" · filtered %d (other TLDs)", tally.Filtered)
	}
	_, _ = fmt.Fprintln(stdout, line)
	return nil
}

// manualEntries reduces each named domain to its registrable domain; one that has none is
// counted as bad.
func manualEntries(names []string) ([]sitelist.Entry, sitelist.Tally) {
	var out []sitelist.Entry
	var t sitelist.Tally
	for _, n := range names {
		d, err := sitelist.Registrable(n)
		if err != nil {
			t.Bad++
			continue
		}
		out = append(out, sitelist.Entry{Domain: d})
	}
	return out, t
}

// parseSiteList reads a ranked list (majestic, tranco: the TLD filter applies) or an HTTP
// Archive export (no filter).
func parseSiteList(kind, path string, tlds []string) ([]sitelist.Entry, sitelist.Tally, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the list file
	if err != nil {
		return nil, sitelist.Tally{}, usageError("%v", err)
	}
	defer func() { _ = f.Close() }()
	keep := sitelist.KeepTLDs(tlds)
	switch kind {
	case "majestic":
		return sitelist.ParseMajestic(f, keep)
	case "tranco":
		return sitelist.ParseTranco(f, keep)
	default:
		return sitelist.ParseHTTPArchive(f)
	}
}

// reservedTLDs are the test and documentation TLDs a dry run is limited to.
var reservedTLDs = []string{"example", "invalid", "test", "localhost"}

func isReserved(domain string) bool {
	d := strings.TrimSuffix(strings.ToLower(domain), ".")
	return slices.Contains(reservedTLDs, d[strings.LastIndexByte(d, '.')+1:])
}

// runSitesProbe is `leads sites probe [--max N] [--domain D]... [--dry-run --fixtures DIR]`:
// it probes the due sites (only the named ones with --domain), at most N of them. A dry run
// reads fixtures and probes only sites on reserved TLDs, so it never marks a real site as
// probed. Any recorded outcome, a stop included, exits 0.
func runSitesProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads sites probe", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	maxSites := fs.Int("max", defaultProbeMax, "most sites to probe in this run")
	var named stringList
	fs.Var(&named, "domain", "probe this site, if it is due (repeatable)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *maxSites < 1 {
		return usageError("usage: leadhound leads sites probe [--max N (at least 1)] [--domain D]... [--dry-run --fixtures DIR]")
	}
	if err := ff.check(); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, n := range named {
		d, err := sitelist.Registrable(n)
		if err != nil {
			return usageError("--domain %q: %v", n, err)
		}
		want[d] = true
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	setup, err := leadsEnv(cfg, leadsForProbe)
	if err != nil {
		return err
	}
	newClient, err := siteClients(cfg, ff, siteLimiter(setup.env))
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "leads sites probe: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	if err != nil {
		return err
	}
	s, _, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	if off, err := switchedOff(ctx, s, leadstore.JobProbe, commandLeadsSitesProbe, sourceSites); off || err != nil {
		return err
	}

	limit := *maxSites
	if len(want) > 0 || ff.dryRun {
		limit = 0 // filtered below, then capped
	}
	reprobe := time.Duration(setup.leads.Probe.ReprobeAfterDays) * 24 * time.Hour
	due, err := leadstore.NewQueries(s.Pool()).DueSites(ctx, limit, reprobe, time.Now())
	if err != nil {
		return err
	}
	due, notDue := pickSites(due, want, ff.dryRun, *maxSites)
	cafeCfg, err := cafeConfig(setup.leads)
	if err != nil {
		return err
	}

	source := sourceSites
	if ff.dryRun {
		source = sourceSitesFixture
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsSitesProbe, source)
	if err != nil {
		return err
	}
	rec := store.NewFetchRecorder(q, runID, source).SizeOnly(fetch.KindSite, fetch.KindRobots)
	sum, runErr := probe.Run(ctx, probe.Deps{NewClient: newClient, Recorder: rec, Save: probe.SaveTx(s.Pool(), runID),
		Stop: storageCheck(cfg, q), Cafe: cafeCfg}, due, probe.Options{Concurrency: setup.env.ProbeConcurrency})
	result := store.RunResult{
		Requests: sum.Requests, Failures: sum.Failures, StopReason: sum.StopReason,
		Note: fmt.Sprintf("%d sites probed, %d Drupal", sum.Sites, sum.Drupal),
	}
	switch {
	case runErr != nil:
		result.Note += "; " + runErr.Error()
	case sum.Cancelled:
		result.Note += "; interrupted"
	case sum.StopReason == probe.StopDiskBudget:
		result.Note += "; stopped storing: " + sum.StopNote
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	printProbeSummary(stdout, runID, ff.dryRun, sum, notDue)
	return ctx.Err()
}

// pickSites keeps the due sites named in want (all when want is empty), only reserved
// TLDs on a dry run, and at most maxSites of them. notDue counts the named sites that are
// unknown or not due.
func pickSites(due []leadstore.Site, want map[string]bool, dryRun bool, maxSites int) (picked []leadstore.Site, notDue int) {
	found := 0
	for _, s := range due {
		if len(want) > 0 && !want[s.Domain] {
			continue
		}
		found++
		if dryRun && !isReserved(s.Domain) {
			continue
		}
		if len(picked) < maxSites {
			picked = append(picked, s)
		}
	}
	if len(want) > 0 {
		notDue = len(want) - found
	}
	return picked, notDue
}

// printProbeSummary prints a probe run decision first: what was found, then what kept
// sites out, then how long it took and why it stopped.
func printProbeSummary(w io.Writer, runID int64, dryRun bool, sum probe.Summary, notDue int) {
	mode := ""
	if dryRun {
		mode = " (dry run, fixtures)"
	}
	_, _ = fmt.Fprintf(w, "leads sites probe: run %d%s · %d sites probed\n", runID, mode, sum.Sites)
	_, _ = fmt.Fprintf(w, "  Drupal found: %d%s\n", sum.Drupal, byMajor(sum))
	_, _ = fmt.Fprintf(w, "  exact versions seen: %s\n", siteCounts(sum.Exact))
	unreachable := sum.Outcomes[leadstore.OutcomeTransportError] - sum.NotPublic
	_, _ = fmt.Fprintf(w, "  bot checks: %d · robots-disallowed: %d · not public: %d\n",
		sum.Outcomes[leadstore.OutcomeBotCheck], sum.Outcomes[leadstore.OutcomeRobotsDisallowed], sum.NotPublic)
	errs := unreachable + sum.Outcomes[leadstore.OutcomeHTTPError] + sum.Outcomes[leadstore.OutcomeNotHTML]
	_, _ = fmt.Fprintf(w, "  errors: %d (unreachable %d · HTTP errors %d · not HTML %d)\n",
		errs, unreachable, sum.Outcomes[leadstore.OutcomeHTTPError], sum.Outcomes[leadstore.OutcomeNotHTML])
	if notDue > 0 {
		_, _ = fmt.Fprintf(w, "  named but unknown or not due: %d\n", notDue)
	}
	if sum.Interrupted > 0 {
		_, _ = fmt.Fprintf(w, "  cut short (still due): %d\n", sum.Interrupted)
	}
	stop := sum.StopReason
	switch {
	case stop == probe.StopFailureRate:
		stop += " (network down: over 80% of the last 100 sites unreachable)"
	case stop == probe.StopDiskBudget:
		stop += " (" + sum.StopNote + ")"
	case sum.Cancelled:
		stop = "interrupted"
	case stop == "":
		stop = "none"
	}
	_, _ = fmt.Fprintf(w, "  %.1f minutes · %d requests · stop %s\n", sum.Elapsed.Minutes(), sum.Requests, stop)
}

// byMajor is " (D7 1 · D10 2 · major unknown 1: 10-or-11 1)", or "" when nothing was found.
func byMajor(sum probe.Summary) string {
	if sum.Drupal == 0 {
		return ""
	}
	majors := make([]int, 0, len(sum.ByMajor))
	for m := range sum.ByMajor {
		if m > 0 {
			majors = append(majors, m)
		}
	}
	slices.Sort(majors)
	parts := make([]string, 0, len(majors)+1)
	for _, m := range majors {
		parts = append(parts, fmt.Sprintf("D%d %d", m, sum.ByMajor[m]))
	}
	if n := sum.ByMajor[0]; n > 0 {
		p := fmt.Sprintf("major unknown %d", n)
		if len(sum.Ambiguous) > 0 {
			p += ": " + siteCounts(sum.Ambiguous)
		}
		parts = append(parts, p)
	}
	return " (" + strings.Join(parts, " · ") + ")"
}

// siteCounts is "a 2 · b 1" in key order, or "none".
func siteCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, " · ")
}

// siteLabel is one hand-checked site from a labels file.
type siteLabel struct {
	Domain    string
	Drupal    bool
	Major     int  // 0: unknown
	Reachable bool // false when the file's final_url is empty
}

// readLabels reads a labels CSV by header name: domain and major are required; final_url,
// when present, marks a row the labelling run could not reach (empty). A major of "" is
// Drupal with the major unknown; "0", "none", "no" or "-" is not Drupal.
func readLabels(r io.Reader) ([]siteLabel, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("labels: read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	di, ok1 := col["domain"]
	mi, ok2 := col["major"]
	if !ok1 || !ok2 {
		return nil, errors.New("labels: the header must name domain and major")
	}
	fi, hasFinal := col["final_url"]
	var out []siteLabel
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("labels: line %d: %w", line, err)
		}
		if di >= len(rec) || mi >= len(rec) {
			return nil, fmt.Errorf("labels: line %d: too few columns", line)
		}
		l := siteLabel{Domain: strings.ToLower(strings.TrimSpace(rec[di])), Drupal: true, Reachable: true}
		if l.Domain == "" {
			continue
		}
		switch m := strings.ToLower(strings.TrimSpace(rec[mi])); m {
		case "":
		case "0", "none", "no", "-":
			l.Drupal = false
		default:
			n, err := strconv.Atoi(m)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("labels: line %d: major %q is not a number", line, rec[mi])
			}
			l.Major = n
		}
		if hasFinal && fi < len(rec) && strings.TrimSpace(rec[fi]) == "" {
			l.Reachable = false
		}
		out = append(out, l)
	}
}

// disagreement is one site where the probe and the label differ.
type disagreement struct {
	Domain, Label, Found, Signals string
}

// evalReport compares labels with the latest probes.
type evalReport struct {
	Labelled      int
	Comparable    int // reachable in both runs
	Agree         int
	MissedDrupal  int
	FalseDrupal   int
	WrongMajor    int
	MajorUnknown  int // labelled with a major the probe could not tell
	Unreachable   int // in either run
	NotProbed     int
	Disagreements []disagreement
}

// evaluate compares each label with its site's latest probe. A site is compared only when
// both runs reached it (the probe's outcome ok). A labelled major the probe could not tell
// is a disagreement (major unknown); a label without a major agrees with any Drupal major.
func evaluate(labels []siteLabel, rows []leadstore.EvalRow) evalReport {
	byDomain := map[string]leadstore.EvalRow{}
	for _, r := range rows {
		byDomain[r.Domain] = r
	}
	rep := evalReport{Labelled: len(labels)}
	for _, l := range labels {
		r, ok := byDomain[l.Domain]
		switch {
		case !ok:
			rep.NotProbed++
			continue
		case !l.Reachable || r.Outcome != leadstore.OutcomeOK:
			rep.Unreachable++
			continue
		}
		rep.Comparable++
		switch {
		case l.Drupal && !r.IsDrupal:
			rep.MissedDrupal++
		case !l.Drupal && r.IsDrupal:
			rep.FalseDrupal++
		case l.Drupal && l.Major > 0 && r.Major > 0 && l.Major != r.Major:
			rep.WrongMajor++
		case l.Drupal && l.Major > 0 && r.Major == 0:
			rep.MajorUnknown++
		default:
			rep.Agree++
			continue
		}
		rep.Disagreements = append(rep.Disagreements, disagreement{
			Domain: l.Domain, Label: labelText(l), Found: foundText(r), Signals: signalText(r.Signals),
		})
	}
	return rep
}

func labelText(l siteLabel) string {
	switch {
	case !l.Drupal:
		return "not Drupal"
	case l.Major == 0:
		return "Drupal (major unknown)"
	}
	return "D" + strconv.Itoa(l.Major)
}

func foundText(r leadstore.EvalRow) string {
	if !r.IsDrupal {
		return "not Drupal"
	}
	s := "Drupal (major unknown)"
	switch {
	case r.Major > 0:
		s = "D" + strconv.Itoa(r.Major)
	case r.Ambiguous != "":
		s = "Drupal " + r.Ambiguous
	}
	if r.ExactVersion != "" {
		s += " " + r.ExactVersion
	}
	return s
}

func signalText(sigs []leadstore.Signal) string {
	if len(sigs) == 0 {
		return "none"
	}
	parts := make([]string, len(sigs))
	for i, s := range sigs {
		parts[i] = s.Signal
		if s.Value != "" {
			parts[i] += ":" + s.Value
		}
	}
	return strings.Join(parts, "|")
}

func printEval(w io.Writer, rep evalReport) {
	pct := 0.0
	if rep.Comparable > 0 {
		pct = 100 * float64(rep.Agree) / float64(rep.Comparable)
	}
	_, _ = fmt.Fprintf(w, "leads sites eval: %d labelled · %d reachable in both runs · agreement %d/%d (%.1f%%)\n",
		rep.Labelled, rep.Comparable, rep.Agree, rep.Comparable, pct)
	_, _ = fmt.Fprintf(w, "  missed Drupal: %d · false Drupal: %d · wrong major: %d · major unknown: %d\n",
		rep.MissedDrupal, rep.FalseDrupal, rep.WrongMajor, rep.MajorUnknown)
	_, _ = fmt.Fprintf(w, "  unreachable in either run: %d · not probed: %d\n", rep.Unreachable, rep.NotProbed)
	if len(rep.Disagreements) == 0 {
		_, _ = fmt.Fprintln(w, "  disagreements: none")
		return
	}
	_, _ = fmt.Fprintln(w, "  disagreements (domain · label · found · signals):")
	for _, d := range rep.Disagreements {
		_, _ = fmt.Fprintf(w, "    %s · %s · %s · %s\n", d.Domain, d.Label, d.Found, d.Signals)
	}
}

// runSitesEval is `leads sites eval --labels FILE`: it compares a hand-checked list with the
// latest stored probe of each labelled site and prints the agreement. It writes nothing.
func runSitesEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads sites eval", flag.ContinueOnError)
	path := fs.String("labels", "", "CSV of hand-checked sites (columns domain and major, by header)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *path == "" {
		return usageError("usage: leadhound leads sites eval --labels FILE")
	}
	f, err := os.Open(*path) //nolint:gosec // G304: the operator names the labels file
	if err != nil {
		return usageError("%v", err)
	}
	labels, err := readLabels(f)
	_ = f.Close()
	if err != nil {
		return usageError("%v", err)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	domains := make([]string, len(labels))
	for i, l := range labels {
		domains[i] = l.Domain
	}
	rows, err := leadstore.NewQueries(s.Pool()).EvalRows(ctx, domains)
	if err != nil {
		return err
	}
	printEval(stdout, evaluate(labels, rows))
	return nil
}
