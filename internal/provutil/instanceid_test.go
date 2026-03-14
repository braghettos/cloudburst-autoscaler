package provutil

import "testing"

func TestParseInstanceID_Valid(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		prefix     string
		wantRegion string
		wantID     string
	}{
		{
			name:       "aws format",
			raw:        "aws/us-east-1/i-0123456789abcdef0",
			prefix:     "aws/",
			wantRegion: "us-east-1",
			wantID:     "i-0123456789abcdef0",
		},
		{
			name:       "azure format",
			raw:        "azure/my-resource-group/my-vm",
			prefix:     "azure/",
			wantRegion: "my-resource-group",
			wantID:     "my-vm",
		},
		{
			name:       "hetzner format",
			raw:        "hetzner/fsn1/12345",
			prefix:     "hetzner/",
			wantRegion: "fsn1",
			wantID:     "12345",
		},
		{
			name:       "id with slashes preserved",
			raw:        "custom/region/path/to/resource",
			prefix:     "custom/",
			wantRegion: "region",
			wantID:     "path/to/resource",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			region, id, err := ParseInstanceID(tt.raw, tt.prefix)
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
		name   string
		raw    string
		prefix string
	}{
		{"wrong prefix", "gcp/us-central1/my-vm", "aws/"},
		{"empty string", "", "aws/"},
		{"prefix only", "aws/", "aws/"},
		{"no id part", "aws/us-east-1", "aws/"},
		{"no region", "aws//i-123", "aws/"},
		{"missing trailing slash in value", "awsus-east-1/i-123", "aws/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ParseInstanceID(tt.raw, tt.prefix)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
