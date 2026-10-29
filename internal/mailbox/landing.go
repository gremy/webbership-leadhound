package mailbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LandingNote says a mailbox domain's website did not answer. It is a note, never a
// failure: a check's outcome does not depend on the website, and it is not stored in the
// check's fix (which the console shows as an error); a finished check with no landing URL
// is how a stored check says it.
const LandingNote = "The domain's website didn't answer."

// landingTimeout bounds the whole fetch, redirects included; landingMaxRedirects is how
// many redirects are followed before the fetch is given up.
var landingTimeout = 10 * time.Second

const landingMaxRedirects = 5

// errTooManyRedirects stops a fetch past landingMaxRedirects.
var errTooManyRedirects = errors.New("mailbox: too many redirects")

// LandingURL is where https://<domain>/ lands: a GET with redirects followed (at most 5)
// within 10 seconds, its final URL. The body is never read beyond a small amount, and is
// discarded. client nil is http.DefaultClient's transport; its own redirect rule and timeout
// are replaced by these. Any failure (no answer, a bad certificate, too many redirects, a
// domain that is not a host name) is an error.
func LandingURL(ctx context.Context, client *http.Client, domain string) (string, error) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" || strings.ContainsAny(domain, "/?#@ \t") {
		return "", fmt.Errorf("mailbox: landing: bad domain %q", domain)
	}
	return followLanding(ctx, client, (&url.URL{Scheme: "https", Host: domain, Path: "/"}).String())
}

// followLanding is LandingURL for any start URL (tests point it at an httptest server).
func followLanding(ctx context.Context, client *http.Client, start string) (string, error) {
	c := http.Client{Timeout: landingTimeout}
	if client != nil {
		c.Transport, c.Jar = client.Transport, client.Jar
	}
	c.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		if len(via) > landingMaxRedirects {
			return errTooManyRedirects
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, landingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, start, nil)
	if err != nil {
		return "", fmt.Errorf("mailbox: landing: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("mailbox: landing: %w", err)
	}
	// Nothing of the body is kept; a little is drained so the connection closes cleanly.
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
	return resp.Request.URL.String(), nil
}
