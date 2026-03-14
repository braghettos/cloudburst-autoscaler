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

package contabo

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

	_, err := NewProvisioner(ctx, "", "secret", "user", "pass")
	if err == nil {
		t.Fatal("expected error for empty clientID")
	}

	_, err = NewProvisioner(ctx, "cid", "csecret", "user", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMapRegion(t *testing.T) {
	tests := []struct {
		region  string
		want    string
		wantErr bool
	}{
		{"eu-de-1", "EU", false},
		{"eu-de-2", "EU", false},
		{"eu-gb-1", "UK", false},
		{"eu-de-3", "EU", false},
		{"eu-fr-1", "", true},
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

func TestMapProductID(t *testing.T) {
	tests := []struct {
		instanceType string
		want         string
		wantErr      bool
	}{
		{"cloud-vps-10", "V91", false},
		{"cloud-vds-s", "V8", false},
		{"unknown", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.instanceType, func(t *testing.T) {
			got, err := mapProductID(tt.instanceType)
			if (err != nil) != tt.wantErr {
				t.Errorf("mapProductID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("mapProductID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "cid", "csecret", "user", "pass")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "openid-connect/token") {
					return jsonResponse(http.StatusOK, `{"access_token":"tok-123","expires_in":3600}`), nil
				}
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/compute/instances") {
					var body createInstanceRequest
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					if body.ProductID != "V91" {
						t.Errorf("productId = %q, want V91", body.ProductID)
					}
					if body.Region != "EU" {
						t.Errorf("region = %q, want EU", body.Region)
					}
					return jsonResponse(http.StatusCreated, `{"data":[{"instanceId":456,"status":"running"}]}`), nil
				}
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/compute/instances/456") {
					return jsonResponse(http.StatusOK, `{"data":{"instanceId":456,"status":"running"}}`), nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	instanceID, err := p.CreateInstance(ctx, "cb-node-1", "cloud-vps-10", "eu-de-1", "", "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if instanceID != "contabo/eu-de-1/456" {
		t.Errorf("instanceID = %q, want contabo/eu-de-1/456", instanceID)
	}
}

func TestDeleteInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "cid", "csecret", "user", "pass")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "openid-connect/token") {
					return jsonResponse(http.StatusOK, `{"access_token":"tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/cancel") {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				// After cancel, provisioner polls until instance is gone; return 404 to confirm deletion
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/compute/instances/789") {
					return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	err = p.DeleteInstance(ctx, "contabo/eu-de-1/789")
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

func TestDeleteInstance_WaitsForCancelledStatus(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "cid", "csecret", "user", "pass")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	statusPollCount := 0
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "openid-connect/token") {
					return jsonResponse(http.StatusOK, `{"access_token":"tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/cancel") {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				// First poll returns running, second returns cancelled (provisioner waits until confirmed)
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/compute/instances/888") {
					statusPollCount++
					if statusPollCount == 1 {
						return jsonResponse(http.StatusOK, `{"data":{"instanceId":888,"status":"running"}}`), nil
					}
					return jsonResponse(http.StatusOK, `{"data":{"instanceId":888,"status":"cancelled"}}`), nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	err = p.DeleteInstance(ctx, "contabo/eu-de-1/888")
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if statusPollCount < 2 {
		t.Errorf("expected at least 2 status polls (running then cancelled), got %d", statusPollCount)
	}
}

func TestIsInstanceRunning_Running(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "cid", "csecret", "user", "pass")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "openid-connect/token") {
					return jsonResponse(http.StatusOK, `{"access_token":"tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/compute/instances/111") {
					return jsonResponse(http.StatusOK, `{"data":{"instanceId":111,"status":"running"}}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "contabo/eu-de-1/111")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if !running {
		t.Error("expected running=true")
	}
}

func TestIsInstanceRunning_NotRunning(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "cid", "csecret", "user", "pass")
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "openid-connect/token") {
					return jsonResponse(http.StatusOK, `{"access_token":"tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodGet {
					return jsonResponse(http.StatusOK, `{"data":{"instanceId":222,"status":"stopped"}}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "contabo/eu-de-1/222")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if running {
		t.Error("expected running=false")
	}
}

func TestClose(t *testing.T) {
	p, _ := NewProvisioner(context.Background(), "cid", "csecret", "user", "pass")
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
