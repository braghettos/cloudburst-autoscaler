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

// Package contabo provides a provisioner for Contabo Cloud VPS/VDS.
// API docs: https://api.contabo.com/
// OAuth2 password grant; x-request-id (UUID) required on all requests.
package contabo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	InstanceIDPrefix    = "contabo/"
	AuthURL             = "https://auth.contabo.com/auth/realms/contabo/protocol/openid-connect/token"
	APIBase             = "https://api.contabo.com/v1"
	DefaultImageID      = "afecbb85-e2fc-46f0-9684-b46b1faf00bb" // Ubuntu 22.04
	createPollInterval  = 10 * time.Second
	createPollTimeout   = 10 * time.Minute
	deletePollInterval  = 15 * time.Second
	deletePollTimeout   = 15 * time.Minute
	tokenRefreshMargin  = 5 * time.Minute
)

// CloudBroker region -> Contabo API region
var regionMap = map[string]string{
	"eu-de-1": "EU",
	"eu-de-2": "EU",
	"eu-gb-1": "UK",
}

// CloudBroker instance type -> Contabo productId
var productIDMap = map[string]string{
	"cloud-vps-10": "V91",
	"cloud-vps-20": "V94",
	"cloud-vps-30": "V97",
	"cloud-vps-40": "V100",
	"cloud-vps-50": "V103",
	"cloud-vps-60": "V106",
	"cloud-vds-s":  "V8",
	"cloud-vds-m":  "V9",
	"cloud-vds-l":  "V10",
}

// Provisioner handles Contabo instance provisioning.
type Provisioner struct {
	clientID     string
	clientSecret string
	apiUser      string
	apiPassword  string
	httpClient   *http.Client
	token        string
	tokenExpiry  time.Time
	tokenMu      sync.Mutex
}

// NewProvisioner creates a new Contabo provisioner.
func NewProvisioner(ctx context.Context, clientID, clientSecret, apiUser, apiPassword string) (*Provisioner, error) {
	if clientID == "" || clientSecret == "" || apiUser == "" || apiPassword == "" {
		return nil, fmt.Errorf("Contabo API credentials (clientID, clientSecret, apiUser, apiPassword) are required")
	}
	return &Provisioner{
		clientID:     clientID,
		clientSecret: clientSecret,
		apiUser:      apiUser,
		apiPassword:  apiPassword,
		httpClient:   &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (p *Provisioner) getToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if p.token != "" && time.Now().Add(tokenRefreshMargin).Before(p.tokenExpiry) {
		return p.token, nil
	}
	form := url.Values{}
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)
	form.Set("username", p.apiUser)
	form.Set("password", p.apiPassword)
	form.Set("grant_type", "password")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AuthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth token (status %d): %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse token: %w", err)
	}
	p.token = tr.AccessToken
	p.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return p.token, nil
}

func (p *Provisioner) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, []byte, error) {
	token, err := p.getToken(ctx)
	if err != nil {
		return nil, nil, err
	}

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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-request-id", uuid.New().String())
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
	if r, ok := regionMap[cloudbrokerRegion]; ok {
		return r, nil
	}
	if strings.HasPrefix(cloudbrokerRegion, "eu-de") {
		return "EU", nil
	}
	return "", fmt.Errorf("unsupported region %q for Contabo (supported: eu-de-1, eu-de-2, eu-gb-1)", cloudbrokerRegion)
}

func mapProductID(instanceType string) (string, error) {
	if id, ok := productIDMap[instanceType]; ok {
		return id, nil
	}
	return "", fmt.Errorf("unsupported instance type %q for Contabo", instanceType)
}

type createInstanceRequest struct {
	ImageID     string `json:"imageId"`
	ProductID   string `json:"productId"`
	Region      string `json:"region"`
	Period      int    `json:"period"`
	DisplayName string `json:"displayName"`
	UserData    string `json:"userData,omitempty"`
}

type createInstanceResponse struct {
	Data []struct {
		InstanceID int64  `json:"instanceId"`
		Status     string `json:"status"`
	} `json:"data"`
}

type instanceResponse struct {
	Data struct {
		InstanceID int64  `json:"instanceId"`
		Status     string `json:"status"`
	} `json:"data"`
}

// CreateInstance creates a Contabo Cloud VPS/VDS instance.
// Returns instanceID in the form "contabo/<instance-id>".
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
	region, err := mapRegion(regionOrZone)
	if err != nil {
		return "", err
	}
	productID, err := mapProductID(instanceType)
	if err != nil {
		return "", err
	}

	imageID := imageFromConfig
	if imageID == "" {
		imageID = DefaultImageID
	}

	userData := ""
	if startupScript != "" {
		userData = base64.StdEncoding.EncodeToString([]byte(startupScript))
	}

	reqBody := createInstanceRequest{
		ImageID:     imageID,
		ProductID:   productID,
		Region:      region,
		Period:      1,
		DisplayName: name,
		UserData:    userData,
	}

	resp, body, err := p.doRequest(ctx, http.MethodPost, "/compute/instances", reqBody)
	if err != nil {
		return "", fmt.Errorf("create instance: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create instance (status %d): %s", resp.StatusCode, string(body))
	}

	var cResp createInstanceResponse
	if err := json.Unmarshal(body, &cResp); err != nil {
		return "", fmt.Errorf("parse create response: %w", err)
	}
	if len(cResp.Data) == 0 {
		return "", fmt.Errorf("Contabo create returned empty data")
	}

	instID := cResp.Data[0].InstanceID
	if err := p.waitForRunning(ctx, instID); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%s/%d", InstanceIDPrefix, regionOrZone, instID), nil
}

func (p *Provisioner) waitForRunning(ctx context.Context, instanceID int64) error {
	deadline := time.Now().Add(createPollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled: %w", ctx.Err())
		default:
		}

		status, err := p.getInstanceStatus(ctx, instanceID)
		if err != nil {
			return err
		}
		switch strings.ToLower(status) {
		case "running":
			return nil
		case "error", "failed":
			return &cberrors.InstanceFailedError{Reason: "instance entered error state during creation"}
		}
		time.Sleep(createPollInterval)
	}
	return fmt.Errorf("instance did not become running within %v", createPollTimeout)
}

func (p *Provisioner) getInstanceStatus(ctx context.Context, instanceID int64) (string, error) {
	path := fmt.Sprintf("/compute/instances/%d", instanceID)
	resp, body, err := p.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("get instance: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "deleted", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get instance (status %d): %s", resp.StatusCode, string(body))
	}

	var iResp instanceResponse
	if err := json.Unmarshal(body, &iResp); err != nil {
		return "", fmt.Errorf("parse instance response: %w", err)
	}
	return iResp.Data.Status, nil
}

// DeleteInstance cancels a Contabo instance and waits until the compute is confirmed deleted.
// Contabo cancel is async; this method polls until the instance is gone (404) or status is cancelled/deleted.
// instanceID must be in the form "contabo/<region>/<instance-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, instIDStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/compute/instances/%s/cancel", instIDStr)
	resp, body, err := p.doRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("cancel instance: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel instance (status %d): %s", resp.StatusCode, string(body))
	}

	// Contabo cancel is async; poll until instance is confirmed deleted or cancelled
	return p.waitForDeleted(ctx, instIDStr)
}

func (p *Provisioner) waitForDeleted(ctx context.Context, instIDStr string) error {
	deadline := time.Now().Add(deletePollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled: %w", ctx.Err())
		default:
		}

		path := fmt.Sprintf("/compute/instances/%s", instIDStr)
		resp, body, err := p.doRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return fmt.Errorf("get instance status: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("get instance (status %d): %s", resp.StatusCode, string(body))
		}

		var iResp instanceResponse
		if err := json.Unmarshal(body, &iResp); err != nil {
			return fmt.Errorf("parse instance response: %w", err)
		}
		status := strings.ToLower(iResp.Data.Status)
		if status == "cancelled" || status == "deleted" {
			return nil
		}

		time.Sleep(deletePollInterval)
	}
	return fmt.Errorf("instance did not reach cancelled/deleted within %v", deletePollTimeout)
}

// IsInstanceRunning returns true if the instance is in running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, instIDStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return false, err
	}

	resp, body, err := p.doRequest(ctx, http.MethodGet, "/compute/instances/"+instIDStr, nil)
	if err != nil {
		return false, fmt.Errorf("get instance: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("get instance (status %d): %s", resp.StatusCode, string(body))
	}

	var iResp instanceResponse
	if err := json.Unmarshal(body, &iResp); err != nil {
		return false, fmt.Errorf("parse instance response: %w", err)
	}

	status := strings.ToLower(iResp.Data.Status)
	switch status {
	case "error", "failed":
		return false, &cberrors.InstanceFailedError{Reason: "instance status: " + status}
	case "deleted", "cancelled", "":
		return false, nil
	}
	return status == "running", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}
