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
	"strings"
	"time"

	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

const (
	// InstanceIDPrefix is the prefix for Scaleway instance IDs (zone/server-id)
	InstanceIDPrefix = "scaleway/"
	// DefaultImage is the default Ubuntu 22.04 image identifier for Scaleway
	DefaultImage = "ubuntu_jammy"
	// MaxRetries is the maximum number of retry attempts
	MaxRetries = 3
	// RetryDelay is the initial delay between retries
	RetryDelay = 2 * time.Second
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
// Client credentials (SCW_ACCESS_KEY, SCW_SECRET_KEY) can come from env or secrets.
func NewProvisioner(ctx context.Context, zoneStr, projectID, organizationID string) (*Provisioner, error) {
	zone, err := scw.ParseZone(zoneStr)
	if err != nil {
		return nil, fmt.Errorf("invalid zone %q: %w", zoneStr, err)
	}

	opts := []scw.ClientOption{
		scw.WithEnv(),
		scw.WithDefaultZone(zone),
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

	// Explicitly set project/organization on the request when provided.
	// Some Scaleway APIs require one of these on the request even if the
	// client has defaults configured.
	if p.projectID != "" {
		req.Project = scw.StringPtr(p.projectID)
	}
	if p.organizationID != "" {
		req.Organization = scw.StringPtr(p.organizationID)
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
			_ = p.DeleteInstance(ctx, instanceID)
			return "", fmt.Errorf("failed to set server user data: %w", err)
		}
	}

	// Power on the server (creation may leave it stopped until user data is set)
	_, err = p.api.ServerAction(&instance.ServerActionRequest{
		Zone:     zone,
		ServerID: serverID,
		Action:   instance.ServerActionPoweron,
	}, scw.WithContext(ctx))
	if err != nil {
		_ = p.DeleteInstance(ctx, instanceID)
		return "", fmt.Errorf("failed to power on server: %w", err)
	}

	return instanceID, nil
}

// DeleteInstance deletes a Scaleway instance. instanceID must be in the form "scaleway/<zone>/<server-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	zone, serverID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	err = p.api.DeleteServer(&instance.DeleteServerRequest{
		Zone:     zone,
		ServerID: serverID,
	}, scw.WithContext(ctx))
	if err != nil {
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
	return state == instance.ServerStateRunning, nil
}

// GenerateInstanceName returns the Scaleway server name for a NodeClaim.
// By convention it equals the NodeClaim name (same as GCP) for consistent node naming.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

// parseInstanceID parses "scaleway/<zone>/<server-id>" into zone and server ID.
func parseInstanceID(instanceID string) (scw.Zone, string, error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", "", fmt.Errorf("invalid Scaleway instance ID format: %s", instanceID)
	}
	rest := strings.TrimPrefix(instanceID, InstanceIDPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid Scaleway instance ID format: %s", instanceID)
	}
	zone, err := scw.ParseZone(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("invalid zone in instance ID: %w", err)
	}
	return zone, parts[1], nil
}
