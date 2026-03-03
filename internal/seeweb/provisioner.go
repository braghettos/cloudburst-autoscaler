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

package seeweb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	InstanceIDPrefix = "seeweb/"
	DefaultImage     = "ubuntu-2204"
	BaseURL          = "https://api.seeweb.it/ecs/v2"
	MaxRetries       = 3
	RetryDelay       = 2 * time.Second
)

// Provisioner handles Seeweb Cloud Server provisioning
type Provisioner struct {
	client   *http.Client
	username string
	password string
	token    string
	tokenExp time.Time
}

// NewProvisioner creates a new Seeweb provisioner.
func NewProvisioner(ctx context.Context, username, password string) (*Provisioner, error) {
	if username == "" || password == "" {
		return nil, fmt.Errorf("Seeweb username and password are required")
	}

	p := &Provisioner{
		client:   &http.Client{Timeout: 30 * time.Second},
		username: username,
		password: password,
	}
	// Ensure we have a valid token
	if _, err := p.ensureToken(ctx); err != nil {
		return nil, fmt.Errorf("failed to authenticate with Seeweb: %w", err)
	}
	return p, nil
}

func (p *Provisioner) ensureToken(ctx context.Context) (string, error) {
	if p.token != "" && time.Now().Before(p.tokenExp) {
		return p.token, nil
	}
	body, _ := json.Marshal(map[string]string{
		"username": p.username,
		"password": p.password,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", BaseURL+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		Token  string `json:"token"`
		Expire int64  `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("failed to decode login response: %w", err)
	}
	if data.Token == "" {
		return "", fmt.Errorf("login did not return a token")
	}
	p.token = data.Token
	p.tokenExp = time.Unix(data.Expire, 0).Add(-60 * time.Second) // refresh 1 min before expiry
	return p.token, nil
}

// CreateInstance creates a Seeweb Cloud Server with the given plan, location, and cloud-init script.
// Returns instanceID in the form "seeweb/<server-name>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, instanceType, regionOrZone, startupScript, imageFromConfig string) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(RetryDelay * time.Duration(1<<uint(attempt-1))):
			}
		}
		instanceID, err := p.createInstanceOnce(ctx, name, instanceType, regionOrZone, startupScript, imageFromConfig)
		if err == nil {
			return instanceID, nil
		}
		if !isRetryableError(err) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("failed after %d attempts: %w", MaxRetries+1, lastErr)
}

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, plan, location, startupScript, imageFromConfig string) (string, error) {
	token, err := p.ensureToken(ctx)
	if err != nil {
		return "", err
	}

	image := imageFromConfig
	if image == "" {
		image = DefaultImage
	}

	payload := map[string]interface{}{
		"plan":     plan,
		"image":   image,
		"location": location,
		"notes":   name,
	}
	if startupScript != "" {
		payload["user_customize"] = startupScript
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", BaseURL+"/servers", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("Seeweb API error %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		Status string `json:"status"`
		Server struct {
			Name string `json:"name"`
		} `json:"server"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to decode create response: %w", err)
	}
	if result.Server.Name == "" {
		return "", fmt.Errorf("create response did not contain server name")
	}

	return InstanceIDPrefix + result.Server.Name, nil
}

// DeleteInstance deletes a Seeweb Cloud Server.
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	serverName, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	token, err := p.ensureToken(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "DELETE", BaseURL+"/servers/"+serverName, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil // already deleted
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Seeweb delete error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// IsInstanceRunning returns true if the server is in Booted/Running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	serverName, err := parseInstanceID(instanceID)
	if err != nil {
		return false, err
	}

	token, err := p.ensureToken(ctx)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", BaseURL+"/servers/"+serverName+"/status", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("Seeweb status error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var data struct {
		Server struct {
			CurrentStatus string `json:"current_status"`
		} `json:"server"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false, fmt.Errorf("failed to decode status response: %w", err)
	}
	return data.Server.CurrentStatus == "Running" || data.Server.CurrentStatus == "Booted", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}

func parseInstanceID(instanceID string) (string, error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", fmt.Errorf("invalid Seeweb instance ID format: %s", instanceID)
	}
	return strings.TrimPrefix(instanceID, InstanceIDPrefix), nil
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, r := range []string{"rate limit", "503", "502", "500", "timeout", "unavailable"} {
		if strings.Contains(s, r) {
			return true
		}
	}
	return false
}
