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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
	"github.com/diegobraga/cloudburst-autoscaler/internal/cloudbroker"
)

func TestNodePoolController_CreateNodeClaims(t *testing.T) {
	// Setup logging
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	// Setup CloudBroker test server
	cloudBrokerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/recommendations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		response := cloudbroker.RecommendationsResponse{
			Recommendations: []cloudbroker.Recommendation{
				{
					Provider:        "gcp",
					Region:          "europe-west1",
					InstanceType:    "n1-standard-2",
					VCPU:            2,
					RAMGb:           7.5,
					PriceEurPerHour: 0.10,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}))
	defer cloudBrokerServer.Close()

	// Set environment variable to point to test server
	originalBaseURL := os.Getenv("CLOUDBROKER_BASEURL")
	os.Setenv("CLOUDBROKER_BASEURL", cloudBrokerServer.URL)
	defer func() {
		if originalBaseURL != "" {
			os.Setenv("CLOUDBROKER_BASEURL", originalBaseURL)
		} else {
			os.Unsetenv("CLOUDBROKER_BASEURL")
		}
	}()

	// Setup envtest
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: false,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start test environment: %v", err)
	}
	defer func() { _ = testEnv.Stop() }()

	// Add our API types to the scheme
	err = cloudburstv1alpha1.AddToScheme(scheme.Scheme)
	if err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	// Create k8s client
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("failed to create k8s client: %v", err)
	}

	// Create manager
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	// Create CloudBroker client
	cbClient := cloudbroker.NewClient()

	// Create and setup controller
	reconciler := &NodePoolReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		CloudBroker: cbClient,
		Recorder:    mgr.GetEventRecorderFor("nodepool-controller"),
	}
	err = reconciler.SetupWithManager(mgr)
	if err != nil {
		t.Fatalf("failed to setup controller: %v", err)
	}

	// Start manager in goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager error: %v", err)
		}
	}()

	// Wait for manager to be ready
	time.Sleep(100 * time.Millisecond)

	// Create namespace
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}
	if err := k8sClient.Create(ctx, namespace); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, namespace) }()

	// Create NodeClass
	nodeClass := &cloudburstv1alpha1.NodeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeclass",
			Namespace: namespace.Name,
		},
		Spec: cloudburstv1alpha1.NodeClassSpec{
			GCP: cloudburstv1alpha1.GCPConfig{
				Project:      "test-project",
				Zone:         "europe-west1-a",
				ImageFamily:  "ubuntu-2204-lts",
				ImageProject: "ubuntu-os-cloud",
			},
			Join: cloudburstv1alpha1.JoinConfig{
				HostApiServer:             "https://example.com:6443",
				KindControlPlaneContainer: "kind-control-plane",
				TokenTtlMinutes:           30,
			},
			Tailscale: cloudburstv1alpha1.TailscaleConfig{},
		},
	}
	if err := k8sClient.Create(ctx, nodeClass); err != nil {
		t.Fatalf("failed to create NodeClass: %v", err)
	}

	// Create NodePool
	nodePool := &cloudburstv1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodepool",
			Namespace: namespace.Name,
		},
		Spec: cloudburstv1alpha1.NodePoolSpec{
			Requirements: cloudburstv1alpha1.NodePoolRequirements{
				RegionConstraint:   "EU",
				Arch:               []string{"x86_64"},
				MaxPriceEurPerHour: 0.15,
			},
			Limits: cloudburstv1alpha1.NodePoolLimits{
				MaxNodes: 10,
			},
			Template: cloudburstv1alpha1.NodePoolTemplate{},
			Disruption: cloudburstv1alpha1.NodePoolDisruption{
				TTLSecondsAfterEmpty:   300,
				TTLSecondsUntilExpired: 3600,
			},
			Weight: 1,
		},
	}
	if err := k8sClient.Create(ctx, nodePool); err != nil {
		t.Fatalf("failed to create NodePool: %v", err)
	}

	// Create an unschedulable pod.
	// Status must be set via a separate Status().Update() call because the
	// API server ignores the status field during creation.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: namespace.Name,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("3000m"),
							corev1.ResourceMemory: resource.MustParse("10Gi"),
						},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	// Set pod status to Pending + Unschedulable
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{
			{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionFalse,
				Reason: "Unschedulable",
			},
		},
	}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	// Trigger reconciliation by updating NodePool
	// (In a real scenario, the controller would watch for pod changes)
	updatedNodePool := nodePool.DeepCopy()
	updatedNodePool.Labels = map[string]string{"reconcile": "trigger"}
	if err := k8sClient.Update(ctx, updatedNodePool); err != nil {
		t.Fatalf("failed to update NodePool: %v", err)
	}

	// Wait for NodeClaim to be created
	var nodeClaimList cloudburstv1alpha1.NodeClaimList
	maxWait := 10 * time.Second
	checkInterval := 500 * time.Millisecond
	deadline := time.Now().Add(maxWait)

	for time.Now().Before(deadline) {
		if err := k8sClient.List(ctx, &nodeClaimList, client.InNamespace(namespace.Name)); err == nil {
			if len(nodeClaimList.Items) > 0 {
				break
			}
		}
		time.Sleep(checkInterval)
	}

	if len(nodeClaimList.Items) == 0 {
		t.Fatal("expected NodeClaim to be created, but none found")
	}

	// Verify NodeClaim was created with correct spec
	nodeClaim := nodeClaimList.Items[0]
	if nodeClaim.Spec.NodePoolRef != nodePool.Name {
		t.Errorf("expected NodePoolRef %s, got %s", nodePool.Name, nodeClaim.Spec.NodePoolRef)
	}
	if nodeClaim.Spec.NodeClassRef != nodeClass.Name {
		t.Errorf("expected NodeClassRef %s, got %s", nodeClass.Name, nodeClaim.Spec.NodeClassRef)
	}
	if nodeClaim.Spec.Chosen.Provider != "gcp" {
		t.Errorf("expected provider gcp, got %s", nodeClaim.Spec.Chosen.Provider)
	}
	if nodeClaim.Spec.Chosen.Region != "europe-west1" {
		t.Errorf("expected region europe-west1, got %s", nodeClaim.Spec.Chosen.Region)
	}
	if nodeClaim.Spec.Chosen.InstanceType != "n1-standard-2" {
		t.Errorf("expected instance type n1-standard-2, got %s", nodeClaim.Spec.Chosen.InstanceType)
	}
	if nodeClaim.Status.Phase != cloudburstv1alpha1.NodeClaimPhasePending {
		t.Errorf("expected phase Pending, got %s", nodeClaim.Status.Phase)
	}
}
