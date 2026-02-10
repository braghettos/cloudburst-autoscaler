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
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/diegobraga/cloudburst-autoscaler/internal/gcp"
	"github.com/diegobraga/cloudburst-autoscaler/internal/tailscale"
)

const (
	// NodeClaimFinalizer is the finalizer that blocks CR deletion until the GCP instance is deleted
	NodeClaimFinalizer = "cloudburst.io/nodeclaim-protection"

	// ProvisioningTimeout is the maximum time to wait for instance provisioning
	ProvisioningTimeout = 10 * time.Minute
	// JoiningTimeout is the maximum time to wait for node to join (bootstrap + Tailscale + kubeadm can be slow)
	JoiningTimeout = 20 * time.Minute
	// RequeueInterval is the interval for requeuing during provisioning/joining
	RequeueInterval = 30 * time.Second
)

// NodeClaimReconciler reconciles a NodeClaim object
type NodeClaimReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	Clientset  kubernetes.Interface
	RestConfig *rest.Config
}

//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclaims/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclasses,verbs=get;list;watch
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
		if errors.IsNotFound(err) {
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
		// Already ready, no action needed
		return ctrl.Result{}, nil
	case cloudburstv1alpha1.NodeClaimPhaseDeleting:
		return r.handleDeleting(ctx, nodeClaim, logger)
	case cloudburstv1alpha1.NodeClaimPhaseFailed:
		// Failed state, don't retry automatically
		return ctrl.Result{}, nil
	default:
		// Unknown phase, set to Pending
		logger.Info("unknown phase, setting to Pending", "phase", nodeClaim.Status.Phase)
		nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhasePending
		nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
		return ctrl.Result{}, r.Status().Update(ctx, nodeClaim)
	}
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

	// Get project and zone (env vars with fallback to NodeClass)
	project := gcp.GetProject(nodeClass.Spec.GCP.Project)
	zone := gcp.GetZone(nodeClass.Spec.GCP.Zone)

	if project == "" || zone == "" {
		err := fmt.Errorf("project and zone must be set (env vars or NodeClass)")
		logger.Error(err, "missing GCP configuration")
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

	startupScript := bootstrap.GenerateStartupScript(&bootstrap.StartupScriptConfig{
		NodeName:                  nodeClaim.Name,
		TailscaleAuthKey:          tailscaleAuthKey,
		KubernetesVersion:         nodeClass.Spec.Bootstrap.KubernetesVersion,
		JoinCommand:               joinCommand,
		HostApiServer:             hostApiServer,
		DiscoveryKubeconfigBase64: discoveryKubeconfigBase64,
	})

	// Ensure finalizer so that when the NodeClaim is deleted we get a reconcile and can delete the GCP instance.
	// Use Patch so we only change metadata.finalizers and avoid overwriting status (which would cause a conflict on the later status update).
	if !controllerutil.ContainsFinalizer(nodeClaim, NodeClaimFinalizer) {
		original := nodeClaim.DeepCopy()
		controllerutil.AddFinalizer(nodeClaim, NodeClaimFinalizer)
		if err := r.Patch(ctx, nodeClaim, client.MergeFrom(original)); err != nil {
			logger.Error(err, "unable to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Create provisioner
	provisioner, err := gcp.NewProvisioner(ctx, project, zone)
	if err != nil {
		logger.Error(err, "unable to create GCP provisioner")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "ProvisionerError", fmt.Sprintf("Failed to create provisioner: %v", err))
		return ctrl.Result{}, err
	}
	defer provisioner.Close()

	// Generate instance name
	instanceName := gcp.GenerateInstanceName(nodeClaim.Name)

	// Build machine type from instance details
	machineType := nodeClaim.Spec.Chosen.InstanceType

	// Create instance
	logger.Info("creating GCP instance",
		"instanceName", instanceName,
		"machineType", machineType,
		"zone", zone,
		"project", project,
	)

	r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceCreationStarted", fmt.Sprintf("Creating GCP instance %s", instanceName))

	instanceID, err := provisioner.CreateInstance(ctx, instanceName, machineType, zone, startupScript)
	if err != nil {
		logger.Error(err, "failed to create GCP instance")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceCreationFailed", fmt.Sprintf("Failed to create instance: %v", err))
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, err
	}

	// Update NodeClaim status
	nodeClaim.Status.InstanceID = instanceID
	nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseProvisioning
	nodeClaim.Status.Timestamps.CreatedAt = metav1.Now()
	nodeClaim.Status.Timestamps.LastTransition = metav1.Now()

	if err := r.Status().Update(ctx, nodeClaim); err != nil {
		logger.Error(err, "unable to update NodeClaim status")
		return ctrl.Result{}, err
	}

	logger.Info("instance created, transitioning to Provisioning",
		"instanceID", instanceID,
	)
	r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceCreated", fmt.Sprintf("GCP instance created: %s", instanceID))

	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
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
			return ctrl.Result{RequeueAfter: RequeueInterval}, err
		}
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}

	elapsed := time.Since(nodeClaim.Status.Timestamps.CreatedAt.Time)
	if elapsed > ProvisioningTimeout {
		err := fmt.Errorf("provisioning timeout after %v", elapsed)
		logger.Error(err, "provisioning timeout exceeded")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "ProvisioningTimeout", err.Error())
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, nil
	}

	// Get NodeClass for project/zone
	nodeClass := &cloudburstv1alpha1.NodeClass{}
	nodeClassKey := client.ObjectKey{
		Namespace: nodeClaim.Namespace,
		Name:      nodeClaim.Spec.NodeClassRef,
	}
	if err := r.Get(ctx, nodeClassKey, nodeClass); err != nil {
		logger.Error(err, "unable to fetch NodeClass")
		return ctrl.Result{}, err
	}

	project := gcp.GetProject(nodeClass.Spec.GCP.Project)
	zone := gcp.GetZone(nodeClass.Spec.GCP.Zone)

	// Create provisioner
	provisioner, err := gcp.NewProvisioner(ctx, project, zone)
	if err != nil {
		logger.Error(err, "unable to create GCP provisioner")
		return ctrl.Result{}, err
	}
	defer provisioner.Close()

	// Check if instance is running
	isRunning, err := provisioner.IsInstanceRunning(ctx, nodeClaim.Status.InstanceID)
	if err != nil {
		logger.Error(err, "unable to check instance status")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceStatusCheckFailed", fmt.Sprintf("Failed to check instance status: %v", err))
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}

	if isRunning {
		logger.Info("instance is running, transitioning to Joining")
		r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceRunning", "GCP instance is now running")

		nodeClaim.Status.Phase = cloudburstv1alpha1.NodeClaimPhaseJoining
		nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
		if err := r.Status().Update(ctx, nodeClaim); err != nil {
			logger.Error(err, "unable to update NodeClaim status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}

	// Instance not running yet, requeue
	logger.Info("instance not running yet, requeuing",
		"instanceID", nodeClaim.Status.InstanceID,
		"elapsed", elapsed.String(),
		"hint", "waiting for GCP instance to reach RUNNING state (VM boot + startup script)",
	)
	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
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
			return ctrl.Result{RequeueAfter: RequeueInterval}, err
		}
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}

	elapsed := time.Since(joiningStartTime)
	if elapsed > JoiningTimeout {
		err := fmt.Errorf("joining timeout after %v", elapsed)
		logger.Error(err, "joining timeout exceeded")
		r.recordEvent(nodeClaim, corev1.EventTypeWarning, "JoiningTimeout", err.Error())
		r.updateStatus(ctx, nodeClaim, cloudburstv1alpha1.NodeClaimPhaseFailed, err.Error())
		return ctrl.Result{}, nil
	}

	// The node name should match the NodeClaim name (set as hostname in bootstrap script)
	// The bootstrap script sets --hostname=$NODE_NAME where NODE_NAME=nodeClaim.Name
	expectedNodeName := nodeClaim.Name

	// List nodes and find matching node
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		logger.Error(err, "unable to list nodes")
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
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
		// Fallback: match by GCP provider ID (instance name) in case hostname didn't apply
		if instanceName != "" && node.Spec.ProviderID != "" && strings.Contains(node.Spec.ProviderID, instanceName) {
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

// handleDeletion handles NodeClaim deletion (when user deletes the CR; DeletionTimestamp is set)
func (r *NodeClaimReconciler) handleDeletion(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("handling deletion",
		"instanceID", nodeClaim.Status.InstanceID,
		"hint", "will delete GCP instance then remove finalizer so CR can be removed",
	)

	// If instanceID is set, delete the instance
	if nodeClaim.Status.InstanceID != "" {
		// Get NodeClass for project/zone
		nodeClass := &cloudburstv1alpha1.NodeClass{}
		nodeClassKey := client.ObjectKey{
			Namespace: nodeClaim.Namespace,
			Name:      nodeClaim.Spec.NodeClassRef,
		}
		if err := r.Get(ctx, nodeClassKey, nodeClass); err != nil {
			logger.Error(err, "unable to fetch NodeClass for deletion")
			// Continue with deletion even if NodeClass is not found
		} else {
			project := gcp.GetProject(nodeClass.Spec.GCP.Project)
			zone := gcp.GetZone(nodeClass.Spec.GCP.Zone)

			if project != "" && zone != "" {
				provisioner, err := gcp.NewProvisioner(ctx, project, zone)
				if err != nil {
					logger.Error(err, "unable to create GCP provisioner for deletion")
				} else {
					defer provisioner.Close()
					if err := provisioner.DeleteInstance(ctx, nodeClaim.Status.InstanceID); err != nil {
						logger.Error(err, "failed to delete GCP instance", "instanceID", nodeClaim.Status.InstanceID)
						r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceDeletionFailed", fmt.Sprintf("Failed to delete instance: %v", err))
						// Don't fail deletion if instance deletion fails
					} else {
						logger.Info("deleted GCP instance", "instanceID", nodeClaim.Status.InstanceID)
						r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceDeleted", fmt.Sprintf("GCP instance deleted: %s", nodeClaim.Status.InstanceID))
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

// instanceNameFromID returns the instance name from a GCP instance ID (projects/.../zones/.../instances/name).
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

// updateStatus updates NodeClaim status with error
func (r *NodeClaimReconciler) updateStatus(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, phase cloudburstv1alpha1.NodeClaimPhase, errorMsg string) {
	nodeClaim.Status.Phase = phase
	nodeClaim.Status.LastError = errorMsg
	nodeClaim.Status.Timestamps.LastTransition = metav1.Now()
	if err := r.Status().Update(ctx, nodeClaim); err != nil {
		log.FromContext(ctx).Error(err, "failed to update NodeClaim status", "phase", phase)
	}
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
