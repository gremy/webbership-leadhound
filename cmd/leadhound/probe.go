package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["probe"] = runProbe }

// commandProbe is run.command for a probe.
const commandProbe = "probe"

// slugRE is what a LinkedIn company slug may hold.
var slugRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// runProbe is `leadhound probe company <slug> --out FILE [--dry-run --fixtures DIR]`: one
// GET of the guest company page, its raw body written to FILE (mode 0600). It writes a run
// row (command probe) and the fetch row, nothing else. Any recorded outcome exits 0.
func runProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	out := fs.String("out", "", "file to write the raw body to (created with mode 0600)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	const use = "usage: leadhound probe company <slug> --out FILE"
	if len(pos) != 2 || pos[0] != "company" || *out == "" {
		return usageError(use)
	}
	slug := pos[1]
	if !slugRE.MatchString(slug) {
		return usageError("%q is not a company slug; %s", slug, use)
	}
	if err := ff.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	// Two: one request and, after a 429, its one retry.
	newClient, source, err := linkedinClients(cfg, ff, fetch.Budgets{fetch.KindCompany: 2})
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "probe company: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	if err != nil {
		return err
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	q := s.Q()
	// Once LinkedIn has throttled or blocked any command today, the probe asks nothing.
	reason, paused, err := q.PausedToday(ctx, source, loc, time.Now())
	if err != nil {
		return err
	}
	if paused {
		id, note, err := linkedin.RecordPause(ctx, q, commandProbe, source, reason)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "probe company: run %d · %s; nothing requested\n", id, note)
		return nil
	}
	runID, err := q.StartRun(ctx, commandProbe, source)
	if err != nil {
		return err
	}
	client := newClient(store.NewFetchRecorder(q, runID, source))
	u := "https://" + linkedinHost + "/company/" + url.PathEscape(slug)
	resp, getErr := client.Get(ctx, fetch.KindCompany, u)

	result := store.RunResult{Requests: client.Breaker.Requests(), Failures: client.Breaker.Failures()}
	var open *fetch.OpenError
	if errors.As(getErr, &open) {
		result.StopReason = string(open.Reason)
	}
	var writeErr error
	if resp.Status != 0 {
		writeErr = writePrivate(*out, resp.Body)
	}
	result.Note = fmt.Sprintf("company page: status %d, %d bytes", resp.Status, len(resp.Body))
	switch {
	case writeErr != nil:
		result.Note += "; not saved: " + writeErr.Error()
	case getErr != nil && resp.Status == 0:
		result.Note += "; " + getErr.Error()
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return err
	}
	if writeErr != nil {
		return writeErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	final := resp.FinalURL
	if final == "" || final == u {
		final = "same as requested"
	}
	stop := result.StopReason
	if stop == "" {
		stop = "none"
	}
	_, _ = fmt.Fprintf(stdout, "probe company: run %d · status %d · %d bytes · final URL %s · stop %s\n",
		runID, resp.Status, len(resp.Body), final, stop)
	if resp.Status != 0 {
		_, _ = fmt.Fprintf(stdout, "  raw body saved to %s (mode 0600)\n", *out)
	}
	return nil
}

// writePrivate writes b to path readable by the owner only, also when path exists.
func writePrivate(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: the operator names the output file
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
