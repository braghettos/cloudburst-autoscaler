package provutil

import "strings"

// Common retryable error substrings shared across all cloud providers.
var baseRetryablePatterns = []string{
	"rate limit",
	"quota",
	"temporary",
	"timeout",
	"connection refused",
	"eof",
	"deadline exceeded",
	"unavailable",
	"503",
	"502",
	"500",
}

// IsRetryableError returns true when err contains any of the common retryable
// error substrings. Providers with additional retryable patterns should compose
// their own check:
//
//	func isRetryableError(err error) bool {
//	    if provutil.IsRetryableError(err) { return true }
//	    // provider-specific checks …
//	}
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	for _, p := range baseRetryablePatterns {
		if strings.Contains(errStr, p) {
			return true
		}
	}
	return false
}
