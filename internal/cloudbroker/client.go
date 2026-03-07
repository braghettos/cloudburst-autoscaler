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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/diegobraga/cloudburst-autoscaler/internal/metrics"
)

const (
	// DefaultBaseURL is the default CloudBroker base URL
	DefaultBaseURL = "http://localhost:8000"
	// DefaultTimeoutSeconds is the default timeout in seconds
	DefaultTimeoutSeconds = 5
	// MaxRetries is the maximum number of retry attempts (1 initial + 3 retries = 4 attempts total)
	MaxRetries = 3
)

// Client is the CloudBroker API client
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// RecommendationRequest represents a request for instance recommendations
type RecommendationRequest struct {
	MinVCPU            int32    `json:"min_vcpu"`
	MinRAMGb           float64  `json:"min_ram_gb"`
	Arch               *string  `json:"arch,omitempty"` // Optional: only set if pod specifies architecture requirement
	RegionConstraint   string   `json:"region_constraint"`
	MaxPriceEurPerHour float64  `json:"max_price_eur_per_hour"`
	Limit              int32    `json:"limit"`
	AllowedProviders   []string `json:"allowed_providers,omitempty"`

	// TCO placement context (optional): when set, CloudBroker factors in egress costs when ranking
	DataSourceProvider       string  `json:"data_source_provider,omitempty"`
	DataSourceRegion         string  `json:"data_source_region,omitempty"`
	EstimatedEgressGbPerHour float64 `json:"estimated_egress_gb_per_hour,omitempty"`
	MaxTCOEurPerHour         float64 `json:"max_tco_eur_per_hour,omitempty"`

	// ExcludedInstanceTypes excludes instance types (e.g. after provisioning failure)
	ExcludedInstanceTypes []string `json:"excluded_instance_types,omitempty"`
}

// RecommendationExplain is the nested explain object from CloudBroker (contains price and TCO breakdown)
type RecommendationExplain struct {
	PriceEurPerHour      float64 `json:"price_eur_per_hour"`
	TCOEurPerHour        float64 `json:"tco_eur_per_hour,omitempty"`
	EgressCostEurPerHour float64 `json:"egress_cost_eur_per_hour,omitempty"`
}

// Recommendation represents a single instance recommendation (field names match CloudBroker API response)
type Recommendation struct {
	Provider        string                 `json:"provider_slug"`
	Region          string                 `json:"region_slug"`
	InstanceType    string                 `json:"instance_type_name"`
	VCPU            int32                  `json:"vcpu"`
	RAMGb           float64                `json:"ram_gb"`
	PriceEurPerHour float64                `json:"price_eur_per_hour"`
	Explain         *RecommendationExplain `json:"explain,omitempty"`
}

// RecommendationsResponse represents the API response
type RecommendationsResponse struct {
	Recommendations []Recommendation `json:"recommendations"`
}

// RecommendationResult represents the chosen recommendation
type RecommendationResult struct {
	Provider        string
	Region          string
	InstanceType    string
	VCPU            int32
	RAMGb           float64
	PriceEurPerHour float64
}

// NewClient creates a new CloudBroker client with configuration from environment variables
func NewClient() *Client {
	baseURL := os.Getenv("CLOUDBROKER_BASEURL")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	timeoutSeconds := DefaultTimeoutSeconds
	if timeoutStr := os.Getenv("CLOUDBROKER_TIMEOUT_SECONDS"); timeoutStr != "" {
		if parsed, err := strconv.Atoi(timeoutStr); err == nil && parsed > 0 {
			timeoutSeconds = parsed
		}
	}

	httpClient := &http.Client{
		Timeout: time.Duration(timeoutSeconds) * time.Second,
	}

	return &Client{
		baseURL:    baseURL,
		apiKey:     os.Getenv("CLOUDBROKER_API_KEY"),
		httpClient: httpClient,
	}
}

// Recommend requests instance recommendations and returns a list of recommendations (up to req.Limit).
// The first is the preferred choice; the rest can be used as fallbacks if instance creation fails.
func (c *Client) Recommend(ctx context.Context, req *RecommendationRequest) ([]RecommendationResult, error) {
	var lastErr error

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			// Wait before retry (exponential backoff: 500ms, 1s, 2s)
			waitTime := time.Duration(500*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
			case <-time.After(waitTime):
			}
		}

		start := time.Now()
		results, err := c.recommendOnce(ctx, req)
		metrics.CloudBrokerRequestDurationSeconds.Observe(time.Since(start).Seconds())
		if err == nil {
			return results, nil
		}

		lastErr = err
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", MaxRetries+1, lastErr)
}

// recommendOnce makes a single recommendation request and returns up to req.Limit recommendations.
func (c *Client) recommendOnce(ctx context.Context, req *RecommendationRequest) ([]RecommendationResult, error) {
	url := fmt.Sprintf("%s/api/recommendations", c.baseURL)

	// Marshal request body
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set request body and headers
	httpReq.Body = io.NopCloser(bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	// Make request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Read response body
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse response
	var apiResp RecommendationsResponse
	if err := json.Unmarshal(bodyBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(apiResp.Recommendations) == 0 {
		return nil, fmt.Errorf("no recommendations returned")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}
	n := int(limit)
	if n > len(apiResp.Recommendations) {
		n = len(apiResp.Recommendations)
	}
	results := make([]RecommendationResult, 0, n)
	for i := 0; i < n; i++ {
		r := apiResp.Recommendations[i]
		price := r.PriceEurPerHour
		if r.Explain != nil {
			price = r.Explain.PriceEurPerHour
		}
		results = append(results, RecommendationResult{
			Provider:        r.Provider,
			Region:          r.Region,
			InstanceType:    r.InstanceType,
			VCPU:            r.VCPU,
			RAMGb:           r.RAMGb,
			PriceEurPerHour: price,
		})
	}
	return results, nil
}
