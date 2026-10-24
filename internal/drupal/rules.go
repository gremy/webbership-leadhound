package drupal

import "regexp"

// DerivedBy names this rule set in the probe rows it produces.
const DerivedBy = "drupal@1"

// MaxBody is how much of a body Detect reads; anything past it is cut and the page is
// parsed as far as it goes.
const MaxBody = 2 << 20

// SiteNameMaxRunes caps Finding.SiteName.
const SiteNameMaxRunes = 80

// An exact version is core's only when its major is in this range.
const (
	minExactMajor = 8
	maxExactMajor = 11
)

// Signal names. A Signal renders in the evidence string as its Name, or as Name:Value
// when it has a value (Finding.Evidence joins them with "|" in the order below).
//
//	Name                  Value   Seen when
//	x-generator           N       X-Generator header "Drupal N …"
//	meta-generator        N       <meta name="Generator" content="Drupal N …">
//	core-asset-v          X.Y.Z   a /core/misc/*.js?v=X.Y.Z asset (Drupal's own version)
//	x-drupal-cache        -       X-Drupal-Cache header present
//	x-drupal-dynamic-cache -      X-Drupal-Dynamic-Cache header present (marks D8+)
//	expires-1978          -       Expires header in 1978 (Drupal's fixed past date)
//	drupalSettings        -       D8+ settings block (application/json, drupal-settings-json)
//	Drupal.settings       -       D6/D7 settings block (jQuery.extend(Drupal.settings)
//	core-paths            -       an asset under /core/{assets,misc,themes,modules}/
//	agg-include(>=10.1)   -       an aggregated css_/js_ asset with an include= query
//	d7-paths              -       an asset under /sites/all/ or a non-core /misc/
//	ajaxPageState         -       ajaxPageState inside the D6/D7 settings block
//	system.base.css       -       D7's system.base.css stylesheet
//	misc-drupal-js        -       a non-core misc/drupal.js script
//	cf-mitigated          challenge 403/503 with Cf-Mitigated: challenge (bot check)
//	challenge-platform    -       a challenge-platform script in the body (bot check)
//
// Only x-generator, meta-generator, drupalSettings and Drupal.settings make a page Drupal;
// every path signal is evidence only.
const (
	SigXGenerator        = "x-generator"
	SigMetaGenerator     = "meta-generator"
	SigCoreAssetV        = "core-asset-v"
	SigXDrupalCache      = "x-drupal-cache"
	SigXDrupalDynamic    = "x-drupal-dynamic-cache"
	SigExpires1978       = "expires-1978"
	SigDrupalSettings    = "drupalSettings"
	SigD7Settings        = "Drupal.settings"
	SigCorePaths         = "core-paths"
	SigAggInclude        = "agg-include(>=10.1)"
	SigD7Paths           = "d7-paths"
	SigAjaxPageState     = "ajaxPageState"
	SigSystemBaseCSS     = "system.base.css"
	SigMiscDrupalJS      = "misc-drupal-js"
	SigCfMitigated       = "cf-mitigated"
	SigChallengePlatform = "challenge-platform"
)

// signalOrder is the order signals appear in Finding.Signals and the evidence string.
var signalOrder = []string{
	SigXGenerator, SigMetaGenerator, SigCoreAssetV, SigXDrupalCache, SigXDrupalDynamic,
	SigExpires1978, SigDrupalSettings, SigD7Settings, SigCorePaths, SigAggInclude, SigD7Paths,
	SigAjaxPageState, SigSystemBaseCSS, SigMiscDrupalJS, SigCfMitigated, SigChallengePlatform,
}

// Ambiguous values: the major version could not be settled, only narrowed.
const (
	Ambiguous10Or11 = "10-or-11" // aggregated include= assets, from 10.1 on
	Ambiguous8Plus  = "8+"       // a D8+ marker and nothing narrower
	Ambiguous6Or7   = "6-or-7"   // Drupal.settings with neither D7 nor D6 marker
)

var (
	// generatorRe reads the major from a Generator value such as "Drupal 10 (https://…)".
	generatorRe = regexp.MustCompile(`(?i)^\s*drupal\s+(\d{1,2})(?:\D|$)`)
	// exactRe is a full X.Y.Z version in a /core/misc/ asset's v= query.
	exactRe = regexp.MustCompile(`^(\d{1,2})\.\d{1,3}\.\d{1,3}$`)
	// aggFileRe is an aggregated asset file name (css_<hash>.css, js_<hash>.js).
	aggFileRe = regexp.MustCompile(`(?:^|/)(?:css|js)_[A-Za-z0-9_-]+\.(?:css|js)$`)
	// metaCharsetRe is a <meta> charset declaration (the same prescan postingguard uses).
	metaCharsetRe = regexp.MustCompile(`(?i)<meta\b[^>]*?charset[\t\n\v\f\r ]*=`)
)

// corePathPrefixes mark a Drupal 8+ core asset path (also used by other software, so
// evidence only).
var corePathPrefixes = []string{"/core/assets/", "/core/misc/", "/core/themes/", "/core/modules/"}

const (
	d7SettingsMarker = "jQuery.extend(Drupal.settings"
	ajaxMarker       = "ajaxPageState"
	systemBaseMarker = "system.base.css"
	challengeMarker  = "challenge-platform"
	// jsdPath marks Cloudflare's passive detection script (…/challenge-platform/scripts/jsd/,
	// …/h/b/scripts/jsd/, …/h/g/scripts/jsd/), served on ordinary pages: not a challenge.
	jsdPath = "/scripts/jsd/"
	// Active challenge markers: the challenge page's orchestrate script, its options object
	// and its form.
	orchestrateMarker = "orchestrate/"
	chlOptMarker      = "_cf_chl_opt"
	challengeFormID   = "challenge-form"
)
