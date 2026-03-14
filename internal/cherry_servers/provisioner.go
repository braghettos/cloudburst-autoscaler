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

// Package cherry_servers provides a provisioner for Cherry Servers (Lithuanian cloud provider).
// API docs: https://api.cherryservers.com/doc/
package cherry_servers

import (
	"bytes"
	"context"
	"encoding/base64"
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
	InstanceIDPrefix   = "cherry_servers/"
	APIBase            = "https://api.cherryservers.com/v1"
	DefaultImage       = "ubuntu_22_04"
	createPollInterval = 5 * time.Second
	createPollTimeout  = 10 * time.Minute
)

// CloudBroker region slug -> Cherry API region slug
var regionSlugMap = map[string]string{
	"eu-lt-1": "LT-Siauliai",
	"eu-nl-1": "NL-Amsterdam",
	"eu-de-1": "DE-Frankfurt",
	"eu-se-1": "SE-Stockholm",
}

// CloudBroker instance type -> Cherry plan slug
var planSlugMap = map[string]string{
	"virtual-ssd-1":  "B1-1-1gb-20s-shared",
	"virtual-ssd-2":  "B1-2-2gb-40s-shared",
	"virtual-ssd-4":  "B1-4-4gb-80s-shared",
	"virtual-ssd-8":  "B1-8-8gb-160s-shared",
	"virtual-ssd-16": "B1-16-16gb-320s-shared",
	"virtual-ssd-32": "B1-32-32gb-640s-shared",
}

// Provisioner handles Cherry Servers instance provisioning.
type Provisioner struct {
	apiToken   string
	projectID  int
	httpClient *http.Client
}

// NewProvisioner creates a new Cherry Servers provisioner.
func NewProvisioner(ctx context.Context, apiToken string, projectID int) (*Provisioner, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("Cherry Servers API token is required")
	}
	if projectID <= 0 {
		return nil, fmt.Errorf("Cherry Servers project ID is required")
	}
	return &Provisioner{
		apiToken:   apiToken,
		projectID:  projectID,
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
	req.Header.Set("Authorization", "Bearer "+p.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

func mapRegion(cloudbrokerRegion string) (string, error) {
	if slug, ok := regionSlugMap[cloudbrokerRegion]; ok {
		return slug, nil
	}
	return "", fmt.Errorf("unsupported region %q for Cherry Servers (supported: eu-lt-1, eu-nl-1, eu-de-1, eu-se-1)", cloudbrokerRegion)
}

func mapPlan(instanceType string) (string, error) {
	if slug, ok := planSlugMap[instanceType]; ok {
		return slug, nil
	}
	return "", fmt.Errorf("unsupported instance type %q for Cherry Servers (supported: virtual-ssd-1 through virtual-ssd-32)", instanceType)
}

type createServerRequest struct {
	Plan     string `json:"plan"`
	Region   string `json:"region"`
	Hostname string `json:"hostname"`
	Image   string `json:"image,omitempty"`
	UserData string `json:"user_data,omitempty"`
	Cycle   string `json:"cycle,omitempty"`
}

type serverResponse struct {
	ID     int    `json:"id"`
	State  string `json:"state"`
	Status string `json:"status"`
}

// CreateInstance creates a Cherry Servers virtual server.
// Returns instanceID in the form "cherry_servers/<region>/<server-id>".
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
	regionSlug, err := mapRegion(regionOrZone)
	if err != nil {
		return "", err
	}
	planSlug, err := mapPlan(instanceType)
	if err != nil {
		return "", err
	}

	image := imageFromConfig
	if image == "" {
		image = DefaultImage
	}

	userData := ""
	if startupScript != "" {
		userData = base64.StdEncoding.EncodeToString([]byte(startupScript))
	}

	reqBody := createServerRequest{
		Plan:     planSlug,
		Region:   regionSlug,
		Hostname: name,
		Image:   image,
		UserData: userData,
		Cycle:   "hourly",
	}

	path := fmt.Sprintf("/projects/%d/servers", p.projectID)
	resp, body, err := p.doRequest(ctx, http.MethodPost, path, reqBody)
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
	if sResp.ID == 0 {
		return "", fmt.Errorf("Cherry Servers create returned empty server id")
	}

	if err := p.waitForActive(ctx, sResp.ID); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%s/%d", InstanceIDPrefix, regionOrZone, sResp.ID), nil
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
		case "active", "running":
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
	if sResp.Status != "" {
		return sResp.Status, nil
	}
	return sResp.State, nil
}

// DeleteInstance deletes a Cherry Servers server.
// instanceID must be in the form "cherry_servers/<region>/<server-id>".
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

// IsInstanceRunning returns true if the server is in active/running state.
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
