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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	// DefaultImageFamily is the default image family
	DefaultImageFamily = "ubuntu-2204-lts"
	// DefaultImageProject is the default image project
	DefaultImageProject = "ubuntu-os-cloud"
)

// Provisioner handles GCP instance provisioning
type Provisioner struct {
	project      string
	zone         string
	imageFamily  string
	imageProject string
	client       *compute.InstancesClient
}

// NewProvisioner creates a new GCP provisioner.
// imageFamily and imageProject are optional; when empty they default to
// DefaultImageFamily ("ubuntu-2204-lts") and DefaultImageProject ("ubuntu-os-cloud").
func NewProvisioner(ctx context.Context, project, zone, imageFamily, imageProject string) (*Provisioner, error) {
	client, err := compute.NewInstancesRESTClient(ctx, option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	if err != nil {
		return nil, fmt.Errorf("failed to create compute client: %w", err)
	}

	if imageFamily == "" {
		imageFamily = DefaultImageFamily
	}
	if imageProject == "" {
		imageProject = DefaultImageProject
	}

	return &Provisioner{
		project:      project,
		zone:         zone,
		imageFamily:  imageFamily,
		imageProject: imageProject,
		client:       client,
	}, nil
}

// GetProject returns the project value from NodeClass.
// The fallback parameter is the value from NodeClass.Spec.GCP.Project.
func GetProject(fallback string) string {
	return fallback
}

// GetZone returns the zone value from NodeClass.
// The fallback parameter is the value from NodeClass.Spec.GCP.Zone.
func GetZone(fallback string) string {
	return fallback
}

// CreateInstance creates a GCP compute instance.
// The image parameter is unused for GCP; imageFamily and imageProject are set on the Provisioner.
func (p *Provisioner) CreateInstance(ctx context.Context, name, machineType, zone, startupScript, image string) (string, error) {
	var instanceID string
	err := provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		var retryErr error
		instanceID, retryErr = p.createInstanceOnce(ctx, name, machineType, zone, startupScript)
		return retryErr
	})
	if err != nil {
		return "", err
	}
	return instanceID, nil
}

// createInstanceOnce makes a single attempt to create an instance
func (p *Provisioner) createInstanceOnce(ctx context.Context, name, machineType, zone, startupScript string) (string, error) {
	// Get image URL from provisioner config (set from NodeClass or defaults)
	imageURL, err := p.getImageURL(ctx, p.imageFamily, p.imageProject)
	if err != nil {
		return "", fmt.Errorf("failed to get image URL: %w", err)
	}

	// Build machine type URL
	machineTypeURL := fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType)

	// Build instance
	instance := &computepb.Instance{
		Name:        &name,
		MachineType: &machineTypeURL,
		Disks: []*computepb.AttachedDisk{
			{
				Boot:       &[]bool{true}[0],
				AutoDelete: &[]bool{true}[0],
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					SourceImage: &imageURL,
				},
			},
		},
		NetworkInterfaces: []*computepb.NetworkInterface{
			{
				AccessConfigs: []*computepb.AccessConfig{
					{
						Name:        &[]string{"External NAT"}[0],
						Type:        &[]string{"ONE_TO_ONE_NAT"}[0],
						NetworkTier: &[]string{"PREMIUM"}[0],
					},
				},
			},
		},
		ServiceAccounts: []*computepb.ServiceAccount{
			{
				Email: &[]string{"default"}[0],
				Scopes: []string{
					"https://www.googleapis.com/auth/cloud-platform",
				},
			},
		},
	}

	if startupScript != "" {
		startupScriptKey := "startup-script"
		instance.Metadata = &computepb.Metadata{
			Items: []*computepb.Items{
				{
					Key:   &startupScriptKey,
					Value: &startupScript,
				},
			},
		}
	}

	// Build request
	req := &computepb.InsertInstanceRequest{
		Project:          p.project,
		Zone:             zone,
		InstanceResource: instance,
	}

	// Create instance
	op, err := p.client.Insert(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to insert instance: %w", err)
	}

	// Wait for operation to complete
	if err := op.Wait(ctx); err != nil {
		return "", fmt.Errorf("failed to wait for instance creation: %w", err)
	}

	// Get instance ID (format: projects/PROJECT/zones/ZONE/instances/NAME)
	instanceID := fmt.Sprintf("projects/%s/zones/%s/instances/%s", p.project, zone, name)
	return instanceID, nil
}

// DeleteInstance deletes a GCP compute instance
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

// deleteInstanceOnce makes a single attempt to delete an instance
func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	// Parse instance ID to get project, zone, and name
	parts := strings.Split(instanceID, "/")
	if len(parts) < 6 || parts[0] != "projects" || parts[2] != "zones" || parts[4] != "instances" {
		return fmt.Errorf("invalid instance ID format: %s", instanceID)
	}

	project := parts[1]
	zone := parts[3]
	name := parts[5]

	req := &computepb.DeleteInstanceRequest{
		Project:  project,
		Zone:     zone,
		Instance: name,
	}

	op, err := p.client.Delete(ctx, req)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	if err := op.Wait(ctx); err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("failed to wait for instance deletion: %w", err)
	}

	return nil
}

// getImageURL gets the image URL for the given image family and project
func (p *Provisioner) getImageURL(ctx context.Context, imageFamily, imageProject string) (string, error) {
	client, err := compute.NewImagesRESTClient(ctx, option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	if err != nil {
		return "", fmt.Errorf("failed to create images client: %w", err)
	}
	defer client.Close()

	req := &computepb.GetFromFamilyImageRequest{
		Project: imageProject,
		Family:  imageFamily,
	}

	image, err := client.GetFromFamily(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to get image: %w", err)
	}

	return *image.SelfLink, nil
}

// GenerateInstanceName returns the GCP instance name for a NodeClaim.
// By convention, the GCP instance name equals the NodeClaim name so that
// NodeClaim, K8s Node, and GCP VM all share a single identifier (cb-<nodepool>-<suffix>).
// The NodeClaim name is already unique (Kubernetes GenerateName) and fits within
// GCP's 63-character instance-name limit.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

// IsInstanceRunning checks whether the VM is in a RUNNING state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	parts := strings.Split(instanceID, "/")
	if len(parts) < 6 || parts[0] != "projects" || parts[2] != "zones" || parts[4] != "instances" {
		return false, fmt.Errorf("invalid instance ID format: %s", instanceID)
	}

	instance, err := p.client.Get(ctx, &computepb.GetInstanceRequest{
		Project:  parts[1],
		Zone:     parts[3],
		Instance: parts[5],
	})
	if err != nil {
		return false, fmt.Errorf("failed to get instance: %w", err)
	}
	if instance.Status == nil {
		return false, nil
	}
	status := *instance.Status
	switch status {
	case "TERMINATED", "SUSPENDED":
		return false, &cberrors.InstanceFailedError{Reason: fmt.Sprintf("instance status: %s", status)}
	}
	return status == "RUNNING", nil
}

// Close closes the provisioner and releases resources
func (p *Provisioner) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}
