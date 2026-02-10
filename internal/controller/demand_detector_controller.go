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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/diegobraga/cloudburst-autoscaler/internal/planner"
)

const (
	// IgnoreAnnotation is the annotation that marks pods to be ignored by the demand detector
	IgnoreAnnotation = "cloudburst.io/ignore"
	// KubeSystemNamespace is the namespace to ignore
	KubeSystemNamespace = "kube-system"
)

// DemandDetectorReconciler reconciles Pods to detect unschedulable demand
type DemandDetectorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile processes Pod events to detect unschedulable demand
func (r *DemandDetectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// List all pods in the cluster
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		logger.Error(err, "unable to list pods")
		return ctrl.Result{}, err
	}

	// Filter eligible pods
	eligiblePods := r.filterEligiblePods(podList.Items)

	// Calculate desired capacity using the planner
	desiredCapacity := planner.CalculateDesiredCapacity(eligiblePods)

	// Log the desired capacity
	logger.Info("calculated desired capacity",
		"cpuMilliTotal", desiredCapacity.CPUMilliTotal,
		"memBytesTotal", desiredCapacity.MemBytesTotal,
		"podCount", desiredCapacity.PodCount,
	)

	return ctrl.Result{}, nil
}

// filterEligiblePods filters pods that are eligible for demand detection
func (r *DemandDetectorReconciler) filterEligiblePods(pods []corev1.Pod) []corev1.Pod {
	var eligible []corev1.Pod

	for _, pod := range pods {
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
			eligible = append(eligible, pod)
		}
	}

	return eligible
}

// SetupWithManager sets up the controller with the Manager.
func (r *DemandDetectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
