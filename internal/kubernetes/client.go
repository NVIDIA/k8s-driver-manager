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
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

// Client represents a Kubernetes client wrapper use to perform all the Kubernetes operations required by k8s-driver-manager
type Client struct {
	ctx context.Context
	log *logrus.Logger

	clientset *kubernetes.Clientset
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
		return "", fmt.Errorf("failed to get node %q: %w", nodeName, err)
	}

	if node.Labels == nil {
		return "", nil
	}

	return node.Labels[label], nil
}

// UpdateNodeLabels updates the labels on a Node given a Node name and a string map of label key-value pairs
// This method uses a strategic merge patch to avoid conflicts with concurrent updates
func (c *Client) UpdateNodeLabels(nodeName string, nodeLabels map[string]string) error {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": nodeLabels,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	return c.patchNodeWithRetry(nodeName, patchBytes, types.StrategicMergePatchType)
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

// CordonNode cordons a Node given a Node name marking it as Unschedulable
func (c *Client) CordonNode(nodeName string) error {
	c.log.Infof("Cordoning node %s", nodeName)

	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %q: %w", nodeName, err)
	}

	drainHelper := &drain.Helper{Ctx: c.ctx, Client: c.clientset}
	return drain.RunCordonOrUncordon(drainHelper, node, true)
}

// UncordonNode uncordons a Node given a Node name marking it as Schedulable
func (c *Client) UncordonNode(nodeName string) error {
	c.log.Infof("Uncordoning node %s", nodeName)

	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %q: %w", nodeName, err)
	}

	drainHelper := &drain.Helper{Ctx: c.ctx, Client: c.clientset}
	return drain.RunCordonOrUncordon(drainHelper, node, false)
}

// TaintNode applies a taint on a Node marking it as Unschedulable. If the taint already exists, it skips updating
// the node
func (c *Client) TaintNode(taint corev1.Taint, nodeName string) error {
	c.log.Infof("Tainting node %s", nodeName)

	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %q: %w", nodeName, err)
	}

	original := node.DeepCopy()

	// First, we check if the taint already exists. If yes, we skip the node patch operation
	for _, nodeTaint := range node.Spec.Taints {
		if taintsEqual(nodeTaint, taint) {
			c.log.Infof("Taint has already been applied. Skipping node taint...")
			return nil
		}
	}

	node.Spec.Taints = append(node.Spec.Taints, taint)

	return c.applyNodeDiff(original, node)
}

// UntaintNode removes the taint on a Node. If the taint doesn't exist, it skips updating the node.
func (c *Client) UntaintNode(taint corev1.Taint, nodeName string) error {
	c.log.Infof("Untainting node %s", nodeName)

	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %q: %w", nodeName, err)
	}

	original := node.DeepCopy()

	for i, nodeTaint := range node.Spec.Taints {
		if taintsEqual(nodeTaint, taint) {
			node.Spec.Taints = append(node.Spec.Taints[:i], node.Spec.Taints[i+1:]...)
			return c.applyNodeDiff(original, node)
		}
	}
	c.log.Infof("Taint not found. Skipping node untaint...")
	return nil
}

// WaitForPodTermination will wait for the termination of pods matching labels from the selectorMap on the node with the specified namespace.
// It will continue to wait until the specified timeout elapses
func (c *Client) WaitForPodTermination(selectorMap map[string]string, namespace, nodeName string, timeout time.Duration) error {
	selector := labels.SelectorFromSet(selectorMap)

	return wait.PollUntilContextTimeout(c.ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := c.clientset.CoreV1().Pods(namespace).List(c.ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return false, err
		}

		// Return true if no pods are found (all terminated)
		return len(pods.Items) == 0, nil
	})
}

// ListPodsOnNodeWithAnnotation will list all pods matching a specified annotation on a node
func (c *Client) ListPodsOnNodeWithAnnotation(annotationKey string, nodeName string) ([]corev1.Pod, error) {

	podList, err := c.clientset.CoreV1().Pods(corev1.NamespaceAll).List(c.ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, err
	}
	var result []corev1.Pod
	for _, pod := range podList.Items {
		if _, exists := pod.Annotations[annotationKey]; exists {
			result = append(result, pod)
		}
	}
	return result, nil
}

// EvictPodWithWait will evict a pod with the name "podName" and return after confirmation of successful pod deletion
func (c *Client) EvictPodWithWait(podName, namespace string, timeout time.Duration) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		// Optional: specify DeleteOptions such as grace period
		DeleteOptions: &metav1.DeleteOptions{},
	}
	err := c.clientset.CoreV1().Pods(namespace).EvictV1(c.ctx, eviction)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to evict pod %s/%s: %w", namespace, podName, err)
	}

	return wait.PollUntilContextTimeout(c.ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := c.clientset.CoreV1().Pods(namespace).Get(c.ctx, podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
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

// DeleteOrEvictPods deletes or evicts the pods on the api server given a Node Name and set of drain option parameters
func (c *Client) DeleteOrEvictPods(nodeName string, drainOpts DrainOptions) error {
	c.log.Infof("Draining node %s of any GPU pods", nodeName)

	customDrainFilter := func(pod corev1.Pod) drain.PodDeleteStatus {
		deletePod := gpuPodSpecFilter(pod)
		if !deletePod {
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
		if gpuPodSpecFilter(pod) {
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

func gpuPodSpecFilter(pod corev1.Pod) bool {
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
			return true
		}
	}
	return false
}

func (c *Client) applyNodeDiff(currentNode *corev1.Node, desiredNode *corev1.Node) error {
	currentNodeJSON, err := json.Marshal(currentNode)
	if err != nil {
		return fmt.Errorf("failed to marshal node object %v: %w", currentNode, err)
	}
	desiredNodeJSON, err := json.Marshal(desiredNode)
	if err != nil {
		return fmt.Errorf("failed to marshal node object %v: %w", desiredNode, err)
	}

	patch, err := jsonpatch.CreateMergePatch(currentNodeJSON, desiredNodeJSON)
	if err != nil {
		return fmt.Errorf("failed to create merge-patch: %w", err)
	}
	return c.patchNodeWithRetry(desiredNode.Name, patch, types.MergePatchType)
}

func (c *Client) patchNodeWithRetry(nodeName string, patch []byte, patchType types.PatchType) error {

	backoff := wait.Backoff{
		Duration: time.Second,
		Factor:   2.0,
		Jitter:   0.2,
		Steps:    7,
	}
	return retry.OnError(backoff, func(err error) bool {
		return true
	}, func() error {
		_, err := c.clientset.CoreV1().Nodes().Patch(c.ctx, nodeName, patchType, patch, metav1.PatchOptions{})
		if err != nil {
			c.log.Warnf("Failed to patch node %s, retrying: %v", nodeName, err)
		}
		return err
	})
}

func taintsEqual(t1 corev1.Taint, t2 corev1.Taint) bool {
	keysEqual := t1.Key == t2.Key
	valuesEqual := t1.Value == t2.Value
	effectsEqual := t1.Effect == t2.Effect
	return keysEqual && valuesEqual && effectsEqual
}
