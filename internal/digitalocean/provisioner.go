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
	"errors"
	"fmt"
	"net/http"
	"strconv"

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
	"github.com/digitalocean/godo"
	"golang.org/x/oauth2"
)

const (
	// InstanceIDPrefix is the prefix for DigitalOcean instance IDs (region/droplet-id)
	InstanceIDPrefix = "digitalocean/"
	// DefaultImage is the default Ubuntu 22.04 image slug for DigitalOcean
	DefaultImage = "ubuntu-22-04-x64"
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
	var instanceID string
	err := provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		var retryErr error
		instanceID, retryErr = p.createInstanceOnce(ctx, name, size, region, startupScript, imageFromConfig)
		return retryErr
	})
	if err != nil {
		return "", err
	}
	return instanceID, nil
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
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

// deleteInstanceOnce makes a single attempt to delete a droplet
func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, dropletID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	_, err = p.client.Droplets.Delete(ctx, dropletID)
	if err != nil {
		var errResp *godo.ErrorResponse
		if errors.As(err, &errResp) && errResp.Response != nil && errResp.Response.StatusCode == http.StatusNotFound {
			return nil
		}
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
	switch status {
	case "off", "archive":
		return false, &cberrors.InstanceFailedError{Reason: fmt.Sprintf("droplet status: %s", status)}
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

func parseInstanceID(instanceID string) (string, int, error) {
	region, idStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return "", 0, err
	}
	dropletID, err := strconv.Atoi(idStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid droplet ID in instance ID %s: %w", instanceID, err)
	}
	return region, dropletID, nil
}
