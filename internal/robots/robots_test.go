package robots

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
)

const agent = "leadhound/0.1 (+https://leadhound.example/bot)"

// drupal7 is written fresh in the shape of Drupal 7's default robots.txt.
const drupal7 = `#
# robots.txt
#
User-agent: *
Crawl-delay: 10
# CSS, JS, Images
Allow: /misc/*.css$
Allow: /misc/*.css?
Allow: /misc/*.js$
Allow: /misc/*.js?
Allow: /modules/*.css$
Allow: /modules/*.js$
# Directories
Disallow: /includes/
Disallow: /misc/
Disallow: /modules/
Disallow: /profiles/
Disallow: /scripts/
Disallow: /themes/
# Files
Disallow: /CHANGELOG.txt
Disallow: /cron.php
Disallow: /install.php
Disallow: /INSTALL.txt
Disallow: /xmlrpc.php
# Paths (clean URLs)
Disallow: /admin/
Disallow: /user/login/
# Paths (no clean URLs)
Disallow: /?q=admin/
Disallow: /?q=user/login/
`

// drupal10 is written fresh in the shape of Drupal 10 and 11's default robots.txt.
const drupal10 = `# robots.txt
User-agent: *
# CSS, JS, Images
Allow: /core/*.css$
Allow: /core/*.css?
Allow: /core/*.js$
Allow: /core/*.js?
Allow: /core/*.gif
Allow: /profiles/*.css$
# Directories
Disallow: /core/
Disallow: /profiles/
# Files
Disallow: /README.md
Disallow: /web.config
# Paths (clean URLs)
Disallow: /admin/
Disallow: /search/
Disallow: /user/login
# Paths (no clean URLs)
Disallow: /index.php/admin/
Disallow: /index.php/user/login
Sitemap: https://northwind.example/sitemap.xml
`

func TestAllowed(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		agent string
		path  string
		want  bool
	}{
		{"d7 changelog", drupal7, agent, "/CHANGELOG.txt", false},
		{"d7 home", drupal7, agent, "/", true},
		{"d7 careers", drupal7, agent, "/careers", true},
		{"d7 misc css allowed", drupal7, agent, "/misc/drupal.css", true},
		{"d7 misc css with query allowed", drupal7, agent, "/misc/drupal.css?v=1", true},
		{"d7 misc js allowed", drupal7, agent, "/misc/drupal.js", true},
		{"d7 misc other disallowed", drupal7, agent, "/misc/logo.png", false},
		{"d7 admin", drupal7, agent, "/admin/config", false},
		{"d7 no clean admin", drupal7, agent, "/?q=admin/config", false},
		{"d7 changelog case differs", drupal7, agent, "/changelog.txt", true},

		{"d10 core dir", drupal10, agent, "/core/install.php", false},
		{"d10 core css", drupal10, agent, "/core/themes/olivero/css/base.css", true},
		{"d10 core css query", drupal10, agent, "/core/misc/a.css?v=10.3", true},
		{"d10 core js", drupal10, agent, "/core/misc/drupal.js", true},
		{"d10 core js query", drupal10, agent, "/core/misc/drupal.js?v=1", true},
		{"d10 core css$ not prefix", drupal10, agent, "/core/misc/a.cssx", false},
		{"d10 core gif prefix", drupal10, agent, "/core/misc/a.gif.bak", true},
		{"d10 readme", drupal10, agent, "/README.md", false},
		{"d10 user login", drupal10, agent, "/user/login", false},
		{"d10 user login prefix", drupal10, agent, "/user/login?destination=/", false},
		{"d10 user register", drupal10, agent, "/user/register", true},
		{"d10 home", drupal10, agent, "/", true},

		{"leadhound group overrides star", "User-agent: *\nDisallow: /\n\nUser-agent: leadhound\nDisallow: /private/\n", agent, "/about", true},
		{"leadhound group still applies", "User-agent: *\nDisallow: /\n\nUser-agent: leadhound\nDisallow: /private/\n", agent, "/private/x", false},
		{"leadhound group case insensitive", "User-agent: *\nAllow: /\n\nUser-agent: LeadHound\nDisallow: /\n", agent, "/about", false},
		{"leadhound group with version", "User-agent: *\nAllow: /\n\nUser-agent: LeadHound/2.0\nDisallow: /\n", agent, "/about", false},
		{"other agent group ignored", "User-agent: otherbot\nDisallow: /\n", agent, "/about", true},
		{"star used for other agents", "User-agent: *\nDisallow: /\n\nUser-agent: leadhound\nAllow: /\n", "otherbot/1.0", "/about", false},
		{"shared group lines", "User-agent: otherbot\nUser-agent: leadhound\nDisallow: /x\n", agent, "/x/1", false},
		{"split groups merge", "User-agent: leadhound\nDisallow: /a\n\nUser-agent: leadhound\nDisallow: /b\n", agent, "/b/1", false},

		{"longest match allow wins", "User-agent: *\nDisallow: /docs/\nAllow: /docs/public/\n", agent, "/docs/public/a", true},
		{"longest match disallow wins", "User-agent: *\nAllow: /docs/\nDisallow: /docs/private/\n", agent, "/docs/private/a", false},
		{"order does not matter", "User-agent: *\nAllow: /docs/public/\nDisallow: /docs/\n", agent, "/docs/other", false},
		{"tie goes to allow", "User-agent: *\nDisallow: /page\nAllow: /page\n", agent, "/page", true},
		{"tie with wildcard goes to allow", "User-agent: *\nDisallow: /*.php\nAllow: /a*.php\n", agent, "/a.php", true},

		{"dollar anchors end", "User-agent: *\nDisallow: /*.pdf$\n", agent, "/files/a.pdf", false},
		{"dollar no match after", "User-agent: *\nDisallow: /*.pdf$\n", agent, "/files/a.pdf?x=1", true},
		{"dollar exact path", "User-agent: *\nDisallow: /$\n", agent, "/", false},
		{"dollar exact path other", "User-agent: *\nDisallow: /$\n", agent, "/about", true},
		{"wildcard middle", "User-agent: *\nDisallow: /*/edit\n", agent, "/node/1/edit", false},
		{"wildcard repeated", "User-agent: *\nDisallow: /a*b*c\n", agent, "/axxbyyc", false},
		{"wildcard no match", "User-agent: *\nDisallow: /a*b*c\n", agent, "/axxcyyb", true},

		{"empty disallow allows all", "User-agent: *\nDisallow:\n", agent, "/anything", true},
		{"disallow root", "User-agent: *\nDisallow: /\n", agent, "/anything", false},
		{"no groups", "", agent, "/x", true},
		{"unknown lines ignored", "Foo: bar\nUser-agent: *\nCrawl-delay: 5\nNoindex: /x\nDisallow: /y\nnot a line\n", agent, "/x", true},
		{"unknown lines keep rules", "Foo: bar\nUser-agent: *\nCrawl-delay: 5\nNoindex: /x\nDisallow: /y\nnot a line\n", agent, "/y", false},
		{"rule before any agent ignored", "Disallow: /\nUser-agent: *\nDisallow: /y\n", agent, "/x", true},
		{"keys case insensitive", "USER-AGENT: *\nDISALLOW: /y\n", agent, "/y", false},
		{"comments stripped", "User-agent: * # all\nDisallow: /y # private\n", agent, "/y", false},
		{"crlf line endings", "User-agent: *\r\nDisallow: /y\r\n", agent, "/y", false},
		{"bom stripped", "\ufeffUser-agent: *\nDisallow: /y\n", agent, "/y", false},
		{"cr only line endings", "User-agent: *\rDisallow: /private\r", agent, "/private", false},
		{"cr only keeps other paths", "User-agent: *\rDisallow: /private\r", agent, "/public", true},
		{"raw pattern blocks upper escape", "User-agent: *\nDisallow: /caf\u00e9\n", agent, "/caf%C3%A9", false},
		{"raw pattern blocks lower escape", "User-agent: *\nDisallow: /caf\u00e9\n", agent, "/caf%c3%a9", false},
		{"escaped pattern blocks raw path", "User-agent: *\nDisallow: /caf%C3%A9\n", agent, "/caf\u00e9", false},
		{"lower escaped pattern blocks upper path", "User-agent: *\nDisallow: /caf%c3%a9\n", agent, "/caf%C3%A9", false},
		{"escaped slash is not a slash", "User-agent: *\nDisallow: /a/b\n", agent, "/a%2Fb", true},
		{"slash is not an escaped slash", "User-agent: *\nDisallow: /a%2Fb\n", agent, "/a/b", true},
		{"escaped slash matches itself", "User-agent: *\nDisallow: /a%2fb\n", agent, "/a%2Fb", false},
		{"empty path is root", "User-agent: *\nDisallow: /$\n", agent, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse([]byte(tt.body)).Allowed(tt.agent, tt.path)
			if got != tt.want {
				t.Errorf("Allowed(%q, %q) = %v, want %v", tt.agent, tt.path, got, tt.want)
			}
		})
	}
}

func TestParseCapsInput(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("User-agent: *\nDisallow: /early\n")
	for b.Len() < MaxBytes {
		b.WriteString("# padding padding padding padding padding padding padding\n")
	}
	b.WriteString("Disallow: /late\n")
	r := Parse(b.Bytes())
	if r.Allowed(agent, "/early") {
		t.Error("rule inside the cap was dropped")
	}
	if !r.Allowed(agent, "/late") {
		t.Error("rule past the cap was applied")
	}
}

func TestParseCapDropsCutLine(t *testing.T) {
	// The cap cuts "Disallow: /admin" to "Disallow: /", which would refuse every path if applied.
	head := "User-agent: *\n"
	cut := "Disallow: /"
	pad := bytes.Repeat([]byte("#"), MaxBytes-len(head)-len(cut)-1)
	body := []byte(head)
	body = append(body, pad...)
	body = append(body, '\n')
	body = append(body, "Disallow: /admin\n"...)
	if !Parse(body).Allowed(agent, "/x") {
		t.Error("a line cut by the cap was applied")
	}
}

func TestNilRulesAllowAll(t *testing.T) {
	var r *Rules
	if !r.Allowed(agent, "/x") {
		t.Error("nil Rules refused a path")
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   Verdict
	}{
		{"200 rules", http.StatusOK, nil, VerdictRules},
		{"204 rules", http.StatusNoContent, nil, VerdictRules},
		{"404 allowed all", http.StatusNotFound, nil, VerdictAllowedAll},
		{"410 allowed all", http.StatusGone, nil, VerdictAllowedAll},
		{"500 skip", http.StatusInternalServerError, nil, VerdictSkip},
		{"503 skip", http.StatusServiceUnavailable, nil, VerdictSkip},
		{"403 skip", http.StatusForbidden, nil, VerdictSkip},
		{"401 skip", http.StatusUnauthorized, nil, VerdictSkip},
		{"429 skip", http.StatusTooManyRequests, nil, VerdictSkip},
		{"301 skip", http.StatusMovedPermanently, nil, VerdictSkip},
		{"zero skip", 0, nil, VerdictSkip},
		{"transport error skip", 0, errors.New("dial tcp: connection refused"), VerdictSkip},
		{"error with 200 skip", http.StatusOK, errors.New("read: connection reset"), VerdictSkip},
		{"error with 404 skip", http.StatusNotFound, errors.New("timeout"), VerdictSkip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(tt.status, []byte("User-agent: *\n"), tt.err); got != tt.want {
				t.Errorf("Decide(%d, _, %v) = %q, want %q", tt.status, tt.err, got, tt.want)
			}
		})
	}
}
