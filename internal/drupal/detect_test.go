package drupal

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const expires1978 = "Sun, 19 Nov 1978 05:00:00 GMT"

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: tests read their own fixtures by name
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type want struct {
	isDrupal  bool
	major     int
	exact     string
	ambiguous string
	botCheck  bool
	siteName  string
	evidence  string
}

func check(t *testing.T, got Finding, w want) {
	t.Helper()
	if got.IsDrupal != w.isDrupal || got.Major != w.major || got.Exact != w.exact ||
		got.Ambiguous != w.ambiguous || got.BotCheck != w.botCheck || got.SiteName != w.siteName {
		t.Errorf("got IsDrupal=%v Major=%d Exact=%q Ambiguous=%q BotCheck=%v SiteName=%q\nwant IsDrupal=%v Major=%d Exact=%q Ambiguous=%q BotCheck=%v SiteName=%q",
			got.IsDrupal, got.Major, got.Exact, got.Ambiguous, got.BotCheck, got.SiteName,
			w.isDrupal, w.major, w.exact, w.ambiguous, w.botCheck, w.siteName)
	}
	if ev := got.Evidence(); ev != w.evidence {
		t.Errorf("evidence = %q, want %q", ev, w.evidence)
	}
}

const base = "https://northwind.example/"

func TestDetectFixtures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		header  http.Header
		fixture string
		want    want
	}{
		{"d7 generator and ajaxPageState", 200,
			hdr("X-Generator", "Drupal 7 (http://drupal.org)", "X-Drupal-Cache", "HIT", "Expires", expires1978),
			"d7_generator.html", want{isDrupal: true, major: 7, siteName: "Northwind Trading",
				evidence: "x-generator:7|meta-generator:7|x-drupal-cache|expires-1978|Drupal.settings|d7-paths|ajaxPageState|misc-drupal-js"}},
		{"d7 without generator: settings and system.base.css", 200, nil,
			"d7_system_base.html", want{isDrupal: true, major: 7, evidence: "Drupal.settings|system.base.css"}},
		{"d6: settings, no d7 marker, non-core misc/drupal.js", 200, nil,
			"d6_misc.html", want{isDrupal: true, major: 6, evidence: "Drupal.settings|d7-paths|misc-drupal-js"}},
		{"d6 or d7: settings with no marker either way", 200, nil,
			"d6_or_7.html", want{isDrupal: true, ambiguous: Ambiguous6Or7, evidence: "Drupal.settings"}},
		{"d10 meta generator", 200, nil,
			"d10_meta.html", want{isDrupal: true, major: 10, siteName: "Woodgrove Bank", evidence: "meta-generator:10|drupalSettings"}},
		{"d11 header only", 200, hdr("X-Generator", "Drupal 11 (https://www.drupal.org)"),
			"d11_header_only.html", want{isDrupal: true, major: 11, evidence: "x-generator:11"}},
		{"exact from /core/misc/drupal.js?v=", 200, nil,
			"d10_exact.html", want{isDrupal: true, major: 10, exact: "10.6.14",
				evidence: "meta-generator:10|core-asset-v:10.6.14|drupalSettings|core-paths"}},
		{"jQuery ?v= trap: vendor version is not Drupal's", 200, nil,
			"jquery_v_trap.html", want{isDrupal: true, ambiguous: Ambiguous10Or11,
				evidence: "drupalSettings|core-paths|agg-include(>=10.1)"}},
		{"WordPress /core/assets/ trap", 200, nil,
			"wp_core_assets.html", want{evidence: "core-paths"}},
		{"WP Engine /sites/all/ trap", 200, nil,
			"wpengine_sites_all.html", want{evidence: "d7-paths"}},
		{"paths only: /sites/default/files and /core/misc never count", 200, nil,
			"sites_default_files.html", want{evidence: "core-asset-v:10.2.0|core-paths"}},
		{"aggregated include= assets: 10 or 11", 200, nil,
			"agg_include.html", want{isDrupal: true, ambiguous: Ambiguous10Or11, evidence: "drupalSettings|agg-include(>=10.1)"}},
		{"X-Drupal-Dynamic-Cache marks D8+ and blocks D7", 200, hdr("X-Drupal-Dynamic-Cache", "MISS"),
			"d8plus_settings.html", want{isDrupal: true, ambiguous: Ambiguous8Plus,
				evidence: "x-drupal-dynamic-cache|Drupal.settings|ajaxPageState"}},
		{"CF challenge header on 403", 403, hdr("Cf-Mitigated", "challenge", "X-Generator", "Drupal 10"),
			"cf_challenge.html", want{botCheck: true, evidence: "cf-mitigated:challenge"}},
		{"CF challenge header on 503", 503, hdr("Cf-Mitigated", "challenge"),
			"d10_meta.html", want{botCheck: true, evidence: "cf-mitigated:challenge"}},
		{"challenge-platform script in the body", 200, nil,
			"cf_challenge.html", want{botCheck: true, evidence: "challenge-platform"}},
		{"Cf-Mitigated on a 200 is not a bot check", 200, hdr("Cf-Mitigated", "challenge"),
			"d10_meta.html", want{isDrupal: true, major: 10, siteName: "Woodgrove Bank", evidence: "meta-generator:10|drupalSettings"}},
		{"Cloudflare's passive jsd script is not a challenge", 200, nil,
			"cf_jsd_drupal.html", want{isDrupal: true, major: 10, evidence: "meta-generator:10|drupalSettings"}},
		{"passive jsd script under /h/b/ is not a challenge", 200, nil,
			"cf_jsd_hb_drupal.html", want{isDrupal: true, major: 10, evidence: "meta-generator:10|drupalSettings"}},
		{"passive jsd script under /h/g/ is not a challenge", 200, nil,
			"cf_jsd_hg_drupal.html", want{isDrupal: true, major: 10, evidence: "meta-generator:10|drupalSettings"}},
		{"challenge form is a bot check", 403, nil,
			"cf_challenge_form.html", want{botCheck: true, evidence: "challenge-platform"}},
		{"CDN-hosted /core/misc/ version is not the site's", 200, nil,
			"cdn_core_misc.html", want{isDrupal: true, major: 10, evidence: "meta-generator:10|core-asset-v:9.5.1|drupalSettings|core-paths"}},
		{"www. host counts as the page's own host", 200, nil,
			"www_core_misc.html", want{isDrupal: true, major: 10, exact: "10.5.3",
				evidence: "meta-generator:10|core-asset-v:10.5.3|drupalSettings|core-paths"}},
		{"windows-1252 declared only in Content-Type", 200, hdr("Content-Type", "text/html; charset=windows-1252"),
			"windows1252_header_only.html", want{isDrupal: true, major: 7, siteName: "Café Fabrikam €", evidence: "meta-generator:7"}},
		{"og:site_name cut to 80 runes", 200, nil,
			"og_long.html", want{isDrupal: true, major: 11,
				siteName: "Northwind Traders and Fabrikam Partners Regional Association of Fine Coffee Roas",
				evidence: "meta-generator:11"}},
		{"windows-1252 page decodes its site name", 200, hdr("Content-Type", "text/html; charset=windows-1252"),
			"windows1252.html", want{isDrupal: true, major: 7, siteName: "Café Northwind", evidence: "meta-generator:7"}},
		{"header and meta disagree: the header wins", 200, hdr("X-Generator", "Drupal 10 (https://www.drupal.org)"),
			"og_long.html", want{isDrupal: true, major: 10,
				siteName: "Northwind Traders and Fabrikam Partners Regional Association of Fine Coffee Roas",
				evidence: "x-generator:10|meta-generator:11"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check(t, Detect(tt.status, tt.header, fixture(t, tt.fixture), base), tt.want)
		})
	}
}

func TestDetectBodies(t *testing.T) {
	settingsJSON := `<script type="application/json" data-drupal-selector="drupal-settings-json">{}</script>`
	pad := strings.Repeat("a", MaxBody)
	gen10 := `<meta name="Generator" content="Drupal 10 (https://www.drupal.org)">`
	tests := []struct {
		name   string
		status int
		header http.Header
		body   []byte
		want   want
	}{
		{"nil body, no headers", 200, nil, nil, want{}},
		{"empty body, header only", 200, hdr("X-Generator", "Drupal 10 (https://www.drupal.org)"), []byte{},
			want{isDrupal: true, major: 10, evidence: "x-generator:10"}},
		{"settings json alone: D8+", 200, nil, []byte(settingsJSON),
			want{isDrupal: true, ambiguous: Ambiguous8Plus, evidence: "drupalSettings"}},
		{"settings json needs its selector", 200, nil, []byte(`<script type="application/json">{}</script>`), want{}},
		{"generator not Drupal", 200, nil, []byte(`<meta name="generator" content="Drupalish 10">`), want{}},
		{"invalid UTF-8 and NUL bytes", 200, nil,
			[]byte("<html><head>" + gen10 + "\xc3\x28\xff\x00\x00<meta property=\"og:site_name\" content=\"North\x00wind\x01\">" +
				"</head><body>\x00\xfe</body></html>"),
			want{isDrupal: true, major: 10, siteName: "North�wind", evidence: "meta-generator:10"}},
		{"only NUL bytes", 200, nil, bytes.Repeat([]byte{0}, 4096), want{}},
		{"generator past the 2 MB cut is not read", 200, nil, []byte("<html><body><!--" + pad + "-->" + gen10), want{}},
		{"generator before the 2 MB cut is read", 200, nil, []byte("<html><head>" + gen10 + "<!--" + pad + "-->"),
			want{isDrupal: true, major: 10, evidence: "meta-generator:10"}},
		{"cut inside a multi-byte rune", 200, nil, append([]byte(gen10+"<p>"+strings.Repeat("a", MaxBody-len(gen10)-4)), "é"...),
			want{isDrupal: true, major: 10, evidence: "meta-generator:10"}},
		{"X-Generator without a number", 200, hdr("X-Generator", "Drupal"), nil, want{}},
		{"Expires and caches alone never count", 200,
			hdr("Expires", expires1978, "X-Drupal-Cache", "HIT", "X-Drupal-Dynamic-Cache", "MISS"), nil,
			want{evidence: "x-drupal-cache|x-drupal-dynamic-cache|expires-1978"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(tt.status, tt.header, tt.body, base)
			check(t, got, tt.want)
			if !utf8.ValidString(got.SiteName) || strings.ContainsRune(got.SiteName, 0) {
				t.Errorf("SiteName %q is not clean UTF-8", got.SiteName)
			}
		})
	}
}

// TestContractDrift: six synthetic pages, each built to produce one evidence shape the
// 2026-10-23 trial recorded, must reproduce both that evidence string and the major the
// trial labelled. Each comment names the trial evidence shape it mirrors; X.Y.Z stands for
// whatever exact version the trial saw.
func TestContractDrift(t *testing.T) {
	cacheHdrs := func(extra ...string) http.Header {
		return hdr(append([]string{"X-Drupal-Cache", "HIT", "X-Drupal-Dynamic-Cache", "MISS", "Expires", expires1978}, extra...)...)
	}
	tests := []struct {
		name    string
		header  http.Header
		fixture string
		major   int
		ambig   string
		exact   string
		// evidence is the trial's shape with this page's own version filled in.
		evidence string
	}{
		// Trial shape (the most common D11 row, labelled 11):
		// x-generator:11|meta-generator:11|x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings|core-paths|agg-include(>=10.1)
		{"d11 aggregated", cacheHdrs("X-Generator", "Drupal 11 (https://www.drupal.org)"), "drift_d11_agg.html", 11, "", "",
			"x-generator:11|meta-generator:11|x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings|core-paths|agg-include(>=10.1)"},
		// Trial shape (labelled 10, exact seen):
		// meta-generator:10|core-asset-v:X.Y.Z|x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings|core-paths|d7-paths
		{"d10 unaggregated with exact", cacheHdrs(), "drift_d10_exact.html", 10, "", "10.3.6",
			"meta-generator:10|core-asset-v:10.3.6|x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings|core-paths|d7-paths"},
		// Trial shape (labelled 7):
		// x-generator:7|meta-generator:7|x-drupal-cache|expires-1978|Drupal.settings|d7-paths|ajaxPageState
		{"d7", hdr("X-Generator", "Drupal 7 (http://drupal.org)", "X-Drupal-Cache", "MISS", "Expires", expires1978),
			"drift_d7.html", 7, "", "",
			"x-generator:7|meta-generator:7|x-drupal-cache|expires-1978|Drupal.settings|d7-paths|ajaxPageState"},
		// Trial shape (labelled 10 from the exact version alone, no Generator):
		// core-asset-v:X.Y.Z|expires-1978|drupalSettings|core-paths|agg-include(>=10.1)|d7-paths
		{"d10 from exact only", hdr("Expires", expires1978), "drift_d10_exact_only.html", 10, "", "10.4.2",
			"core-asset-v:10.4.2|expires-1978|drupalSettings|core-paths|agg-include(>=10.1)|d7-paths"},
		// Trial shape (major left blank, inferred 10.1+):
		// x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings|core-paths|agg-include(>=10.1)
		{"aggregated only: 10 or 11", cacheHdrs(), "drift_agg_only.html", 0, Ambiguous10Or11, "",
			"x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings|core-paths|agg-include(>=10.1)"},
		// Trial shape (labelled 9):
		// x-generator:9|meta-generator:9|x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings
		{"d9", cacheHdrs("X-Generator", "Drupal 9 (https://www.drupal.org)"), "drift_d9.html", 9, "", "",
			"x-generator:9|meta-generator:9|x-drupal-cache|x-drupal-dynamic-cache|expires-1978|drupalSettings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check(t, Detect(200, tt.header, fixture(t, tt.fixture), base),
				want{isDrupal: true, major: tt.major, exact: tt.exact, ambiguous: tt.ambig, evidence: tt.evidence})
		})
	}
}

func TestSignalOrderCoversEveryName(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range signalOrder {
		if seen[n] {
			t.Errorf("duplicate signal %q", n)
		}
		seen[n] = true
	}
	if len(seen) != 16 {
		t.Errorf("signalOrder has %d names, want 16", len(seen))
	}
	if DerivedBy != "drupal@1" {
		t.Errorf("DerivedBy = %q", DerivedBy)
	}
}
