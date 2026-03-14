package provutil

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithRetry_SuccessFirstAttempt(t *testing.T) {
	calls := 0
	err := WithRetry(context.Background(), 3, time.Millisecond, IsRetryableError, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestWithRetry_SuccessAfterRetries(t *testing.T) {
	calls := 0
	err := WithRetry(context.Background(), 3, time.Millisecond, IsRetryableError, func() error {
		calls++
		if calls < 3 {
			return errors.New("timeout waiting for response")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestWithRetry_NonRetryableError(t *testing.T) {
	calls := 0
	err := WithRetry(context.Background(), 3, time.Millisecond, IsRetryableError, func() error {
		calls++
		return errors.New("permission denied")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry for non-retryable), got %d", calls)
	}
}

func TestWithRetry_MaxRetriesExceeded(t *testing.T) {
	calls := 0
	err := WithRetry(context.Background(), 2, time.Millisecond, IsRetryableError, func() error {
		calls++
		return errors.New("connection refused")
	})
	if err == nil {
		t.Fatal("expected error after max retries, got nil")
	}
	// maxRetries=2 means 3 total attempts (initial + 2 retries)
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	if !errors.Is(err, errors.Unwrap(err)) {
		// Just verify the wrapping message includes attempt count
		want := "failed after 3 attempts"
		if got := err.Error(); !contains(got, want) {
			t.Fatalf("expected error to contain %q, got %q", want, got)
		}
	}
}

func TestWithRetry_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := WithRetry(ctx, 5, 50*time.Millisecond, IsRetryableError, func() error {
		calls++
		if calls == 1 {
			cancel()
		}
		return errors.New("timeout")
	})
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if !contains(err.Error(), "context cancelled") && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got: %v", err)
	}
}

func TestWithRetry_ZeroRetries(t *testing.T) {
	calls := 0
	err := WithRetry(context.Background(), 0, time.Millisecond, IsRetryableError, func() error {
		calls++
		return errors.New("timeout")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call with zero retries, got %d", calls)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
