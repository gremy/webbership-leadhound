//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
)

// TestClaudeLimitRecorded: the caller claudeCapped builds stores a money refusal, its own
// daily cap's or the Anthropic account's spend limit, as Claude's last error for the Status
// page, and a normal call stores nothing.
func TestClaudeLimitRecorded(t *testing.T) {
	orderScene(t)
	key := sentinelKey(t)
	claudeKeyFile(t, key)
	limited := false
	newAnthropicFake(t, key, func(int) (int, string) {
		if limited {
			return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"enforced_spend_limit_reached"}}`
		}
		return 200, anthropicMessage("end_turn", `{"subject":"ok"}`, 100, 20)
	})
	s := testStore(t)
	q := s.Q()
	ctx := context.Background()
	lead := leadIDOf(t, "cpandl.example")
	lq := leadstoretest.Q(t)
	lastError := func() string {
		t.Helper()
		r, err := lq.ServiceStatus(ctx, leadstore.ServiceClaude)
		if err != nil {
			return ""
		}
		return r.Error
	}

	if _, _, err := cappedFor(t, q, claudeCampaignCaps{}).Write(ctx, draftRequest(lead)); err != nil {
		t.Fatalf("normal call: %v", err)
	}
	if got := lastError(); got != "" {
		t.Errorf("a normal call stored %q", got)
	}

	limited = true
	if _, _, err := cappedFor(t, q, claudeCampaignCaps{}).Write(ctx, draftRequest(lead)); err == nil {
		t.Fatal("spend limit answer: no error")
	}
	if got, want := lastError(), "Anthropic's spend limit is reached: Claude calls are refused until it is raised or resets"; got != want {
		t.Errorf("after the spend limit answer: %q, want %q", got, want)
	}

	if _, _, err := cappedFor(t, q, claudeCampaignCaps{DailyMicros: 1}).Write(ctx, draftRequest(lead)); err == nil {
		t.Fatal("over the daily cap: no error")
	}
	if got := lastError(); !strings.HasPrefix(got, "Daily cap reached on drafting: ") {
		t.Errorf("after a cap refusal: %q", got)
	}
}
