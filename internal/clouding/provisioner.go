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

// Package clouding provides a provisioner for Clouding.io (Spanish cloud provider).
// API docs: https://api.clouding.io
// Single region (Barcelona). Instance ID format: clouding/es-bcn-1/<serverId>
package clouding

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	InstanceIDPrefix   = "clouding/"
	APIBase            = "https://api.clouding.io/v1"
	DefaultRegion      = "es-bcn-1"
	createPollInterval = 5 * time.Second
	createPollTimeout  = 10 * time.Minute
)

// CloudBroker instance type -> (vcpu, ramGB) for flavor matching
var planSpecs = map[string]struct{ vcpu int; ram float64 }{
	"micro-1":  {1, 1},
	"micro-2":  {1, 2},
	"small-1":  {2, 4},
	"small-2":  {4, 8},
	"medium-1": {6, 16},
	"medium-2": {8, 32},
	"large-1":  {12, 48},
	"large-2":  {16, 64},
}

// Provisioner handles Clouding.io instance provisioning.
type Provisioner struct {
	apiKey     string
	firewallID string
	httpClient *http.Client
}

// NewProvisioner creates a new Clouding.io provisioner.
// firewallID is optional; if empty, the provisioner fetches the first available firewall at create time.
func NewProvisioner(ctx context.Context, apiKey, firewallID string) (*Provisioner, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("Clouding.io API key is required")
	}
	return &Provisioner{
		apiKey:     apiKey,
		firewallID: firewallID,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (p *Provisioner) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	url := APIBase + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

type flavorItem struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	VCPUs int     `json:"vcpus"`
	RAM   float64 `json:"ram"`
}

type flavorsResponse struct {
	Items []flavorItem `json:"items"`
}

func (p *Provisioner) fetchFlavors(ctx context.Context) ([]flavorItem, error) {
	resp, body, err := p.doRequest(ctx, http.MethodGet, "/sizes/flavors", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch flavors: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch flavors (status %d): %s", resp.StatusCode, string(body))
	}
	var fr flavorsResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return nil, fmt.Errorf("parse flavors: %w", err)
	}
	return fr.Items, nil
}

func (p *Provisioner) findFlavorID(ctx context.Context, instanceType string) (string, error) {
	spec, ok := planSpecs[instanceType]
	if !ok {
		return "", fmt.Errorf("unsupported instance type %q for Clouding (supported: micro-1, small-1, medium-1, large-1, etc.)", instanceType)
	}
	flavors, err := p.fetchFlavors(ctx)
	if err != nil {
		return "", err
	}
	for _, f := range flavors {
		if f.VCPUs == spec.vcpu && f.RAM >= spec.ram-0.5 && f.RAM <= spec.ram+0.5 {
			return f.ID, nil
		}
	}
	for _, f := range flavors {
		if f.VCPUs >= spec.vcpu && f.RAM >= spec.ram {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("no matching Clouding flavor for %s (%dvCPU, %.0fGB RAM)", instanceType, spec.vcpu, spec.ram)
}

type firewallItem struct {
	ID string `json:"id"`
}

type firewallsResponse struct {
	Items []firewallItem `json:"items"`
}

func (p *Provisioner) fetchFirewallID(ctx context.Context) (string, error) {
	if p.firewallID != "" {
		return p.firewallID, nil
	}
	resp, body, err := p.doRequest(ctx, http.MethodGet, "/firewalls", nil)
	if err != nil {
		return "", fmt.Errorf("fetch firewalls: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch firewalls (status %d): %s", resp.StatusCode, string(body))
	}
	var fr firewallsResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return "", fmt.Errorf("parse firewalls: %w", err)
	}
	if len(fr.Items) == 0 {
		return "", fmt.Errorf("no firewalls found; create a default firewall in Clouding portal or set spec.clouding.firewallId in NodeClass")
	}
	return fr.Items[0].ID, nil
}

func generatePassword() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 24)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

type createServerRequest struct {
	Name                 string                 `json:"name"`
	Hostname             string                 `json:"hostname"`
	FlavorID             string                 `json:"flavorId"`
	AccessConfiguration  map[string]interface{} `json:"accessConfiguration"`
	Volume               map[string]interface{} `json:"volume"`
	PublicPortFirewallIDs []string              `json:"publicPortFirewallIds"`
	UserData             string                 `json:"userData,omitempty"`
}

type serverResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	State  string `json:"state"`
}

// CreateInstance creates a Clouding.io instance.
// Returns instanceID in the form "clouding/es-bcn-1/<server-id>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, instanceType, regionOrZone, startupScript, imageFromConfig string) (string, error) {
	var instanceID string
	err := provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		var retryErr error
		instanceID, retryErr = p.createInstanceOnce(ctx, name, instanceType, regionOrZone, startupScript, imageFromConfig)
		return retryErr
	})
	if err != nil {
		return "", err
	}
	return instanceID, nil
}

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, instanceType, regionOrZone, startupScript, imageFromConfig string) (string, error) {
	flavorID, err := p.findFlavorID(ctx, instanceType)
	if err != nil {
		return "", err
	}
	firewallID, err := p.fetchFirewallID(ctx)
	if err != nil {
		return "", err
	}

	userData := ""
	if startupScript != "" {
		userData = base64.StdEncoding.EncodeToString([]byte(startupScript))
	}

	reqBody := createServerRequest{
		Name:   name,
		Hostname: name,
		FlavorID: flavorID,
		AccessConfiguration: map[string]interface{}{
			"password":     generatePassword(),
			"savePassword": false,
		},
		Volume: map[string]interface{}{
			"source": "image",
			"ssdGb":  20,
		},
		PublicPortFirewallIDs: []string{firewallID},
		UserData:              userData,
	}
	if imageFromConfig != "" {
		reqBody.Volume["imageId"] = imageFromConfig
	}

	resp, body, err := p.doRequest(ctx, http.MethodPost, "/servers", reqBody)
	if err != nil {
		return "", fmt.Errorf("create server: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create server (status %d): %s", resp.StatusCode, string(body))
	}

	var sResp serverResponse
	if err := json.Unmarshal(body, &sResp); err != nil {
		return "", fmt.Errorf("parse create server response: %w", err)
	}
	if sResp.ID == "" {
		return "", fmt.Errorf("Clouding create returned empty server id")
	}

	if err := p.waitForActive(ctx, sResp.ID); err != nil {
		return "", err
	}

	region := DefaultRegion
	if regionOrZone != "" {
		region = regionOrZone
	}
	return fmt.Sprintf("%s%s/%s", InstanceIDPrefix, region, sResp.ID), nil
}

func (p *Provisioner) waitForActive(ctx context.Context, serverID string) error {
	deadline := time.Now().Add(createPollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled: %w", ctx.Err())
		default:
		}

		status, err := p.getServerStatusByID(ctx, serverID)
		if err != nil {
			return err
		}
		switch strings.ToLower(status) {
		case "running", "active":
			return nil
		case "error", "failed":
			return &cberrors.InstanceFailedError{Reason: "server entered error state during creation"}
		}
		time.Sleep(createPollInterval)
	}
	return fmt.Errorf("server did not become active within %v", createPollTimeout)
}

func (p *Provisioner) getServerStatusByID(ctx context.Context, serverID string) (string, error) {
	path := fmt.Sprintf("/servers/%s", serverID)
	resp, body, err := p.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("get server: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "deleted", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get server (status %d): %s", resp.StatusCode, string(body))
	}

	var sResp serverResponse
	if err := json.Unmarshal(body, &sResp); err != nil {
		return "", fmt.Errorf("parse server response: %w", err)
	}
	if sResp.Status != "" {
		return sResp.Status, nil
	}
	return sResp.State, nil
}

// DeleteInstance deletes a Clouding.io instance.
// instanceID must be in the form "clouding/<region>/<server-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, serverIDStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/servers/%s", serverIDStr)
	resp, body, err := p.doRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete server (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// IsInstanceRunning returns true if the server is in running/active state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, serverIDStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return false, err
	}

	resp, body, err := p.doRequest(ctx, http.MethodGet, "/servers/"+serverIDStr, nil)
	if err != nil {
		return false, fmt.Errorf("get server: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("get server (status %d): %s", resp.StatusCode, string(body))
	}

	var sResp serverResponse
	if err := json.Unmarshal(body, &sResp); err != nil {
		return false, fmt.Errorf("parse server response: %w", err)
	}

	status := strings.ToLower(sResp.Status)
	if status == "" {
		status = strings.ToLower(sResp.State)
	}

	switch status {
	case "error", "failed":
		return false, &cberrors.InstanceFailedError{Reason: "server status: " + status}
	case "deleted", "":
		return false, nil
	}
	return status == "active" || status == "running", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}
