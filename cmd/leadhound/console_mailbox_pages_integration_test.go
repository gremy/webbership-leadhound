//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// mailboxRows is every row a mailbox write can make, for the console↔CLI contract: each
// mailbox with its fields and whether its sealed password opens with the test key to
// testPassword for exactly its stored connection fields (sealing is randomised, so the bytes
// never compare), every state row and every check, and the mailbox-check request file in
// dir.
func mailboxRows(t *testing.T, dir string) []string {
	t.Helper()
	ctx := context.Background()
	key := currentTestKey(t)
	conn := storetest.OwnerConn(t)
	var out []string
	rows, err := conn.Query(ctx, `SELECT id, address, domain, login, smtp_addr, imap_addr, dkim_selector, from_name, password_sealed, sealed_by
		FROM sender_mailbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m leadstore.Mailbox
		if err := rows.Scan(&m.ID, &m.Address, &m.Domain, &m.Login, &m.SMTPAddr, &m.IMAPAddr, &m.DKIMSelector, &m.FromName,
			&m.PasswordSealed, &m.SealedBy); err != nil {
			t.Fatal(err)
		}
		c, err := secretbox.Open(key, m.PasswordSealed)
		opens := err == nil && c.Matches(m.Address, m.Login, m.SMTPAddr, m.IMAPAddr) && c.Password.Reveal() == testPassword
		out = append(out, fmt.Sprintf("mailbox %d %s %s %s %s %s %s %q sealed by %s opens %v",
			m.ID, m.Address, m.Domain, m.Login, m.SMTPAddr, m.IMAPAddr, m.DKIMSelector, m.FromName, m.SealedBy, opens))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT format('state %s %s [%s] %s %s', mailbox_id, state, reason, scope, actor) FROM mailbox_state ORDER BY id`,
		`SELECT format('check %s %s %s %s', mailbox_id, actor, outcome, CASE WHEN checked_at IS NULL THEN 'waiting' ELSE 'checked' END)
		   FROM mailbox_check ORDER BY id`,
	} {
		r, err := conn.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		r.Close()
	}
	req, err := os.ReadFile(filepath.Join(dir, runRequestDir, mailboxCheckAction)) // #nosec G304 -- the test's data dir
	if err != nil {
		req = []byte("none")
	}
	return append(out, "request file "+strings.TrimSpace(string(req)))
}

// allowReservedNames lets a test's reserved mailbox names (sender.example) be added, as
// only tests do.
func allowReservedNames(t *testing.T) {
	t.Helper()
	saved := leadsMail.allowReserved
	leadsMail.allowReserved = true
	t.Cleanup(func() { leadsMail.allowReserved = saved })
}

// sealWithTestKey points the public key the console and the CLI seal with at the test key.
func sealWithTestKey(t *testing.T) {
	t.Helper()
	t.Setenv(secretbox.PublicKeyEnv, currentTestKey(t).Public().String())
}

// consoleClient drives the console handler over b as a signed-in operator, as a browser
// does: it keeps the cookies the console sets (the one-time flash) and follows each redirect
// after a POST. It keeps every page it got back, with its Set-Cookie lines.
type consoleClient struct {
	h       http.Handler
	session string
	jar     map[string]string
	pages   []string
}

func newConsoleClient(t *testing.T, b *backend, logger *slog.Logger) *consoleClient {
	t.Helper()
	a, err := console.NewAuth("operator@mail.example", testHash(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return &consoleClient{h: console.New(b, a, console.Options{Dev: true, Location: b.loc, Logger: logger}), session: token, jar: map[string]string{}}
}

func (c *consoleClient) do(r *http.Request) *httptest.ResponseRecorder {
	r.AddCookie(&http.Cookie{Name: "lh_session", Value: c.session}) // #nosec G124 -- a test request's cookie, never set on a response
	for k, v := range c.jar {
		r.AddCookie(&http.Cookie{Name: k, Value: v}) // #nosec G124 -- a test request's cookie, never set on a response
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.jar, ck.Name)
		} else {
			c.jar[ck.Name] = ck.Value
		}
	}
	c.pages = append(c.pages, strings.Join(w.Header().Values("Set-Cookie"), "\n")+"\n"+w.Body.String())
	return w
}

func (c *consoleClient) get(path string, hx bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if hx {
		r.Header.Set("HX-Request", "true")
	}
	return c.do(r)
}

// post submits form to path; after a redirect it reads the page redirected to (kept in
// pages), without its fragment as a browser does, and returns the POST's own answer.
func (c *consoleClient) post(path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := c.do(r)
	if w.Code == http.StatusSeeOther {
		// A browser never sends the fragment (#your-call): it scrolls to it.
		next, _, _ := strings.Cut(w.Header().Get("Location"), "#")
		c.get(next, false)
	}
	return w
}

// lockedBuffer is a log sink the console's goroutines may share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs sends slog's default logger to a buffer for the test, and returns it with a
// logger over it.
func captureLogs(t *testing.T) (*lockedBuffer, *slog.Logger) {
	t.Helper()
	buf := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	saved := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(saved) })
	return buf, logger
}

// mailboxForm is the add or edit form as the operator fills it, reading at
// imap.other.example.
func mailboxForm(address, smtp, selector, fromName, password string) url.Values {
	f := url.Values{"smtp_addr": {smtp}, "imap_addr": {"imap.other.example:993"}, "selector": {selector}, "from_name": {fromName}, "password": {password}}
	if address != "" {
		f.Set("address", address)
	}
	return f
}

// TestConsoleMailboxPages: the Mailboxes pages through the real backend over the stored
// mailboxes. The list groups by registrable domain with its limits and today's records per
// host; an add seals the password with the public key, requests the first check, writes the
// request file and lands on the new mailbox, whose card polls while the check waits; a
// refusal comes back as the form without the password; pause, resume and the domain pause
// write their rows; and without a public key the page says so and an add is refused, while
// everything else still works.
func TestConsoleMailboxPages(t *testing.T) {
	dir := leadsScene(t)
	allowReservedNames(t)
	sealWithTestKey(t)
	logs, logger := captureLogs(t)
	c := newConsoleClient(t, leadsBackend(t), logger)

	w := c.get("/mailboxes", false)
	page := w.Body.String()
	for _, want := range []string{testSenderDomain, testUser, `Sent today</span><span class="stat-value">0 of 50</span>`, `Mailboxes</span><span class="stat-value">1 of 2</span>`,
		"Records for " + testSenderDomain + " (selector sel1), checked", "Ready", `href="/mailboxes/new"`} {
		if w.Code != http.StatusOK || !strings.Contains(page, want) {
			t.Fatalf("mailboxes page %d lacks %q:\n%s", w.Code, want, page)
		}
	}
	if w := c.get("/mailboxes/new?preset=Spacemail", false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "mail.spacemail.com:465") {
		t.Errorf("preset form %d:\n%s", w.Code, w.Body.String())
	}

	// A refused add: the form comes back with the fields and never the password.
	w = c.post("/mailboxes", mailboxForm("not an address", "smtp.other.example:465", "s2", "Desk", testPassword))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "is not one mailbox address") ||
		!strings.Contains(w.Body.String(), "smtp.other.example:465") {
		t.Errorf("refused add %d:\n%s", w.Code, w.Body.String())
	}

	w = c.post("/mailboxes", mailboxForm("Desk@Other.example", "smtp.other.example:465", "s2", "Front Desk", testPassword))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/mailboxes/2" {
		t.Fatalf("add: %d to %q:\n%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	rows := strings.Join(mailboxRows(t, dir), "\n")
	for _, want := range []string{
		`mailbox 2 desk@other.example other.example desk@other.example smtp.other.example:465 imap.other.example:993 s2 "Front Desk" sealed by ` +
			secretbox.Fingerprint(currentTestKey(t).Public()) + " opens true",
		"check 2 operator requested waiting", "request file 2026-10-27T",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("rows lack %q:\n%s", want, rows)
		}
	}
	w = c.get("/mailboxes/2", false)
	if page := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(page, `hx-trigger="every 5s"`) || !strings.Contains(page, "Checking…") ||
		!strings.Contains(page, "Front Desk") {
		t.Errorf("new mailbox page %d:\n%s", w.Code, page)
	}
	if w := c.get("/mailboxes/2/check", true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `hx-trigger="every 5s"`) {
		t.Errorf("check card %d:\n%s", w.Code, w.Body.String())
	}
	if w := c.get("/mailboxes/9", false); w.Code != http.StatusNotFound {
		t.Errorf("no such mailbox: %d", w.Code)
	}

	// A change of server without the password is refused; the form keeps what was typed.
	w = c.post("/mailboxes/2", mailboxForm("", "smtp2.other.example:587", "s2", "Front Desk", ""))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "needs the password again") ||
		!strings.Contains(w.Body.String(), "smtp2.other.example:587") {
		t.Errorf("edit without the password %d:\n%s", w.Code, w.Body.String())
	}

	if w := c.post("/mailboxes/2/pause", url.Values{"reason": {"warming up"}}); w.Code != http.StatusSeeOther {
		t.Errorf("pause %d", w.Code)
	}
	if page := c.get("/mailboxes/2", false).Body.String(); !strings.Contains(page, "Paused: warming up") || !strings.Contains(page, "Resume this mailbox") {
		t.Errorf("paused mailbox page:\n%s", page)
	}
	if w := c.post("/mailboxes/2/resume", nil); w.Code != http.StatusSeeOther {
		t.Errorf("resume %d", w.Code)
	}
	if w := c.post("/mailboxes/domains/"+testSenderDomain+"/pause", url.Values{"reason": {"blocklisted"}}); w.Code != http.StatusSeeOther {
		t.Errorf("domain pause %d", w.Code)
	}
	if page := c.get("/mailboxes", false).Body.String(); !strings.Contains(page, "This domain is paused: blocklisted") ||
		!strings.Contains(page, `action="/mailboxes/domains/`+testSenderDomain+`/resume"`) {
		t.Errorf("paused domain:\n%s", page)
	}
	if page := c.get("/mailboxes/1", false).Body.String(); !strings.Contains(page, "Paused with its whole domain") {
		t.Errorf("a mailbox paused with its domain:\n%s", page)
	}
	if w := c.post("/mailboxes/2/retire", nil); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Confirm to retire it") {
		t.Errorf("retire without the box %d", w.Code)
	}
	if w := c.post("/mailboxes/2/retire", url.Values{"confirm": {"yes"}}); w.Code != http.StatusSeeOther {
		t.Errorf("retire %d", w.Code)
	}
	rows = strings.Join(mailboxRows(t, dir), "\n")
	for _, want := range []string{"state 2 paused [warming up] mailbox operator", "state 2 active [] mailbox operator",
		"state 1 paused [blocklisted] domain operator", "state 2 retired [] mailbox operator"} {
		if !strings.Contains(rows, want) {
			t.Errorf("rows lack %q:\n%s", want, rows)
		}
	}

	// The Leads page shows each mailbox not retired, with its domain and its records.
	page = c.get("/leads/outreach", false).Body.String()
	if !strings.Contains(page, testUser) || !strings.Contains(page, "Paused: blocklisted") || strings.Contains(page, "desk@other.example") {
		t.Errorf("leads page mailboxes:\n%s", page)
	}

	// No public key: the page says so, an add is refused with the plain words, and the
	// rest still reads.
	t.Setenv(secretbox.PublicKeyEnv, "")
	c = newConsoleClient(t, leadsBackend(t), logger)
	if page := c.get("/mailboxes", false).Body.String(); !strings.Contains(page, "the mailbox key is not set up") || strings.Contains(page, `href="/mailboxes/new"`) {
		t.Errorf("mailboxes without a key:\n%s", page)
	}
	w = c.post("/mailboxes", mailboxForm("new@other.example", "smtp.other.example:465", "s2", "", testPassword))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), secretbox.PublicKeyEnv+" is not set") {
		t.Errorf("add without a key %d:\n%s", w.Code, w.Body.String())
	}
	if w := c.get("/mailboxes/1", false); w.Code != http.StatusOK {
		t.Errorf("mailbox page without a key %d", w.Code)
	}
	if strings.Contains(logs.String(), testPassword) || strings.Contains(strings.Join(c.pages, ""), testPassword) {
		t.Error("the password reached a log line or a page")
	}
}

// TestConsoleMailboxesSendingRows: the Leads page's sending card lists every mailbox not
// retired, grouped by domain, each with today's sends against its ramp limit, its state and
// its own host's records today; and the batch page shows each email's From as frozen on the
// message when it was drafted.
func TestConsoleMailboxesSendingRows(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	lq := leadstoretest.Q(t)
	desk := seedMailbox(t, lq, "desk@a.other.example", withSelector("s2"))
	gone := seedMailbox(t, lq, "gone@zz.example")
	if err := lq.SetMailboxState(ctx, gone.ID, leadstore.MailboxRetired, "", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	b := leadsBackend(t)
	p, err := b.Leads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Sending.Mailboxes
	if len(got) != 2 || got[0].Address != desk.Address || got[1].Address != testUser {
		t.Fatalf("sending rows %+v, want desk (other.example) then %s, never the retired one", got, testUser)
	}
	if r := got[0]; r.Domain != "other.example" || r.State != console.MailboxNeedsCheck || r.CapToday != 10 || r.DNS != nil {
		t.Errorf("desk row %+v: want other.example, needs a check, cap 10, no records today", r)
	}
	if r := got[1]; r.Domain != testSenderDomain || r.State != console.MailboxReady || r.CapToday != 10 || r.DNS == nil || !r.DNS.DKIM {
		t.Errorf("%s row %+v: want ready, cap 10, records passed", testUser, r)
	}

	mustRun(t, "leads", "batch", "new")
	h, cookie := jobsHandler(t, b)
	r := httptest.NewRequest(http.MethodGet, "/leads/batches/1", nil)
	r.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	page := w.Body.String()
	// Each email shows the From of its own mailbox, frozen on the message (a shadow batch
	// spreads its leads over the mailboxes too).
	for _, m := range []leadstore.Mailbox{{ID: 1, Address: testUser}, desk} {
		emails := batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM outreach_message WHERE batch_id = 1 AND channel = 'email' AND mailbox_id = %d`, m.ID))
		from := "From: Demo Sender &lt;" + m.Address + "&gt;"
		if got := strings.Count(page, from); got != emails {
			t.Errorf("batch page shows %q %d times, want once per email of mailbox %d (%d)", from, got, m.ID, emails)
		}
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE batch_id = 1 AND channel = 'email'`); n == 0 || strings.Count(page, "From: ") != n {
		t.Errorf("%d From lines for %d emails:\n%s", strings.Count(page, "From: "), n, page)
	}
}

// TestMailboxPasswordNeverLeaks (MB-R12): one fixed test password goes through a console
// add, a CLI add, a console edit, a check, a shadow and a live batch, a send to the fake
// SMTP server and a sync; it then appears in no column of any table (every column, bytea
// searched for its bytes, the rest as text), no log line, no rendered console page, no
// command output and no file under the data dir (the outbox and the request files). Only
// password_sealed holds it, sealed, and every one of those rows opens to it with the test
// key, so the password really flowed.
func TestMailboxPasswordNeverLeaks(t *testing.T) {
	ctx := context.Background()
	w := newSendWorld(t, true, 20)
	sealWithTestKey(t)
	for _, host := range []string{"other.example", "third.example"} {
		w.dns[host] = []string{"v=spf1 -all"}
		w.dns["sel1._domainkey."+host] = []string{"v=DKIM1; k=rsa; p=MIIB"}
		w.dns["_dmarc."+host] = []string{"v=DMARC1; p=none"}
	}
	logs, logger := captureLogs(t)
	c := newConsoleClient(t, leadsBackend(t), logger)
	saved := mailboxPasswordInput
	t.Cleanup(func() { mailboxPasswordInput = saved })

	// The console's add, its refused edit and its edit, each with the password.
	if r := c.post("/mailboxes", mailboxForm("desk@other.example", "smtp.other.example:465", "sel1", "Desk",
		testPassword)); r.Code != http.StatusSeeOther {
		t.Fatalf("console add %d:\n%s", r.Code, r.Body.String())
	}
	c.post("/mailboxes/2", mailboxForm("", "bad server", "sel1", "Desk", testPassword))
	if r := c.post("/mailboxes/2", mailboxForm("", "smtp.other.example:587", "sel1", "Desk", testPassword)); r.Code != http.StatusSeeOther {
		t.Fatalf("console edit %d:\n%s", r.Code, r.Body.String())
	}
	// The CLI's add, the password on stdin.
	mailboxPasswordInput = stdinFile(t, testPassword+"\n")
	w.mustRun(t, "leads", "mailboxes", "add", "--address", "cli@third.example", "--smtp", "smtp.third.example:465", "--imap", "imap.third.example:993",
		"--selector", "sel1")
	w.mustRun(t, "leads", "mailboxes", "check", "--requested")
	w.mustRun(t, "leads", "mailboxes", "list")
	for _, path := range []string{"/mailboxes", "/mailboxes/2", "/mailboxes/2/edit", "/mailboxes/3", "/leads/outreach", "/leads"} {
		c.get(path, false)
	}
	c.get("/mailboxes/2/check", true)

	// A shadow batch and a live one, sent to the fake server, then a sync.
	w.batch(t, true, map[int64]string{w.lead(t, "bravo.example"): "bravo.example"})
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})
	w.mustRun(t, "leads", "send")
	w.run(t, "leads", "sync")
	if w.smtp.count() == 0 {
		t.Fatal("nothing was sent: the sweep would prove little")
	}
	c.get("/leads/batches/1", false)
	c.get("/leads/batches/2", false)

	conn := storetest.OwnerConn(t)
	cols, err := conn.Query(ctx, `SELECT table_schema, table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema') AND table_name NOT LIKE 'pg\_%'
		  AND table_name IN (SELECT table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE')`)
	if err != nil {
		t.Fatal(err)
	}
	type column struct{ schema, table, name, kind string }
	var all []column
	for cols.Next() {
		var col column
		if err := cols.Scan(&col.schema, &col.table, &col.name, &col.kind); err != nil {
			t.Fatal(err)
		}
		all = append(all, col)
	}
	cols.Close()
	if len(all) < 100 {
		t.Fatalf("only %d columns found", len(all))
	}
	sealedCols := 0
	for _, col := range all {
		ref := pgx.Identifier{col.schema, col.table}.Sanitize()
		name := pgx.Identifier{col.name}.Sanitize()
		match := name + `::text LIKE '%' || $1 || '%'`
		if col.kind == "bytea" {
			match = `position(convert_to($1, 'UTF8') in ` + name + `) > 0`
		}
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+ref+` WHERE `+match, testPassword).Scan(&n); err != nil {
			t.Fatalf("%s.%s: %v", col.table, col.name, err)
		}
		if n != 0 {
			t.Errorf("%s.%s holds the password in %d rows", col.table, col.name, n)
		}
		if col.table == "sender_mailbox" && col.name == "password_sealed" {
			sealedCols++
		}
	}
	if sealedCols != 1 {
		t.Errorf("password_sealed found %d times among the columns", sealedCols)
	}
	rows := mailboxRows(t, w.dir)
	opened := 0
	for _, r := range rows {
		if strings.HasPrefix(r, "mailbox ") {
			if !strings.HasSuffix(r, " opens true") {
				t.Errorf("a sealed password does not open to the test password: %s", r)
			}
			opened++
		}
	}
	if opened != 3 {
		t.Errorf("%d mailboxes, want 3:\n%s", opened, strings.Join(rows, "\n"))
	}

	if strings.Contains(logs.String(), testPassword) {
		t.Errorf("a log line holds the password:\n%s", logs.String())
	}
	if len(c.pages) < 11 {
		t.Errorf("only %d pages rendered", len(c.pages))
	}
	for i, pg := range c.pages {
		if strings.Contains(pg, testPassword) {
			t.Errorf("rendered page %d holds the password:\n%s", i, pg)
		}
	}
	files := 0
	if err := filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 G122 -- the test's own temp data dir
		if err != nil {
			return err
		}
		files++
		if bytes.Contains(b, []byte(testPassword)) {
			t.Errorf("%s holds the password", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Error("no file under the data dir to sweep")
	}
	if strings.Contains(strings.Join(w.outs, "\n"), testPassword) {
		t.Errorf("a command's output holds the password:\n%s", strings.Join(w.outs, "\n"))
	}
}

// TestConsoleMailboxLongPassword: a password far longer than any other field (600
// characters) is taken whole: the add form's password field has no length limit, and the
// console's add seals exactly what was typed, so the seal opens to the full password.
func TestConsoleMailboxLongPassword(t *testing.T) {
	leadsScene(t)
	allowReservedNames(t)
	sealWithTestKey(t)
	_, logger := captureLogs(t)
	c := newConsoleClient(t, leadsBackend(t), logger)
	page := c.get("/mailboxes/new", false).Body.String()
	i := strings.Index(page, `<input class="input" id="mb-password"`)
	if i < 0 {
		t.Fatalf("no password field:\n%s", page)
	}
	if field := page[i : i+strings.Index(page[i:], ">")]; strings.Contains(field, "maxlength") {
		t.Errorf("the password field is capped: %s", field)
	}
	long := strings.Repeat("pw-0123456789-", 43)[:600]
	if w := c.post("/mailboxes", mailboxForm("desk@other.example", "smtp.other.example:465", "s2", "Desk", long)); w.Code != http.StatusSeeOther {
		t.Fatalf("add %d:\n%s", w.Code, w.Body.String())
	}
	var sealed []byte
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT password_sealed FROM sender_mailbox WHERE address = 'desk@other.example'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	creds, err := secretbox.Open(currentTestKey(t), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Password.Reveal(); got != long {
		t.Errorf("the seal opens to %d characters, want the 600 typed", len(got))
	}
}

// TestConsoleMailboxCheckNotStarted: a requested check that has waited more than
// console.CheckNotStartedAfter is shown as not started (the card stops polling and offers
// Check now); a fresh request is still checking.
func TestConsoleMailboxCheckNotStarted(t *testing.T) {
	leadsScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	m := seedMailbox(t, lq, "late@"+testSenderDomain)
	if _, err := lq.RequestCheck(ctx, m.ID, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	b := leadsBackend(t)
	card, err := b.MailboxCheckCard(ctx, m.ID)
	if err != nil || card.Outcome != console.MailboxCheckRequested || card.NotStarted {
		t.Fatalf("a fresh request: %+v, %v", card, err)
	}
	ownerExec(t, `UPDATE mailbox_check SET requested_at = now() - interval '6 minutes' WHERE mailbox_id = $1`, m.ID)
	card, err = b.MailboxCheckCard(ctx, m.ID)
	if err != nil || !card.NotStarted {
		t.Errorf("a request 6 minutes old: %+v, %v", card, err)
	}
	page, err := b.Mailbox(ctx, m.ID)
	if err != nil || !page.Check.NotStarted {
		t.Errorf("the mailbox page's card: %+v, %v", page.Check, err)
	}
}
