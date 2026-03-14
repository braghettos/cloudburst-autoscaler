package provutil

import "time"

const (
	// MaxRetries is the default number of retry attempts for cloud provider API calls.
	MaxRetries = 3
	// RetryDelay is the initial delay between retries (doubled on each subsequent attempt).
	RetryDelay = 2 * time.Second
)
