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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
	"github.com/diegobraga/cloudburst-autoscaler/internal/gcp"
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

	// Step 3: Delete GCP instance
	if nodeClaim.Status.InstanceID != "" {
		nodeClass := &cloudburstv1alpha1.NodeClass{}
		nodeClassKey := client.ObjectKey{
			Namespace: nodeClaim.Namespace,
			Name:      nodeClaim.Spec.NodeClassRef,
		}
		if err := r.Get(ctx, nodeClassKey, nodeClass); err == nil {
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
	for _, pod := range podList.Items {
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
		if err := r.Delete(ctx, &pod, client.GracePeriodSeconds(int64(DrainGracePeriod.Seconds()))); err != nil {
			logger.Error(err, "failed to delete pod", "pod", pod.Name, "namespace", pod.Namespace)
			// Continue with other pods
		}
	}

	// Wait for pods to be terminated (simplified - in production you'd poll)
	// For MVP, we'll wait a short time
	time.Sleep(5 * time.Second)

	return nil
}
