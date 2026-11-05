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
