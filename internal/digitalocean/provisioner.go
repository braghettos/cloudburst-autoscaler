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

package digitalocean

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/oauth2"
)

const (
	// InstanceIDPrefix is the prefix for DigitalOcean instance IDs (region/droplet-id)
	InstanceIDPrefix = "digitalocean/"
	// DefaultImage is the default Ubuntu 22.04 image slug for DigitalOcean
	DefaultImage = "ubuntu-22-04-x64"
	// MaxRetries is the maximum number of retry attempts
	MaxRetries = 3
	// RetryDelay is the initial delay between retries
	RetryDelay = 2 * time.Second
)

// Provisioner handles DigitalOcean Droplet provisioning
type Provisioner struct {
	client *godo.Client
	region string
}

// NewProvisioner creates a new DigitalOcean provisioner.
// apiToken is the DigitalOcean API token, region is the region slug (e.g. "nyc1", "sfo3").
func NewProvisioner(ctx context.Context, apiToken, region string) (*Provisioner, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("DigitalOcean API token is required")
	}

	tokenSrc := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: apiToken})
	oauthClient := oauth2.NewClient(ctx, tokenSrc)
	client := godo.NewClient(oauthClient)

	return &Provisioner{
		client: client,
		region: region,
	}, nil
}

// CreateInstance creates a DigitalOcean Droplet with the given name, size, and cloud-init script.
// Returns instanceID in the form "digitalocean/<region>/<droplet-id>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, size, region, startupScript, imageFromConfig string) (string, error) {
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

		instanceID, err := p.createInstanceOnce(ctx, name, size, region, startupScript, imageFromConfig)
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

// createInstanceOnce makes a single attempt to create a droplet
func (p *Provisioner) createInstanceOnce(ctx context.Context, name, size, region, startupScript, imageFromConfig string) (string, error) {
	imageRef := imageFromConfig
	if imageRef == "" {
		imageRef = DefaultImage
	}

	createRequest := &godo.DropletCreateRequest{
		Name:   name,
		Region: region,
		Size:   size,
		Image: godo.DropletCreateImage{
			Slug: imageRef,
		},
		UserData: startupScript,
	}

	droplet, _, err := p.client.Droplets.Create(ctx, createRequest)
	if err != nil {
		return "", fmt.Errorf("failed to create droplet: %w", err)
	}

	instanceID := fmt.Sprintf("%s%s/%d", InstanceIDPrefix, region, droplet.ID)
	return instanceID, nil
}

// DeleteInstance deletes a DigitalOcean Droplet. instanceID must be in the form "digitalocean/<region>/<droplet-id>".
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

// deleteInstanceOnce makes a single attempt to delete a droplet
func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, dropletID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	_, err = p.client.Droplets.Delete(ctx, dropletID)
	if err != nil {
		return fmt.Errorf("failed to delete droplet: %w", err)
	}

	return nil
}

// GetDropletStatus returns the droplet status (active, off, etc.).
func (p *Provisioner) GetDropletStatus(ctx context.Context, instanceID string) (string, error) {
	_, dropletID, err := parseInstanceID(instanceID)
	if err != nil {
		return "", err
	}

	droplet, _, err := p.client.Droplets.Get(ctx, dropletID)
	if err != nil {
		return "", fmt.Errorf("failed to get droplet: %w", err)
	}
	if droplet == nil {
		return "", fmt.Errorf("droplet %d not found", dropletID)
	}
	return droplet.Status, nil
}

// IsInstanceRunning returns true if the droplet is in active state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	status, err := p.GetDropletStatus(ctx, instanceID)
	if err != nil {
		return false, err
	}
	return status == "active", nil
}

// Close releases resources. DigitalOcean client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the DigitalOcean droplet name for a NodeClaim.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

// parseInstanceID parses "digitalocean/<region>/<droplet-id>" into region and droplet ID.
func parseInstanceID(instanceID string) (string, int, error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", 0, fmt.Errorf("invalid DigitalOcean instance ID format: %s", instanceID)
	}
	rest := strings.TrimPrefix(instanceID, InstanceIDPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid DigitalOcean instance ID format: %s", instanceID)
	}
	dropletID, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid droplet ID in instance ID %s: %w", instanceID, err)
	}
	return parts[0], dropletID, nil
}

// isRetryableError checks if an error is retryable
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	retryableErrors := []string{
		"rate limit",
		"quota",
		"temporary",
		"timeout",
		"deadline exceeded",
		"unavailable",
		"503",
		"502",
		"500",
	}

	for _, retryable := range retryableErrors {
		if strings.Contains(strings.ToLower(errStr), retryable) {
			return true
		}
	}

	return false
}
