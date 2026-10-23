// Command scrubfixture turns raw LinkedIn guest pages into test fixtures that keep the
// markup and replace every value: text becomes synthetic text in the page's language, job
// IDs become synthetic ten-digit IDs, companies become fictional ones, and recruiter cards,
// images, scripts, JSON blobs and tracking values are removed.
//
// Usage:
//
//	scrubfixture [--kind search|detail|company] IN OUT   scrub one page
//	scrubfixture generate --probe DIR --out DIR          write the whole fixture set
//	scrubfixture detect FILE [FILE]                      print each page's description language
//
// Output is deterministic: the same input gives byte-identical output. The scrubbed page
// is checked before it is written; a page that still holds a recruiter card, a profile
// link, an original job ID or an original company is refused.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "scrubfixture:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage: scrubfixture [--kind search|detail|company] IN OUT | generate --probe DIR --out DIR | detect FILE [FILE]")

func run(args []string, stdout io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "generate":
			fs := flag.NewFlagSet("generate", flag.ContinueOnError)
			probe := fs.String("probe", "", "directory holding the raw probe pages")
			out := fs.String("out", "", "directory the fixtures are written to")
			if err := fs.Parse(args[1:]); err != nil {
				return err
			}
			if *probe == "" || *out == "" {
				return errUsage
			}
			return Generate(*probe, *out, stdout)
		case "detect":
			for _, f := range args[1:] {
				raw, err := os.ReadFile(f) // #nosec G304 G703 -- operator-chosen file
				if err != nil {
					return err
				}
				_, nodes, _, err := parse(raw)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(stdout, "%s %s\n", filepath.Base(f), detectLanguage(descriptionText(nodes)))
			}
			return nil
		}
	}
	fs := flag.NewFlagSet("scrubfixture", flag.ContinueOnError)
	kind := fs.String("kind", string(KindDetail), "page kind: search, detail or company")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errUsage
	}
	k := Kind(*kind)
	if k != KindSearch && k != KindDetail && k != KindCompany {
		return errUsage
	}
	raw, err := os.ReadFile(fs.Arg(0)) // #nosec G304 -- operator-chosen file
	if err != nil {
		return err
	}
	out, err := NewScrubber(detectPhraseLang).Scrub(raw, Options{Kind: k})
	if err != nil {
		return err
	}
	if leaks := Leaks(out, CollectOriginals(raw)); len(leaks) > 0 {
		return fmt.Errorf("scrubbed output still breaks %v; nothing written", leaks)
	}
	return os.WriteFile(fs.Arg(1), out, 0o600) // #nosec G703 -- operator-chosen output path
}
