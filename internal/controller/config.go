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

package controller

import (
	"os"
	"time"
)

// ControllerConfig holds controller timeouts and intervals loaded from environment.
type ControllerConfig struct {
	ProvisioningTimeout        time.Duration
	JoiningTimeout             time.Duration
	RequeueInterval            time.Duration
	DrainTimeout               time.Duration
	DrainGracePeriod           time.Duration
	DrainPollInterval          time.Duration
	NodeUnreachableGracePeriod time.Duration
}

const (
	defaultProvisioningTimeout        = 10 * time.Minute
	defaultJoiningTimeout             = 20 * time.Minute
	defaultRequeueInterval            = 30 * time.Second
	defaultDrainTimeout               = 5 * time.Minute
	defaultDrainGracePeriod           = 30 * time.Second
	defaultDrainPollInterval          = 2 * time.Second
	defaultNodeUnreachableGracePeriod = 2 * time.Minute
)

// LoadConfig reads controller configuration from environment variables with fallback to defaults.
func LoadConfig() *ControllerConfig {
	return &ControllerConfig{
		ProvisioningTimeout:        durationFromEnv("CLOUDBURST_PROVISIONING_TIMEOUT", defaultProvisioningTimeout),
		JoiningTimeout:             durationFromEnv("CLOUDBURST_JOINING_TIMEOUT", defaultJoiningTimeout),
		RequeueInterval:            durationFromEnv("CLOUDBURST_REQUEUE_INTERVAL", defaultRequeueInterval),
		DrainTimeout:               durationFromEnv("CLOUDBURST_DRAIN_TIMEOUT", defaultDrainTimeout),
		DrainGracePeriod:           durationFromEnv("CLOUDBURST_DRAIN_GRACE_PERIOD", defaultDrainGracePeriod),
		DrainPollInterval:          durationFromEnv("CLOUDBURST_DRAIN_POLL_INTERVAL", defaultDrainPollInterval),
		NodeUnreachableGracePeriod: durationFromEnv("CLOUDBURST_NODE_UNREACHABLE_GRACE_PERIOD", defaultNodeUnreachableGracePeriod),
	}
}

func durationFromEnv(key string, defaultVal time.Duration) time.Duration {
	s := os.Getenv(key)
	if s == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultVal
	}
	return d
}
