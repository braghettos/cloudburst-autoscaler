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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
	"github.com/diegobraga/cloudburst-autoscaler/internal/gcp"
	"github.com/diegobraga/cloudburst-autoscaler/internal/hetzner"
	"github.com/diegobraga/cloudburst-autoscaler/internal/scaleway"
)

const (
	// DrainTimeout is the timeout for draining a node
	DrainTimeout = 5 * time.Minute
	// DrainGracePeriod is the grace period for pod deletion during drain
	DrainGracePeriod = 30 * time.Second
)

// handleDeleting handles NodeClaims in Deleting phase
func (r *NodeClaimReconciler) handleDeleting(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("handling Deleting phase", "nodeClaim", nodeClaim.Name, "nodeName", nodeClaim.Status.NodeName)

	// Step 1: Cordon and drain node (if node exists)
	if nodeClaim.Status.NodeName != "" {
		if err := r.cordonAndDrainNode(ctx, nodeClaim.Status.NodeName, logger); err != nil {
			logger.Error(err, "failed to cordon and drain node")
			r.recordEvent(nodeClaim, corev1.EventTypeWarning, "DrainFailed", fmt.Sprintf("Failed to drain node: %v", err))
			// Continue with deletion even if drain fails
		} else {
			logger.Info("node cordoned and drained", "nodeName", nodeClaim.Status.NodeName)
			r.recordEvent(nodeClaim, corev1.EventTypeNormal, "NodeDrained", fmt.Sprintf("Node %s drained successfully", nodeClaim.Status.NodeName))
		}
	}

	// Step 2: Delete Kubernetes Node object
	if nodeClaim.Status.NodeName != "" {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodeClaim.Status.NodeName}, node); err == nil {
			if err := r.Delete(ctx, node); err != nil {
				logger.Error(err, "failed to delete node object", "nodeName", nodeClaim.Status.NodeName)
				r.recordEvent(nodeClaim, corev1.EventTypeWarning, "NodeDeletionFailed", fmt.Sprintf("Failed to delete node: %v", err))
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			logger.Info("deleted node object", "nodeName", nodeClaim.Status.NodeName)
			r.recordEvent(nodeClaim, corev1.EventTypeNormal, "NodeDeleted", fmt.Sprintf("Node %s deleted", nodeClaim.Status.NodeName))
		}
	}

	// Step 3: Delete cloud instance (GCP or Scaleway)
	if nodeClaim.Status.InstanceID != "" {
		provider := nodeClaim.Spec.Chosen.Provider
		nodeClass := &cloudburstv1alpha1.NodeClass{}
		nodeClassKey := client.ObjectKey{
			Namespace: nodeClaim.Namespace,
			Name:      nodeClaim.Spec.NodeClassRef,
		}
		if err := r.Get(ctx, nodeClassKey, nodeClass); err == nil {
			switch provider {
			case "gcp":
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
							return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
						}
						logger.Info("deleted GCP instance", "instanceID", nodeClaim.Status.InstanceID)
						r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceDeleted", fmt.Sprintf("GCP instance deleted: %s", nodeClaim.Status.InstanceID))
					}
				}
			case "scaleway":
				if nodeClass.Spec.Scaleway != nil && nodeClass.Spec.Scaleway.Zone != "" {
					// Fetch Scaleway API keys from secret if APIKeySecretRef is provided (same as in provisionScalewayInstance)
					var apiSecretKey, apiAccessKey string
					if nodeClass.Spec.Scaleway.APIKeySecretRef != nil {
						secret := &corev1.Secret{}
						secretKey := client.ObjectKey{
							Namespace: nodeClaim.Namespace,
							Name:      nodeClass.Spec.Scaleway.APIKeySecretRef.Name,
						}
						if err := r.Get(ctx, secretKey, secret); err != nil {
							logger.Error(err, "failed to get Scaleway API key secret for deletion")
							r.recordEvent(nodeClaim, corev1.EventTypeWarning, "SecretNotFound", fmt.Sprintf("Failed to get Scaleway API key secret: %v", err))
							return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
						}

						// Read secret key (required)
						keyName := nodeClass.Spec.Scaleway.APIKeySecretRef.Key
						if keyName == "" {
							keyName = "SCW_SECRET_KEY"
						}
						apiSecretKey = string(secret.Data[keyName])
						if apiSecretKey == "" {
							apiSecretKey = string(secret.Data["secret-key"])
						}
						if apiSecretKey == "" {
							err := fmt.Errorf("Scaleway API key secret %s/%s missing key %s or 'secret-key'", secretKey.Namespace, secretKey.Name, keyName)
							logger.Error(err, "Scaleway API key secret missing key")
							r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InvalidSecret", err.Error())
							return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
						}

						// Read access key (optional, but recommended)
						apiAccessKey = string(secret.Data["SCW_ACCESS_KEY"])
						if apiAccessKey == "" {
							apiAccessKey = string(secret.Data["access-key"])
						}
					}

					// Set env vars for Scaleway SDK (if secret provided)
					if apiSecretKey != "" {
						os.Setenv("SCW_SECRET_KEY", apiSecretKey)
						defer os.Unsetenv("SCW_SECRET_KEY")
					}
					if apiAccessKey != "" {
						os.Setenv("SCW_ACCESS_KEY", apiAccessKey)
						defer os.Unsetenv("SCW_ACCESS_KEY")
					}

					provisioner, err := scaleway.NewProvisioner(ctx, nodeClass.Spec.Scaleway.Zone, nodeClass.Spec.Scaleway.ProjectID, nodeClass.Spec.Scaleway.OrganizationID)
					if err != nil {
						logger.Error(err, "unable to create Scaleway provisioner for deletion")
						r.recordEvent(nodeClaim, corev1.EventTypeWarning, "ProvisionerError", fmt.Sprintf("Failed to create Scaleway provisioner: %v", err))
						return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
					}
					if err := provisioner.DeleteInstance(ctx, nodeClaim.Status.InstanceID); err != nil {
						logger.Error(err, "failed to delete Scaleway instance", "instanceID", nodeClaim.Status.InstanceID)
						r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceDeletionFailed", fmt.Sprintf("Failed to delete instance: %v", err))
						return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
					}
				logger.Info("deleted Scaleway instance", "instanceID", nodeClaim.Status.InstanceID)
				r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceDeleted", fmt.Sprintf("Scaleway instance deleted: %s", nodeClaim.Status.InstanceID))
				}
			case "hetzner":
				if nodeClass.Spec.Hetzner != nil && nodeClass.Spec.Hetzner.Location != "" {
					// Get API token from secret or env
					var apiToken string
					if nodeClass.Spec.Hetzner.APITokenSecretRef != nil {
						secret := &corev1.Secret{}
						secretKey := client.ObjectKey{
							Namespace: nodeClaim.Namespace,
							Name:      nodeClass.Spec.Hetzner.APITokenSecretRef.Name,
						}
						if err := r.Get(ctx, secretKey, secret); err != nil {
							logger.Error(err, "failed to get Hetzner API token secret for deletion")
							r.recordEvent(nodeClaim, corev1.EventTypeWarning, "SecretNotFound", fmt.Sprintf("Failed to get Hetzner API token secret: %v", err))
							return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
						}
						keyName := nodeClass.Spec.Hetzner.APITokenSecretRef.Key
						if keyName == "" {
							keyName = "HETZNER_API_TOKEN"
						}
						apiToken = string(secret.Data[keyName])
						if apiToken == "" {
							err := fmt.Errorf("Hetzner API token secret %s/%s missing key %s", secretKey.Namespace, secretKey.Name, keyName)
							logger.Error(err, "Hetzner API token secret missing key")
							r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InvalidSecret", err.Error())
							return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
						}
					} else {
						apiToken = os.Getenv("HETZNER_API_TOKEN")
					}

					provisioner, err := hetzner.NewProvisioner(ctx, apiToken, nodeClass.Spec.Hetzner.Location)
					if err != nil {
						logger.Error(err, "unable to create Hetzner provisioner for deletion")
						r.recordEvent(nodeClaim, corev1.EventTypeWarning, "ProvisionerError", fmt.Sprintf("Failed to create Hetzner provisioner: %v", err))
						return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
					}
					if err := provisioner.DeleteInstance(ctx, nodeClaim.Status.InstanceID); err != nil {
						logger.Error(err, "failed to delete Hetzner instance", "instanceID", nodeClaim.Status.InstanceID)
						r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceDeletionFailed", fmt.Sprintf("Failed to delete instance: %v", err))
						return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
					}
					logger.Info("deleted Hetzner instance", "instanceID", nodeClaim.Status.InstanceID)
					r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceDeleted", fmt.Sprintf("Hetzner instance deleted: %s", nodeClaim.Status.InstanceID))
				}
			default:
				logger.Error(fmt.Errorf("unsupported provider"), "cannot delete instance", "provider", provider)
			}
		}
	}

	// Step 4: Delete NodeClaim CR
	logger.Info("deleting NodeClaim", "nodeClaim", nodeClaim.Name)
	if err := r.Delete(ctx, nodeClaim); err != nil {
		logger.Error(err, "failed to delete NodeClaim")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// cordonAndDrainNode cordons and drains a Kubernetes node
func (r *NodeClaimReconciler) cordonAndDrainNode(ctx context.Context, nodeName string, logger logr.Logger) error {
	// Get node
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return fmt.Errorf("unable to get node: %w", err)
	}

	// Cordon node (mark as unschedulable)
	if !node.Spec.Unschedulable {
		node.Spec.Unschedulable = true
		if err := r.Update(ctx, node); err != nil {
			return fmt.Errorf("failed to cordon node: %w", err)
		}
		logger.Info("cordoned node", "nodeName", nodeName)
	}

	// Drain node (evict pods)
	if err := r.drainNode(ctx, nodeName, logger); err != nil {
		return fmt.Errorf("failed to drain node: %w", err)
	}

	return nil
}

// drainNode drains pods from a node
func (r *NodeClaimReconciler) drainNode(ctx context.Context, nodeName string, logger logr.Logger) error {
	// List all pods
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	// Evict non-daemonset pods on this node
	for i := range podList.Items {
		pod := &podList.Items[i]
		// Only process pods on this node
		if pod.Spec.NodeName != nodeName {
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
			logger.Info("skipping daemonset pod", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}

		// Delete pod (this will trigger eviction)
		logger.Info("deleting pod for drain", "pod", pod.Name, "namespace", pod.Namespace)
		if err := r.Delete(ctx, pod, client.GracePeriodSeconds(int64(DrainGracePeriod.Seconds()))); err != nil {
			logger.Error(err, "failed to delete pod", "pod", pod.Name, "namespace", pod.Namespace)
			// Continue with other pods
		}
	}

	// Wait for pods to be terminated (simplified - in production you'd poll)
	// For MVP, we'll wait a short time
	time.Sleep(5 * time.Second)

	return nil
}
