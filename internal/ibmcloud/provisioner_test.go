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

package ibmcloud

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

	_, err := NewProvisioner(ctx, "", "eu-de", "rg-123", "subnet-123", nil)
	if err == nil {
		t.Fatal("expected error for empty API key")
	}

	_, err = NewProvisioner(ctx, "key", "", "rg-123", "subnet-123", nil)
	if err == nil {
		t.Fatal("expected error for empty region")
	}

	_, err = NewProvisioner(ctx, "key", "eu-de", "", "subnet-123", nil)
	if err == nil {
		t.Fatal("expected error for empty resourceGroup")
	}

	_, err = NewProvisioner(ctx, "key", "eu-de", "rg-123", "subnet-123", nil)
	if err != nil {
		t.Fatalf("unexpected error with subnetID: %v", err)
	}

	_, err = NewProvisioner(ctx, "key", "eu-de", "rg-123", "", nil)
	if err != nil {
		t.Fatalf("unexpected error with empty subnetID (auto-create): %v", err)
	}
}

func TestCreateInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "eu-de", "rg-123", "subnet-456", nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Host, "iam.cloud.ibm.com") {
					return jsonResponse(http.StatusOK, `{"access_token":"iam-tok","expires_in":3600}`), nil
				}
				if strings.Contains(req.URL.Path, "/v1/subnets/subnet-456") {
					return jsonResponse(http.StatusOK, `{"id":"subnet-456","zone":{"name":"eu-de-1"},"vpc":{"id":"vpc-789"}}`), nil
				}
				if strings.Contains(req.URL.Path, "/v1/images") {
					return jsonResponse(http.StatusOK, `{"images":[{"id":"img-uuid","name":"ubuntu-22.04"}]}`), nil
				}
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/v1/instances") {
					var body createInstanceRequest
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					if body.Profile.Name != "cx2-2x4" {
						t.Errorf("profile = %q, want cx2-2x4", body.Profile.Name)
					}
					if body.VPC.ID != "vpc-789" {
						t.Errorf("vpc = %q, want vpc-789", body.VPC.ID)
					}
					return jsonResponse(http.StatusCreated, `{"id":"inst-abc","status":"running"}`), nil
				}
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v1/instances/inst-abc") {
					return jsonResponse(http.StatusOK, `{"id":"inst-abc","status":"running"}`), nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	instanceID, err := p.CreateInstance(ctx, "cb-node-1", "cx2-2x4", "eu-de", "", "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if instanceID != "ibm_cloud/eu-de/inst-abc" {
		t.Errorf("instanceID = %q, want ibm_cloud/eu-de/inst-abc", instanceID)
	}
}

func TestCreateInstance_AutoCreateVPCSubnet(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "eu-de", "rg-123", "", nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Host, "iam.cloud.ibm.com") {
					return jsonResponse(http.StatusOK, `{"access_token":"iam-tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v1/vpcs") {
					return jsonResponse(http.StatusOK, `{"vpcs":[]}`), nil
				}
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v1/subnets") {
					return jsonResponse(http.StatusOK, `{"subnets":[]}`), nil
				}
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/v1/vpcs") {
					return jsonResponse(http.StatusCreated, `{"id":"vpc-auto-123"}`), nil
				}
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/v1/subnets") {
					return jsonResponse(http.StatusCreated, `{"id":"subnet-auto-456"}`), nil
				}
				if strings.Contains(req.URL.Path, "/v1/instance/profiles") {
					return jsonResponse(http.StatusOK, `{"profiles":[{"name":"cx2-2x4","vcpu_count":{"value":2},"memory":{"value":4}}]}`), nil
				}
				if strings.Contains(req.URL.Path, "/v1/images") {
					return jsonResponse(http.StatusOK, `{"images":[{"id":"img-uuid","name":"ubuntu-22.04"}]}`), nil
				}
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/v1/instances") {
					var body createInstanceRequest
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					if body.PrimaryNetworkInterface.Subnet.ID != "subnet-auto-456" {
						t.Errorf("subnet = %q, want subnet-auto-456", body.PrimaryNetworkInterface.Subnet.ID)
					}
					if body.VPC.ID != "vpc-auto-123" {
						t.Errorf("vpc = %q, want vpc-auto-123", body.VPC.ID)
					}
					if body.Zone.Name != "eu-de-1" {
						t.Errorf("zone = %q, want eu-de-1", body.Zone.Name)
					}
					return jsonResponse(http.StatusCreated, `{"id":"inst-xyz","status":"running"}`), nil
				}
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v1/instances/inst-xyz") {
					return jsonResponse(http.StatusOK, `{"id":"inst-xyz","status":"running"}`), nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	instanceID, err := p.CreateInstance(ctx, "cb-node-2", "cx2-2x4", "eu-de", "", "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if instanceID != "ibm_cloud/eu-de/inst-xyz" {
		t.Errorf("instanceID = %q, want ibm_cloud/eu-de/inst-xyz", instanceID)
	}
}

func TestDeleteInstance_Success(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "eu-de", "rg-123", "subnet-456", nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Host, "iam.cloud.ibm.com") {
					return jsonResponse(http.StatusOK, `{"access_token":"iam-tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/v1/instances/inst-123") {
					return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	err = p.DeleteInstance(ctx, "ibm_cloud/eu-de/inst-123")
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

func TestIsInstanceRunning_Running(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "eu-de", "rg-123", "subnet-456", nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Host, "iam.cloud.ibm.com") {
					return jsonResponse(http.StatusOK, `{"access_token":"iam-tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v1/instances/inst-1") {
					return jsonResponse(http.StatusOK, `{"id":"inst-1","status":"running"}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "ibm_cloud/eu-de/inst-1")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if !running {
		t.Error("expected running=true")
	}
}

func TestIsInstanceRunning_NotRunning(t *testing.T) {
	ctx := context.Background()

	p, err := NewProvisioner(ctx, "test-key", "eu-de", "rg-123", "subnet-456", nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	p.httpClient = &http.Client{
		Transport: &mockTransport{
			roundTrip: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Host, "iam.cloud.ibm.com") {
					return jsonResponse(http.StatusOK, `{"access_token":"iam-tok","expires_in":3600}`), nil
				}
				if req.Method == http.MethodGet {
					return jsonResponse(http.StatusOK, `{"id":"inst-2","status":"stopped"}`), nil
				}
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			},
		},
	}

	running, err := p.IsInstanceRunning(ctx, "ibm_cloud/eu-de/inst-2")
	if err != nil {
		t.Fatalf("IsInstanceRunning: %v", err)
	}
	if running {
		t.Error("expected running=false")
	}
}

func TestClose(t *testing.T) {
	p, _ := NewProvisioner(context.Background(), "key", "eu-de", "rg", "subnet", nil)
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
