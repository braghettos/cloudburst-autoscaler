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

package scaleway

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

const (
	// InstanceIDPrefix is the prefix for Scaleway instance IDs (zone/server-id)
	InstanceIDPrefix = "scaleway/"
	// DefaultImage is the default Ubuntu 22.04 image identifier for Scaleway
	DefaultImage = "ubuntu_jammy"
)

// Provisioner handles Scaleway instance provisioning
type Provisioner struct {
	api            *instance.API
	zone           scw.Zone
	projectID      string
	organizationID string
}

// NewProvisioner creates a new Scaleway provisioner.
// zoneStr, projectID, and organizationID come from NodeClass.Spec.Scaleway.
// When both projectID and organizationID are empty, they are read from
// SCW_DEFAULT_PROJECT_ID and SCW_DEFAULT_ORGANIZATION_ID env vars (thread-safe).
// When accessKey and secretKey are non-empty, they are used via scw.WithAuth (thread-safe).
// When both are empty, the client falls back to scw.WithEnv() (SCW_ACCESS_KEY, SCW_SECRET_KEY).
func NewProvisioner(ctx context.Context, zoneStr, projectID, organizationID, accessKey, secretKey string) (*Provisioner, error) {
	zone, err := scw.ParseZone(zoneStr)
	if err != nil {
		return nil, fmt.Errorf("invalid zone %q: %w", zoneStr, err)
	}

	if projectID == "" {
		projectID = os.Getenv("SCW_DEFAULT_PROJECT_ID")
	}
	if organizationID == "" {
		organizationID = os.Getenv("SCW_DEFAULT_ORGANIZATION_ID")
	}

	opts := []scw.ClientOption{
		scw.WithDefaultZone(zone),
	}
	if accessKey != "" && secretKey != "" {
		opts = append(opts, scw.WithAuth(accessKey, secretKey))
	} else {
		opts = append(opts, scw.WithEnv())
	}
	if projectID != "" {
		opts = append(opts, scw.WithDefaultProjectID(projectID))
	}
	if organizationID != "" {
		opts = append(opts, scw.WithDefaultOrganizationID(organizationID))
	}

	client, err := scw.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Scaleway client: %w", err)
	}

	api := instance.NewAPI(client)
	return &Provisioner{
		api:            api,
		zone:           zone,
		projectID:      projectID,
		organizationID: organizationID,
	}, nil
}

// CreateInstance creates a Scaleway instance (server) with the given name, commercial type, and cloud-init script.
// imageFromConfig is optional (e.g. from NodeClass); if empty, defaults to Ubuntu 22.04.
// Returns instanceID in the form "scaleway/<zone>/<server-id>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, commercialType, zoneStr, startupScript, imageFromConfig string) (string, error) {
	var instanceID string
	err := provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		var retryErr error
		instanceID, retryErr = p.createInstanceOnce(ctx, name, commercialType, zoneStr, startupScript, imageFromConfig)
		return retryErr
	})
	if err != nil {
		return "", err
	}
	return instanceID, nil
}

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, commercialType, zoneStr, startupScript, imageFromConfig string) (string, error) {
	zone, err := scw.ParseZone(zoneStr)
	if err != nil {
		return "", fmt.Errorf("invalid zone: %w", err)
	}

	imageRef := imageFromConfig
	if imageRef == "" {
		imageRef = DefaultImage
	}

	req := &instance.CreateServerRequest{
		Zone:              zone,
		Name:              name,
		CommercialType:    commercialType,
		Image:             scw.StringPtr(imageRef),
		DynamicIPRequired: scw.BoolPtr(true),
	}

	if p.projectID != "" {
		req.Project = scw.StringPtr(p.projectID)
	} else if p.organizationID != "" {
		req.Project = scw.StringPtr(p.organizationID)
	}

	createResp, err := p.api.CreateServer(req, scw.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to create server: %w", err)
	}

	serverID := createResp.Server.ID
	instanceID := fmt.Sprintf("%s%s/%s", InstanceIDPrefix, zone, serverID)

	if startupScript != "" {
		err = p.api.SetServerUserData(&instance.SetServerUserDataRequest{
			Zone:     zone,
			ServerID: serverID,
			Key:      "cloud-init",
			Content:  strings.NewReader(startupScript),
		}, scw.WithContext(ctx))
		if err != nil {
			_ = p.deleteInstanceOnce(ctx, instanceID)
			return "", fmt.Errorf("failed to set server user data: %w", err)
		}
	}

	_, err = p.api.ServerAction(&instance.ServerActionRequest{
		Zone:     zone,
		ServerID: serverID,
		Action:   instance.ServerActionPoweron,
	}, scw.WithContext(ctx))
	if err != nil {
		_ = p.deleteInstanceOnce(ctx, instanceID)
		return "", fmt.Errorf("failed to power on server: %w", err)
	}

	return instanceID, nil
}

// DeleteInstance deletes a Scaleway instance. instanceID must be in the form "scaleway/<zone>/<server-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	zone, serverID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	// Scaleway requires the server to be powered off before deletion.
	// Check state and power off if running.
	resp, err := p.api.GetServer(&instance.GetServerRequest{
		Zone:     zone,
		ServerID: serverID,
	}, scw.WithContext(ctx))
	if err != nil {
		// If the server is already gone (404), treat as success
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "404") {
			return nil
		}
		return fmt.Errorf("failed to get server state: %w", err)
	}

	if resp.Server.State == instance.ServerStateRunning || resp.Server.State == instance.ServerStateStarting {
		_, err = p.api.ServerAction(&instance.ServerActionRequest{
			Zone:     zone,
			ServerID: serverID,
			Action:   instance.ServerActionPoweroff,
		}, scw.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("failed to power off server before deletion: %w", err)
		}
		// Wait for server to reach stopped state
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled while waiting for power off: %w", ctx.Err())
			case <-time.After(2 * time.Second):
			}
			sr, err := p.api.GetServer(&instance.GetServerRequest{
				Zone:     zone,
				ServerID: serverID,
			}, scw.WithContext(ctx))
			if err != nil {
				break // server may already be gone
			}
			if sr.Server.State == instance.ServerStateStopped || sr.Server.State == instance.ServerStateStoppedInPlace {
				break
			}
		}
	}

	err = p.api.DeleteServer(&instance.DeleteServerRequest{
		Zone:     zone,
		ServerID: serverID,
	}, scw.WithContext(ctx))
	if err != nil {
		// If already gone, treat as success
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "404") {
			return nil
		}
		return fmt.Errorf("failed to delete server: %w", err)
	}
	return nil
}

// GetInstanceStatus returns the server state (running, stopped, etc.).
func (p *Provisioner) GetInstanceStatus(ctx context.Context, instanceID string) (instance.ServerState, error) {
	zone, serverID, err := parseInstanceID(instanceID)
	if err != nil {
		return "", err
	}

	resp, err := p.api.GetServer(&instance.GetServerRequest{
		Zone:     zone,
		ServerID: serverID,
	}, scw.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to get server: %w", err)
	}
	return resp.Server.State, nil
}

// IsInstanceRunning returns true if the server is in running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	state, err := p.GetInstanceStatus(ctx, instanceID)
	if err != nil {
		return false, err
	}
	switch state {
	case instance.ServerStateStopped, instance.ServerStateStoppedInPlace, instance.ServerStateLocked:
		return false, &cberrors.InstanceFailedError{Reason: fmt.Sprintf("server state: %s", state)}
	}
	return state == instance.ServerStateRunning, nil
}

// Close releases resources. Scaleway API client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the Scaleway server name for a NodeClaim.
// By convention it equals the NodeClaim name (same as GCP) for consistent node naming.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

func parseInstanceID(instanceID string) (scw.Zone, string, error) {
	zoneStr, serverID, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return "", "", err
	}
	zone, err := scw.ParseZone(zoneStr)
	if err != nil {
		return "", "", fmt.Errorf("invalid zone in instance ID: %w", err)
	}
	return zone, serverID, nil
}
