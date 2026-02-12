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

package hetzner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

const (
	// InstanceIDPrefix is the prefix for Hetzner instance IDs (location/server-id)
	InstanceIDPrefix = "hetzner/"
	// DefaultImage is the default Ubuntu 22.04 image for Hetzner Cloud
	DefaultImage = "ubuntu-22.04"
	// MaxRetries is the maximum number of retry attempts
	MaxRetries = 3
	// RetryDelay is the initial delay between retries
	RetryDelay = 2 * time.Second
)

// Provisioner handles Hetzner Cloud server provisioning
type Provisioner struct {
	client   *hcloud.Client
	location string
}

// NewProvisioner creates a new Hetzner Cloud provisioner.
// apiToken is the Hetzner Cloud API token, location is the datacenter location (e.g. "fsn1").
func NewProvisioner(ctx context.Context, apiToken, location string) (*Provisioner, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("Hetzner API token is required")
	}

	client := hcloud.NewClient(hcloud.WithToken(apiToken))

	return &Provisioner{
		client:   client,
		location: location,
	}, nil
}

// CreateInstance creates a Hetzner Cloud server with the given name, server type, and cloud-init script.
// Returns instanceID in the form "hetzner/<location>/<server-id>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, serverType, location, startupScript, imageFromConfig string) (string, error) {
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

		instanceID, err := p.createInstanceOnce(ctx, name, serverType, location, startupScript, imageFromConfig)
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

// createInstanceOnce makes a single attempt to create a server
func (p *Provisioner) createInstanceOnce(ctx context.Context, name, serverType, location, startupScript, imageFromConfig string) (string, error) {
	imageRef := imageFromConfig
	if imageRef == "" {
		imageRef = DefaultImage
	}

	opts := hcloud.ServerCreateOpts{
		Name: name,
		ServerType: &hcloud.ServerType{
			Name: serverType,
		},
		Image: &hcloud.Image{
			Name: imageRef,
		},
		Location: &hcloud.Location{
			Name: location,
		},
		UserData: startupScript,
	}

	result, _, err := p.client.Server.Create(ctx, opts)
	if err != nil {
		return "", fmt.Errorf("failed to create server: %w", err)
	}

	// Wait for the create action to complete
	if result.Action != nil {
		_, errCh := p.client.Action.WatchProgress(ctx, result.Action)
		if err := <-errCh; err != nil {
			return "", fmt.Errorf("failed waiting for server creation: %w", err)
		}
	}

	// Wait for any next actions (e.g. network setup)
	for _, action := range result.NextActions {
		_, errCh := p.client.Action.WatchProgress(ctx, action)
		if err := <-errCh; err != nil {
			return "", fmt.Errorf("failed waiting for server next action: %w", err)
		}
	}

	serverID := result.Server.ID
	instanceID := fmt.Sprintf("%s%s/%d", InstanceIDPrefix, location, serverID)

	return instanceID, nil
}

// DeleteInstance deletes a Hetzner Cloud server. instanceID must be in the form "hetzner/<location>/<server-id>".
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

// deleteInstanceOnce makes a single attempt to delete a server
func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, serverID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	server := &hcloud.Server{ID: serverID}
	result, _, err := p.client.Server.DeleteWithResult(ctx, server)
	if err != nil {
		return fmt.Errorf("failed to delete server: %w", err)
	}

	// Wait for the delete action to complete
	if result.Action != nil {
		_, errCh := p.client.Action.WatchProgress(ctx, result.Action)
		if err := <-errCh; err != nil {
			return fmt.Errorf("failed waiting for server deletion: %w", err)
		}
	}

	return nil
}

// GetInstanceStatus returns the server status (running, off, etc.).
func (p *Provisioner) GetInstanceStatus(ctx context.Context, instanceID string) (hcloud.ServerStatus, error) {
	_, serverID, err := parseInstanceID(instanceID)
	if err != nil {
		return "", err
	}

	server, _, err := p.client.Server.GetByID(ctx, serverID)
	if err != nil {
		return "", fmt.Errorf("failed to get server: %w", err)
	}
	if server == nil {
		return "", fmt.Errorf("server %d not found", serverID)
	}
	return server.Status, nil
}

// IsInstanceRunning returns true if the server is in running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	status, err := p.GetInstanceStatus(ctx, instanceID)
	if err != nil {
		return false, err
	}
	return status == hcloud.ServerStatusRunning, nil
}

// GenerateInstanceName returns the Hetzner server name for a NodeClaim.
// By convention it equals the NodeClaim name (same as GCP/Scaleway) for consistent node naming.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

// parseInstanceID parses "hetzner/<location>/<server-id>" into location and server ID.
func parseInstanceID(instanceID string) (string, int64, error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", 0, fmt.Errorf("invalid Hetzner instance ID format: %s", instanceID)
	}
	rest := strings.TrimPrefix(instanceID, InstanceIDPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid Hetzner instance ID format: %s", instanceID)
	}
	serverID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid server ID in instance ID %s: %w", instanceID, err)
	}
	return parts[0], serverID, nil
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
