package fetch

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// TestLiveATSHostFamilies: under LEADHOUND_LIVE=ats a per-company ATS host goes out only when
// it is one label in front of the family's suffix, judged on the URL's real host: after
// lower-casing and dropping one root dot, with any userinfo and port set aside.
func TestLiveATSHostFamilies(t *testing.T) {
	tt := ".teamtailor.com"
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://acme" + tt + "/jobs.rss?offset=0", true},
		{"https://ACME.TeamTailor.COM/jobs.rss", true},
		{"https://acme" + tt + "./jobs.rss", true},
		{"https://acme" + tt + ":443/jobs.rss", true},
		{"https://user@acme" + tt + "/jobs.rss", true}, // userinfo is not the host
		{"https://acme.jobs.personio.de/xml?language=en", true},
		{"https://acme.jobs.personio.com/xml", true},
		{"https://api.smartrecruiters.com/v1/companies/Acme/postings", true},
		{"https://teamtailor.com/jobs.rss", false},
		{"https://a.b" + tt + "/jobs.rss", false},
		{"https://acmeteamtailor.com/jobs.rss", false},
		{"https://teamtailor.com.evil.example/jobs.rss", false},
		{"https://acme" + tt + ".evil.example/jobs.rss", false},
		{"https://acme" + tt + "@evil.example/jobs.rss", false}, // the host is evil.example
		{"https://acme" + tt + "../jobs.rss", false},
		{"https://jobs.personio.de/xml", false},
		{"https://acme.jobs.personio.de.evil.example/xml", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) { return okResponse(r, 200, "ok"), nil }}
			clk, rec := newFakeClock(), &memRecorder{}
			c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveATS))...)
			_, err := c.Get(context.Background(), KindBoard, tc.url)
			if tc.want {
				if err != nil || st.calls.Load() != 1 {
					t.Fatalf("err = %v, calls %d; want it requested", err, st.calls.Load())
				}
				return
			}
			if !errors.Is(err, ErrLiveOff) && !errors.Is(err, ErrBadURL) {
				t.Errorf("err = %v, want ErrLiveOff", err)
			}
			if st.calls.Load() != 0 {
				t.Errorf("calls = %d, want 0: a refused host is never requested", st.calls.Load())
			}
		})
	}
}

// TestLiveATSHostFamilyRedirects: a redirect is judged by the same rule: to another
// company's board it is followed, out of the family's narrow shape it is refused.
func TestLiveATSHostFamilyRedirects(t *testing.T) {
	for _, tc := range []struct {
		location string
		want     bool
	}{
		{"https://other.teamtailor.com/jobs.rss", true},
		{"https://a.b.teamtailor.com/jobs.rss", false},
		{"https://teamtailor.com/jobs.rss", false},
		{"https://acme.teamtailor.com.evil.example/jobs.rss", false},
		{"https://acme.teamtailor.com" + "@" + "evil.example/jobs.rss", false},
	} {
		t.Run(tc.location, func(t *testing.T) {
			st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "acme.teamtailor.com" {
					resp := okResponse(r, http.StatusFound, "")
					resp.Header.Set("Location", tc.location)
					return resp, nil
				}
				return okResponse(r, 200, "ok"), nil
			}}
			clk, rec := newFakeClock(), &memRecorder{}
			c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveATS))...)
			resp, err := c.Get(context.Background(), KindBoard, "https://acme.teamtailor.com/jobs.rss")
			if tc.want {
				if err != nil || resp.Status != 200 || st.calls.Load() != 2 {
					t.Fatalf("Get = %d, %v, calls %d; want the redirect followed", resp.Status, err, st.calls.Load())
				}
				return
			}
			if !errors.Is(err, ErrLiveOff) || st.calls.Load() != 1 {
				t.Errorf("Get err = %v, calls %d; want ErrLiveOff and the target never requested", err, st.calls.Load())
			}
		})
	}
}
