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

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/drain"
)

const (
	nvidiaDomainPrefix       = "nvidia.com"
	nvidiaResourceNamePrefix = nvidiaDomainPrefix + "/" + "gpu"
	nvidiaMigResourcePrefix  = nvidiaDomainPrefix + "/" + "mig-"
	nvidiaDRADriverName      = "gpu." + nvidiaDomainPrefix

	computeDomainDRADriverName    = "compute-domain." + nvidiaDomainPrefix
	computeDomainDaemonConfigKind = "ComputeDomainDaemonConfig"

	nodeInitialUnschedulableAnnotationKey = nvidiaDomainPrefix + "/" + "driver-manager.node-initial-state.unschedulable"

	kubeClientPollInterval = 5 * time.Second
)

// Client represents a Kubernetes client wrapper use to perform all the Kubernetes operations required by k8s-driver-manager
type Client struct {
	ctx context.Context
	log *logrus.Logger

	clientset kubernetes.Interface
}

// DrainOptions represents the option parameters that can passed to the drain.Helper struct
type DrainOptions struct {
	Force              bool
	DeleteEmptyDirData bool
	Timeout            time.Duration
	PodSelector        string
}

// NewClient instantiates a new Kubernetes.Client
func NewClient(ctx context.Context, kubeconfig string, log *logrus.Logger) (*Client, error) {
	// Load kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	// Create clientset
	k8sClientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return &Client{
		ctx:       ctx,
		log:       log,
		clientset: k8sClientSet,
	}, nil
}

// GetNodeLabelValue returns the label value given a label key and node
func (c *Client) GetNodeLabelValue(nodeName, label string) (string, error) {
	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if node.Labels == nil {
		return "", nil
	}

	return node.Labels[label], nil
}

// UpdateNodeLabels updates the labels on a Node given a Node name and a string map of label key-value pairs
// This method uses a strategic merge patch to avoid conflicts with concurrent updates
func (c *Client) UpdateNodeLabels(nodeName string, nodeLabels map[string]string) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": nodeLabels,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	backoff := wait.Backoff{
		Duration: time.Second,
		Factor:   2.0,
		Jitter:   0.2,
		Steps:    7,
	}

	return retry.OnError(backoff, func(err error) bool {
		return true
	}, func() error {
		_, err := c.clientset.CoreV1().Nodes().Patch(c.ctx, nodeName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			c.log.Warnf("Failed to update labels on node %s, retrying: %v", nodeName, err)
		}
		return err
	})
}

// GetNodeAnnotationValue returns the annotation value given a node name and annotation key
func (c *Client) GetNodeAnnotationValue(nodeName, annotation string) (string, error) {
	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	if node.Annotations == nil {
		return "", nil
	}
	return node.Annotations[annotation], nil
}

// CordonNode atomically records a Node's initial schedulable state and marks it
// Unschedulable. An existing recording on an Unschedulable Node is retained so
// that restarting driver-manager does not lose the initial state.
func (c *Client) CordonNode(nodeName string) error {
	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	_, recorded := node.Annotations[nodeInitialUnschedulableAnnotationKey]
	if node.Spec.Unschedulable && recorded {
		return nil
	}

	initialState := strconv.FormatBool(node.Spec.Unschedulable)
	c.log.Infof("Cordoning node %s and recording initial state in annotation %s=%s", nodeName, nodeInitialUnschedulableAnnotationKey, initialState)
	return c.patchNodeSchedulingState(node, true, initialState)
}

// UncordonNode atomically marks a Node as schedulable and removes the initial
// state recording. The recording is not enforced yet, so this preserves the
// historical behavior of unconditionally uncordoning the Node.
func (c *Client) UncordonNode(nodeName string) error {
	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	_, recorded := node.Annotations[nodeInitialUnschedulableAnnotationKey]
	if !node.Spec.Unschedulable && !recorded {
		return nil
	}

	c.log.Infof("Uncordoning node %s", nodeName)
	return c.patchNodeSchedulingState(node, false, nil)
}

func (c *Client) patchNodeSchedulingState(node *corev1.Node, unschedulable bool, annotationValue any) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"resourceVersion": node.ResourceVersion,
			"annotations": map[string]any{
				nodeInitialUnschedulableAnnotationKey: annotationValue,
			},
		},
		"spec": map[string]any{
			"unschedulable": unschedulable,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal node scheduling state patch: %w", err)
	}

	_, err = c.clientset.CoreV1().Nodes().Patch(c.ctx, node.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		c.log.Warnf("Failed to update scheduling state on node %s: %v", node.Name, err)
	}
	return err
}

// WaitForPodTermination will wait for the termination of pods matching labels from the selectorMap on the node with the specified namespace.
// It will continue to wait until the specified timeout elapses
func (c *Client) WaitForPodTermination(selectorMap map[string]string, namespace, nodeName string, timeout time.Duration) error {
	return c.WaitForPodTerminationExcept(selectorMap, namespace, nodeName, timeout, nil)
}

// WaitForPodTerminationExcept is WaitForPodTermination, but it does not wait for
// the pods for which ignore returns true.
func (c *Client) WaitForPodTerminationExcept(selectorMap map[string]string, namespace, nodeName string, timeout time.Duration, ignore func(corev1.Pod) bool) error {
	selector := labels.SelectorFromSet(selectorMap)

	return wait.PollUntilContextTimeout(c.ctx, kubeClientPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		})
		if err != nil {
			return false, err
		}

		// Return true if no pods are found (all terminated) or all remaining pods are ignored
		for _, pod := range pods.Items {
			if ignore == nil || !ignore(pod) {
				return false, nil
			}
		}
		return true, nil
	})
}

// IsAnyPodReady reports whether a pod matching labels from the selectorMap on the node with
// the specified namespace is Ready and is not terminating.
func (c *Client) IsAnyPodReady(selectorMap map[string]string, namespace, nodeName string) (bool, error) {
	pods, err := c.clientset.CoreV1().Pods(namespace).List(c.ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(selectorMap).String(),
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		return false, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil && isPodReady(pod) {
			return true, nil
		}
	}
	return false, nil
}

// IsPodWaitingForGPUClaimUnprepare reports whether the pod is terminating, none of its
// containers is running, and it holds a ResourceClaim allocated by the NVIDIA GPU DRA driver.
// The kubelet stops the containers first, then asks the DRA kubelet-plugin to unprepare the
// claims, and sets a terminal phase only after that succeeds. The pod status shows the
// stopped containers only after the kubelet retries a failed unprepare.
func (c *Client) IsPodWaitingForGPUClaimUnprepare(pod corev1.Pod) (bool, error) {
	if pod.DeletionTimestamp == nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false, nil
	}
	if hasRunningContainer(pod) {
		return false, nil
	}
	return c.podHasGPUResourceClaim(pod)
}

func isPodReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// hasRunningContainer reports whether the pod status shows a running init, regular, or
// ephemeral container.
func hasRunningContainer(pod corev1.Pod) bool {
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.InitContainerStatuses,
		pod.Status.ContainerStatuses,
		pod.Status.EphemeralContainerStatuses,
	} {
		for _, status := range statuses {
			if status.State.Running != nil {
				return true
			}
		}
	}
	return false
}

// WaitForPodsWithNodeSelector waits for all daemon set pods on the given node which has the specified key in
// its nodeSelector to terminate.
func (c *Client) WaitForPodsWithNodeSelector(nodeName, nodeSelectorKey string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(c.ctx, kubeClientPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		podList, err := c.clientset.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		})
		if err != nil {
			return false, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
		}

		matchCount := 0
		for _, pod := range podList.Items {
			ownerRef := metav1.GetControllerOf(&pod)
			if ownerRef == nil || ownerRef.Kind != "DaemonSet" {
				continue
			}
			if _, ok := pod.Spec.NodeSelector[nodeSelectorKey]; ok {
				matchCount++
			}
		}

		if matchCount > 0 {
			c.log.Infof("Waiting for %d daemon set pod(s) with nodeSelector key %q to terminate", matchCount, nodeSelectorKey)
		}
		return matchCount == 0, nil
	})
}

// GetGPUResourceClaimHolders returns the namespaced names of the pods on the node which
// still hold a ResourceClaim allocated by the NVIDIA GPU DRA driver, i.e. the pods the
// kubelet still needs the DRA kubelet-plugin to unprepare. Pods in a terminal phase are
// excluded, since the kubelet has unprepared their claims by then. Pods for which ignore
// returns true are also excluded.
func (c *Client) GetGPUResourceClaimHolders(nodeName string, ignore func(corev1.Pod) bool) ([]string, error) {
	podList, err := c.clientset.CoreV1().Pods(corev1.NamespaceAll).List(c.ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	var holders []string
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if ignore != nil && ignore(pod) {
			continue
		}
		hasClaim, err := c.podHasGPUResourceClaim(pod)
		if err != nil {
			return nil, err
		}
		if hasClaim {
			holders = append(holders, pod.Namespace+"/"+pod.Name)
		}
	}

	return holders, nil
}

// WaitForComputeDomainDaemons waits until no IMEX daemon pod on the node is running. An IMEX
// daemon pod holds a ResourceClaim allocated by the NVIDIA ComputeDomain DRA driver with a
// ComputeDomainDaemonConfig. It gets all GPUs through a CDI spec, not through a GPU claim,
// and uses the driver while it runs. The DaemonSet controller deletes the daemon pod after
// the kubelet unprepares the channel claim of the ComputeDomain workload on the node. A
// terminating daemon pod whose containers have stopped no longer uses the driver, so this
// function does not wait for it to finish terminating.
func (c *Client) WaitForComputeDomainDaemons(nodeName string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(c.ctx, kubeClientPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		podList, err := c.clientset.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		})
		if err != nil {
			return false, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
		}

		var daemons []string
		for _, pod := range podList.Items {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			if pod.DeletionTimestamp != nil && !hasRunningContainer(pod) {
				continue
			}
			isDaemon, err := c.podHasResourceClaim(pod, isAllocatedComputeDomainDaemonClaim)
			if err != nil {
				return false, err
			}
			if isDaemon {
				daemons = append(daemons, pod.Namespace+"/"+pod.Name)
			}
		}

		if len(daemons) > 0 {
			c.log.Infof("Waiting for ComputeDomain daemon pod(s) to stop: %s", strings.Join(daemons, ", "))
		}
		return len(daemons) == 0, nil
	})
}

// DrainNode drains a Node given a Node name and a set of drain option parameters
func (c *Client) DrainNode(nodeName string, drainOpts DrainOptions) error {
	c.log.Infof("Draining node %s", nodeName)

	drainHelper := &drain.Helper{
		Ctx:                c.ctx,
		Client:             c.clientset,
		Force:              drainOpts.Force,
		DeleteEmptyDirData: drainOpts.DeleteEmptyDirData,
		Timeout:            drainOpts.Timeout,
	}

	if drainOpts.PodSelector != "" {
		drainHelper.PodSelector = drainOpts.PodSelector
	}

	return drain.RunNodeDrain(drainHelper, nodeName)
}

// DeleteOrEvictPods deletes or evicts the pods on the api server given a Node Name and set of drain option parameters.
// Pods for which ignore returns true are not counted and not deleted.
func (c *Client) DeleteOrEvictPods(nodeName string, drainOpts DrainOptions, ignore func(corev1.Pod) bool) error {
	c.log.Infof("Draining node %s of any GPU pods", nodeName)

	customDrainFilter := func(pod corev1.Pod) drain.PodDeleteStatus {
		if ignore != nil && ignore(pod) {
			return drain.MakePodDeleteStatusSkip()
		}
		usesGPU, err := c.podUsesGPU(pod)
		if err != nil {
			return drain.MakePodDeleteStatusWithError(err.Error())
		}
		if !usesGPU {
			return drain.MakePodDeleteStatusSkip()
		}
		return drain.MakePodDeleteStatusOkay()
	}

	drainHelper := drain.Helper{
		Ctx:                 c.ctx,
		Client:              c.clientset,
		Out:                 os.Stdout,
		ErrOut:              os.Stderr,
		ChunkSize:           cmdutil.DefaultChunkSize,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  drainOpts.DeleteEmptyDirData,
		Force:               drainOpts.Force,
		Timeout:             drainOpts.Timeout,
		AdditionalFilters:   []drain.PodFilter{customDrainFilter},
	}

	c.log.Infof("Identifying GPU pods to delete")

	// List all pods
	podList, err := c.clientset.CoreV1().Pods(corev1.NamespaceAll).List(
		c.ctx,
		metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName},
	)
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}

	// Get number of GPU pods on the node which require deletion
	numPodsToDelete := 0
	for _, pod := range podList.Items {
		if ignore != nil && ignore(pod) {
			continue
		}
		usesGPU, err := c.podUsesGPU(pod)
		if err != nil {
			return fmt.Errorf("failed to check GPU usage for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		if usesGPU {
			numPodsToDelete += 1
		}
	}

	if numPodsToDelete == 0 {
		c.log.Infof("No GPU pods to delete. Exiting.")
		return nil
	}

	podDeleteList, errs := drainHelper.GetPodsForDeletion(nodeName)
	numPodsCanDelete := len(podDeleteList.Pods())
	if numPodsCanDelete != numPodsToDelete {
		c.log.Error("Cannot delete all GPU pods")
		for _, err := range errs {
			c.log.Errorf("error reported by drain helper: %v", err)
		}
		return fmt.Errorf("failed to delete all GPU pods")
	}

	for _, p := range podDeleteList.Pods() {
		c.log.Infof("GPU pod - %s/%s", p.Namespace, p.Name)
	}

	c.log.Info("Deleting GPU pods...")
	err = drainHelper.DeleteOrEvictPods(podDeleteList.Pods())
	if err != nil {
		return fmt.Errorf("failed to delete all GPU pods: %w", err)
	}

	return nil
}

// podUsesGPU reports whether a pod uses NVIDIA GPU resources, either via
// traditional device-plugin resource requests/limits or via DRA ResourceClaims
// allocated by the NVIDIA GPU DRA driver.
func (c *Client) podUsesGPU(pod corev1.Pod) (bool, error) {
	gpuInResourceList := func(rl corev1.ResourceList) bool {
		for resourceName := range rl {
			str := string(resourceName)
			if strings.HasPrefix(str, nvidiaResourceNamePrefix) || strings.HasPrefix(str, nvidiaMigResourcePrefix) {
				return true
			}
		}
		return false
	}

	for _, c := range pod.Spec.Containers {
		if gpuInResourceList(c.Resources.Limits) || gpuInResourceList(c.Resources.Requests) {
			return true, nil
		}
	}

	return c.podHasGPUResourceClaim(pod)
}

// podHasGPUResourceClaim reports whether the pod holds a ResourceClaim allocated by
// the NVIDIA GPU DRA driver.
func (c *Client) podHasGPUResourceClaim(pod corev1.Pod) (bool, error) {
	return c.podHasResourceClaim(pod, isAllocatedNvidiaGPUClaim)
}

// podHasResourceClaim reports whether the pod holds a ResourceClaim for which match
// returns true. It resolves the name of each claim the pod references and inspects
// that claim.
func (c *Client) podHasResourceClaim(pod corev1.Pod, match func(*resourcev1.ResourceClaim) bool) (bool, error) {
	for _, claimRef := range pod.Spec.ResourceClaims {
		claimName := resolveResourceClaimName(pod, claimRef)
		if claimName == "" {
			continue
		}
		claim, err := c.clientset.ResourceV1().ResourceClaims(pod.Namespace).Get(c.ctx, claimName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("failed to get ResourceClaim %s/%s: %w", pod.Namespace, claimName, err)
		}
		if match(claim) {
			return true, nil
		}
	}
	return false, nil
}

// resolveResourceClaimName returns the name of the ResourceClaim backing a pod's
// claim reference.
func resolveResourceClaimName(pod corev1.Pod, claimRef corev1.PodResourceClaim) string {
	if claimRef.ResourceClaimName != nil {
		return *claimRef.ResourceClaimName
	}
	for _, status := range pod.Status.ResourceClaimStatuses {
		if status.Name == claimRef.Name && status.ResourceClaimName != nil {
			return *status.ResourceClaimName
		}
	}
	return ""
}

// isAllocatedNvidiaGPUClaim reports whether a ResourceClaim has been allocated a
// device by the NVIDIA GPU DRA driver.
func isAllocatedNvidiaGPUClaim(claim *resourcev1.ResourceClaim) bool {
	if claim.Status.Allocation == nil {
		return false
	}
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == nvidiaDRADriverName {
			return true
		}
	}
	return false
}

// isAllocatedComputeDomainDaemonClaim reports whether a ResourceClaim has been allocated
// a device by the NVIDIA ComputeDomain DRA driver for an IMEX daemon. To identify a
// ComputeDomain Daemon claim, we need to check if the kind is ComputeDomainDaemonConfig.
func isAllocatedComputeDomainDaemonClaim(claim *resourcev1.ResourceClaim) bool {
	if claim.Status.Allocation == nil {
		return false
	}
	allocated := false
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == computeDomainDRADriverName {
			allocated = true
			break
		}
	}
	if !allocated {
		return false
	}
	for _, config := range claim.Status.Allocation.Devices.Config {
		if config.Opaque == nil || config.Opaque.Driver != computeDomainDRADriverName {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(config.Opaque.Parameters.Raw, &typeMeta); err != nil {
			continue
		}
		if typeMeta.Kind == computeDomainDaemonConfigKind {
			return true
		}
	}
	return false
}
