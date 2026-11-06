package enrich

import "testing"

// The handles here are invented (privacycheck's Instagram allow list).
func TestInstagramHandle(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		ok        bool
	}{
		{"https://instagram.com/cafe.exemplu", "cafe.exemplu", true},
		{"https://www.instagram.com/cafe.exemplu", "cafe.exemplu", true},
		{"https://www.instagram.com/cafe.exemplu/", "cafe.exemplu", true},
		{"https://www.instagram.com/cafe.exemplu/?hl=ro&igsh=abc", "cafe.exemplu", true},
		{"https://www.instagram.com/cafe.exemplu#top", "cafe.exemplu", true},
		{"https://m.instagram.com/cafe.exemplu/reels/", "cafe.exemplu", true},
		{"http://instagram.com/cafe.exemplu", "cafe.exemplu", true},
		{"instagram.com/cafe.exemplu", "cafe.exemplu", true},
		{"//instagram.com/cafe.exemplu", "cafe.exemplu", true},
		{"https://instagr.am/cafe.exemplu", "cafe.exemplu", true},
		{"https://www.instagram.com/Cafe_Exemplu/", "cafe_exemplu", true},
		{"  @Cafe_Exemplu ", "cafe_exemplu", true},
		{"cafe_example", "cafe_example", true},
		{"https://www.instagram.com/p/AbC123/", "", false},
		{"https://www.instagram.com/reel/AbC123/", "", false},
		{"https://www.instagram.com/reels/AbC123/", "", false},
		{"https://www.instagram.com/explore/tags/cafea/", "", false},
		{"https://www.instagram.com/stories/cafe.exemplu/1/", "", false},
		{"https://www.instagram.com/tv/x/", "", false},
		{"https://www.instagram.com/accounts/login/", "", false},
		{"https://www.instagram.com/", "", false},
		{"https://www.instagram.com/../", "", false},
		{"https://www.instagram.com/exemplu%C3%A9/", "", false},
		{"https://www.instagram.com/" + "a1234567890123456789012345678901/", "", false}, // 32 characters
		{"https://www.instagram.com:8443/cafe.exemplu", "", false},
		{"https://user" + "@" + "instagram.com/cafe.exemplu", "", false},
		{"https://instagram.com.example/cafe.exemplu", "", false},
		{"https://cafe.example/cafe.exemplu", "", false},
		{"ftp://instagram.com/cafe.exemplu", "", false},
		{"@", "", false},
		{"@p", "", false},
		{"cafe exemplu", "", false},
		{"cafe.exemplu", "cafe.exemplu", true},
		{"instagram.com", "", false},
		{"Instagram.COM", "", false},
		{"instagram.com.", "", false},
		{" instagram.com \n", "", false},
		{"www.instagram.com", "", false},
		{"m.instagram.com", "", false},
		{"instagr.am", "", false},
		{"www.instagr.am.", "", false},
		{"@instagram.com", "", false},
		{"https://instagram.com", "", false},
		{"https://instagram.com/", "", false},
		{"instagram.com/", "", false},
		{"https://instagram.com/instagram.com", "", false},
		{"https://www.instagram.com/www.instagram.com/", "", false},
		{"cafe.instagram", "cafe.instagram", true},
		{"", "", false},
	} {
		got, ok := InstagramHandle(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("InstagramHandle(%q) = %q, %v; want %q, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestInstagramURL(t *testing.T) {
	for raw, want := range map[string]string{
		"cafe.exemplu":                            "https://www.instagram.com/cafe.exemplu/",
		"@Cafe.Exemplu":                           "https://www.instagram.com/cafe.exemplu/",
		"https://instagram.com/cafe.exemplu":      "https://www.instagram.com/cafe.exemplu/",
		"https://www.instagram.com/p/AbC123/":     "",
		"not a handle":                            "",
		"https://www.instagram.com/cafe.exemplu/": "https://www.instagram.com/cafe.exemplu/",
	} {
		if got := InstagramURL(raw); got != want {
			t.Errorf("InstagramURL(%q) = %q, want %q", raw, got, want)
		}
	}
	// The canonical address reads back as the same handle.
	if h, ok := InstagramHandle(InstagramURL("cafe.exemplu")); !ok || h != "cafe.exemplu" {
		t.Errorf("round trip = %q, %v", h, ok)
	}
}

// TestInstagramRoute: a social fact holding a profile gives the one canonical route value; a post,
// another network, a bare word and any other kind of fact give none.
func TestInstagramRoute(t *testing.T) {
	links := `<a href="https://www.instagram.com/Cafe.Exemplu/?hl=ro">a</a>` +
		`<a href="https://www.instagram.com/p/AbC123/">b</a>` +
		`<a href="https://www.facebook.com/cafe.exemplu">c</a>` +
		`<a href="https://uk.linkedin.com/company/cafe-exemplu">d</a>`
	var routes []string
	for _, f := range Extract([]byte(links), "https://cafe.example/", "cafe.example", false) {
		if v, ok := InstagramRoute(f); ok {
			routes = append(routes, v)
		}
		if _, ok := LinkedInRoute(f); ok {
			if _, also := InstagramRoute(f); also {
				t.Errorf("fact %q is both a LinkedIn and an Instagram route", f.Normalized)
			}
		}
	}
	if len(routes) != 1 || routes[0] != "https://www.instagram.com/cafe.exemplu/" {
		t.Errorf("routes = %q, want the one canonical profile address", routes)
	}
	for _, f := range []Fact{
		{Kind: KindSocial, Value: "x", Normalized: "https://instagram.com/cafe_example"},
		{Kind: KindSocial, Value: "x", Normalized: "https://www.instagram.com/cafe_example/"},
	} {
		if v, ok := InstagramRoute(f); !ok || v != "https://www.instagram.com/cafe_example/" {
			t.Errorf("%q: route %q, %v", f.Normalized, v, ok)
		}
	}
	for _, f := range []Fact{
		{Kind: KindDescription, Value: "x", Normalized: "https://instagram.com/cafe_example"},
		{Kind: KindSocial, Value: "x", Normalized: "cafe_example"},
		{Kind: KindSocial, Value: "x", Normalized: "https://instagram.com/reel/AbC123"},
		{Kind: KindSocial, Value: "x", Normalized: "https://instagram.com"},
		{Kind: KindSocial, Value: "x", Normalized: "https://facebook.com/cafe_example"},
	} {
		if v, ok := InstagramRoute(f); ok {
			t.Errorf("%+v gave route %q", f, v)
		}
	}
}
