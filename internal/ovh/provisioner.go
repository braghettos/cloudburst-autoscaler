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
	"fmt"
	"strings"
	"time"

	"github.com/ovh/go-ovh/ovh"
)

const (
	// InstanceIDPrefix is the prefix for OVH instance IDs (projectId/instanceId)
	InstanceIDPrefix = "ovh/"
	// DefaultImageID is the default Ubuntu 22.04 image ID for OVH Public Cloud (EU; may need adjustment per region)
	DefaultImageID = "Ubuntu 22.04"
	// MaxRetries is the maximum number of retry attempts
	MaxRetries = 3
	// RetryDelay is the initial delay between retries
	RetryDelay = 2 * time.Second
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
	var lastErr error

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			waitTime := RetryDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("context cancelled: %w", ctx.Err())
			case <-time.After(waitTime):
			}
		}

		instanceID, err := p.createInstanceOnce(ctx, name, flavorID, region, startupScript, imageFromConfig)
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
	var lastErr error

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			waitTime := RetryDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled: %w", ctx.Err())
			case <-time.After(waitTime):
			}
		}

		err := p.deleteInstanceOnce(ctx, instanceID)
		if err == nil {
			return nil
		}

		if !isRetryableError(err) {
			return err
		}

		lastErr = err
	}

	return fmt.Errorf("failed after %d attempts: %w", MaxRetries+1, lastErr)
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/cloud/project/%s/instance/%s", p.projectID, id)
	if err := p.client.DeleteWithContext(ctx, path, nil); err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	return nil
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
	return strings.ToUpper(status) == "ACTIVE", nil
}

// Close releases resources. OVH client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the OVH instance name for a NodeClaim.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

// parseInstanceID parses "ovh/<projectId>/<instanceId>" into projectId and instanceId.
func parseInstanceID(instanceID string) (string, string, error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", "", fmt.Errorf("invalid OVH instance ID format: %s", instanceID)
	}
	rest := strings.TrimPrefix(instanceID, InstanceIDPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid OVH instance ID format: %s", instanceID)
	}
	return parts[0], parts[1], nil
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

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	retryable := []string{"rate limit", "quota", "temporary", "timeout", "deadline exceeded", "unavailable", "503", "502", "500"}
	for _, r := range retryable {
		if strings.Contains(strings.ToLower(errStr), r) {
			return true
		}
	}
	return false
}
