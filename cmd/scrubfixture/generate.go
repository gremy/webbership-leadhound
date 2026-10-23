package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// fixtureSpec is one output of the fixture set.
type fixtureSpec struct {
	Out  string
	Kind Kind
	// Want is the language the source must be in; empty means any.
	Want   Lang
	Closed bool
	Broken bool
}

// fixtureSet is the fixture set Task 2.1 writes, in processing order. Each output names
// only its role; which probe page feeds it lives in the private probe directory's
// sources.json, because probe file names carry real posting IDs. The search page
// comes first so its cards claim synthetic jobs 1 to 10 in card order; every detail
// page here is one of those cards, so its synthetic ID matches its card. The Dutch
// fixture shares its source with the simple-apply one, so the two are the same bytes and
// the manifest points that job at detail_simple.html.
var fixtureSet = []fixtureSpec{
	{Out: "search.html", Kind: KindSearch},
	{Out: "detail_offsite.html", Kind: KindDetail, Want: LangEnglish},
	{Out: "detail_onsite.html", Kind: KindDetail},
	{Out: "detail_simple.html", Kind: KindDetail},
	{Out: "detail_recruiter_removed.html", Kind: KindDetail},
	{Out: "detail_closed.html", Kind: KindDetail, Closed: true},
	{Out: "detail_fr.html", Kind: KindDetail, Want: LangFrench},
	{Out: "detail_es.html", Kind: KindDetail, Want: LangSpanish},
	{Out: "detail_nl.html", Kind: KindDetail, Want: LangDutch},
	{Out: "search_broken.html", Kind: KindSearch, Broken: true},
}

// fixtureMode is the mode of written fixtures: they are tracked, readable test data.
const fixtureMode = 0o644

const searchEmpty = "<!-- scrubfixture: a search page with no result cards -->\n"

// Generate scrubs the probe pages in probeDir into the fixture set in outDir and writes
// manifest.json. log receives one line per output, never a value from the raw pages.
func Generate(probeDir, outDir string, log io.Writer) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil { // #nosec G301 -- a tracked testdata directory
		return err
	}
	sources, err := loadSources(probeDir)
	if err != nil {
		return err
	}
	s := NewScrubber(detectPhraseLang)
	// SearchURL puts start last, so the start patterns end the URL: start=10 never
	// matches start=100.
	manifest := map[string]string{
		"*seeMoreJobPostings/search?*&start=0":  "search.html",
		"*seeMoreJobPostings/search?*&start=10": "search_empty.html",
	}
	for _, f := range fixtureSet {
		src := sources[f.Out]
		raw, err := os.ReadFile(filepath.Join(probeDir, src)) // #nosec G304 G703 -- operator-chosen probe directory
		if err != nil {
			return err
		}
		srcLang := ""
		if f.Kind == KindDetail {
			_, nodes, _, perr := parse(raw)
			if perr != nil {
				return perr
			}
			srcLang = detectLanguage(descriptionText(nodes))
			if f.Want != "" && Lang(srcLang) != f.Want {
				return fmt.Errorf("%s: source language is %q, want %q", f.Out, srcLang, f.Want)
			}
		}
		out, err := s.Scrub(raw, Options{Kind: f.Kind, Closed: f.Closed, RenameTitleClass: f.Broken})
		if err != nil {
			return fmt.Errorf("%s: %w", f.Out, err)
		}
		if leaks := Leaks(out, CollectOriginals(raw)); len(leaks) > 0 {
			return fmt.Errorf("%s: scrubbed output still breaks %v", f.Out, leaks)
		}
		g := struct {
			File, SourceLang, OutputLang string
			Job                          int
		}{File: f.Out, SourceLang: srcLang}
		if f.Kind == KindDetail {
			_, nodes, _, _ := parse(out)
			g.OutputLang = detectLanguage(descriptionText(nodes))
			if _, ok := phrases[Lang(srcLang)]; ok && g.OutputLang != srcLang {
				return fmt.Errorf("%s: output language %q differs from source %q", f.Out, g.OutputLang, srcLang)
			}
			g.Job = s.pageJob
			key := "*jobPosting/" + strconv.Itoa(firstFakeID-1+s.pageJob)
			if _, taken := manifest[key]; !taken {
				manifest[key] = f.Out
			}
		}
		if err := os.WriteFile(filepath.Join(outDir, f.Out), out, fixtureMode); err != nil { // #nosec G306 G703 -- tracked test data under the operator-chosen directory
			return err
		}
		_, _ = fmt.Fprintf(log, "%-30s job=%-2d source_lang=%-2s output_lang=%s\n", g.File, g.Job, g.SourceLang, g.OutputLang)
	}
	if err := os.WriteFile(filepath.Join(outDir, "search_empty.html"), []byte(searchEmpty), fixtureMode); err != nil { // #nosec G306 -- tracked test data
		return err
	}
	_, _ = fmt.Fprintf(log, "%-30s (no cards)\n", "search_empty.html")
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), data.Bytes(), fixtureMode); err != nil { // #nosec G306 -- tracked test data
		return err
	}
	_, _ = fmt.Fprintf(log, "%-30s %d entries\n", "manifest.json", len(manifest))
	return nil
}

// sourcesFile maps each output to the probe page it is scrubbed from. It lives in the
// private probe directory, never in the repository.
const sourcesFile = "sources.json"

// loadSources reads probeDir/sources.json and checks it names a plain file for every
// output in fixtureSet.
func loadSources(probeDir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(probeDir, sourcesFile)) // #nosec G304 G703 -- operator-chosen probe directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s missing in the probe directory; it maps each output (e.g. \"detail_offsite.html\") to its probe page", sourcesFile)
	}
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", sourcesFile, err)
	}
	for _, f := range fixtureSet {
		src := m[f.Out]
		if src == "" || filepath.Base(src) != src {
			return nil, fmt.Errorf("%s: no plain probe file name for %s", sourcesFile, f.Out)
		}
	}
	return m, nil
}
