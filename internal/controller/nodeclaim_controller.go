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
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
	"github.com/diegobraga/cloudburst-autoscaler/internal/bootstrap"
	"github.com/diegobraga/cloudburst-autoscaler/internal/cloudbroker"
	cloudbursterrors "github.com/diegobraga/cloudburst-autoscaler/internal/errors"
	"github.com/diegobraga/cloudburst-autoscaler/internal/metrics"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provisioner"
	"github.com/diegobraga/cloudburst-autoscaler/internal/tailscale"
)

const (
	// NodeClaimFinalizer is the finalizer that blocks CR deletion until the cloud instance is deleted
	NodeClaimFinalizer = "cloudburst.io/nodeclaim-protection"
)

// NodeClaimReconciler reconciles a NodeClaim object
type NodeClaimReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	Clientset   kubernetes.Interface
	RestConfig  *rest.Config
	Config      *ControllerConfig
	CloudBroker CloudBrokerClient
}

// CloudBrokerClient is the interface for fetching recommendations (used for retry on provisioning failure).
type CloudBrokerClient interface {
	Recommend(ctx context.Context, req *cloudbroker.RecommendationRequest) ([]cloudbroker.RecommendationResult, error)
}

//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile reconciles NodeClaim and provisions GCP instances
func (r *NodeClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("nodeclaim", req.NamespacedName)

	// Fetch NodeClaim
	nodeClaim := &cloudburstv1alpha1.NodeClaim{}
	if err := r.Get(ctx, req.NamespacedName, nodeClaim); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch NodeClaim")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !nodeClaim.DeletionTimestamp.IsZero() {
		logger.Info("reconcile: deletion requested", "phase", nodeClaim.Status.Phase)
		return r.handleDeletion(ctx, nodeClaim, logger)
	}

	// Handle provisioning based on phase
	logger.V(1).Info("reconcile", "phase", nodeClaim.Status.Phase)
	switch nodeClaim.Status.Phase {
	case cloudburstv1alpha1.NodeClaimPhasePending:
		return r.handlePending(ctx, nodeClaim, logger)
	case cloudburstv1alpha1.NodeClaimPhaseProvisioning:
		return r.handleProvisioning(ctx, nodeClaim, logger)
	case cloudburstv1alpha1.NodeClaimPhaseJoining:
		return r.handleJoining(ctx, nodeClaim, logger)
	case cloudburstv1alpha1.NodeClaimPhaseReady:
		return r.handleReady(ctx, nodeClaim, logger)
	case cloudburstv1alpha1.NodeClaimPhaseDeleting:
		return r.handleDeleting(ctx, nodeClaim, logger)
	case cloudburstv1alpha1.NodeClaimPhaseFailed:
		return r.handleFailed(ctx, nodeClaim, logger)
	case cloudburstv1alpha1.NodeClaimPhaseQuotaExceeded:
		// Quota exceeded (e.g. AWS VcpuLimitExceeded): retry provisioning periodically
		return r.handleQuotaExceeded(ctx, nodeClaim, logger)
	default:
		// Unknown phase, set to Pending. Retry on conflict (NodePool may have just created this NodeClaim).
		logger.Info("unknown phase, setting to Pending", "phase", nodeClaim.Status.Phase)
		fromPhase := nodeClaim.Status.Phase
		metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(nodeClaim.Spec.Chosen.Provider, string(fromPhase), string(cloudburstv1alpha1.NodeClaimPhasePending)).Inc()
		apply := func(nc *cloudburstv1alpha1.NodeClaim) {
			nc.Status.Phase = cloudburstv1alpha1.NodeClaimPhasePending
			nc.Status.Timestamps.LastTransition = metav1.Now()
		}
		if err := r.statusUpdateWithRetry(ctx, nodeClaim, apply, logger); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
}

// handleFailed handles NodeClaims in Failed phase. When a replacement NodeClaim exists
// for the same NodePool (created because Failed NodeClaims are excluded from activeCount),
// this failed NodeClaim is deleted so the cloud instance is cleaned up and the UI no longer
// shows stale failed claims.
func (r *NodeClaimReconciler) handleFailed(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	if nodeClaim.Spec.NodePoolRef == "" {
		return ctrl.Result{}, nil
	}

	nodeClaimList := &cloudburstv1alpha1.NodeClaimList{}
	if err := r.List(ctx, nodeClaimList, client.InNamespace(nodeClaim.Namespace)); err != nil {
		logger.Error(err, "unable to list NodeClaims for NodePool")
		return ctrl.Result{}, err
	}

	for i := range nodeClaimList.Items {
		other := &nodeClaimList.Items[i]
		if other.Name == nodeClaim.Name {
			continue
		}
		if other.Spec.NodePoolRef != nodeClaim.Spec.NodePoolRef {
			continue
		}
		if other.Status.Phase == cloudburstv1alpha1.NodeClaimPhaseFailed {
			continue
		}
		// Found an active replacement (Pending, Provisioning, Joining, Ready, Deleting, QuotaExceeded)
		logger.Info("deleting failed NodeClaim: replacement exists",
			"replacement", other.Name,
			"replacementPhase", other.Status.Phase,
		)
		r.recordEvent(nodeClaim, corev1.EventTypeNormal, "ReplacementExists", fmt.Sprintf("Deleting failed NodeClaim; replacement %s is %s", other.Name, other.Status.Phase))
		if err := r.Delete(ctx, nodeClaim); err != nil {
			logger.Error(err, "failed to delete failed NodeClaim")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// handleQuotaExceeded handles NodeClaims in QuotaExceeded phase.
// Transitions to Pending to retry provisioning (user may have fixed the quota).
func (r *NodeClaimReconciler) handleQuotaExceeded(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("retrying provisioning after quota exceeded", "nodeClaim", nodeClaim.Name, "lastError", nodeClaim.Status.LastError)
	apply := func(nc *cloudburstv1alpha1.NodeClaim) {
		nc.Status.Phase = cloudburstv1alpha1.NodeClaimPhasePending
		nc.Status.Timestamps.LastTransition = metav1.Now()
		// Keep LastError for visibility until we succeed
	}
	if err := r.statusUpdateWithRetry(ctx, nodeClaim, apply, logger); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// handlePending handles NodeClaims in Pending phase
func (r *NodeClaimReconciler) handlePending(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("handling Pending phase", "nodeClaim", nodeClaim.Name)

	// Get NodeClass
	nodeClass := &cloudburstv1alpha1.NodeClass{}
	nodeClassKey := client.ObjectKey{
		Namespace: nodeClaim.Namespace,
		Name:      nodeClaim.Spec.NodeClassRef,
	}
	if err := r.Get(ctx, nodeClassKey, nodeClass); err != nil {
		logger.Error(err, "unable to fetch NodeClass")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "NodeClassNotFound", fmt.Sprintf("Failed to fetch NodeClass: %v", err))
		return ctrl.Result{}, err
	}

	// Determine provider from NodeClaim
	provider := nodeClaim.Spec.Chosen.Provider
	if provider == "" {
		err := fmt.Errorf("NodeClaim %s spec.chosen.provider is required", nodeClaim.Name)
		logger.Error(err, "missing provider")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InvalidConfiguration", err.Error())
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}

	// Get Tailscale auth key (from API ephemeral key or from secret/env)
	tailscaleAuthKey, err := r.getTailscaleAuthKey(ctx, nodeClaim, nodeClass, logger)
	if err != nil {
		logger.Error(err, "unable to get Tailscale auth key")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "TailscaleAuthKeyError", fmt.Sprintf("Failed to get Tailscale auth key: %v", err))
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}

	// Get join command
	joinCommandProvider := bootstrap.NewJoinCommandProvider(
		nodeClass.Spec.Join.KindControlPlaneContainer,
		nodeClass.Spec.Join.KindClusterName,
		nodeClass.Spec.Join.TokenTtlMinutes,
	)
	// Set Kubernetes client for API-based command execution
	if r.Clientset != nil && r.RestConfig != nil {
		joinCommandProvider.SetKubernetesClient(r.Clientset, r.RestConfig)
	}
	joinCommand, err := joinCommandProvider.GetJoinCommand(ctx)
	if err != nil {
		logger.Error(err, "unable to get join command")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "JoinCommandError", fmt.Sprintf("Failed to get join command: %v", err))
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}

	logger.Info("retrieved join command", "joinCommand", joinCommand)

	// Host API server URL from NodeClass (e.g. https://<HOST_TAILSCALE_IP>:6443 when Tailscale runs on the host).
	hostApiServer := strings.TrimSpace(nodeClass.Spec.Join.HostApiServer)
	if hostApiServer == "" {
		err := fmt.Errorf("NodeClass %s spec.join.hostApiServer is required (e.g. https://<HOST_TAILSCALE_IP>:6443)", nodeClass.Name)
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "HostApiServerRequired", err.Error())
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}
	if !strings.HasPrefix(hostApiServer, "https://") && !strings.HasPrefix(hostApiServer, "http://") {
		hostApiServer = "https://" + hostApiServer
	}

	// Optionally embed cluster-info discovery kubeconfig with server URL set to hostApiServer so the VM does not need to fetch it.
	var discoveryKubeconfigBase64 string
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: "kube-public", Name: "cluster-info"}, &cm); err == nil {
		kc := cm.Data["kubeconfig"]
		if kc == "" {
			for _, v := range cm.Data {
				kc = v
				break
			}
		}
		if kc != "" {
			modified := bootstrap.ReplaceDiscoveryKubeconfigServer(kc, hostApiServer)
			discoveryKubeconfigBase64 = base64.StdEncoding.EncodeToString([]byte(modified))
			logger.Info("embedded cluster-info discovery kubeconfig in startup script", "server", hostApiServer)
		}
	}

	// For GCP, use instance name (cb-gcp-...) as node hostname so the K8s node is identifiable.
	nodeNameForBootstrap := instanceNameForProvider(provider, nodeClaim.Name)
	startupScript := bootstrap.GenerateStartupScript(&bootstrap.StartupScriptConfig{
		NodeName:                  nodeNameForBootstrap,
		TailscaleAuthKey:          tailscaleAuthKey,
		KubernetesVersion:         nodeClass.Spec.Bootstrap.KubernetesVersion,
		JoinCommand:               joinCommand,
		HostApiServer:             hostApiServer,
		DiscoveryKubeconfigBase64: discoveryKubeconfigBase64,
	})

	// Ensure finalizer so that when the NodeClaim is deleted we get a reconcile and can delete the cloud instance.
	// Use Patch so we only change metadata.finalizers and avoid overwriting status (which would cause a conflict on the later status update).
	if !controllerutil.ContainsFinalizer(nodeClaim, NodeClaimFinalizer) {
		original := nodeClaim.DeepCopy()
		controllerutil.AddFinalizer(nodeClaim, NodeClaimFinalizer)
		if err := r.Patch(ctx, nodeClaim, client.MergeFrom(original)); err != nil {
			logger.Error(err, "unable to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Provision instance based on provider (via factory). Region comes from CloudBroker recommendation.
	chosenRegion := nodeClaim.Spec.Chosen.Region
	regionOrZone, image, err := getProvisioningParams(provider, nodeClass, chosenRegion)
	if err != nil {
		logger.Error(err, "invalid provisioning params")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InvalidConfiguration", err.Error())
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}
	prov, err := provisioner.NewFromNodeClass(ctx, provider, nodeClass, nodeClaim.Namespace, r, regionOrZone)
	if err != nil {
		logger.Error(err, "unable to create provisioner")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "ProvisionerError", fmt.Sprintf("Failed to create provisioner: %v", err))
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}
	defer prov.Close()
	instanceName := instanceNameForProvider(provider, nodeClaim.Name)
	instanceType := nodeClaim.Spec.Chosen.InstanceType
	chosen := nodeClaim.Spec.Chosen
	logger.Info("provisioning with CloudBroker recommendation",
		"provider", provider,
		"region", chosen.Region,
		"instanceType", instanceType,
		"vcpu", chosen.VCPU,
		"ramGb", chosen.RamGb,
		"priceEurPerHour", chosen.PriceEurPerHour,
		"instanceName", instanceName,
		"alternativesCount", len(nodeClaim.Spec.Alternatives),
	)
	r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceCreationStarted", fmt.Sprintf("Creating %s instance %s", provider, instanceName))
	instanceID, err := prov.CreateInstance(ctx, instanceName, instanceType, regionOrZone, startupScript, image)
	if err != nil {
		logger.Error(err, "failed to create instance")
		metrics.ProviderErrorsTotal.WithLabelValues(provider, "create").Inc()
		// VcpuLimitExceeded: write error to status, stop creating new NodeClaims, retry periodically
		if cloudbursterrors.IsVcpuLimitExceeded(err) {
			metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(nodeClaim.Status.Phase), string(cloudburstv1alpha1.NodeClaimPhaseQuotaExceeded)).Inc()
			r.recordEvent(nodeClaim, corev1.EventTypeWarning, "QuotaExceeded", fmt.Sprintf("Cloud provider quota exceeded: %v. Will retry when limit is fixed.", err))
			r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseQuotaExceeded, err.Error())
			return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
		}
		// Other errors: try alternatives if available
		if len(nodeClaim.Spec.Alternatives) > 0 {
			next := nodeClaim.Spec.Alternatives[0]
			remaining := nodeClaim.Spec.Alternatives[1:]
			logger.Info("falling back to next CloudBroker alternative",
				"previous", nodeClaim.Spec.Chosen.InstanceType,
				"next", next.InstanceType,
				"nextProvider", next.Provider,
				"remaining", len(remaining),
			)
			r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceCreationFallback", fmt.Sprintf("Create failed (%v), trying alternative: %s/%s", err, next.Provider, next.InstanceType))
			original := nodeClaim.DeepCopy()
			nodeClaim.Spec.Chosen = next
			nodeClaim.Spec.Alternatives = remaining
			if err := r.Patch(ctx, nodeClaim, client.MergeFrom(original)); err != nil {
				logger.Error(err, "failed to patch NodeClaim with alternative")
				r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		// No alternatives: fetch new recommendations from CloudBroker excluding the failed instance type
		if r.CloudBroker != nil && nodeClaim.Spec.RecommendationRequest != nil {
			brokerReq := recommendationRequestFromSummary(nodeClaim.Spec.RecommendationRequest)
			brokerReq.ExcludedInstanceTypes = []string{nodeClaim.Spec.Chosen.InstanceType}
			brokerReq.Limit = 10
			recommendations, fetchErr := r.CloudBroker.Recommend(ctx, brokerReq)
			if fetchErr == nil && len(recommendations) > 0 {
				next := recommendations[0]
				remaining := recommendations[1:]
				logger.Info("fetching new CloudBroker recommendations after provisioning failure",
					"failedInstanceType", nodeClaim.Spec.Chosen.InstanceType,
					"next", next.InstanceType,
					"nextProvider", next.Provider,
					"remaining", len(remaining),
				)
				r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceCreationFallback", fmt.Sprintf("Create failed (%v), trying new recommendation: %s/%s", err, next.Provider, next.InstanceType))
				original := nodeClaim.DeepCopy()
				nodeClaim.Spec.Chosen = cloudburstv1alpha1.InstanceDetails{
					Provider:        next.Provider,
					Region:          next.Region,
					InstanceType:    next.InstanceType,
					VCPU:            next.VCPU,
					RamGb:           next.RAMGb,
					PriceEurPerHour: next.PriceEurPerHour,
				}
				nodeClaim.Spec.Alternatives = make([]cloudburstv1alpha1.InstanceDetails, 0, len(remaining))
				for _, a := range remaining {
					nodeClaim.Spec.Alternatives = append(nodeClaim.Spec.Alternatives, cloudburstv1alpha1.InstanceDetails{
						Provider:        a.Provider,
						Region:          a.Region,
						InstanceType:    a.InstanceType,
						VCPU:            a.VCPU,
						RamGb:           a.RAMGb,
						PriceEurPerHour: a.PriceEurPerHour,
					})
				}
				if err := r.Patch(ctx, nodeClaim, client.MergeFrom(original)); err != nil {
					logger.Error(err, "failed to patch NodeClaim with new recommendation")
					r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
					return ctrl.Result{}, err
				}
				return ctrl.Result{Requeue: true}, nil
			}
			if fetchErr != nil {
				logger.V(1).Info("CloudBroker retry fetch failed, marking as Failed", "err", fetchErr)
			}
		}
		metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(nodeClaim.Status.Phase), string(cloudburstv1alpha1.NodeClaimPhaseFailed)).Inc()
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceCreationFailed", fmt.Sprintf("Failed to create instance: %v", err))
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}

	// Update NodeClaim status with instance ID and the recommendation used for provisioning
	nodeClaim.Status.InstanceID = instanceID
	nodeClaim.Status.RecommendationUsed = &cloudburstv1alpha1.InstanceDetails{
		Provider:        nodeClaim.Spec.Chosen.Provider,
		Region:          nodeClaim.Spec.Chosen.Region,
		InstanceType:    nodeClaim.Spec.Chosen.InstanceType,
		VCPU:            nodeClaim.Spec.Chosen.VCPU,
		RamGb:           nodeClaim.Spec.Chosen.RamGb,
		PriceEurPerHour: nodeClaim.Spec.Chosen.PriceEurPerHour,
	}
	fromPhase := nodeClaim.Status.Phase
	nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseProvisioning
	metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(fromPhase), string(cloudburstv1alpha1.NodeClaimPhaseProvisioning)).Inc()
	nodeClaim.Status.Timestamps.CreatedAt = metav1.Now()
	nodeClaim.Status.Timestamps.LastTransition = metav1.Now()

	if err := r.Status().Update(ctx, nodeClaim); err != nil {
		logger.Error(err, "unable to update NodeClaim status")
		return ctrl.Result{}, err
	}

	logger.Info("instance created, transitioning to Provisioning",
		"instanceID", instanceID,
		"provider", provider,
	)
	r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceCreated", fmt.Sprintf("%s instance created: %s", provider, instanceID))

	return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
}

// getProvisioningParams returns regionOrZone and image for the given provider. Region/zone comes from chosenRegion (CloudBroker recommendation); NodeClass supplies image and optional fallbacks.
func getProvisioningParams(provider string, nodeClass *cloudburstv1alpha1.NodeClass, chosenRegion string) (regionOrZone, image string, err error) {
	if chosenRegion == "" {
		return "", "", fmt.Errorf("chosen region is required (from CloudBroker recommendation)")
	}
	switch provider {
	case "gcp":
		// CloudBroker returns region (e.g. europe-west1); GCP API needs zone (e.g. europe-west1-b).
		zone := chosenRegion
		if !gcpLooksLikeZone(chosenRegion) {
			suffix := nodeClass.Spec.GCP.PreferredZoneSuffix
			if suffix == "" {
				suffix = "-b"
			}
			zone = chosenRegion + suffix
		}
		return zone, "", nil
	case "scaleway":
		if nodeClass.Spec.Scaleway == nil {
			return "", "", fmt.Errorf("NodeClass spec.scaleway is required")
		}
		return chosenRegion, nodeClass.Spec.Scaleway.Image, nil
	case "hetzner":
		if nodeClass.Spec.Hetzner == nil {
			return "", "", fmt.Errorf("NodeClass spec.hetzner is required")
		}
		// NodeClass.Location overrides CloudBroker recommendation (e.g. when chosen location is disabled).
		hetznerLocation := chosenRegion
		if nodeClass.Spec.Hetzner.Location != "" {
			hetznerLocation = nodeClass.Spec.Hetzner.Location
		}
		return hetznerLocation, nodeClass.Spec.Hetzner.Image, nil
	case "digitalocean":
		if nodeClass.Spec.DigitalOcean == nil {
			return "", "", fmt.Errorf("NodeClass spec.digitalocean is required")
		}
		return chosenRegion, nodeClass.Spec.DigitalOcean.Image, nil
	case "ovh":
		if nodeClass.Spec.OVH == nil {
			return "", "", fmt.Errorf("NodeClass spec.ovh is required")
		}
		// CloudBroker now returns actual OVH region slugs (e.g. "GRA7", "DE1", "SBG5")
		// which are directly usable with the OVH API. NodeClass.Spec.OVH.Region can
		// optionally pin to a specific region, overriding the CloudBroker recommendation.
		ovhRegion := chosenRegion
		if nodeClass.Spec.OVH.Region != "" {
			ovhRegion = nodeClass.Spec.OVH.Region
		}
		return ovhRegion, nodeClass.Spec.OVH.ImageID, nil
	case "aws":
		if nodeClass.Spec.AWS == nil || nodeClass.Spec.AWS.AMI == "" {
			return "", "", fmt.Errorf("NodeClass spec.aws with ami is required")
		}
		return chosenRegion, nodeClass.Spec.AWS.AMI, nil
	case "azure":
		if nodeClass.Spec.Azure == nil {
			return "", "", fmt.Errorf("NodeClass spec.azure is required")
		}
		// Azure locations from CloudBroker may not exactly match Azure API names.
		// When the NodeClass specifies a location, use it as the authoritative value.
		azureLocation := chosenRegion
		if nodeClass.Spec.Azure.Location != "" {
			azureLocation = nodeClass.Spec.Azure.Location
		}
		return azureLocation, nodeClass.Spec.Azure.ImageReference, nil
	case "seeweb":
		if nodeClass.Spec.Seeweb == nil {
			return "", "", fmt.Errorf("NodeClass spec.seeweb is required")
		}
		// CloudBroker returns region slugs like it-fr2, it-mi2, ch-lug1, bg-sof1
		seewebLocation := chosenRegion
		if nodeClass.Spec.Seeweb.Location != "" {
			seewebLocation = nodeClass.Spec.Seeweb.Location
		}
		return seewebLocation, nodeClass.Spec.Seeweb.Image, nil
	case "aruba":
		if nodeClass.Spec.Aruba == nil {
			return "", "", fmt.Errorf("NodeClass spec.aruba is required")
		}
		// CloudBroker returns itbg-bergamo; Aruba uses ITBG-Bergamo and ITBG-1/2/3 for datacenter
		arubaLocation := chosenRegion
		if arubaLocation == "" {
			arubaLocation = "itbg-bergamo"
		}
		return arubaLocation, nodeClass.Spec.Aruba.Image, nil
	default:
		return "", "", fmt.Errorf("unsupported provider: %s", provider)
	}
}

// gcpLooksLikeZone returns true if s looks like a GCP zone (e.g. europe-west1-b) rather than a region (e.g. europe-west1).
func gcpLooksLikeZone(s string) bool {
	// GCP zones are region-suffix (e.g. europe-west1-b). Regions have no trailing -a/-b/-c.
	parts := strings.Split(s, "-")
	if len(parts) < 3 {
		return false
	}
	last := parts[len(parts)-1]
	return len(last) == 1 && last >= "a" && last <= "z"
}

// handleProvisioning handles NodeClaims in Provisioning phase
func (r *NodeClaimReconciler) handleProvisioning(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("handling Provisioning phase", "instanceID", nodeClaim.Status.InstanceID)

	// Check timeout
	if nodeClaim.Status.Timestamps.CreatedAt.IsZero() {
		// CreatedAt not set, set it now
		nodeClaim.Status.Timestamps.CreatedAt = metav1.Now()
		if err := r.Status().Update(ctx, nodeClaim); err != nil {
			logger.Error(err, "failed to update CreatedAt timestamp")
			return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, err
		}
		return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
	}

	elapsed := time.Since(nodeClaim.Status.Timestamps.CreatedAt.Time)
	if elapsed > r.Config.ProvisioningTimeout {
		err := fmt.Errorf("provisioning timeout after %v", elapsed)
		logger.Error(err, "provisioning timeout exceeded")
		metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(nodeClaim.Spec.Chosen.Provider, string(nodeClaim.Status.Phase), string(cloudburstv1alpha1.NodeClaimPhaseFailed)).Inc()
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "ProvisioningTimeout", err.Error())
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, nil
	}

	// Get NodeClass
	nodeClass := &cloudburstv1alpha1.NodeClass{}
	nodeClassKey := client.ObjectKey{
		Namespace: nodeClaim.Namespace,
		Name:      nodeClaim.Spec.NodeClassRef,
	}
	if err := r.Get(ctx, nodeClassKey, nodeClass); err != nil {
		logger.Error(err, "unable to fetch NodeClass")
		return ctrl.Result{}, err
	}

	provider := nodeClaim.Spec.Chosen.Provider
	regionOrZone, _, err := getProvisioningParams(provider, nodeClass, nodeClaim.Spec.Chosen.Region)
	if err != nil {
		logger.Error(err, "invalid provisioning params in Provisioning phase")
		return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
	}
	prov, err := provisioner.NewFromNodeClass(ctx, provider, nodeClass, nodeClaim.Namespace, r, regionOrZone)
	if err != nil {
		logger.Error(err, "unable to create provisioner")
		return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
	}
	defer prov.Close()
	isRunning, err := prov.IsInstanceRunning(ctx, nodeClaim.Status.InstanceID)
	if err != nil {
		var failedErr *cloudbursterrors.InstanceFailedError
		if errors.As(err, &failedErr) {
			logger.Error(err, "instance failed permanently")
			metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(nodeClaim.Status.Phase), string(cloudburstv1alpha1.NodeClaimPhaseFailed)).Inc()
			r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceFailed", err.Error())
			r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to check instance status")
		metrics.ProviderErrorsTotal.WithLabelValues(provider, "status").Inc()
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceStatusCheckFailed", fmt.Sprintf("Failed to check instance status: %v", err))
		return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
	}

	// (per-provider switch replaced by provisioner.NewFromNodeClass + IsInstanceRunning above)

	if isRunning {
		logger.Info("instance is running, transitioning to Joining", "provider", provider)
		r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceRunning", fmt.Sprintf("%s instance is now running", provider))
		if !nodeClaim.Status.Timestamps.CreatedAt.IsZero() {
			metrics.ProvisioningDurationSeconds.WithLabelValues(provider).Observe(time.Since(nodeClaim.Status.Timestamps.CreatedAt.Time).Seconds())
		}
		fromPhase := nodeClaim.Status.Phase
		nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseJoining
		metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(fromPhase), string(cloudburstv1alpha1.NodeClaimPhaseJoining)).Inc()
		nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
		if err := r.Status().Update(ctx, nodeClaim); err != nil {
			logger.Error(err, "unable to update NodeClaim status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
	}

	// Instance not running yet, requeue
	logger.Info("instance not running yet, requeuing",
		"instanceID", nodeClaim.Status.InstanceID,
		"provider", provider,
		"elapsed", elapsed.String(),
		"hint", fmt.Sprintf("waiting for %s instance to reach running state (VM boot + startup script)", provider),
	)
	return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
}

// handleJoining handles NodeClaims in Joining phase
func (r *NodeClaimReconciler) handleJoining(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("handling Joining phase",
		"instanceID", nodeClaim.Status.InstanceID,
		"hint", "waiting for Kubernetes node to appear and become Ready",
	)

	// Check timeout
	joiningStartTime := nodeClaim.Status.Timestamps.LastTransition.Time
	if joiningStartTime.IsZero() {
		// LastTransition not set, set it now
		nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
		if err := r.Status().Update(ctx, nodeClaim); err != nil {
			logger.Error(err, "failed to update LastTransition timestamp")
			return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, err
		}
		return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
	}

	elapsed := time.Since(joiningStartTime)
	if elapsed > r.Config.JoiningTimeout {
		err := fmt.Errorf("joining timeout after %v", elapsed)
		logger.Error(err, "joining timeout exceeded")
		metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(nodeClaim.Spec.Chosen.Provider, string(nodeClaim.Status.Phase), string(cloudburstv1alpha1.NodeClaimPhaseFailed)).Inc()
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "JoiningTimeout", err.Error())
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, nil
	}

	// The node name matches instanceNameForProvider (set as hostname in bootstrap script).
	// For GCP this is cb-gcp-<nodepool>-<suffix>; for others it's nodeClaim.Name.
	expectedNodeName := instanceNameForProvider(nodeClaim.Spec.Chosen.Provider, nodeClaim.Name)

	// List nodes and find matching node
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		logger.Error(err, "unable to list nodes")
		return ctrl.Result{RequeueAfter: r.Config.RequeueInterval}, nil
	}

	instanceName := instanceNameFromID(nodeClaim.Status.InstanceID)
	nodeNames := make([]string, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		nodeNames = append(nodeNames, nodeList.Items[i].Name)
	}
	logger.V(1).Info("listed nodes", "count", len(nodeList.Items), "nodeNames", nodeNames)

	var matchingNode *corev1.Node
	var matchReason string
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		// Check if node name matches the expected name (hostname set in bootstrap)
		if node.Name == expectedNodeName {
			matchingNode = node
			matchReason = "name"
			break
		}
		// Check labels for nodeclaim reference
		if node.Labels != nil {
			if val, ok := node.Labels["cloudburst.io/nodeclaim"]; ok && val == nodeClaim.Name {
				matchingNode = node
				matchReason = "label"
				break
			}
		}
		// Fallback: match by provider ID (instance name or full instance ID for OVH/UUID-style IDs)
		if providerIDMatchesInstance(node.Spec.ProviderID, nodeClaim.Status.InstanceID) {
			matchingNode = node
			matchReason = "providerID"
			break
		}
	}

	if matchingNode == nil {
		logger.Info("node not found yet, requeuing",
			"expectedNodeName", expectedNodeName,
			"instanceNameFromID", instanceName,
			"nodesInCluster", nodeNames,
			"elapsed", elapsed.String(),
			"hint", "waiting for a node with name equal to NodeClaim name, or label cloudburst.io/nodeclaim, or providerID containing instance name",
		)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	logger.Info("found matching node", "nodeName", matchingNode.Name, "matchReason", matchReason)

	// Apply NodePool template labels (and cloudburst metadata) to the joined node so that
	// scheduler nodeAffinity (e.g. on cloudburst.io/provider) can match this node.
	// Best-effort: failures here should not block the node from becoming Ready.
	if nodeClaim.Spec.NodePoolRef != "" {
		nodePool := &cloudburstv1alpha1.NodePool{}
		nodePoolKey := client.ObjectKey{
			Namespace: nodeClaim.Namespace,
			Name:      nodeClaim.Spec.NodePoolRef,
		}
		if err := r.Get(ctx, nodePoolKey, nodePool); err != nil {
			logger.Error(err, "unable to fetch NodePool for labeling node", "nodePool", nodeClaim.Spec.NodePoolRef)
		} else {
			original := matchingNode.DeepCopy()
			if matchingNode.Labels == nil {
				matchingNode.Labels = make(map[string]string)
			}

			// Copy template labels from NodePool.
			for k, v := range nodePool.Spec.Template.Labels {
				// Don't overwrite existing labels from bootstrap/cloud provider unless explicitly set in template.
				if _, exists := matchingNode.Labels[k]; !exists {
					matchingNode.Labels[k] = v
				}
			}

			// Always attach nodepool and nodeclaim labels for debugging / affinity.
			matchingNode.Labels["cloudburst.io/nodepool"] = nodeClaim.Spec.NodePoolRef
			matchingNode.Labels["cloudburst.io/nodeclaim"] = nodeClaim.Name

			// Ensure provider label is present; prefer NodePool template but fall back to NodeClaim.Spec.Chosen.Provider.
			if _, exists := matchingNode.Labels["cloudburst.io/provider"]; !exists {
				provider := strings.TrimSpace(nodeClaim.Spec.Chosen.Provider)
				if provider != "" {
					matchingNode.Labels["cloudburst.io/provider"] = provider
				}
			}

			if err := r.Patch(ctx, matchingNode, client.MergeFrom(original)); err != nil {
				logger.Error(err, "failed to apply NodePool template labels to node", "nodeName", matchingNode.Name)
			} else {
				logger.Info("applied NodePool template labels to node",
					"nodeName", matchingNode.Name,
					"nodePool", nodeClaim.Spec.NodePoolRef,
					"labels", matchingNode.Labels,
				)
			}
		}
	}

	// Check if node is ready
	var nodeReadyStatus, nodeReadyReason, nodeReadyMessage string
	isReady := false
	for _, condition := range matchingNode.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			nodeReadyStatus = string(condition.Status)
			nodeReadyReason = condition.Reason
			nodeReadyMessage = condition.Message
			if condition.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}
	}

	if !isReady {
		logger.Info("node found but not ready yet, requeuing",
			"nodeName", matchingNode.Name,
			"readyStatus", nodeReadyStatus,
			"readyReason", nodeReadyReason,
			"readyMessage", nodeReadyMessage,
			"elapsed", elapsed.String(),
			"hint", "waiting for NodeReady condition to become True (e.g. CNI, kubelet health)",
		)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Node is ready!
	logger.Info("node is ready, transitioning to Ready",
		"nodeName", matchingNode.Name,
		"elapsed", elapsed,
	)
	r.recordEvent(nodeClaim, corev1.EventTypeNormal, "NodeReady", fmt.Sprintf("Node %s is ready", matchingNode.Name))
	provider := nodeClaim.Spec.Chosen.Provider
	metrics.JoiningDurationSeconds.WithLabelValues(provider).Observe(elapsed.Seconds())
	metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(nodeClaim.Status.Phase), string(cloudburstv1alpha1.NodeClaimPhaseReady)).Inc()
	nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseReady
	nodeClaim.Status.NodeName = matchingNode.Name
	nodeClaim.Status.Timestamps.ReadyAt = &metav1.Time{Time: time.Now()}
	nodeClaim.Status.Timestamps.LastTransition = metav1.Now()

	if err := r.Status().Update(ctx, nodeClaim); err != nil {
		logger.Error(err, "unable to update NodeClaim status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleReady reconciles NodeClaims in Ready phase. It detects when the node has left
// the cluster (e.g. provider failure, VM terminated) and initiates deletion so the
// NodePool can create a replacement. Triggers transition to Deleting when: (1) node
// is NotFound, or (2) node exists but NodeReady is False for longer than
// NodeUnreachableGracePeriod (VM gone, kubelet stopped; node stays NotReady until
// something removes it). Configurable via CLOUDBURST_NODE_UNREACHABLE_GRACE_PERIOD.
func (r *NodeClaimReconciler) handleReady(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	if nodeClaim.Status.NodeName == "" {
		return ctrl.Result{}, nil
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeClaim.Status.NodeName}, node); err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Info("node no longer in cluster, initiating deletion",
				"nodeName", nodeClaim.Status.NodeName,
				"hint", "node may have left due to provider failure; NodeClaim will be cleaned up so NodePool can create a replacement",
			)
			r.recordEvent(nodeClaim, corev1.EventTypeWarning, "NodeLost", fmt.Sprintf("Node %s is no longer in cluster, cleaning up NodeClaim", nodeClaim.Status.NodeName))
			provider := nodeClaim.Spec.Chosen.Provider
			metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(cloudburstv1alpha1.NodeClaimPhaseReady), string(cloudburstv1alpha1.NodeClaimPhaseDeleting)).Inc()
			nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseDeleting
			nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
			if err := r.Status().Update(ctx, nodeClaim); err != nil {
				logger.Error(err, "unable to update NodeClaim status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "unable to fetch node")
		return ctrl.Result{}, err
	}

	// Node exists. Check if it has been NotReady for too long (e.g. VM deleted, kubelet stopped).
	if r.Config != nil && r.Config.NodeUnreachableGracePeriod > 0 {
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
				unreachableDuration := time.Since(c.LastTransitionTime.Time)
				if unreachableDuration >= r.Config.NodeUnreachableGracePeriod {
					logger.Info("node unreachable beyond grace period, initiating deletion",
						"nodeName", node.Name,
						"reason", c.Reason,
						"unreachableDuration", unreachableDuration.String(),
						"gracePeriod", r.Config.NodeUnreachableGracePeriod.String(),
					)
					return r.transitionToDeletingNodeLost(ctx, nodeClaim, logger,
						fmt.Sprintf("Node %s unreachable for %v (VM may be gone)", node.Name, unreachableDuration))
				}
				break
			}
		}
	}

	// Requeue periodically so we can detect if the node leaves or becomes unreachable
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *NodeClaimReconciler) transitionToDeletingNodeLost(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger, hint string) (ctrl.Result, error) {
	logger.Info("initiating deletion", "hint", hint)
	r.recordEvent(nodeClaim, corev1.EventTypeWarning, "NodeLost", fmt.Sprintf("Node lost; cleaning up NodeClaim: %s", hint))
	provider := nodeClaim.Spec.Chosen.Provider
	metrics.NodeClaimPhaseTransitionsTotal.WithLabelValues(provider, string(nodeClaim.Status.Phase), string(cloudburstv1alpha1.NodeClaimPhaseDeleting)).Inc()
	nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseDeleting
	nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
	if err := r.Status().Update(ctx, nodeClaim); err != nil {
		logger.Error(err, "unable to update NodeClaim status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// getTailscaleAuthKey returns an auth key for the NodeClaim. When EphemeralKeyFromAPI is set
// on the NodeClass, it creates a new ephemeral key via the Tailscale API (no key in specs).
// Otherwise it uses AuthKeySecretRef or TAILSCALE_AUTHKEY env.
func (r *NodeClaimReconciler) getTailscaleAuthKey(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, nodeClass *cloudburstv1alpha1.NodeClass, logger logr.Logger) (string, error) {
	// Ephemeral key from API: create a new key per NodeClaim (no auth key in specs)
	if nodeClass.Spec.Tailscale.EphemeralKeyFromAPI {
		if nodeClass.Spec.Tailscale.APITokenSecretRef == nil {
			return "", fmt.Errorf("EphemeralKeyFromAPI requires APITokenSecretRef")
		}
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Namespace: nodeClass.Namespace,
			Name:      nodeClass.Spec.Tailscale.APITokenSecretRef.Name,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return "", fmt.Errorf("failed to get Tailscale API token secret: %w", err)
		}
		keyName := nodeClass.Spec.Tailscale.APITokenSecretRef.Key
		if keyName == "" {
			keyName = "token"
		}
		apiTokenBytes, ok := secret.Data[keyName]
		if !ok {
			return "", fmt.Errorf("key %q not found in secret %s", keyName, secretKey.Name)
		}
		apiToken := string(apiTokenBytes)
		tsClient := tailscale.NewClient(apiToken)
		authKey, err := tsClient.CreateEphemeralKey(ctx, nodeClaim.Name)
		if err != nil {
			return "", fmt.Errorf("create ephemeral Tailscale key: %w", err)
		}
		logger.Info("created ephemeral Tailscale auth key for NodeClaim", "nodeClaim", nodeClaim.Name)
		return authKey, nil
	}

	// Static key from Secret
	if nodeClass.Spec.Tailscale.AuthKeySecretRef != nil {
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Namespace: nodeClass.Namespace,
			Name:      nodeClass.Spec.Tailscale.AuthKeySecretRef.Name,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return "", fmt.Errorf("failed to get Tailscale auth key secret: %w", err)
		}

		keyName := nodeClass.Spec.Tailscale.AuthKeySecretRef.Key
		authKeyBytes, ok := secret.Data[keyName]
		if !ok {
			return "", fmt.Errorf("key %s not found in secret %s", keyName, secretKey.Name)
		}

		return string(authKeyBytes), nil
	}

	// Fallback to environment variable
	authKey := os.Getenv("TAILSCALE_AUTHKEY")
	if authKey == "" {
		return "", fmt.Errorf("Tailscale auth key not found: set EphemeralKeyFromAPI+APITokenSecretRef, AuthKeySecretRef, or TAILSCALE_AUTHKEY")
	}

	return authKey, nil
}

// isInstanceNotFoundError returns true if the error indicates the instance was already deleted (404/not found).
// Treating this as success allows idempotent delete: instance is gone, proceed with finalizer removal.
func isInstanceNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "404") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "was not found") ||
		strings.Contains(s, "could not be found") ||
		strings.Contains(s, "does not exist")
}

// handleDeletion handles NodeClaim deletion (when user deletes the CR; DeletionTimestamp is set).
// Uses the provider-agnostic factory so that instances from any provider are cleaned up.
func (r *NodeClaimReconciler) handleDeletion(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	provider := nodeClaim.Spec.Chosen.Provider
	logger.Info("handling deletion",
		"instanceID", nodeClaim.Status.InstanceID,
		"provider", provider,
		"hint", "will delete cloud instance then remove finalizer so CR can be removed",
	)

	// If instanceID is set, delete the cloud instance via the provider-agnostic factory
	if nodeClaim.Status.InstanceID != "" {
		nodeClass := &cloudburstv1alpha1.NodeClass{}
		nodeClassKey := client.ObjectKey{
			Namespace: nodeClaim.Namespace,
			Name:      nodeClaim.Spec.NodeClassRef,
		}
		if err := r.Get(ctx, nodeClassKey, nodeClass); err != nil {
			logger.Error(err, "unable to fetch NodeClass for deletion")
			// Continue with finalizer removal even if NodeClass is not found
		} else {
			regionOrZone, _, err := getProvisioningParams(provider, nodeClass, nodeClaim.Spec.Chosen.Region)
			if err != nil {
				logger.Error(err, "unable to get provisioning params for deletion")
			} else {
				prov, err := provisioner.NewFromNodeClass(ctx, provider, nodeClass, nodeClaim.Namespace, r, regionOrZone)
				if err != nil {
					logger.Error(err, "unable to create provisioner for deletion")
				} else {
					defer prov.Close()
					if err := prov.DeleteInstance(ctx, nodeClaim.Status.InstanceID); err != nil {
						if isInstanceNotFoundError(err) {
							logger.Info("instance already deleted (404/not found), treating as success",
								"instanceID", nodeClaim.Status.InstanceID, "provider", provider)
							r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceDeleted", fmt.Sprintf("%s instance already gone: %s", provider, nodeClaim.Status.InstanceID))
						} else {
							metrics.ProviderErrorsTotal.WithLabelValues(provider, "delete").Inc()
							logger.Error(err, "failed to delete instance", "instanceID", nodeClaim.Status.InstanceID, "provider", provider)
							r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceDeletionFailed", fmt.Sprintf("Failed to delete %s instance: %v", provider, err))
						}
						// Continue with finalizer removal for non-Aruba-state errors (don't block scale-down)
					} else {
						logger.Info("deleted instance", "instanceID", nodeClaim.Status.InstanceID, "provider", provider)
						r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceDeleted", fmt.Sprintf("%s instance deleted: %s", provider, nodeClaim.Status.InstanceID))
					}
				}
			}
		}
	}

	// Remove finalizer so the API server can delete the NodeClaim CR
	if controllerutil.ContainsFinalizer(nodeClaim, NodeClaimFinalizer) {
		controllerutil.RemoveFinalizer(nodeClaim, NodeClaimFinalizer)
		if err := r.Update(ctx, nodeClaim); err != nil {
			logger.Error(err, "unable to remove finalizer")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// instanceNameForProvider returns the cloud instance name. For GCP, injects "gcp" after "cb-"
// so VMs are identifiable in the GCP console (e.g. cb-gcp-e2e-nodepool-xxx).
// instanceNameForProvider returns the cloud instance name. For all providers, including GCP,
// we use the NodeClaim name as-is so the pattern cb-e2e-<provider>-nodepool is consistent.
func instanceNameForProvider(provider, nodeClaimName string) string {
	return nodeClaimName
}

// instanceNameFromID returns the instance name from an instance ID.
// GCP: projects/.../zones/.../instances/name -> name
// OVH: ovh/projectId/uuid -> uuid (last segment)
func instanceNameFromID(instanceID string) string {
	if instanceID == "" {
		return ""
	}
	parts := strings.Split(instanceID, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return ""
}

// providerIDMatchesInstance returns true if the node's providerID identifies the given instance.
// Handles GCP (providerID contains instance name) and OVH (providerID may contain full
// instance ID "ovh/projectId/uuid" or just the UUID).
func providerIDMatchesInstance(providerID, instanceID string) bool {
	if providerID == "" || instanceID == "" {
		return false
	}
	// providerID often contains the instance identifier (e.g. gce://project/zone/name, ovh://project/uuid)
	if strings.Contains(providerID, instanceID) {
		return true
	}
	// OVH uses UUID-style IDs; CCM may use only the UUID part in providerID
	instanceName := instanceNameFromID(instanceID)
	if instanceName != "" && instanceName != instanceID && strings.Contains(providerID, instanceName) {
		return true
	}
	return false
}

// statusUpdateWithRetry performs Status().Update with retry on conflict (up to 3 attempts).
// Used to avoid 409 when NodePool and NodeClaim controllers reconcile concurrently.
// apply is called before each Update to set the desired status fields.
func (r *NodeClaimReconciler) statusUpdateWithRetry(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, apply func(*cloudburstv1alpha1.NodeClaim), logger logr.Logger) error {
	for attempt := 0; attempt < 3; attempt++ {
		apply(nodeClaim)
		if err := r.Status().Update(ctx, nodeClaim); err != nil {
			if k8serrors.IsConflict(err) && attempt < 2 {
				logger.V(1).Info("status update conflict, re-fetching and retrying", "attempt", attempt+1)
				if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), nodeClaim); err != nil {
					return err
				}
				continue
			}
			return err
		}
		return nil
	}
	return nil
}

// updateStatus updates NodeClaim status with error
func (r *NodeClaimReconciler) updateStatus(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, phase cloudburstv1alpha1.NodeClaimPhase, errorMsg string) {
	nodeClaim.Status.Phase = phase
	nodeClaim.Status.LastError = errorMsg
	nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
	if err := r.Status().Update(ctx, nodeClaim); err != nil {
		log.FromContext(ctx).Error(err, "failed to update NodeClaim status", "phase", phase)
	}
}

// recommendationRequestFromSummary converts NodeClaim RecommendationRequestSummary to CloudBroker request.
func recommendationRequestFromSummary(s *cloudburstv1alpha1.RecommendationRequestSummary) *cloudbroker.RecommendationRequest {
	if s == nil {
		return nil
	}
	arch := s.Arch
	if arch == "" {
		arch = "x86_64" // default when not specified
	}
	return &cloudbroker.RecommendationRequest{
		MinVCPU:                  s.MinVCPU,
		MinRAMGb:                 s.MinRAMGb,
		Arch:                     &arch,
		RegionConstraint:         s.RegionConstraint,
		MaxPriceEurPerHour:       s.MaxPriceEurPerHour,
		Limit:                    10,
		AllowedProviders:         append([]string(nil), s.AllowedProviders...),
		DataSourceProvider:       s.DataSourceProvider,
		DataSourceRegion:         s.DataSourceRegion,
		EstimatedEgressGbPerHour: s.EstimatedEgressGbPerHour,
		MaxTCOEurPerHour:         s.MaxTCOEurPerHour,
	}
}

// sortAlternativesByVCPUAsc sorts InstanceDetails by VCPU ascending.
// Used when VcpuLimitExceeded to try smaller instance types first.
func sortAlternativesByVCPUAsc(alt []cloudburstv1alpha1.InstanceDetails) []cloudburstv1alpha1.InstanceDetails {
	sorted := make([]cloudburstv1alpha1.InstanceDetails, len(alt))
	copy(sorted, alt)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].VCPU < sorted[j].VCPU
	})
	return sorted
}

// recordEvent records a Kubernetes event
func (r *NodeClaimReconciler) recordEvent(nodeClaim *cloudburstv1alpha1.NodeClaim, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(nodeClaim, eventType, reason, message)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudburstv1alpha1.NodeClaim{}).
		Owns(&corev1.Node{}).
		Complete(r)
}
