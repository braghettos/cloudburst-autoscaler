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
	"time"

	"cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
)

const (
	// DefaultImageFamily is the default image family
	DefaultImageFamily = "ubuntu-2204-lts"
	// DefaultImageProject is the default image project
	DefaultImageProject = "ubuntu-os-cloud"
	// MaxRetries is the maximum number of retry attempts
	MaxRetries = 3
	// RetryDelay is the initial delay between retries
	RetryDelay = 2 * time.Second
)

// Provisioner handles GCP instance provisioning
type Provisioner struct {
	project string
	zone    string
	client  *compute.InstancesClient
}

// NewProvisioner creates a new GCP provisioner
func NewProvisioner(ctx context.Context, project, zone string) (*Provisioner, error) {
	client, err := compute.NewInstancesRESTClient(ctx, option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	if err != nil {
		return nil, fmt.Errorf("failed to create compute client: %w", err)
	}

	return &Provisioner{
		project: project,
		zone:    zone,
		client:  client,
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

// CreateInstance creates a GCP compute instance
func (p *Provisioner) CreateInstance(ctx context.Context, name, machineType, zone, startupScript string) (string, error) {
	var lastErr error

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			waitTime := RetryDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("context cancelled: %w", ctx.Err())
			case <-time.After(waitTime):
			}
		}

		instanceID, err := p.createInstanceOnce(ctx, name, machineType, zone, startupScript)
		if err == nil {
			return instanceID, nil
		}

		// Check if error is retryable
		if !isRetryableError(err) {
			return "", err
		}

		lastErr = err
	}

	return "", fmt.Errorf("failed after %d attempts: %w", MaxRetries+1, lastErr)
}

// createInstanceOnce makes a single attempt to create an instance
func (p *Provisioner) createInstanceOnce(ctx context.Context, name, machineType, zone, startupScript string) (string, error) {
	// Get image URL
	imageURL, err := p.getImageURL(ctx, DefaultImageFamily, DefaultImageProject)
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
	var lastErr error

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
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

		// Check if error is retryable
		if !isRetryableError(err) {
			return err
		}

		lastErr = err
	}

	return fmt.Errorf("failed after %d attempts: %w", MaxRetries+1, lastErr)
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
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	// Wait for operation to complete
	if err := op.Wait(ctx); err != nil {
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

// isRetryableError checks if an error is retryable
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	// Check for common retryable errors
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

// Close closes the provisioner and releases resources
func (p *Provisioner) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}
