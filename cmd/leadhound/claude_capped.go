package main

import (
	"fmt"
	"os"
	"time"

	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// claudeBaseURL is where claudeCapped's client sends its requests. Tests point it at an
// httptest server; nothing else changes it, and the SDK's ANTHROPIC_BASE_URL is never read.
var claudeBaseURL = claude.DefaultBaseURL

// claudeCampaignCaps are a campaign's own Claude caps in micro-dollars; 0 keeps the default.
// config.ClaudeCapsFor never lets them rise above LEADHOUND_CLAUDE_DAILY_USD_CAP.
type claudeCampaignCaps struct {
	DailyMicros, LeadMicros int64
}

// claudeCapped builds the spend-capped Claude caller a command uses for purpose on
// accountID: the key from claude.LoadKey (the systemd credential, else the 0600 file named
// by LEADHOUND_ANTHROPIC_KEY_FILE), the client on claudeBaseURL, and claude.Capped counting
// in q's ai_usage on LEADHOUND_TZ's calendar day at the caps config.ClaudeCapsFor gives.
//
// Like leads rate, it calls nothing unless LEADHOUND_LIVE=1: otherwise it prints one line
// naming cmd and returns nil, nil, and the command stops with exit 0. A missing or unsafe
// key is an error that never carries the key.
func claudeCapped(cmd string, q store.Queries, purpose string, accountID int64, own claudeCampaignCaps) (*claude.Capped, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Live != config.LiveAll {
		_, _ = fmt.Fprintf(stdout, "%s: %s (LEADHOUND_LIVE is %s); no Claude call made\n", cmd, liveOffMessage, cfg.Live)
		return nil, nil
	}
	caps, err := cfg.ClaudeCapsFor(purpose, own.DailyMicros, own.LeadMicros)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return nil, usageError("LEADHOUND_TZ: %v", err)
	}
	key, err := claude.LoadKey(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	client, err := claude.New(key, claude.WithBaseURL(claudeBaseURL))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	return &claude.Capped{
		Client:         client,
		Store:          q,
		AccountID:      accountID,
		Purpose:        purpose,
		DailyCapMicros: caps.DailyMicros,
		TotalCapMicros: caps.TotalMicros,
		LeadCapMicros:  caps.LeadMicros,
		Loc:            loc,
	}, nil
}
