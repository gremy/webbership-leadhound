package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["forget"] = runLeadsForget }

const leadsForgetUsage = "usage: leadhound leads forget org ID --reason TEXT"

// runLeadsForget is `leads forget org ID --reason TEXT` (CP5 erasure): as the owner role
// (LEADHOUND_MIGRATE_DATABASE_URL, like migrate; the app role holds no DELETE on these
// tables), in one transaction, it suppresses the org's domain and numbers (hashed) so it is
// never collected, researched or called again, deletes its phone facts, phone research and
// screenings, blanks its call notes, empties its kept page bodies and deletes its
// description and social facts. It prints counts only, never a number or a page. Run again
// (after a backup restore, say) it finds nothing left and counts zero; its log line keeps
// the org id, so the erasures to re-apply can be listed.
func runLeadsForget(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "org" {
		return usageError(leadsForgetUsage)
	}
	fs := flag.NewFlagSet("leads forget org", flag.ContinueOnError)
	reason := fs.String("reason", "", "why (one line, at most 200 characters, no address)")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(leadsForgetUsage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return usageError("ID must be an org number; %s", leadsForgetUsage)
	}
	cfg, err := loadConfig("LEADHOUND_MIGRATE_DATABASE_URL")
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.MigrateDatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	f, p, err := forgetOrg(ctx, s, id, *reason)
	switch {
	case errors.Is(err, leadstore.ErrBadForgetReason):
		return usageError("--reason must be one line of 1 to %d characters with no @", leadstore.MaxForgetReason)
	case errors.Is(err, leadstore.ErrOrgNotFound):
		return usageError("There is no org %d. Nothing was changed.", id)
	case err != nil:
		return err
	}
	slog.Default().Info("leads forget org", "org", id, "phone_facts", f.PhoneFacts, "screenings", f.Screenings,
		"research", f.PhoneResearch, "notes", f.NotesBlanked, "places", f.PlacesBlanked, "consents", f.Consents, "consents_blanked", f.ConsentsBlanked, "suppressions", f.Suppressions,
		"bodies", p.BodiesBlanked, "other_facts", p.OtherFacts)
	_, _ = fmt.Fprintln(stdout, forgetWords(id, f, p))
	return nil
}

// forgetOrg runs ForgetOrg and ForgetOrgPages for org id in one transaction on s (the
// owner's connection).
func forgetOrg(ctx context.Context, s *store.Store, id int64, reason string) (leadstore.Forgotten, leadstore.ForgottenPages, error) {
	var f leadstore.Forgotten
	var p leadstore.ForgottenPages
	err := leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		if f, err = lq.ForgetOrg(ctx, id, reason); err != nil {
			return err
		}
		p, err = lq.ForgetOrgPages(ctx, id)
		return err
	})
	if err != nil {
		return leadstore.Forgotten{}, leadstore.ForgottenPages{}, err
	}
	return f, p, nil
}

// forgetWords is what forget did, counts only.
func forgetWords(id int64, f leadstore.Forgotten, p leadstore.ForgottenPages) string {
	if f == (leadstore.Forgotten{}) && p == (leadstore.ForgottenPages{}) {
		return fmt.Sprintf("Org %d: nothing left to forget (already erased).", id)
	}
	places := ""
	if f.PlacesBlanked > 0 {
		places = ", " + plural(int(f.PlacesBlanked), "map listing's phones and emails", "map listings' phones and emails") + " emptied"
	}
	if f.Consents > 0 || f.ConsentsBlanked > 0 {
		places += ", " + plural(int(max(f.Consents, f.ConsentsBlanked)), "email consent and its address", "email consents and their addresses") + " erased"
	}
	return fmt.Sprintf("Org %d forgotten: %s, %s, %s, %s blanked, %s emptied, %s deleted%s; %s written (%s moved to suppressed, %s cancelled).",
		id, plural(int(f.PhoneFacts), "phone fact", "phone facts"), plural(int(f.PhoneResearch), "research row", "research rows"),
		plural(int(f.Screenings), "screening", "screenings"), plural(int(f.NotesBlanked), "call note", "call notes"),
		plural(int(p.BodiesBlanked), "page body", "page bodies"), plural(int(p.OtherFacts), "description or social fact", "description or social facts"), places,
		plural(f.Suppressions, "suppression", "suppressions"), plural(f.LeadsSuppressed, "lead", "leads"),
		plural(int(f.Cancelled), "pending message", "pending messages"))
}
