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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Arubacloud/sdk-go/pkg/aruba"
	"github.com/Arubacloud/sdk-go/pkg/types"
	"github.com/diegobraga/cloudburst-autoscaler/internal/errors"
)

const (
	InstanceIDPrefix       = "aruba/"
	DefaultImage           = "LU22-001" // Ubuntu 22 64bit
	MaxRetries             = 3
	RetryDelay             = 2 * time.Second
	cloudburstVPCName       = "cloudburst-default"
	cloudburstKeyPairName   = "cloudburst-default"
	cloudburstElasticIPName = "cloudburst-default"
	cloudburstSSHPublicKey = "ssh-rsa AAAAB3NzaC1yc2EAAAABJQAAAQEA2No7At0tgHrcZTL0kGWyLLUqPKfOhD9hGdNV9PbJxhjOGNFxcwdQ9wCXsJ3RQaRHBuGIgVodDurrlqzxFK86yCHMgXT2YLHF0j9P4m9GDiCfOK6msbFb89p5xZExjwD2zK+w68r7iOKZeRB2yrznW5TD3KDemSPIQQIVcyLF+yxft49HWBTI3PVQ4rBVOBJ2PdC9SAOf7CYnptW24CRrC0h85szIDwMA+Kmasfl3YGzk4MxheHrTO8C40aXXpieJ9S2VQA4VJAMRyAboptIK0cKjBYrbt5YkEL0AlyBGPIu6MPYr5K/MHyDunDi9yc7VYRYRR0f46MBOSqMUiGPnMw== cloudburst@aruba"
)

// Provisioner handles Aruba Cloud Server provisioning using the official SDK.
type Provisioner struct {
	client    aruba.Client
	projectID string
}

// NewProvisioner creates a new Aruba provisioner using the official Aruba Cloud SDK.
func NewProvisioner(ctx context.Context, baseURL, appKey, appSecret, _ string, projectID string) (*Provisioner, error) {
	if projectID == "" {
		return nil, fmt.Errorf("Aruba project ID is required")
	}
	if appKey == "" || appSecret == "" {
		return nil, fmt.Errorf("Aruba API key and secret are required")
	}
	opts := aruba.DefaultOptions(appKey, appSecret)
	// SDK v0.1.21 has a typo in defaultTokenIssuerURL (https:/// missing host); override with correct URL
	opts = opts.WithTokenIssuerURL("https://mylogin.aruba.it/auth/realms/cmp-new-apikey/protocol/openid-connect/token")
	if baseURL != "" && baseURL != "https://api.arubacloud.com" {
		opts = opts.WithBaseURL(strings.TrimSuffix(baseURL, "/"))
	}
	opts = opts.WithCustomHTTPClient(&http.Client{Timeout: 60 * time.Second})
	client, err := aruba.NewClient(opts)
	if err != nil {
		return nil, fmt.Errorf("create Aruba SDK client: %w", err)
	}
	return &Provisioner{
		client:    client,
		projectID: projectID,
	}, nil
}

// CreateInstance creates an Aruba Cloud Server.
// Requires pre-created VPC, subnet, security group - or use vpcPreset.
// Returns instanceID in the form "aruba/<projectId>/<instanceId>".
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

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, flavorName, location, startupScript, imageFromConfig string) (string, error) {
	// Map CloudBroker region (itbg-bergamo) to Aruba location and datacenter
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

	// Ensure VPC, subnet, security group exist (create if not)
	vpcURI, subnetURI, sgURI, err := p.ensureNetwork(ctx, loc)
	if err != nil {
		return "", fmt.Errorf("ensure network: %w", err)
	}

	// Ensure KeyPair exists (Aruba API rejects keyPair with empty URI; SDK sends {"uri":""} when unset)
	keyPairURI, err := p.ensureKeyPair(ctx, loc)
	if err != nil {
		return "", fmt.Errorf("ensure key pair: %w", err)
	}

	// Ensure ElasticIP exists (Aruba API rejects elasticIp with empty URI; SDK sends {"uri":""} when unset)
	elasticIPURI, err := p.ensureElasticIP(ctx, loc)
	if err != nil {
		return "", fmt.Errorf("ensure elastic IP: %w", err)
	}

	// Create boot volume
	image := imageFromConfig
	if image == "" {
		image = DefaultImage
	}

	volResp, err := p.createBootVolume(ctx, name+"-boot", loc, dc, image)
	if err != nil {
		return "", fmt.Errorf("create boot volume: %w", err)
	}
	// Aruba requires the boot volume to be ready (NotUsed or Active) before creating the server.
	if err := p.waitForVolumeReady(ctx, volResp.URI); err != nil {
		return "", fmt.Errorf("wait for boot volume: %w", err)
	}

	// Build server create request
	req := types.CloudServerRequest{
		Metadata: types.RegionalResourceMetadataRequest{
			ResourceMetadataRequest: types.ResourceMetadataRequest{Name: name},
			Location:                types.LocationRequest{Value: loc},
		},
		Properties: types.CloudServerPropertiesRequest{
			Zone:        dc,
			VPC:         types.ReferenceResource{URI: vpcURI},
			BootVolume:  types.ReferenceResource{URI: volResp.URI},
			KeyPair:     types.ReferenceResource{URI: keyPairURI},
			ElastcIP:    types.ReferenceResource{URI: elasticIPURI}, // SDK typo: ElastcIP
			FlavorName:  strPtr(flavorName),
			Subnets:     []types.ReferenceResource{{URI: subnetURI}},
			SecurityGroups: []types.ReferenceResource{{URI: sgURI}},
		},
	}
	if startupScript != "" {
		req.Properties.UserData = strPtr(startupScript)
	}

	resp, err := p.client.FromCompute().CloudServers().Create(ctx, p.projectID, req, nil)
	if err != nil {
		return "", err
	}
	if !resp.IsSuccess() {
		return "", fmt.Errorf("Aruba API error %d: %s", resp.StatusCode, formatAPIError(resp.StatusCode, resp.Error, resp.RawBody))
	}

	instanceID := ""
	if resp.Data != nil && resp.Data.Metadata.ID != nil {
		instanceID = *resp.Data.Metadata.ID
	}
	if instanceID == "" && resp.Data != nil && resp.Data.Metadata.URI != nil {
		instanceID = extractIDFromURI(*resp.Data.Metadata.URI, "cloudServers")
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
	// List existing VPCs
	vpcList, err := p.client.FromNetwork().VPCs().List(ctx, p.projectID, nil)
	if err != nil {
		return "", "", "", err
	}
	if !vpcList.IsSuccess() {
		return "", "", "", fmt.Errorf("list VPCs %d: %s", vpcList.StatusCode, formatAPIError(vpcList.StatusCode, vpcList.Error, vpcList.RawBody))
	}

	for _, v := range vpcList.Data.Values {
		locVal := ""
		if v.Metadata.LocationResponse != nil {
			locVal = v.Metadata.LocationResponse.Value
		}
		if v.Metadata.Name != nil && *v.Metadata.Name == cloudburstVPCName && locVal == location {
			if v.Metadata.URI == nil {
				continue
			}
			subnetURI, sgURI, err = p.getVPCSubnetAndSG(ctx, *v.Metadata.URI, location)
			if err != nil {
				return "", "", "", err
			}
			return *v.Metadata.URI, subnetURI, sgURI, nil
		}
	}

	// Create VPC with preset=true (auto-creates subnet and security group)
	vpcReq := types.VPCRequest{
		Metadata: types.RegionalResourceMetadataRequest{
			ResourceMetadataRequest: types.ResourceMetadataRequest{Name: cloudburstVPCName},
			Location:                types.LocationRequest{Value: location},
		},
		Properties: types.VPCPropertiesRequest{
			Properties: &types.VPCProperties{
				Default: boolPtr(true),
				Preset:  boolPtr(true),
			},
		},
	}
	vpcResp, err := p.client.FromNetwork().VPCs().Create(ctx, p.projectID, vpcReq, nil)
	if err != nil {
		return "", "", "", err
	}
	if !vpcResp.IsSuccess() {
		return "", "", "", fmt.Errorf("create VPC %d: %s", vpcResp.StatusCode, formatAPIError(vpcResp.StatusCode, vpcResp.Error, vpcResp.RawBody))
	}
	if vpcResp.Data == nil || vpcResp.Data.Metadata.URI == nil {
		return "", "", "", fmt.Errorf("create VPC response did not contain URI")
	}
	vpcURI = *vpcResp.Data.Metadata.URI

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

	// List subnets; create if empty
	subnetList, err := p.client.FromNetwork().Subnets().List(ctx, p.projectID, vpcID, nil)
	if err != nil {
		return "", "", fmt.Errorf("list subnets: %w", err)
	}
	if subnetList.IsSuccess() && len(subnetList.Data.Values) > 0 && subnetList.Data.Values[0].Metadata.URI != nil {
		subnetURI = *subnetList.Data.Values[0].Metadata.URI
	}
	if subnetURI == "" {
		subnetReq := types.SubnetRequest{
			Metadata: types.RegionalResourceMetadataRequest{
				ResourceMetadataRequest: types.ResourceMetadataRequest{Name: "cloudburst-default-subnet"},
				Location:                types.LocationRequest{Value: location},
			},
			Properties: types.SubnetPropertiesRequest{
				Type:    types.SubnetTypeBasic,
				Default: true,
			},
		}
		subnetResp, err := p.client.FromNetwork().Subnets().Create(ctx, p.projectID, vpcID, subnetReq, nil)
		if err != nil {
			return "", "", fmt.Errorf("create subnet: %w", err)
		}
		if !subnetResp.IsSuccess() {
			return "", "", fmt.Errorf("create subnet %d: %s", subnetResp.StatusCode, formatAPIError(subnetResp.StatusCode, subnetResp.Error, subnetResp.RawBody))
		}
		if subnetResp.Data != nil && subnetResp.Data.Metadata.URI != nil {
			subnetURI = *subnetResp.Data.Metadata.URI
		}
	}

	// List security groups; create if empty
	sgList, err := p.client.FromNetwork().SecurityGroups().List(ctx, p.projectID, vpcID, nil)
	if err != nil {
		return "", "", fmt.Errorf("list security groups: %w", err)
	}
	if sgList.IsSuccess() && len(sgList.Data.Values) > 0 && sgList.Data.Values[0].Metadata.URI != nil {
		sgURI = *sgList.Data.Values[0].Metadata.URI
	}
	if sgURI == "" {
		sgReq := types.SecurityGroupRequest{
			Metadata: types.ResourceMetadataRequest{Name: "cloudburst-default-sg"},
			Properties: types.SecurityGroupPropertiesRequest{
				Default: boolPtr(true),
			},
		}
		sgResp, err := p.client.FromNetwork().SecurityGroups().Create(ctx, p.projectID, vpcID, sgReq, nil)
		if err != nil {
			return "", "", fmt.Errorf("create security group: %w", err)
		}
		if !sgResp.IsSuccess() {
			return "", "", fmt.Errorf("create security group %d: %s", sgResp.StatusCode, formatAPIError(sgResp.StatusCode, sgResp.Error, sgResp.RawBody))
		}
		if sgResp.Data != nil && sgResp.Data.Metadata.URI != nil {
			sgURI = *sgResp.Data.Metadata.URI
		}
		// Ensure egress rules for Tailscale, API server, etc.
		if err := p.ensureSecurityGroupEgress(ctx, vpcID, sgURI, location); err != nil {
			return "", "", fmt.Errorf("ensure security group egress: %w", err)
		}
	}

	return subnetURI, sgURI, nil
}

func (p *Provisioner) ensureKeyPair(ctx context.Context, location string) (string, error) {
	listResp, err := p.client.FromCompute().KeyPairs().List(ctx, p.projectID, nil)
	if err != nil {
		return "", err
	}
	if !listResp.IsSuccess() {
		return "", fmt.Errorf("list key pairs %d: %s", listResp.StatusCode, formatAPIError(listResp.StatusCode, listResp.Error, listResp.RawBody))
	}
	for _, kp := range listResp.Data.Values {
		locVal := ""
		if kp.Metadata.LocationResponse != nil {
			locVal = kp.Metadata.LocationResponse.Value
		}
		if kp.Metadata.Name != nil && *kp.Metadata.Name == cloudburstKeyPairName && locVal == location && kp.Metadata.URI != nil {
			return *kp.Metadata.URI, nil
		}
	}
	req := types.KeyPairRequest{
		Metadata: types.RegionalResourceMetadataRequest{
			ResourceMetadataRequest: types.ResourceMetadataRequest{Name: cloudburstKeyPairName},
			Location:                types.LocationRequest{Value: location},
		},
		Properties: types.KeyPairPropertiesRequest{Value: cloudburstSSHPublicKey},
	}
	resp, err := p.client.FromCompute().KeyPairs().Create(ctx, p.projectID, req, nil)
	if err != nil {
		return "", err
	}
	if !resp.IsSuccess() {
		return "", fmt.Errorf("create key pair %d: %s", resp.StatusCode, formatAPIError(resp.StatusCode, resp.Error, resp.RawBody))
	}
	if resp.Data == nil || resp.Data.Metadata.URI == nil {
		return "", fmt.Errorf("key pair response did not contain URI")
	}
	return *resp.Data.Metadata.URI, nil
}

func (p *Provisioner) ensureElasticIP(ctx context.Context, location string) (string, error) {
	listResp, err := p.client.FromNetwork().ElasticIPs().List(ctx, p.projectID, nil)
	if err != nil {
		return "", err
	}
	if !listResp.IsSuccess() {
		return "", fmt.Errorf("list elastic IPs %d: %s", listResp.StatusCode, formatAPIError(listResp.StatusCode, listResp.Error, listResp.RawBody))
	}
	for _, eip := range listResp.Data.Values {
		locVal := ""
		if eip.Metadata.LocationResponse != nil {
			locVal = eip.Metadata.LocationResponse.Value
		}
		if eip.Metadata.Name != nil && *eip.Metadata.Name == cloudburstElasticIPName && locVal == location && eip.Metadata.URI != nil {
			return *eip.Metadata.URI, nil
		}
	}
	req := types.ElasticIPRequest{
		Metadata: types.RegionalResourceMetadataRequest{
			ResourceMetadataRequest: types.ResourceMetadataRequest{Name: cloudburstElasticIPName},
			Location:                types.LocationRequest{Value: location},
		},
		Properties: types.ElasticIPPropertiesRequest{
			BillingPlan: types.BillingPeriodResource{BillingPeriod: "Hour"},
		},
	}
	resp, err := p.client.FromNetwork().ElasticIPs().Create(ctx, p.projectID, req, nil)
	if err != nil {
		return "", err
	}
	if !resp.IsSuccess() {
		return "", fmt.Errorf("create elastic IP %d: %s", resp.StatusCode, formatAPIError(resp.StatusCode, resp.Error, resp.RawBody))
	}
	if resp.Data == nil || resp.Data.Metadata.URI == nil {
		return "", fmt.Errorf("elastic IP response did not contain URI")
	}
	return *resp.Data.Metadata.URI, nil
}

func (p *Provisioner) ensureSecurityGroupEgress(ctx context.Context, vpcID, sgURI, location string) error {
	sgID := extractIDFromURI(sgURI, "securityGroups")
	if sgID == "" {
		return fmt.Errorf("could not extract securityGroupId from URI %s", sgURI)
	}
	target := &types.RuleTarget{Kind: types.EndpointTypeIP, Value: "0.0.0.0/0"}
	for _, rule := range []struct {
		protocol string
		port     string
	}{
		{"TCP", "*"},
		{"UDP", "*"},
		{"ICMP", ""},
	} {
		props := types.SecurityRulePropertiesRequest{
			Direction: types.RuleDirectionEgress,
			Protocol:  rule.protocol,
			Target:    target,
		}
		if rule.port != "" {
			props.Port = rule.port
		}
		req := types.SecurityRuleRequest{
			Metadata: types.RegionalResourceMetadataRequest{
				ResourceMetadataRequest: types.ResourceMetadataRequest{
					Name: fmt.Sprintf("cloudburst-egress-%s", strings.ToLower(rule.protocol)),
				},
				Location: types.LocationRequest{Value: location},
			},
			Properties: props,
		}
		resp, err := p.client.FromNetwork().SecurityGroupRules().Create(ctx, p.projectID, vpcID, sgID, req, nil)
		if err != nil {
			return err
		}
		if !resp.IsSuccess() {
			return fmt.Errorf("create security rule %d: %s", resp.StatusCode, formatAPIError(resp.StatusCode, resp.Error, resp.RawBody))
		}
	}
	return nil
}

type volumeResult struct {
	URI string
}

func (p *Provisioner) createBootVolume(ctx context.Context, volName, location, dc, image string) (*volumeResult, error) {
	bootable := true
	req := types.BlockStorageRequest{
		Metadata: types.RegionalResourceMetadataRequest{
			ResourceMetadataRequest: types.ResourceMetadataRequest{Name: volName},
			Location:                types.LocationRequest{Value: location},
		},
		Properties: types.BlockStoragePropertiesRequest{
			SizeGB:        20,
			BillingPeriod:  "Hour",
			Zone:          &dc,
			Type:          types.BlockStorageTypeStandard,
			Bootable:      &bootable,
			Image:         &image,
		},
	}
	resp, err := p.client.FromStorage().Volumes().Create(ctx, p.projectID, req, nil)
	if err != nil {
		return nil, err
	}
	if !resp.IsSuccess() {
		return nil, fmt.Errorf("Aruba block storage error %d: %s", resp.StatusCode, formatAPIError(resp.StatusCode, resp.Error, resp.RawBody))
	}
	if resp.Data == nil || resp.Data.Metadata.URI == nil {
		return nil, fmt.Errorf("volume response did not contain URI")
	}
	return &volumeResult{URI: *resp.Data.Metadata.URI}, nil
}

func (p *Provisioner) waitForVolumeReady(ctx context.Context, volURI string) error {
	volID := extractIDFromURI(volURI, "blockStorages")
	if volID == "" {
		return fmt.Errorf("could not extract volume ID from URI %s", volURI)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("volume %s did not become ready in time", volID)
			}
			resp, err := p.client.FromStorage().Volumes().Get(ctx, p.projectID, volID, nil)
			if err != nil {
				return err
			}
			if !resp.IsSuccess() {
				return fmt.Errorf("get volume %d: %s", resp.StatusCode, formatAPIError(resp.StatusCode, resp.Error, resp.RawBody))
			}
			state := ""
			if resp.Data != nil && resp.Data.Status.State != nil {
				state = strings.TrimSpace(*resp.Data.Status.State)
			}
			switch state {
			case "Active", "NotUsed":
				return nil
			case "Failed":
				return fmt.Errorf("volume %s failed", volID)
			}
		}
	}
}

// DeleteInstance deletes an Aruba Cloud Server and its boot volume.
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	// Get server to find boot volume for cleanup (SDK Delete may not cascade)
	var bootVolID string
	getResp, err := p.client.FromCompute().CloudServers().Get(ctx, p.projectID, id, nil)
	if err == nil && getResp.IsSuccess() && getResp.Data != nil &&
		getResp.Data.Properties.BootVolume.URI != "" {
		bootVolID = extractIDFromURI(getResp.Data.Properties.BootVolume.URI, "blockStorages")
	}

	delResp, err := p.client.FromCompute().CloudServers().Delete(ctx, p.projectID, id, nil)
	if err != nil {
		return err
	}
	if delResp.StatusCode == 404 {
		return nil
	}
	if !delResp.IsSuccess() {
		return fmt.Errorf("Aruba delete error %d: %s", delResp.StatusCode, formatAPIError(delResp.StatusCode, delResp.Error, delResp.RawBody))
	}

	// SDK Delete may not support deleteBootVolumeOnCascade; delete volume explicitly if we found it
	if bootVolID != "" {
		volDel, _ := p.client.FromStorage().Volumes().Delete(ctx, p.projectID, bootVolID, nil)
		if volDel != nil && !volDel.IsSuccess() && volDel.StatusCode != 404 {
			// Log but don't fail - server is already deleted
			_ = volDel
		}
	}
	return nil
}

// IsInstanceRunning returns true if the server is running.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return false, err
	}

	resp, err := p.client.FromCompute().CloudServers().Get(ctx, p.projectID, id, nil)
	if err != nil {
		return false, err
	}
	if resp.StatusCode == 404 {
		return false, nil
	}
	if !resp.IsSuccess() {
		return false, fmt.Errorf("Aruba get error %d: %s", resp.StatusCode, formatAPIError(resp.StatusCode, resp.Error, resp.RawBody))
	}

	state := ""
	if resp.Data != nil && resp.Data.Status.State != nil {
		state = strings.ToLower(strings.TrimSpace(*resp.Data.Status.State))
	}
	if resp.Data != nil && resp.Data.Status.FailureReason != nil {
		reason := strings.TrimSpace(*resp.Data.Status.FailureReason)
		if state == "failed" {
			if reason == "" {
				reason = "unknown (check Aruba Cloud console)"
			}
			return false, &errors.InstanceFailedError{Reason: reason}
		}
	}
	return state == "running" || state == "started" || state == "active", nil
}

// Close releases resources.
func (p *Provisioner) Close() error {
	return nil
}

func parseInstanceID(instanceID string) (projectID, instanceIDPart string, err error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", "", fmt.Errorf("invalid Aruba instance ID format: %s", instanceID)
	}
	rest := strings.TrimPrefix(instanceID, InstanceIDPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid Aruba instance ID format: %s", instanceID)
	}
	return parts[0], parts[1], nil
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

// formatAPIError builds a detailed error string from Aruba API error responses.
// Includes Detail, Title, Extensions (e.g. validation errors), and raw body when available.
func formatAPIError(statusCode int, errResp *types.ErrorResponse, rawBody []byte) string {
	var parts []string
	if errResp != nil {
		if errResp.Detail != nil && *errResp.Detail != "" {
			parts = append(parts, *errResp.Detail)
		}
		if errResp.Title != nil && *errResp.Title != "" {
			parts = append(parts, "title: "+*errResp.Title)
		}
		if len(errResp.Extensions) > 0 {
			extJSON, _ := json.Marshal(errResp.Extensions)
			parts = append(parts, "extensions: "+string(extJSON))
		}
	}
	if len(rawBody) > 0 && len(rawBody) < 2048 {
		parts = append(parts, "raw: "+string(rawBody))
	} else if len(rawBody) >= 2048 {
		parts = append(parts, "raw: "+string(rawBody[:2048])+"...")
	}
	if len(parts) == 0 {
		return "unknown error"
	}
	return strings.Join(parts, "; ")
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

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
