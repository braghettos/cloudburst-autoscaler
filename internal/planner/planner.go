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
	corev1 "k8s.io/api/core/v1"

	cloudburstv1alpha1 "github.com/diegobraga/cloudburst-autoscaler/api/v1alpha1"
)

// DesiredCapacity represents the calculated resource requirements
type DesiredCapacity struct {
	// CPUMilliTotal is the total CPU requested in millicores
	CPUMilliTotal int64
	// MemBytesTotal is the total memory requested in bytes
	MemBytesTotal int64
	// PodCount is the number of pods requiring resources
	PodCount int
	// Arch is the architecture requirement extracted from pod nodeSelector/nodeAffinity.
	// Empty string means no architecture requirement (any architecture is acceptable).
	// If multiple pods have different arch requirements, this will contain the first one found.
	Arch string
}

// CalculateDesiredCapacity sums CPU and memory requests across eligible pods
// and extracts architecture requirements from pod nodeSelector/nodeAffinity
func CalculateDesiredCapacity(pods []corev1.Pod) DesiredCapacity {
	capacity := DesiredCapacity{
		PodCount: len(pods),
	}

	for i := range pods {
		cpuMilli, memBytes := CalculateDesiredCapacityForPod(&pods[i])
		capacity.CPUMilliTotal += cpuMilli
		capacity.MemBytesTotal += memBytes

		// Extract architecture requirement from pod (if not already set)
		if capacity.Arch == "" {
			capacity.Arch = ExtractArchFromPod(&pods[i])
		}
	}

	return capacity
}

// CalculateDesiredCapacityForPod calculates capacity for a single pod
// This properly handles init containers by taking the maximum of:
// - Maximum init container request (init containers run sequentially)
// - Sum of regular container requests (regular containers run concurrently)
func CalculateDesiredCapacityForPod(pod *corev1.Pod) (cpuMilli int64, memBytes int64) {
	var maxInitCPU int64
	var maxInitMemory int64

	// Find maximum init container requests (init containers run sequentially)
	for i := range pod.Spec.InitContainers {
		container := &pod.Spec.InitContainers[i]
		if cpu, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
			if cpu.MilliValue() > maxInitCPU {
				maxInitCPU = cpu.MilliValue()
			}
		}
		if memory, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
			if memory.Value() > maxInitMemory {
				maxInitMemory = memory.Value()
			}
		}
	}

	// Sum regular container requests (regular containers run concurrently)
	var sumCPU int64
	var sumMemory int64
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if cpu, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
			sumCPU += cpu.MilliValue()
		}
		if memory, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
			sumMemory += memory.Value()
		}
	}

	// Take the maximum: init containers run sequentially, so we need max(init) OR sum(containers)
	// But since init containers finish before regular containers start, we need:
	// max(maxInitCPU, sumCPU) and max(maxInitMemory, sumMemory)
	if sumCPU > maxInitCPU {
		cpuMilli = sumCPU
	} else {
		cpuMilli = maxInitCPU
	}

	if sumMemory > maxInitMemory {
		memBytes = sumMemory
	} else {
		memBytes = maxInitMemory
	}

	return cpuMilli, memBytes
}

// ExtractArchFromPod extracts architecture requirement from pod's nodeSelector or nodeAffinity.
// Returns empty string if no architecture requirement is found.
// Architecture is extracted from:
// - nodeSelector: kubernetes.io/arch
// - nodeAffinity: requiredDuringSchedulingIgnoredDuringExecution with kubernetes.io/arch
// - nodeAffinity: preferredDuringSchedulingIgnoredDuringExecution with kubernetes.io/arch (first match)
func ExtractArchFromPod(pod *corev1.Pod) string {
	// Check nodeSelector first
	if pod.Spec.NodeSelector != nil {
		if arch, ok := pod.Spec.NodeSelector["kubernetes.io/arch"]; ok && arch != "" {
			return arch // Return raw value, CloudBroker will normalize
		}
	}

	// Check nodeAffinity
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		// Check requiredDuringSchedulingIgnoredDuringExecution
		if required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution; required != nil {
			for _, term := range required.NodeSelectorTerms {
				for _, expr := range term.MatchExpressions {
					if expr.Key == "kubernetes.io/arch" {
						if len(expr.Values) > 0 {
							return expr.Values[0] // Return raw value, CloudBroker will normalize
						}
					}
				}
				for _, field := range term.MatchFields {
					if field.Key == "kubernetes.io/arch" {
						if len(field.Values) > 0 {
							return field.Values[0] // Return raw value, CloudBroker will normalize
						}
					}
				}
			}
		}

		// Check preferredDuringSchedulingIgnoredDuringExecution (use first match)
		for _, pref := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			for _, expr := range pref.Preference.MatchExpressions {
				if expr.Key == "kubernetes.io/arch" {
					if len(expr.Values) > 0 {
						return expr.Values[0] // Return raw value, CloudBroker will normalize
					}
				}
			}
			for _, field := range pref.Preference.MatchFields {
				if field.Key == "kubernetes.io/arch" {
					if len(field.Values) > 0 {
						return field.Values[0] // Return raw value, CloudBroker will normalize
					}
				}
			}
		}
	}

	return ""
}

// PodHasProviderOrNodePoolConstraint returns true if the pod already has a constraint
// on cloudburst.io/provider or cloudburst.io/nodepool via nodeSelector or nodeAffinity.
// Such pods do not need to be patched by the controller.
func PodHasProviderOrNodePoolConstraint(pod *corev1.Pod) bool {
	const providerKey = "cloudburst.io/provider"
	const nodepoolKey = "cloudburst.io/nodepool"

	if pod.Spec.NodeSelector != nil {
		if _, ok := pod.Spec.NodeSelector[providerKey]; ok {
			return true
		}
		if _, ok := pod.Spec.NodeSelector[nodepoolKey]; ok {
			return true
		}
	}

	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if required != nil {
			for _, term := range required.NodeSelectorTerms {
				for _, expr := range term.MatchExpressions {
					if expr.Key == providerKey || expr.Key == nodepoolKey {
						return true
					}
				}
				for _, field := range term.MatchFields {
					if field.Key == providerKey || field.Key == nodepoolKey {
						return true
					}
				}
			}
		}
		preferred := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		for _, pref := range preferred {
			for _, expr := range pref.Preference.MatchExpressions {
				if expr.Key == providerKey || expr.Key == nodepoolKey {
					return true
				}
			}
			for _, field := range pref.Preference.MatchFields {
				if field.Key == providerKey || field.Key == nodepoolKey {
					return true
				}
			}
		}
	}

	return false
}

// GetPodNodePoolConstraint returns the pod's cloudburst.io/nodepool value from nodeSelector or nodeAffinity.
// Returns empty string if not set.
func GetPodNodePoolConstraint(pod *corev1.Pod) string {
	const nodepoolKey = "cloudburst.io/nodepool"

	if pod.Spec.NodeSelector != nil {
		if v, ok := pod.Spec.NodeSelector[nodepoolKey]; ok && v != "" {
			return v
		}
	}

	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if required != nil {
			for _, term := range required.NodeSelectorTerms {
				for _, expr := range term.MatchExpressions {
					if expr.Key == nodepoolKey && len(expr.Values) > 0 {
						return expr.Values[0]
					}
				}
				for _, field := range term.MatchFields {
					if field.Key == nodepoolKey && len(field.Values) > 0 {
						return field.Values[0]
					}
				}
			}
		}
		for _, pref := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			for _, expr := range pref.Preference.MatchExpressions {
				if expr.Key == nodepoolKey && len(expr.Values) > 0 {
					return expr.Values[0]
				}
			}
			for _, field := range pref.Preference.MatchFields {
				if field.Key == nodepoolKey && len(field.Values) > 0 {
					return field.Values[0]
				}
			}
		}
	}

	return ""
}

// GetPodProviderConstraint returns the pod's cloudburst.io/provider value from nodeSelector or nodeAffinity.
// Returns empty string if not set.
func GetPodProviderConstraint(pod *corev1.Pod) string {
	const providerKey = "cloudburst.io/provider"

	if pod.Spec.NodeSelector != nil {
		if v, ok := pod.Spec.NodeSelector[providerKey]; ok && v != "" {
			return v
		}
	}

	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if required != nil {
			for _, term := range required.NodeSelectorTerms {
				for _, expr := range term.MatchExpressions {
					if expr.Key == providerKey && len(expr.Values) > 0 {
						return expr.Values[0]
					}
				}
				for _, field := range term.MatchFields {
					if field.Key == providerKey && len(field.Values) > 0 {
						return field.Values[0]
					}
				}
			}
		}
		for _, pref := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			for _, expr := range pref.Preference.MatchExpressions {
				if expr.Key == providerKey && len(expr.Values) > 0 {
					return expr.Values[0]
				}
			}
			for _, field := range pref.Preference.MatchFields {
				if field.Key == providerKey && len(field.Values) > 0 {
					return field.Values[0]
				}
			}
		}
	}

	return ""
}

// PodMatchesNodePool returns true if the pod's scheduling constraints match the NodePool.
// A pod matches when:
// - Pod has cloudburst.io/nodepool and it equals nodePool.Name, OR
// - Pod has cloudburst.io/provider and it is in nodePool.Spec.Requirements.AllowedProviders, OR
// - Pod has no nodepool/provider constraint (unbound) and nodePool has no AllowedProviders (catch-all).
func PodMatchesNodePool(pod *corev1.Pod, nodePool *cloudburstv1alpha1.NodePool) bool {
	nodepoolVal := GetPodNodePoolConstraint(pod)
	providerVal := GetPodProviderConstraint(pod)

	if nodepoolVal != "" {
		return nodepoolVal == nodePool.Name
	}
	if providerVal != "" {
		if len(nodePool.Spec.Requirements.AllowedProviders) == 0 {
			return true
		}
		for _, p := range nodePool.Spec.Requirements.AllowedProviders {
			if p == providerVal {
				return true
			}
		}
		return false
	}
	// Unbound pod: only match NodePools with no AllowedProviders (catch-all)
	return len(nodePool.Spec.Requirements.AllowedProviders) == 0
}

// AddProviderNodeAffinity returns a copy of the pod's affinity with
// cloudburst.io/provider=provider added to requiredDuringSchedulingIgnoredDuringExecution.
// Merges with existing nodeAffinity if present; otherwise creates a new structure.
func AddProviderNodeAffinity(pod *corev1.Pod, provider string) *corev1.Affinity {
	providerExpr := corev1.NodeSelectorRequirement{
		Key:      "cloudburst.io/provider",
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{provider},
	}

	var required *corev1.NodeSelector
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil &&
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		required = pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.DeepCopy()
	} else {
		required = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{}},
		}
	}

	// Add provider constraint to the first term
	if len(required.NodeSelectorTerms) == 0 {
		required.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
	}
	required.NodeSelectorTerms[0].MatchExpressions = append(
		required.NodeSelectorTerms[0].MatchExpressions,
		providerExpr,
	)

	affinity := pod.Spec.Affinity.DeepCopy()
	if affinity == nil {
		affinity = &corev1.Affinity{}
	}
	if affinity.NodeAffinity == nil {
		affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = required
	return affinity
}
