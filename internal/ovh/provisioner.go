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

package ovh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
	"github.com/ovh/go-ovh/ovh"
)

const (
	// InstanceIDPrefix is the prefix for OVH instance IDs (projectId/instanceId)
	InstanceIDPrefix = "ovh/"
	// DefaultImageID is the default Ubuntu 22.04 image ID for OVH Public Cloud (EU; may need adjustment per region)
	DefaultImageID = "Ubuntu 22.04"
)

// ovhInstanceCreateRequest is the body for POST /cloud/project/{serviceName}/instance
type ovhInstanceCreateRequest struct {
	FlavorID       string  `json:"flavorId"`
	ImageID        string  `json:"imageId"`
	Name           string  `json:"name"`
	Region         string  `json:"region"`
	UserData       *string `json:"userData,omitempty"`
	MonthlyBilling *bool   `json:"monthlyBilling,omitempty"`
}

// ovhInstance is the instance resource returned by OVH Cloud API
type ovhInstance struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Region string `json:"region"`
}

// Provisioner handles OVH Cloud instance provisioning
type Provisioner struct {
	client    *ovh.Client
	projectID string
	region    string
}

// NewProvisioner creates a new OVH Cloud provisioner.
// endpoint is the OVH API endpoint (e.g. "ovh-eu"), appKey, appSecret, consumerKey are from credentials.
func NewProvisioner(ctx context.Context, endpoint, appKey, appSecret, consumerKey, projectID, region string) (*Provisioner, error) {
	if projectID == "" {
		return nil, fmt.Errorf("OVH project ID is required")
	}
	if region == "" {
		return nil, fmt.Errorf("OVH region is required")
	}
	if appKey == "" || appSecret == "" || consumerKey == "" {
		return nil, fmt.Errorf("OVH application key, application secret, and consumer key are required")
	}
	if endpoint == "" {
		endpoint = "ovh-eu"
	}

	client, err := ovh.NewClient(endpoint, appKey, appSecret, consumerKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create OVH client: %w", err)
	}

	return &Provisioner{
		client:    client,
		projectID: projectID,
		region:    region,
	}, nil
}

// CreateInstance creates an OVH Cloud instance with the given name, flavor (instance type), region, and cloud-init script.
// Returns instanceID in the form "ovh/<projectId>/<instanceId>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, flavorID, region, startupScript, imageFromConfig string) (string, error) {
	var instanceID string
	err := provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		var retryErr error
		instanceID, retryErr = p.createInstanceOnce(ctx, name, flavorID, region, startupScript, imageFromConfig)
		return retryErr
	})
	if err != nil {
		return "", err
	}
	return instanceID, nil
}

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, flavorID, region, startupScript, imageFromConfig string) (string, error) {
	// Resolve flavor name → UUID (OVH API requires UUIDs, CloudBroker returns names like "b2-15")
	resolvedFlavorID, err := p.resolveFlavorID(ctx, flavorID, region)
	if err != nil {
		return "", fmt.Errorf("failed to resolve flavor %q: %w", flavorID, err)
	}

	// Resolve image name → UUID (OVH API requires UUIDs, default is "Ubuntu 22.04")
	imageID := imageFromConfig
	if imageID == "" {
		imageID = DefaultImageID
	}
	resolvedImageID, err := p.resolveImageID(ctx, imageID, region)
	if err != nil {
		return "", fmt.Errorf("failed to resolve image %q: %w", imageID, err)
	}

	req := ovhInstanceCreateRequest{
		FlavorID: resolvedFlavorID,
		ImageID:  resolvedImageID,
		Name:     name,
		Region:   region,
	}
	if startupScript != "" {
		req.UserData = &startupScript
	}
	monthlyBilling := false
	req.MonthlyBilling = &monthlyBilling

	path := fmt.Sprintf("/cloud/project/%s/instance", p.projectID)
	var result ovhInstance
	if err := p.client.PostWithContext(ctx, path, req, &result); err != nil {
		return "", fmt.Errorf("failed to create instance: %w", err)
	}

	instanceID := fmt.Sprintf("%s%s/%s", InstanceIDPrefix, p.projectID, result.ID)
	return instanceID, nil
}

// DeleteInstance deletes an OVH Cloud instance. instanceID must be in the form "ovh/<projectId>/<instanceId>".
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

	path := fmt.Sprintf("/cloud/project/%s/instance/%s", p.projectID, id)
	if err := p.client.DeleteWithContext(ctx, path, nil); err != nil {
		var apiErr *ovh.APIError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	return nil
}

// ListInstances returns all instances in the project (across regions).
func (p *Provisioner) ListInstances(ctx context.Context) ([]ovhInstanceInfo, error) {
	path := fmt.Sprintf("/cloud/project/%s/instance", p.projectID)
	var raw json.RawMessage
	if err := p.client.GetWithContext(ctx, path, &raw); err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	// OVH API returns map[region][]instanceId: {"GRA7": ["id1","id2"], "DE1": ["id3"]}
	var regionMap map[string][]string
	if err := json.Unmarshal(raw, &regionMap); err == nil {
		var result []ovhInstanceInfo
		for _, regionIDs := range regionMap {
			for _, id := range regionIDs {
				info, err := p.getInstanceInfo(ctx, id)
				if err != nil {
					continue
				}
				result = append(result, *info)
			}
		}
		return result, nil
	}
	// Fallback: []string or []object
	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil {
		var result []ovhInstanceInfo
		for _, id := range ids {
			info, err := p.getInstanceInfo(ctx, id)
			if err != nil {
				continue
			}
			result = append(result, *info)
		}
		return result, nil
	}
	var objs []ovhInstanceInfo
	if err := json.Unmarshal(raw, &objs); err == nil {
		return objs, nil
	}
	return nil, fmt.Errorf("unexpected OVH instances API response format")
}

// ovhInstanceInfo holds instance list item fields.
type ovhInstanceInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Region string `json:"region"`
}

func (p *Provisioner) getInstanceInfo(ctx context.Context, instanceID string) (*ovhInstanceInfo, error) {
	path := fmt.Sprintf("/cloud/project/%s/instance/%s", p.projectID, instanceID)
	var result ovhInstanceInfo
	if err := p.client.GetWithContext(ctx, path, &result); err != nil {
		return nil, err
	}
	result.ID = instanceID
	return &result, nil
}

// GetInstanceStatus returns the instance status (ACTIVE, PENDING, etc.).
func (p *Provisioner) GetInstanceStatus(ctx context.Context, instanceID string) (string, error) {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("/cloud/project/%s/instance/%s", p.projectID, id)
	var result ovhInstance
	if err := p.client.GetWithContext(ctx, path, &result); err != nil {
		return "", fmt.Errorf("failed to get instance: %w", err)
	}
	return result.Status, nil
}

// IsInstanceRunning returns true if the instance is in ACTIVE state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	status, err := p.GetInstanceStatus(ctx, instanceID)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(status, "ACTIVE"), nil
}

// Close releases resources. OVH client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the OVH instance name for a NodeClaim.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

func parseInstanceID(instanceID string) (string, string, error) {
	return provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
}

// ovhFlavor represents a flavor from the OVH API
type ovhFlavor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ovhImage represents an image from the OVH API
type ovhImage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// resolveFlavorID resolves a flavor name (e.g. "b2-15") to its UUID using the OVH API.
// If the value is already a UUID (36 chars with dashes), it is returned as-is.
// If the API call fails or the name is not found, an error is returned (OVH API requires UUIDs).
func (p *Provisioner) resolveFlavorID(ctx context.Context, nameOrID, region string) (string, error) {
	if looksLikeUUID(nameOrID) {
		return nameOrID, nil
	}

	path := fmt.Sprintf("/cloud/project/%s/flavor?region=%s", p.projectID, region)
	var flavors []ovhFlavor
	if err := p.client.GetWithContext(ctx, path, &flavors); err != nil {
		return "", fmt.Errorf("failed to list flavors for region %q: %w", region, err)
	}

	for _, f := range flavors {
		if strings.EqualFold(f.Name, nameOrID) {
			return f.ID, nil
		}
	}

	return "", fmt.Errorf("flavor %q not found in region %q (available: %d flavors)", nameOrID, region, len(flavors))
}

// resolveImageID resolves an image name (e.g. "Ubuntu 22.04") to its UUID using the OVH API.
// If the value is already a UUID, it is returned as-is.
// If the API call fails or the name is not found, an error is returned (OVH API requires UUIDs).
func (p *Provisioner) resolveImageID(ctx context.Context, nameOrID, region string) (string, error) {
	if looksLikeUUID(nameOrID) {
		return nameOrID, nil
	}

	path := fmt.Sprintf("/cloud/project/%s/image?region=%s", p.projectID, region)
	var images []ovhImage
	if err := p.client.GetWithContext(ctx, path, &images); err != nil {
		return "", fmt.Errorf("failed to list images for region %q: %w", region, err)
	}

	// Try exact match first, then prefix match (e.g. "Ubuntu 22.04" matches "Ubuntu 22.04 - ...")
	for _, img := range images {
		if strings.EqualFold(img.Name, nameOrID) {
			return img.ID, nil
		}
	}
	for _, img := range images {
		if strings.Contains(strings.ToLower(img.Name), strings.ToLower(nameOrID)) {
			return img.ID, nil
		}
	}

	return "", fmt.Errorf("image %q not found in region %q (available: %d images)", nameOrID, region, len(images))
}

// looksLikeUUID returns true if s looks like a UUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
