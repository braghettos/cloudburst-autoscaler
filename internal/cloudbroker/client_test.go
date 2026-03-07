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

package cloudbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// stringPtr returns a pointer to the given string value.
func stringPtr(s string) *string { return &s }

func TestNewClient(t *testing.T) {
	tests := []struct {
		name            string
		baseURLEnv      string
		timeoutEnv      string
		expectedBaseURL string
		expectedTimeout time.Duration
	}{
		{
			name:            "default values",
			baseURLEnv:      "",
			timeoutEnv:      "",
			expectedBaseURL: DefaultBaseURL,
			expectedTimeout: DefaultTimeoutSeconds * time.Second,
		},
		{
			name:            "custom base URL",
			baseURLEnv:      "http://example.com:9000",
			timeoutEnv:      "",
			expectedBaseURL: "http://example.com:9000",
			expectedTimeout: DefaultTimeoutSeconds * time.Second,
		},
		{
			name:            "custom timeout",
			baseURLEnv:      "",
			timeoutEnv:      "10",
			expectedBaseURL: DefaultBaseURL,
			expectedTimeout: 10 * time.Second,
		},
		{
			name:            "both custom",
			baseURLEnv:      "http://test.com",
			timeoutEnv:      "15",
			expectedBaseURL: "http://test.com",
			expectedTimeout: 15 * time.Second,
		},
		{
			name:            "invalid timeout falls back to default",
			baseURLEnv:      "",
			timeoutEnv:      "invalid",
			expectedBaseURL: DefaultBaseURL,
			expectedTimeout: DefaultTimeoutSeconds * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original env vars
			originalBaseURL := os.Getenv("CLOUDBROKER_BASEURL")
			originalTimeout := os.Getenv("CLOUDBROKER_TIMEOUT_SECONDS")

			// Set test env vars
			if tt.baseURLEnv != "" {
				os.Setenv("CLOUDBROKER_BASEURL", tt.baseURLEnv)
			} else {
				os.Unsetenv("CLOUDBROKER_BASEURL")
			}
			if tt.timeoutEnv != "" {
				os.Setenv("CLOUDBROKER_TIMEOUT_SECONDS", tt.timeoutEnv)
			} else {
				os.Unsetenv("CLOUDBROKER_TIMEOUT_SECONDS")
			}

			// Create client
			client := NewClient()

			// Verify base URL
			if client.baseURL != tt.expectedBaseURL {
				t.Errorf("expected baseURL %s, got %s", tt.expectedBaseURL, client.baseURL)
			}

			// Verify timeout
			if client.httpClient.Timeout != tt.expectedTimeout {
				t.Errorf("expected timeout %v, got %v", tt.expectedTimeout, client.httpClient.Timeout)
			}

			// Restore original env vars
			if originalBaseURL != "" {
				os.Setenv("CLOUDBROKER_BASEURL", originalBaseURL)
			} else {
				os.Unsetenv("CLOUDBROKER_BASEURL")
			}
			if originalTimeout != "" {
				os.Setenv("CLOUDBROKER_TIMEOUT_SECONDS", originalTimeout)
			} else {
				os.Unsetenv("CLOUDBROKER_TIMEOUT_SECONDS")
			}
		})
	}
}

func TestRecommend_Success(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/recommendations" {
			t.Errorf("expected path /api/recommendations, got %s", r.URL.Path)
		}

		// Verify request body
		var req RecommendationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}

		// Verify request fields
		if req.MinVCPU != 2 {
			t.Errorf("expected MinVCPU 2, got %d", req.MinVCPU)
		}
		if req.MinRAMGb != 4.0 {
			t.Errorf("expected MinRAMGb 4.0, got %f", req.MinRAMGb)
		}
		if req.Arch == nil || *req.Arch != "x86_64" {
			t.Errorf("expected Arch x86_64, got %v", req.Arch)
		}

		// Return response
		response := RecommendationsResponse{
			Recommendations: []Recommendation{
				{
					Provider:        "gcp",
					Region:          "europe-west1",
					InstanceType:    "n1-standard-2",
					VCPU:            2,
					RAMGb:           7.5,
					PriceEurPerHour: 0.10,
				},
				{
					Provider:        "aws",
					Region:          "eu-west-1",
					InstanceType:    "t3.medium",
					VCPU:            2,
					RAMGb:           4.0,
					PriceEurPerHour: 0.08,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create client with test server URL
	client := &Client{
		baseURL:    server.URL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	// Make recommendation request
	req := RecommendationRequest{
		MinVCPU:            2,
		MinRAMGb:           4.0,
		Arch:               stringPtr("x86_64"),
		RegionConstraint:   "EU",
		MaxPriceEurPerHour: 0.15,
		Limit:              10,
	}

	results, err := client.Recommend(context.Background(), &req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one recommendation, got none")
	}
	result := results[0]

	// Verify result (should be top recommendation)
	if result.Provider != "gcp" {
		t.Errorf("expected provider gcp, got %s", result.Provider)
	}
	if result.Region != "europe-west1" {
		t.Errorf("expected region europe-west1, got %s", result.Region)
	}
	if result.InstanceType != "n1-standard-2" {
		t.Errorf("expected instance type n1-standard-2, got %s", result.InstanceType)
	}
	if result.VCPU != 2 {
		t.Errorf("expected VCPU 2, got %d", result.VCPU)
	}
	if result.RAMGb != 7.5 {
		t.Errorf("expected RAMGb 7.5, got %f", result.RAMGb)
	}
	if result.PriceEurPerHour != 0.10 {
		t.Errorf("expected PriceEurPerHour 0.10, got %f", result.PriceEurPerHour)
	}
}

func TestRecommend_NoRecommendations(t *testing.T) {
	// Create test server returning empty recommendations
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := RecommendationsResponse{
			Recommendations: []Recommendation{},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	req := RecommendationRequest{
		MinVCPU:            2,
		MinRAMGb:           4.0,
		Arch:               stringPtr("x86_64"),
		RegionConstraint:   "EU",
		MaxPriceEurPerHour: 0.15,
		Limit:              10,
	}

	_, err := client.Recommend(context.Background(), &req)
	if err == nil {
		t.Fatal("expected error for empty recommendations, got nil")
	}
	if err.Error() != "failed after 4 attempts: no recommendations returned" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRecommend_HTTPError(t *testing.T) {
	// Create test server returning error status
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	req := RecommendationRequest{
		MinVCPU:            2,
		MinRAMGb:           4.0,
		Arch:               stringPtr("x86_64"),
		RegionConstraint:   "EU",
		MaxPriceEurPerHour: 0.15,
		Limit:              10,
	}

	_, err := client.Recommend(context.Background(), &req)
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
	if err.Error() != "failed after 4 attempts: unexpected status code 500: internal server error" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRecommend_RetryOnFailure(t *testing.T) {
	attemptCount := 0

	// Create test server that fails first time, succeeds second time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptCount++
		if attemptCount < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("temporary error"))
			return
		}

		response := RecommendationsResponse{
			Recommendations: []Recommendation{
				{
					Provider:        "gcp",
					Region:          "europe-west1",
					InstanceType:    "n1-standard-2",
					VCPU:            2,
					RAMGb:           7.5,
					PriceEurPerHour: 0.10,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	req := RecommendationRequest{
		MinVCPU:            2,
		MinRAMGb:           4.0,
		Arch:               stringPtr("x86_64"),
		RegionConstraint:   "EU",
		MaxPriceEurPerHour: 0.15,
		Limit:              10,
	}

	results, err := client.Recommend(context.Background(), &req)
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one recommendation, got none")
	}
	if results[0].Provider != "gcp" {
		t.Errorf("expected provider gcp, got %s", results[0].Provider)
	}

	if attemptCount != 2 {
		t.Errorf("expected 2 attempts, got %d", attemptCount)
	}
}

func TestRecommend_InvalidJSON(t *testing.T) {
	// Create test server returning invalid JSON
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("invalid json"))
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	req := RecommendationRequest{
		MinVCPU:            2,
		MinRAMGb:           4.0,
		Arch:               stringPtr("x86_64"),
		RegionConstraint:   "EU",
		MaxPriceEurPerHour: 0.15,
		Limit:              10,
	}

	_, err := client.Recommend(context.Background(), &req)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestRecommend_ContextCancellation(t *testing.T) {
	// Create test server with delay
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		response := RecommendationsResponse{
			Recommendations: []Recommendation{
				{
					Provider:        "gcp",
					Region:          "europe-west1",
					InstanceType:    "n1-standard-2",
					VCPU:            2,
					RAMGb:           7.5,
					PriceEurPerHour: 0.10,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	req := RecommendationRequest{
		MinVCPU:            2,
		MinRAMGb:           4.0,
		Arch:               stringPtr("x86_64"),
		RegionConstraint:   "EU",
		MaxPriceEurPerHour: 0.15,
		Limit:              10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := client.Recommend(ctx, &req)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}
