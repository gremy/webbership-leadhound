package console

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// staticFS holds the console's CSS, favicon, robots.txt and htmx.
// static/htmx-2.0.11.min.js is https://unpkg.com/htmx.org@2.0.11/dist/htmx.min.js,
// SHA-256 d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717.
// It is served from here only, never from a CDN.
//
//go:embed static
var staticFS embed.FS

const (
	// maxBody caps every POST body.
	maxBody = 64 << 10

	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
		"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

	loginFailed  = "That email and password don't match."
	signedOut    = "Signed out. Sign in again."
	tooMany      = "Too many sign-in attempts. Wait a minute and try again."
	tooLarge     = "The form is too large."
	serverFailed = "Something went wrong. The details are in the server log."
	noBriefYet   = "No brief has been sent yet."
)

// cssVersion is the start of console.css's SHA-256. The page links the
// stylesheet with it as a query, so a new stylesheet is fetched at once although
// static files are cached for a day.
var cssVersion = staticVersion("static/console.css")

// jsVersion is console.js's version pin, made the same way as cssVersion.
var jsVersion = staticVersion("static/console.js")

// staticVersion is the start of the embedded file's SHA-256.
func staticVersion(name string) string {
	b, err := fs.ReadFile(staticFS, name)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// route is one entry of the console's routing table. New builds the mux from
// routes, and the tests sweep the same table.
type route struct {
	pattern string // ServeMux pattern
	sample  string // a concrete path that reaches the route
	public  bool   // reachable without a session
	handle  func(*server, http.ResponseWriter, *http.Request)
}

// routes is every route the console serves. Each route that is not public is
// wrapped in requireSession.
var routes = []route{
	{"GET /login", "/login", true, (*server).loginPage},
	{"POST /login", "/login", true, (*server).login},
	{"POST /logout", "/logout", false, (*server).logout},
	{"POST /theme", "/theme", true, (*server).setTheme},
	{"GET /{$}", "/", false, (*server).dashboard},
	{"GET /leads/home", "/leads/home", false, (*server).leadsHome},
	{"GET /jobs/home", "/jobs/home", false, (*server).jobsHome},
	{"GET /account", "/account", false, (*server).account},
	{"GET /controls", "/controls", false, (*server).controls},
	{"GET /status", "/status", false, (*server).status},
	{"POST /status/refresh", "/status/refresh", false, (*server).refreshStatus},
	{"POST /status/claude-topup", "/status/claude-topup", false, (*server).claudeTopup},
	// Both literal /controls/... POSTs are more specific than {job}, so they win.
	{"POST /controls/send-now", "/controls/send-now", false, (*server).sendNow},
	{"POST /controls/window", "/controls/window", false, (*server).saveWindow},
	{"POST /controls/{job}", "/controls/batch", false, (*server).controlAct},
	{"GET /jobs", "/jobs", false, (*server).jobs},
	{"POST /jobs/track", "/jobs/track", false, (*server).track},
	{"GET /tracked", "/tracked", false, (*server).tracked},
	{"GET /runs", "/runs", false, (*server).runs},
	{"GET /jobs/{id}", "/jobs/42", false, (*server).job},
	{"POST /jobs/{id}/track", "/jobs/42/track", false, (*server).trackJob},
	{"GET /rules", "/rules", false, (*server).rules},
	{"POST /rules", "/rules", false, (*server).addRule},
	{"POST /rules/{id}/disable", "/rules/7/disable", false, (*server).disableRule},
	{"GET /leads", "/leads", false, (*server).leadList},
	{"GET /leads/outreach", "/leads/outreach", false, (*server).leadsOutreach},
	{"GET /leads/next", "/leads/next", false, (*server).leadsNext},
	{"GET /leads/priorities", "/leads/priorities", false, (*server).leadPrioritiesMoved},
	{"GET /leads/settings", "/leads/settings", false, (*server).leadSettings},
	{"GET /leads/campaigns", "/leads/campaigns", false, (*server).leadsCampaigns},
	{"GET /leads/today", "/leads/today", false, (*server).leadsToday},
	{"GET /leads/drafts", "/leads/drafts", false, (*server).leadsDrafts},
	{"GET /leads/calls/{message}", "/leads/calls/9", false, (*server).leadCall},
	{"GET /leads/batches/{id}", "/leads/batches/3", false, (*server).leadBatch},
	{"GET /leads/reports/{id}", "/leads/reports/5", false, (*server).leadReport},
	// Every literal /leads/... GET above is more specific than this pattern, so it wins.
	{"GET /leads/{id}", "/leads/5", false, (*server).leadDetail},
	{"POST /leads/batches/new", "/leads/batches/new", false, (*server).newBatch},
	{"POST /leads/batches/{id}/approve", "/leads/batches/3/approve", false, (*server).approveBatch},
	{"POST /leads/batches/{id}/discard", "/leads/batches/3/discard", false, (*server).discardBatch},
	{"POST /leads/messages/{id}/resolve", "/leads/messages/9/resolve", false, (*server).resolveMessage},
	{"POST /leads/items/{id}/done", "/leads/items/9/done", false, (*server).itemDone},
	{"POST /leads/suppress", "/leads/suppress", false, (*server).suppress},
	{"POST /leads/suppressions/lift", "/leads/suppressions/lift", false, (*server).liftSuppression},
	{"POST /leads/contacted", "/leads/contacted", false, (*server).contacted},
	{"POST /leads/sending", "/leads/sending", false, (*server).sending},
	{"POST /leads/campaigns/{slug}/mailboxes", "/leads/campaigns/drupal-eol/mailboxes", false, (*server).campaignMailboxes},
	{"POST /leads/settings/{section}", "/leads/settings/who", false, (*server).saveLeadSettings},
	{"POST /leads/calls/{message}/outcome", "/leads/calls/9/outcome", false, (*server).leadCallOutcome},
	{"POST /leads/drafts/{id}/review", "/leads/drafts/4/review", false, (*server).reviewDraft},
	// Every literal /leads/... POST above is more specific than this pattern, so it wins.
	{"POST /leads/{id}/{action}", "/leads/5/star", false, (*server).leadAct},
	{"GET /mailboxes", "/mailboxes", false, (*server).mailboxes},
	{"GET /mailboxes/new", "/mailboxes/new", false, (*server).newMailbox},
	{"POST /mailboxes", "/mailboxes", false, (*server).addMailbox},
	{"GET /mailboxes/{id}", "/mailboxes/1", false, (*server).mailbox},
	{"GET /mailboxes/{id}/edit", "/mailboxes/1/edit", false, (*server).editMailboxForm},
	{"POST /mailboxes/{id}", "/mailboxes/1", false, (*server).editMailbox},
	{"GET /mailboxes/{id}/check", "/mailboxes/1/check", false, (*server).mailboxCheck},
	{"POST /mailboxes/{id}/check", "/mailboxes/1/check", false, (*server).checkMailbox},
	{"POST /mailboxes/{id}/pause", "/mailboxes/1/pause", false, (*server).pauseMailbox},
	{"POST /mailboxes/{id}/resume", "/mailboxes/1/resume", false, (*server).resumeMailbox},
	{"POST /mailboxes/{id}/retire", "/mailboxes/1/retire", false, (*server).retireMailbox},
	{"POST /mailboxes/domains/{domain}/pause", "/mailboxes/domains/northwind.example/pause", false, (*server).pauseDomain},
	{"POST /mailboxes/domains/{domain}/resume", "/mailboxes/domains/northwind.example/resume", false, (*server).resumeDomain},
	{"POST /run/{action}", "/run/preview", false, (*server).runNow},
	{"GET /run/status", "/run/status", false, (*server).runStatus},
	{"GET /results", "/results", false, (*server).results},
	{"GET /static/", "/static/console.css", true, (*server).static},
	{"GET /robots.txt", "/robots.txt", true, (*server).robots},
	{"GET /favicon.ico", "/favicon.ico", true, (*server).favicon},
	{"GET /healthz", "/healthz", true, (*server).healthz},
}

// navItem is one link of a filter row (the Tracked page's status filters).
type navItem struct{ Label, Path string }

// trackedFilters are the Tracked page's status filters: "" is every job no
// longer new, "all" every job with a status, then each status.
var trackedFilters = []navItem{
	{"Active", ""}, {"All", "all"},
	{"Shortlisted", "shortlisted"}, {"Applied", "applied"}, {"Interviewing", "interviewing"},
	{"Offer", "offer"}, {"Rejected", "rejected"}, {"Skipped", "skipped"},
	{"Withdrawn", "withdrawn"}, {"New", "new"},
}

// laterStatuses are the statuses the Tracked page moves a job to.
var laterStatuses = []string{"interviewing", "offer", "rejected", "withdrawn"}

// ruleFields are the fields a skip rule may match, as the store accepts them.
var ruleFields = []string{"company", "title", "description", "location"}

// server holds what the handlers share.
type server struct {
	b         Backend
	a         *Auth
	opt       Options
	log       *slog.Logger
	cookie    string // the session cookie's name
	flash     string // the one-time message cookie's name
	device    string // the trusted-device cookie's name
	themeName string // the theme cookie's name
	pages     map[string]*template.Template
	next      map[string]bool // the pages a sign-in may return to
	nextAt    []string        // the patterns of signed-in pages with an id in their path
}

// page is what the layout renders. Product and Shell are set by render from the request's
// path, never by a handler.
type page struct {
	Title    string
	Active   string
	Product  string // "leads", "jobs" or "" (the dashboard and the gear pages)
	Shell    shellView
	SignedIn bool
	Theme    string // light or dark; "" follows the device
	Back     string // where the theme switch goes back to
	Flash    string // the flash this request carried, encoded, so the theme switch keeps it
	Message  string
	Error    bool
	Data     any
}

// rowView is one job of the Jobs page, with the result of the last action on it.
// Day is the brief's day, YYYY-MM-DD, which every form of the row posts so its
// number is read in that brief and no other.
type rowView struct {
	Job     BriefJob
	Day     string
	Message string
	Error   bool
}

// trackedView is the Tracked page's list. Total is how many jobs match, more
// than len(Rows) when the list is capped.
type trackedView struct {
	Status  string
	Filters []navItem
	Rows    []Tracked
	Total   int
	Message string
	Error   bool
}

// ruleSpot is a rule as one of the Rules page's two layouts shows it: Where keeps the
// ids of its form apart from the other layout's.
type ruleSpot struct {
	Where string
	Rule  RuleRow
}

// loginView is the sign-in form.
type loginView struct{ Next string }

// New returns the console's handler. Every response carries the security
// headers; cross-origin POSTs are refused; POST bodies are capped at 64 KiB;
// every route but the sign-in page, the theme switch, static files, robots.txt, the favicon and
// /healthz needs
// a session.
func New(b Backend, a *Auth, opt Options) http.Handler {
	if opt.Location == nil {
		opt.Location = time.UTC
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	s := &server{
		b:         b,
		a:         a,
		opt:       opt,
		log:       opt.Logger,
		cookie:    "__Host-lh_session",
		flash:     "__Host-lh_flash",
		device:    "__Host-lh_device",
		themeName: "__Host-lh_theme",
		next:      make(map[string]bool),
	}
	if opt.Dev {
		s.cookie, s.flash, s.device, s.themeName = "lh_session", "lh_flash", "lh_device", "lh_theme"
	}
	s.pages = s.parseTemplates()

	mux := http.NewServeMux()
	for _, rt := range routes {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rt.handle(s, w, r) })
		if rt.public {
			mux.Handle(rt.pattern, h)
			continue
		}
		mux.Handle(rt.pattern, s.requireSession(h))
		switch {
		case strings.HasPrefix(rt.pattern, "GET ") && strings.Contains(rt.pattern, "{"):
			s.nextAt = append(s.nextAt, strings.TrimPrefix(rt.pattern, "GET "))
		case strings.HasPrefix(rt.pattern, "GET "):
			s.next[rt.sample] = true
		}
	}
	if opt.Dev {
		mux.HandleFunc("GET /board/", board)
	}
	cop := http.NewCrossOriginProtection()
	return s.headers(cop.Handler(capBody(mux)))
}

// parseTemplates parses the layout, the job row, the Run now card, the mailbox check card,
// the Leads sub-nav and the Send now button once, then each page over a copy of them.
func (s *server) parseTemplates() map[string]*template.Template {
	base := template.Must(template.New("").Funcs(s.funcs()).ParseFS(templateFS,
		"templates/layout.html", "templates/job_row.html", "templates/run_now.html", "templates/mailbox_check.html",
		"templates/leads_tabs.html", "templates/send_now.html", "templates/home_parts.html"))
	pages := make(map[string]*template.Template)
	for _, name := range []string{"login", "dashboard", "jobs", "tracked", "runs", "rules", "job",
		"leads_list", "lead_detail", "leads_outreach", "leads_off", "lead_batch", "leads_settings", "leads_campaigns", "leads_drafts", "leads_today", "lead_call", "results",
		"mailboxes", "mailbox", "mailbox_form", "controls", "leads_home", "jobs_home", "account", "status"} {
		t := template.Must(base.Clone())
		pages[name] = template.Must(t.ParseFS(templateFS, "templates/"+name+".html"))
	}
	pages["job_row"] = base
	pages["run_now"] = base
	pages["mailbox_check"] = base
	return pages
}

// funcs are the helpers the templates use. Times are shown in Options.Location.
func (s *server) funcs() template.FuncMap {
	// format takes a time.Time or a *time.Time; a zero or nil time is "".
	format := func(layout string) func(any) string {
		return func(v any) string {
			var t time.Time
			switch v := v.(type) {
			case time.Time:
				t = v
			case *time.Time:
				if v != nil {
					t = *v
				}
			}
			if t.IsZero() {
				return ""
			}
			return t.In(s.opt.Location).Format(layout)
		}
	}
	return template.FuncMap{
		"clock": format("15:04"),
		"when":  format("Mon 2 Jan 15:04"),
		"day": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("Monday 2 January")
		},
		"thousands":    thousands,
		"health":       func(l string) string { return strings.TrimPrefix(l, "Health: ") },
		"took":         took,
		"sources":      sources,
		"costLine":     costLine,
		"callsLine":    callsLine,
		"claudeLine":   claudeLine,
		"row":          func(j BriefJob, day time.Time) rowView { return rowView{Job: j, Day: day.Format(time.DateOnly)} },
		"ruleFields":   func() []string { return ruleFields },
		"cssVersion":   func() string { return cssVersion },
		"jsVersion":    func() string { return jsVersion },
		"approveWords": approveWords,
		"maxText":      func() int { return maxLeadsText },
		"add":          func(a, b int) int { return a + b },
		"afterColon":   AfterColon,
		"join":         strings.Join,
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return "1 " + one
			}
			return strconv.Itoa(n) + " " + many
		},
		"ruleIn":           func(where string, r RuleRow) ruleSpot { return ruleSpot{Where: where, Rule: r} },
		"runCard":          func(c RunNowCard) runNowView { return runNowView{Card: c} },
		"checkCard":        func(c MailboxCheckView) mailboxCheckView { return mailboxCheckView{Card: c} },
		"mailboxState":     mailboxState,
		"maxMailbox":       func() int { return maxMailbox },
		"maxControlReason": func() int { return ControlReasonMax },
		"sendNowWords":     func() string { return SendNowWords },
		"sendNowIn": func(why any, back string) sendNowView {
			v := sendNowView{Back: back}
			switch w := why.(type) {
			case SendingWhy:
				v.Why = w
			case *SendingWhy:
				if w != nil {
					v.Why = *w
				}
			}
			return v
		},
		"controlNote":  func(job string) string { return controlNotes[job] },
		"leadsTab":     func(tab string, exact bool) leadsTabView { return leadsTabView{Tab: tab, Exact: exact} },
		"webURL":       webURL,
		"stepWhen":     stepWhen,
		"writerWords":  writerWords,
		"shortSHA":     shortSHA,
		"telHref":      telHref,
		"researchLine": func(r LeadResearch) string { return researchLine(r, s.opt.Location) },
		"noticeLines":  noticeLines,
		"researchNext": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.In(s.opt.Location).Format("2 Jan 2006")
		},
		"lineKind":    lineKindWords,
		"reasonWords": ReasonWords,
		"sourceWords": sourceWords,
		"groupWords":  groupWords,
		"reviewIn": func(form reviewForm, id int64) reviewForm {
			if form.ID != id {
				return reviewForm{}
			}
			return form
		},
		"outcomeWords": OutcomeWords,
		"outcomeHelp":  func(o string) string { return callOutcomeHelp[o] },
		"changed":      func(what string, src SettingSource) string { return changedWords(what, src, s.opt.Location) },
		"leadAct": func(id int64, do, label, back, sr string) leadActView {
			return leadActView{ID: id, Do: do, Label: label, Back: back, SR: sr}
		},
		"in": func(v string, list []string) bool { return slices.Contains(list, v) },
		// The Status page and the dashboard's cards.
		"ago": func(t any, now time.Time) string {
			switch t := t.(type) {
			case time.Time:
				return ago(t, now)
			case *time.Time:
				if t != nil {
					return ago(*t, now)
				}
			}
			return ""
		},
		"updated": func(t *time.Time, now time.Time) string {
			if t == nil {
				return "Last updated never"
			}
			at, n, layout := t.In(s.opt.Location), now.In(s.opt.Location), "15:04"
			if at.YearDay() != n.YearDay() || at.Year() != n.Year() {
				layout = "Mon 2 Jan 15:04"
			}
			return "Last updated " + at.Format(layout) + " (" + ago(*t, now) + ")"
		},
		"width":        func(n, total any) string { return widthClass(int64Of(n), int64Of(total)) },
		"usd":          USD,
		"noBriefYet":   func() string { return noBriefYet },
		"cents":        centsUSD,
		"bytes":        bytesWords,
		"pct":          pct,
		"optional":     optional,
		"uptime":       uptime,
		"mbLevel":      mailboxLevel,
		"mbState":      mailboxStateWords,
		"checkWords":   checkWords,
		"bounceWords":  bounceWords,
		"unitLevel":    unitLevel,
		"spendLevel":   spendLevel,
		"dbLevel":      dbLevel,
		"diskLevel":    diskLevel,
		"diskFreePct":  diskFreePct,
		"reoonLevel":   reoonLevel,
		"jevLevel":     jevLevel,
		"jevMeter":     func(label string, n, limit int) jevMeterView { return jevMeterView{label, n, limit} },
		"purposeWords": purposeWords,
		"maxSent": func(rows []CampaignFunnelRow) int {
			m := 0
			for _, r := range rows {
				m = max(m, r.Sent)
			}
			return m
		},
		"laterThan": func(status string) []string {
			return slices.DeleteFunc(slices.Clone(laterStatuses), func(s string) bool { return s == status })
		},
	}
}

// headers sets the security headers on every response, and no-store on every
// response outside /static/.
func (s *server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		if !s.opt.Dev {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// capBody limits every POST body to maxBody bytes.
func capBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// requireSession lets a request with a live session through. Without one, a
// plain GET is sent to the sign-in page with next set to where it was going; an
// htmx request gets 401 with HX-Redirect to the sign-in page; any other request
// gets 401 and the sign-in form.
func (s *server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.a.Valid(s.token(r)) {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case isHX(r):
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			target := r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(target), http.StatusSeeOther)
		default:
			s.renderLogin(w, r, http.StatusUnauthorized, "/")
		}
	})
}

// token returns the session cookie's value, or "".
func (s *server) token(r *http.Request) string {
	c, err := r.Cookie(s.cookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// setSession sets the session cookie; an empty token clears it.
func (s *server) setSession(w http.ResponseWriter, token string) {
	c := &http.Cookie{ //nolint:gosec // G124: Secure is off only in dev mode, over plain http on loopback
		Name:     s.cookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.opt.Dev,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	}
	if token == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// setDevice marks this browser as a device that has signed in. Logout leaves
// it: it marks the device, not the session.
func (s *server) setDevice(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is off only in dev mode, over plain http on loopback
		Name:     s.device,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.opt.Dev,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(deviceTTL / time.Second),
	})
}

// trustedDevice reports whether the request carries a trusted-device cookie
// made under the current password.
func (s *server) trustedDevice(r *http.Request) bool {
	c, err := r.Cookie(s.device)
	return err == nil && s.a.TrustedDevice(c.Value)
}

// setFlash stores a one-line message for the next page; an empty message clears it.
func (s *server) setFlash(w http.ResponseWriter, msg string) {
	c := &http.Cookie{ //nolint:gosec // G124: Secure is off only in dev mode, over plain http on loopback
		Name:     s.flash,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(msg)),
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.opt.Dev,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   60,
	}
	if msg == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// takeFlash returns the one-line message set by the last action, and clears it.
func (s *server) takeFlash(w http.ResponseWriter, r *http.Request) string {
	msg, _ := s.takeFlashAt(w, r)
	return msg
}

// callFlash marks a flash that belongs beside a lead's "Your call" block rather than at the
// top of the page. A control character, so no message a backend words can start with it.
const callFlash = "\x1fcall\x1f"

// takeFlashAt is takeFlash, and whether the message belongs beside the "Your call" block.
func (s *server) takeFlashAt(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(s.flash)
	if err != nil {
		return "", false
	}
	s.setFlash(w, "")
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return "", false
	}
	msg, atCall := strings.CutPrefix(string(b), callFlash)
	return msg, atCall
}

// maxFlashCarry caps the encoded flash the theme switch carries back (a cookie holds ~4 KB).
const maxFlashCarry = 2048

// flashCarry is the flash cookie this request carried, still encoded, when it is a valid
// one; "". The page reads (and clears) the flash as it renders, so the theme switch posts it
// back and setTheme sets it again: switching the theme never loses the line on the page.
func (s *server) flashCarry(r *http.Request) string {
	c, err := r.Cookie(s.flash)
	if err != nil || !validFlash(c.Value) {
		return ""
	}
	return c.Value
}

// validFlash reports whether v is a non-empty encoded flash no longer than maxFlashCarry.
func validFlash(v string) bool {
	if v == "" || len(v) > maxFlashCarry {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil
}

// safeNext returns next when it is the path of a signed-in page, with its query;
// anything else, another host included, is "/".
func (s *server) safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return "/"
	}
	if !s.next[u.Path] && !s.nextWithID(u.Path) || path.Clean(u.Path) != u.Path {
		return "/"
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// nextWithID reports whether p is the path of a signed-in page with ids in it: each
// {name} segment of its pattern a positive whole number.
func (s *server) nextWithID(p string) bool {
	got := strings.Split(p, "/")
	for _, pattern := range s.nextAt {
		want := strings.Split(pattern, "/")
		if len(want) != len(got) {
			continue
		}
		ok := true
		for i, w := range want {
			if strings.HasPrefix(w, "{") {
				n, err := strconv.ParseInt(got[i], 10, 64)
				ok = ok && err == nil && n > 0 && strconv.FormatInt(n, 10) == got[i]
			} else {
				ok = ok && w == got[i]
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// render writes the named page inside the layout with status code.
func (s *server) render(w http.ResponseWriter, r *http.Request, code int, name string, p page) {
	p.Product = productOf(r.URL.Path)
	p.Shell = s.shell(r.URL.Path, p.Product)
	p.Theme = s.theme(r)
	p.Back = themeBack(r, p)
	p.Flash = s.flashCarry(r)
	s.write(w, r, code, s.pages[name], "layout", p)
}

// write executes one template into a buffer, then sends it, so a template
// failure becomes a 500 rather than half a page.
func (s *server) write(w http.ResponseWriter, r *http.Request, code int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, r, fmt.Errorf("console: render %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

// fail logs err and answers 500 with a plain line that does not repeat it.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("console error", "path", r.URL.Path, "err", err.Error())
	http.Error(w, serverFailed, http.StatusInternalServerError)
}

// parseForm reads a POST form. A body over the cap answers 413 and reports false.
func parseForm(w http.ResponseWriter, r *http.Request) (ok, tooBig bool) {
	err := r.ParseForm()
	if err == nil {
		return true, false
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		http.Error(w, tooLarge, http.StatusRequestEntityTooLarge)
		return false, true
	}
	return false, false
}

// renderLogin shows the sign-in form. A request that still carries a session
// cookie no longer valid (the server restarted, or it expired) is told it was
// signed out, and the cookie is cleared.
func (s *server) renderLogin(w http.ResponseWriter, r *http.Request, code int, next string) {
	p := page{Title: "Sign in", Data: loginView{Next: next}}
	if tok := s.token(r); tok != "" && !s.a.Valid(tok) {
		s.setSession(w, "")
		p.Message = signedOut
	}
	s.render(w, r, code, "login", p)
}

// loginPage is GET /login. Signed in already, it goes on to next.
func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := s.safeNext(r.URL.Query().Get("next"))
	if s.a.Valid(s.token(r)) {
		http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // G710: next comes from safeNext, a signed-in page's path
		return
	}
	s.renderLogin(w, r, http.StatusOK, next)
}

// login is POST /login: the limiter first, then the check. A browser with a
// trusted-device cookie skips the limiter's global bound. Every failure past
// the limiter counts toward the global bound and gets the same answer. A
// success replaces any session the request carried with a new one and sets a
// fresh device cookie.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	ip := ClientIP(r)
	trusted := s.trustedDevice(r)
	if !s.a.AllowLoginFrom(ip, trusted) {
		s.log.Warn("console login", "outcome", "limited", "ip", ip, "trusted", trusted)
		http.Error(w, tooMany, http.StatusTooManyRequests)
		return
	}
	ok, tooBig := parseForm(w, r)
	if tooBig {
		s.a.RecordFailure()
		s.log.Warn("console login", "outcome", "denied", "ip", ip, "trusted", trusted)
		return
	}
	email, password := r.PostForm.Get("email"), r.PostForm.Get("password")
	next := s.safeNext(r.PostForm.Get("next"))
	if !ok || email == "" || password == "" || !s.a.Check(email, password) {
		s.a.RecordFailure()
		s.log.Warn("console login", "outcome", "denied", "ip", ip, "trusted", trusted)
		s.render(w, r, http.StatusUnauthorized, "login", page{
			Title: "Sign in", Message: loginFailed, Error: true, Data: loginView{Next: next},
		})
		return
	}
	device, err := s.a.NewDevice()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.a.Revoke(s.token(r))
	token, err := s.a.NewSession()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setSession(w, token)
	s.setDevice(w, device)
	s.log.Info("console login", "outcome", "ok", "ip", ip, "trusted", trusted)
	http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // G710: next comes from safeNext, a signed-in page's path
}

// logout is POST /logout: it ends the session and clears the cookie.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	s.a.Revoke(s.token(r))
	s.setSession(w, "")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// dashboard is GET /: the widget cards, with the line from the last action, if one was.
func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.renderHome(w, r, http.StatusOK, "dashboard", page{Title: "Dashboard", Active: "/", Message: s.takeFlash(w, r)})
}

// renderHome renders a page made of the dashboard's parts (the dashboard, Leads home, Jobs
// home) with the dashboard's data. Only the dashboard reads the Status figures, and a failed
// read of them leaves its status card grey rather than failing the page.
func (s *server) renderHome(w http.ResponseWriter, r *http.Request, code int, name string, p page) {
	d, err := s.b.Dashboard(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := dashboardView{Dashboard: d}
	if d.Leads != nil && name != "dashboard" {
		v.Why = s.sendingWhy(r)
	}
	if name == "dashboard" {
		v.Now = s.now().In(s.opt.Location)
		st, err := s.b.Status(r.Context())
		if err != nil {
			s.log.Error("console error", "route", r.Pattern, "read", "status", "err", err.Error())
			v.StatusFailed = true
		} else {
			v.Areas = statusAreas(st, v.Now)
		}
	}
	p.SignedIn, p.Data = true, v
	s.render(w, r, code, name, p)
}

// dashboardView is the dashboard and, on Leads home with leads set up, the sending sentence
// its leads card leads with (nil when not read). On the dashboard, Areas is the status card
// (statusAreas, worst first), StatusFailed true when the Status figures could not be read,
// and Now the time it was judged at.
type dashboardView struct {
	Dashboard
	Why          *SendingWhy
	Areas        []statusArea
	StatusFailed bool
	Now          time.Time // in Options.Location
}

// Greeting is the dashboard's heading for the time of day.
func (v dashboardView) Greeting() string {
	switch h := v.Now.Hour(); {
	case h < 5 || h >= 18:
		return "Good evening"
	case h < 12:
		return "Good morning"
	}
	return "Good afternoon"
}

// NewToday is the day's new jobs, every source together.
func (v dashboardView) NewToday() int {
	n := 0
	for _, c := range v.Funnel.NewBySource {
		n += c
	}
	return n
}

// NeedsMeMore is how many leads wait on the operator beyond the ones listed.
func (v dashboardView) NeedsMeMore() int { return max(v.NeedsMeTotal-len(v.NeedsMe), 0) }

// jobs is GET /jobs: the latest sent brief's jobs under the numbers it used.
func (s *server) jobs(w http.ResponseWriter, r *http.Request) {
	msg := s.takeFlash(w, r)
	s.renderJobs(w, r, http.StatusOK, msg, false)
}

func (s *server) renderJobs(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	bj, err := s.b.BriefJobs(r.Context(), "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, code, "jobs", page{
		Title: "Jobs", Active: "/jobs", SignedIn: true, Message: msg, Error: isErr, Data: bj,
	})
}

// tracked is GET /tracked?status=S. An unknown status shows the active jobs.
func (s *server) tracked(w http.ResponseWriter, r *http.Request) {
	msg := s.takeFlash(w, r)
	s.renderTracked(w, r, http.StatusOK, trackedStatus(r.URL.Query().Get("status")), msg, false, false)
}

// trackedStatus returns status when it is one of the Tracked page's filters, else "".
func trackedStatus(status string) string {
	for _, f := range trackedFilters {
		if f.Path == status {
			return status
		}
	}
	return ""
}

func (s *server) renderTracked(w http.ResponseWriter, r *http.Request, code int, status, msg string, isErr, fragment bool) {
	l, err := s.b.Tracked(r.Context(), status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := trackedView{Status: status, Filters: trackedFilters, Rows: l.Rows, Total: l.Total, Message: msg, Error: isErr}
	if fragment {
		s.write(w, r, code, s.pages["tracked"], "tracked_list", v)
		return
	}
	s.render(w, r, code, "tracked", page{Title: "Tracked", Active: "/tracked", SignedIn: true, Data: v})
}

// track is POST /jobs/track. A ref "N" comes from the Jobs page with the day of
// the brief that numbered it, and answers there; a ref "job:ID" comes from the
// Tracked page and answers there, keeping its status filter, or from the Results
// page (back=results) and answers there. An htmx request gets
// the updated row (or list) in place; a plain form gets a redirect with the result
// in a one-time cookie. A refusal the operator can fix is shown in place (htmx,
// 200) or on the page (422).
func (s *server) track(w http.ResponseWriter, r *http.Request) {
	ok, tooBig := parseForm(w, r)
	if tooBig {
		return
	}
	if !ok {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	req := TrackRequest{
		Ref:         strings.TrimSpace(f.Get("ref")),
		Day:         strings.TrimSpace(f.Get("day")),
		Action:      f.Get("action"),
		Reason:      f.Get("reason"),
		RuleField:   f.Get("rule_field"),
		RulePattern: f.Get("rule_pattern"),
	}
	fromResults := f.Get("back") == "results" && strings.HasPrefix(req.Ref, "job:")
	fromTracked := strings.HasPrefix(req.Ref, "job:") && !fromResults
	status := trackedStatus(f.Get("status"))

	out, err := s.b.Track(r.Context(), req)
	if err != nil && !errors.Is(err, ErrUserFixable) {
		s.fail(w, r, err)
		return
	}
	if err != nil {
		// The refusal's words follow ErrUserFixable in the error text.
		msg := cmp.Or(strings.TrimPrefix(strings.TrimPrefix(err.Error(), ErrUserFixable.Error()), ": "), out.Message)
		s.log.Info("console track refused", "ref", req.Ref, "action", req.Action, "why", msg)
		switch {
		case fromResults:
			s.renderResults(w, r, http.StatusUnprocessableEntity, msg, true)
		case fromTracked:
			code := http.StatusUnprocessableEntity
			if isHX(r) {
				code = http.StatusOK
			}
			s.renderTracked(w, r, code, status, msg, true, isHX(r))
		case isHX(r):
			s.unchangedRow(w, r, req, msg)
		default:
			s.renderJobs(w, r, http.StatusUnprocessableEntity, msg, true)
		}
		return
	}

	s.log.Info("console track", "job", out.Job.JobID, "from", out.From, "to", out.To)
	switch {
	case fromResults:
		s.setFlash(w, outcomeLine(out))
		http.Redirect(w, r, "/results#result-"+strconv.FormatInt(out.Job.JobID, 10), http.StatusSeeOther)
	case fromTracked && isHX(r):
		s.renderTracked(w, r, http.StatusOK, status, outcomeLine(out), false, true)
	case fromTracked:
		s.setFlash(w, outcomeLine(out))
		target := "/tracked"
		if status != "" {
			target += "?status=" + url.QueryEscape(status)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	case isHX(r):
		s.write(w, r, http.StatusOK, s.pages["job_row"], "job_row", rowView{Job: out.Job, Day: req.Day, Message: out.Message})
	default:
		s.setFlash(w, outcomeLine(out))
		http.Redirect(w, r, "/jobs#job-"+strconv.Itoa(out.Job.N), http.StatusSeeOther)
	}
}

// outcomeLine names the job and says what happened to it.
func outcomeLine(out TrackOutcome) string {
	if out.Job.Title == "" {
		return out.Message
	}
	return out.Job.Title + ": " + out.Message
}

// unchangedRow answers a refused htmx action on the Jobs page with the row as
// it stands in the brief of req.Day, carrying the refusal. When that brief has
// no such row (a bad number or day, or no such brief) there is no row to show:
// the page reloads (HX-Refresh) and shows the refusal from the one-time cookie.
func (s *server) unchangedRow(w http.ResponseWriter, r *http.Request, req TrackRequest, msg string) {
	bj, err := s.b.BriefJobs(r.Context(), req.Day)
	if err != nil && !errors.Is(err, ErrUserFixable) {
		s.fail(w, r, err)
		return
	}
	if n, nerr := strconv.Atoi(req.Ref); err == nil && nerr == nil {
		for _, j := range bj.Jobs {
			if j.N == n {
				s.write(w, r, http.StatusOK, s.pages["job_row"], "job_row", rowView{Job: j, Day: req.Day, Message: msg, Error: true})
				return
			}
		}
	}
	s.setFlash(w, msg)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// runs is GET /runs: the latest 100 runs.
func (s *server) runs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.b.Runs(r.Context(), 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "runs", page{Title: "Runs", Active: "/runs", SignedIn: true, Data: rows})
}

// rules is GET /rules: every rule, disabled ones included.
func (s *server) rules(w http.ResponseWriter, r *http.Request) {
	s.renderRules(w, r, http.StatusOK, s.takeFlash(w, r), false)
}

func (s *server) renderRules(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	rows, err := s.b.Rules(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, code, "rules", page{Title: "Rules", Active: "/rules", SignedIn: true, Message: msg, Error: isErr, Data: rows})
}

// static serves the embedded files under /static/, cached for a day.
func (s *server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "/") {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, "static/"+name, "public, max-age=86400")
}

// favicon is GET /favicon.ico, which browsers ask for on their own: the SVG icon.
func (s *server) favicon(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "static/favicon.svg", "")
}

// robots is GET /robots.txt.
func (s *server) robots(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "static/robots.txt", "")
}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request, name, cacheControl string) {
	b, err := fs.ReadFile(staticFS, name)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(b))
}

// healthz is GET /healthz: "ok" and nothing else.
func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// thousands writes n with a comma between each group of three digits.
func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// compact writes a token count as 950, 12.4K or 2.7M.
func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "K"
	default:
		return strconv.Itoa(n)
	}
}

// callsLine is the dashboard's Jev calls today, jobs and leads each against its own cap, e.g.
// "Jev today: 120 of 300 calls for jobs · 1,400 of 10,000 for leads".
func callsLine(d Dashboard) string {
	part := func(n, limit int, calls, what string) string {
		if limit > 0 {
			return thousands(n) + " of " + thousands(limit) + calls + " for " + what
		}
		return thousands(n) + calls + " for " + what
	}
	return "Jev today: " + part(d.JobsCalls, d.JobsCallCap, " calls", "jobs") + " · " + part(d.LeadsCalls, d.LeadsCallCap, "", "leads")
}

// costLine is the dashboard's Jev cost line, both purposes together, e.g.
// "$0.12 · 2.7M input tokens".
func costLine(d Dashboard) string {
	return fmt.Sprintf("$%.2f · %s input tokens", d.AICostUSD, compact(d.AIInputTokens))
}

// USD words micro-dollars as dollars and cents, "$0.84"; a spend above zero but under a cent
// is "under $0.01", never "$0.00".
func USD(micros int64) string {
	if micros > 0 && micros < 10_000 {
		return "under $0.01"
	}
	return fmt.Sprintf("$%.2f", float64(micros)/1e6)
}

// SpendOf words spent against a cap, "$0.84 of $5.00".
func SpendOf(spent, capMicros int64) string { return USD(spent) + " of " + USD(capMicros) }

// claudeLine is the dashboard's Claude spend today against the daily ceiling, with the
// purposes that spent, e.g. "Claude today: $0.84 of $5.00 (drafting $0.61, research $0.23)";
// "Claude today: $0.00 of $5.00" when nothing was spent.
func claudeLine(d Dashboard) string {
	line := "Claude today: " + SpendOf(d.ClaudeMicros, d.ClaudeCapMicros)
	var parts []string
	for _, p := range d.ClaudeByPurpose {
		if p.Micros > 0 {
			parts = append(parts, strings.ReplaceAll(p.Purpose, "_", " ")+" "+USD(p.Micros))
		}
	}
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	return line
}

// took is how long a run took, or "running" when it has not finished.
func took(r RunRow) string {
	if r.FinishedAt == nil {
		return "running"
	}
	d := r.FinishedAt.Sub(r.StartedAt).Round(time.Second)
	if d < time.Minute {
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	return strconv.Itoa(int(d/time.Minute)) + "m " + strconv.Itoa(int(d%time.Minute/time.Second)) + "s"
}

// sourceCount is one source's new jobs, for a stable order in the template.
type sourceCount struct {
	Name string
	N    int
}

// sources lists the funnel's new jobs per source, largest first, then by name.
func sources(m map[string]int) []sourceCount {
	out := make([]sourceCount, 0, len(m))
	for k, v := range m {
		out = append(out, sourceCount{k, v})
	}
	slices.SortFunc(out, func(a, b sourceCount) int {
		return cmp.Or(cmp.Compare(b.N, a.N), strings.Compare(a.Name, b.Name))
	})
	return out
}
