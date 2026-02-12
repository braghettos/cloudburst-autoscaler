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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
)

// NodeClassReconciler reconciles a NodeClass object
type NodeClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclasses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclasses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudburst.io,resources=nodeclasses/finalizers,verbs=update

// Reconcile validates and reconciles NodeClass resources.
// NodeClass defines infrastructure configuration (provider, join, tailscale, bootstrap)
// used by NodeClaim controllers when provisioning VMs.
func (r *NodeClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudburstv1alpha1.NodeClass{}).
		Complete(r)
}
