package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
)

func init() { leadsCommands["mailboxes"] = runLeadsMailboxes }

const mailboxesUsage = "usage: leadhound leads mailboxes list | add --address A [--login L] [--smtp H:P] [--imap H:P] [--selector S] [--from-name N] [--preset NAME] (the password on stdin) | " +
	"check ID|--requested | pause ID|--domain D [--reason R] | resume ID|--domain D | retire ID --confirm | keygen --out PATH | pubkey --key PATH"

// mailboxPasswordInput is where add reads the password; tests replace it. The password is
// never taken from the command line, where other users of the machine could read it.
var mailboxPasswordInput = os.Stdin

// runLeadsMailboxes is `leads mailboxes <verb>`: the sending mailboxes from the command
// line, through the same core as the console (mailboxcore.go), so every refusal exits 2
// with the console's words. Editing is console-only. keygen and pubkey need no environment
// and no data dir: install.sh runs them as root before any env file exists.
func runLeadsMailboxes(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("%s", mailboxesUsage)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "keygen":
		return mailboxesKeygen(rest)
	case "pubkey":
		return mailboxesPubkey(rest)
	case "list":
		if len(rest) != 0 {
			return usageError("%s", mailboxesUsage)
		}
		return mailboxesList(ctx)
	case "add":
		return mailboxesAdd(ctx, rest)
	case "check":
		return mailboxesCheck(ctx, rest)
	case "pause", "resume":
		return mailboxesPause(ctx, verb == "pause", rest)
	case "retire":
		return mailboxesRetire(ctx, rest)
	}
	return usageError("unknown mailboxes verb %q; %s", verb, mailboxesUsage)
}

// mailboxCLIError turns a core refusal into a usage error (exit 2) with the same words.
func mailboxCLIError(err error) error {
	if errors.Is(err, console.ErrUserFixable) {
		return usageError("%s", strings.TrimPrefix(err.Error(), console.ErrUserFixable.Error()+": "))
	}
	return err
}

// readMailboxPassword reads the password from f: without echo, twice, from a terminal; one
// line from a pipe.
func readMailboxPassword(f *os.File) (config.Secret, error) {
	pw, err := readNewPassword(f)
	if err != nil {
		return "", err
	}
	s := config.Secret(pw)
	clear(pw)
	return s, nil
}

func mailboxesKeygen(args []string) error {
	fs := flag.NewFlagSet("leads mailboxes keygen", flag.ContinueOnError)
	out := fs.String("out", "", "where to write the new private key (mode 0600; never overwritten)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *out == "" {
		return usageError("usage: leadhound leads mailboxes keygen --out PATH")
	}
	pub, priv, err := secretbox.GenerateKeyPair(nil)
	if err != nil {
		return err
	}
	if err := secretbox.WritePrivateKey(*out, priv); err != nil {
		if errors.Is(err, os.ErrExist) {
			return usageError("%s already exists; a mailbox key is never overwritten", *out)
		}
		return usageError("%v", err)
	}
	_, _ = fmt.Fprintf(stdout, "%s=%s\nFingerprint: %s\n", secretbox.PublicKeyEnv, pub, secretbox.Fingerprint(pub))
	return nil
}

func mailboxesPubkey(args []string) error {
	fs := flag.NewFlagSet("leads mailboxes pubkey", flag.ContinueOnError)
	keyPath := fs.String("key", "", "the private key file")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *keyPath == "" {
		return usageError("usage: leadhound leads mailboxes pubkey --key PATH")
	}
	priv, err := secretbox.ReadPrivateKey(*keyPath, true)
	if err != nil {
		return usageError("%v", err)
	}
	_, _ = fmt.Fprintf(stdout, "%s=%s\n", secretbox.PublicKeyEnv, priv.Public())
	return nil
}

// mailboxesOpen opens the store, with the setup and the operator's zone.
func mailboxesOpen(ctx context.Context) (leadstore.Queries, leadsSetup, *time.Location, func(), error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return leadstore.Queries{}, leadsSetup{}, nil, nil, err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return leadstore.Queries{}, leadsSetup{}, nil, nil, usageError("LEADHOUND_TZ: %v", err)
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return leadstore.Queries{}, leadsSetup{}, nil, nil, err
	}
	return leadstore.NewQueries(s.Pool()), setup, loc, s.Close, nil
}

func mailboxesList(ctx context.Context) error {
	lq, _, loc, done, err := mailboxesOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	ms, err := lq.Mailboxes(ctx, startOfDay(leadsNow(), loc))
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		_, _ = fmt.Fprintln(stdout, "No mailbox yet: add one with leadhound leads mailboxes add --address A, the password on stdin.")
		return nil
	}
	for _, m := range ms {
		state := m.State
		if m.State == leadstore.MailboxPaused {
			state = "paused"
			if m.Scope == leadstore.ScopeDomain {
				state += " (domain)"
			}
			if m.Reason != "" {
				state += ": " + m.Reason
			}
		}
		check := "never checked"
		if c := m.LastCheck; c != nil {
			check = "check " + strings.ReplaceAll(c.Outcome, "_", " ")
			if c.Fix != "" {
				check += ": " + c.Fix
			}
		}
		dns := "fail or not run"
		if m.DNSPassedToday {
			dns = "pass"
		}
		_, _ = fmt.Fprintf(stdout, "%d  %s · %s · %s · DNS today %s · sent today %d\n", m.ID, m.Address, state, check, dns, m.SentToday)
	}
	return nil
}

func mailboxesAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads mailboxes add", flag.ContinueOnError)
	var in MailboxInput
	fs.StringVar(&in.Address, "address", "", "the mailbox address")
	fs.StringVar(&in.Login, "login", "", "the login (default: the address)")
	fs.StringVar(&in.SMTPAddr, "smtp", "", "the sending server, host:port (465 or 587)")
	fs.StringVar(&in.IMAPAddr, "imap", "", "the reading server, host:port (993)")
	fs.StringVar(&in.Selector, "selector", "", "the DKIM selector")
	fs.StringVar(&in.FromName, "from-name", "", "the From name (default: leads.yaml sender.name)")
	fs.StringVar(&in.Preset, "preset", "", "a provider preset: "+presetNames())
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || strings.TrimSpace(in.Address) == "" {
		return usageError("%s", mailboxesUsage)
	}
	if in.Password, err = readMailboxPassword(mailboxPasswordInput); err != nil {
		return err
	}
	lq, setup, _, done, err := mailboxesOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	pub, err := secretbox.ParsePublicKey(os.Getenv(secretbox.PublicKeyEnv))
	if err != nil && os.Getenv(secretbox.PublicKeyEnv) != "" {
		return usageError("%s: %v", secretbox.PublicKeyEnv, err)
	}
	id, err := addMailbox(ctx, lq, setup.leads, pub, setup.dataDir, leadsNow(), in)
	if err != nil {
		return mailboxCLIError(err)
	}
	m, err := lq.MailboxByID(ctx, id)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Mailbox %d added: %s. Its check is requested; the mailbox-check job runs it, or run leadhound leads mailboxes check %d.\n",
		id, m.Address, id)
	return nil
}

// mailboxID reads a mailbox number.
func mailboxID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, usageError("%q is not a mailbox number; %s", s, mailboxesUsage)
	}
	return id, nil
}

func mailboxesCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads mailboxes check", flag.ContinueOnError)
	requested := fs.Bool("requested", false, "run every requested check (the mailbox-check job)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	switch {
	case *requested && len(pos) == 0:
		return runChecksCommand(ctx, os.Getenv)
	case !*requested && len(pos) == 1:
		id, err := mailboxID(pos[0])
		if err != nil {
			return err
		}
		return checksCommand(ctx, os.Getenv, id)
	}
	return usageError("%s", mailboxesUsage)
}

func mailboxesPause(ctx context.Context, pause bool, args []string) error {
	name := "leads mailboxes resume"
	if pause {
		name = "leads mailboxes pause"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	domain := fs.String("domain", "", "every mailbox on this sending domain")
	var reason *string
	if pause {
		reason = fs.String("reason", "", "why (kept with the pause)")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if (*domain == "") == (len(pos) == 0) || len(pos) > 1 {
		return usageError("%s", mailboxesUsage)
	}
	var id int64
	if len(pos) == 1 {
		if id, err = mailboxID(pos[0]); err != nil {
			return err
		}
	}
	why := ""
	if reason != nil {
		why = *reason
	}
	lq, _, _, done, err := mailboxesOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	now := leadsNow()
	verb := "resumed"
	if pause {
		verb = "paused"
	}
	if *domain != "" {
		sw, err := setDomainPause(ctx, lq, now, *domain, pause, why)
		if err != nil {
			return mailboxCLIError(err)
		}
		_, _ = fmt.Fprintf(stdout, "%d mailboxes on %s are %s.\n", sw.Changed, sw.Domain, verb)
		if sw.Kept > 0 {
			_, _ = fmt.Fprintf(stdout, "%d stay paused on their own: resume each by its number.\n", sw.Kept)
		}
		return nil
	}
	sw, err := setMailboxPause(ctx, lq, now, id, pause, why)
	if err != nil {
		return mailboxCLIError(err)
	}
	switch {
	case pause && !sw.Changed:
		_, _ = fmt.Fprintf(stdout, "Mailbox %d is already paused.\n", id)
	case !pause && !sw.Changed:
		_, _ = fmt.Fprintf(stdout, "Mailbox %d is not paused.\n", id)
	default:
		_, _ = fmt.Fprintf(stdout, "Mailbox %d is %s.\n", id, verb)
	}
	return nil
}

func mailboxesRetire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads mailboxes retire", flag.ContinueOnError)
	confirm := fs.Bool("confirm", false, "retire it for good")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("%s", mailboxesUsage)
	}
	id, err := mailboxID(pos[0])
	if err != nil {
		return err
	}
	lq, _, _, done, err := mailboxesOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	n, err := retireMailbox(ctx, lq, leadsNow(), id, *confirm)
	if err != nil {
		return mailboxCLIError(err)
	}
	_, _ = fmt.Fprintf(stdout, "Mailbox %d is retired; %d unsent messages cancelled.\n", id, n)
	return nil
}
