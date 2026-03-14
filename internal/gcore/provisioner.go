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

// Package gcore provides a provisioner for Gcore Cloud.
// API docs: https://gcore.com/docs/api-reference/cloud/instances/create-instance
// Async create/delete; poll tasks until FINISHED.
package gcore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	InstanceIDPrefix   = "gcore/"
	APIBase            = "https://api.gcore.com"
	createPollInterval = 5 * time.Second
	createPollTimeout  = 10 * time.Minute
	taskPollInterval   = 3 * time.Second
	taskPollTimeout    = 15 * time.Minute
)

// CloudBroker region slug -> display_name substring for region lookup
var regionDisplayNames = map[string][]string{
	"lux-1":  {"Luxembourg 1", "Luxembourg"},
	"lux-2":  {"Luxembourg 2"},
	"ams-1":  {"Amsterdam"},
	"fra-1":  {"Frankfurt"},
	"darm-1": {"Darmstadt"},
	"par-2":  {"Paris"},
	"waw-1":  {"Warsaw"},
	"hel-1":  {"Helsinki"},
	"lon-1":  {"London"},
}

// Provisioner handles Gcore Cloud instance provisioning.
type Provisioner struct {
	apiKey    string
	projectID int
	httpClient *http.Client
	regionCache map[string]int
	regionMu   sync.RWMutex
}

// NewProvisioner creates a new Gcore provisioner.
func NewProvisioner(ctx context.Context, apiKey string, projectID int) (*Provisioner, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("Gcore API key is required")
	}
	if projectID <= 0 {
		return nil, fmt.Errorf("Gcore project ID is required")
	}
	return &Provisioner{
		apiKey:     apiKey,
		projectID:  projectID,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		regionCache: make(map[string]int),
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
	req.Header.Set("Authorization", "apikey "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

type regionItem struct {
	ID          int    `json:"id"`
	DisplayName string `json:"display_name"`
	KeystoneName string `json:"keystone_name"`
}

type regionsResponse struct {
	Results []regionItem `json:"results"`
}

func (p *Provisioner) getRegionID(ctx context.Context, slug string) (int, error) {
	p.regionMu.RLock()
	if id, ok := p.regionCache[slug]; ok {
		p.regionMu.RUnlock()
		return id, nil
	}
	p.regionMu.RUnlock()

	resp, body, err := p.doRequest(ctx, http.MethodGet, "/cloud/v1/regions", nil)
	if err != nil {
		return 0, fmt.Errorf("fetch regions: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch regions (status %d): %s", resp.StatusCode, string(body))
	}

	var rResp regionsResponse
	if err := json.Unmarshal(body, &rResp); err != nil {
		return 0, fmt.Errorf("parse regions: %w", err)
	}

	matchers, ok := regionDisplayNames[slug]
	if !ok {
		matchers = []string{slug}
	}

	for _, r := range rResp.Results {
		dn := strings.ToLower(r.DisplayName)
		kn := strings.ToLower(r.KeystoneName)
		for _, m := range matchers {
			ml := strings.ToLower(m)
			if strings.Contains(dn, ml) || strings.Contains(kn, ml) || strings.Contains(dn, slug) {
				p.regionMu.Lock()
				p.regionCache[slug] = r.ID
				p.regionMu.Unlock()
				return r.ID, nil
			}
		}
	}
	return 0, fmt.Errorf("region %q not found in Gcore (supported: lux-1, fra-1, ams-1, etc.)", slug)
}

func (p *Provisioner) getImageID(ctx context.Context, regionID int, imageName string) (string, error) {
	if imageName != "" && len(imageName) == 36 && strings.Contains(imageName, "-") {
		return imageName, nil
	}
	path := fmt.Sprintf("/cloud/v1/images/%d/%d", p.projectID, regionID)
	resp, body, err := p.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("fetch images: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch images (status %d): %s", resp.StatusCode, string(body))
	}

	var ir struct {
		Results []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &ir); err != nil {
		return "", fmt.Errorf("parse images: %w", err)
	}

	search := "ubuntu-22.04"
	if imageName != "" {
		search = imageName
	}
	for _, img := range ir.Results {
		if strings.Contains(strings.ToLower(img.Name), strings.ToLower(search)) {
			return img.ID, nil
		}
	}
	return "", fmt.Errorf("no Ubuntu 22.04 image found in region %d", regionID)
}

type createInstanceRequest struct {
	Flavor     string        `json:"flavor"`
	Name       string        `json:"name"`
	Interfaces []interface{} `json:"interfaces"`
	Volumes    []interface{} `json:"volumes"`
	UserData   string        `json:"user_data,omitempty"`
}

type tasksResponse struct {
	Tasks []string `json:"tasks"`
}

type taskResponse struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Result struct {
		InstanceID string `json:"instance_id"`
	} `json:"result"`
}

// CreateInstance creates a Gcore Cloud instance.
// Returns instanceID in the form "gcore/<region_id>/<instance-id>".
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
	regionID, err := p.getRegionID(ctx, regionOrZone)
	if err != nil {
		return "", err
	}
	imageID, err := p.getImageID(ctx, regionID, imageFromConfig)
	if err != nil {
		return "", err
	}

	flavor := instanceType
	if flavor == "" {
		flavor = "g1-standard-2-4"
	}

	userData := ""
	if startupScript != "" {
		userData = base64.StdEncoding.EncodeToString([]byte(startupScript))
	}

	reqBody := createInstanceRequest{
		Flavor:   flavor,
		Name:     name,
		UserData: userData,
		Interfaces: []interface{}{
			map[string]string{"type": "external"},
		},
		Volumes: []interface{}{
			map[string]interface{}{
				"source":     "image",
				"image_id":   imageID,
				"size":       20,
				"boot_index": 0,
			},
		},
	}

	path := fmt.Sprintf("/cloud/v2/instances/%d/%d", p.projectID, regionID)
	resp, body, err := p.doRequest(ctx, http.MethodPost, path, reqBody)
	if err != nil {
		return "", fmt.Errorf("create instance: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("create instance (status %d): %s", resp.StatusCode, string(body))
	}

	var tResp tasksResponse
	if err := json.Unmarshal(body, &tResp); err != nil {
		return "", fmt.Errorf("parse create response: %w", err)
	}
	if len(tResp.Tasks) == 0 {
		return "", fmt.Errorf("Gcore create returned no task IDs")
	}

	instID, err := p.waitForTask(ctx, tResp.Tasks[0])
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%d/%s", InstanceIDPrefix, regionID, instID), nil
}

func (p *Provisioner) waitForTask(ctx context.Context, taskID string) (string, error) {
	deadline := time.Now().Add(taskPollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("context cancelled: %w", ctx.Err())
		default:
		}

		path := fmt.Sprintf("/cloud/v1/tasks/%s", taskID)
		resp, body, err := p.doRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return "", fmt.Errorf("get task: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("get task (status %d): %s", resp.StatusCode, string(body))
		}

		var tr taskResponse
		if err := json.Unmarshal(body, &tr); err != nil {
			return "", fmt.Errorf("parse task: %w", err)
		}

		switch strings.ToUpper(tr.State) {
		case "FINISHED":
			if tr.Result.InstanceID != "" {
				return tr.Result.InstanceID, nil
			}
			return "", fmt.Errorf("task finished but no instance_id in result")
		case "ERROR":
			return "", &cberrors.InstanceFailedError{Reason: "Gcore create task failed"}
		}
		time.Sleep(taskPollInterval)
	}
	return "", fmt.Errorf("task did not complete within %v", taskPollTimeout)
}

type instanceResponse struct {
	Data struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		State  string `json:"state"`
	} `json:"data"`
}

func (p *Provisioner) getInstanceStatus(ctx context.Context, regionID int, instanceID string) (string, error) {
	path := fmt.Sprintf("/cloud/v1/instances/%d/%d/%s", p.projectID, regionID, instanceID)
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
		return "", fmt.Errorf("parse instance: %w", err)
	}
	if iResp.Data.Status != "" {
		return iResp.Data.Status, nil
	}
	return iResp.Data.State, nil
}

// DeleteInstance deletes a Gcore instance (async).
// instanceID must be in the form "gcore/<region_id>/<instance-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	regionStr, instIDStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return err
	}

	var regionID int
	if _, err := fmt.Sscanf(regionStr, "%d", &regionID); err != nil {
		return fmt.Errorf("invalid region in instance ID: %w", err)
	}

	path := fmt.Sprintf("/cloud/v1/instances/%d/%d/%s", p.projectID, regionID, instIDStr)
	resp, body, err := p.doRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("delete instance: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete instance (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// IsInstanceRunning returns true if the instance is in running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	regionStr, instIDStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return false, err
	}

	var regionID int
	if _, err := fmt.Sscanf(regionStr, "%d", &regionID); err != nil {
		return false, fmt.Errorf("invalid region in instance ID: %w", err)
	}

	status, err := p.getInstanceStatus(ctx, regionID, instIDStr)
	if err != nil {
		return false, err
	}

	s := strings.ToLower(status)
	switch s {
	case "error", "failed":
		return false, &cberrors.InstanceFailedError{Reason: "instance status: " + status}
	case "deleted", "":
		return false, nil
	}
	return s == "active" || s == "running", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}
