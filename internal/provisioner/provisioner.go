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

package provisioner

import "context"

// Provisioner is the common interface all cloud providers must implement.
type Provisioner interface {
	// CreateInstance provisions a new VM and returns a provider-prefixed instance ID.
	// regionOrZone is provider-specific (e.g. zone for GCP, region for AWS).
	// image is optional; providers may ignore it and use a default.
	CreateInstance(ctx context.Context, name, instanceType, regionOrZone, startupScript, image string) (string, error)

	// DeleteInstance destroys a VM by its provider-prefixed instance ID.
	DeleteInstance(ctx context.Context, instanceID string) error

	// IsInstanceRunning checks whether the VM is in a running state.
	IsInstanceRunning(ctx context.Context, instanceID string) (bool, error)

	// Close releases any resources held by the provisioner (e.g. gRPC connections).
	// Implementations that don't need cleanup should return nil.
	Close() error
}
