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

package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/diegobraga/cloudburst-autoscaler/internal/metrics"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	apiBaseURL = "https://api.tailscale.com/api/v2"
)

// Client creates ephemeral auth keys via the Tailscale API.
type Client struct {
	apiToken string
	client   *http.Client
}

// NewClient returns a Tailscale API client using the given API token (tskey-api-...).
func NewClient(apiToken string) *Client {
	return &Client{
		apiToken: apiToken,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// CreateKeyRequest is the request body for creating an auth key.
// See https://tailscale.com/kb/1101/api#post-tailnet-keys
type CreateKeyRequest struct {
	Capabilities  KeyCapabilities `json:"capabilities"`
	ExpirySeconds int             `json:"expirySeconds,omitempty"`
}

// KeyCapabilities describes what the key can do.
// Exactly one capability scope must be populated.
type KeyCapabilities struct {
	Devices KeyDeviceCreate `json:"devices,omitempty"`
}

// KeyDeviceCreate configures device-creation capability.
type KeyDeviceCreate struct {
	Create KeyDeviceCreateParams `json:"create"`
}

// KeyDeviceCreateParams configures ephemeral and reusable behavior.
type KeyDeviceCreateParams struct {
	Reusable  bool `json:"reusable"`
	Ephemeral bool `json:"ephemeral"`
}

// CreateKeyResponse is the response from the keys endpoint.
type CreateKeyResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// apiError represents a non-2xx response from the Tailscale API.
type apiError struct {
	statusCode int
	message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("tailscale API %d: %s", e.statusCode, e.message)
}

// isRetryable returns true for transient errors: network/transport failures
// and 5xx server errors.
func isRetryable(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.statusCode >= 500
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// CreateEphemeralKey creates a single-use ephemeral auth key for a device.
// The key is not reusable and nodes using it are removed from the tailnet when they logout.
// hostname is optional (can be empty); some Tailscale backends use it for display.
func (c *Client) CreateEphemeralKey(ctx context.Context, hostname string) (authKey string, err error) {
	start := time.Now()
	defer func() {
		metrics.TailscaleRequestDurationSeconds.Observe(time.Since(start).Seconds())
	}()

	reqBody := CreateKeyRequest{
		Capabilities: KeyCapabilities{
			Devices: KeyDeviceCreate{
				Create: KeyDeviceCreateParams{
					Reusable:  false,
					Ephemeral: true,
				},
			},
		},
		ExpirySeconds: 86400, // 24h; key is single-use so only matters until VM uses it
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal create key request: %w", err)
	}

	var result string
	err = provutil.WithRetry(ctx, 3, time.Second, isRetryable, func() error {
		endpoint := apiBaseURL + "/tailnet/-/keys"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiToken)

		resp, err := c.client.Do(req)
		if err != nil {
			return fmt.Errorf("request: %w", err)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return &apiError{statusCode: resp.StatusCode, message: string(respBody)}
		}

		var keyResp CreateKeyResponse
		if err := json.Unmarshal(respBody, &keyResp); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
		if keyResp.Key == "" {
			return fmt.Errorf("tailscale API returned empty key")
		}
		result = keyResp.Key
		return nil
	})
	if err != nil {
		return "", err
	}
	return result, nil
}
