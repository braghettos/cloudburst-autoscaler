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

package gcore

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

	_, err := NewProvisioner(ctx, "", 123)
	if err == nil {
		t.Fatal("expected error for empty API key")
	}

	_, err = NewProvisioner(ctx, "key", 0)
	if err == nil {
		t.Fatal("expected error for zero project ID")
	}

	p, err := NewProvisioner(ctx, "key", 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provisioner")
	}
}

func TestCreateInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", 100)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") != "apikey test-key" {
					t.Errorf("missing or wrong Authorization header")
				}
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/cloud/v1/regions"):
					return jsonResponse(http.StatusOK, `{"results":[{"id":1,"display_name":"Luxembourg 1","keystone_name":"lux-1"}]}`), nil
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/cloud/v1/images/"):
					return jsonResponse(http.StatusOK, `{"results":[{"id":"img-uuid-123","name":"ubuntu-22.04-lts"}]}`), nil
				case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/cloud/v2/instances/100/1"):
					var body createInstanceRequest
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					if body.Flavor != "g1-standard-2-4" {
						t.Errorf("flavor = %q, want g1-standard-2-4", body.Flavor)
					}
					return jsonResponse(http.StatusAccepted, `{"tasks":["task-abc"]}`), nil
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/cloud/v1/tasks/task-abc"):
					return jsonResponse(http.StatusOK, `{"id":"task-abc","state":"FINISHED","result":{"instance_id":"inst-xyz"}}`), nil
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					return jsonResponse(http.StatusInternalServerError, `{}`), nil
				}
			},
		},
	}

	instanceID, err := p.CreateInstance(ctx, "cb-node-1", "g1-standard-2-4", "lux-1", "", "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if instanceID != "gcore/1/inst-xyz" {
		t.Errorf("instanceID = %q, want gcore/1/inst-xyz", instanceID)
	}
}

func TestDeleteInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", 100)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/cloud/v1/instances/100/1/inst-123") {
					return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	err = p.DeleteInstance(ctx, "gcore/1/inst-123")
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

func TestIsInstanceRunning_Running(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", 100)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/cloud/v1/instances/100/1/inst-1") {
					return jsonResponse(http.StatusOK, `{"data":{"id":"inst-1","status":"running","state":"running"}}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "gcore/1/inst-1")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if !running {
		t.Error("expected running=true")
	}
}

func TestIsInstanceRunning_NotRunning(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", 100)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return jsonResponse(http.StatusOK, `{"data":{"id":"inst-2","status":"stopped","state":"stopped"}}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "gcore/1/inst-2")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if running {
		t.Error("expected running=false")
	}
}

func TestClose(t *testing.T) {
	p, _ := NewProvisioner(context.Background(), "key", 1)
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
