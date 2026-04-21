//go:build !darwin && !windows

/*
 * Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

const (
	autoDrainSupersededByPolicyMsg      = "Auto drain by driver-manager has been superseded by the GPU Operator's upgrade policy. The GPU Operator will perform the GPU node drain instead"
	gpuPodEvictionSupersededByPolicyMsg = "GPU pod eviction by driver-manager has been superseded by the GPU Operator's upgrade policy. The GPU Operator will perform the workload eviction instead"

	cleanupGuidancePolicyEnabledMsg = `Cleanup could not automatically proceed on node %s because the GPU driver auto-upgrade policy is enabled and upgrade-controller manages GPU workload eviction.

Note: reconciliation is automatic; after any option below is applied, the system will retry without manual intervention.

Cleanup guidance (choose one):
	[1] Manually stop workloads using GPUs on this node so that the GPUs are released.
	[2] Disable the GPU driver auto-upgrade policy in the GPU Operator ClusterPolicy and ensure k8s-driver-manager has ENABLE_GPU_POD_EVICTION=true (and optionally ENABLE_AUTO_DRAIN=true) so driver-manager can auto-evict GPU workloads.
	[3] Request upgrade-controller to auto-evict GPU workloads for this node now by running:
			kubectl label node %s nvidia.com/gpu-driver-upgrade-state=upgrade-required --overwrite
	[4] Wait for running GPU workloads to finish and release GPUs on this node.
`

	cleanupGuidanceFeaturesDisabledMsg = `Cleanup could not automatically proceed on node %s because both GPU pod eviction and auto-drain are disabled.

Note: reconciliation is automatic; after any option below is applied, the system will retry without manual intervention.

Cleanup guidance (choose one):
	[1] Enable driver-manager eviction settings: set ENABLE_GPU_POD_EVICTION=true and/or ENABLE_AUTO_DRAIN=true.
	[2] Manually stop GPU workloads on this node so that the GPUs are released.
	[3] Wait for running GPU workloads to finish and release GPUs on this node.
`
)
