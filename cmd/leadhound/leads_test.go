package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// leadsTestRecorder counts the records it is given.
type leadsTestRecorder struct{ n int }

func (r *leadsTestRecorder) Record(context.Context, fetch.Record) error { r.n++; return nil }

func TestRunLeadsDispatch(t *testing.T) {
	saved := leadsCommands
	t.Cleanup(func() { leadsCommands = saved })
	var gotArgs []string
	leadsCommands = map[string]command{
		"fake": func(_ context.Context, args []string) error { gotArgs = args; return nil },
		"also": func(context.Context, []string) error { return nil },
	}
	tests := []struct {
		name       string
		args       []string
		want       int
		wantStderr []string
	}{
		{"no sub", []string{"leads"}, 2, []string{"leads <sub>", "also, fake"}},
		{"unknown sub", []string{"leads", "nope"}, 2, []string{`unknown subcommand "nope"`, "also, fake"}},
		{"registered sub", []string{"leads", "fake", "--dry-run", "x"}, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotArgs = nil
			var stderr bytes.Buffer
			if got := run(context.Background(), tt.args, &stderr); got != tt.want {
				t.Fatalf("run(%q) = %d, want %d; stderr %q", tt.args, got, tt.want, stderr.String())
			}
			for _, w := range tt.wantStderr {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), w)
				}
			}
		})
	}
	if want := []string{"--dry-run", "x"}; strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("fake sub got %q, want %q", gotArgs, want)
	}
}

func TestLeadsIsRegistered(t *testing.T) {
	if _, ok := commands["leads"]; !ok {
		t.Fatal(`commands["leads"] is not registered`)
	}
}

func TestSiteClientsLiveNeedsLiveAll(t *testing.T) {
	lim := rate.NewLimiter(rate.Inf, 1)
	for _, live := range []config.Live{config.LiveOff, config.LiveATS} {
		_, err := siteClients(config.Config{Live: live}, fetchFlags{}, lim)
		if !errors.Is(err, errLiveOff) {
			t.Errorf("Live %s: err = %v, want errLiveOff", live, err)
		}
	}
	newClient, err := siteClients(config.Config{Live: config.LiveAll}, fetchFlags{}, lim)
	if err != nil {
		t.Fatalf("LiveAll: %v", err)
	}
	c := newClient(&leadsTestRecorder{})
	if c.Profile.Name != fetch.Site.Name {
		t.Errorf("profile = %q, want %q", c.Profile.Name, fetch.Site.Name)
	}
	if c.HTTP.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", c.HTTP.Timeout)
	}
	if c.MaxBody != drupal.MaxBody {
		t.Errorf("max body = %d, want %d", c.MaxBody, drupal.MaxBody)
	}
	if c.Limiter != lim {
		t.Error("the client does not use the run's limiter")
	}
	// The public-only guard refuses a port other than 80 or 443 before any request.
	if _, err := c.Get(context.Background(), fetch.KindSite, "https://northwind.example:8443/"); !errors.Is(err, fetch.ErrNotPublic) {
		t.Errorf("Get on port 8443: err = %v, want ErrNotPublic", err)
	}
	// Four requests in a chain: the fourth redirect is refused, the third followed.
	req, _ := http.NewRequest(http.MethodGet, "https://northwind.example/", nil)
	if err := c.HTTP.CheckRedirect(req, make([]*http.Request, 3)); err != nil {
		t.Errorf("third redirect: %v, want it followed", err)
	}
	if err := c.HTTP.CheckRedirect(req, make([]*http.Request, 4)); err == nil {
		t.Error("fourth redirect followed, want it refused")
	}
	if other := newClient(&leadsTestRecorder{}); other.Breaker == c.Breaker {
		t.Error("two site clients share a breaker, want one each")
	}
}

func TestSiteClientsDryRun(t *testing.T) {
	dir := t.TempDir()
	writeBoards(t, dir, "manifest.json", `{"https://northwind.example/robots.txt": "robots.txt"}`)
	writeBoards(t, dir, "robots.txt", "User-agent: *\nAllow: /\n")

	// A dry run needs no live setting.
	newClient, err := siteClients(config.Config{}, fetchFlags{dryRun: true, fixtures: dir}, rate.NewLimiter(1, 1))
	if err != nil {
		t.Fatalf("siteClients: %v", err)
	}
	rec := &leadsTestRecorder{}
	c := newClient(rec)
	ctx := context.Background()
	for i := range 4 {
		resp, err := c.Get(ctx, fetch.KindRobots, "https://northwind.example/robots.txt")
		if err != nil || resp.Status != http.StatusOK {
			t.Fatalf("robots %d: status %d, err %v", i+1, resp.Status, err)
		}
	}
	// The robots budget is four per site.
	var open *fetch.OpenError
	if _, err := c.Get(ctx, fetch.KindRobots, "https://northwind.example/robots.txt"); !errors.As(err, &open) || open.Reason != fetch.StopBudget {
		t.Errorf("fifth robots fetch: err = %v, want a budget stop", err)
	}
	if rec.n != 4 {
		t.Errorf("recorded %d, want 4", rec.n)
	}
	// The guard applies to fixture URLs too.
	if _, err := newClient(rec).Get(ctx, fetch.KindSite, "http://127.0.0.1/"); !errors.Is(err, fetch.ErrNotPublic) {
		t.Errorf("fixture Get of a loopback address: err = %v, want ErrNotPublic", err)
	}

	if _, err := siteClients(config.Config{}, fetchFlags{dryRun: true, fixtures: filepath.Join(dir, "none")}, nil); !errors.Is(err, errUsage) {
		t.Errorf("missing fixtures: err = %v, want a usage error", err)
	}
}

func TestLeadsEnv(t *testing.T) {
	for _, k := range []string{"LEADHOUND_PROBE_RATE", "LEADHOUND_PROBE_CONCURRENCY", "LEADHOUND_OUTREACH"} {
		t.Setenv(k, "")
	}
	if _, err := leadsEnv(config.Config{}, leadsForProbe); !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "LEADHOUND_DATA_DIR") {
		t.Errorf("no data dir: err = %v, want a usage error naming LEADHOUND_DATA_DIR", err)
	}
	dir := t.TempDir()
	if _, err := leadsEnv(config.Config{DataDir: dir}, leadsForProbe); !errors.Is(err, errUsage) {
		t.Errorf("no leads.yaml: err = %v, want a usage error", err)
	}

	// The demo file without its templates: enough for the probe side, not for outreach.
	demo, err := os.ReadFile(filepath.Join(examplesDir, "leads.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeBoards(t, dir, "leads.yaml", string(demo))
	le, err := leadsEnv(config.Config{DataDir: dir}, leadsForProbe)
	if err != nil {
		t.Fatalf("probe side: %v", err)
	}
	if le.dataDir != dir || len(le.leads.SiteLists.TLDs) == 0 || le.env.ProbeRate != leadconfig.DefaultProbeRate {
		t.Errorf("probe side = %+v, want the data dir, the demo policy and the default env", le)
	}
	if _, err := leadsEnv(config.Config{DataDir: dir}, leadsForOutreach); !errors.Is(err, errUsage) {
		t.Errorf("outreach side without templates: err = %v, want a usage error", err)
	}

	// The whole demo, templates included, passes the outreach side.
	full := filepath.Join(t.TempDir(), "data")
	if err := os.CopyFS(full, os.DirFS(examplesDir)); err != nil {
		t.Fatal(err)
	}
	le, err = leadsEnv(config.Config{DataDir: full}, leadsForOutreach)
	if err != nil {
		t.Fatalf("outreach side: %v", err)
	}
	if le.dataDir != full || len(le.leads.Templates.Sets) == 0 {
		t.Errorf("outreach side = %+v, want the data dir and the demo templates", le)
	}

	t.Setenv("LEADHOUND_PROBE_RATE", "0")
	if _, err := leadsEnv(config.Config{DataDir: dir}, leadsForProbe); !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "LEADHOUND_PROBE_RATE") {
		t.Errorf("bad probe rate: err = %v, want a usage error naming it", err)
	}
}

func TestSiteLimiter(t *testing.T) {
	lim := siteLimiter(leadconfig.Env{ProbeRate: 12, ProbeConcurrency: 16})
	if lim.Limit() != 12 || lim.Burst() != 16 {
		t.Errorf("limiter = %v/%d, want 12/16", lim.Limit(), lim.Burst())
	}
}
