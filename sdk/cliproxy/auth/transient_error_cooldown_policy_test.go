package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRecoverableFailureRetryAfterWithHint_Cap(t *testing.T) {
	previous := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(0)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })

	now := time.Date(2026, 11, 24, 8, 30, 0, 0, time.UTC)
	long := 355 * time.Second
	short := 12 * time.Second
	for _, test := range []struct {
		name  string
		hint  *time.Duration
		limit time.Duration
		want  time.Duration
	}{
		{name: "uncapped", hint: &long, want: long},
		{name: "capped", hint: &long, limit: 30 * time.Second, want: 30 * time.Second},
		{name: "short hint", hint: &short, limit: 30 * time.Second, want: short},
		{name: "fallback", limit: 30 * time.Second, want: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := recoverableFailureRetryAfterWithHint(now, test.hint, false, test.limit)
			if want := now.Add(test.want); !got.Equal(want) {
				t.Fatalf("deadline = %v, want %v", got, want)
			}
		})
	}
}

func TestMarkResult_TransientOverloadCapDoesNotAffectQuota429(t *testing.T) {
	withQuotaCooldownEnabled(t)
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{TransientErrorMaxCooldownSeconds: 30})
	ctx := context.Background()

	overloadAuth := &Auth{ID: "codex-overload", Provider: "codex", Status: StatusActive}
	if _, err := mgr.Register(ctx, overloadAuth); err != nil {
		t.Fatalf("Register overloadAuth failed: %v", err)
	}
	retryAfter := 355 * time.Second
	before := time.Now()
	mgr.MarkResult(ctx, Result{
		AuthID:     overloadAuth.ID,
		Provider:   "codex",
		Model:      "gpt-6.1-sol",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			HTTPStatus: http.StatusServiceUnavailable,
			Message:    `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`,
		},
	})
	updated, ok := mgr.GetByID(overloadAuth.ID)
	if !ok {
		t.Fatalf("overloadAuth missing after MarkResult")
	}
	state := updated.ModelStates["gpt-6.1-sol"]
	if state == nil || !state.Unavailable {
		t.Fatalf("expected gpt-6.1-sol model state unavailable, got %+v", state)
	}
	remaining := state.NextRetryAfter.Sub(before)
	if remaining < 25*time.Second || remaining > 35*time.Second {
		t.Fatalf("expected capped cooldown near 30s, got %v", remaining)
	}

	quotaAuth := &Auth{ID: "codex-quota", Provider: "codex", Status: StatusActive}
	if _, err := mgr.Register(ctx, quotaAuth); err != nil {
		t.Fatalf("Register quotaAuth failed: %v", err)
	}
	quotaRetry := 2 * time.Hour
	beforeQuota := time.Now()
	mgr.MarkResult(ctx, Result{
		AuthID:          quotaAuth.ID,
		Provider:        "codex",
		Model:           "gpt-6.1-sol",
		Success:         false,
		CredentialScope: true,
		RetryAfter:      &quotaRetry,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`,
		},
	})
	updatedQuota, ok := mgr.GetByID(quotaAuth.ID)
	if !ok {
		t.Fatalf("quotaAuth missing after MarkResult")
	}
	quotaRemaining := updatedQuota.NextRetryAfter.Sub(beforeQuota)
	if quotaRemaining < time.Hour {
		t.Fatalf("expected quota 429 cooldown to stay uncapped (~2h), got %v", quotaRemaining)
	}
}
