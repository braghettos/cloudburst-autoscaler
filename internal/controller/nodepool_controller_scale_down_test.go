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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
)

func TestCheckScaleDown_TTLExpiry(t *testing.T) {
	scheme := runtime.NewScheme()
	cloudburstv1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	tests := []struct {
		name           string
		nodePool       *cloudburstv1alpha1.NodePool
		nodeClaims     []cloudburstv1alpha1.NodeClaim
		expectDeletion bool
	}{
		{
			name: "node expired - should delete",
			nodePool: &cloudburstv1alpha1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-nodepool",
					Namespace: "default",
				},
				Spec: cloudburstv1alpha1.NodePoolSpec{
					Disruption: cloudburstv1alpha1.NodePoolDisruption{
						TTLSecondsUntilExpired: 300, // 5 minutes
					},
				},
			},
			nodeClaims: []cloudburstv1alpha1.NodeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-nodeclaim-1",
						Namespace: "default",
					},
					Spec: cloudburstv1alpha1.NodeClaimSpec{
						NodePoolRef: "test-nodepool",
					},
					Status: cloudburstv1alpha1.NodeClaimStatus{
						Phase: cloudburstv1alpha1.NodeClaimPhaseReady,
						Timestamps: cloudburstv1alpha1.NodeClaimTimestamps{
							ReadyAt: &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}, // Expired
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-nodeclaim-2",
						Namespace: "default",
					},
					Spec: cloudburstv1alpha1.NodeClaimSpec{
						NodePoolRef: "test-nodepool",
					},
					Status: cloudburstv1alpha1.NodeClaimStatus{
						Phase: cloudburstv1alpha1.NodeClaimPhaseReady,
						Timestamps: cloudburstv1alpha1.NodeClaimTimestamps{
							ReadyAt: &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}, // Not expired
						},
					},
				},
			},
			expectDeletion: true,
		},
		{
			name: "node not expired - should not delete",
			nodePool: &cloudburstv1alpha1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-nodepool",
					Namespace: "default",
				},
				Spec: cloudburstv1alpha1.NodePoolSpec{
					Disruption: cloudburstv1alpha1.NodePoolDisruption{
						TTLSecondsUntilExpired: 300, // 5 minutes
					},
				},
			},
			nodeClaims: []cloudburstv1alpha1.NodeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-nodeclaim-1",
						Namespace: "default",
					},
					Spec: cloudburstv1alpha1.NodeClaimSpec{
						NodePoolRef: "test-nodepool",
					},
					Status: cloudburstv1alpha1.NodeClaimStatus{
						Phase: cloudburstv1alpha1.NodeClaimPhaseReady,
						Timestamps: cloudburstv1alpha1.NodeClaimTimestamps{
							ReadyAt: &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}, // Not expired
						},
					},
				},
			},
			expectDeletion: false,
		},
		{
			name: "minimum node count - should not delete",
			nodePool: &cloudburstv1alpha1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-nodepool",
					Namespace: "default",
				},
				Spec: cloudburstv1alpha1.NodePoolSpec{
					Disruption: cloudburstv1alpha1.NodePoolDisruption{
						TTLSecondsUntilExpired: 300,
					},
				},
			},
			nodeClaims: []cloudburstv1alpha1.NodeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-nodeclaim-1",
						Namespace: "default",
					},
					Spec: cloudburstv1alpha1.NodeClaimSpec{
						NodePoolRef: "test-nodepool",
					},
					Status: cloudburstv1alpha1.NodeClaimStatus{
						Phase: cloudburstv1alpha1.NodeClaimPhaseReady,
						Timestamps: cloudburstv1alpha1.NodeClaimTimestamps{
							ReadyAt: &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}, // Expired
						},
					},
				},
			},
			expectDeletion: false, // Only 1 node, should not delete
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client
			objs := []client.Object{tt.nodePool}
			for i := range tt.nodeClaims {
				objs = append(objs, &tt.nodeClaims[i])
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

			reconciler := &NodePoolReconciler{
				Client: fakeClient,
			}

			ctx := context.Background()
			logger := log.Log.WithName("test")

			// Call checkScaleDown
			err := reconciler.checkScaleDown(ctx, tt.nodePool, logger)
			if err != nil {
				t.Fatalf("checkScaleDown failed: %v", err)
			}

			// Check if deletion was initiated
			// Find expired NodeClaims and verify they were marked for deletion
			expiredFound := false
			for _, claim := range tt.nodeClaims {
				updatedClaim := &cloudburstv1alpha1.NodeClaim{}
				if err := fakeClient.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, updatedClaim); err != nil {
					continue
				}

				// Check if this claim is expired (older than TTL)
				isExpired := false
				if updatedClaim.Status.Timestamps.ReadyAt != nil {
					age := time.Since(updatedClaim.Status.Timestamps.ReadyAt.Time)
					ttl := time.Duration(tt.nodePool.Spec.Disruption.TTLSecondsUntilExpired) * time.Second
					if age >= ttl {
						isExpired = true
					}
				}

				if isExpired {
					expiredFound = true
					if tt.expectDeletion {
						if updatedClaim.Status.Phase != cloudburstv1alpha1.NodeClaimPhaseDeleting {
							t.Errorf("expected expired NodeClaim %s to be in Deleting phase, got %s", claim.Name, updatedClaim.Status.Phase)
						}
					}
				} else {
					// Non-expired claims should not be deleted
					if updatedClaim.Status.Phase == cloudburstv1alpha1.NodeClaimPhaseDeleting {
						t.Errorf("expected non-expired NodeClaim %s not to be in Deleting phase, but it is", claim.Name)
					}
				}
			}

			if tt.expectDeletion && !expiredFound {
				t.Error("expected to find expired NodeClaim but none found")
			}
		})
	}
}

func TestIsNodeEmpty(t *testing.T) {
	scheme := runtime.NewScheme()
	cloudburstv1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)

	tests := []struct {
		name      string
		nodeClaim *cloudburstv1alpha1.NodeClaim
		pods      []corev1.Pod
		expected  bool
	}{
		{
			name: "node with no pods",
			nodeClaim: &cloudburstv1alpha1.NodeClaim{
				Status: cloudburstv1alpha1.NodeClaimStatus{
					NodeName: "test-node",
				},
			},
			pods:     []corev1.Pod{},
			expected: true,
		},
		{
			name: "node with only daemonset pods",
			nodeClaim: &cloudburstv1alpha1.NodeClaim{
				Status: cloudburstv1alpha1.NodeClaimStatus{
					NodeName: "test-node",
				},
			},
			pods: []corev1.Pod{
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
			},
			expected: true,
		},
		{
			name: "node with non-daemonset pod",
			nodeClaim: &cloudburstv1alpha1.NodeClaim{
				Status: cloudburstv1alpha1.NodeClaimStatus{
					NodeName: "test-node",
				},
			},
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "regular-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.nodeClaim}
			for i := range tt.pods {
				objs = append(objs, &tt.pods[i])
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

			reconciler := &NodePoolReconciler{
				Client: fakeClient,
			}

			ctx := context.Background()
			logger := log.Log.WithName("test")

			result, err := reconciler.isNodeEmpty(ctx, tt.nodeClaim, logger)
			if err != nil {
				t.Fatalf("isNodeEmpty failed: %v", err)
			}

			if result != tt.expected {
				t.Errorf("expected isEmpty=%v, got %v", tt.expected, result)
			}
		})
	}
}
