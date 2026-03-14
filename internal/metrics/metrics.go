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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// NodeClaimPhaseTransitionsTotal counts phase transitions by provider and phase pair.
	NodeClaimPhaseTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cloudburst_nodeclaim_phase_transitions_total",
			Help: "Total number of NodeClaim phase transitions",
		},
		[]string{"provider", "from_phase", "to_phase"},
	)

	// ScaleEventsTotal counts scale-up and scale-down events by provider.
	ScaleEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cloudburst_scale_events_total",
			Help: "Total number of scale events (scale-up or scale-down)",
		},
		[]string{"direction", "provider"},
	)

	// ProviderErrorsTotal counts provider API errors by provider and operation.
	ProviderErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cloudburst_provider_errors_total",
			Help: "Total number of provider API errors (create, delete, status)",
		},
		[]string{"provider", "operation"},
	)

	// ProvisioningDurationSeconds is the time from Pending to instance running (transition to Joining).
	ProvisioningDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cloudburst_provisioning_duration_seconds",
			Help:    "Time from NodeClaim Pending to instance running (seconds)",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
		[]string{"provider"},
	)

	// JoiningDurationSeconds is the time from Joining to Ready.
	JoiningDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cloudburst_joining_duration_seconds",
			Help:    "Time from Joining phase to Ready (seconds)",
			Buckets: prometheus.ExponentialBuckets(30, 2, 8),
		},
		[]string{"provider"},
	)

	// DrainDurationSeconds is the time to drain a node.
	DrainDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "cloudburst_drain_duration_seconds",
			Help:    "Time to drain a node (seconds)",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		},
	)

	// CloudBrokerRequestDurationSeconds is the CloudBroker API request latency.
	CloudBrokerRequestDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "cloudburst_cloudbroker_request_duration_seconds",
			Help:    "CloudBroker recommendation API request duration (seconds)",
			Buckets: prometheus.DefBuckets,
		},
	)

	// NodeClaimsActive is the current number of NodeClaims by provider and phase.
	NodeClaimsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cloudburst_nodeclaims_active",
			Help: "Current number of NodeClaims by provider and phase",
		},
		[]string{"provider", "phase"},
	)

	// UnschedulablePods is the current number of unschedulable pods by namespace and nodepool.
	UnschedulablePods = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cloudburst_unschedulable_pods",
			Help: "Current number of unschedulable pods (demand signal)",
		},
		[]string{"namespace", "nodepool"},
	)

	// ReconciliationDurationSeconds is the per-controller reconcile loop duration.
	ReconciliationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cloudburst_reconciliation_duration_seconds",
			Help:    "Duration of controller reconciliation loops (seconds)",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller"},
	)

	// TailscaleRequestDurationSeconds is the Tailscale API request latency.
	TailscaleRequestDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "cloudburst_tailscale_request_duration_seconds",
			Help:    "Tailscale API request duration (seconds)",
			Buckets: prometheus.DefBuckets,
		},
	)
)

func init() {
	metrics.Registry.MustRegister(
		NodeClaimPhaseTransitionsTotal,
		ScaleEventsTotal,
		ProviderErrorsTotal,
		ProvisioningDurationSeconds,
		JoiningDurationSeconds,
		DrainDurationSeconds,
		CloudBrokerRequestDurationSeconds,
		NodeClaimsActive,
		UnschedulablePods,
		ReconciliationDurationSeconds,
		TailscaleRequestDurationSeconds,
	)
}
