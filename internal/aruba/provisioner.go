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

package aruba

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	InstanceIDPrefix  = "aruba/"
	DefaultImage      = "LU22-001" // Ubuntu 22 64bit
	cloudburstVPCName = "cloudburst-default"
	tokenURL          = "https://mylogin.aruba.it/auth/realms/cmp-new-apikey/protocol/openid-connect/token"
	defaultBaseURL    = "https://api.arubacloud.com"
)

// Provisioner handles Aruba Cloud Server provisioning via direct REST API calls.
type Provisioner struct {
	httpClient *http.Client
	baseURL    string
	appKey     string
	appSecret  string
	projectID  string
	token      string
	tokenExp   time.Time
	tokenMu    sync.Mutex
}

// NewProvisioner creates a new Aruba provisioner using direct HTTP API calls.
func NewProvisioner(ctx context.Context, baseURL, appKey, appSecret, _ string, projectID string) (*Provisioner, error) {
	if projectID == "" {
		return nil, fmt.Errorf("Aruba project ID is required")
	}
	if appKey == "" || appSecret == "" {
		return nil, fmt.Errorf("Aruba API key and secret are required")
	}
	b := strings.TrimSuffix(defaultBaseURL, "/")
	if baseURL != "" && baseURL != defaultBaseURL {
		b = strings.TrimSuffix(baseURL, "/")
	}
	return &Provisioner{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:    b,
		appKey:     appKey,
		appSecret:  appSecret,
		projectID:  projectID,
	}, nil
}

func (p *Provisioner) getToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if p.token != "" && time.Now().Before(p.tokenExp) {
		return p.token, nil
	}
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", p.appKey)
	data.Set("client_secret", p.appSecret)
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token request failed %d: %s", resp.StatusCode, string(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", err
	}
	p.token = tok.AccessToken
	p.tokenExp = time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second)
	return p.token, nil
}

func (p *Provisioner) do(ctx context.Context, method, path string, body interface{}) (*http.Response, []byte, error) {
	token, err := p.getToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	u := p.baseURL + path
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, respBody, nil
}

func (p *Provisioner) apiPath(provider, resource string) string {
	return fmt.Sprintf("/projects/%s/providers/%s/%s", p.projectID, provider, resource)
}

// CreateInstance creates an Aruba Cloud Server.
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

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, flavorName, location, startupScript, imageFromConfig string) (string, error) {
	loc := "ITBG-Bergamo"
	dc := "ITBG-1"
	if location != "" {
		switch strings.ToLower(location) {
		case "itbg-bergamo":
			loc = "ITBG-Bergamo"
			dc = "ITBG-1"
		default:
			loc = location
			dc = "ITBG-1"
		}
	}

	vpcURI, subnetURI, sgURI, err := p.ensureNetwork(ctx, loc)
	if err != nil {
		return "", fmt.Errorf("ensure network: %w", err)
	}

	image := imageFromConfig
	if image == "" {
		image = DefaultImage
	}

	volResp, err := p.getOrCreateBootVolume(ctx, name+"-boot", loc, dc, image)
	if err != nil {
		return "", fmt.Errorf("create boot volume: %w", err)
	}
	if err := p.waitForVolumeReady(ctx, volResp.URI); err != nil {
		return "", fmt.Errorf("wait for boot volume: %w", err)
	}

	req := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":     name,
			"location": map[string]string{"value": loc},
		},
		"properties": map[string]interface{}{
			"dataCenter":     dc,
			"vpc":            map[string]string{"uri": vpcURI},
			"bootVolume":     map[string]string{"uri": volResp.URI},
			"flavorName":     flavorName,
			"subnets":        []map[string]string{{"uri": subnetURI}},
			"securityGroups": []map[string]string{{"uri": sgURI}},
		},
	}
	if startupScript != "" {
		req["properties"].(map[string]interface{})["userData"] = startupScript
	}

	resp, body, err := p.do(ctx, "POST", p.apiPath("Aruba.Compute", "cloudServers")+"?api-version=1.1", req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Aruba API error %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}

	var data struct {
		Metadata *struct {
			ID  *string `json:"id"`
			URI *string `json:"uri"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	instanceID := ""
	if data.Metadata != nil {
		if data.Metadata.ID != nil {
			instanceID = *data.Metadata.ID
		}
		if instanceID == "" && data.Metadata.URI != nil {
			instanceID = extractIDFromURI(*data.Metadata.URI, "cloudServers")
		}
	}
	if instanceID == "" {
		return "", fmt.Errorf("create response did not contain instance ID")
	}
	if strings.Contains(instanceID, "/") {
		parts := strings.Split(strings.TrimSuffix(instanceID, "/"), "/")
		if len(parts) > 0 {
			instanceID = parts[len(parts)-1]
		}
	}
	return InstanceIDPrefix + p.projectID + "/" + instanceID, nil
}

func (p *Provisioner) ensureNetwork(ctx context.Context, location string) (vpcURI, subnetURI, sgURI string, err error) {
	resp, body, err := p.do(ctx, "GET", p.apiPath("Aruba.Network", "vpcs"), nil)
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode != 200 {
		return "", "", "", fmt.Errorf("list VPCs %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	var vpcList struct {
		Values []struct {
			Metadata *struct {
				Name     *string `json:"name"`
				URI      *string `json:"uri"`
				Location *struct {
					Value string `json:"value"`
				} `json:"location"`
			} `json:"metadata"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &vpcList); err != nil {
		return "", "", "", err
	}
	for _, v := range vpcList.Values {
		if v.Metadata == nil {
			continue
		}
		locVal := ""
		if v.Metadata.Location != nil {
			locVal = v.Metadata.Location.Value
		}
		if v.Metadata.Name != nil && *v.Metadata.Name == cloudburstVPCName && locVal == location && v.Metadata.URI != nil {
			subnetURI, sgURI, err = p.getVPCSubnetAndSG(ctx, *v.Metadata.URI, location)
			if err != nil {
				return "", "", "", err
			}
			return *v.Metadata.URI, subnetURI, sgURI, nil
		}
	}

	vpcReq := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":     cloudburstVPCName,
			"location": map[string]string{"value": location},
		},
		"properties": map[string]interface{}{
			"properties": map[string]interface{}{
				"default": true,
				"preset":  true,
			},
		},
	}
	resp, body, err = p.do(ctx, "POST", p.apiPath("Aruba.Network", "vpcs"), vpcReq)
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("create VPC %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	var vpcData struct {
		Metadata *struct {
			URI *string `json:"uri"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &vpcData); err != nil || vpcData.Metadata == nil || vpcData.Metadata.URI == nil {
		return "", "", "", fmt.Errorf("create VPC response did not contain URI")
	}
	vpcURI = *vpcData.Metadata.URI

	subnetURI, sgURI, err = p.getVPCSubnetAndSG(ctx, vpcURI, location)
	if err != nil {
		return "", "", "", err
	}
	return vpcURI, subnetURI, sgURI, nil
}

func (p *Provisioner) getVPCSubnetAndSG(ctx context.Context, vpcURI, location string) (subnetURI, sgURI string, err error) {
	vpcID := extractIDFromURI(vpcURI, "vpcs")
	if vpcID == "" {
		return "", "", fmt.Errorf("could not extract vpcId from URI %s", vpcURI)
	}

	path := fmt.Sprintf("/projects/%s/providers/Aruba.Network/vpcs/%s/subnets", p.projectID, vpcID)
	resp, body, err := p.do(ctx, "GET", path, nil)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode == 200 {
		var list struct {
			Values []struct {
				Metadata *struct {
					URI *string `json:"uri"`
				} `json:"metadata"`
			} `json:"values"`
		}
		if json.Unmarshal(body, &list) == nil && len(list.Values) > 0 && list.Values[0].Metadata != nil && list.Values[0].Metadata.URI != nil {
			subnetURI = *list.Values[0].Metadata.URI
		}
	}
	if subnetURI == "" {
		subnetReq := map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":     "cloudburst-default-subnet",
				"location": map[string]string{"value": location},
			},
			"properties": map[string]interface{}{
				"type":    "Basic",
				"default": true,
			},
		}
		resp, body, err = p.do(ctx, "POST", path, subnetReq)
		if err != nil {
			return "", "", err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", "", fmt.Errorf("create subnet %d: %s", resp.StatusCode, formatAPIErrorBody(body))
		}
		var subData struct {
			Metadata *struct {
				URI *string `json:"uri"`
			} `json:"metadata"`
		}
		if json.Unmarshal(body, &subData) == nil && subData.Metadata != nil && subData.Metadata.URI != nil {
			subnetURI = *subData.Metadata.URI
		}
	}

	sgPath := fmt.Sprintf("/projects/%s/providers/Aruba.Network/vpcs/%s/securityGroups", p.projectID, vpcID)
	resp, body, err = p.do(ctx, "GET", sgPath, nil)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode == 200 {
		var list struct {
			Values []struct {
				Metadata *struct {
					URI *string `json:"uri"`
				} `json:"metadata"`
			} `json:"values"`
		}
		if json.Unmarshal(body, &list) == nil && len(list.Values) > 0 && list.Values[0].Metadata != nil && list.Values[0].Metadata.URI != nil {
			sgURI = *list.Values[0].Metadata.URI
		}
	}
	if sgURI == "" {
		sgReq := map[string]interface{}{
			"metadata":   map[string]interface{}{"name": "cloudburst-default-sg"},
			"properties": map[string]interface{}{"default": true},
		}
		resp, body, err = p.do(ctx, "POST", sgPath, sgReq)
		if err != nil {
			return "", "", err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", "", fmt.Errorf("create security group %d: %s", resp.StatusCode, formatAPIErrorBody(body))
		}
		var sgData struct {
			Metadata *struct {
				URI *string `json:"uri"`
			} `json:"metadata"`
		}
		if json.Unmarshal(body, &sgData) == nil && sgData.Metadata != nil && sgData.Metadata.URI != nil {
			sgURI = *sgData.Metadata.URI
		}
		if err := p.ensureSecurityGroupEgress(ctx, vpcID, sgURI, location); err != nil {
			return "", "", err
		}
	}
	return subnetURI, sgURI, nil
}

func (p *Provisioner) ensureSecurityGroupEgress(ctx context.Context, vpcID, sgURI, location string) error {
	sgID := extractIDFromURI(sgURI, "securityGroups")
	if sgID == "" {
		return fmt.Errorf("could not extract securityGroupId from URI %s", sgURI)
	}
	rulesPath := fmt.Sprintf("/projects/%s/providers/Aruba.Network/vpcs/%s/securityGroups/%s/rules", p.projectID, vpcID, sgID)
	for _, rule := range []struct {
		protocol string
		port     string
	}{
		{"TCP", "*"},
		{"UDP", "*"},
		{"ICMP", ""},
	} {
		req := map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":     fmt.Sprintf("cloudburst-egress-%s", strings.ToLower(rule.protocol)),
				"location": map[string]string{"value": location},
			},
			"properties": map[string]interface{}{
				"direction": "Egress",
				"protocol":  rule.protocol,
				"target":    map[string]interface{}{"kind": "IP", "value": "0.0.0.0/0"},
			},
		}
		if rule.port != "" {
			req["properties"].(map[string]interface{})["port"] = rule.port
		}
		resp, body, err := p.do(ctx, "POST", rulesPath, req)
		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("create security rule %d: %s", resp.StatusCode, formatAPIErrorBody(body))
		}
	}
	return nil
}

type volumeResult struct {
	URI string
}

// findExistingReadyVolume returns the URI of a volume with the given name that is
// in NotUsed or Active state. Used to reuse volumes from previous failed attempts
// instead of creating duplicates on retry.
func (p *Provisioner) findExistingReadyVolume(ctx context.Context, volName string) (string, bool) {
	resp, body, err := p.do(ctx, "GET", p.apiPath("Aruba.Storage", "blockStorages"), nil)
	if err != nil || resp == nil || resp.StatusCode != 200 {
		return "", false
	}
	var list struct {
		Values []struct {
			Metadata *struct {
				Name *string `json:"name"`
				URI  *string `json:"uri"`
			} `json:"metadata"`
			Status *struct {
				State *string `json:"state"`
			} `json:"status"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", false
	}
	for _, v := range list.Values {
		if v.Metadata == nil || v.Metadata.Name == nil || *v.Metadata.Name != volName {
			continue
		}
		state := ""
		if v.Status != nil && v.Status.State != nil {
			state = strings.TrimSpace(*v.Status.State)
		}
		if state == "Active" || state == "NotUsed" {
			if v.Metadata.URI != nil {
				return *v.Metadata.URI, true
			}
		}
	}
	return "", false
}

// getOrCreateBootVolume returns an existing ready volume with the given name, or creates one.
// Prevents duplicate volumes when CreateInstance retries after a failed server create.
func (p *Provisioner) getOrCreateBootVolume(ctx context.Context, volName, location, dc, image string) (*volumeResult, error) {
	if uri, ok := p.findExistingReadyVolume(ctx, volName); ok {
		return &volumeResult{URI: uri}, nil
	}
	return p.createBootVolume(ctx, volName, location, dc, image)
}

func (p *Provisioner) createBootVolume(ctx context.Context, volName, location, dc, image string) (*volumeResult, error) {
	bootable := true
	req := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":     volName,
			"location": map[string]string{"value": location},
		},
		"properties": map[string]interface{}{
			"sizeGb":        20,
			"billingPeriod": "Hour",
			"dataCenter":    dc,
			"type":          "Standard",
			"bootable":      bootable,
			"image":         image,
		},
	}
	resp, body, err := p.do(ctx, "POST", p.apiPath("Aruba.Storage", "blockStorages"), req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Aruba block storage error %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	var data struct {
		Metadata *struct {
			URI *string `json:"uri"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &data); err != nil || data.Metadata == nil || data.Metadata.URI == nil {
		return nil, fmt.Errorf("volume response did not contain URI")
	}
	return &volumeResult{URI: *data.Metadata.URI}, nil
}

func (p *Provisioner) waitForVolumeReady(ctx context.Context, volURI string) error {
	volID := extractIDFromURI(volURI, "blockStorages")
	if volID == "" {
		volID = extractIDFromURI(volURI, "blockstorages")
	}
	if volID == "" {
		return fmt.Errorf("could not extract volume ID from URI %s", volURI)
	}
	path := p.apiPath("Aruba.Storage", "blockStorages") + "/" + volID
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, body, err := p.do(ctx, "GET", path, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("get volume %d: %s", resp.StatusCode, formatAPIErrorBody(body))
		}
		var data struct {
			Status *struct {
				State *string `json:"state"`
			} `json:"status"`
		}
		if json.Unmarshal(body, &data) == nil && data.Status != nil && data.Status.State != nil {
			state := strings.TrimSpace(*data.Status.State)
			switch state {
			case "Active", "NotUsed":
				return nil
			case "Failed":
				return fmt.Errorf("volume %s failed", volID)
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("volume %s did not become ready in time", volID)
}

// ListInstances returns all cloud servers in the project.
func (p *Provisioner) ListInstances(ctx context.Context) ([]arubaInstanceInfo, error) {
	resp, body, err := p.do(ctx, "GET", p.apiPath("Aruba.Compute", "cloudServers"), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("list cloud servers %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	var list struct {
		Values []struct {
			Metadata *struct {
				ID       *string `json:"id"`
				URI      *string `json:"uri"`
				Name     *string `json:"name"`
				Location *struct {
					Value string `json:"value"`
				} `json:"location"`
			} `json:"metadata"`
			Status *struct {
				State *string `json:"state"`
			} `json:"status"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var result []arubaInstanceInfo
	for _, v := range list.Values {
		info := arubaInstanceInfo{}
		if v.Metadata != nil {
			if v.Metadata.ID != nil {
				info.ID = *v.Metadata.ID
			}
			if v.Metadata.URI != nil && info.ID == "" {
				if id := extractIDFromURI(*v.Metadata.URI, "cloudServers"); id != "" {
					info.ID = id
				}
			}
			if v.Metadata.Name != nil {
				info.Name = *v.Metadata.Name
			}
			if v.Metadata.Location != nil {
				info.Region = v.Metadata.Location.Value
			}
		}
		if v.Status != nil && v.Status.State != nil {
			info.Status = strings.TrimSpace(*v.Status.State)
		}
		result = append(result, info)
	}
	return result, nil
}

type arubaInstanceInfo struct {
	ID     string
	Name   string
	Status string
	Region string
}

// ListCloudBurstVolumes returns names of cb-*-boot volumes.
func (p *Provisioner) ListCloudBurstVolumes(ctx context.Context) ([]string, error) {
	resp, body, err := p.do(ctx, "GET", p.apiPath("Aruba.Storage", "blockStorages"), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("list volumes %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	var list struct {
		Values []struct {
			Metadata *struct {
				Name *string `json:"name"`
			} `json:"metadata"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var names []string
	for _, v := range list.Values {
		if v.Metadata != nil && v.Metadata.Name != nil {
			n := *v.Metadata.Name
			if strings.HasPrefix(n, "cb-") && strings.HasSuffix(n, "-boot") {
				names = append(names, n)
			}
		}
	}
	return names, nil
}

// CleanupOrphanedResources deletes orphaned cb-*-boot volumes.
func (p *Provisioner) CleanupOrphanedResources(ctx context.Context) (volumes, elasticIPs, keyPairs int, err error) {
	resp, body, err := p.do(ctx, "GET", p.apiPath("Aruba.Storage", "blockStorages"), nil)
	if err != nil {
		return 0, 0, 0, err
	}
	if resp.StatusCode != 200 || body == nil {
		return 0, 0, 0, nil
	}
	var list struct {
		Values []struct {
			Metadata *struct {
				Name *string `json:"name"`
				ID   *string `json:"id"`
				URI  *string `json:"uri"`
			} `json:"metadata"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return 0, 0, 0, nil
	}
	for _, v := range list.Values {
		if v.Metadata == nil || v.Metadata.Name == nil {
			continue
		}
		n := *v.Metadata.Name
		if !strings.HasPrefix(n, "cb-") || !strings.HasSuffix(n, "-boot") {
			continue
		}
		volID := ""
		if v.Metadata.ID != nil {
			volID = *v.Metadata.ID
		}
		if volID == "" && v.Metadata.URI != nil {
			volID = extractVolumeIDFromURI(*v.Metadata.URI)
		}
		if volID != "" {
			p.deleteBootVolumeWithRetry(ctx, volID)
			volumes++
		}
	}
	return volumes, 0, 0, nil
}

// StopInstance powers off an Aruba Cloud Server. Required before delete when the API
// rejects deletion due to state restrictions (e.g. server must be stopped first).
func (p *Provisioner) StopInstance(ctx context.Context, instanceID string) error {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}
	// Poweroff uses api-version=1.0; 1.1 can return 403 on some accounts
	path := p.apiPath("Aruba.Compute", "cloudServers") + "/" + id + "/poweroff" + "?api-version=1.0"
	resp, body, err := p.do(ctx, "POST", path, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Aruba poweroff error %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	// Wait for server to reach stopped state
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		state, err := p.GetInstanceStatus(ctx, instanceID)
		if err != nil {
			return err
		}
		stateLower := strings.ToLower(strings.TrimSpace(state))
		if stateLower == "stopped" || stateLower == "poweredoff" || stateLower == "" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("Aruba server did not stop in time")
}

// DeleteInstance deletes an Aruba Cloud Server and its boot volume.
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	// DELETE /projects/:projectId/providers/Aruba.Compute/cloudServers/:cloudServerId
	// deleteBootVolumeOnCascade=true removes the boot volume with the VM.
	path := p.apiPath("Aruba.Compute", "cloudServers") + "/" + id + "?deleteBootVolumeOnCascade=true"
	resp, body, err := p.do(ctx, "DELETE", path, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Aruba delete error %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	return nil
}

func (p *Provisioner) deleteBootVolumeWithRetry(ctx context.Context, bootVolID string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	for attempt := 0; attempt < 6; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		path := p.apiPath("Aruba.Storage", "blockStorages") + "/" + bootVolID
		resp, _, err := p.do(ctx, "DELETE", path, nil)
		if err == nil && resp != nil && (resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == 404) {
			p.waitForVolumeDeleted(ctx, bootVolID)
			return
		}
		if attempt < 5 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}
}

func (p *Provisioner) waitForVolumeDeleted(ctx context.Context, volID string) {
	path := p.apiPath("Aruba.Storage", "blockStorages") + "/" + volID
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		resp, _, err := p.do(ctx, "GET", path, nil)
		if err != nil || resp == nil || resp.StatusCode == 404 {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

// GetInstanceStatus returns the server status.
func (p *Provisioner) GetInstanceStatus(ctx context.Context, instanceID string) (string, error) {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return "", err
	}
	path := p.apiPath("Aruba.Compute", "cloudServers") + "/" + id
	resp, body, err := p.do(ctx, "GET", path, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == 404 {
		return "", nil
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Aruba get error %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	var data struct {
		Status *struct {
			State *string `json:"state"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &data) == nil && data.Status != nil && data.Status.State != nil {
		return strings.TrimSpace(*data.Status.State), nil
	}
	return "", nil
}

// IsInstanceRunning returns true if the server is running.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return false, err
	}
	path := p.apiPath("Aruba.Compute", "cloudServers") + "/" + id
	resp, body, err := p.do(ctx, "GET", path, nil)
	if err != nil {
		return false, err
	}
	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("Aruba get error %d: %s", resp.StatusCode, formatAPIErrorBody(body))
	}
	var data struct {
		Status *struct {
			State         *string `json:"state"`
			FailureReason *string `json:"failureReason"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &data); err != nil || data.Status == nil {
		return false, nil
	}
	state := ""
	if data.Status.State != nil {
		state = strings.ToLower(strings.TrimSpace(*data.Status.State))
	}
	if state == "failed" {
		reason := "unknown (check Aruba Cloud console)"
		if data.Status.FailureReason != nil {
			if r := strings.TrimSpace(*data.Status.FailureReason); r != "" {
				reason = r
			}
		}
		return false, &errors.InstanceFailedError{Reason: reason}
	}
	return state == "running" || state == "started" || state == "active", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the Aruba cloud server name for a NodeClaim.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

func parseInstanceID(instanceID string) (projectID, instanceIDPart string, err error) {
	return provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
}

func extractIDFromURI(uri, segment string) string {
	parts := strings.Split(strings.TrimSuffix(uri, "/"), "/")
	for i, part := range parts {
		if part == segment && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func extractVolumeIDFromURI(uri string) string {
	for _, seg := range []string{"blockStorages", "blockstorages", "blockStorage", "volumes"} {
		if id := extractIDFromURI(uri, seg); id != "" {
			return id
		}
	}
	return ""
}

func formatAPIErrorBody(body []byte) string {
	if len(body) == 0 {
		return "unknown error"
	}
	if len(body) < 2048 {
		return string(body)
	}
	return string(body[:2048]) + "..."
}
