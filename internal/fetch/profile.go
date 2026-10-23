package fetch

import (
	"net/http"
	"slices"
	"strings"
)

// Profile is what a Client sends and which responses mean it has been blocked. The zero
// Profile sends no extra headers and treats nothing as blocked.
type Profile struct {
	Name            string
	Header          http.Header // sent on every request; the User-Agent comes from the Client
	BlockedStatuses []int       // statuses that open the breaker with StopBlocked
	BlockedPaths    []string    // final-URL path prefixes that open it with StopBlocked
}

// LinkedInJobs is the profile for LinkedIn's guest job endpoints: the headers of the
// upstream CLI (see NOTICE) and 403, 999 or a redirect to a sign-in wall as blocked.
var LinkedInJobs = Profile{
	Name: "linkedin-jobs",
	Header: http.Header{
		"Accept":           {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		"Accept-Language":  {"en-US,en;q=0.9"},
		"X-Requested-With": {"XMLHttpRequest"},
	},
	BlockedStatuses: []int{http.StatusForbidden, 999},
	BlockedPaths:    []string{"/authwall", "/login", "/checkpoint"},
}

// clone returns a deep copy, so a Client never shares maps or slices with a package value.
func (p Profile) clone() Profile {
	p.Header = p.Header.Clone()
	p.BlockedStatuses = slices.Clone(p.BlockedStatuses)
	p.BlockedPaths = slices.Clone(p.BlockedPaths)
	return p
}

// blocked reports whether a response with this status and final path means blocked.
func (p Profile) blocked(status int, path string) bool {
	if slices.Contains(p.BlockedStatuses, status) {
		return true
	}
	for _, prefix := range p.BlockedPaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// ProjectURL is the repository the User-Agent points to.
const ProjectURL = "https://github.com/gremy/webbership-leadhound"

// UserAgentFor is the honest User-Agent naming the tool and its version.
func UserAgentFor(version string) string {
	return "leadhound/" + version + " (+" + ProjectURL + ")"
}
