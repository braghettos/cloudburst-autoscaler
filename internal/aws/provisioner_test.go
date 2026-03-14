package aws

import (
	"errors"
	"testing"
)

func TestGenerateInstanceName(t *testing.T) {
	tests := []struct {
		nodeClaimName string
		want          string
	}{
		{"cb-pool1-abcde", "cb-pool1-abcde"},
		{"cb-gpu-pool-xyz12", "cb-gpu-pool-xyz12"},
		{"my-node", "my-node"},
	}
	for _, tt := range tests {
		t.Run(tt.nodeClaimName, func(t *testing.T) {
			got := GenerateInstanceName(tt.nodeClaimName)
			if got != tt.want {
				t.Errorf("GenerateInstanceName(%q) = %q, want %q", tt.nodeClaimName, got, tt.want)
			}
		})
	}
}

func TestParseInstanceID_Valid(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		wantRegion string
		wantID     string
	}{
		{
			name:       "standard ec2 id",
			instanceID: "aws/us-east-1/i-0123456789abcdef0",
			wantRegion: "us-east-1",
			wantID:     "i-0123456789abcdef0",
		},
		{
			name:       "eu region",
			instanceID: "aws/eu-west-1/i-abcdef1234567890a",
			wantRegion: "eu-west-1",
			wantID:     "i-abcdef1234567890a",
		},
		{
			name:       "ap region",
			instanceID: "aws/ap-southeast-2/i-1111111111111111",
			wantRegion: "ap-southeast-2",
			wantID:     "i-1111111111111111",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			region, id, err := parseInstanceID(tt.instanceID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if region != tt.wantRegion {
				t.Errorf("region = %q, want %q", region, tt.wantRegion)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestParseInstanceID_Invalid(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
	}{
		{"wrong prefix", "gcp/us-central1/my-vm"},
		{"azure prefix", "azure/rg/vm"},
		{"no prefix", "us-east-1/i-123"},
		{"empty", ""},
		{"prefix only", "aws/"},
		{"no id", "aws/us-east-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseInstanceID(tt.instanceID)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{"nil", nil, false},
		{"throttling", errors.New("Throttling: Rate exceeded"), true},
		{"request limit", errors.New("RequestLimitExceeded"), true},
		{"timeout (base)", errors.New("request timeout"), true},
		{"connection refused (base)", errors.New("connection refused"), true},
		{"permission denied", errors.New("UnauthorizedAccess"), false},
		{"not found", errors.New("instance not found"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryableError(tt.err)
			if got != tt.wantRetry {
				t.Errorf("isRetryableError(%v) = %v, want %v", tt.err, got, tt.wantRetry)
			}
		})
	}
}

func TestIsInvalidAMIIDNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"matching error", errors.New("InvalidAMIID.NotFound: The image id 'ami-xxx' does not exist"), true},
		{"other error", errors.New("InvalidParameterValue"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isInvalidAMIIDNotFound(tt.err)
			if got != tt.want {
				t.Errorf("isInvalidAMIIDNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestInstanceIDPrefix(t *testing.T) {
	if InstanceIDPrefix != "aws/" {
		t.Errorf("InstanceIDPrefix = %q, want %q", InstanceIDPrefix, "aws/")
	}
}
