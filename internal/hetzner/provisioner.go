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
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

const (
	// InstanceIDPrefix is the prefix for Hetzner instance IDs (location/server-id)
	InstanceIDPrefix = "hetzner/"
	// DefaultImage is the default Ubuntu 22.04 image for Hetzner Cloud
	DefaultImage = "ubuntu-22.04"
)

// Provisioner handles Hetzner Cloud server provisioning
type Provisioner struct {
	client   *hcloud.Client
	location string
}

// NewProvisioner creates a new Hetzner Cloud provisioner.
// apiToken is the Hetzner Cloud API token, location is the datacenter location (e.g. "fsn1").
// When HETZNER_INSECURE_SKIP_VERIFY is set (e.g. for in-cluster mock with hostAliases),
// TLS verification is skipped so a self-signed mock cert is accepted.
func NewProvisioner(ctx context.Context, apiToken, location string) (*Provisioner, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("Hetzner API token is required")
	}

	opts := []hcloud.ClientOption{hcloud.WithToken(apiToken)}
	if os.Getenv("HETZNER_INSECURE_SKIP_VERIFY") == "true" {
		transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		opts = append(opts, hcloud.WithHTTPClient(&http.Client{Transport: transport}))
	}
	client := hcloud.NewClient(opts...)

	return &Provisioner{
		client:   client,
		location: location,
	}, nil
}

// CreateInstance creates a Hetzner Cloud server with the given name, server type, and cloud-init script.
// Returns instanceID in the form "hetzner/<location>/<server-id>".
// When the chosen location returns "server location disabled", tries alternative locations from the API.
func (p *Provisioner) CreateInstance(ctx context.Context, name, serverType, location, startupScript, imageFromConfig string) (string, error) {
	instanceID, err := p.createInstanceOnce(ctx, name, serverType, location, startupScript, imageFromConfig)
	if err == nil {
		return instanceID, nil
	}

	// When location is disabled, try alternative locations instead of failing immediately.
	if isLocationDisabledError(err) {
		altID, altErr := p.tryAlternativeLocations(ctx, name, serverType, location, startupScript, imageFromConfig, err)
		if altErr == nil {
			return altID, nil
		}
		return "", altErr
	}

	// For non-location errors, check retryability before entering retry loop.
	if !isRetryableError(err) {
		return "", err
	}
	var retInstanceID string
	retryErr := provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, isRetryableError, func() error {
		var retryErr error
		retInstanceID, retryErr = p.createInstanceOnce(ctx, name, serverType, location, startupScript, imageFromConfig)
		return retryErr
	})
	if retryErr != nil {
		return "", retryErr
	}
	return retInstanceID, nil
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

	result, resp, err := p.client.Server.Create(ctx, opts)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		// Include full error and response for debugging (Hetzner API sometimes returns generic "server_error")
		errMsg := err.Error()
		if resp != nil && resp.Body != nil {
			// Response body may contain API error details
			bodyBytes, _ := io.ReadAll(resp.Body)
			if len(bodyBytes) > 0 {
				errMsg = fmt.Sprintf("%s (API response: %s)", errMsg, strings.TrimSpace(string(bodyBytes)))
			}
		}
		if errMsg == "" || strings.TrimSpace(errMsg) == "" {
			errMsg = "unknown Hetzner API error"
		}
		return "", fmt.Errorf("failed to create server: %s", errMsg)
	}

	// Wait for the create action to complete
	if result.Action != nil {
		if err := p.client.Action.WaitFor(ctx, result.Action); err != nil {
			return "", fmt.Errorf("failed waiting for server creation: %w", err)
		}
	}

	// Wait for any next actions (e.g. network setup)
	for _, action := range result.NextActions {
		if err := p.client.Action.WaitFor(ctx, action); err != nil {
			return "", fmt.Errorf("failed waiting for server next action: %w", err)
		}
	}

	serverID := result.Server.ID
	instanceID := fmt.Sprintf("%s%s/%d", InstanceIDPrefix, location, serverID)

	return instanceID, nil
}

// DeleteInstance deletes a Hetzner Cloud server. instanceID must be in the form "hetzner/<location>/<server-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, isRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

// deleteInstanceOnce makes a single attempt to delete a server.
// If the server is already gone (404/not found), treat as success (idempotent).
func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, serverID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	server := &hcloud.Server{ID: serverID}
	result, _, err := p.client.Server.DeleteWithResult(ctx, server)
	if err != nil {
		// If the server is already gone (404), treat as success
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
			return nil
		}
		return fmt.Errorf("failed to delete server: %w", err)
	}

	// Wait for the delete action to complete
	if result.Action != nil {
		if err := p.client.Action.WaitFor(ctx, result.Action); err != nil {
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
	switch status {
	case hcloud.ServerStatusDeleting, hcloud.ServerStatusUnknown:
		return false, &cberrors.InstanceFailedError{Reason: fmt.Sprintf("server status: %s", status)}
	}
	return status == hcloud.ServerStatusRunning, nil
}

// Close releases resources. Hetzner client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the Hetzner server name for a NodeClaim.
// By convention it equals the NodeClaim name (same as GCP/Scaleway) for consistent node naming.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

func parseInstanceID(instanceID string) (string, int64, error) {
	location, idStr, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return "", 0, err
	}
	serverID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid server ID in instance ID %s: %w", instanceID, err)
	}
	return location, serverID, nil
}

// isLocationDisabledError returns true when the Hetzner API reports the location as disabled.
func isLocationDisabledError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "resource_unavailable") || strings.Contains(s, "location disabled")
}

// tryAlternativeLocations attempts to create the server in other Hetzner locations when the chosen one is disabled.
// Removes any orphaned server with the same name (e.g. in the failed location from a previous run) before retrying.
func (p *Provisioner) tryAlternativeLocations(ctx context.Context, name, serverType, failedLocation, startupScript, imageFromConfig string, originalErr error) (string, error) {
	// Remove orphaned server that may block creation (e.g. in failed location from previous run)
	if err := p.deleteServerByNameIfInLocation(ctx, name, failedLocation); err != nil {
		return "", fmt.Errorf("location disabled (%s), failed to remove orphaned server: %w", failedLocation, err)
	}

	locations, err := p.client.Location.All(ctx)
	if err != nil {
		return "", fmt.Errorf("location disabled (%s), failed to list alternatives: %w", failedLocation, originalErr)
	}

	var lastErr error
	for _, loc := range locations {
		if loc.Name == failedLocation {
			continue
		}
		instanceID, err := p.createInstanceOnce(ctx, name, serverType, loc.Name, startupScript, imageFromConfig)
		if err == nil {
			return instanceID, nil
		}
		// If "server name is already used", remove the blocking server (may be in this location) and retry
		if isUniquenessError(err) {
			_ = p.deleteServerByName(ctx, name)
			instanceID, retryErr := p.createInstanceOnce(ctx, name, serverType, loc.Name, startupScript, imageFromConfig)
			if retryErr == nil {
				return instanceID, nil
			}
			lastErr = retryErr
		} else {
			lastErr = err
		}
	}

	return "", fmt.Errorf("location %s disabled, tried %d alternatives without success: %w", failedLocation, len(locations)-1, lastErr)
}

// deleteServerByNameIfInLocation deletes a server by name only if it exists in the given location.
func (p *Provisioner) deleteServerByNameIfInLocation(ctx context.Context, name, location string) error {
	server, _, err := p.client.Server.GetByName(ctx, name)
	if err != nil || server == nil {
		return nil // no server or error looking up - nothing to delete
	}
	if server.Location == nil || server.Location.Name != location {
		return nil // server is in a different location, leave it
	}
	result, _, err := p.client.Server.DeleteWithResult(ctx, server)
	if err != nil {
		return err
	}
	if result.Action != nil {
		_ = p.client.Action.WaitFor(ctx, result.Action)
	}
	return nil
}

// deleteServerByName deletes a server by name if it exists (any location).
func (p *Provisioner) deleteServerByName(ctx context.Context, name string) error {
	server, _, err := p.client.Server.GetByName(ctx, name)
	if err != nil || server == nil {
		return nil
	}
	_, _, err = p.client.Server.DeleteWithResult(ctx, server)
	return err
}

// isUniquenessError returns true when the Hetzner API reports server name is already used.
func isUniquenessError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "uniqueness_error") || strings.Contains(s, "server name is already used")
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	for _, nr := range []string{"resource_unavailable", "location disabled"} {
		if strings.Contains(errStr, nr) {
			return false
		}
	}
	return provutil.IsRetryableError(err)
}
