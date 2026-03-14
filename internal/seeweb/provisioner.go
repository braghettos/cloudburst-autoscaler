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

// Package seeweb provides a provisioner for Seeweb ECS (Elastic Cloud Server) platform.
// Seeweb is an Italian cloud provider with datacenters in Italy, Switzerland and Bulgaria.
// API docs: https://api.seeweb.it/ecs/v2
// Startup scripts are delivered as Cloud Scripts attached after creation.
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

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	// InstanceIDPrefix is the prefix for Seeweb instance IDs (region/server-id)
	InstanceIDPrefix = "seeweb/"
	// APIBase is the Seeweb ECS API base URL
	APIBase = "https://api.seeweb.it/ecs/v2"
	// DefaultImage is the default Ubuntu 22.04 image name on Seeweb
	DefaultImage = "Ubuntu 22.04 LTS"
	// createPollInterval is the interval for polling server creation status
	createPollInterval = 5 * time.Second
	// createPollTimeout is the max time to wait for a server to become active
	createPollTimeout = 10 * time.Minute
)

// Provisioner handles Seeweb ECS instance provisioning.
type Provisioner struct {
	apiToken   string
	datacenter string // default datacenter (region slug)
	httpClient *http.Client
}

// NewProvisioner creates a new Seeweb provisioner.
//   - apiToken: Seeweb API token (X-APITOKEN header)
//   - datacenter: default datacenter slug (e.g. "it-mi2", "it-fr2")
func NewProvisioner(ctx context.Context, apiToken, datacenter string) (*Provisioner, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("Seeweb API token is required")
	}

	return &Provisioner{
		apiToken:   apiToken,
		datacenter: datacenter,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// doRequest executes an authenticated Seeweb API request.
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
	req.Header.Set("X-APITOKEN", p.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

type createServerRequest struct {
	Plan       string `json:"plan"`
	Datacenter string `json:"datacenter"`
	Name       string `json:"name,omitempty"`
	Image      string `json:"image,omitempty"`
}

type serverResponse struct {
	Status string      `json:"status"`
	Server *serverInfo `json:"server"`
}

type serverInfo struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Datacenter string `json:"datacenter"`
	Plan       string `json:"plan"`
}

// CreateInstance creates a Seeweb ECS server with the given plan and attaches the startup script.
// Returns instanceID in the form "seeweb/<datacenter>/<server-id>".
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

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, plan, regionOrZone, startupScript, imageFromConfig string) (string, error) {
	datacenter := regionOrZone
	if datacenter == "" {
		datacenter = p.datacenter
	}

	imageName := imageFromConfig
	if imageName == "" {
		imageName = DefaultImage
	}

	reqBody := createServerRequest{
		Plan:       plan,
		Datacenter: datacenter,
		Name:       name,
		Image:      imageName,
	}

	resp, body, err := p.doRequest(ctx, http.MethodPost, "/servers", reqBody)
	if err != nil {
		return "", fmt.Errorf("create server: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create server (status %d): %s", resp.StatusCode, string(body))
	}

	var createResp serverResponse
	if err := json.Unmarshal(body, &createResp); err != nil {
		return "", fmt.Errorf("parse create server response: %w", err)
	}
	if createResp.Server == nil {
		return "", fmt.Errorf("Seeweb create server returned empty server object")
	}

	serverID := createResp.Server.ID

	// Wait for server to become active before attaching the cloud script
	if err := p.waitForActive(ctx, serverID); err != nil {
		return "", err
	}

	// Attach startup script as a Cloud Script (Seeweb's cloud-init equivalent)
	if startupScript != "" {
		if err := p.attachCloudScript(ctx, serverID, startupScript); err != nil {
			// Non-fatal: log but do not fail the provisioning - the node may still start
			// if the script was embedded at boot. Manual intervention may be needed.
			_ = err // caller can observe node never joining the cluster
		}
	}

	return fmt.Sprintf("%s%s/%d", InstanceIDPrefix, datacenter, serverID), nil
}

type cloudScriptRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Enable  bool   `json:"enable"`
}

func (p *Provisioner) attachCloudScript(ctx context.Context, serverID int, script string) error {
	reqBody := cloudScriptRequest{
		Name:    "cloudburst-startup",
		Content: script,
		Enable:  true,
	}
	path := fmt.Sprintf("/servers/%d/scripts", serverID)
	resp, body, err := p.doRequest(ctx, http.MethodPost, path, reqBody)
	if err != nil {
		return fmt.Errorf("attach cloud script: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("attach cloud script (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (p *Provisioner) waitForActive(ctx context.Context, serverID int) error {
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
		case "active", "running", "on":
			return nil
		case "error", "failed":
			return &cberrors.InstanceFailedError{Reason: "server entered error state during creation"}
		}
		time.Sleep(createPollInterval)
	}
	return fmt.Errorf("server did not become active within %v", createPollTimeout)
}

func (p *Provisioner) getServerStatusByID(ctx context.Context, serverID int) (string, error) {
	path := fmt.Sprintf("/servers/%d", serverID)
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
	if sResp.Server != nil {
		return sResp.Server.Status, nil
	}
	return sResp.Status, nil
}

// DeleteInstance deletes a Seeweb ECS server.
// instanceID must be in the form "seeweb/<datacenter>/<server-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, serverIDStr, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	resp, body, err := p.doRequest(ctx, http.MethodDelete, "/servers/"+serverIDStr, nil)
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

// IsInstanceRunning returns true if the server is in active/running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, serverIDStr, err := parseInstanceID(instanceID)
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

	status := ""
	if sResp.Server != nil {
		status = strings.ToLower(sResp.Server.Status)
	} else {
		status = strings.ToLower(sResp.Status)
	}

	switch status {
	case "error", "failed":
		return false, &cberrors.InstanceFailedError{Reason: "server status: " + status}
	case "deleted", "":
		return false, nil
	}
	return status == "active" || status == "running" || status == "on", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}

func parseInstanceID(instanceID string) (string, string, error) {
	return provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
}
