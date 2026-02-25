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

package planner

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCalculateDesiredCapacityForPod(t *testing.T) {
	tests := []struct {
		name           string
		pod            corev1.Pod
		expectedCPU    int64
		expectedMemory int64
	}{
		{
			name: "single container with CPU and memory",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
			expectedCPU:    100,
			expectedMemory: 134217728, // 128Mi in bytes
		},
		{
			name: "multiple containers - sum resources",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("300m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
				},
			},
			expectedCPU:    500,       // 200m + 300m
			expectedMemory: 805306368, // 256Mi + 512Mi = 768Mi in bytes
		},
		{
			name: "init container with higher requests",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
			expectedCPU:    500,        // max(500m init, 200m container) = 500m
			expectedMemory: 1073741824, // max(1Gi init, 256Mi container) = 1Gi
		},
		{
			name: "init container with lower requests than containers",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("300m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
				},
			},
			expectedCPU:    300,       // max(100m init, 300m container) = 300m
			expectedMemory: 536870912, // max(128Mi init, 512Mi container) = 512Mi
		},
		{
			name: "multiple init containers - take max",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("400m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
			expectedCPU:    400,       // max(200m, 400m init, 100m container) = 400m
			expectedMemory: 536870912, // max(256Mi, 512Mi init, 128Mi container) = 512Mi
		},
		{
			name: "pod with no resource requests",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{},
					},
				},
			},
			expectedCPU:    0,
			expectedMemory: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpuMilli, memBytes := CalculateDesiredCapacityForPod(&tt.pod)
			if cpuMilli != tt.expectedCPU {
				t.Errorf("expected CPU %d millicores, got %d", tt.expectedCPU, cpuMilli)
			}
			if memBytes != tt.expectedMemory {
				t.Errorf("expected Memory %d bytes, got %d", tt.expectedMemory, memBytes)
			}
		})
	}
}

func TestCalculateDesiredCapacity(t *testing.T) {
	tests := []struct {
		name             string
		pods             []corev1.Pod
		expectedCPU      int64
		expectedMemory   int64
		expectedPodCount int
	}{
		{
			name:             "empty pod list",
			pods:             []corev1.Pod{},
			expectedCPU:      0,
			expectedMemory:   0,
			expectedPodCount: 0,
		},
		{
			name: "single pod",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							},
						},
					},
				},
			},
			expectedCPU:      100,
			expectedMemory:   134217728,
			expectedPodCount: 1,
		},
		{
			name: "multiple pods - sum resources",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("200m"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("300m"),
										corev1.ResourceMemory: resource.MustParse("512Mi"),
									},
								},
							},
						},
					},
				},
			},
			expectedCPU:      500,       // 200m + 300m
			expectedMemory:   805306368, // 256Mi + 512Mi
			expectedPodCount: 2,
		},
		{
			name: "pods with init containers",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1"},
					Spec: corev1.PodSpec{
						InitContainers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("500m"),
										corev1.ResourceMemory: resource.MustParse("1Gi"),
									},
								},
							},
						},
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("200m"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							},
						},
					},
				},
			},
			expectedCPU:      600,        // max(500m init, 200m container) + 100m = 500m + 100m
			expectedMemory:   1207959552, // max(1Gi init, 256Mi container) + 128Mi = 1073741824 + 134217728
			expectedPodCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capacity := CalculateDesiredCapacity(tt.pods)
			if capacity.CPUMilliTotal != tt.expectedCPU {
				t.Errorf("expected CPU %d millicores, got %d", tt.expectedCPU, capacity.CPUMilliTotal)
			}
			if capacity.MemBytesTotal != tt.expectedMemory {
				t.Errorf("expected Memory %d bytes, got %d", tt.expectedMemory, capacity.MemBytesTotal)
			}
			if capacity.PodCount != tt.expectedPodCount {
				t.Errorf("expected PodCount %d, got %d", tt.expectedPodCount, capacity.PodCount)
			}
		})
	}
}

func TestExtractArchFromPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      corev1.Pod
		expected string
	}{
		{
			name: "no architecture requirement",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{},
			},
			expected: "",
		},
		{
			name: "architecture from nodeSelector - amd64",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/arch": "amd64",
					},
				},
			},
			expected: "amd64", // Raw value, CloudBroker will normalize to x86_64
		},
		{
			name: "architecture from nodeSelector - x86_64",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/arch": "x86_64",
					},
				},
			},
			expected: "x86_64",
		},
		{
			name: "architecture from nodeSelector - arm64",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/arch": "arm64",
					},
				},
			},
			expected: "arm64",
		},
		{
			name: "architecture from nodeSelector - aarch64",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/arch": "aarch64",
					},
				},
			},
			expected: "aarch64", // Raw value, CloudBroker will normalize to arm64
		},
		{
			name: "architecture from required nodeAffinity",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "kubernetes.io/arch",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"arm64"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "arm64",
		},
		{
			name: "architecture from preferred nodeAffinity",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
								{
									Weight: 100,
									Preference: corev1.NodeSelectorTerm{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "kubernetes.io/arch",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"amd64"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "amd64", // Raw value, CloudBroker will normalize to x86_64
		},
		{
			name: "nodeSelector takes precedence over nodeAffinity",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/arch": "arm64",
					},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "kubernetes.io/arch",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"amd64"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "arm64", // nodeSelector should be checked first
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractArchFromPod(&tt.pod)
			if result != tt.expected {
				t.Errorf("expected architecture %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestCalculateDesiredCapacityWithArch(t *testing.T) {
	tests := []struct {
		name           string
		pods           []corev1.Pod
		expectedArch   string
		expectedCPU    int64
		expectedMemory int64
	}{
		{
			name:           "no pods - no arch",
			pods:           []corev1.Pod{},
			expectedArch:   "",
			expectedCPU:    0,
			expectedMemory: 0,
		},
		{
			name: "pod with arch requirement",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1"},
					Spec: corev1.PodSpec{
						NodeSelector: map[string]string{
							"kubernetes.io/arch": "arm64",
						},
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							},
						},
					},
				},
			},
			expectedArch:   "arm64",
			expectedCPU:    100,
			expectedMemory: 134217728,
		},
		{
			name: "multiple pods - first arch wins",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1"},
					Spec: corev1.PodSpec{
						NodeSelector: map[string]string{
							"kubernetes.io/arch": "amd64",
						},
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("100m"),
									},
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2"},
					Spec: corev1.PodSpec{
						NodeSelector: map[string]string{
							"kubernetes.io/arch": "arm64",
						},
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("200m"),
									},
								},
							},
						},
					},
				},
			},
			expectedArch:   "amd64", // First pod's arch (raw value, CloudBroker will normalize)
			expectedCPU:    300,
			expectedMemory: 0,
		},
		{
			name: "pod without arch requirement",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("100m"),
									},
								},
							},
						},
					},
				},
			},
			expectedArch:   "", // No arch requirement
			expectedCPU:    100,
			expectedMemory: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capacity := CalculateDesiredCapacity(tt.pods)
			if capacity.Arch != tt.expectedArch {
				t.Errorf("expected Arch %q, got %q", tt.expectedArch, capacity.Arch)
			}
			if capacity.CPUMilliTotal != tt.expectedCPU {
				t.Errorf("expected CPU %d millicores, got %d", tt.expectedCPU, capacity.CPUMilliTotal)
			}
			if capacity.MemBytesTotal != tt.expectedMemory {
				t.Errorf("expected Memory %d bytes, got %d", tt.expectedMemory, capacity.MemBytesTotal)
			}
		})
	}
}

func TestPodHasProviderOrNodePoolConstraint(t *testing.T) {
	tests := []struct {
		name     string
		pod      corev1.Pod
		expected bool
	}{
		{
			name:     "no constraints",
			pod:      corev1.Pod{Spec: corev1.PodSpec{}},
			expected: false,
		},
		{
			name: "nodeSelector cloudburst.io/provider",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"cloudburst.io/provider": "gcp"},
				},
			},
			expected: true,
		},
		{
			name: "nodeSelector cloudburst.io/nodepool",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"cloudburst.io/nodepool": "my-pool"},
				},
			},
			expected: true,
		},
		{
			name: "nodeAffinity required cloudburst.io/provider",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{Key: "cloudburst.io/provider", Operator: corev1.NodeSelectorOpIn, Values: []string{"scaleway"}},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "nodeAffinity with only kubernetes.io/arch - no provider constraint",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := PodHasProviderOrNodePoolConstraint(&tt.pod)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestAddProviderNodeAffinity(t *testing.T) {
	tests := []struct {
		name           string
		pod            corev1.Pod
		provider       string
		expectProvider string
	}{
		{
			name:           "pod with no affinity",
			pod:            corev1.Pod{Spec: corev1.PodSpec{}},
			provider:       "scaleway",
			expectProvider: "scaleway",
		},
		{
			name: "pod with existing nodeAffinity for arch",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
										},
									},
								},
							},
						},
					},
				},
			},
			provider:       "hetzner",
			expectProvider: "hetzner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			affinity := AddProviderNodeAffinity(&tt.pod, tt.provider)
			if affinity == nil || affinity.NodeAffinity == nil {
				t.Fatal("expected non-nil affinity with NodeAffinity")
			}
			required := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			if required == nil || len(required.NodeSelectorTerms) == 0 {
				t.Fatal("expected required NodeSelectorTerms")
			}
			found := false
			for _, expr := range required.NodeSelectorTerms[0].MatchExpressions {
				if expr.Key == "cloudburst.io/provider" && len(expr.Values) > 0 && expr.Values[0] == tt.expectProvider {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected cloudburst.io/provider=%q in MatchExpressions, got %+v", tt.expectProvider, required.NodeSelectorTerms[0].MatchExpressions)
			}
		})
	}
}
