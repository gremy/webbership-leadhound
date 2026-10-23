package ai

import (
	"context"
	"errors"
	"testing"
)

func TestFakeScriptedAnswers(t *testing.T) {
	qs := []Question{
		{Name: "go_role", Options: []Option{{Name: "yes"}, {Name: "no"}}},
		{Name: "seniority", Options: []Option{{Name: "mid"}, {Name: "senior"}}},
	}
	f := &Fake{
		Answers: map[string]Answer{
			"go_role":   {Choice: "yes", Confidence: 0.9},
			"seniority": {Choice: "mid", Confidence: 0.6},
		},
		ByState: map[string]map[string]Answer{
			"a Drupal shop": {"go_role": {Choice: "no", Confidence: 0.8}},
		},
		Usage: Usage{InputTokens: 10},
	}
	if f.Name() != "fake" {
		t.Fatalf("Name = %q", f.Name())
	}

	got, usage, err := f.Decide(context.Background(), "a Go team", qs)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got["go_role"].Choice != "yes" || got["seniority"].Choice != "mid" || usage.InputTokens != 10 {
		t.Fatalf("answers = %+v, usage = %+v", got, usage)
	}

	got, _, err = f.Decide(context.Background(), "a Drupal shop", qs)
	if err != nil {
		t.Fatalf("Decide by state: %v", err)
	}
	if got["go_role"].Choice != "no" || got["seniority"].Choice != "mid" {
		t.Fatalf("per-state answers = %+v", got)
	}
	if f.Calls() != 2 {
		t.Fatalf("Calls = %d, want 2", f.Calls())
	}
}

func TestFakeUnscriptedAndErr(t *testing.T) {
	qs := []Question{{Name: "unknown", Options: []Option{{Name: "a"}}}}
	f := &Fake{}
	if _, _, err := f.Decide(context.Background(), "s", qs); !errors.Is(err, ErrNotScripted) {
		t.Fatalf("err = %v, want ErrNotScripted", err)
	}

	f = &Fake{Answers: map[string]Answer{"unknown": {Choice: "b"}}}
	if _, _, err := f.Decide(context.Background(), "s", qs); err == nil {
		t.Fatal("a scripted choice outside the options must fail")
	}

	f = &Fake{Err: ErrTooLong}
	if _, _, err := f.Decide(context.Background(), "s", qs); !errors.Is(err, ErrTooLong) {
		t.Fatalf("err = %v, want ErrTooLong", err)
	}
}
