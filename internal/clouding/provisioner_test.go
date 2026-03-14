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

package clouding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

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

	_, err := NewProvisioner(ctx, "", "")
	if err == nil {
		t.Fatal("expected error for empty API key")
	}
	if !strings.Contains(err.Error(), "API key is required") {
		t.Errorf("unexpected error: %v", err)
	}

	p, err := NewProvisioner(ctx, "key", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provisioner")
	}
}

func TestCreateInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "fw-123")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("X-API-KEY") != "test-key" {
					t.Errorf("missing or wrong X-API-KEY header")
				}
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/sizes/flavors"):
					return jsonResponse(http.StatusOK, `{"items":[{"id":"1x2","name":"1x2","vcpus":2,"ram":4}]}`), nil
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/servers"):
					var body createServerRequest
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					if body.FlavorID != "1x2" {
						t.Errorf("flavorId = %q, want 1x2", body.FlavorID)
					}
					if body.PublicPortFirewallIDs[0] != "fw-123" {
						t.Errorf("firewallIds = %v, want [fw-123]", body.PublicPortFirewallIDs)
					}
					return jsonResponse(http.StatusCreated, `{"id":"srv-456","status":"running","state":"running"}`), nil
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/servers/srv-456"):
					return jsonResponse(http.StatusOK, `{"id":"srv-456","status":"running","state":"running"}`), nil
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					return jsonResponse(http.StatusInternalServerError, `{}`), nil
				}
			},
		},
	}

	instanceID, err := p.CreateInstance(ctx, "cb-node-1", "small-1", "es-bcn-1", "", "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if instanceID != "clouding/es-bcn-1/srv-456" {
		t.Errorf("instanceID = %q, want clouding/es-bcn-1/srv-456", instanceID)
	}
}

func TestCreateInstance_FetchesFirewallWhenNotProvided(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/firewalls"):
					return jsonResponse(http.StatusOK, `{"items":[{"id":"fw-default"}]}`), nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/sizes/flavors"):
					return jsonResponse(http.StatusOK, `{"items":[{"id":"1x2","vcpus":2,"ram":4}]}`), nil
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/servers"):
					var body createServerRequest
					json.NewDecoder(req.Body).Decode(&body)
					if body.PublicPortFirewallIDs[0] != "fw-default" {
						t.Errorf("expected firewall fw-default, got %v", body.PublicPortFirewallIDs)
					}
					return jsonResponse(http.StatusCreated, `{"id":"srv-789","status":"running","state":"running"}`), nil
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/servers/srv-789"):
					return jsonResponse(http.StatusOK, `{"id":"srv-789","status":"running","state":"running"}`), nil
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					return jsonResponse(http.StatusInternalServerError, `{}`), nil
				}
			},
		},
	}

	instanceID, err := p.CreateInstance(ctx, "cb-node-2", "small-1", "", "", "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if instanceID != "clouding/es-bcn-1/srv-789" {
		t.Errorf("instanceID = %q, want clouding/es-bcn-1/srv-789", instanceID)
	}
}

func TestDeleteInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/servers/srv-123") {
					return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	err = p.DeleteInstance(ctx, "clouding/es-bcn-1/srv-123")
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

func TestIsInstanceRunning_Running(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/servers/srv-1") {
					return jsonResponse(http.StatusOK, `{"id":"srv-1","status":"running","state":"running"}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "clouding/es-bcn-1/srv-1")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if !running {
		t.Error("expected running=true")
	}
}

func TestIsInstanceRunning_NotRunning(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return jsonResponse(http.StatusOK, `{"id":"srv-2","status":"stopped","state":"stopped"}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "clouding/es-bcn-1/srv-2")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if running {
		t.Error("expected running=false")
	}
}

func TestClose(t *testing.T) {
	p, _ := NewProvisioner(context.Background(), "key", "")
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
