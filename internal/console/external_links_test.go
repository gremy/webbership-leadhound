package console

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// anchorTag matches one opening <a> tag in a template, template actions inside it included.
var anchorTag = regexp.MustCompile(`<a\s(?:[^>{]|\{\{[^}]*\}\})*>`)

// relAttr matches a rel attribute and captures its value.
var relAttr = regexp.MustCompile(`\brel="([^"]*)"`)

// TestExternalLinksSweep is the invariant sweep over every console template: a link that leaves
// the console opens in a new tab and never hands the console's window to the page it opens.
// A link counts as leaving when its tag sits behind a webURL guard on the same line or its rel
// names noopener or noreferrer; each such link has target="_blank", and every target="_blank"
// has a rel holding noopener.
func TestExternalLinksSweep(t *testing.T) {
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	links := 0
	for _, f := range files {
		raw, err := fs.ReadFile(templateFS, f)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(raw), "\n") {
			for _, loc := range anchorTag.FindAllStringIndex(line, -1) {
				tag := line[loc[0]:loc[1]]
				rel := ""
				if m := relAttr.FindStringSubmatch(tag); m != nil {
					rel = m[1]
				}
				blank := strings.Contains(tag, `target="_blank"`)
				guarded := strings.Contains(line[:loc[0]], "webURL")
				external := guarded || strings.Contains(rel, "noopener") || strings.Contains(rel, "noreferrer")
				if external {
					links++
				}
				if external && !blank {
					t.Errorf("%s:%d: an external link lacks target=\"_blank\": %s", f, n+1, tag)
				}
				if blank && !strings.Contains(rel, "noopener") {
					t.Errorf("%s:%d: a target=\"_blank\" link lacks rel noopener: %s", f, n+1, tag)
				}
			}
		}
	}
	// The sweep must find the links it is about, or a change to the matcher passes it vacuously.
	if links < 10 {
		t.Errorf("the sweep found only %d external links", links)
	}
}

// TestLeadDetailPlaceType renders a café lead's place type: the badge in words, who decided
// it, and the two corrections posting the list's own actions; a Drupal lead shows none of it.
func TestLeadDetailPlaceType(t *testing.T) {
	b := richBackend("Brewlane Corner")
	b.detail = cafeLeadDetail()
	b.detail.PlaceType, b.detail.PlaceTypeSource = "roastery", "name"
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/leads/5", c)).Body.String()
	for _, want := range []string{
		`<span class="badge badge-accent">Roastery or online store</span>`,
		`<span class="muted small">from its name</span>`,
		`action="/leads/5/roastery"`, `action="/leads/5/cafe"`,
		`>It's a roastery</button>`, `>It's a coffee shop</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the roastery page lacks %q", want)
		}
	}
	if strings.Contains(body, `aria-current="true">It's a`) {
		t.Error("a name guess is marked as the operator's own call")
	}

	// The operator's own call marks its button as current.
	b.detail.PlaceType, b.detail.PlaceTypeSource = "cafe", "operator"
	body = h.do(get("/leads/5", c)).Body.String()
	for _, want := range []string{
		`<span class="badge">Coffee shop</span>`, `<span class="muted small">your call</span>`,
		`aria-current="true">It's a coffee shop</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the operator's café lacks %q", want)
		}
	}

	// Jev's and Instagram's reads are named as such, and the default names no source at all.
	b.detail.PlaceTypeSource = "jev"
	if body = h.do(get("/leads/5", c)).Body.String(); !strings.Contains(body, `<span class="muted small">Jev's read</span>`) {
		t.Error("Jev's read is not named")
	}
	b.detail.PlaceTypeSource = "instagram"
	if body = h.do(get("/leads/5", c)).Body.String(); !strings.Contains(body, `<span class="muted small">Instagram's read</span>`) {
		t.Error("Instagram's read is not named")
	}
	b.detail.PlaceTypeSource = ""
	if body = h.do(get("/leads/5", c)).Body.String(); strings.Contains(body, "place-type stack-sm") &&
		strings.Contains(body, `<span class="muted small">your call</span>`) {
		t.Error("the default place type names a source")
	}

	// A lead that is not a café has no place type and shows no block.
	b.detail.PlaceType = ""
	if body = h.do(get("/leads/5", c)).Body.String(); strings.Contains(body, "/leads/5/roastery") {
		t.Error("a lead with no place type offers the correction")
	}
}
