/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cherry_servers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mockTransport intercepts HTTP requests and returns mock responses.
type mockTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTrip(req)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     map[string][]string{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestNewProvisioner_Validation(t *testing.T) {
	ctx := context.Background()

	_, err := NewProvisioner(ctx, "", 123)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if !strings.Contains(err.Error(), "API token is required") {
		t.Errorf("unexpected error: %v", err)
	}

	_, err = NewProvisioner(ctx, "token", 0)
	if err == nil {
		t.Fatal("expected error for zero project ID")
	}
	if !strings.Contains(err.Error(), "project ID is required") {
		t.Errorf("unexpected error: %v", err)
	}

	p, err := NewProvisioner(ctx, "token", 123)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provisioner")
	}
}

func TestMapRegion(t *testing.T) {
	tests := []struct {
		region   string
		want     string
		wantErr  bool
	}{
		{"eu-lt-1", "LT-Siauliai", false},
		{"eu-nl-1", "NL-Amsterdam", false},
		{"eu-de-1", "DE-Frankfurt", false},
		{"eu-se-1", "SE-Stockholm", false},
		{"eu-fr-1", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			got, err := mapRegion(tt.region)
			if (err != nil) != tt.wantErr {
				t.Errorf("mapRegion() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("mapRegion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMapPlan(t *testing.T) {
	tests := []struct {
		instanceType string
		want         string
		wantErr      bool
	}{
		{"virtual-ssd-1", "B1-1-1gb-20s-shared", false},
		{"virtual-ssd-4", "B1-4-4gb-80s-shared", false},
		{"virtual-ssd-32", "B1-32-32gb-640s-shared", false},
		{"unknown", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.instanceType, func(t *testing.T) {
			got, err := mapPlan(tt.instanceType)
			if (err != nil) != tt.wantErr {
				t.Errorf("mapPlan() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("mapPlan() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateInstance_Success(t *testing.T) {
	ctx := context.Background()
	callCount := 0

	p, err := NewProvisioner(ctx, "test-token", 123)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				callCount++
				if req.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("missing or wrong Authorization header")
				}
				switch {
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/projects/123/servers"):
					var body createServerRequest
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					if body.Plan != "B1-2-2gb-40s-shared" {
						t.Errorf("plan = %q, want B1-2-2gb-40s-shared", body.Plan)
					}
					if body.Region != "LT-Siauliai" {
						t.Errorf("region = %q, want LT-Siauliai", body.Region)
					}
					if body.Hostname != "cb-node-1" {
						t.Errorf("hostname = %q, want cb-node-1", body.Hostname)
					}
					return jsonResponse(http.StatusCreated, `{"id":456,"state":"active","status":"active"}`), nil
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/servers/456"):
					return jsonResponse(http.StatusOK, `{"id":456,"state":"active","status":"active"}`), nil
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					return jsonResponse(http.StatusInternalServerError, `{}`), nil
				}
			},
		},
	}

	instanceID, err := p.CreateInstance(ctx, "cb-node-1", "virtual-ssd-2", "eu-lt-1", "", "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if instanceID != "cherry_servers/eu-lt-1/456" {
		t.Errorf("instanceID = %q, want cherry_servers/eu-lt-1/456", instanceID)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 API calls (create + status poll), got %d", callCount)
	}
}

func TestDeleteInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-token", 123)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/servers/789") {
					return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	err = p.DeleteInstance(ctx, "cherry_servers/eu-lt-1/789")
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

func TestDeleteInstance_NotFound(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-token", 123)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete {
					return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	err = p.DeleteInstance(ctx, "cherry_servers/eu-lt-1/999")
	if err != nil {
		t.Fatalf("DeleteInstance (404): %v", err)
	}
}

func TestIsInstanceRunning_Running(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-token", 123)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/servers/111") {
					return jsonResponse(http.StatusOK, `{"id":111,"state":"active","status":"active"}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "cherry_servers/eu-lt-1/111")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if !running {
		t.Error("expected running=true")
	}
}

func TestIsInstanceRunning_NotRunning(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-token", 123)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/servers/222") {
					return jsonResponse(http.StatusOK, `{"id":222,"state":"stopped","status":"stopped"}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "cherry_servers/eu-lt-1/222")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if running {
		t.Error("expected running=false")
	}
}

func TestIsInstanceRunning_NotFound(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-token", 123)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "cherry_servers/eu-lt-1/333")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if running {
		t.Error("expected running=false for 404")
	}
}

func TestIsInstanceRunning_InvalidInstanceID(t *testing.T) {
	ctx := context.Background()
	p, _ := NewProvisioner(ctx, "token", 123)

	_, err := p.IsInstanceRunning(ctx, "aws/us-east-1/i-123")
	if err == nil {
		t.Fatal("expected error for wrong prefix")
	}
}

func TestClose(t *testing.T) {
	p, _ := NewProvisioner(context.Background(), "token", 123)
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
