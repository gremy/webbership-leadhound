package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/boardlist"
	"github.com/gremy/webbership-leadhound/internal/fetch"
)

// ccIndexHost is the Common Crawl URL index (a pywb CDX server).
const ccIndexHost = "index.commoncrawl.org"

// ccIndexBase is where the index is asked; tests point it at a local server.
var ccIndexBase = "https://" + ccIndexHost

// harvestInterval is the pause between two requests to the index, and harvestJitter the
// most random delay added to it; tests shorten both.
var (
	harvestInterval = 2 * time.Second
	harvestJitter   = fetch.DefaultJitter
)

// harvestTimeout is how long one index request may take: a page of a wide prefix is slow.
const harvestTimeout = 90 * time.Second

// harvestMaxBody is the largest index page read.
const harvestMaxBody = 64 << 20

// harvestKind is the fetch kind of an index request: the index is no ATS board, so it
// draws from the site budget.
const harvestKind = fetch.KindSite

// defaultHarvestPatterns are the board hosts asked for when no --pattern is given. The
// SmartRecruiters, Teamtailor and Personio boards are asked for by name (moreHarvestPatterns):
// a domain pattern reads every page of every company on the ATS, so it is the operator's
// call when to spend the pages.
var defaultHarvestPatterns = []string{
	"boards.greenhouse.io/*",
	"job-boards.greenhouse.io/*",
	"jobs.lever.co/*",
	"jobs.eu.lever.co/*",
	"jobs.ashbyhq.com/*",
}

// moreHarvestPatterns are the --pattern values that find SmartRecruiters, Teamtailor and
// Personio boards. Teamtailor and Personio give each company a host of its own, so their
// patterns are domain patterns, *.HOST: every host under HOST.
var moreHarvestPatterns = []string{
	"jobs.smartrecruiters.com/*",
	"*.teamtailor.com",
	"*.jobs.personio.de",
	"*.jobs.personio.com",
}

// crawlRe is a Common Crawl crawl id.
var crawlRe = regexp.MustCompile(`^CC-MAIN-[0-9]{4}-[0-9]{2}$`)

// patternRe is a CDX url pattern without a scheme: a host, then an optional path that may
// end in *.
var patternRe = regexp.MustCompile(`^[A-Za-z0-9.-]+\.[A-Za-z]{2,}(/[A-Za-z0-9._~/-]*\*?)?$`)

// domainPatternRe is a domain pattern: "*." and a host with no path. The index is asked
// for the host and every host under it (matchType=domain).
var domainPatternRe = regexp.MustCompile(`^\*\.([A-Za-z0-9-]+\.)+[A-Za-z]{2,}$`)

// validPattern reports whether p is a pattern --pattern takes.
func validPattern(p string) bool { return patternRe.MatchString(p) || domainPatternRe.MatchString(p) }

const harvestUsage = "usage: leadhound boards harvest --crawl CC-MAIN-YYYY-WW --out FILE [--pattern HOST/* | --pattern '*.HOST']... [--max-pages N] [--force] [--dry-run --fixtures DIR]" +
	"\n  default patterns: boards.greenhouse.io/* job-boards.greenhouse.io/* jobs.lever.co/* jobs.eu.lever.co/* jobs.ashbyhq.com/*" +
	"\n  SmartRecruiters, Teamtailor, Personio: --pattern 'jobs.smartrecruiters.com/*' --pattern '*.teamtailor.com' --pattern '*.jobs.personio.de' --pattern '*.jobs.personio.com'"

// harvestRecorder records nothing: a harvest opens no database, but fetch.Client refuses
// to fetch without a Recorder.
type harvestRecorder struct{}

func (harvestRecorder) Record(context.Context, fetch.Record) error { return nil }

// harvestOpts is what one harvest asks the index for.
type harvestOpts struct {
	crawl    string
	patterns []string
	maxPages int
}

// patternResult is what one pattern gave.
type patternResult struct {
	pattern        string
	pages, ofPages int // pages read, pages the index has
	pageErrors     int
	lines          int
	entries        []boardlist.Entry
	refused        []boardlist.Refused
	skipped        int
}

// runBoardsHarvest is `leadhound boards harvest`: it asks the Common Crawl URL index for
// every captured URL under each board host pattern, maps them to boards and writes a CSV in
// the `boards import` format. It opens no database; the import is the one door into the
// board table. args are the arguments after "harvest".
func runBoardsHarvest(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("boards harvest", flag.ContinueOnError)
	var (
		ff       fetchFlags
		opts     harvestOpts
		patterns stringList
		out      string
		force    bool
	)
	ff.register(fset)
	fset.StringVar(&opts.crawl, "crawl", "", "crawl id, CC-MAIN-YYYY-WW")
	fset.Var(&patterns, "pattern", "CDX url pattern, HOST/* or *.HOST for every host under HOST (repeatable; default: the five Greenhouse, Lever and Ashby board hosts; for the others: "+strings.Join(moreHarvestPatterns, ", ")+")")
	fset.IntVar(&opts.maxPages, "max-pages", 50, "most index pages read per pattern")
	fset.StringVar(&out, "out", "", "CSV file to write (required; never stdout)")
	fset.BoolVar(&force, "force", false, "overwrite --out if it exists")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) != 0:
		return usageError("%s", harvestUsage)
	case !crawlRe.MatchString(opts.crawl):
		return usageError("--crawl must be a crawl id like CC-MAIN-2025-08; %s", harvestUsage)
	case out == "" || out == "-":
		return usageError("--out FILE is required (the harvest never writes to stdout); %s", harvestUsage)
	case opts.maxPages < 1:
		return usageError("--max-pages must be at least 1")
	}
	if err := ff.check(); err != nil {
		return err
	}
	opts.patterns = defaultHarvestPatterns
	if len(patterns) > 0 {
		opts.patterns = nil
		for _, p := range patterns {
			p = strings.TrimSpace(p)
			if !validPattern(p) {
				return usageError("--pattern %q: want HOST/* or *.HOST with no scheme, e.g. jobs.lever.co/* or *.teamtailor.com", p)
			}
			opts.patterns = append(opts.patterns, p)
		}
	}
	if !force {
		if _, err := os.Stat(out); err == nil {
			return usageError("%s exists; pass --force to overwrite it", out)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("boards harvest: %w", err)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	var c *fetch.Client
	if ff.dryRun {
		ft, err := fetch.NewFixtureTransport(ff.fixtures)
		if err != nil {
			return usageError("%v", err)
		}
		c = fetch.New(fetch.WithFixtures(ft), fetch.WithRecorder(harvestRecorder{}), fetch.WithMaxBody(harvestMaxBody),
			fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)), fetch.WithJitter(0))
	} else {
		u, err := url.Parse(ccIndexBase)
		if err != nil {
			return fmt.Errorf("boards harvest: %w", err)
		}
		if host := u.Hostname(); !isLoopbackHost(host) && !cfg.Live.Allows(host) {
			_, _ = fmt.Fprintf(stdout, "boards harvest: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
			return nil
		}
		c = fetch.New(fetch.WithLiveGate(cfg.Live), fetch.WithRecorder(harvestRecorder{}), fetch.WithMaxBody(harvestMaxBody),
			fetch.WithLimiter(rate.NewLimiter(rate.Every(harvestInterval), 1)), fetch.WithJitter(harvestJitter))
		c.HTTP.Timeout = harvestTimeout
	}

	results, herr := harvest(ctx, c, opts)
	if errors.Is(herr, context.Canceled) || errors.Is(herr, context.DeadlineExceeded) {
		return herr
	}
	var all []boardlist.Entry
	byReason := map[string]int{}
	for _, r := range results {
		all = append(all, r.entries...)
		for _, ref := range r.refused {
			byReason[ref.Reason]++
		}
	}
	all = boardlist.Dedupe(all)
	if err := writeHarvest(ctx, out, all, force); err != nil {
		return err
	}
	perATS := map[string]int{}
	for _, e := range all {
		perATS[e.Board.ATS]++
	}
	_, _ = fmt.Fprintf(stdout, "harvest: %d unique boards (%s) written to %s\n", len(all), countList(perATS), out)
	if len(byReason) > 0 {
		_, _ = fmt.Fprintf(stdout, "refused by reason: %s\n", countList(byReason))
	}
	if herr != nil {
		return fmt.Errorf("boards harvest: stopped early, the CSV holds what was read: %w", herr)
	}
	return nil
}

// isLoopbackHost reports whether host is localhost or a loopback address (tests).
func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// harvest reads every pattern in turn and prints one line per pattern. A page that fails
// is reported and skipped. The error is set when the client's breaker opened (every later
// request would be refused) or the context ended; the stopped pattern's line says why, each
// pattern after it gets a "not run" line, and results hold what was read until then.
func harvest(ctx context.Context, c *fetch.Client, o harvestOpts) ([]patternResult, error) {
	var results []patternResult
	for i, p := range o.patterns {
		r, err := harvestPattern(ctx, c, o, p)
		results = append(results, r)
		note := ""
		switch {
		case err != nil:
			note = " (stopped: " + stopReason(err) + ")"
		case r.ofPages > o.maxPages:
			note = fmt.Sprintf(" (capped by --max-pages %d)", o.maxPages)
		}
		_, _ = fmt.Fprintf(stdout, "%s: pages %d/%d%s, page errors %d, lines %d, boards %d, refused %d, skipped %d\n",
			p, r.pages, r.ofPages, note, r.pageErrors, r.lines, len(r.entries), len(r.refused), r.skipped)
		if err != nil {
			for _, rest := range o.patterns[i+1:] {
				_, _ = fmt.Fprintf(stdout, "%s: not run (the run stopped)\n", rest)
			}
			return results, err
		}
	}
	return results, nil
}

// stopReason says in a word or two why a stop error ended the run: the breaker's reason,
// or that the context was cancelled or timed out.
func stopReason(err error) string {
	var open *fetch.OpenError
	switch {
	case errors.As(err, &open):
		return string(open.Reason)
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	default:
		return "cancelled"
	}
}

// harvestPattern asks the index how many pages the pattern has, then reads up to maxPages
// of them.
func harvestPattern(ctx context.Context, c *fetch.Client, o harvestOpts, pattern string) (patternResult, error) {
	r := patternResult{pattern: pattern}
	body, status, err := indexGet(ctx, c, indexURL(o.crawl, pattern, "showNumPages=true"))
	if err != nil {
		if stop := stopError(err); stop != nil {
			return r, stop
		}
		_, _ = fmt.Fprintf(stdout, "%s: page count failed (%s); pattern skipped\n", pattern, shortReasonErr(status, err))
		r.pageErrors++
		return r, nil
	}
	if status == http.StatusNotFound {
		return r, nil // no captures under this pattern
	}
	var n struct {
		Pages int `json:"pages"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &n); err != nil || n.Pages < 0 {
		_, _ = fmt.Fprintf(stdout, "%s: page count unreadable; pattern skipped\n", pattern)
		r.pageErrors++
		return r, nil
	}
	r.ofPages = n.Pages
	pages := min(n.Pages, o.maxPages)
	for page := range pages {
		body, status, err := indexGet(ctx, c, indexURL(o.crawl, pattern, fmt.Sprintf("page=%d", page)))
		if err != nil {
			if stop := stopError(err); stop != nil {
				return r, stop
			}
			_, _ = fmt.Fprintf(stdout, "%s: page %d failed (%s); skipped\n", pattern, page, shortReasonErr(status, err))
			r.pageErrors++
			continue
		}
		r.pages++
		if status == http.StatusNotFound {
			continue
		}
		entries, refused, skipped, err := boardlist.ParseCDX(bytes.NewReader(body))
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "%s: page %d unreadable; skipped\n", pattern, page)
			r.pageErrors++
			r.pages--
			continue
		}
		r.lines += nonBlankLines(body)
		r.entries = boardlist.Dedupe(append(r.entries, entries...))
		r.refused = append(r.refused, refused...)
		r.skipped += skipped
	}
	return r, nil
}

// indexURL is one index query: the pattern, JSON lines with the url field only, plus extra
// (showNumPages=true or page=N). A domain pattern, *.HOST, asks for HOST with
// matchType=domain: HOST and every host under it.
func indexURL(crawl, pattern, extra string) string {
	q := "url=" + url.QueryEscape(pattern)
	if host, ok := strings.CutPrefix(pattern, "*."); ok {
		q = "url=" + url.QueryEscape(host) + "&matchType=domain"
	}
	return ccIndexBase + "/" + crawl + "-index?" + q + "&output=json&fl=url&" + extra
}

// indexGet requests one index URL and returns its body and status. A 404 (no captures) is
// not an error.
func indexGet(ctx context.Context, c *fetch.Client, u string) ([]byte, int, error) {
	resp, err := c.Get(ctx, harvestKind, u)
	if err != nil {
		slog.Warn("index request failed", "url", u, "err", err)
		return nil, resp.Status, err
	}
	return resp.Body, resp.Status, nil
}

// stopError returns err when no further request can go out (the context ended or the
// breaker opened), else nil.
func stopError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, fetch.ErrBreakerOpen) {
		return err
	}
	return nil
}

// shortReasonErr says in a few words why an index request failed.
func shortReasonErr(status int, err error) string {
	return shortReason(fetch.Response{Status: status}, err)
}

// writeHarvest writes the CSV to a temp file beside path and only then puts it in place, so
// a file already at path is never left half-written. Without force the temp file is linked
// to path, which fails if path exists (the O_EXCL rule); with force it is renamed over it.
// If ctx has ended by then, the temp file is removed and path is left as it was.
func writeHarvest(ctx context.Context, path string, entries []boardlist.Entry, force bool) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("boards harvest: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // a no-op once renamed; after a link, path keeps the data
	if err := boardlist.WriteCSV(f, entries); err != nil {
		_ = f.Close()
		return fmt.Errorf("boards harvest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("boards harvest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if force {
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("boards harvest: %w", err)
		}
		return nil
	}
	err = os.Link(tmp, path)
	if errors.Is(err, fs.ErrExist) {
		return usageError("%s exists; pass --force to overwrite it", path)
	}
	if err != nil {
		return fmt.Errorf("boards harvest: %w", err)
	}
	return nil
}

// countList is "a 3, b 1": the counts, largest first, then by name.
func countList(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// nonBlankLines counts the lines of b that hold more than white space.
func nonBlankLines(b []byte) int {
	n := 0
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}
