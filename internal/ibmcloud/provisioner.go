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

// Package ibmcloud provides a provisioner for IBM Cloud VPC.
// API: https://{region}.iaas.cloud.ibm.com/v1/
package ibmcloud

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

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	InstanceIDPrefix         = "ibm_cloud/"
	IAMTokenURL              = "https://iam.cloud.ibm.com/identity/token"
	VPCAPIVersion            = "2025-06-01"
	createPollInterval       = 10 * time.Second
	createPollTimeout        = 10 * time.Minute
	tokenRefreshMargin       = 5 * time.Minute
	DefaultVPCName           = "cloudburst-vpc"
	DefaultSubnetName        = "cloudburst-subnet"
	DefaultSubnetIPv4Count   = 256
)

// Provisioner handles IBM Cloud VPC instance provisioning.
// It auto-creates VPC and subnet if subnetID is not provided (same approach as Azure).
type Provisioner struct {
	apiKey         string
	region         string
	resourceGroup  string
	subnetID       string
	securityGroups []string
	httpClient     *http.Client
	token          string
	tokenExpiry    time.Time
	tokenMu        sync.Mutex
	infraReady     bool   // true after ensureInfrastructure succeeds
	infraVPCID     string // VPC ID when auto-created
	infraSubnetID  string // subnet ID when auto-created
	infraZone      string // zone for auto-created subnet
	infraMu        sync.Mutex
}

// NewProvisioner creates a new IBM Cloud VPC provisioner.
// subnetID is optional; when empty, VPC and subnet are auto-created (same approach as Azure).
func NewProvisioner(ctx context.Context, apiKey, region, resourceGroup, subnetID string, securityGroups []string) (*Provisioner, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("IBM Cloud API key is required")
	}
	if region == "" || resourceGroup == "" {
		return nil, fmt.Errorf("IBM Cloud region and resourceGroup are required")
	}
	return &Provisioner{
		apiKey:         apiKey,
		region:         region,
		resourceGroup:  resourceGroup,
		subnetID:       subnetID,
		securityGroups: securityGroups,
		httpClient:     &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (p *Provisioner) vpcBaseURL() string {
	return fmt.Sprintf("https://%s.iaas.cloud.ibm.com", p.region)
}

func (p *Provisioner) getToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if p.token != "" && time.Now().Add(tokenRefreshMargin).Before(p.tokenExpiry) {
		return p.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ibm:params:oauth:grant-type:apikey")
	form.Set("apikey", p.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, IAMTokenURL, strings.NewReader(form.Encode()))
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
		ExpiresIn   int64  `json:"expires_in"`
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

	urlStr := p.vpcBaseURL() + path + "?version=" + VPCAPIVersion + "&generation=2"
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

// doRequestWithQuery is like doRequest but appends extra query parameters.
func (p *Provisioner) doRequestWithQuery(ctx context.Context, method, path string, body interface{}, query map[string]string) (*http.Response, []byte, error) {
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

	urlStr := p.vpcBaseURL() + path + "?version=" + VPCAPIVersion + "&generation=2"
	for k, v := range query {
		urlStr += "&" + url.QueryEscape(k) + "=" + url.QueryEscape(v)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

// ensureInfrastructure creates VPC and subnet if subnetID is not provided.
// Idempotent: safe to call multiple times; reuses existing VPC/subnet if found by name (same approach as Azure).
func (p *Provisioner) ensureInfrastructure(ctx context.Context) error {
	p.infraMu.Lock()
	defer p.infraMu.Unlock()
	if p.infraReady {
		return nil
	}

	zone := p.region + "-1"

	// 1. Ensure VPC exists (get by name or create)
	var vpcID string
	resp, body, err := p.doRequestWithQuery(ctx, http.MethodGet, "/v1/vpcs", nil, map[string]string{"name": DefaultVPCName})
	if err != nil {
		return fmt.Errorf("list VPCs: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		var listResp struct {
			VPCs []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"vpcs"`
		}
		if err := json.Unmarshal(body, &listResp); err == nil && len(listResp.VPCs) > 0 {
			vpcID = listResp.VPCs[0].ID
		}
	}
	if vpcID == "" {
		vpcReq := map[string]interface{}{
			"name":                     DefaultVPCName,
			"address_prefix_management": "auto",
			"resource_group":           map[string]string{"id": p.resourceGroup},
		}
		resp, body, err = p.doRequest(ctx, http.MethodPost, "/v1/vpcs", vpcReq)
		if err != nil {
			return fmt.Errorf("create VPC: %w", err)
		}
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("create VPC (status %d): %s", resp.StatusCode, string(body))
		}
		var vpcResp struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &vpcResp); err != nil {
			return fmt.Errorf("parse VPC response: %w", err)
		}
		if vpcResp.ID == "" {
			return fmt.Errorf("create VPC returned empty id")
		}
		vpcID = vpcResp.ID
	}

	// 2. Ensure subnet exists (get by name in VPC or create)
	var subnetID string
	resp, body, err = p.doRequestWithQuery(ctx, http.MethodGet, "/v1/subnets", nil, map[string]string{"name": DefaultSubnetName, "vpc.id": vpcID})
	if err != nil {
		return fmt.Errorf("list subnets: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		var listResp struct {
			Subnets []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"subnets"`
		}
		if err := json.Unmarshal(body, &listResp); err == nil && len(listResp.Subnets) > 0 {
			subnetID = listResp.Subnets[0].ID
		}
	}
	if subnetID == "" {
		subnetReq := map[string]interface{}{
			"name":                    DefaultSubnetName,
			"vpc":                     map[string]string{"id": vpcID},
			"zone":                    map[string]string{"name": zone},
			"total_ipv4_address_count": DefaultSubnetIPv4Count,
			"resource_group":          map[string]string{"id": p.resourceGroup},
		}
		resp, body, err = p.doRequest(ctx, http.MethodPost, "/v1/subnets", subnetReq)
		if err != nil {
			return fmt.Errorf("create subnet: %w", err)
		}
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("create subnet (status %d): %s", resp.StatusCode, string(body))
		}
		var subnetResp struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &subnetResp); err != nil {
			return fmt.Errorf("parse subnet response: %w", err)
		}
		if subnetResp.ID == "" {
			return fmt.Errorf("create subnet returned empty id")
		}
		subnetID = subnetResp.ID
	}

	p.infraVPCID = vpcID
	p.infraSubnetID = subnetID
	p.infraZone = zone
	p.infraReady = true
	return nil
}

func (p *Provisioner) getInfraVPCZoneSubnet() (vpcID, zone, subnetID string) {
	return p.infraVPCID, p.infraZone, p.infraSubnetID
}

func (p *Provisioner) getSubnetVPC(ctx context.Context) (string, string, error) {
	path := fmt.Sprintf("/v1/subnets/%s", p.subnetID)
	resp, body, err := p.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", "", fmt.Errorf("get subnet: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("get subnet (status %d): %s", resp.StatusCode, string(body))
	}

	var sn struct {
		ID     string `json:"id"`
		Zone   *struct{ Name string } `json:"zone"`
		VPC    *struct{ ID string } `json:"vpc"`
	}
	if err := json.Unmarshal(body, &sn); err != nil {
		return "", "", fmt.Errorf("parse subnet: %w", err)
	}
	if sn.VPC == nil || sn.VPC.ID == "" {
		return "", "", fmt.Errorf("subnet has no VPC")
	}
	zone := fmt.Sprintf("%s-1", p.region)
	if sn.Zone != nil && sn.Zone.Name != "" {
		zone = sn.Zone.Name
	}
	return sn.VPC.ID, zone, nil
}

func (p *Provisioner) getProfileName(ctx context.Context, instanceType string) (string, error) {
	if instanceType != "" && !strings.Contains(instanceType, " ") {
		return instanceType, nil
	}
	resp, body, err := p.doRequest(ctx, http.MethodGet, "/v1/instance/profiles", nil)
	if err != nil {
		return "", fmt.Errorf("list profiles: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list profiles (status %d): %s", resp.StatusCode, string(body))
	}

	var pr struct {
		Profiles []struct {
			Name   string `json:"name"`
			VcpuCount *struct{ Value int } `json:"vcpu_count"`
			Memory *struct{ Value int } `json:"memory"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", fmt.Errorf("parse profiles: %w", err)
	}
	if instanceType != "" {
		for _, p := range pr.Profiles {
			if p.Name == instanceType {
				return p.Name, nil
			}
		}
	}
	if len(pr.Profiles) > 0 {
		return pr.Profiles[0].Name, nil
	}
	return "", fmt.Errorf("no profiles found")
}

func (p *Provisioner) getImageID(ctx context.Context, imageName string) (string, error) {
	if imageName != "" && len(imageName) == 36 && strings.Contains(imageName, "-") {
		return imageName, nil
	}
	resp, body, err := p.doRequest(ctx, http.MethodGet, "/v1/images", nil)
	if err != nil {
		return "", fmt.Errorf("list images: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list images (status %d): %s", resp.StatusCode, string(body))
	}

	var ir struct {
		Images []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"images"`
	}
	if err := json.Unmarshal(body, &ir); err != nil {
		return "", fmt.Errorf("parse images: %w", err)
	}

	search := "ubuntu-22"
	if imageName != "" {
		search = imageName
	}
	for _, img := range ir.Images {
		if strings.Contains(strings.ToLower(img.Name), strings.ToLower(search)) {
			return img.ID, nil
		}
	}
	return "", fmt.Errorf("no Ubuntu 22.04 image found")
}

type createInstanceRequest struct {
	Name                   string `json:"name"`
	Profile                *struct{ Name string } `json:"profile"`
	Image                  *struct{ ID string } `json:"image"`
	Zone                   *struct{ Name string } `json:"zone"`
	VPC                    *struct{ ID string } `json:"vpc"`
	ResourceGroup          *struct{ ID string } `json:"resource_group,omitempty"`
	PrimaryNetworkInterface *struct {
		Subnet *struct{ ID string } `json:"subnet"`
	} `json:"primary_network_interface"`
	UserData string `json:"user_data,omitempty"`
}

type instanceResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// CreateInstance creates an IBM Cloud VPC instance.
// Returns instanceID in the form "ibm_cloud/<region>/<instance-id>".
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
	var vpcID, zone, subnetID string
	if p.subnetID != "" {
		var err error
		vpcID, zone, err = p.getSubnetVPC(ctx)
		if err != nil {
			return "", err
		}
		subnetID = p.subnetID
	} else {
		if err := p.ensureInfrastructure(ctx); err != nil {
			return "", fmt.Errorf("ensure infrastructure: %w", err)
		}
		vpcID, zone, subnetID = p.getInfraVPCZoneSubnet()
	}
	profileName, err := p.getProfileName(ctx, instanceType)
	if err != nil {
		return "", err
	}
	imageID, err := p.getImageID(ctx, imageFromConfig)
	if err != nil {
		return "", err
	}

	userData := ""
	if startupScript != "" {
		userData = base64.StdEncoding.EncodeToString([]byte(startupScript))
	}

	reqBody := createInstanceRequest{
		Name:    name,
		Profile: &struct{ Name string }{Name: profileName},
		Image:   &struct{ ID string }{ID: imageID},
		Zone:    &struct{ Name string }{Name: zone},
		VPC:     &struct{ ID string }{ID: vpcID},
		ResourceGroup: &struct{ ID string }{ID: p.resourceGroup},
		PrimaryNetworkInterface: &struct {
			Subnet *struct{ ID string } `json:"subnet"`
		}{Subnet: &struct{ ID string }{ID: subnetID}},
		UserData: userData,
	}

	resp, body, err := p.doRequest(ctx, http.MethodPost, "/v1/instances", reqBody)
	if err != nil {
		return "", fmt.Errorf("create instance: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create instance (status %d): %s", resp.StatusCode, string(body))
	}

	var iResp instanceResponse
	if err := json.Unmarshal(body, &iResp); err != nil {
		return "", fmt.Errorf("parse create response: %w", err)
	}
	if iResp.ID == "" {
		return "", fmt.Errorf("IBM Cloud create returned empty instance id")
	}

	if err := p.waitForRunning(ctx, iResp.ID); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%s/%s", InstanceIDPrefix, p.region, iResp.ID), nil
}

func (p *Provisioner) waitForRunning(ctx context.Context, instanceID string) error {
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
		case "failed":
			return &cberrors.InstanceFailedError{Reason: "instance entered failed state during creation"}
		}
		time.Sleep(createPollInterval)
	}
	return fmt.Errorf("instance did not become running within %v", createPollTimeout)
}

func (p *Provisioner) getInstanceStatus(ctx context.Context, instanceID string) (string, error) {
	path := fmt.Sprintf("/v1/instances/%s", instanceID)
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
	return iResp.Status, nil
}

// DeleteInstance deletes an IBM Cloud VPC instance.
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

	path := fmt.Sprintf("/v1/instances/%s", instIDStr)
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
	_, instIDStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return false, err
	}

	status, err := p.getInstanceStatus(ctx, instIDStr)
	if err != nil {
		return false, err
	}

	s := strings.ToLower(status)
	switch s {
	case "failed":
		return false, &cberrors.InstanceFailedError{Reason: "instance status: " + status}
	case "deleted", "stopping", "":
		return false, nil
	}
	return s == "running", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}
