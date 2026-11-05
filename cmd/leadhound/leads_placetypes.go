package main

import (
	"context"
	"flag"
	"fmt"
	"slices"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func init() { leadsCommands["place-types"] = runLeadsPlaceTypes }

const placeTypesUsage = "usage: leadhound leads place-types report [--campaign SLUG] [--sample N] | apply [--campaign SLUG]"

// defaultPlaceTypeSample is how many matched names report prints when --sample is not given.
const defaultPlaceTypeSample = 20

// runLeadsPlaceTypes is `leads place-types <verb>`: what the configured roastery words find
// in the café leads' names. report writes nothing; apply writes the name rows. They are two
// verbs because make remote's free list matches a command's first words.
func runLeadsPlaceTypes(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("%s", placeTypesUsage)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "report":
		return placeTypesReport(ctx, rest)
	case "apply":
		return placeTypesApply(ctx, rest)
	}
	return usageError("unknown place-types verb %q; %s", verb, placeTypesUsage)
}

// placeNameMatch is the one check of a lead's names for a roastery word, for the report, apply
// and the café build: the name that matched and its word, and whether either did. The map
// listing's name goes first because a website's own title is often generic.
func placeNameMatch(l leadstore.CafeLeadName) (name, word string, ok bool) {
	for _, name := range []string{l.OrgName, l.SiteName} {
		if word, ok := cafe.RoasteryWord(name); ok {
			return name, word, true
		}
	}
	return "", "", false
}

// placeNameDue is the roastery word lead l's names carry, and whether that word still needs
// its name row: a word the lead already has a row from is never written again.
func placeNameDue(l leadstore.CafeLeadName) (string, bool) {
	_, word, ok := placeNameMatch(l)
	return word, ok && !slices.Contains(l.NameWords, word)
}

// tagPlaceNames writes the name row each of leads is due and returns how many it wrote. It is
// the one writer of name rows, for apply and for the café build. A name word only ever says
// roastery, so a name with no word writes nothing, and the row never changes the lead's state
// or call. An operator's or Jev's answer still outranks the row (placeTypeWinnerSQL).
func tagPlaceNames(ctx context.Context, lq leadstore.Queries, leads []leadstore.CafeLeadName) (int, error) {
	written := 0
	for _, l := range leads {
		word, due := placeNameDue(l)
		if !due {
			continue
		}
		if err := lq.SetPlaceType(ctx, l.LeadID, leadstore.PlaceRoastery, leadstore.PlaceFromName, word, nil, leadstore.ActorSystem); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// placeTypesLeads opens the database and reads the café leads of the campaign named slug,
// every campaign's when slug is empty. The label names what was read, for the output.
func placeTypesLeads(ctx context.Context, slug string) (lq leadstore.Queries, leads []leadstore.CafeLeadName, label string, done func(), err error) {
	lq, _, _, done, err = switchesOpen(ctx)
	if err != nil {
		return leadstore.Queries{}, nil, "", nil, err
	}
	var campaignID int64
	label = "all café campaigns"
	if slug != "" {
		c, err := activeCampaign(ctx, lq, slug)
		if err != nil {
			done()
			return leadstore.Queries{}, nil, "", nil, err
		}
		campaignID, label = c.ID, "campaign "+c.Slug
	}
	if leads, err = lq.CafeLeadNames(ctx, campaignID); err != nil {
		done()
		return leadstore.Queries{}, nil, "", nil, err
	}
	return lq, leads, label, done, nil
}

// placeTypesReport is `leads place-types report`: it writes nothing. It counts every café lead
// whatever its state, by its place type now, and looks for the words in the stored names with
// the same check apply uses, so it says what apply would write before apply has run.
func placeTypesReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads place-types report", flag.ContinueOnError)
	slug := fs.String("campaign", "", "only this campaign's leads (default: every café campaign)")
	sample := fs.Int("sample", defaultPlaceTypeSample, "how many matched names to print")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *sample < 0 {
		return usageError("%s", placeTypesUsage)
	}
	_, leads, label, done, err := placeTypesLeads(ctx, *slug)
	if err != nil {
		return err
	}
	defer done()
	var cafes, roasteries, fromName, due int
	var matched []string
	for _, l := range leads {
		switch l.PlaceType {
		case leadstore.PlaceCafe:
			cafes++
		case leadstore.PlaceRoastery:
			roasteries++
			if l.Source == leadstore.PlaceFromName {
				fromName++
			}
		}
		if _, ok := placeNameDue(l); ok {
			due++
		}
		if name, word, ok := placeNameMatch(l); ok {
			matched = append(matched, fmt.Sprintf("  %s (%s)", name, word))
		}
	}
	_, _ = fmt.Fprintf(stdout, "leads place-types report: %s, %s, in any state\n", label, plural(len(leads), "café lead", "café leads"))
	_, _ = fmt.Fprintf(stdout, "  coffee shops: %d\n", cafes)
	_, _ = fmt.Fprintf(stdout, "  roasteries: %d, of them %d from a name match\n", roasteries, fromName)
	_, _ = fmt.Fprintf(stdout, "  names with a roastery word and no name row yet: %d (apply would write these)\n", due)
	_, _ = fmt.Fprintf(stdout, "  names with a roastery word: %d\n", len(matched))
	for _, line := range matched[:min(*sample, len(matched))] {
		_, _ = fmt.Fprintln(stdout, line)
	}
	return nil
}

// placeTypesApply is `leads place-types apply`: it writes the name rows the leads are due. Every
// café lead counts, whatever its state, so a lead that comes back from a skip already has its
// type. A rerun finds each row in place, writes nothing and says so.
func placeTypesApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads place-types apply", flag.ContinueOnError)
	slug := fs.String("campaign", "", "only this campaign's leads (default: every café campaign)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", placeTypesUsage)
	}
	lq, leads, label, done, err := placeTypesLeads(ctx, *slug)
	if err != nil {
		return err
	}
	defer done()
	written, err := tagPlaceNames(ctx, lq, leads)
	if err != nil {
		return fmt.Errorf("place-types apply wrote %d rows, then: %w", written, err)
	}
	if written == 0 {
		_, _ = fmt.Fprintf(stdout, "leads place-types apply: %s: 0 rows written, nothing was due.\n", label)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "leads place-types apply: %s: %s written.\n", label, plural(written, "name row", "name rows"))
	return nil
}
