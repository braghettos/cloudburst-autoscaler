package provutil

import (
	"errors"
	"testing"
)

func TestIsRetryableError_Nil(t *testing.T) {
	if IsRetryableError(nil) {
		t.Fatal("expected false for nil error")
	}
}

func TestIsRetryableError_RetryablePatterns(t *testing.T) {
	tests := []struct {
		name string
		err  string
	}{
		{"rate limit", "request hit rate limit"},
		{"quota", "quota exceeded for resource"},
		{"temporary", "temporary failure in name resolution"},
		{"timeout", "context timeout exceeded"},
		{"connection refused", "dial tcp: connection refused"},
		{"eof", "unexpected EOF"},
		{"deadline exceeded", "context deadline exceeded"},
		{"unavailable", "service unavailable"},
		{"503", "HTTP 503"},
		{"502", "HTTP 502 Bad Gateway"},
		{"500", "HTTP 500 Internal Server Error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !IsRetryableError(errors.New(tt.err)) {
				t.Fatalf("expected retryable for error %q", tt.err)
			}
		})
	}
}

func TestIsRetryableError_CaseInsensitive(t *testing.T) {
	if !IsRetryableError(errors.New("Connection REFUSED")) {
		t.Fatal("expected retryable (case-insensitive match)")
	}
	if !IsRetryableError(errors.New("RATE LIMIT exceeded")) {
		t.Fatal("expected retryable (case-insensitive match)")
	}
}

func TestIsRetryableError_NonRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  string
	}{
		{"permission denied", "permission denied"},
		{"not found", "resource not found"},
		{"invalid argument", "invalid argument: name is required"},
		{"auth error", "authentication failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsRetryableError(errors.New(tt.err)) {
				t.Fatalf("expected non-retryable for error %q", tt.err)
			}
		})
	}
}
