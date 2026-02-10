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
)

// DesiredCapacity represents the calculated resource requirements
type DesiredCapacity struct {
	// CPUMilliTotal is the total CPU requested in millicores
	CPUMilliTotal int64
	// MemBytesTotal is the total memory requested in bytes
	MemBytesTotal int64
	// PodCount is the number of pods requiring resources
	PodCount int
}

// CalculateDesiredCapacity sums CPU and memory requests across eligible pods
func CalculateDesiredCapacity(pods []corev1.Pod) DesiredCapacity {
	capacity := DesiredCapacity{
		PodCount: len(pods),
	}

	for i := range pods {
		cpuMilli, memBytes := CalculateDesiredCapacityForPod(&pods[i])
		capacity.CPUMilliTotal += cpuMilli
		capacity.MemBytesTotal += memBytes
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
