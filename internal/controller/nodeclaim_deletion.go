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
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
	"github.com/diegobraga/cloudburst-autoscaler/internal/metrics"
	"github.com/diegobraga/cloudburst-autoscaler/internal/provisioner"
)

// handleDeleting handles NodeClaims in Deleting phase
func (r *NodeClaimReconciler) handleDeleting(ctx context.Context, nodeClaim *cloudburstv1alpha1.NodeClaim, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("handling Deleting phase", "nodeClaim", nodeClaim.Name, "nodeName", nodeClaim.Status.NodeName)

	// Track if the node was already gone (e.g. provider failure). When true, we proceed
	// past instance deletion failure to unblock disaster recovery; the NodePool can
	// create a replacement on another provider.
	nodeGone := false

	// Step 1: Cordon and drain node (if node exists)
	if nodeClaim.Status.NodeName != "" {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodeClaim.Status.NodeName}, node); err != nil {
			if errors.IsNotFound(err) {
				nodeGone = true
				logger.Info("node already gone, skipping drain", "nodeName", nodeClaim.Status.NodeName)
			} else {
				logger.Error(err, "unable to get node for drain")
				return ctrl.Result{}, err
			}
		} else {
			drainStart := time.Now()
			if err := r.cordonAndDrainNode(ctx, nodeClaim.Status.NodeName, logger); err != nil {
				logger.Error(err, "failed to cordon and drain node")
				metrics.DrainDurationSeconds.Observe(time.Since(drainStart).Seconds())
				r.recordEvent(nodeClaim, corev1.EventTypeWarning, "DrainFailed", fmt.Sprintf("Failed to drain node: %v", err))
				// Continue with deletion even if drain fails
			} else {
				metrics.DrainDurationSeconds.Observe(time.Since(drainStart).Seconds())
				logger.Info("node cordoned and drained", "nodeName", nodeClaim.Status.NodeName)
				r.recordEvent(nodeClaim, corev1.EventTypeNormal, "NodeDrained", fmt.Sprintf("Node %s drained successfully", nodeClaim.Status.NodeName))
			}
		}
	}

	// Step 2: Delete Kubernetes Node object
	if nodeClaim.Status.NodeName != "" && !nodeGone {
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

	// Step 3: Delete cloud instance
	if nodeClaim.Status.InstanceID != "" {
		provider := nodeClaim.Spec.Chosen.Provider
		nodeClass := &cloudburstv1alpha1.NodeClass{}
		nodeClassKey := client.ObjectKey{
			Namespace: nodeClaim.Namespace,
			Name:      nodeClaim.Spec.NodeClassRef,
		}
		if err := r.Get(ctx, nodeClassKey, nodeClass); err != nil {
			// nodeClass not found, skip instance deletion
		} else {
			regionOrZone, _, err := getProvisioningParams(provider, nodeClass, nodeClaim.Spec.Chosen.Region)
			if err != nil {
				logger.Error(err, "unable to get provisioning params for deletion")
			} else {
				prov, err := provisioner.NewFromNodeClass(ctx, provider, nodeClass, nodeClaim.Namespace, r, regionOrZone)
				if err != nil {
					logger.Error(err, "unable to create provisioner for deletion")
					r.recordEvent(nodeClaim, corev1.EventTypeWarning, "ProvisionerError", fmt.Sprintf("Failed to create provisioner: %v", err))
					if nodeGone {
						logger.Info("node was gone, proceeding to remove NodeClaim despite provisioner error")
					} else {
						return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
					}
				} else {
					defer prov.Close()
					if stopper, ok := prov.(provisioner.Stopper); ok {
						if err := stopper.StopInstance(ctx, nodeClaim.Status.InstanceID); err != nil {
							logger.Error(err, "failed to stop instance before delete", "instanceID", nodeClaim.Status.InstanceID)
						}
					}
					if err := prov.DeleteInstance(ctx, nodeClaim.Status.InstanceID); err != nil {
						metrics.ProviderErrorsTotal.WithLabelValues(provider, "delete").Inc()
						logger.Error(err, "failed to delete instance", "instanceID", nodeClaim.Status.InstanceID)
						r.recordEvent(nodeClaim, corev1.EventTypeWarning, "InstanceDeletionFailed", fmt.Sprintf("Failed to delete instance: %v", err))
						if nodeGone {
							logger.Info("node was gone, proceeding to remove NodeClaim despite instance deletion failure (provider may be unreachable)")
						} else {
							return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
						}
					} else {
						logger.Info("deleted instance", "instanceID", nodeClaim.Status.InstanceID, "provider", provider)
						r.recordEvent(nodeClaim, corev1.EventTypeNormal, "InstanceDeleted", fmt.Sprintf("Instance deleted: %s", nodeClaim.Status.InstanceID))
					}
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

// drainNode drains pods from a node using the eviction API (respects PDBs) and polls until pods are gone or DrainTimeout.
func (r *NodeClaimReconciler) drainNode(ctx context.Context, nodeName string, logger logr.Logger) error {
	if r.Config == nil {
		return fmt.Errorf("controller config is nil")
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	graceSeconds := int64(r.Config.DrainGracePeriod.Seconds())
	var evictedPods []types.NamespacedName

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName {
			continue
		}
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

		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			DeleteOptions: &metav1.DeleteOptions{
				GracePeriodSeconds: &graceSeconds,
			},
		}
		logger.Info("evicting pod for drain", "pod", pod.Name, "namespace", pod.Namespace)
		if err := r.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "failed to evict pod", "pod", pod.Name, "namespace", pod.Namespace)
			}
			continue
		}
		evictedPods = append(evictedPods, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
	}

	if len(evictedPods) == 0 {
		return nil
	}

	deadline := time.Now().Add(r.Config.DrainTimeout)
	for time.Now().Before(deadline) {
		allGone := true
		for _, nn := range evictedPods {
			p := &corev1.Pod{}
			err := r.Get(ctx, nn, p)
			if err == nil {
				allGone = false
				break
			}
			if !errors.IsNotFound(err) {
				allGone = false
				break
			}
		}
		if allGone {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.Config.DrainPollInterval):
		}
	}
	return fmt.Errorf("drain did not complete within %v: %d pod(s) still terminating", r.Config.DrainTimeout, len(evictedPods))
}
