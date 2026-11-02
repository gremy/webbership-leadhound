package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// The operator's own contacts, as `leads contacts add|edit|retire|list` and the lead page's forms
// both run them. Each write and the channel it changes share one transaction, so a lead never
// shows a contact it has not been re-picked for.

const leadsContactsOperatorUsage = "usage: leadhound leads contacts add LEAD --kind email|phone|form|linkedin|person [--value V] [--name N] [--role R] [--note T] [--person ID] | " +
	"edit LEAD CONTACT [--value V] [--name N] [--role R] [--note T] | retire LEAD CONTACT [--note T] | list LEAD"

// operatorContactVerbs are the `leads contacts` words that are not the route search.
var operatorContactVerbs = map[string]command{
	"add":    runContactsAdd,
	"edit":   runContactsEdit,
	"retire": runContactsRetire,
	"list":   runContactsList,
}

// contactTx runs fn on one transaction and re-picks the channel of every new lead of org orgID in
// it: a contact belongs to the org, so every lead of the org changes list together.
func contactTx(ctx context.Context, pool *pgxpool.Pool, cfg leadconfig.Config, orgID int64, fn func(leadstore.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("leads: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	tq := leadstore.NewQueries(tx)
	if err := fn(tq); err != nil {
		return err
	}
	if err := repickOrgLeads(ctx, tx, tq, cfg, orgID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("leads: commit: %w", err)
	}
	return nil
}

// repickOrgLeads gives every new lead of the org the channel a build would give it now: the
// country's policy over the routes still in use. It reads the leads after the write, because
// retiring a route can send a batched lead back to new.
func repickOrgLeads(ctx context.Context, tx pgx.Tx, tq leadstore.Queries, cfg leadconfig.Config, orgID int64) error {
	rows, err := tx.Query(ctx, `SELECT id FROM lead WHERE org_id = $1 AND state = 'new' ORDER BY id`, orgID)
	if err != nil {
		return fmt.Errorf("leads: re-pick: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("leads: re-pick: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	org, err := tq.OrgByID(ctx, orgID)
	if err != nil {
		return err
	}
	routes, err := tq.RoutesFor(ctx, orgID)
	if err != nil {
		return err
	}
	channel, route := outreach.PickChannel(cfg.Policy(org.Country).Channel, channelRoutes(routes))
	for _, id := range ids {
		if route == nil {
			_, err = tq.ClearNewLeadRoute(ctx, id)
		} else {
			_, err = tq.SetNewLeadRoute(ctx, id, channel, *route)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// contactErr turns a store error from a contact write into the console's: a contact or lead that
// is not there is console.ErrNotFound, a value the store refused a refusal in its own words.
func contactErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, leadstore.ErrContactNotFound), errors.Is(err, leadstore.ErrLeadNotFound), errors.Is(err, leadstore.ErrOrgNotFound):
		return console.ErrNotFound
	case errors.Is(err, leadstore.ErrBadContact):
		return storeRefusal(err, leadstore.ErrBadContact)
	case errors.Is(err, leadstore.ErrUnknownRouteKind):
		return refused("Choose email, phone, contact form, LinkedIn or person. Nothing was saved.")
	}
	return err
}

// storeRefusal is a store sentence wrapped with sentinel as a refusal: what the store said after
// the sentinel, capitalised, saying nothing was saved.
func storeRefusal(err, sentinel error) error {
	words := err.Error()
	if i := strings.LastIndex(words, sentinel.Error()+": "); i >= 0 {
		words = words[i+len(sentinel.Error())+2:]
	}
	return refused("%s. Nothing was saved.", capitalised(words))
}

func capitalised(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// leadOrgID is the org of lead id, console.ErrNotFound when there is no such lead.
func leadOrgID(ctx context.Context, lq leadstore.Queries, id int64) (int64, error) {
	l, err := lq.LeadByID(ctx, id)
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return 0, console.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return l.OrgID, nil
}

// addOperatorContact is `leads contacts add` and the Add form. It returns the line to show, "" for
// the caller's own.
func addOperatorContact(ctx context.Context, pool *pgxpool.Pool, cfg leadconfig.Config, c console.NewContact) (string, error) {
	orgID, err := leadOrgID(ctx, leadstore.NewQueries(pool), c.LeadID)
	if err != nil {
		return "", err
	}
	var added leadstore.ContactAdded
	line := ""
	err = contactTx(ctx, pool, cfg, orgID, func(tq leadstore.Queries) error {
		var err error
		switch c.Kind {
		case console.ContactPhone:
			added, err = tq.AddOperatorPhone(ctx, leadstore.OperatorPhone{OrgID: orgID, Value: c.Value, Note: c.Note})
		case console.ContactPerson:
			added.Added = true
			added.ID, err = tq.AddPerson(ctx, leadstore.PersonRow{OrgID: orgID, Name: c.Name, Role: c.Role, Note: c.Note})
			if err == nil {
				line, err = addPersonRoute(ctx, tq, orgID, added.ID, c.Value)
			}
		default:
			added, err = tq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: orgID, Kind: c.Kind, Value: c.Value, Note: c.Note, PersonID: c.PersonID})
		}
		return err
	})
	if err != nil {
		return "", contactErr(err)
	}
	return orWords(line, contactAddedWords(added)), nil
}

// addPersonRoute writes value as a route of person personID, in the caller's transaction: a
// LinkedIn address or an email address; a blank value writes nothing. Anything else is refused, so
// the caller's transaction saves nothing. It returns the line to show when the value was already on
// the org: that row keeps the person it was written with (a route's person is fixed at insert).
func addPersonRoute(ctx context.Context, tq leadstore.Queries, orgID, personID int64, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	kind, err := leadstore.PersonRouteKind(value)
	if err != nil {
		return "", err
	}
	added, err := tq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: orgID, Kind: kind, Value: value, PersonID: personID})
	if err != nil {
		return "", err
	}
	if !added.Added {
		return "The person is saved. That address was already here, so it is now marked as yours and is not tied to this person.", nil
	}
	return "", nil
}

// contactAddedWords says what happened when the value was already on the org; "" when a new row
// was written.
func contactAddedWords(a leadstore.ContactAdded) string {
	switch {
	case a.WasRetired:
		return "Back in use; now marked as yours."
	case !a.Added:
		return "Already there; now marked as yours."
	}
	return ""
}

// editOperatorContact is `leads contacts edit` and a contact's Edit form.
func editOperatorContact(ctx context.Context, pool *pgxpool.Pool, cfg leadconfig.Config, c console.ContactChange) (string, error) {
	orgID, err := leadOrgID(ctx, leadstore.NewQueries(pool), c.LeadID)
	if err != nil {
		return "", err
	}
	var res leadstore.ContactAdded
	line := ""
	err = contactTx(ctx, pool, cfg, orgID, func(tq leadstore.Queries) error {
		var err error
		res, err = tq.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: orgID, RouteID: c.Ref.RouteID, FactID: c.Ref.FactID,
			PersonID: c.Ref.PersonID, Value: c.Value, Name: c.Name, Role: c.Role, Note: c.Note})
		if err == nil && c.Ref.PersonID != 0 {
			line, err = addPersonRoute(ctx, tq, orgID, res.ID, c.Value)
		}
		return err
	})
	if err != nil {
		return "", contactErr(err)
	}
	switch {
	case line != "":
		return line, nil
	case res.Replaced:
		return "Saved. The old one stays on the page as retired.", nil
	case res.WasRetired:
		return "Back in use; now marked as yours.", nil
	}
	return "", nil
}

// retireOperatorContact is `leads contacts retire` and a contact's Retire form.
func retireOperatorContact(ctx context.Context, pool *pgxpool.Pool, cfg leadconfig.Config, leadID int64, ref console.ContactRef, note string) error {
	orgID, err := leadOrgID(ctx, leadstore.NewQueries(pool), leadID)
	if err != nil {
		return err
	}
	return contactErr(contactTx(ctx, pool, cfg, orgID, func(tq leadstore.Queries) error {
		return tq.MarkContact(ctx, leadstore.Mark{OrgID: orgID, RouteID: ref.RouteID, FactID: ref.FactID, PersonID: ref.PersonID,
			Mark: leadstore.MarkRetire, Note: note})
	}))
}

// consoleRefRow is a store contact's ref, as the page and the CLI name it.
func consoleRefRow(c leadstore.Contact) console.ContactRef {
	switch c.Kind {
	case leadstore.ContactPhone:
		return console.ContactRef{FactID: c.ID}
	case leadstore.ContactPerson:
		return console.ContactRef{PersonID: c.ID}
	}
	return console.ContactRef{RouteID: c.ID}
}

// --- the CLI ---

// cliAnswer is what a console-shared write's error means to a command: a refusal or a missing row
// is a usage error in plain words.
func cliAnswer(err error) error {
	switch {
	case errors.Is(err, console.ErrUserFixable):
		return usageError("%s", draftRefusalWords(err))
	case errors.Is(err, console.ErrNotFound):
		return usageError("There is no such lead, contact, proposal or proof.")
	}
	return err
}

// parsePositive reads a positional id, naming what it should be when it is not one.
func parsePositive(v, what, usage string) (int64, error) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 1 {
		return 0, usageError("%q is not a %s number; %s", v, what, usage)
	}
	return id, nil
}

// contactFlags are the flags of a contact's text.
type contactFlags struct{ value, name, role, note *string }

func registerContactFlags(fs *flag.FlagSet) contactFlags {
	return contactFlags{
		value: fs.String("value", "", "the address, number or link"),
		name:  fs.String("name", "", "a person's name"),
		role:  fs.String("role", "", "a person's role"),
		note:  fs.String("note", "", "your note"),
	}
}

func runContactsAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads contacts add", flag.ContinueOnError)
	kind := fs.String("kind", "", "email, phone, form, linkedin or person")
	person := fs.Int64("person", 0, "the person it belongs to")
	f := registerContactFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *kind == "" {
		return usageError(leadsContactsOperatorUsage)
	}
	lead, err := parsePositive(pos[0], "lead", leadsContactsOperatorUsage)
	if err != nil {
		return err
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	line, err := addOperatorContact(ctx, s.Pool(), setup.leads, console.NewContact{LeadID: lead, Kind: *kind, Value: *f.value,
		Name: *f.name, Role: *f.role, Note: *f.note, PersonID: *person})
	if err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintln(stdout, orWords(line, "Contact added."))
	return nil
}

func runContactsEdit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads contacts edit", flag.ContinueOnError)
	f := registerContactFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	lead, ref, err := contactArgs(pos)
	if err != nil {
		return err
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	line, err := editOperatorContact(ctx, s.Pool(), setup.leads, console.ContactChange{LeadID: lead, Ref: ref, Value: *f.value,
		Name: *f.name, Role: *f.role, Note: *f.note})
	if err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintln(stdout, orWords(line, "Contact saved."))
	return nil
}

func runContactsRetire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads contacts retire", flag.ContinueOnError)
	note := fs.String("note", "", "why")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	lead, ref, err := contactArgs(pos)
	if err != nil {
		return err
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := retireOperatorContact(ctx, s.Pool(), setup.leads, lead, ref, *note); err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintln(stdout, "Contact retired.")
	return nil
}

func runContactsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads contacts list", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(leadsContactsOperatorUsage)
	}
	lead, err := parsePositive(pos[0], "lead", leadsContactsOperatorUsage)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	orgID, err := leadOrgID(ctx, lq, lead)
	if err != nil {
		return cliAnswer(err)
	}
	contacts, err := lq.ContactsFor(ctx, orgID)
	if err != nil {
		return err
	}
	for _, c := range contacts {
		state := ""
		if c.Retired {
			state = " retired"
		}
		_, _ = fmt.Fprintf(stdout, "%s %s: %s (%s)%s", consoleRefRow(c), c.Kind, c.Value, console.ProvenanceWords(c.Provenance), state)
		if c.Note != "" {
			_, _ = fmt.Fprintf(stdout, " · %s", c.Note)
		}
		_, _ = fmt.Fprintln(stdout)
	}
	return nil
}

// contactArgs reads LEAD CONTACT.
func contactArgs(pos []string) (int64, console.ContactRef, error) {
	if len(pos) != 2 {
		return 0, console.ContactRef{}, usageError(leadsContactsOperatorUsage)
	}
	lead, err := parsePositive(pos[0], "lead", leadsContactsOperatorUsage)
	if err != nil {
		return 0, console.ContactRef{}, err
	}
	ref, ok := console.ParseContactRef(pos[1])
	if !ok {
		return 0, console.ContactRef{}, usageError("%q is not a contact as `leads contacts list` names one; %s", pos[1], leadsContactsOperatorUsage)
	}
	return lead, ref, nil
}

// orWords is line, or def when it is empty.
func orWords(line, def string) string {
	if line == "" {
		return def
	}
	return line
}
