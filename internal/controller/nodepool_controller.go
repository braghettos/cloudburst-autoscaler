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
	"os"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
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
)

// NodePoolReconciler reconciles a NodePool object.
//
// Concept of NodePool: a pool of nodes that share the same constraints (region, arch, price, providers),
// limits (min/max nodes), template (labels/taints), and disruption
// policy. The controller creates NodeClaims when unschedulable pod demand exceeds current capacity,
// using NodePool.Requirements to decide how many nodes are needed and what to ask CloudBroker for.
type NodePoolReconciler struct {
	client.Client
	// APIReader bypasses the cache for fresh reads. Used when counting NodeClaims
	// before create decisions to avoid race conditions from stale cache.
	APIReader   client.Reader
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

	// Calculate desired capacity from unschedulable pods (scoped to this NodePool)
	desiredCapacity, err := r.calculateDesiredCapacity(ctx, nodePool)
	if err != nil {
		logger.Error(err, "unable to calculate desired capacity")
		return ctrl.Result{}, err
	}

	// Size the next node to fit current unschedulable demand so we create one node that
	// accommodates the pod(s), rather than multiple small nodes from a fixed pool size.
	// We need 1 node when there is demand; that node will be requested with min vCPU/RAM
	// derived from desiredCapacity (pod demand).
	nodesNeeded := int32(0)
	if desiredCapacity.CPUMilliTotal > 0 || desiredCapacity.MemBytesTotal > 0 {
		nodesNeeded = 1
	}

	logger.Info("calculated nodes needed",
		"desiredCapacity", desiredCapacity,
		"nodesNeeded", nodesNeeded,
	)

	// Count active NodeClaims for this NodePool. When nodesNeeded > 0, use APIReader
	// (bypasses cache) to avoid race: stale cache could show activeCount=0 and cause
	// duplicate NodeClaim creation before the cache sees the first Create.
	var reader client.Reader = r.Client
	if nodesNeeded > 0 && r.APIReader != nil {
		reader = r.APIReader
	}
	activeCount, err := r.countActiveNodeClaimsWithReader(ctx, req.Namespace, req.Name, reader)
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

	// Block creating new NodeClaims if any NodeClaim for this NodePool is in QuotaExceeded
	// (e.g. AWS VcpuLimitExceeded). Wait for the user to fix the limit; the NodeClaim
	// controller will retry periodically.
	if hasQuotaExceeded, err := r.hasQuotaExceededNodeClaim(ctx, req.Namespace, req.Name); err != nil {
		logger.Error(err, "unable to check for quota-exceeded NodeClaims")
		return ctrl.Result{}, err
	} else if hasQuotaExceeded {
		logger.Info("skipping NodeClaim creation: existing NodeClaim in QuotaExceeded, waiting for limit to be fixed")
		return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
	}

	// Create NodeClaims if needed. Create at most one per reconcile so that when the
	// controller is re-enqueued by Owns(NodeClaim) after Create(), the next reconcile
	// sees updated activeCount (or we create one more). We use APIReader for the count
	// when nodesNeeded > 0 to avoid stale cache causing duplicate creates.
	maxNodes := nodePool.Spec.Limits.MaxNodes
	if maxNodes > 0 && activeCount >= maxNodes {
		logger.Info("skipping NodeClaim creation: at maxNodes limit", "activeCount", activeCount, "maxNodes", maxNodes)
	} else if nodesNeeded > activeCount {
		nodesToCreate := nodesNeeded - activeCount
		if nodesToCreate > 1 {
			nodesToCreate = 1
		}
		if maxNodes > 0 && activeCount+nodesToCreate > maxNodes {
			nodesToCreate = maxNodes - activeCount
		}
		if nodesToCreate < 1 {
			logger.Info("skipping NodeClaim creation: would exceed maxNodes", "activeCount", activeCount, "maxNodes", maxNodes)
		} else {
			logger.Info("creating node claims",
				"nodesToCreate", nodesToCreate,
				"nodePool", req.Name,
			)

			// Get CloudBroker recommendation first to determine provider
			// This allows us to match NodeClass to the provider
			// Architecture is extracted from pod requirements (nodeSelector/nodeAffinity)
			// If no architecture requirement is found in pods, pass empty string to CloudBroker
			// (CloudBroker will handle the default or any-arch recommendation)
			overheadCPU, overheadMem := r.estimateDaemonSetOverhead(ctx)
			brokerReq := cloudbroker.RecommendationRequest{
				MinVCPU:            vcpuFromDemand(desiredCapacity.CPUMilliTotal, overheadCPU),
				MinRAMGb:           ramGbFromDemand(desiredCapacity.MemBytesTotal, overheadMem),
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
			if len(nodePool.Spec.Requirements.ExcludedInstanceTypes) > 0 {
				brokerReq.ExcludedInstanceTypes = nodePool.Spec.Requirements.ExcludedInstanceTypes
			}
			if len(nodePool.Spec.Requirements.ExcludedInstanceTypePrefixes) > 0 {
				brokerReq.ExcludedInstanceTypePrefixes = nodePool.Spec.Requirements.ExcludedInstanceTypePrefixes
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
				// Requeue instead of returning error so we retry after a delay (e.g. CloudBroker may recover)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			if len(recommendations) == 0 {
				err := fmt.Errorf("CloudBroker returned no recommendations")
				logger.Error(err, "CloudBroker returned empty list")
				r.recordEvent(nodePool, corev1.EventTypeWarning, "CloudBrokerError", err.Error())
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			chosen := &recommendations[0]
			alternatives := recommendations[1:]

			// Log CloudBroker recommendations that will drive provisioning
			logger.Info("CloudBroker recommendations",
				"request", fmt.Sprintf("minVCPU=%d minRAMGb=%.1f regionConstraint=%q maxPrice=%.4f", brokerReq.MinVCPU, brokerReq.MinRAMGb, brokerReq.RegionConstraint, brokerReq.MaxPriceEurPerHour),
				"chosen", fmt.Sprintf("%s/%s/%s (vcpu=%d ramGb=%.1f price=%.4f EUR/h)", chosen.Provider, chosen.Region, chosen.InstanceType, chosen.VCPU, chosen.RAMGb, chosen.PriceEurPerHour),
				"alternativesCount", len(alternatives),
			)
			if chosen.Provider != "gcp" && chosen.Provider != "scaleway" && chosen.Provider != "hetzner" && chosen.Provider != "digitalocean" && chosen.Provider != "ovh" && chosen.Provider != "aws" && chosen.Provider != "azure" && chosen.Provider != "aruba" && chosen.Provider != "upcloud" && chosen.Provider != "exoscale" && chosen.Provider != "seeweb" && chosen.Provider != "contabo" && chosen.Provider != "gcore" && chosen.Provider != "cherry_servers" && chosen.Provider != "clouding" && chosen.Provider != "ibm_cloud" {
				err := fmt.Errorf("recommendation provider is %q, only gcp, scaleway, hetzner, digitalocean, ovh, aws, azure, aruba, upcloud, exoscale, seeweb, contabo, gcore, cherry_servers, clouding, ibm_cloud are supported", chosen.Provider)
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
				if latestClaim.Status.Timestamps.EmptySince == nil {
					latestClaim.Status.Timestamps.EmptySince = &metav1.Time{Time: now}
					if err := r.Status().Update(ctx, latestClaim); err != nil {
						logger.Error(err, "failed to set EmptySince", "nodeClaim", latestClaim.Name)
					}
				}
				emptyDuration := now.Sub(latestClaim.Status.Timestamps.EmptySince.Time)
				if emptyDuration >= time.Duration(nodePool.Spec.Disruption.TTLSecondsAfterEmpty)*time.Second {
					logger.Info("node empty for TTL, initiating deletion",
						"nodeClaim", latestClaim.Name,
						"emptyDuration", emptyDuration,
						"ttl", nodePool.Spec.Disruption.TTLSecondsAfterEmpty,
					)
					r.recordEvent(nodePool, corev1.EventTypeNormal, "NodeEmpty", fmt.Sprintf("NodeClaim %s empty for TTL (empty: %v)", latestClaim.Name, emptyDuration))
					if err := r.initiateDeletion(ctx, latestClaim, logger); err != nil {
						logger.Error(err, "failed to initiate deletion", "nodeClaim", latestClaim.Name)
						continue
					}
				}
			} else if latestClaim.Status.Timestamps.EmptySince != nil {
				latestClaim.Status.Timestamps.EmptySince = nil
				if err := r.Status().Update(ctx, latestClaim); err != nil {
					logger.Error(err, "failed to clear EmptySince", "nodeClaim", latestClaim.Name)
				}
			}
		}
	}

	return nil
}

// isNodeEmpty checks if a node has no non-daemonset pods
func (r *NodePoolReconciler) isNodeEmpty(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (bool, error) {
	if nodeClaim.Status.NodeName == "" {
		return false, nil
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.MatchingFields{"spec.nodeName": nodeClaim.Status.NodeName}); err != nil {
		return false, fmt.Errorf("unable to list pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]

		isDaemonSet := false
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" {
				isDaemonSet = true
				break
			}
		}
		if isDaemonSet {
			continue
		}

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
// scoped to pods that match this NodePool (via cloudburst.io/nodepool or cloudburst.io/provider).
// Pods are grouped by architecture so we never mix arm64 and amd64 pods in one capacity request.
// The architecture group with the highest total demand (CPU+Memory) is returned.
func (r *NodePoolReconciler) calculateDesiredCapacity(ctx context.Context, nodePool *cloudburstv1alpha1.NodePool) (planner.DesiredCapacity, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return planner.DesiredCapacity{}, err
	}

	eligiblePods := r.filterEligiblePodsForNodePool(podList.Items, nodePool)

	archGroups := planner.GroupPodsByArch(eligiblePods)

	var bestCapacity planner.DesiredCapacity
	var bestDemand int64
	for _, pods := range archGroups {
		capacity := planner.CalculateDesiredCapacity(pods)
		demand := capacity.CPUMilliTotal + capacity.MemBytesTotal
		if demand > bestDemand {
			bestDemand = demand
			bestCapacity = capacity
		}
	}

	return bestCapacity, nil
}

// filterEligiblePodsForNodePool filters pods that are eligible for demand detection
// and match this NodePool (via cloudburst.io/nodepool or cloudburst.io/provider).
func (r *NodePoolReconciler) filterEligiblePodsForNodePool(pods []corev1.Pod, nodePool *cloudburstv1alpha1.NodePool) []corev1.Pod {
	eligible := r.filterEligiblePods(pods)
	var matched []corev1.Pod
	for i := range eligible {
		if planner.PodMatchesNodePool(&eligible[i], nodePool) {
			matched = append(matched, eligible[i])
		}
	}
	return matched
}

// patchPodsWithProviderAffinity patches eligible unschedulable pods that have no
// cloudburst.io/provider or cloudburst.io/nodepool constraint, adding nodeAffinity
// for the chosen provider so they land on the node being provisioned.
//
// Note: Kubernetes forbids changing spec.affinity on existing pods (only a limited
// set of spec fields are mutable). Adding affinity to a pod with nil Affinity will
// fail. We skip such pods to avoid error log spam; the NodeClaim is still created
// and the scheduler may place the pod on the new node via other constraints.
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

		// Skip pods with nil Affinity: K8s forbids changing spec.affinity on existing pods.
		if latest.Spec.Affinity == nil {
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
// hasQuotaExceededNodeClaim returns true if any NodeClaim for this NodePool is in QuotaExceeded phase.
func (r *NodePoolReconciler) hasQuotaExceededNodeClaim(ctx context.Context, namespace, nodePoolName string) (bool, error) {
	nodeClaimList := &cloudburstv1alpha1.NodeClaimList{}
	if err := r.List(ctx, nodeClaimList, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range nodeClaimList.Items {
		claim := &nodeClaimList.Items[i]
		if claim.Spec.NodePoolRef != nodePoolName {
			continue
		}
		if claim.Status.Phase == cloudburstv1alpha1.NodeClaimPhaseQuotaExceeded {
			return true, nil
		}
	}
	return false, nil
}

func (r *NodePoolReconciler) countActiveNodeClaimsWithReader(ctx context.Context, namespace, nodePoolName string, reader client.Reader) (int32, error) {
	if reader == nil {
		reader = r.Client
	}
	nodeClaimList := &cloudburstv1alpha1.NodeClaimList{}
	if err := reader.List(ctx, nodeClaimList, client.InNamespace(namespace)); err != nil {
		return 0, err
	}

	count := int32(0)
	for i := range nodeClaimList.Items {
		claim := &nodeClaimList.Items[i]
		// Check if this NodeClaim belongs to this NodePool
		if claim.Spec.NodePoolRef != nodePoolName {
			continue
		}

		// Count only active phases. Exclude Failed (terminal).
		// IMPORTANT: treat empty phase as active. Newly created NodeClaims have
		// status.Phase="" until the NodeClaim controller runs; excluding them
		// caused duplicate NodeClaim creation when the NodePool requeue (15s)
		// ran before the NodeClaim controller had set Phase to Pending.
		// We also treat Deleting as "active" to avoid spinning up a replacement
		// node while the old one is still being drained/terminated.
		// QuotaExceeded is counted as active (we're waiting for the limit to be fixed).
		phase := claim.Status.Phase
		if phase == cloudburstv1alpha1.NodeClaimPhaseFailed {
			continue
		}
		// Empty, Pending, Provisioning, Joining, Ready, Deleting, QuotaExceeded
		count++
	}

	return count, nil
}

// vcpuFromDemand returns the minimum vCPU to request for a node that fits the demand
// plus DaemonSet/system overhead so the node has enough room for both.
func vcpuFromDemand(cpuMilli int64, overheadMilli int64) int32 {
	need := int32((cpuMilli + overheadMilli + 999) / 1000)
	if need < 1 {
		need = 1
	}
	return need
}

// ramGbFromDemand returns the minimum RAM in GB to request for a node that fits the demand
// plus DaemonSet/system overhead so the node has enough room for both.
func ramGbFromDemand(memBytes int64, overheadBytes int64) float64 {
	const bytesPerGb = 1024 * 1024 * 1024
	needGb := float64((memBytes + overheadBytes + bytesPerGb - 1) / bytesPerGb)
	if needGb < 1 {
		needGb = 1
	}
	return needGb
}

// estimateDaemonSetOverhead sums resource requests from all DaemonSets in the cluster
// and adds a fixed buffer for system daemons (kubelet, containerd, tailscale).
func (r *NodePoolReconciler) estimateDaemonSetOverhead(ctx context.Context) (cpuMilli int64, memBytes int64) {
	const (
		systemCPUMilliFallback = 500
		systemMemBytesFallback = 512 * 1024 * 1024
		systemCPUMilli         = 300
		systemMemBytes         = 256 * 1024 * 1024
	)

	dsList := &appsv1.DaemonSetList{}
	if err := r.List(ctx, dsList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list DaemonSets for overhead estimation, using fallback")
		return systemCPUMilliFallback, systemMemBytesFallback
	}

	for _, ds := range dsList.Items {
		for _, c := range ds.Spec.Template.Spec.Containers {
			if cpu, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				cpuMilli += cpu.MilliValue()
			}
			if mem, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				memBytes += mem.Value()
			}
		}
	}

	cpuMilli += systemCPUMilli
	memBytes += systemMemBytes
	return
}

// brokerReqToSummary converts a CloudBroker request to NodeClaim RecommendationRequestSummary for retry.
func brokerReqToSummary(req *cloudbroker.RecommendationRequest) *cloudburstv1alpha1.RecommendationRequestSummary {
	if req == nil {
		return nil
	}
	s := &cloudburstv1alpha1.RecommendationRequestSummary{
		MinVCPU:                  req.MinVCPU,
		MinRAMGb:                 req.MinRAMGb,
		RegionConstraint:         req.RegionConstraint,
		MaxPriceEurPerHour:       req.MaxPriceEurPerHour,
		AllowedProviders:         append([]string(nil), req.AllowedProviders...),
		DataSourceProvider:       req.DataSourceProvider,
		DataSourceRegion:         req.DataSourceRegion,
		EstimatedEgressGbPerHour: req.EstimatedEgressGbPerHour,
		MaxTCOEurPerHour:         req.MaxTCOEurPerHour,
	}
	if req.Arch != nil {
		s.Arch = *req.Arch
	}
	return s
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
		return nodeClass.Spec.Azure != nil && nodeClass.Spec.Azure.SubscriptionID != "" && nodeClass.Spec.Azure.ResourceGroup != ""
	case "aruba":
		return nodeClass.Spec.Aruba != nil && nodeClass.Spec.Aruba.ProjectID != ""
	case "upcloud":
		return nodeClass.Spec.UpCloud != nil && nodeClass.Spec.UpCloud.APITokenSecretRef != nil && nodeClass.Spec.UpCloud.APITokenSecretRef.Name != ""
	case "exoscale":
		return nodeClass.Spec.Exoscale != nil && nodeClass.Spec.Exoscale.APIKeySecretRef != nil && nodeClass.Spec.Exoscale.APIKeySecretRef.Name != ""
	case "seeweb":
		return nodeClass.Spec.Seeweb != nil && nodeClass.Spec.Seeweb.APITokenSecretRef != nil && nodeClass.Spec.Seeweb.APITokenSecretRef.Name != ""
	case "contabo":
		return nodeClass.Spec.Contabo != nil && ((nodeClass.Spec.Contabo.ClientIDSecretRef != nil && nodeClass.Spec.Contabo.ClientSecretSecretRef != nil) || (os.Getenv("CONTABO_CLIENT_ID") != "" && os.Getenv("CONTABO_CLIENT_SECRET") != ""))
	case "gcore":
		return nodeClass.Spec.Gcore != nil && nodeClass.Spec.Gcore.ProjectID > 0 && (nodeClass.Spec.Gcore.APIKeySecretRef != nil || os.Getenv("GCORE_API_KEY") != "")
	case "cherry_servers":
		return nodeClass.Spec.CherryServers != nil && nodeClass.Spec.CherryServers.ProjectID > 0 && (nodeClass.Spec.CherryServers.APITokenSecretRef != nil || os.Getenv("CHERRY_SERVERS_API_TOKEN") != "")
	case "clouding":
		return nodeClass.Spec.Clouding != nil && (nodeClass.Spec.Clouding.APIKeySecretRef != nil || os.Getenv("CLOUDING_API_KEY") != "")
	case "ibm_cloud":
		return nodeClass.Spec.IBMCloud != nil && nodeClass.Spec.IBMCloud.ResourceGroup != "" && (nodeClass.Spec.IBMCloud.APIKeySecretRef != nil || os.Getenv("IBMCLOUD_API_KEY") != "")
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
	reqSummary := brokerReqToSummary(brokerReq)
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
			Alternatives:          altDetails,
			RecommendationRequest: reqSummary,
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
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
		pod := obj.(*corev1.Pod)
		if pod.Spec.NodeName == "" {
			return nil
		}
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudburstv1alpha1.NodePool{}).
		Owns(&cloudburstv1alpha1.NodeClaim{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.enqueueNodePoolsForPod)).
		Complete(r)
}

// enqueueNodePoolsForPod enqueues NodePools that the pod could match, so that demand
// from unschedulable pods triggers scale-up. If the pod has cloudburst.io/nodepool or
// cloudburst.io/provider, only matching NodePools are enqueued. Otherwise all NodePools
// in the namespace are enqueued (unbound pod).
func (r *NodePoolReconciler) enqueueNodePoolsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	list := &cloudburstv1alpha1.NodePoolList{}
	if err := r.List(ctx, list, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}
	// Unbound pods (no nodepool/provider constraint) enqueue all NodePools
	if !planner.PodHasProviderOrNodePoolConstraint(pod) {
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return reqs
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		np := &list.Items[i]
		if planner.PodMatchesNodePool(pod, np) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(np)})
		}
	}
	return reqs
}
