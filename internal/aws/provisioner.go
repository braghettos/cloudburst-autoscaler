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

package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	// InstanceIDPrefix is the prefix for AWS instance IDs (region/instance-id)
	InstanceIDPrefix = "aws/"
	// DefaultAMI is a placeholder; callers should set AMI from NodeClass
	DefaultAMI = ""
	// MaxRetries is the maximum number of retry attempts
	MaxRetries = 3
	// RetryDelay is the initial delay between retries
	RetryDelay = 2 * time.Second
)

// Provisioner handles AWS EC2 instance provisioning
type Provisioner struct {
	ec2Client          *ec2.Client
	region             string
	subnetID           string
	securityIDs        []string
	instanceProfileARN string
}

// NewProvisioner creates a new AWS EC2 provisioner.
// accessKeyID and secretAccessKey can be empty to use default credential chain (env, IAM role).
func NewProvisioner(ctx context.Context, region, accessKeyID, secretAccessKey string, subnetID string, securityGroupIDs []string, instanceProfileARN string) (*Provisioner, error) {
	if region == "" {
		return nil, fmt.Errorf("AWS region is required")
	}

	var cfg aws.Config
	var err error
	if accessKeyID != "" && secretAccessKey != "" {
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
		)
	} else {
		cfg, err = config.LoadDefaultConfig(ctx, config.WithRegion(region))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := ec2.NewFromConfig(cfg)
	return &Provisioner{
		ec2Client:          client,
		region:             region,
		subnetID:           subnetID,
		securityIDs:        securityGroupIDs,
		instanceProfileARN: instanceProfileARN,
	}, nil
}

// CreateInstance creates an EC2 instance with the given name, instance type, and cloud-init script.
// regionParam is the region (must match provisioner region), imageFromConfig is the AMI ID.
// Returns instanceID in the form "aws/<region>/<instance-id>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, instanceType, regionParam, startupScript, imageFromConfig string) (string, error) {
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

		instanceID, err := p.createInstanceOnce(ctx, name, instanceType, regionParam, startupScript, imageFromConfig)
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

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, instanceType, regionParam, startupScript, imageFromConfig string) (string, error) {
	amiID := imageFromConfig
	if amiID == "" {
		return "", fmt.Errorf("AMI ID is required for AWS (set in NodeClass spec.aws.ami)")
	}

	runInput := &ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: types.InstanceType(instanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String(name)},
				},
			},
		},
	}

	if startupScript != "" {
		runInput.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(startupScript)))
	}
	if p.subnetID != "" {
		runInput.SubnetId = aws.String(p.subnetID)
	}
	if len(p.securityIDs) > 0 {
		runInput.SecurityGroupIds = p.securityIDs
	}
	if p.instanceProfileARN != "" {
		runInput.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Arn: aws.String(p.instanceProfileARN),
		}
	}

	out, err := p.ec2Client.RunInstances(ctx, runInput)
	if err != nil {
		// If the configured AMI is not in this region (e.g. CloudBroker chose a different region), resolve Ubuntu 22.04 in this region and retry once.
		if isInvalidAMIIDNotFound(err) {
			resolvedAMI, resolveErr := p.resolveUbuntu2204AMI(ctx)
			if resolveErr == nil && resolvedAMI != "" {
				runInput.ImageId = aws.String(resolvedAMI)
				out, err = p.ec2Client.RunInstances(ctx, runInput)
			}
		}
		if err != nil {
			return "", fmt.Errorf("failed to run instances: %w", err)
		}
	}
	if len(out.Instances) == 0 {
		return "", fmt.Errorf("RunInstances returned no instances")
	}
	instanceID := aws.ToString(out.Instances[0].InstanceId)
	return fmt.Sprintf("%s%s/%s", InstanceIDPrefix, p.region, instanceID), nil
}

// DeleteInstance deletes an EC2 instance. instanceID must be in the form "aws/<region>/<instance-id>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}

	_, err = p.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}
	return nil
}

// IsInstanceRunning returns true if the instance is in running state.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, id, err := parseInstanceID(instanceID)
	if err != nil {
		return false, err
	}

	out, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		return false, fmt.Errorf("failed to describe instance: %w", err)
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return false, fmt.Errorf("instance %s not found", id)
	}
	state := out.Reservations[0].Instances[0].State.Name
	return state == types.InstanceStateNameRunning, nil
}

// Close releases resources. AWS SDK client has no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the EC2 instance name (used as Name tag) for a NodeClaim.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

// parseInstanceID parses "aws/<region>/<instance-id>" into region and instance-id.
func parseInstanceID(instanceID string) (string, string, error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", "", fmt.Errorf("invalid AWS instance ID format: %s", instanceID)
	}
	rest := strings.TrimPrefix(instanceID, InstanceIDPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid AWS instance ID format: %s", instanceID)
	}
	return parts[0], parts[1], nil
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// VcpuLimitExceeded is not retryable with the same instance type; the controller
	// will try alternatives (smaller instance types) from NodeClaim.Spec.Alternatives.
	retryable := []string{"rate limit", "throttl", "temporary", "timeout", "deadline exceeded", "unavailable", "503", "502", "500", "RequestLimitExceeded"}
	for _, r := range retryable {
		if strings.Contains(strings.ToLower(errStr), r) {
			return true
		}
	}
	return false
}

// isInvalidAMIIDNotFound returns true if the error is EC2 InvalidAMIID.NotFound (e.g. AMI from another region).
func isInvalidAMIIDNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "InvalidAMIID.NotFound")
}

// resolveUbuntu2204AMI returns the latest Ubuntu 22.04 (Jammy) AMI ID in the provisioner's region, or empty string on error.
func (p *Provisioner) resolveUbuntu2204AMI(ctx context.Context) (string, error) {
	desc, err := p.ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"099720109477"},
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{"ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(desc.Images) == 0 {
		return "", fmt.Errorf("no Ubuntu 22.04 AMI found in %s", p.region)
	}
	// Sort by creation date and pick latest (simplified: first by name/date if available).
	latest := desc.Images[0]
	for i := 1; i < len(desc.Images); i++ {
		if desc.Images[i].CreationDate != nil && (latest.CreationDate == nil || *desc.Images[i].CreationDate > *latest.CreationDate) {
			latest = desc.Images[i]
		}
	}
	if latest.ImageId == nil {
		return "", fmt.Errorf("no Ubuntu 22.04 AMI id in %s", p.region)
	}
	return *latest.ImageId, nil
}
