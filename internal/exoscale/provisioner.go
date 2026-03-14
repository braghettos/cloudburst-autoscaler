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

package exoscale

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	cberrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provutil"
	exo "github.com/exoscale/egoscale/v3"
	"github.com/exoscale/egoscale/v3/credentials"
)

const (
	// InstanceIDPrefix is the prefix for Exoscale instance IDs (zone/instance-uuid)
	InstanceIDPrefix = "exoscale/"
	// DefaultTemplate is the default Ubuntu 22.04 template name for Exoscale
	DefaultTemplate = "Linux Ubuntu 22.04 LTS 64-bit"
	// DefaultDiskSizeGB is the default disk size in GiB
	DefaultDiskSizeGB = 10
	// OperationPollInterval is the interval for polling operation status
	OperationPollInterval = 2 * time.Second
	// OperationPollTimeout is the max time to wait for create operation
	OperationPollTimeout = 5 * time.Minute
)

// Provisioner handles Exoscale compute instance provisioning
type Provisioner struct {
	client     *exo.Client
	zone       string
	DiskSizeGB int64
}

// NewProvisioner creates a new Exoscale provisioner.
// apiKey and apiSecret are Exoscale API credentials.
// zone is the Exoscale zone slug (e.g. "ch-gva-2", "at-vie-1").
func NewProvisioner(ctx context.Context, apiKey, apiSecret, zone string, diskSizeGB int64) (*Provisioner, error) {
	if apiKey == "" || apiSecret == "" {
		return nil, fmt.Errorf("Exoscale API key and secret are required")
	}
	if zone == "" {
		return nil, fmt.Errorf("Exoscale zone is required")
	}

	creds := credentials.NewStaticCredentials(apiKey, apiSecret)
	client, err := exo.NewClient(creds, exo.ClientOptWithEndpoint(exo.Endpoint(zoneEndpoint(zone))))
	if err != nil {
		return nil, fmt.Errorf("failed to create Exoscale client: %w", err)
	}

	return &Provisioner{
		client:     client,
		zone:       zone,
		DiskSizeGB: diskSizeGB,
	}, nil
}

// zoneEndpoint returns the API endpoint URL for the given zone slug (e.g. ch-gva-2 -> https://api-ch-gva-2.exoscale.com/v2)
func zoneEndpoint(zone string) string {
	return "https://api-" + zone + ".exoscale.com/v2"
}

// CreateInstance creates an Exoscale compute instance with the given name, instance type, and cloud-init script.
// Returns instanceID in the form "exoscale/<zone>/<instance-uuid>".
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

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, instanceTypeSlug, zone, startupScript, imageFromConfig string) (string, error) {
	// Resolve instance type (CloudBroker uses "standard-medium", "cpu-huge" - format family-size)
	instanceTypesResp, err := p.client.ListInstanceTypes(ctx)
	if err != nil {
		return "", fmt.Errorf("list instance types: %w", err)
	}
	// Try family-size (CloudBroker) or family.size (Exoscale API format)
	instanceTypeSlugNorm := strings.ReplaceAll(instanceTypeSlug, "-", ".")
	instanceType, err := instanceTypesResp.FindInstanceTypeByIdOrFamilyAndSize(instanceTypeSlugNorm)
	if err != nil {
		instanceType, err = instanceTypesResp.FindInstanceTypeByIdOrFamilyAndSize(instanceTypeSlug)
		if err != nil {
			return "", fmt.Errorf("instance type %q not found: %w", instanceTypeSlug, err)
		}
	}

	// Resolve template (Ubuntu 22.04)
	templateName := imageFromConfig
	if templateName == "" {
		templateName = DefaultTemplate
	}
	templatesResp, err := p.client.ListTemplates(ctx, exo.ListTemplatesWithVisibility(exo.ListTemplatesVisibilityPublic))
	if err != nil {
		return "", fmt.Errorf("list templates: %w", err)
	}
	template, err := templatesResp.FindTemplate(templateName)
	if err != nil {
		return "", fmt.Errorf("template %q not found: %w", templateName, err)
	}

	diskSize := int64(DefaultDiskSizeGB)
	if p.DiskSizeGB > 0 {
		diskSize = p.DiskSizeGB
	}
	userData := base64.StdEncoding.EncodeToString([]byte(startupScript))

	req := exo.CreateInstanceRequest{
		Name:         name,
		DiskSize:     diskSize,
		InstanceType: &exo.InstanceType{ID: instanceType.ID},
		Template:     &exo.Template{ID: template.ID},
		UserData:     userData,
	}

	op, err := p.client.CreateInstance(ctx, req)
	if err != nil {
		return "", fmt.Errorf("create instance: %w", err)
	}

	// Poll for operation completion
	instanceUUID, err := p.waitForCreateOperation(ctx, op)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%s/%s", InstanceIDPrefix, zone, string(instanceUUID)), nil
}

func (p *Provisioner) waitForCreateOperation(ctx context.Context, op *exo.Operation) (exo.UUID, error) {
	deadline := time.Now().Add(OperationPollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("context cancelled: %w", ctx.Err())
		default:
		}

		op, err := p.client.GetOperation(ctx, op.ID)
		if err != nil {
			return "", fmt.Errorf("get operation: %w", err)
		}

		switch op.State {
		case exo.OperationStateSuccess:
			if op.Reference != nil && op.Reference.ID != "" {
				return op.Reference.ID, nil
			}
			return "", fmt.Errorf("operation succeeded but no instance ID in reference")
		case exo.OperationStateFailure, exo.OperationStateTimeout:
			return "", fmt.Errorf("operation %s: %s", op.State, op.Message)
		}

		time.Sleep(OperationPollInterval)
	}

	return "", fmt.Errorf("operation timed out after %v", OperationPollTimeout)
}

// DeleteInstance deletes an Exoscale instance. instanceID must be in the form "exoscale/<zone>/<instance-uuid>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	return provutil.WithRetry(ctx, provutil.MaxRetries, provutil.RetryDelay, provutil.IsRetryableError, func() error {
		return p.deleteInstanceOnce(ctx, instanceID)
	})
}

func (p *Provisioner) deleteInstanceOnce(ctx context.Context, instanceID string) error {
	_, instanceUUID, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	_, err = p.client.DeleteInstance(ctx, instanceUUID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
			return nil
		}
		return fmt.Errorf("delete instance: %w", err)
	}

	return nil
}

// IsInstanceRunning returns true if the instance is in running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, instanceUUID, err := parseInstanceID(instanceID)
	if err != nil {
		return false, err
	}

	instance, err := p.client.GetInstance(ctx, instanceUUID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("get instance: %w", err)
	}
	if instance == nil {
		return false, nil
	}

	switch instance.State {
	case exo.InstanceStateDestroyed, exo.InstanceStateError:
		return false, &cberrors.InstanceFailedError{Reason: fmt.Sprintf("instance state: %s", instance.State)}
	}
	return instance.State == exo.InstanceStateRunning, nil
}

// Close releases resources. Exoscale client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

func parseInstanceID(instanceID string) (string, exo.UUID, error) {
	zone, id, err := provutil.ParseInstanceID(instanceID, InstanceIDPrefix)
	if err != nil {
		return "", "", err
	}
	return zone, exo.UUID(id), nil
}
