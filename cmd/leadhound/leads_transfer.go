package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["transfer"] = runLeadsTransfer }

const leadsTransferUsage = "usage: leadhound leads transfer export DIR | import DIR"

// commandTransfer is the run row every imported probe, fetch and attempt hangs from; fetches
// are matched again only among the fetches of runs of this command.
const commandTransfer = "transfer"

// The transfer's files, in the order the import reads them.
const (
	transferManifestName = "manifest.json"
	tfSites              = "sites.jsonl"
	tfFetches            = "fetches.jsonl"
	tfProbes             = "probes.jsonl"
	tfOrgs               = "orgs.jsonl"
	tfRoutes             = "routes.jsonl"
	tfFindings           = "findings.jsonl"
	tfAttempts           = "attempts.jsonl"
	tfLeads              = "leads.jsonl"
	tfEvents             = "events.jsonl"
	tfSuppressions       = "suppressions.jsonl"
)

var transferFiles = []string{tfSites, tfFetches, tfProbes, tfOrgs, tfRoutes, tfFindings, tfAttempts, tfLeads, tfEvents, tfSuppressions}

// transferFormat is the manifest's format number; an import refuses any other.
const transferFormat = 1

const (
	// transferBatchRows is how many rows one import transaction writes.
	transferBatchRows = 5000
	// transferBatchBodyBytes caps the page bodies one fetch batch holds, so a batch of large
	// pages stays inside the import's memory limit.
	transferBatchBodyBytes = 32 << 20
	// transferStatementTimeout bounds each import statement and the export's reads.
	transferStatementTimeout = 5 * time.Minute
	transferExportTimeout    = 30 * time.Minute
)

// transferReservedTLDs are the TLDs whose rows never cross (leadstore.ReservedTLDs); a test
// narrows it so its own fixtures can stand for real rows.
var transferReservedTLDs = leadstore.ReservedTLDs

// transferReservedRE is a Postgres regular expression matching a domain on a reserved TLD.
func transferReservedRE() string {
	return `\.(` + strings.Join(transferReservedTLDs, "|") + `)$`
}

// transferBatchedReason is the history line a lead left batched on the Mac gets as it
// arrives new, so its state and its events agree.
const transferBatchedReason = "transfer: batch not carried"

// runLeadsTransfer is `leads transfer export DIR` (the source machine) and `leads transfer
// import DIR` (the server): the lead data moves re-keyed by natural keys, because ids
// collide with the server's own rows.
func runLeadsTransfer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads transfer", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || strings.TrimSpace(pos[1]) == "" {
		return usageError(leadsTransferUsage)
	}
	switch pos[0] {
	case "export":
		return withTransferStore(ctx, func(s *store.Store) error { return transferExport(ctx, s.Pool(), pos[1]) })
	case "import":
		return withTransferStore(ctx, func(s *store.Store) error { return transferImport(ctx, s.Pool(), pos[1]) })
	}
	return usageError("unknown transfer subcommand %q; %s", pos[0], leadsTransferUsage)
}

// withTransferStore opens the app database; the transfer needs no leads.yaml.
func withTransferStore(ctx context.Context, fn func(*store.Store) error) error {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
}

// The records, one JSON object per line. Every reference is the referenced row's natural
// key, never its id.

// tFetchKey is a fetch's natural key; body_sha256 is null for a transport error.
type tFetchKey struct {
	URL       string    `json:"url"`
	FetchedAt time.Time `json:"fetched_at"`
	SHA256    []byte    `json:"body_sha256"`
}

func (k tFetchKey) key() string {
	sha := "-"
	if k.SHA256 != nil {
		sha = hex.EncodeToString(k.SHA256)
	}
	return k.URL + "\x00" + microKey(k.FetchedAt) + "\x00" + sha
}

func microKey(t time.Time) string { return strconv.FormatInt(t.UnixMicro(), 10) }

// hkey is a natural key folded to 16 bytes (the first half of its SHA-256), so the import's
// large key sets hold a fixed 16 bytes a row, not the URL. Two of a few million keys share
// a hash with odds near 1e-26, far below any failure the import can see otherwise.
type hkey [16]byte

func hashKey(k string) hkey {
	sum := sha256.Sum256([]byte(k))
	var h hkey
	copy(h[:], sum[:16])
	return h
}

type tFetch struct {
	tFetchKey
	Source      string          `json:"source"`
	Kind        string          `json:"kind"`
	FinalURL    string          `json:"final_url"`
	Status      int             `json:"status"`
	Body        []byte          `json:"body"`
	ContentType string          `json:"content_type"`
	BodySize    *int            `json:"body_size"`
	Error       string          `json:"error"`
	Headers     json.RawMessage `json:"headers,omitempty"` // fetch_header.headers, when kept
}

type tSite struct {
	Domain      string     `json:"domain"`
	Country     string     `json:"country"`
	Origin      string     `json:"origin"`
	ListRank    *int       `json:"list_rank"`
	AddedAt     time.Time  `json:"added_at"`
	State       string     `json:"state"`
	LastProbeAt *time.Time `json:"last_probe_at"` // the key of site.last_probe_id within the site
}

type tProbeKey struct {
	Site      string    `json:"site"`
	CreatedAt time.Time `json:"created_at"`
}

func (k tProbeKey) key() string { return k.Site + "\x00" + microKey(k.CreatedAt) }

type tProbe struct {
	tProbeKey
	Outcome      string          `json:"outcome"`
	FinalHost    string          `json:"final_host"`
	IsDrupal     bool            `json:"is_drupal"`
	Major        int             `json:"major"`
	ExactVersion string          `json:"exact_version"`
	Ambiguous    string          `json:"ambiguous"`
	Signals      json.RawMessage `json:"signals"` // each fetch_id replaced by "fetch": its key
	Fetches      []tFetchKey     `json:"fetches"`
	SiteName     string          `json:"site_name"`
	DerivedBy    string          `json:"derived_by"`
}

type tOrg struct {
	Domain      string     `json:"domain"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Country     string     `json:"country"`
	Site        *string    `json:"site"`
	Source      string     `json:"source"`
	SourceFetch *tFetchKey `json:"source_fetch"`
	CreatedAt   time.Time  `json:"created_at"`
}

type tRouteKey struct {
	Org   string `json:"org"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

func (k tRouteKey) key() string { return k.Org + "\x00" + k.Kind + "\x00" + k.Value }

type tRoute struct {
	tRouteKey
	Fetch     tFetchKey `json:"fetch"`
	MXOK      bool      `json:"mx_ok"`
	DerivedBy string    `json:"derived_by"`
	FoundAt   time.Time `json:"found_at"`
}

type tFindingKey struct {
	Org       string    `json:"org"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

func (k tFindingKey) key() string { return k.Org + "\x00" + k.URL + "\x00" + microKey(k.CreatedAt) }

type tFinding struct {
	tFindingKey
	Phrase    string    `json:"phrase"`
	Fetch     tFetchKey `json:"fetch"`
	DerivedBy string    `json:"derived_by"`
}

type tAttempt struct {
	Org         string    `json:"org"`
	RoutesFound int       `json:"routes_found"`
	AttemptedAt time.Time `json:"attempted_at"`
}

type tLead struct {
	Org       string       `json:"org"`
	Kind      string       `json:"kind"`
	Probe     *tProbeKey   `json:"probe"`
	Finding   *tFindingKey `json:"finding"`
	Channel   string       `json:"channel"`
	Route     *tRouteKey   `json:"route"`
	State     string       `json:"state"`
	StateAt   time.Time    `json:"state_at"`
	CreatedAt time.Time    `json:"created_at"`
}

type tEvent struct {
	Lead   string    `json:"lead"` // the lead's org domain
	State  string    `json:"state"`
	Reason string    `json:"reason"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

type tSuppression struct {
	Kind      string    `json:"kind"`
	Value     string    `json:"value"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

type transferManifest struct {
	Format     int            `json:"format"`
	ExportedAt time.Time      `json:"exported_at"`
	Files      []manifestFile `json:"files"`
}

type manifestFile struct {
	Name  string `json:"name"`
	Rows  int    `json:"rows"`
	Bytes int64  `json:"bytes"`
}

// ---- export ----

// errLiveSending refuses an export while a message on the source may be going out.
var errLiveSending = errors.New("leads transfer: a live message is sending or may have been sent")

// jsonlWriter writes one kind's records, one per line, to a new 0600 file.
type jsonlWriter struct {
	f    *os.File
	w    *bufio.Writer
	enc  *json.Encoder
	rows int
}

func newJSONL(dir, name string) (*jsonlWriter, error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- the operator's own transfer directory
	if err != nil {
		return nil, fmt.Errorf("leads transfer: create %s: %w", name, err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &jsonlWriter{f: f, w: w, enc: enc}, nil
}

func (j *jsonlWriter) put(v any) error {
	j.rows++
	return j.enc.Encode(v)
}

// close flushes and closes the file and returns its manifest line.
func (j *jsonlWriter) close() (manifestFile, error) {
	name := filepath.Base(j.f.Name())
	if err := j.w.Flush(); err != nil {
		_ = j.f.Close()
		return manifestFile{}, fmt.Errorf("leads transfer: write %s: %w", name, err)
	}
	st, err := j.f.Stat()
	if err != nil {
		_ = j.f.Close()
		return manifestFile{}, fmt.Errorf("leads transfer: stat %s: %w", name, err)
	}
	if err := j.f.Close(); err != nil {
		return manifestFile{}, fmt.Errorf("leads transfer: close %s: %w", name, err)
	}
	return manifestFile{Name: name, Rows: j.rows, Bytes: st.Size()}, nil
}

// exportCounts is what the export printed beside the per-file rows.
type exportCounts struct {
	bodies  int // fetches carrying a body
	batched int // leads batched on the source, carried as new
}

// liveSendingSQL counts the live messages that are sending or may have been sent: not in a
// shadow batch, not a selftest's.
const liveSendingSQL = `SELECT count(*) FROM outreach_message m
	JOIN outreach_batch b ON b.id = m.batch_id
	JOIN lead l ON l.id = m.lead_id
	JOIN org o ON o.id = l.org_id
	WHERE NOT b.shadow AND m.state IN ('sending', 'maybe_sent')
	  AND l.kind <> 'selftest' AND o.kind <> 'selftest'`

// carriedOrgSQL is true for an org that crosses: not a selftest, not on a reserved TLD ($1).
const carriedOrgSQL = `o.kind <> 'selftest' AND o.domain !~ $1`

// transferExport writes the lead data at pool to dir, read in one repeatable-read, read-only
// transaction, so every file is one moment's picture. It writes nothing to the database.
func transferExport(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("leads transfer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(transferExportTimeout.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("leads transfer: set statement_timeout: %w", err)
	}
	var sending int
	if err := tx.QueryRow(ctx, liveSendingSQL).Scan(&sending); err != nil {
		return fmt.Errorf("leads transfer: count sending messages: %w", err)
	}
	if sending > 0 {
		return fmt.Errorf("%w: %s; run leads sync and resolve each before exporting",
			errLiveSending, plural(sending, "message", "messages"))
	}
	return exportTx(ctx, tx, dir)
}

// exportTx writes the export from tx, a repeatable-read transaction, into dir.
func exportTx(ctx context.Context, tx pgx.Tx, dir string) error {
	if err := emptyDir(dir); err != nil {
		return err
	}
	var exportedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT date_trunc('microseconds', clock_timestamp())`).Scan(&exportedAt); err != nil {
		return fmt.Errorf("leads transfer: clock: %w", err)
	}
	ex := exporter{tx: tx, dir: dir, re: transferReservedRE(), at: exportedAt}
	files, counts, err := ex.run(ctx)
	if err != nil {
		return err
	}
	m := transferManifest{Format: transferFormat, ExportedAt: exportedAt, Files: files}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("leads transfer: manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, transferManifestName), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("leads transfer: write manifest: %w", err)
	}
	printExport(stdout, files, counts)
	return nil
}

// emptyDir makes dir (0700) or accepts it when it exists and is empty, so an export never
// mixes with an earlier one.
func emptyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("leads transfer: make directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("leads transfer: read directory: %w", err)
	}
	if len(entries) > 0 {
		return usageError("the export directory is not empty; give a new or empty one")
	}
	return nil
}

type exporter struct {
	tx  pgx.Tx
	dir string
	re  string    // transferReservedRE
	at  time.Time // the export's moment: a batched lead's new state_at
	// fetchKeys maps every carried fetch's id to its natural key.
	fetchKeys map[int64]tFetchKey
}

func (ex *exporter) run(ctx context.Context) ([]manifestFile, exportCounts, error) {
	if err := ex.loadFetchKeys(ctx); err != nil {
		return nil, exportCounts{}, err
	}
	var files []manifestFile
	var counts exportCounts
	steps := []struct {
		name string
		fn   func(context.Context, *jsonlWriter, *exportCounts) error
	}{
		{tfSites, ex.sites}, {tfFetches, ex.fetches}, {tfProbes, ex.probes}, {tfOrgs, ex.orgs},
		{tfRoutes, ex.routes}, {tfFindings, ex.findings}, {tfAttempts, ex.attempts},
	}
	for _, st := range steps {
		mf, err := ex.write(ctx, st.name, func(w *jsonlWriter) error { return st.fn(ctx, w, &counts) })
		if err != nil {
			return nil, exportCounts{}, err
		}
		files = append(files, mf)
	}
	// Leads before events: the events follow the leads that crossed.
	var leadIDs []int64
	var batched []string
	mf, err := ex.write(ctx, tfLeads, func(w *jsonlWriter) error {
		var err error
		leadIDs, batched, err = ex.leads(ctx, w)
		return err
	})
	if err != nil {
		return nil, exportCounts{}, err
	}
	counts.batched = len(batched)
	files = append(files, mf)
	mf, err = ex.write(ctx, tfEvents, func(w *jsonlWriter) error { return ex.events(ctx, w, leadIDs, batched) })
	if err != nil {
		return nil, exportCounts{}, err
	}
	files = append(files, mf)
	mf, err = ex.write(ctx, tfSuppressions, func(w *jsonlWriter) error { return ex.suppressions(ctx, w) })
	if err != nil {
		return nil, exportCounts{}, err
	}
	return append(files, mf), counts, nil
}

func (ex *exporter) write(_ context.Context, name string, fn func(*jsonlWriter) error) (manifestFile, error) {
	w, err := newJSONL(ex.dir, name)
	if err != nil {
		return manifestFile{}, err
	}
	if err := fn(w); err != nil {
		_, _ = w.close()
		return manifestFile{}, err
	}
	return w.close()
}

// loadFetchKeys reads the key of every fetch a carried row points at: a carried probe's
// fetches and its signals' fetches, a carried org's source fetch, its routes' and its
// findings' pages. No other fetch crosses.
func (ex *exporter) loadFetchKeys(ctx context.Context) error {
	rows, err := ex.tx.Query(ctx, `WITH cp AS (
		  SELECT p.fetch_ids, p.signals FROM site_probe p JOIN site s ON s.id = p.site_id WHERE s.domain !~ $1
		), co AS (
		  SELECT o.id, o.source_fetch_id FROM org o WHERE `+carriedOrgSQL+`
		), ids AS (
		  SELECT unnest(fetch_ids) AS id FROM cp
		  UNION SELECT (e->>'fetch_id')::bigint FROM cp, jsonb_array_elements(cp.signals) e
		    WHERE jsonb_typeof(e->'fetch_id') = 'number' AND (e->>'fetch_id')::bigint > 0
		  UNION SELECT source_fetch_id FROM co WHERE source_fetch_id IS NOT NULL
		  UNION SELECT r.fetch_id FROM contact_route r JOIN co ON co.id = r.org_id
		  UNION SELECT f.fetch_id FROM agency_finding f JOIN co ON co.id = f.org_id
		)
		SELECT f.id, f.url, f.fetched_at, f.body_sha256 FROM http_fetch f JOIN ids ON ids.id = f.id`, ex.re)
	if err != nil {
		return fmt.Errorf("leads transfer: fetch keys: %w", err)
	}
	defer rows.Close()
	ex.fetchKeys = map[int64]tFetchKey{}
	for rows.Next() {
		var id int64
		var k tFetchKey
		if err := rows.Scan(&id, &k.URL, &k.FetchedAt, &k.SHA256); err != nil {
			return fmt.Errorf("leads transfer: fetch keys: %w", err)
		}
		ex.fetchKeys[id] = k
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("leads transfer: fetch keys: %w", err)
	}
	return nil
}

// fetchKey is the key of carried fetch id; a reference outside the carried set is an error.
func (ex *exporter) fetchKey(id int64) (tFetchKey, error) {
	k, ok := ex.fetchKeys[id]
	if !ok {
		return tFetchKey{}, errors.New("leads transfer: a row points at a fetch that is not in the database")
	}
	return k, nil
}

// each runs sql and calls fn per row, streaming.
func each(ctx context.Context, tx pgx.Tx, what, sql string, args []any, fn func(pgx.Rows) error) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("leads transfer: %s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("leads transfer: %s: %w", what, err)
	}
	return nil
}

func (ex *exporter) sites(ctx context.Context, w *jsonlWriter, _ *exportCounts) error {
	return each(ctx, ex.tx, "sites", `SELECT s.domain, s.country, s.origin, s.list_rank, s.added_at, s.state, p.created_at
		FROM site s LEFT JOIN site_probe p ON p.id = s.last_probe_id
		WHERE s.domain !~ $1 ORDER BY s.id`, []any{ex.re}, func(r pgx.Rows) error {
		var s tSite
		if err := r.Scan(&s.Domain, &s.Country, &s.Origin, &s.ListRank, &s.AddedAt, &s.State, &s.LastProbeAt); err != nil {
			return fmt.Errorf("leads transfer: sites: %w", err)
		}
		return w.put(s)
	})
}

func (ex *exporter) fetches(ctx context.Context, w *jsonlWriter, c *exportCounts) error {
	ids := make([]int64, 0, len(ex.fetchKeys))
	for id := range ex.fetchKeys {
		ids = append(ids, id)
	}
	seen := map[hkey]bool{}
	return each(ctx, ex.tx, "fetches", `SELECT f.source, f.kind, f.url, f.final_url, f.status, f.fetched_at, f.body,
		  f.content_type, f.body_sha256, f.body_size, f.error, h.headers::text
		FROM http_fetch f LEFT JOIN fetch_header h ON h.fetch_id = f.id
		WHERE f.id = ANY($1) ORDER BY f.id`, []any{ids}, func(r pgx.Rows) error {
		var f tFetch
		var headers *string
		if err := r.Scan(&f.Source, &f.Kind, &f.URL, &f.FinalURL, &f.Status, &f.FetchedAt, &f.Body,
			&f.ContentType, &f.SHA256, &f.BodySize, &f.Error, &headers); err != nil {
			return fmt.Errorf("leads transfer: fetches: %w", err)
		}
		// Two rows with one key cross once; the import maps every reference to it.
		k := hashKey(f.key())
		if seen[k] {
			return nil
		}
		seen[k] = true
		if headers != nil {
			f.Headers = json.RawMessage(*headers)
		}
		if len(f.Body) > 0 {
			c.bodies++
		}
		return w.put(f)
	})
}

func (ex *exporter) probes(ctx context.Context, w *jsonlWriter, _ *exportCounts) error {
	return each(ctx, ex.tx, "probes", `SELECT s.domain, p.created_at, p.outcome, p.final_host, p.is_drupal, p.major,
		  p.exact_version, p.ambiguous, p.signals::text, p.fetch_ids, p.site_name, p.derived_by
		FROM site_probe p JOIN site s ON s.id = p.site_id
		WHERE s.domain !~ $1 ORDER BY p.id`, []any{ex.re}, func(r pgx.Rows) error {
		var p tProbe
		var signals string
		var fetchIDs []int64
		if err := r.Scan(&p.Site, &p.CreatedAt, &p.Outcome, &p.FinalHost, &p.IsDrupal, &p.Major,
			&p.ExactVersion, &p.Ambiguous, &signals, &fetchIDs, &p.SiteName, &p.DerivedBy); err != nil {
			return fmt.Errorf("leads transfer: probes: %w", err)
		}
		sig, err := signalsOut([]byte(signals), ex.fetchKey)
		if err != nil {
			return err
		}
		p.Signals = sig
		p.Fetches = make([]tFetchKey, 0, len(fetchIDs))
		for _, id := range fetchIDs {
			k, err := ex.fetchKey(id)
			if err != nil {
				return err
			}
			p.Fetches = append(p.Fetches, k)
		}
		return w.put(p)
	})
}

// signalsOut replaces each signal's positive fetch_id with "fetch": that fetch's key; every
// other field of a signal is kept as it is.
func signalsOut(raw []byte, key func(int64) (tFetchKey, error)) (json.RawMessage, error) {
	var sigs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sigs); err != nil {
		return nil, fmt.Errorf("leads transfer: probe signals: %w", err)
	}
	for _, s := range sigs {
		v, ok := s["fetch_id"]
		if !ok {
			continue
		}
		var id int64
		if err := json.Unmarshal(v, &id); err != nil {
			return nil, fmt.Errorf("leads transfer: probe signal fetch_id: %w", err)
		}
		if id <= 0 {
			continue
		}
		k, err := key(id)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(k)
		if err != nil {
			return nil, fmt.Errorf("leads transfer: probe signals: %w", err)
		}
		delete(s, "fetch_id")
		s["fetch"] = b
	}
	if sigs == nil {
		return json.RawMessage("[]"), nil
	}
	b, err := json.Marshal(sigs)
	if err != nil {
		return nil, fmt.Errorf("leads transfer: probe signals: %w", err)
	}
	return b, nil
}

func (ex *exporter) orgs(ctx context.Context, w *jsonlWriter, _ *exportCounts) error {
	// A site on a reserved TLD does not cross, so an org pointing at one arrives without it.
	return each(ctx, ex.tx, "orgs", `SELECT o.domain, o.kind, o.name, o.country, s.domain, o.source, o.source_fetch_id, o.created_at
		FROM org o LEFT JOIN site s ON s.id = o.site_id AND s.domain !~ $1
		WHERE `+carriedOrgSQL+` ORDER BY o.id`, []any{ex.re}, func(r pgx.Rows) error {
		var o tOrg
		var fetchID *int64
		if err := r.Scan(&o.Domain, &o.Kind, &o.Name, &o.Country, &o.Site, &o.Source, &fetchID, &o.CreatedAt); err != nil {
			return fmt.Errorf("leads transfer: orgs: %w", err)
		}
		if fetchID != nil {
			k, err := ex.fetchKey(*fetchID)
			if err != nil {
				return err
			}
			o.SourceFetch = &k
		}
		return w.put(o)
	})
}

func (ex *exporter) routes(ctx context.Context, w *jsonlWriter, _ *exportCounts) error {
	return each(ctx, ex.tx, "routes", `SELECT o.domain, r.kind, r.value, r.fetch_id, r.mx_ok, r.derived_by, r.found_at
		FROM contact_route r JOIN org o ON o.id = r.org_id
		WHERE `+carriedOrgSQL+` ORDER BY r.id`, []any{ex.re}, func(r pgx.Rows) error {
		var rt tRoute
		var fetchID int64
		if err := r.Scan(&rt.Org, &rt.Kind, &rt.Value, &fetchID, &rt.MXOK, &rt.DerivedBy, &rt.FoundAt); err != nil {
			return fmt.Errorf("leads transfer: routes: %w", err)
		}
		k, err := ex.fetchKey(fetchID)
		if err != nil {
			return err
		}
		rt.Fetch = k
		return w.put(rt)
	})
}

func (ex *exporter) findings(ctx context.Context, w *jsonlWriter, _ *exportCounts) error {
	return each(ctx, ex.tx, "findings", `SELECT o.domain, f.url, f.created_at, f.phrase, f.fetch_id, f.derived_by
		FROM agency_finding f JOIN org o ON o.id = f.org_id
		WHERE `+carriedOrgSQL+` ORDER BY f.id`, []any{ex.re}, func(r pgx.Rows) error {
		var f tFinding
		var fetchID int64
		if err := r.Scan(&f.Org, &f.URL, &f.CreatedAt, &f.Phrase, &fetchID, &f.DerivedBy); err != nil {
			return fmt.Errorf("leads transfer: findings: %w", err)
		}
		k, err := ex.fetchKey(fetchID)
		if err != nil {
			return err
		}
		f.Fetch = k
		return w.put(f)
	})
}

func (ex *exporter) attempts(ctx context.Context, w *jsonlWriter, _ *exportCounts) error {
	return each(ctx, ex.tx, "attempts", `SELECT o.domain, a.routes_found, a.attempted_at
		FROM contact_attempt a JOIN org o ON o.id = a.org_id
		WHERE `+carriedOrgSQL+` ORDER BY a.id`, []any{ex.re}, func(r pgx.Rows) error {
		var a tAttempt
		if err := r.Scan(&a.Org, &a.RoutesFound, &a.AttemptedAt); err != nil {
			return fmt.Errorf("leads transfer: attempts: %w", err)
		}
		return w.put(a)
	})
}

// leads writes every lead that crosses: not a selftest, its org, finding and route all
// crossing. A lead batched on the source arrives new (its batch stays behind) at the
// export's moment. It returns the ids written and the org domains of the batched ones.
func (ex *exporter) leads(ctx context.Context, w *jsonlWriter) (ids []int64, batched []string, err error) {
	err = each(ctx, ex.tx, "leads", `SELECT l.id, o.domain, l.kind, ps.domain, p.created_at, fo.domain, f.url, f.created_at,
		  l.channel, ro.domain, r.kind, r.value, l.state, l.state_at, l.created_at
		FROM lead l JOIN org o ON o.id = l.org_id
		LEFT JOIN site_probe p ON p.id = l.probe_id LEFT JOIN site ps ON ps.id = p.site_id
		LEFT JOIN agency_finding f ON f.id = l.agency_finding_id LEFT JOIN org fo ON fo.id = f.org_id
		LEFT JOIN contact_route r ON r.id = l.route_id LEFT JOIN org ro ON ro.id = r.org_id
		WHERE l.kind <> 'selftest' AND `+carriedOrgSQL+`
		  AND (p.id IS NULL OR ps.domain !~ $1)
		  AND (f.id IS NULL OR (fo.kind <> 'selftest' AND fo.domain !~ $1))
		  AND (r.id IS NULL OR (ro.kind <> 'selftest' AND ro.domain !~ $1))
		ORDER BY l.id`, []any{ex.re}, func(r pgx.Rows) error {
		var (
			id                          int64
			l                           tLead
			probeSite, findOrg, findURL *string
			routeOrg, routeKind, value  *string
			probeAt, findAt             *time.Time
		)
		if err := r.Scan(&id, &l.Org, &l.Kind, &probeSite, &probeAt, &findOrg, &findURL, &findAt,
			&l.Channel, &routeOrg, &routeKind, &value, &l.State, &l.StateAt, &l.CreatedAt); err != nil {
			return fmt.Errorf("leads transfer: leads: %w", err)
		}
		if probeSite != nil && probeAt != nil {
			l.Probe = &tProbeKey{Site: *probeSite, CreatedAt: *probeAt}
		}
		if findOrg != nil && findURL != nil && findAt != nil {
			l.Finding = &tFindingKey{Org: *findOrg, URL: *findURL, CreatedAt: *findAt}
		}
		if routeOrg != nil && routeKind != nil && value != nil {
			l.Route = &tRouteKey{Org: *routeOrg, Kind: *routeKind, Value: *value}
		}
		if l.State == leadstore.LeadBatched {
			l.State, l.StateAt = leadstore.LeadNew, ex.at
			batched = append(batched, l.Org)
		}
		ids = append(ids, id)
		return w.put(l)
	})
	return ids, batched, err
}

// events writes the history of every lead that crossed, then, for each lead that was
// batched, the event that moved it back to new.
func (ex *exporter) events(ctx context.Context, w *jsonlWriter, leadIDs []int64, batched []string) error {
	if leadIDs == nil {
		leadIDs = []int64{}
	}
	err := each(ctx, ex.tx, "events", `SELECT o.domain, e.state, e.reason, e.actor, e.at
		FROM lead_event e JOIN lead l ON l.id = e.lead_id JOIN org o ON o.id = l.org_id
		WHERE e.lead_id = ANY($1) ORDER BY e.id`, []any{leadIDs}, func(r pgx.Rows) error {
		var e tEvent
		if err := r.Scan(&e.Lead, &e.State, &e.Reason, &e.Actor, &e.At); err != nil {
			return fmt.Errorf("leads transfer: events: %w", err)
		}
		return w.put(e)
	})
	if err != nil {
		return err
	}
	for _, org := range batched {
		if err := w.put(tEvent{Lead: org, State: leadstore.LeadNew, Reason: transferBatchedReason,
			Actor: leadstore.ActorSystem, At: ex.at}); err != nil {
			return err
		}
	}
	return nil
}

// suppressions writes every suppression but those on a reserved TLD and a selftest's own
// address.
func (ex *exporter) suppressions(ctx context.Context, w *jsonlWriter) error {
	return each(ctx, ex.tx, "suppressions", `SELECT x.kind, x.value, x.reason, x.source, x.created_at
		FROM suppression x
		WHERE CASE WHEN x.kind = 'domain' THEN x.value ELSE split_part(x.value, '@', 2) END !~ $1
		  AND NOT (x.kind = 'address' AND EXISTS (
		    SELECT 1 FROM outreach_message m JOIN lead l ON l.id = m.lead_id
		    WHERE l.kind = 'selftest' AND lower(m.to_address) = x.value))
		ORDER BY x.id`, []any{ex.re}, func(r pgx.Rows) error {
		var s tSuppression
		if err := r.Scan(&s.Kind, &s.Value, &s.Reason, &s.Source, &s.CreatedAt); err != nil {
			return fmt.Errorf("leads transfer: suppressions: %w", err)
		}
		return w.put(s)
	})
}

func printExport(w io.Writer, files []manifestFile, c exportCounts) {
	parts := make([]string, 0, len(files))
	var total int64
	for _, f := range files {
		total += f.Bytes
		label := fmt.Sprintf("%s %d", strings.TrimSuffix(f.Name, ".jsonl"), f.Rows)
		switch f.Name {
		case tfFetches:
			label += fmt.Sprintf(" (%d with a body)", c.bodies)
		case tfLeads:
			label += fmt.Sprintf(" (%d batched, carried as new)", c.batched)
		}
		parts = append(parts, label)
	}
	_, _ = fmt.Fprintf(w, "leads transfer export: %s\n", strings.Join(parts, " · "))
	_, _ = fmt.Fprintf(w, "leads transfer export: %d files and the manifest, %s in all, each mode 0600\n", len(files), humanBytes(total))
}

// humanBytes is n in B, KB, MB or GB (powers of 1024), one decimal above bytes.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, units := float64(n)/unit, []string{"KB", "MB", "GB", "TB"}
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// ---- import ----

// readManifest reads dir's manifest and refuses one whose format, file list, rows or bytes
// do not match the files beside it.
func readManifest(dir string) (transferManifest, error) {
	var m transferManifest
	b, err := os.ReadFile(filepath.Join(dir, transferManifestName)) // #nosec G304 -- the operator's own transfer directory
	if err != nil {
		return m, usageError("no transfer manifest in that directory: %v", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, usageError("the transfer manifest does not parse: %v", err)
	}
	if m.Format != transferFormat {
		return m, usageError("the transfer manifest is format %d; this leadhound reads format %d", m.Format, transferFormat)
	}
	byName := map[string]manifestFile{}
	for _, f := range m.Files {
		if !slices.Contains(transferFiles, f.Name) {
			return m, usageError("the transfer manifest names a file this leadhound does not read")
		}
		byName[f.Name] = f
	}
	var problems []string
	for _, name := range transferFiles {
		f, ok := byName[name]
		if !ok {
			problems = append(problems, name+" is not in the manifest")
			continue
		}
		rows, size, err := countLines(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if rows != f.Rows || size != f.Bytes {
			problems = append(problems, fmt.Sprintf("%s has %d rows and %d bytes, the manifest says %d and %d", name, rows, size, f.Rows, f.Bytes))
		}
	}
	if len(problems) > 0 {
		return m, usageError("the transfer does not match its manifest: %s", strings.Join(problems, "; "))
	}
	return m, nil
}

// countLines counts the newline-ended lines of path and its size in bytes.
func countLines(path string) (int, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator's own transfer directory
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1<<20)
	var lines int
	var size int64
	for {
		n, err := f.Read(buf)
		lines += bytes.Count(buf[:n], []byte{'\n'})
		size += int64(n)
		if errors.Is(err, io.EOF) {
			return lines, size, nil
		}
		if err != nil {
			return 0, 0, err
		}
	}
}

// eachBatch decodes path's records and calls fn with up to transferBatchRows of them at a
// time, or fewer once their weight (bodies, for fetches) reaches maxWeight.
func eachBatch[T any](dir, name string, weight func(*T) int, maxWeight int, fn func([]T) error) error {
	f, err := os.Open(filepath.Join(dir, name)) // #nosec G304 -- the operator's own transfer directory
	if err != nil {
		return fmt.Errorf("leads transfer: open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	dec.DisallowUnknownFields()
	batch := make([]T, 0, transferBatchRows)
	w := 0
	for {
		var v T
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("leads transfer: read %s: %w", name, err)
		}
		batch = append(batch, v)
		if weight != nil {
			w += weight(&v)
		}
		if len(batch) == transferBatchRows || (weight != nil && w >= maxWeight) {
			if err := fn(batch); err != nil {
				return err
			}
			batch, w = batch[:0], 0
		}
	}
	if len(batch) > 0 {
		return fn(batch)
	}
	return nil
}

// tally is what the import did with one kind.
type tally struct{ added, known int }

type importer struct {
	pool *pgxpool.Pool
	dir  string
	// runID is the import's run row, 0 until a row that hangs from it is written; pendingRun
	// is the one the open transaction created.
	runID, pendingRun int64

	// Sites and probes, the large kinds, are never held: their ids are joined in SQL by
	// natural key. The fetch maps are keyed by hash and dropped once nothing reads them.
	fetches  map[hkey]int64    // fetch key → id, until the findings are in
	needed   map[hkey]struct{} // the fetch keys a row still to be written points at
	orgs     map[string]int64  // domain → id
	routes   map[string]int64  // route key → id
	findings map[string]int64  // finding key → id
	leads    map[string]int64  // org domain → id

	tallies  map[string]*tally
	pointers int // sites whose last probe and state were set from the transfer
}

// transferImport reads the transfer in dir into the database at pool. Each kind is matched
// by its natural key, and a row already there is left as it is, so a second import of the
// same directory changes nothing.
func transferImport(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	if _, err := readManifest(dir); err != nil {
		return err
	}
	im := &importer{
		pool: pool, dir: dir,
		fetches: map[hkey]int64{}, needed: map[hkey]struct{}{},
		orgs: map[string]int64{}, routes: map[string]int64{}, findings: map[string]int64{},
		leads: map[string]int64{}, tallies: map[string]*tally{},
	}
	for _, name := range transferFiles {
		im.tallies[name] = &tally{}
	}
	steps := []func(context.Context) error{
		im.importSites, im.findNeeded, im.importFetches, im.importProbes, im.setPointers,
		im.importOrgs, im.importRoutes, im.importFindings, im.dropFetches, im.importAttempts, im.importLeads,
		im.importEvents, im.importSuppressions,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	if im.runID != 0 {
		if err := store.NewQueries(pool).FinishRun(ctx, im.runID, store.RunResult{Note: "leads transfer import"}); err != nil {
			return err
		}
	}
	im.print(stdout)
	return nil
}

// tx runs fn in one transaction whose statements are bounded by transferStatementTimeout
// (SET LOCAL, so the bound ends with the transaction). A run row created inside becomes the
// import's only once the transaction commits.
func (im *importer) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	im.pendingRun = 0
	tx, err := im.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("leads transfer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(transferStatementTimeout.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("leads transfer: set statement_timeout: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("leads transfer: commit: %w", err)
	}
	if im.pendingRun != 0 {
		im.runID, im.pendingRun = im.pendingRun, 0
	}
	return nil
}

// run returns the import's run row, writing it in tx the first time a row needs it.
func (im *importer) run(ctx context.Context, tx pgx.Tx) (int64, error) {
	if im.runID != 0 {
		return im.runID, nil
	}
	if im.pendingRun == 0 {
		if err := tx.QueryRow(ctx, `INSERT INTO run (command, source) VALUES ($1, $2) RETURNING id`,
			commandTransfer, sourceLeads).Scan(&im.pendingRun); err != nil {
			return 0, fmt.Errorf("leads transfer: start run: %w", err)
		}
	}
	return im.pendingRun, nil
}

// matchIDs runs sql, which returns (ordinal, id) for every batch row whose natural key is
// already in the database, and returns the ids by ordinal (1-based).
func matchIDs(ctx context.Context, db store.DBTX, what, sql string, args ...any) (map[int64]int64, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("leads transfer: match %s: %w", what, err)
	}
	defer rows.Close()
	ids := map[int64]int64{}
	for rows.Next() {
		var i, id int64
		if err := rows.Scan(&i, &id); err != nil {
			return nil, fmt.Errorf("leads transfer: match %s: %w", what, err)
		}
		if _, dup := ids[i]; !dup {
			ids[i] = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leads transfer: match %s: %w", what, err)
	}
	return ids, nil
}

// The natural-key matches, one per kind. Each joins a batch of keys to the table through
// its key's index.
const (
	matchProbesSQL = `SELECT t.i, p.id FROM unnest($1::text[], $2::timestamptz[]) WITH ORDINALITY AS t(d, at, i)
		JOIN site s ON s.domain = t.d JOIN site_probe p ON p.site_id = s.id AND p.created_at = t.at`
	matchOrgsSQL = `SELECT t.i, o.id FROM unnest($1::text[]) WITH ORDINALITY AS t(d, i)
		JOIN org o ON o.domain = t.d`
	matchRoutesSQL = `SELECT t.i, r.id FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS t(d, k, v, i)
		JOIN org o ON o.domain = t.d JOIN contact_route r ON r.org_id = o.id AND r.kind = t.k AND r.value = t.v`
	matchFindingsSQL = `SELECT t.i, f.id FROM unnest($1::text[], $2::text[], $3::timestamptz[]) WITH ORDINALITY AS t(d, u, at, i)
		JOIN org o ON o.domain = t.d JOIN agency_finding f ON f.org_id = o.id AND f.url = t.u AND f.created_at = t.at`
	matchAttemptsSQL = `SELECT t.i, a.id FROM unnest($1::text[], $2::timestamptz[]) WITH ORDINALITY AS t(d, at, i)
		JOIN org o ON o.domain = t.d JOIN contact_attempt a ON a.org_id = o.id AND a.attempted_at = t.at`
	matchLeadsSQL = `SELECT t.i, l.id FROM unnest($1::text[]) WITH ORDINALITY AS t(d, i)
		JOIN org o ON o.domain = t.d JOIN lead l ON l.org_id = o.id`
	matchEventsSQL = `SELECT t.i, e.id FROM unnest($1::bigint[], $2::timestamptz[], $3::text[]) WITH ORDINALITY AS t(l, at, st, i)
		JOIN lead_event e ON e.lead_id = t.l AND e.at = t.at AND e.state = t.st`
	matchSuppressionsSQL = `SELECT t.i, x.id FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS t(k, v, r, i)
		JOIN suppression x ON x.kind = t.k AND x.value = t.v AND x.reason = t.r`
)

func probeArgs(batch []tProbe) []any {
	d, at := make([]string, len(batch)), make([]time.Time, len(batch))
	for i, p := range batch {
		d[i], at[i] = p.Site, p.CreatedAt
	}
	return []any{d, at}
}

func orgArgs(domains []string) []any { return []any{domains} }

func routeArgs(batch []tRoute) []any {
	d, k, v := make([]string, len(batch)), make([]string, len(batch)), make([]string, len(batch))
	for i, r := range batch {
		d[i], k[i], v[i] = r.Org, r.Kind, r.Value
	}
	return []any{d, k, v}
}

func findingArgs(batch []tFinding) []any {
	d, u, at := make([]string, len(batch)), make([]string, len(batch)), make([]time.Time, len(batch))
	for i, f := range batch {
		d[i], u[i], at[i] = f.Org, f.URL, f.CreatedAt
	}
	return []any{d, u, at}
}

func (im *importer) importSites(ctx context.Context) error {
	t := im.tallies[tfSites]
	return eachBatch(im.dir, tfSites, nil, 0, func(batch []tSite) error {
		n := len(batch)
		domain, country, origin, state := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		rank, added := make([]*int, n), make([]time.Time, n)
		for i, s := range batch {
			domain[i], country[i], origin[i], state[i], rank[i], added[i] = s.Domain, s.Country, s.Origin, s.State, s.ListRank, s.AddedAt
		}
		return im.tx(ctx, func(tx pgx.Tx) error {
			// The pointer to the last probe is set once the probes are in (setPointers).
			tag, err := tx.Exec(ctx, `INSERT INTO site (domain, country, origin, list_rank, added_at, state)
				SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::int[], $5::timestamptz[], $6::text[])
				ON CONFLICT (domain) DO NOTHING`, domain, country, origin, rank, added, state)
			if err != nil {
				return fmt.Errorf("leads transfer: insert sites: %w", err)
			}
			t.added += int(tag.RowsAffected())
			t.known += n - int(tag.RowsAffected())
			return nil
		})
	})
}

// scanInto runs sql, which returns (key, id) pairs, into m.
func scanInto(ctx context.Context, db store.DBTX, what string, m map[string]int64, sql string, args ...any) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("leads transfer: %s ids: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var id int64
		if err := rows.Scan(&k, &id); err != nil {
			return fmt.Errorf("leads transfer: %s ids: %w", what, err)
		}
		m[k] = id
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("leads transfer: %s ids: %w", what, err)
	}
	return nil
}

// findNeeded collects the fetch keys that the probes, orgs, routes and findings still to be
// written point at; only those fetches are imported.
func (im *importer) findNeeded(ctx context.Context) error {
	need := func(k tFetchKey) { im.needed[hashKey(k.key())] = struct{}{} }
	db := store.DBTX(im.pool)
	err := eachBatch(im.dir, tfProbes, nil, 0, func(batch []tProbe) error {
		have, err := matchIDs(ctx, db, "probes", matchProbesSQL, probeArgs(batch)...)
		if err != nil {
			return err
		}
		for i := range batch {
			if _, ok := have[int64(i+1)]; ok {
				continue
			}
			keys, err := probeFetchKeys(batch[i])
			if err != nil {
				return err
			}
			for _, k := range keys {
				need(k)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = eachBatch(im.dir, tfOrgs, nil, 0, func(batch []tOrg) error {
		domains := make([]string, len(batch))
		for i, o := range batch {
			domains[i] = o.Domain
		}
		have, err := matchIDs(ctx, db, "orgs", matchOrgsSQL, orgArgs(domains)...)
		if err != nil {
			return err
		}
		for i, o := range batch {
			if _, ok := have[int64(i+1)]; !ok && o.SourceFetch != nil {
				need(*o.SourceFetch)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = eachBatch(im.dir, tfRoutes, nil, 0, func(batch []tRoute) error {
		have, err := matchIDs(ctx, db, "routes", matchRoutesSQL, routeArgs(batch)...)
		if err != nil {
			return err
		}
		for i, r := range batch {
			if _, ok := have[int64(i+1)]; !ok {
				need(r.Fetch)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return eachBatch(im.dir, tfFindings, nil, 0, func(batch []tFinding) error {
		have, err := matchIDs(ctx, db, "findings", matchFindingsSQL, findingArgs(batch)...)
		if err != nil {
			return err
		}
		for i, f := range batch {
			if _, ok := have[int64(i+1)]; !ok {
				need(f.Fetch)
			}
		}
		return nil
	})
}

// probeFetchKeys is every fetch a probe points at: its fetches and its signals' fetches.
func probeFetchKeys(p tProbe) ([]tFetchKey, error) {
	keys := slices.Clone(p.Fetches)
	var sigs []map[string]json.RawMessage
	if err := json.Unmarshal(p.Signals, &sigs); err != nil {
		return nil, fmt.Errorf("leads transfer: probe signals: %w", err)
	}
	for _, s := range sigs {
		if v, ok := s["fetch"]; ok {
			var k tFetchKey
			if err := json.Unmarshal(v, &k); err != nil {
				return nil, fmt.Errorf("leads transfer: probe signal fetch: %w", err)
			}
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// importFetches writes the needed fetches with their bodies and headers. A fetch is matched
// first by its natural key among the fetches of earlier transfer runs (the batch's keys in
// a temporary table, joined through http_fetch_run_idx), so a fetch already imported is
// reused and never written twice; the server's own fetches are never scanned.
func (im *importer) importFetches(ctx context.Context) error {
	t := im.tallies[tfFetches]
	if len(im.needed) == 0 {
		return nil
	}
	// The needed keys are only read here; dropping them after leaves room for the id maps.
	defer func() { im.needed = nil }()
	weight := func(f *tFetch) int { return len(f.Body) }
	return eachBatch(im.dir, tfFetches, weight, transferBatchBodyBytes, func(all []tFetch) error {
		batch := make([]tFetch, 0, len(all))
		inBatch := map[hkey]bool{}
		for _, f := range all {
			k := hashKey(f.key())
			if _, need := im.needed[k]; !need || inBatch[k] {
				continue
			}
			if _, done := im.fetches[k]; done {
				continue
			}
			inBatch[k] = true
			batch = append(batch, f)
		}
		if len(batch) == 0 {
			return nil
		}
		return im.tx(ctx, func(tx pgx.Tx) error { return im.fetchBatch(ctx, tx, batch, t) })
	})
}

func (im *importer) fetchBatch(ctx context.Context, tx pgx.Tx, batch []tFetch, t *tally) error {
	n := len(batch)
	ord, urls, at, sha := make([]int64, n), make([]string, n), make([]time.Time, n), make([][]byte, n)
	for i, f := range batch {
		ord[i], urls[i], at[i], sha[i] = int64(i+1), f.URL, f.FetchedAt, f.SHA256
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE transfer_fetch_key (i bigint, url text, fetched_at timestamptz, body_sha256 bytea) ON COMMIT DROP`); err != nil {
		return fmt.Errorf("leads transfer: fetch keys table: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO transfer_fetch_key SELECT * FROM unnest($1::bigint[], $2::text[], $3::timestamptz[], $4::bytea[])`,
		ord, urls, at, sha); err != nil {
		return fmt.Errorf("leads transfer: fetch keys: %w", err)
	}
	have, err := matchIDs(ctx, tx, "fetches", `SELECT k.i, f.id FROM transfer_fetch_key k
		JOIN run r ON r.command = $1
		JOIN http_fetch f ON f.run_id = r.id AND f.url = k.url AND f.fetched_at = k.fetched_at
		  AND f.body_sha256 IS NOT DISTINCT FROM k.body_sha256`, commandTransfer)
	if err != nil {
		return err
	}
	var (
		source, kind, url, final, ctype, ferr []string
		status                                []int
		fetched                               []time.Time
		body, sums                            [][]byte
		size                                  []*int
		headers                               []*string
	)
	for i, f := range batch {
		if id, ok := have[int64(i+1)]; ok {
			im.fetches[hashKey(f.key())] = id
			t.known++
			continue
		}
		b := f.Body
		if b == nil {
			b = []byte{} // NOT NULL
		}
		var h *string
		if len(f.Headers) > 0 && string(f.Headers) != "null" {
			s := string(f.Headers)
			h = &s
		}
		source, kind, url, final, ctype, ferr = append(source, f.Source), append(kind, f.Kind), append(url, f.URL),
			append(final, f.FinalURL), append(ctype, f.ContentType), append(ferr, f.Error)
		status, fetched, body, sums, size, headers = append(status, f.Status), append(fetched, f.FetchedAt), append(body, b),
			append(sums, f.SHA256), append(size, f.BodySize), append(headers, h)
	}
	if len(url) == 0 {
		return nil
	}
	runID, err := im.run(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `WITH ins AS (
		  INSERT INTO http_fetch (run_id, source, kind, url, final_url, status, fetched_at, body, content_type, body_sha256, body_size, error)
		  SELECT $1, t.source, t.kind, t.url, t.final_url, t.status, t.fetched_at, t.body, t.content_type, t.sha, t.size, t.error
		  FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::int[], $7::timestamptz[], $8::bytea[], $9::text[], $10::bytea[], $11::int[], $12::text[])
		    AS t(source, kind, url, final_url, status, fetched_at, body, content_type, sha, size, error)
		  RETURNING id, url, fetched_at, body_sha256
		)
		SELECT id, url, fetched_at, body_sha256 FROM ins`,
		runID, source, kind, url, final, status, fetched, body, ctype, sums, size, ferr)
	if err != nil {
		return fmt.Errorf("leads transfer: insert fetches: %w", err)
	}
	inserted, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct {
		id int64
		k  tFetchKey
	}, error) {
		var v struct {
			id int64
			k  tFetchKey
		}
		err := r.Scan(&v.id, &v.k.URL, &v.k.FetchedAt, &v.k.SHA256)
		return v, err
	})
	if err != nil {
		return fmt.Errorf("leads transfer: insert fetches: %w", err)
	}
	t.added += len(inserted)
	byKey := map[string]*string{}
	for i := range url {
		byKey[tFetchKey{URL: url[i], FetchedAt: fetched[i], SHA256: sums[i]}.key()] = headers[i]
	}
	var hid []int64
	var htext []string
	for _, v := range inserted {
		k := v.k.key()
		im.fetches[hashKey(k)] = v.id
		if h := byKey[k]; h != nil {
			hid, htext = append(hid, v.id), append(htext, *h)
		}
	}
	if len(hid) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO fetch_header (fetch_id, headers)
			SELECT t.id, t.h::jsonb FROM unnest($1::bigint[], $2::text[]) AS t(id, h)
			ON CONFLICT (fetch_id) DO NOTHING`, hid, htext); err != nil {
			return fmt.Errorf("leads transfer: insert fetch headers: %w", err)
		}
	}
	return nil
}

// fetchID is the server id of the fetch with key k, imported or reused.
func (im *importer) fetchID(k tFetchKey) (int64, error) {
	id, ok := im.fetches[hashKey(k.key())]
	if !ok {
		return 0, errors.New("leads transfer: a row points at a fetch the transfer does not carry")
	}
	return id, nil
}

// signalsIn replaces each signal's "fetch" key with fetch_id, the server's id.
func (im *importer) signalsIn(raw json.RawMessage) (string, error) {
	var sigs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sigs); err != nil {
		return "", fmt.Errorf("leads transfer: probe signals: %w", err)
	}
	for _, s := range sigs {
		v, ok := s["fetch"]
		if !ok {
			continue
		}
		var k tFetchKey
		if err := json.Unmarshal(v, &k); err != nil {
			return "", fmt.Errorf("leads transfer: probe signal fetch: %w", err)
		}
		id, err := im.fetchID(k)
		if err != nil {
			return "", err
		}
		delete(s, "fetch")
		s["fetch_id"] = json.RawMessage(strconv.FormatInt(id, 10))
	}
	if sigs == nil {
		return "[]", nil
	}
	b, err := json.Marshal(sigs)
	if err != nil {
		return "", fmt.Errorf("leads transfer: probe signals: %w", err)
	}
	return string(b), nil
}

func (im *importer) importProbes(ctx context.Context) error {
	t := im.tallies[tfProbes]
	return eachBatch(im.dir, tfProbes, nil, 0, func(batch []tProbe) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			have, err := matchIDs(ctx, tx, "probes", matchProbesSQL, probeArgs(batch)...)
			if err != nil {
				return err
			}
			var (
				site, outcome, host, version, amb, sig, fids, name []string
				derived                                            []string
				drupal                                             []bool
				major                                              []int
				at                                                 []time.Time
			)
			for i, p := range batch {
				if _, ok := have[int64(i+1)]; ok {
					t.known++
					continue
				}
				s, err := im.signalsIn(p.Signals)
				if err != nil {
					return err
				}
				ids := make([]string, len(p.Fetches))
				for j, k := range p.Fetches {
					id, err := im.fetchID(k)
					if err != nil {
						return err
					}
					ids[j] = strconv.FormatInt(id, 10)
				}
				site, outcome, host, drupal, major = append(site, p.Site), append(outcome, p.Outcome), append(host, p.FinalHost),
					append(drupal, p.IsDrupal), append(major, p.Major)
				version, amb, sig, fids = append(version, p.ExactVersion), append(amb, p.Ambiguous), append(sig, s),
					append(fids, "{"+strings.Join(ids, ",")+"}")
				name, derived, at = append(name, p.SiteName), append(derived, p.DerivedBy), append(at, p.CreatedAt)
			}
			if len(site) == 0 {
				return nil
			}
			runID, err := im.run(ctx, tx)
			if err != nil {
				return err
			}
			// The site's id is joined by its domain; every site was imported first.
			tag, err := tx.Exec(ctx, `INSERT INTO site_probe (site_id, run_id, outcome, final_host, is_drupal, major,
				  exact_version, ambiguous, signals, fetch_ids, site_name, derived_by, created_at)
				SELECT s.id, $1, t.o, t.h, t.d, t.m, t.v, t.a, t.sig::jsonb, t.f::bigint[], t.n, t.db, t.at
				FROM unnest($2::text[], $3::text[], $4::text[], $5::bool[], $6::int[], $7::text[], $8::text[],
				  $9::text[], $10::text[], $11::text[], $12::text[], $13::timestamptz[])
				  AS t(site, o, h, d, m, v, a, sig, f, n, db, at)
				JOIN site s ON s.domain = t.site`,
				runID, site, outcome, host, drupal, major, version, amb, sig, fids, name, derived, at)
			if err != nil {
				return fmt.Errorf("leads transfer: insert probes: %w", err)
			}
			if int(tag.RowsAffected()) != len(site) {
				return errors.New("leads transfer: a probe's site is not in the transfer")
			}
			t.added += len(site)
			return nil
		})
	})
}

// setPointers points each site at its last probe from the transfer (joined by the probe's
// key), with the source's state, where the site has no last probe yet or an older one; a
// site already pointing at that probe or a newer one is left as it is.
func (im *importer) setPointers(ctx context.Context) error {
	return eachBatch(im.dir, tfSites, nil, 0, func(batch []tSite) error {
		var domain, state []string
		var at []time.Time
		for _, s := range batch {
			if s.LastProbeAt != nil {
				domain, state, at = append(domain, s.Domain), append(state, s.State), append(at, *s.LastProbeAt)
			}
		}
		if len(domain) == 0 {
			return nil
		}
		return im.tx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE site s SET last_probe_id = p.id, state = t.st
				FROM unnest($1::text[], $2::text[], $3::timestamptz[]) AS t(d, st, at), site_probe p
				WHERE s.domain = t.d AND p.site_id = s.id AND p.created_at = t.at
				  AND s.last_probe_id IS DISTINCT FROM p.id
				  AND (s.last_probe_id IS NULL
				    OR (SELECT created_at FROM site_probe WHERE id = s.last_probe_id) < t.at)`, domain, state, at)
			if err != nil {
				return fmt.Errorf("leads transfer: point sites: %w", err)
			}
			im.pointers += int(tag.RowsAffected())
			return nil
		})
	})
}

func (im *importer) importOrgs(ctx context.Context) error {
	t := im.tallies[tfOrgs]
	return eachBatch(im.dir, tfOrgs, nil, 0, func(batch []tOrg) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			all := make([]string, len(batch))
			for i, o := range batch {
				all[i] = o.Domain
			}
			have, err := matchIDs(ctx, tx, "orgs", matchOrgsSQL, orgArgs(all)...)
			if err != nil {
				return err
			}
			var kind, domain, name, country, source []string
			var site []*string
			var fetch []*int64
			var at []time.Time
			for i, o := range batch {
				if _, ok := have[int64(i+1)]; ok {
					t.known++
					continue
				}
				var fetchID *int64
				if o.SourceFetch != nil {
					id, err := im.fetchID(*o.SourceFetch)
					if err != nil {
						return err
					}
					fetchID = &id
				}
				kind, domain, name, country, source = append(kind, o.Kind), append(domain, o.Domain), append(name, o.Name),
					append(country, o.Country), append(source, o.Source)
				site, fetch, at = append(site, o.Site), append(fetch, fetchID), append(at, o.CreatedAt)
			}
			if len(domain) > 0 {
				// The site's id is joined by its domain; the export carries every site an org names.
				tag, err := tx.Exec(ctx, `INSERT INTO org (kind, domain, name, country, site_id, source, source_fetch_id, created_at)
					SELECT t.k, t.d, t.n, t.c, s.id, t.src, t.f, t.at
					FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::bigint[], $8::timestamptz[])
					  AS t(k, d, n, c, site, src, f, at)
					LEFT JOIN site s ON s.domain = t.site
					ON CONFLICT (domain) DO NOTHING`, kind, domain, name, country, site, source, fetch, at)
				if err != nil {
					return fmt.Errorf("leads transfer: insert orgs: %w", err)
				}
				t.added += int(tag.RowsAffected())
				t.known += len(domain) - int(tag.RowsAffected())
			}
			return scanInto(ctx, tx, "orgs", im.orgs, `SELECT domain, id FROM org WHERE domain = ANY($1)`, all)
		})
	})
}

// dropFetches lets the fetch ids go once the last kind that points at a fetch is in.
func (im *importer) dropFetches(context.Context) error {
	im.fetches = nil
	return nil
}

// orgID is the server id of the org on domain.
func (im *importer) orgID(domain string) (int64, error) {
	id, ok := im.orgs[domain]
	if !ok {
		return 0, errors.New("leads transfer: a row's org is not in the transfer")
	}
	return id, nil
}

func (im *importer) importRoutes(ctx context.Context) error {
	t := im.tallies[tfRoutes]
	return eachBatch(im.dir, tfRoutes, nil, 0, func(batch []tRoute) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			have, err := matchIDs(ctx, tx, "routes", matchRoutesSQL, routeArgs(batch)...)
			if err != nil {
				return err
			}
			var org, fetch []int64
			var kind, value, derived []string
			var mx []bool
			var at []time.Time
			var fresh []tRoute
			for i, r := range batch {
				if id, ok := have[int64(i+1)]; ok {
					im.routes[r.key()] = id
					t.known++
					continue
				}
				o, err := im.orgID(r.Org)
				if err != nil {
					return err
				}
				f, err := im.fetchID(r.Fetch)
				if err != nil {
					return err
				}
				fresh = append(fresh, r)
				org, fetch, kind, value = append(org, o), append(fetch, f), append(kind, r.Kind), append(value, r.Value)
				derived, mx, at = append(derived, r.DerivedBy), append(mx, r.MXOK), append(at, r.FoundAt)
			}
			if len(fresh) == 0 {
				return nil
			}
			tag, err := tx.Exec(ctx, `INSERT INTO contact_route (org_id, kind, value, fetch_id, mx_ok, derived_by, found_at)
				SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::bigint[], $5::bool[], $6::text[], $7::timestamptz[])
				ON CONFLICT (org_id, kind, value) DO NOTHING`, org, kind, value, fetch, mx, derived, at)
			if err != nil {
				return fmt.Errorf("leads transfer: insert routes: %w", err)
			}
			t.added += int(tag.RowsAffected())
			t.known += len(fresh) - int(tag.RowsAffected())
			got, err := matchIDs(ctx, tx, "routes", matchRoutesSQL, routeArgs(fresh)...)
			if err != nil {
				return err
			}
			for i, r := range fresh {
				if id, ok := got[int64(i+1)]; ok {
					im.routes[r.key()] = id
				}
			}
			return nil
		})
	})
}

func (im *importer) importFindings(ctx context.Context) error {
	t := im.tallies[tfFindings]
	return eachBatch(im.dir, tfFindings, nil, 0, func(batch []tFinding) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			have, err := matchIDs(ctx, tx, "findings", matchFindingsSQL, findingArgs(batch)...)
			if err != nil {
				return err
			}
			var org, fetch []int64
			var url, phrase, derived []string
			var at []time.Time
			var fresh []tFinding
			seen := map[string]bool{}
			for i, f := range batch {
				if id, ok := have[int64(i+1)]; ok {
					im.findings[f.key()] = id
					t.known++
					continue
				}
				if seen[f.key()] {
					t.known++
					continue
				}
				seen[f.key()] = true
				o, err := im.orgID(f.Org)
				if err != nil {
					return err
				}
				fid, err := im.fetchID(f.Fetch)
				if err != nil {
					return err
				}
				fresh = append(fresh, f)
				org, fetch, url, phrase = append(org, o), append(fetch, fid), append(url, f.URL), append(phrase, f.Phrase)
				derived, at = append(derived, f.DerivedBy), append(at, f.CreatedAt)
			}
			if len(fresh) == 0 {
				return nil
			}
			if _, err := tx.Exec(ctx, `INSERT INTO agency_finding (org_id, url, phrase, fetch_id, derived_by, created_at)
				SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::bigint[], $5::text[], $6::timestamptz[])`,
				org, url, phrase, fetch, derived, at); err != nil {
				return fmt.Errorf("leads transfer: insert findings: %w", err)
			}
			t.added += len(fresh)
			got, err := matchIDs(ctx, tx, "findings", matchFindingsSQL, findingArgs(fresh)...)
			if err != nil {
				return err
			}
			for i, f := range fresh {
				if id, ok := got[int64(i+1)]; ok {
					im.findings[f.key()] = id
				}
			}
			return nil
		})
	})
}

func (im *importer) importAttempts(ctx context.Context) error {
	t := im.tallies[tfAttempts]
	return eachBatch(im.dir, tfAttempts, nil, 0, func(batch []tAttempt) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			d, at := make([]string, len(batch)), make([]time.Time, len(batch))
			for i, a := range batch {
				d[i], at[i] = a.Org, a.AttemptedAt
			}
			have, err := matchIDs(ctx, tx, "attempts", matchAttemptsSQL, d, at)
			if err != nil {
				return err
			}
			var org []int64
			var found []int
			var when []time.Time
			seen := map[string]bool{}
			for i, a := range batch {
				k := a.Org + "\x00" + microKey(a.AttemptedAt)
				if _, ok := have[int64(i+1)]; ok || seen[k] {
					t.known++
					continue
				}
				seen[k] = true
				o, err := im.orgID(a.Org)
				if err != nil {
					return err
				}
				org, found, when = append(org, o), append(found, a.RoutesFound), append(when, a.AttemptedAt)
			}
			if len(org) == 0 {
				return nil
			}
			runID, err := im.run(ctx, tx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO contact_attempt (org_id, run_id, routes_found, attempted_at)
				SELECT t.o, $1, t.n, t.at FROM unnest($2::bigint[], $3::int[], $4::timestamptz[]) AS t(o, n, at)`,
				runID, org, found, when); err != nil {
				return fmt.Errorf("leads transfer: insert attempts: %w", err)
			}
			t.added += len(org)
			return nil
		})
	})
}

func (im *importer) importLeads(ctx context.Context) error {
	t := im.tallies[tfLeads]
	return eachBatch(im.dir, tfLeads, nil, 0, func(batch []tLead) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			all := make([]string, len(batch))
			for i, l := range batch {
				all[i] = l.Org
			}
			have, err := matchIDs(ctx, tx, "leads", matchLeadsSQL, all)
			if err != nil {
				return err
			}
			// The batch's probe references, matched by key in one query.
			var probeKeys []tProbe
			for _, l := range batch {
				if l.Probe != nil {
					probeKeys = append(probeKeys, tProbe{tProbeKey: *l.Probe})
				}
			}
			probeIDs := map[string]int64{}
			if len(probeKeys) > 0 {
				got, err := matchIDs(ctx, tx, "lead probes", matchProbesSQL, probeArgs(probeKeys)...)
				if err != nil {
					return err
				}
				for i, p := range probeKeys {
					if id, ok := got[int64(i+1)]; ok {
						probeIDs[p.key()] = id
					}
				}
			}
			var org []int64
			var probe, finding, route []*int64
			var kind, channel, state []string
			var stateAt, created []time.Time
			for i, l := range batch {
				if _, ok := have[int64(i+1)]; ok {
					t.known++
					continue
				}
				o, err := im.orgID(l.Org)
				if err != nil {
					return err
				}
				var p, f, r *int64
				if l.Probe != nil {
					id, ok := probeIDs[l.Probe.key()]
					if !ok {
						return errors.New("leads transfer: a lead's probe is not in the transfer")
					}
					p = &id
				}
				if l.Finding != nil {
					id, ok := im.findings[l.Finding.key()]
					if !ok {
						return errors.New("leads transfer: a lead's finding is not in the transfer")
					}
					f = &id
				}
				if l.Route != nil {
					id, ok := im.routes[l.Route.key()]
					if !ok {
						return errors.New("leads transfer: a lead's route is not in the transfer")
					}
					r = &id
				}
				org, probe, finding, route = append(org, o), append(probe, p), append(finding, f), append(route, r)
				kind, channel, state = append(kind, l.Kind), append(channel, l.Channel), append(state, l.State)
				stateAt, created = append(stateAt, l.StateAt), append(created, l.CreatedAt)
			}
			if len(org) > 0 {
				tag, err := tx.Exec(ctx, `INSERT INTO lead (org_id, kind, probe_id, agency_finding_id, channel, route_id, state, state_at, created_at)
					SELECT * FROM unnest($1::bigint[], $2::text[], $3::bigint[], $4::bigint[], $5::text[], $6::bigint[], $7::text[], $8::timestamptz[], $9::timestamptz[])
					ON CONFLICT (org_id) DO NOTHING`, org, kind, probe, finding, channel, route, state, stateAt, created)
				if err != nil {
					return fmt.Errorf("leads transfer: insert leads: %w", err)
				}
				t.added += int(tag.RowsAffected())
				t.known += len(org) - int(tag.RowsAffected())
			}
			return scanInto(ctx, tx, "leads", im.leads,
				`SELECT o.domain, l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = ANY($1)`, all)
		})
	})
}

// importEvents writes the lead history. lead_event has no index on its lead, so the match
// reads that table; it holds one row per state a lead entered, a small table.
func (im *importer) importEvents(ctx context.Context) error {
	t := im.tallies[tfEvents]
	seen := map[string]bool{}
	return eachBatch(im.dir, tfEvents, nil, 0, func(batch []tEvent) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			n := len(batch)
			lid, at, st := make([]int64, n), make([]time.Time, n), make([]string, n)
			for i, e := range batch {
				id, ok := im.leads[e.Lead]
				if !ok {
					return errors.New("leads transfer: an event's lead is not in the transfer")
				}
				lid[i], at[i], st[i] = id, e.At, e.State
			}
			have, err := matchIDs(ctx, tx, "events", matchEventsSQL, lid, at, st)
			if err != nil {
				return err
			}
			var l []int64
			var state, reason, actor []string
			var when []time.Time
			for i, e := range batch {
				k := e.Lead + "\x00" + microKey(e.At) + "\x00" + e.State
				if _, ok := have[int64(i+1)]; ok || seen[k] {
					t.known++
					continue
				}
				seen[k] = true
				l, state, reason, actor, when = append(l, lid[i]), append(state, e.State), append(reason, e.Reason),
					append(actor, e.Actor), append(when, e.At)
			}
			if len(l) == 0 {
				return nil
			}
			if _, err := tx.Exec(ctx, `INSERT INTO lead_event (lead_id, state, reason, actor, at)
				SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::text[], $5::timestamptz[])`,
				l, state, reason, actor, when); err != nil {
				return fmt.Errorf("leads transfer: insert events: %w", err)
			}
			t.added += len(l)
			return nil
		})
	})
}

func (im *importer) importSuppressions(ctx context.Context) error {
	t := im.tallies[tfSuppressions]
	seen := map[string]bool{}
	return eachBatch(im.dir, tfSuppressions, nil, 0, func(batch []tSuppression) error {
		return im.tx(ctx, func(tx pgx.Tx) error {
			n := len(batch)
			k, v, r := make([]string, n), make([]string, n), make([]string, n)
			for i, s := range batch {
				k[i], v[i], r[i] = s.Kind, s.Value, s.Reason
			}
			have, err := matchIDs(ctx, tx, "suppressions", matchSuppressionsSQL, k, v, r)
			if err != nil {
				return err
			}
			var kind, value, reason, source []string
			var at []time.Time
			for i, s := range batch {
				// One row per kind, value and reason, as Suppress records them.
				key := s.Kind + "\x00" + s.Value + "\x00" + s.Reason
				if _, ok := have[int64(i+1)]; ok || seen[key] {
					t.known++
					continue
				}
				seen[key] = true
				kind, value, reason, source, at = append(kind, s.Kind), append(value, s.Value), append(reason, s.Reason),
					append(source, s.Source), append(at, s.CreatedAt)
			}
			if len(kind) == 0 {
				return nil
			}
			if _, err := tx.Exec(ctx, `INSERT INTO suppression (kind, value, reason, source, created_at)
				SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::timestamptz[])`,
				kind, value, reason, source, at); err != nil {
				return fmt.Errorf("leads transfer: insert suppressions: %w", err)
			}
			t.added += len(kind)
			return nil
		})
	})
}

func (im *importer) print(w io.Writer) {
	run := "nothing new, no run row"
	if im.runID != 0 {
		run = fmt.Sprintf("run %d", im.runID)
	}
	_, _ = fmt.Fprintf(w, "leads transfer import (%s):\n", run)
	for _, name := range transferFiles {
		t := im.tallies[name]
		known := "already there"
		if name == tfFetches {
			known = "reused"
		}
		line := fmt.Sprintf("  %s: %d added · %d %s", strings.TrimSuffix(name, ".jsonl"), t.added, t.known, known)
		if name == tfSites {
			line += fmt.Sprintf(" · %d pointed at their last probe", im.pointers)
		}
		_, _ = fmt.Fprintln(w, line)
	}
}
