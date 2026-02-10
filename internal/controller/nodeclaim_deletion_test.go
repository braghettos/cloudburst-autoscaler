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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
)

// MockGCPProvisioner is a mock provisioner for testing
type MockGCPProvisioner struct {
	deleteInstanceCalled bool
	deleteInstanceError  error
}

func (m *MockGCPProvisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	m.deleteInstanceCalled = true
	return m.deleteInstanceError
}

func TestHandleDeleting_DeleteFlow(t *testing.T) {
	scheme := runtime.NewScheme()
	cloudburstv1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	tests := []struct {
		name             string
		nodeClaim        *cloudburstv1alpha1.NodeClaim
		node             *corev1.Node
		pods             []corev1.Pod
		nodeClass        *cloudburstv1alpha1.NodeClass
		expectNodePatch  bool
		expectNodeDelete bool
		expectPodDelete  bool
	}{
		{
			name: "delete flow with node and pods",
			nodeClaim: &cloudburstv1alpha1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-nodeclaim",
					Namespace: "default",
				},
				Spec: cloudburstv1alpha1.NodeClaimSpec{
					NodeClassRef: "test-nodeclass",
				},
				Status: cloudburstv1alpha1.NodeClaimStatus{
					Phase:      cloudburstv1alpha1.NodeClaimPhaseDeleting,
					InstanceID: "projects/test/zones/us-central1-a/instances/test-instance",
					NodeName:   "test-node",
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			},
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
					},
				},
			},
			nodeClass: &cloudburstv1alpha1.NodeClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-nodeclass",
					Namespace: "default",
				},
				Spec: cloudburstv1alpha1.NodeClassSpec{
					GCP: cloudburstv1alpha1.GCPConfig{
						Project: "test-project",
						Zone:    "us-central1-a",
					},
				},
			},
			expectNodePatch:  true,
			expectNodeDelete: true,
			expectPodDelete:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.nodeClaim}
			if tt.node != nil {
				objs = append(objs, tt.node)
			}
			if tt.nodeClass != nil {
				objs = append(objs, tt.nodeClass)
			}
			for i := range tt.pods {
				objs = append(objs, &tt.pods[i])
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

			reconciler := &NodeClaimReconciler{
				Client: fakeClient,
			}

			ctx := context.Background()
			logger := log.Log.WithName("test")

			// Call handleDeleting
			_, err := reconciler.handleDeleting(ctx, tt.nodeClaim, logger)
			if err != nil {
				t.Fatalf("handleDeleting failed: %v", err)
			}

			// Check if node was patched (cordoned)
			if tt.expectNodePatch && tt.node != nil {
				updatedNode := &corev1.Node{}
				if err := fakeClient.Get(ctx, types.NamespacedName{Name: tt.node.Name}, updatedNode); err == nil {
					if !updatedNode.Spec.Unschedulable {
						t.Error("expected node to be cordoned (Unschedulable=true)")
					}
				}
			}

			// Check if pods were deleted
			if tt.expectPodDelete {
				for _, pod := range tt.pods {
					deletedPod := &corev1.Pod{}
					err := fakeClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
					// Pod should be deleted (not found)
					if err == nil {
						t.Errorf("expected pod %s/%s to be deleted", pod.Namespace, pod.Name)
					}
				}
			}
		})
	}
}

func TestCordonAndDrainNode(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: corev1.NodeSpec{
			Unschedulable: false,
		},
	}

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				NodeName: "test-node",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "daemonset-pod",
				Namespace: "kube-system",
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "DaemonSet",
						Name: "test-daemonset",
					},
				},
			},
			Spec: corev1.PodSpec{
				NodeName: "test-node",
			},
		},
	}

	objs := []client.Object{node}
	for i := range pods {
		objs = append(objs, &pods[i])
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	reconciler := &NodeClaimReconciler{
		Client: fakeClient,
	}

	ctx := context.Background()
	logger := log.Log.WithName("test")

	// Call cordonAndDrainNode
	err := reconciler.cordonAndDrainNode(ctx, "test-node", logger)
	if err != nil {
		t.Fatalf("cordonAndDrainNode failed: %v", err)
	}

	// Verify node was cordoned
	updatedNode := &corev1.Node{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "test-node"}, updatedNode); err != nil {
		t.Fatalf("failed to get node: %v", err)
	}

	if !updatedNode.Spec.Unschedulable {
		t.Error("expected node to be cordoned")
	}

	// Verify regular pod was deleted (daemonset pod should remain)
	regularPod := &corev1.Pod{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "test-pod", Namespace: "default"}, regularPod); err == nil {
		t.Error("expected regular pod to be deleted")
	}

	// Daemonset pod should still exist
	daemonsetPod := &corev1.Pod{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "daemonset-pod", Namespace: "kube-system"}, daemonsetPod); err != nil {
		t.Error("expected daemonset pod to remain")
	}
}
