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
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
	"github.com/diegobraga/cloudburst-autoscaler/internal/cloudbroker"
	"github.com/diegobraga/cloudburst-autoscaler/internal/metrics"
	"github.com/diegobraga/cloudburst-autoscaler/internal/planner"
)

const (
	// DefaultMinNodes is the default minimum number of nodes when NodePool.Spec.Limits.MinNodes is unset
	DefaultMinNodes = 1
	// DefaultMinVCPU is the default minimum vCPU per node when NodePool.Spec.Requirements.MinVCPU is unset
	DefaultMinVCPU = 2
	// DefaultMinRAMGb is the default minimum RAM per node in GB when NodePool.Spec.Requirements.MinRAMGb is unset
	DefaultMinRAMGb = 8.0
)

// NodePoolReconciler reconciles a NodePool object.
//
// Concept of NodePool: a pool of nodes that share the same constraints (region, arch, price, providers),
// limits (min/max nodes), per-node capacity (MinVCPU, MinRAMGb), template (labels/taints), and disruption
// policy. The controller creates NodeClaims when unschedulable pod demand exceeds current capacity,
// using NodePool.Requirements to decide how many nodes are needed and what to ask CloudBroker for.
type NodePoolReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	CloudBroker *cloudbroker.Client
	Recorder    record.EventRecorder
}

//+kubebuilder:rbac:groups=cloudburst.io,resources=nodepools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodepools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodepools/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile reconciles NodePool and creates NodeClaims when needed
func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("nodepool", req.NamespacedName)

	// Fetch NodePool
	nodePool := &cloudburstv1alpha1.NodePool{}
	if err := r.Get(ctx, req.NamespacedName, nodePool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch NodePool")
		return ctrl.Result{}, err
	}

	// Calculate desired capacity from unschedulable pods
	desiredCapacity, err := r.calculateDesiredCapacity(ctx)
	if err != nil {
		logger.Error(err, "unable to calculate desired capacity")
		return ctrl.Result{}, err
	}

	// Size the next node to fit current unschedulable demand so we create one node that
	// accommodates the pod(s), rather than multiple small nodes from a fixed pool size.
	// We need 1 node when there is demand; that node will be requested with min vCPU/RAM
	// derived from desiredCapacity (NodePool.MinVCPU/MinRAMGb are the floor).
	nodesNeeded := int32(0)
	if desiredCapacity.CPUMilliTotal > 0 || desiredCapacity.MemBytesTotal > 0 {
		nodesNeeded = 1
	}

	logger.Info("calculated nodes needed",
		"desiredCapacity", desiredCapacity,
		"nodesNeeded", nodesNeeded,
	)

	// Count active NodeClaims for this NodePool
	activeCount, err := r.countActiveNodeClaims(ctx, req.Namespace, req.Name)
	if err != nil {
		logger.Error(err, "unable to count active NodeClaims")
		return ctrl.Result{}, err
	}

	logger.Info("node claim status",
		"activeCount", activeCount,
		"nodesNeeded", nodesNeeded,
	)

	// Check for scale-down opportunities
	if err := r.checkScaleDown(ctx, nodePool, logger); err != nil {
		logger.Error(err, "error checking scale-down")
		// Don't fail reconciliation on scale-down errors
	}

	// Create NodeClaims if needed. Create at most one per reconcile so that when the
	// controller is re-enqueued by Owns(NodeClaim) after Create(), the next reconcile
	// sees updated activeCount (or we create one more); avoids creating nodesNeeded
	// copies when the cache is still stale and activeCount appears 0.
	if nodesNeeded > activeCount {
		nodesToCreate := nodesNeeded - activeCount
		if nodesToCreate > 1 {
			nodesToCreate = 1
		}
		logger.Info("creating node claims",
			"nodesToCreate", nodesToCreate,
			"nodePool", req.Name,
		)

		// Get CloudBroker recommendation first to determine provider
		// This allows us to match NodeClass to the provider
		// Architecture is extracted from pod requirements (nodeSelector/nodeAffinity)
		// If no architecture requirement is found in pods, pass empty string to CloudBroker
		// (CloudBroker will handle the default or any-arch recommendation)
		minVCPU := vcpuFromDemand(desiredCapacity.CPUMilliTotal, getMinVCPUFromNodePool(nodePool))
		minRAMGb := ramGbFromDemand(desiredCapacity.MemBytesTotal, getMinRAMGbFromNodePool(nodePool))

		brokerReq := cloudbroker.RecommendationRequest{
			MinVCPU:            minVCPU,
			MinRAMGb:           minRAMGb,
			RegionConstraint:   nodePool.Spec.Requirements.RegionConstraint,
			MaxPriceEurPerHour: nodePool.Spec.Requirements.MaxPriceEurPerHour,
			Limit:              10,
		}
		// Set Arch: prefer pod's architecture requirement, fall back to first entry in NodePool.Spec.Requirements.Arch
		// CloudBroker will normalize Kubernetes values (amd64, x86_64, arm64, aarch64) to canonical values
		if desiredCapacity.Arch != "" {
			brokerReq.Arch = &desiredCapacity.Arch
		} else if len(nodePool.Spec.Requirements.Arch) > 0 {
			brokerReq.Arch = &nodePool.Spec.Requirements.Arch[0]
		}
		if len(nodePool.Spec.Requirements.AllowedProviders) > 0 {
			brokerReq.AllowedProviders = nodePool.Spec.Requirements.AllowedProviders
		}
		if nodePool.Spec.Requirements.TCOPlacement != nil {
			brokerReq.DataSourceProvider = nodePool.Spec.Requirements.TCOPlacement.DataSourceProvider
			brokerReq.DataSourceRegion = nodePool.Spec.Requirements.TCOPlacement.DataSourceRegion
			brokerReq.EstimatedEgressGbPerHour = nodePool.Spec.Requirements.TCOPlacement.EstimatedEgressGbPerHour
			if nodePool.Spec.Requirements.TCOPlacement.MaxTCOEurPerHour > 0 {
				brokerReq.MaxTCOEurPerHour = nodePool.Spec.Requirements.TCOPlacement.MaxTCOEurPerHour
			}
		}

		recommendations, err := r.CloudBroker.Recommend(ctx, &brokerReq)
		if err != nil {
			logger.Error(err, "failed to get recommendation from CloudBroker")
			r.recordEvent(nodePool, corev1.EventTypeWarning, "CloudBrokerError", fmt.Sprintf("Failed to get recommendation: %v", err))
			return ctrl.Result{}, err
		}
		if len(recommendations) == 0 {
			err := fmt.Errorf("CloudBroker returned no recommendations")
			logger.Error(err, "CloudBroker returned empty list")
			r.recordEvent(nodePool, corev1.EventTypeWarning, "CloudBrokerError", err.Error())
			return ctrl.Result{}, err
		}
		chosen := &recommendations[0]
		alternatives := recommendations[1:]

		// Log CloudBroker recommendations that will drive provisioning
		logger.Info("CloudBroker recommendations",
			"request", fmt.Sprintf("minVCPU=%d minRAMGb=%.1f regionConstraint=%q maxPrice=%.4f", brokerReq.MinVCPU, brokerReq.MinRAMGb, brokerReq.RegionConstraint, brokerReq.MaxPriceEurPerHour),
			"chosen", fmt.Sprintf("%s/%s/%s (vcpu=%d ramGb=%.1f price=%.4f EUR/h)", chosen.Provider, chosen.Region, chosen.InstanceType, chosen.VCPU, chosen.RAMGb, chosen.PriceEurPerHour),
			"alternativesCount", len(alternatives),
		)
		for i, alt := range alternatives {
			logger.V(1).Info("CloudBroker alternative",
				"rank", i+2,
				"provider", alt.Provider,
				"region", alt.Region,
				"instanceType", alt.InstanceType,
				"vcpu", alt.VCPU,
				"ramGb", alt.RAMGb,
				"priceEurPerHour", alt.PriceEurPerHour,
			)
		}
		if chosen.Provider != "gcp" && chosen.Provider != "scaleway" && chosen.Provider != "hetzner" && chosen.Provider != "digitalocean" && chosen.Provider != "ovh" && chosen.Provider != "aws" && chosen.Provider != "azure" {
			err := fmt.Errorf("recommendation provider is %q, only gcp, scaleway, hetzner, digitalocean, ovh, aws, and azure are supported", chosen.Provider)
			logger.Error(err, "unsupported provider")
			r.recordEvent(nodePool, corev1.EventTypeWarning, "UnsupportedProvider", err.Error())
			return ctrl.Result{}, err
		}

		// Get NodeClass matching the provider from recommendation
		nodeClassList := &cloudburstv1alpha1.NodeClassList{}
		if err := r.List(ctx, nodeClassList, client.InNamespace(req.Namespace)); err != nil {
			logger.Error(err, "unable to list NodeClasses")
			return ctrl.Result{}, err
		}

		if len(nodeClassList.Items) == 0 {
			logger.Info("no NodeClass found, skipping NodeClaim creation")
			r.recordEvent(nodePool, corev1.EventTypeWarning, "NoNodeClass", "No NodeClass found in namespace")
			return ctrl.Result{}, nil
		}

		// Find NodeClass that matches the provider
		var nodeClass *cloudburstv1alpha1.NodeClass
		for i := range nodeClassList.Items {
			nc := &nodeClassList.Items[i]
			if r.nodeClassSupportsProvider(nc, chosen.Provider) {
				nodeClass = nc
				logger.Info("matched NodeClass to provider",
					"nodeClass", nc.Name,
					"provider", chosen.Provider,
				)
				break
			}
		}

		if nodeClass == nil {
			err := fmt.Errorf("no NodeClass found that supports provider %q", chosen.Provider)
			logger.Error(err, "no matching NodeClass")
			r.recordEvent(nodePool, corev1.EventTypeWarning, "NoMatchingNodeClass", err.Error())
			return ctrl.Result{}, err
		}

		// Patch pods without provider/nodepool constraint so they land on the node we're about to provision.
		// This ensures workloads without nodeAffinity are bound to the chosen provider.
		if err := r.patchPodsWithProviderAffinity(ctx, chosen.Provider, logger); err != nil {
			logger.Error(err, "failed to patch pods with provider affinity")
			// Don't block NodeClaim creation; pods may still schedule correctly
		}

		// Create NodeClaims using the matched NodeClass (chosen + alternatives for fallback on create failure)
		for i := int32(0); i < nodesToCreate; i++ {
			if err := r.createNodeClaim(ctx, nodePool, nodeClass, desiredCapacity, &brokerReq, chosen, alternatives); err != nil {
				logger.Error(err, "unable to create NodeClaim", "index", i)
				r.recordEvent(nodePool, corev1.EventTypeWarning, "NodeClaimCreationFailed", fmt.Sprintf("Failed to create NodeClaim: %v", err))
				continue
			}
			metrics.ScaleEventsTotal.WithLabelValues("scale_up", chosen.Provider).Inc()
		}

		r.recordEvent(nodePool, corev1.EventTypeNormal, "NodeClaimsCreated", fmt.Sprintf("Created %d NodeClaim(s)", nodesToCreate))
		// Requeue so next reconciliation sees updated activeCount. Use a delay so the informer
		// cache is likely to include the new NodeClaim(s) before we consider creating more.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Always requeue periodically so we can:
	// 1. Detect newly-unschedulable pods that appear after initial reconciliation
	// 2. Run checkScaleDown to detect empty nodes once pods finish terminating
	// Use a shorter interval when there are active claims to speed up scale-down detection.
	if activeCount > 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// checkScaleDown checks for nodes that should be deleted based on disruption settings
func (r *NodePoolReconciler) checkScaleDown(ctx context.Context, nodePool *cloudburstv1alpha1.NodePool, logger logr.Logger) error {
	// List all NodeClaims for this NodePool
	nodeClaimList := &cloudburstv1alpha1.NodeClaimList{}
	if err := r.List(ctx, nodeClaimList, client.InNamespace(nodePool.Namespace)); err != nil {
		return fmt.Errorf("unable to list NodeClaims: %w", err)
	}

	// Filter Ready NodeClaims for this NodePool
	var readyNodeClaims []cloudburstv1alpha1.NodeClaim
	for i := range nodeClaimList.Items {
		claim := &nodeClaimList.Items[i]
		if claim.Spec.NodePoolRef != nodePool.Name {
			continue
		}
		if claim.Status.Phase == cloudburstv1alpha1.NodeClaimPhaseReady {
			readyNodeClaims = append(readyNodeClaims, *claim)
		}
	}

	// Safety guard: never delete if fewer than minNodes remain
	minNodes := DefaultMinNodes
	if nodePool.Spec.Limits.MinNodes != nil {
		minNodes = int(*nodePool.Spec.Limits.MinNodes)
		if minNodes < 0 {
			minNodes = 0
		}
	}
	if len(readyNodeClaims) <= minNodes {
		logger.Info("skipping scale-down: minimum node count reached",
			"readyCount", len(readyNodeClaims),
			"minNodes", minNodes,
		)
		return nil
	}

	// Check max spend guard (optional)
	if err := r.checkMaxSpend(ctx, nodePool, readyNodeClaims, logger); err != nil {
		logger.Info("skipping scale-down: max spend check failed", "error", err)
		// Continue with other checks even if max spend check fails
	}

	now := time.Now()

	// Check each Ready NodeClaim for disruption conditions
	for i := range readyNodeClaims {
		claim := &readyNodeClaims[i]

		// Fetch latest version to ensure we have the current state
		latestClaim := &cloudburstv1alpha1.NodeClaim{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(claim), latestClaim); err != nil {
			logger.Error(err, "unable to fetch latest NodeClaim", "nodeClaim", claim.Name)
			continue
		}

		// Check ttlSecondsUntilExpired
		if nodePool.Spec.Disruption.TTLSecondsUntilExpired > 0 {
			if latestClaim.Status.Timestamps.ReadyAt != nil {
				age := now.Sub(latestClaim.Status.Timestamps.ReadyAt.Time)
				if age >= time.Duration(nodePool.Spec.Disruption.TTLSecondsUntilExpired)*time.Second {
					logger.Info("node expired, initiating deletion",
						"nodeClaim", latestClaim.Name,
						"age", age,
						"ttl", nodePool.Spec.Disruption.TTLSecondsUntilExpired,
					)
					r.recordEvent(nodePool, corev1.EventTypeNormal, "NodeExpired", fmt.Sprintf("NodeClaim %s expired (age: %v)", latestClaim.Name, age))
					if err := r.initiateDeletion(ctx, latestClaim, logger); err != nil {
						logger.Error(err, "failed to initiate deletion", "nodeClaim", latestClaim.Name)
						continue
					}
					continue
				}
			}
		}

		// Check ttlSecondsAfterEmpty (optional, can be skipped)
		if nodePool.Spec.Disruption.TTLSecondsAfterEmpty > 0 {
			isEmpty, err := r.isNodeEmpty(ctx, latestClaim, logger)
			if err != nil {
				logger.Error(err, "unable to check if node is empty", "nodeClaim", latestClaim.Name)
				continue
			}

			if isEmpty {
				// Check when node became empty (we'd need to track this, but for MVP we can use ReadyAt)
				// For MVP, we'll check if node has been empty for the TTL duration
				// This is simplified - in production you'd track when node became empty
				if latestClaim.Status.Timestamps.ReadyAt != nil {
					age := now.Sub(latestClaim.Status.Timestamps.ReadyAt.Time)
					if age >= time.Duration(nodePool.Spec.Disruption.TTLSecondsAfterEmpty)*time.Second {
						logger.Info("node empty for TTL, initiating deletion",
							"nodeClaim", latestClaim.Name,
							"age", age,
							"ttl", nodePool.Spec.Disruption.TTLSecondsAfterEmpty,
						)
						r.recordEvent(nodePool, corev1.EventTypeNormal, "NodeEmpty", fmt.Sprintf("NodeClaim %s empty for TTL (age: %v)", latestClaim.Name, age))
						if err := r.initiateDeletion(ctx, latestClaim, logger); err != nil {
							logger.Error(err, "failed to initiate deletion", "nodeClaim", latestClaim.Name)
							continue
						}
					}
				}
			}
		}
	}

	return nil
}

// checkMaxSpend checks if total spend exceeds NodePool limits
func (r *NodePoolReconciler) checkMaxSpend(ctx context.Context, nodePool *cloudburstv1alpha1.NodePool, nodeClaims []cloudburstv1alpha1.NodeClaim, logger logr.Logger) error {
	// Calculate total spend
	var totalSpend float64
	for i := range nodeClaims {
		totalSpend += nodeClaims[i].Spec.Chosen.PriceEurPerHour
	}

	// Check against limits (if maxPriceEurPerHour is set per node, we'd need to check differently)
	// For MVP, we'll skip this check as it's optional
	logger.Info("max spend check",
		"totalSpend", totalSpend,
		"nodeCount", len(nodeClaims),
	)

	return nil
}

// isNodeEmpty checks if a node has no non-daemonset pods
func (r *NodePoolReconciler) isNodeEmpty(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (bool, error) {
	if nodeClaim.Status.NodeName == "" {
		return false, nil
	}

	// List all pods
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return false, fmt.Errorf("unable to list pods: %w", err)
	}

	// Check for pods on this node
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeClaim.Status.NodeName {
			continue
		}

		// Skip daemonset pods
		isDaemonSet := false
		if pod.OwnerReferences != nil {
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == "DaemonSet" {
					isDaemonSet = true
					break
				}
			}
		}
		if isDaemonSet {
			continue
		}

		// Found a non-daemonset pod
		return false, nil
	}

	return true, nil
}

// initiateDeletion initiates the deletion flow for a NodeClaim
func (r *NodePoolReconciler) initiateDeletion(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) error {
	// Fetch latest version
	latestClaim := &cloudburstv1alpha1.NodeClaim{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), latestClaim); err != nil {
		return fmt.Errorf("failed to get latest NodeClaim: %w", err)
	}

	// Update NodeClaim phase to Deleting
	metrics.ScaleEventsTotal.WithLabelValues("scale_down", latestClaim.Spec.Chosen.Provider).Inc()
	latestClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseDeleting
	latestClaim.Status.Timestamps.LastTransition = metav1.Now()
	if err := r.Status().Update(ctx, latestClaim); err != nil {
		return fmt.Errorf("failed to update NodeClaim phase: %w", err)
	}

	logger.Info("initiated deletion", "nodeClaim", latestClaim.Name)
	r.recordEvent(latestClaim, corev1.EventTypeNormal, "DeletionInitiated", "NodeClaim deletion initiated")

	return nil
}

// calculateDesiredCapacity calculates desired capacity from unschedulable pods
func (r *NodePoolReconciler) calculateDesiredCapacity(ctx context.Context) (planner.DesiredCapacity, error) {
	// List all pods
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return planner.DesiredCapacity{}, err
	}

	// Filter eligible pods (same logic as DemandDetector)
	eligiblePods := r.filterEligiblePods(podList.Items)

	// Calculate desired capacity
	return planner.CalculateDesiredCapacity(eligiblePods), nil
}

// patchPodsWithProviderAffinity patches eligible unschedulable pods that have no
// cloudburst.io/provider or cloudburst.io/nodepool constraint, adding nodeAffinity
// for the chosen provider so they land on the node being provisioned.
func (r *NodePoolReconciler) patchPodsWithProviderAffinity(ctx context.Context, provider string, logger logr.Logger) error {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return fmt.Errorf("unable to list pods: %w", err)
	}

	eligiblePods := r.filterEligiblePods(podList.Items)
	patched := 0
	for i := range eligiblePods {
		pod := &eligiblePods[i]
		if planner.PodHasProviderOrNodePoolConstraint(pod) {
			continue
		}

		// Fetch latest to avoid conflict
		latest := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(pod), latest); err != nil {
			logger.Error(err, "unable to fetch pod for patch", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		if planner.PodHasProviderOrNodePoolConstraint(latest) {
			continue
		}

		original := latest.DeepCopy()
		latest.Spec.Affinity = planner.AddProviderNodeAffinity(latest, provider)
		if err := r.Patch(ctx, latest, client.MergeFrom(original)); err != nil {
			logger.Error(err, "failed to patch pod with provider affinity", "pod", latest.Name, "namespace", latest.Namespace, "provider", provider)
			continue
		}
		logger.Info("patched pod with provider affinity", "pod", latest.Name, "namespace", latest.Namespace, "provider", provider)
		patched++
	}
	if patched > 0 {
		logger.Info("patched pods with provider affinity", "count", patched, "provider", provider)
	}
	return nil
}

// filterEligiblePods filters pods that are eligible for demand detection
func (r *NodePoolReconciler) filterEligiblePods(pods []corev1.Pod) []corev1.Pod {
	var eligible []corev1.Pod

	for i := range pods {
		pod := &pods[i]
		// Ignore kube-system namespace
		if pod.Namespace == KubeSystemNamespace {
			continue
		}

		// Ignore pods with cloudburst.io/ignore=true annotation
		if pod.Annotations != nil {
			if pod.Annotations[IgnoreAnnotation] == "true" {
				continue
			}
		}

		// Check if pod is Pending
		if pod.Status.Phase != corev1.PodPending {
			continue
		}

		// Check if PodScheduled condition is False with reason "Unschedulable"
		podScheduled := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled {
				if condition.Status == corev1.ConditionFalse && condition.Reason == "Unschedulable" {
					podScheduled = true
					break
				}
			}
		}

		if podScheduled {
			eligible = append(eligible, *pod)
		}
	}

	return eligible
}

// countActiveNodeClaims counts NodeClaims in active phases
func (r *NodePoolReconciler) countActiveNodeClaims(ctx context.Context, namespace, nodePoolName string) (int32, error) {
	nodeClaimList := &cloudburstv1alpha1.NodeClaimList{}
	if err := r.List(ctx, nodeClaimList, client.InNamespace(namespace)); err != nil {
		return 0, err
	}

	count := int32(0)
	for i := range nodeClaimList.Items {
		claim := &nodeClaimList.Items[i]
		// Check if this NodeClaim belongs to this NodePool
		if claim.Spec.NodePoolRef != nodePoolName {
			continue
		}

		// Count only active phases.
		// IMPORTANT: we also treat Deleting as "active" to avoid spinning up a
		// replacement node while the old one is still being drained/terminated,
		// which could otherwise cause multiple VMs to exist concurrently.
		phase := claim.Status.Phase
		if phase == cloudburstv1alpha1.NodeClaimPhasePending ||
			phase == cloudburstv1alpha1.NodeClaimPhaseProvisioning ||
			phase == cloudburstv1alpha1.NodeClaimPhaseJoining ||
			phase == cloudburstv1alpha1.NodeClaimPhaseReady ||
			phase == cloudburstv1alpha1.NodeClaimPhaseDeleting {
			count++
		}
	}

	return count, nil
}

// getMinVCPUFromNodePool returns the minimum vCPU per node for this pool (from spec or default).
func getMinVCPUFromNodePool(nodePool *cloudburstv1alpha1.NodePool) int32 {
	if nodePool.Spec.Requirements.MinVCPU != nil && *nodePool.Spec.Requirements.MinVCPU > 0 {
		return *nodePool.Spec.Requirements.MinVCPU
	}
	return DefaultMinVCPU
}

// getMinRAMGbFromNodePool returns the minimum RAM per node in GB for this pool (from spec or default).
func getMinRAMGbFromNodePool(nodePool *cloudburstv1alpha1.NodePool) float64 {
	if nodePool.Spec.Requirements.MinRAMGb != nil && *nodePool.Spec.Requirements.MinRAMGb > 0 {
		return *nodePool.Spec.Requirements.MinRAMGb
	}
	return DefaultMinRAMGb
}

// vcpuFromDemand returns the minimum vCPU to request for a node that fits the demand; poolMin is the NodePool floor.
func vcpuFromDemand(cpuMilli int64, poolMin int32) int32 {
	need := int32((cpuMilli + 999) / 1000)
	if need < 1 {
		need = 1
	}
	if poolMin > need {
		return poolMin
	}
	return need
}

// ramGbFromDemand returns the minimum RAM in GB to request for a node that fits the demand; poolMin is the NodePool floor.
func ramGbFromDemand(memBytes int64, poolMin float64) float64 {
	const bytesPerGb = 1024 * 1024 * 1024
	needGb := float64((memBytes + bytesPerGb - 1) / bytesPerGb)
	if needGb < 1 {
		needGb = 1
	}
	if poolMin > needGb {
		return poolMin
	}
	return needGb
}

// createNodeClaim creates a new NodeClaim with recommendation from CloudBroker
// nodeClassSupportsProvider checks if a NodeClass supports the given provider.
// Region/zone comes from CloudBroker recommendation; only non-region required fields are checked here.
func (r *NodePoolReconciler) nodeClassSupportsProvider(nodeClass *cloudburstv1alpha1.NodeClass, provider string) bool {
	switch provider {
	case "gcp":
		return nodeClass.Spec.GCP.Project != ""
	case "scaleway":
		return nodeClass.Spec.Scaleway != nil
	case "hetzner":
		return nodeClass.Spec.Hetzner != nil
	case "digitalocean":
		return nodeClass.Spec.DigitalOcean != nil
	case "ovh":
		return nodeClass.Spec.OVH != nil && nodeClass.Spec.OVH.ProjectID != ""
	case "aws":
		return nodeClass.Spec.AWS != nil && nodeClass.Spec.AWS.AMI != ""
	case "azure":
		return nodeClass.Spec.Azure != nil && nodeClass.Spec.Azure.SubscriptionID != "" && nodeClass.Spec.Azure.ResourceGroup != "" && nodeClass.Spec.Azure.SubnetID != ""
	default:
		return false
	}
}

func (r *NodePoolReconciler) createNodeClaim(ctx context.Context, nodePool *cloudburstv1alpha1.NodePool, nodeClass *cloudburstv1alpha1.NodeClass, desiredCapacity planner.DesiredCapacity, brokerReq *cloudbroker.RecommendationRequest, chosen *cloudbroker.RecommendationResult, alternatives []cloudbroker.RecommendationResult) error {
	logger := log.FromContext(ctx)

	altDetails := make([]cloudburstv1alpha1.InstanceDetails, 0, len(alternatives))
	for _, a := range alternatives {
		altDetails = append(altDetails, cloudburstv1alpha1.InstanceDetails{
			Provider:        a.Provider,
			Region:          a.Region,
			InstanceType:    a.InstanceType,
			VCPU:            a.VCPU,
			RamGb:           a.RAMGb,
			PriceEurPerHour: a.PriceEurPerHour,
		})
	}

	// Create NodeClaim
	// Naming standard: cb-<nodepool>-<k8s-suffix>
	// The NodeClaim name becomes the K8s node name AND the GCP instance name,
	// so all three are identical for easy correlation during debugging.
	// Truncate NodePool name so total stays within GCP's 63-char limit:
	//   "cb-" (3) + nodepool (max 53) + "-" (1) + k8s-suffix (5) = 62 chars
	truncatedPoolName := nodePool.Name
	if len(truncatedPoolName) > 53 {
		truncatedPoolName = truncatedPoolName[:53]
	}
	nodeClaim := &cloudburstv1alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("cb-%s-", truncatedPoolName),
			Namespace:    nodePool.Namespace,
			Labels: map[string]string{
				"cloudburst.io/nodepool": nodePool.Name,
			},
		},
		Spec: cloudburstv1alpha1.NodeClaimSpec{
			NodePoolRef:  nodePool.Name,
			NodeClassRef: nodeClass.Name,
			Chosen: cloudburstv1alpha1.InstanceDetails{
				Provider:        chosen.Provider,
				Region:          chosen.Region,
				InstanceType:    chosen.InstanceType,
				VCPU:            chosen.VCPU,
				RamGb:           chosen.RAMGb,
				PriceEurPerHour: chosen.PriceEurPerHour,
			},
			Alternatives: altDetails,
		},
	}

	if err := r.Create(ctx, nodeClaim); err != nil {
		return fmt.Errorf("failed to create NodeClaim: %w", err)
	}

	// Do NOT set NodeClaim status here. The NodeClaim controller owns status updates.
	// Setting status from NodePool caused 409 conflicts when both controllers reconciled
	// concurrently. The NodeClaim controller will set Phase to Pending when it sees empty phase.

	logger.Info("created NodeClaim",
		"nodeClaim", nodeClaim.Name,
		"provider", chosen.Provider,
		"region", chosen.Region,
		"instanceType", chosen.InstanceType,
	)

	return nil
}

// recordEvent records an event for the NodePool
func (r *NodePoolReconciler) recordEvent(obj client.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, message)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudburstv1alpha1.NodePool{}).
		Owns(&cloudburstv1alpha1.NodeClaim{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.enqueueNodePoolsForPod)).
		Complete(r)
}

// enqueueNodePoolsForPod enqueues all NodePools in the same namespace as the pod,
// so that demand from unschedulable pods triggers scale-up.
func (r *NodePoolReconciler) enqueueNodePoolsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	list := &cloudburstv1alpha1.NodePoolList{}
	if err := r.List(ctx, list, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}
