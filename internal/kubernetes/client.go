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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
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
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if node.Labels == nil {
		return "", nil
	}

	return node.Labels[label], nil
}

// GetNodeLabels returns all the labels for a given node
func (c *Client) GetAllNodeLabels(nodeName string) (map[string]string, error) {
	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return node.Labels, nil
}

// GetNode returns a Node given a Node name
func (c *Client) GetNode(nodeName string) (*corev1.Node, error) {
	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return node, nil
}

// UpdateNode updates a Node given a Node name and a Node object
func (c *Client) UpdateNode(node *corev1.Node) error {
	_, err := c.clientset.CoreV1().Nodes().Update(c.ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node %s: %w", node.Name, err)
	}
	return nil
}

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

func (c *Client) RemoveNodeLabel(nodeName, labelKey string) error {
	// JSON Patch operation to remove a specific label key
	patch := []map[string]string{
		{
			"op":   "remove",
			"path": fmt.Sprintf("/metadata/labels/%s", escapeJSONPointer(labelKey)),
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = c.clientset.CoreV1().Nodes().Patch(c.ctx, nodeName, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
	return err
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

	_, err = c.clientset.CoreV1().Nodes().Patch(c.ctx, nodeName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to update the labels of node %s: %w", nodeName, err)
	}

	return nil
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
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	drainHelper := &drain.Helper{Ctx: c.ctx, Client: c.clientset}
	return drain.RunCordonOrUncordon(drainHelper, node, true)
}

// UncordonNode uncordons a Node given a Node name marking it as Schedulable
func (c *Client) UncordonNode(nodeName string) error {
	c.log.Infof("Uncordoning node %s", nodeName)

	node, err := c.clientset.CoreV1().Nodes().Get(c.ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	drainHelper := &drain.Helper{Ctx: c.ctx, Client: c.clientset}
	return drain.RunCordonOrUncordon(drainHelper, node, false)
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

// WaitForNvidiaLabelsRemoval will wait for the removal of all the nvidia labels from the node
func (c *Client) WaitForNvidiaLabelsRemoval(nodeName string, nvidiaDomainPrefix string, timeout time.Duration) error {

	return wait.PollUntilContextTimeout(c.ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		labels, err := c.GetAllNodeLabels(nodeName)
		if err != nil {
			return false, fmt.Errorf("failed to get all node labels: %w", err)
		}
		for key := range labels {
			if strings.HasPrefix(key, nvidiaDomainPrefix) {
				return false, fmt.Errorf("nvidia label %s still present on the node", key)
			}
		}
		return true, nil
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
