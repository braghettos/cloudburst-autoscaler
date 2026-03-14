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

package upcloud

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/service"
	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
)

const (
	// InstanceIDPrefix is the prefix for UpCloud instance IDs (zone/server-uuid)
	InstanceIDPrefix = "upcloud/"
	// DefaultTemplate is the default Ubuntu 22.04 template UUID for UpCloud
	DefaultTemplate = "01000000-0000-4000-8000-000030220200"
)

// Provisioner handles UpCloud server provisioning
type Provisioner struct {
	svc    *service.Service
	zone   string
	region string
}

// NewProvisioner creates a new UpCloud provisioner.
// apiToken is the UpCloud API token (from hub.upcloud.com/account/api-tokens).
// zone is the UpCloud zone (e.g. "fi-hel1", "de-fra1").
func NewProvisioner(ctx context.Context, apiToken, zone string) (*Provisioner, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("UpCloud API token is required")
	}
	if zone == "" {
		return nil, fmt.Errorf("UpCloud zone is required")
	}

	clnt := client.New("", "", client.WithBearerAuth(apiToken), client.WithTimeout(30*time.Second))
	svc := service.New(clnt)

	return &Provisioner{
		svc:    svc,
		zone:   zone,
		region: zone,
	}, nil
}

// CreateInstance creates an UpCloud server with the given name, plan, and cloud-init script.
// Returns instanceID in the form "upcloud/<zone>/<server-uuid>".
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

// createInstanceOnce makes a single attempt to create a server
func (p *Provisioner) createInstanceOnce(ctx context.Context, name, plan, zone, startupScript, imageFromConfig string) (string, error) {
	templateUUID := imageFromConfig
	if templateUUID == "" {
		templateUUID = DefaultTemplate
	}

	createReq := &request.CreateServerRequest{
		Zone:             zone,
		Title:            name,
		Hostname:         name,
		Plan:             plan,
		PasswordDelivery: request.PasswordDeliveryNone,
		Metadata:         upcloud.True,
		UserData:         startupScript,
		StorageDevices: []request.CreateServerStorageDevice{
			{
				Action:  request.CreateServerStorageDeviceActionClone,
				Storage: templateUUID,
				Title:   name + "-disk0",
				Size:    25,
				Tier:    storageTierForPlan(plan),
			},
		},
		Networking: &request.CreateServerNetworking{
			Interfaces: []request.CreateServerInterface{
				{
					IPAddresses: []request.CreateServerIPAddress{
						{Family: upcloud.IPAddressFamilyIPv4},
					},
					Type: upcloud.NetworkTypeUtility,
				},
				{
					IPAddresses: []request.CreateServerIPAddress{
						{Family: upcloud.IPAddressFamilyIPv4},
					},
					Type: upcloud.NetworkTypePublic,
				},
				{
					IPAddresses: []request.CreateServerIPAddress{
						{Family: upcloud.IPAddressFamilyIPv6},
					},
					Type: upcloud.NetworkTypePublic,
				},
			},
		},
	}

	serverDetails, err := p.svc.CreateServer(ctx, createReq)
	if err != nil {
		return "", fmt.Errorf("failed to create server: %w", err)
	}

	instanceID := fmt.Sprintf("%s%s/%s", InstanceIDPrefix, zone, serverDetails.UUID)
	return instanceID, nil
}

// DeleteInstance deletes an UpCloud server. instanceID must be in the form "upcloud/<zone>/<server-uuid>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

// deleteInstanceOnce makes a single attempt to delete a server.
// UpCloud requires the server to be stopped before deletion; we stop it if delete fails with SERVER_STATE_ILLEGAL.
func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, serverUUID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	err = p.svc.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{
		UUID: serverUUID,
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
			return nil
		}
		// UpCloud rejects delete while server is started or maintenance; stop first
		if strings.Contains(err.Error(), "SERVER_STATE_ILLEGAL") || strings.Contains(err.Error(), "state 'started'") || strings.Contains(err.Error(), "state 'maintenance'") {
			if stopErr := p.stopServerAndWait(ctx, serverUUID); stopErr != nil {
				return fmt.Errorf("failed to stop server before delete: %w", stopErr)
			}
			err = p.svc.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: serverUUID})
			if err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
					return nil
				}
				return fmt.Errorf("failed to delete server after stop: %w", err)
			}
			return nil
		}
		return fmt.Errorf("failed to delete server: %w", err)
	}

	return nil
}

// stopServerAndWait stops the server and waits for it to reach stopped state (up to 2 minutes).
func (p *Provisioner) stopServerAndWait(ctx context.Context, serverUUID string) error {
	_, err := p.svc.StopServer(ctx, &request.StopServerRequest{
		UUID:     serverUUID,
		StopType: "hard",
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
			return nil
		}
		return err
	}

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		details, err := p.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: serverUUID})
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
				return nil
			}
			return err
		}
		if details.State == upcloud.ServerStateStopped {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("server did not stop in time")
}

// IsInstanceRunning returns true if the server is in started state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, serverUUID, err := parseInstanceID(instanceID)
	if err != nil {
		return false, err
	}

	details, err := p.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: serverUUID})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("failed to get server details: %w", err)
	}
	if details == nil {
		return false, nil
	}

	if details.State == upcloud.ServerStateError {
		return false, &cberrors.InstanceFailedError{Reason: "server state: error"}
	}
	return details.State == upcloud.ServerStateStarted, nil
}

// Close releases resources. UpCloud client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// storageTierForPlan returns the storage tier for the given plan.
// DEV plans (e.g. DEV-1xCPU-1GB) do not support maxiops; use HDD for them.
func storageTierForPlan(plan string) string {
	if strings.HasPrefix(strings.ToUpper(plan), "DEV-") {
		return string(upcloud.StorageTierHDD)
	}
	return string(upcloud.StorageTierMaxIOPS)
}

func parseInstanceID(instanceID string) (string, string, error) {
	return provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
}
