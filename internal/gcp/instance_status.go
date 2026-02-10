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

package gcp

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
)

// InstanceStatus represents the status of a GCP instance
type InstanceStatus string

const (
	// InstanceStatusRunning indicates the instance is running
	InstanceStatusRunning InstanceStatus = "RUNNING"
	// InstanceStatusProvisioning indicates the instance is provisioning
	InstanceStatusProvisioning InstanceStatus = "PROVISIONING"
	// InstanceStatusStaging indicates the instance is staging
	InstanceStatusStaging InstanceStatus = "STAGING"
	// InstanceStatusStopped indicates the instance is stopped
	InstanceStatusStopped InstanceStatus = "STOPPED"
	// InstanceStatusStopping indicates the instance is stopping
	InstanceStatusStopping InstanceStatus = "STOPPING"
	// InstanceStatusSuspending indicates the instance is suspending
	InstanceStatusSuspending InstanceStatus = "SUSPENDING"
	// InstanceStatusSuspended indicates the instance is suspended
	InstanceStatusSuspended InstanceStatus = "SUSPENDED"
	// InstanceStatusTerminated indicates the instance is terminated
	InstanceStatusTerminated InstanceStatus = "TERMINATED"
)

// GetInstanceStatus gets the status of a GCP instance
func (p *Provisioner) GetInstanceStatus(ctx context.Context, instanceID string) (InstanceStatus, error) {
	// Parse instance ID to get project, zone, and name
	parts := strings.Split(instanceID, "/")
	if len(parts) < 6 || parts[0] != "projects" || parts[2] != "zones" || parts[4] != "instances" {
		return "", fmt.Errorf("invalid instance ID format: %s", instanceID)
	}

	project := parts[1]
	zone := parts[3]
	name := parts[5]

	req := &computepb.GetInstanceRequest{
		Project:  project,
		Zone:     zone,
		Instance: name,
	}

	instance, err := p.client.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to get instance: %w", err)
	}

	if instance.Status == nil {
		return "", fmt.Errorf("instance status is nil")
	}

	return InstanceStatus(*instance.Status), nil
}

// IsInstanceRunning checks if the instance is running
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	status, err := p.GetInstanceStatus(ctx, instanceID)
	if err != nil {
		return false, err
	}
	return status == InstanceStatusRunning, nil
}
