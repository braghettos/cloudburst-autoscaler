package provutil

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// WithRetry executes op up to maxRetries+1 times with exponential backoff and jitter.
// isRetryable determines whether a failed attempt should be retried.
// The backoff starts at baseDelay and doubles each attempt, with ±25% jitter.
func WithRetry(ctx context.Context, maxRetries int, baseDelay time.Duration, isRetryable func(error) bool, op func() error) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			waitTime := baseDelay * time.Duration(1<<uint(attempt-1))
			jitter := time.Duration(rand.Int64N(int64(waitTime)/2)) - waitTime/4
			waitTime += jitter
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled: %w", ctx.Err())
			case <-time.After(waitTime):
			}
		}

		err := op()
		if err == nil {
			return nil
		}
		if !isRetryable(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("failed after %d attempts: %w", maxRetries+1, lastErr)
}
