//go:build !darwin && !windows

/*
 * Copyright (c) 2019-2024, NVIDIA CORPORATION.  All rights reserved.
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

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/moby/sys/mount"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/drain"

	"github.com/NVIDIA/k8s-driver-manager/internal/nvml"
)

const (
	// Constants from the original bash script
	driverRoot          = "/run/nvidia/driver"
	driverPIDFile       = "/run/nvidia/nvidia-driver.pid"
	operatorNamespace   = "gpu-operator-resources"
	pausedStr           = "paused-for-driver-upgrade"
	defaultDrainTimeout = time.Second * 0
	defaultGracePeriod  = 5 * time.Minute

	nvidiaResourceNamePrefix = "nvidia.com/gpu"
	nvidiaMigResourcePrefix  = "nvidia.com/mig-"

	nvmlSharedObject = "libnvidia-ml.so.1"
	nvmlSOPath       = "/usr/lib/x86_64-linux-gnu" + "/" + nvmlSharedObject
)

// Configuration holds all the configuration from environment variables
type config struct {
	nodeName                   string
	drainUseForce              bool
	drainPodSelectorLabel      string
	drainTimeout               time.Duration
	drainDeleteEmptyDirData    bool
	enableAutoDrain            bool
	enableGPUPodEviction       bool
	operatorNamespace          string
	nodeLabelForGPUPodEviction string
	gpuDirectRDMAEnabled       bool
	useHostMofed               bool
	kubeconfig                 string
}

// ComponentState tracks the deployment state of GPU operator components
type componentState struct {
	pluginDeployed              string
	gfdDeployed                 string
	dcgmDeployed                string
	dcgmExporterDeployed        string
	nvsmDeployed                string
	toolkitDeployed             string
	validatorDeployed           string
	migManagerDeployed          string
	sandboxValidatorDeployed    string
	sandboxPluginDeployed       string
	vgpuDeviceManagerDeployed   string
	customOperandNodeLabelValue string
	autoUpgradePolicyEnabled    string
}

// DriverManager handles the driver management operations
type DriverManager struct {
	ctx context.Context

	config     *config
	components *componentState
	clientset  *kubernetes.Clientset
	log        *logrus.Logger
}

func main() {
	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	cfg := &config{}
	components := &componentState{}

	app := cli.NewApp()
	app.Name = "driver-manager"
	app.Usage = "Manage NVIDIA GPU drivers and Kubernetes GPU operator components"
	app.Version = "1.0.0"

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "node-name",
			Usage:       "The name of the node to manage",
			Destination: &cfg.nodeName,
			EnvVars:     []string{"NODE_NAME"},
			Required:    true,
		},
		&cli.BoolFlag{
			Name:        "drain-use-force",
			Usage:       "Use force when draining nodes",
			Destination: &cfg.drainUseForce,
			EnvVars:     []string{"DRAIN_USE_FORCE"},
			Value:       false,
		},
		&cli.StringFlag{
			Name:        "drain-pod-selector-label",
			Usage:       "Pod selector label for draining",
			Destination: &cfg.drainPodSelectorLabel,
			EnvVars:     []string{"DRAIN_POD_SELECTOR_LABEL"},
			Value:       "",
		},
		&cli.DurationFlag{
			Name:        "drain-timeout-seconds",
			Usage:       "Timeout for drain operations",
			Destination: &cfg.drainTimeout,
			EnvVars:     []string{"DRAIN_TIMEOUT_SECONDS"},
			Value:       defaultDrainTimeout,
		},
		&cli.BoolFlag{
			Name:        "drain-delete-emptydir-data",
			Usage:       "Delete emptyDir data during drain",
			Destination: &cfg.drainDeleteEmptyDirData,
			EnvVars:     []string{"DRAIN_DELETE_EMPTYDIR_DATA"},
			Value:       false,
		},
		&cli.BoolFlag{
			Name:        "enable-auto-drain",
			Usage:       "Enable automatic node draining",
			Destination: &cfg.enableAutoDrain,
			EnvVars:     []string{"ENABLE_AUTO_DRAIN"},
			Value:       true,
		},
		&cli.BoolFlag{
			Name:        "enable-gpu-pod-eviction",
			Usage:       "Enable GPU pod eviction",
			Destination: &cfg.enableGPUPodEviction,
			EnvVars:     []string{"ENABLE_GPU_POD_EVICTION"},
			Value:       true,
		},
		&cli.StringFlag{
			Name:        "operator-namespace",
			Usage:       "Namespace for GPU operator resources",
			Destination: &cfg.operatorNamespace,
			EnvVars:     []string{"OPERATOR_NAMESPACE"},
			Value:       operatorNamespace,
		},
		&cli.StringFlag{
			Name:        "node-label-for-gpu-pod-eviction",
			Usage:       "Node label for GPU pod eviction",
			Destination: &cfg.nodeLabelForGPUPodEviction,
			EnvVars:     []string{"NODE_LABEL_FOR_GPU_POD_EVICTION"},
			Value:       "",
		},
		&cli.BoolFlag{
			Name:        "gpu-direct-rdma-enabled",
			Usage:       "Enable GPU Direct RDMA",
			Destination: &cfg.gpuDirectRDMAEnabled,
			EnvVars:     []string{"GPU_DIRECT_RDMA_ENABLED"},
			Value:       false,
		},
		&cli.BoolFlag{
			Name:        "use-host-mofed",
			Usage:       "Use host MOFED driver",
			Destination: &cfg.useHostMofed,
			EnvVars:     []string{"USE_HOST_MOFED"},
			Value:       false,
		},
		&cli.StringFlag{
			Name:        "kubeconfig",
			Usage:       "Path to kubeconfig file",
			Destination: &cfg.kubeconfig,
			EnvVars:     []string{"KUBECONFIG"},
			Value:       "",
		},
	}

	app.Commands = []*cli.Command{
		{
			Name:  "uninstall_driver",
			Usage: "Uninstall NVIDIA driver and manage GPU operator components",
			Action: func(c *cli.Context) error {
				dm, err := newDriverManager(cfg, components, log)
				if err != nil {
					return fmt.Errorf("failed to create driver manager: %w", err)
				}
				return dm.uninstallDriver()
			},
		},
		{
			Name:  "preflight_check",
			Usage: "Perform preflight checks",
			Action: func(c *cli.Context) error {
				dm, err := newDriverManager(cfg, components, log)
				if err != nil {
					return fmt.Errorf("failed to create driver manager: %w", err)
				}
				return dm.preflightCheck()
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func newDriverManager(cfg *config, components *componentState, log *logrus.Logger) (*DriverManager, error) {
	// Load kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", cfg.kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return &DriverManager{
		config:     cfg,
		components: components,
		clientset:  clientset,
		log:        log,
		ctx:        context.Background(),
	}, nil
}

func (dm *DriverManager) uninstallDriver() error {
	dm.log.Info("Starting driver uninstallation process")

	// Check if driver is pre-installed on host
	if dm.isHostDriver() {
		dm.log.Info("NVIDIA GPU driver is already pre-installed on the node, disabling the containerized driver")
		if err := dm.disableContainerizedDriver(); err != nil {
			return fmt.Errorf("failed to disable containerized driver: %w", err)
		}
		// Wait for pod termination
		time.Sleep(60 * time.Second)
		return fmt.Errorf("driver is pre-installed on host")
	}

	// Fetch current component states
	if err := dm.fetchCurrentLabels(); err != nil {
		return fmt.Errorf("failed to fetch current labels: %w", err)
	}

	// Fetch auto upgrade policy annotation
	if err := dm.fetchAutoUpgradeAnnotation(); err != nil {
		return fmt.Errorf("failed to fetch auto upgrade annotation: %w", err)
	}

	// Always evict all GPU operator components across a driver restart
	if err := dm.evictAllGPUOperatorComponents(); err != nil {
		dm.log.Error("Failed to evict GPU operator components, attempting cleanup")
		dm.cleanupOnFailure()
		return fmt.Errorf("failed to evict GPU operator components: %w", err)
	}

	// Handle GPU pod eviction if enabled
	if dm.isGPUPodEvictionEnabled() {
		if err := dm.handleGPUPodEviction(); err != nil {
			dm.log.Error("Failed to handle GPU pod eviction")
			dm.cleanupOnFailure()
			return fmt.Errorf("failed to handle GPU pod eviction: %w", err)
		}
	}

	// Check if driver is loaded and cleanup if needed
	if dm.isDriverLoaded() {
		if err := dm.cleanupDriver(); err != nil {
			if dm.isAutoDrainEnabled() {
				dm.log.Info("Unable to cleanup driver modules, attempting again with node drain")
				if err := dm.drainK8sNode(); err != nil {
					dm.log.Error("Failed to drain node")
					dm.cleanupOnFailure()
					return fmt.Errorf("failed to drain node: %w", err)
				}
				if err := dm.cleanupDriver(); err != nil {
					dm.log.Error("Failed to cleanup NVIDIA driver")
					dm.cleanupOnFailure()
					return fmt.Errorf("failed to cleanup NVIDIA driver: %w", err)
				}
			} else {
				dm.log.Error("Failed to uninstall nvidia driver components")
				dm.cleanupOnFailure()
				return fmt.Errorf("failed to uninstall nvidia driver components: %w", err)
			}
		}
		dm.log.Info("Successfully uninstalled nvidia driver components")
	}

	// Handle vfio-pci driver unbinding
	if err := dm.unbindVfioPCI(); err != nil {
		dm.log.Error("Unable to unbind vfio-pci driver from all devices")
		dm.cleanupOnFailure()
		return fmt.Errorf("failed to unbind vfio-pci driver: %w", err)
	}

	// Handle GPUDirect RDMA if enabled
	if dm.isGPUDirectRDMAEnabled() {
		dm.log.Info("GPUDirectRDMA is enabled, validating MOFED driver installation")
		if err := dm.waitForMofedDriver(); err != nil {
			return fmt.Errorf("failed to wait for MOFED driver: %w", err)
		}
	}

	// Cleanup and reschedule components
	if dm.isGPUPodEvictionEnabled() || dm.isAutoDrainEnabled() {
		if err := dm.uncordonK8sNode(); err != nil {
			dm.log.Warn("Failed to uncordon node")
		}
	}

	if err := dm.rescheduleGPUOperatorComponents(); err != nil {
		dm.log.Warn("Failed to reschedule GPU operator components")
	}

	// Handle nouveau driver
	if dm.isNouveauLoaded() {
		if err := dm.unloadNouveau(); err != nil {
			return fmt.Errorf("failed to unload nouveau driver: %w", err)
		}
		dm.log.Info("Successfully unloaded nouveau driver")
	}

	dm.log.Info("Driver uninstallation completed successfully")
	return nil
}

func (dm *DriverManager) preflightCheck() error {
	dm.log.Info("Performing preflight checks")
	// TODO: Add checks for driver package availability for current kernel
	// TODO: Add checks for driver dependencies
	// TODO: Add checks for entitlements(OCP)
	dm.log.Info("Preflight checks completed")
	return nil
}

// Helper methods for driver management

func (dm *DriverManager) isHostDriver() bool {
	// Check if driver is pre-installed on the host
	nvmlLibPath := "/host" + nvmlSOPath
	if _, err := os.Stat(nvmlLibPath); err == nil {
		nvmlClient := nvml.NewClient(nvmlLibPath, dm.log)
		if err := nvmlClient.ValidateDriver(); err == nil {
			return true
		}
	}
	return false
}

func (dm *DriverManager) disableContainerizedDriver() error {
	label := "nvidia.com/gpu.deploy.driver=pre-installed"
	dm.log.Infof("Labeling node %s with %s", dm.config.nodeName, label)

	// Get the node
	node, err := dm.clientset.CoreV1().Nodes().Get(dm.ctx, dm.config.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", dm.config.nodeName, err)
	}

	// Add the label
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels["nvidia.com/gpu.deploy.driver"] = "pre-installed"

	// Update the node
	_, err = dm.clientset.CoreV1().Nodes().Update(dm.ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node %s: %w", dm.config.nodeName, err)
	}

	return nil
}

func (dm *DriverManager) fetchCurrentLabels() error {
	dm.log.Info("Fetching current component labels")

	labels := []string{
		"nvidia.com/gpu.deploy.operator-validator",
		"nvidia.com/gpu.deploy.container-toolkit",
		"nvidia.com/gpu.deploy.device-plugin",
		"nvidia.com/gpu.deploy.gpu-feature-discovery",
		"nvidia.com/gpu.deploy.dcgm-exporter",
		"nvidia.com/gpu.deploy.dcgm",
		"nvidia.com/gpu.deploy.mig-manager",
		"nvidia.com/gpu.deploy.nvsm",
		"nvidia.com/gpu.deploy.sandbox-validator",
		"nvidia.com/gpu.deploy.sandbox-device-plugin",
		"nvidia.com/gpu.deploy.vgpu-device-manager",
	}

	for _, label := range labels {
		value, err := dm.getNodeLabelValue(label)
		if err != nil {
			return fmt.Errorf("failed to get label %s: %w", label, err)
		}
		dm.setComponentState(label, value)
	}

	// Handle custom operand node label
	if dm.config.nodeLabelForGPUPodEviction != "" {
		value, err := dm.getNodeLabelValue(dm.config.nodeLabelForGPUPodEviction)
		if err != nil {
			return fmt.Errorf("failed to get custom operand label %s: %w", dm.config.nodeLabelForGPUPodEviction, err)
		}
		dm.components.customOperandNodeLabelValue = value
	}

	return nil
}

func (dm *DriverManager) getNodeLabelValue(label string) (string, error) {
	node, err := dm.clientset.CoreV1().Nodes().Get(dm.ctx, dm.config.nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", dm.config.nodeName, err)
	}

	if node.Labels == nil {
		return "", nil
	}

	value, exists := node.Labels[label]
	if !exists {
		return "", nil
	}

	return value, nil
}

func (dm *DriverManager) setComponentState(label, value string) {
	switch label {
	case "nvidia.com/gpu.deploy.operator-validator":
		dm.components.validatorDeployed = value
	case "nvidia.com/gpu.deploy.container-toolkit":
		dm.components.toolkitDeployed = value
	case "nvidia.com/gpu.deploy.device-plugin":
		dm.components.pluginDeployed = value
	case "nvidia.com/gpu.deploy.gpu-feature-discovery":
		dm.components.gfdDeployed = value
	case "nvidia.com/gpu.deploy.dcgm-exporter":
		dm.components.dcgmExporterDeployed = value
	case "nvidia.com/gpu.deploy.dcgm":
		dm.components.dcgmDeployed = value
	case "nvidia.com/gpu.deploy.mig-manager":
		dm.components.migManagerDeployed = value
	case "nvidia.com/gpu.deploy.nvsm":
		dm.components.nvsmDeployed = value
	case "nvidia.com/gpu.deploy.sandbox-validator":
		dm.components.sandboxValidatorDeployed = value
	case "nvidia.com/gpu.deploy.sandbox-device-plugin":
		dm.components.sandboxPluginDeployed = value
	case "nvidia.com/gpu.deploy.vgpu-device-manager":
		dm.components.vgpuDeviceManagerDeployed = value
	}
}

func (dm *DriverManager) fetchAutoUpgradeAnnotation() error {
	node, err := dm.clientset.CoreV1().Nodes().Get(dm.ctx, dm.config.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", dm.config.nodeName, err)
	}

	if node.Annotations == nil {
		dm.components.autoUpgradePolicyEnabled = ""
	} else {
		dm.components.autoUpgradePolicyEnabled = node.Annotations["nvidia.com/gpu-driver-upgrade-enabled"]
	}

	dm.log.Infof("Current value of AUTO_UPGRADE_POLICY_ENABLED=%s", dm.components.autoUpgradePolicyEnabled)
	return nil
}

func (dm *DriverManager) evictAllGPUOperatorComponents() error {
	dm.log.Info("Shutting down all GPU clients on the current node by disabling their component-specific nodeSelector labels")

	// Get the node
	node, err := dm.clientset.CoreV1().Nodes().Get(dm.ctx, dm.config.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", dm.config.nodeName, err)
	}

	// Prepare labels to update
	labels := map[string]string{
		"nvidia.com/gpu.deploy.operator-validator":    dm.maybeSetPaused(dm.components.validatorDeployed),
		"nvidia.com/gpu.deploy.container-toolkit":     dm.maybeSetPaused(dm.components.toolkitDeployed),
		"nvidia.com/gpu.deploy.device-plugin":         dm.maybeSetPaused(dm.components.pluginDeployed),
		"nvidia.com/gpu.deploy.gpu-feature-discovery": dm.maybeSetPaused(dm.components.gfdDeployed),
		"nvidia.com/gpu.deploy.dcgm-exporter":         dm.maybeSetPaused(dm.components.dcgmExporterDeployed),
		"nvidia.com/gpu.deploy.dcgm":                  dm.maybeSetPaused(dm.components.dcgmDeployed),
		"nvidia.com/gpu.deploy.nvsm":                  dm.maybeSetPaused(dm.components.nvsmDeployed),
		"nvidia.com/gpu.deploy.sandbox-validator":     dm.maybeSetPaused(dm.components.sandboxValidatorDeployed),
		"nvidia.com/gpu.deploy.sandbox-device-plugin": dm.maybeSetPaused(dm.components.sandboxPluginDeployed),
		"nvidia.com/gpu.deploy.vgpu-device-manager":   dm.maybeSetPaused(dm.components.vgpuDeviceManagerDeployed),
	}

	if dm.components.migManagerDeployed != "" {
		labels["nvidia.com/gpu.deploy.mig-manager"] = dm.maybeSetPaused(dm.components.migManagerDeployed)
	}

	// Update node labels
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	for k, v := range labels {
		node.Labels[k] = v
	}

	// Handle custom operand node selector label
	if dm.components.customOperandNodeLabelValue != "" {
		labelValue := dm.maybeSetPaused(dm.components.customOperandNodeLabelValue)
		node.Labels[dm.config.nodeLabelForGPUPodEviction] = labelValue
	}

	// Update the node
	_, err = dm.clientset.CoreV1().Nodes().Update(dm.ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node labels: %w", err)
	}

	// Wait for pods to terminate
	return dm.waitForPodsToTerminate()
}

func (dm *DriverManager) maybeSetPaused(currentValue string) string {
	if currentValue == "" {
		return ""
	} else if currentValue == "false" {
		return "false"
	} else if currentValue == "true" {
		return pausedStr
	} else if strings.Contains(currentValue, pausedStr) {
		return currentValue
	} else {
		return currentValue + "_" + pausedStr
	}
}

func (dm *DriverManager) waitForPodsToTerminate() error {
	podSelectors := []struct {
		app     string
		timeout time.Duration
	}{
		{"nvidia-operator-validator", defaultGracePeriod},
		{"nvidia-container-toolkit-daemonset", defaultGracePeriod},
		{"nvidia-device-plugin-daemonset", defaultGracePeriod},
		{"gpu-feature-discovery", defaultGracePeriod},
		{"nvidia-dcgm-exporter", defaultGracePeriod},
		{"nvidia-dcgm", defaultGracePeriod},
	}

	for _, selector := range podSelectors {
		dm.log.Infof("Waiting for %s to shutdown", selector.app)
		if err := dm.waitForPodTermination(selector.app, selector.timeout); err != nil {
			dm.log.Warnf("Failed to wait for %s to shutdown: %v", selector.app, err)
		}
	}

	// Handle optional components
	if dm.components.migManagerDeployed != "" {
		dm.log.Info("Waiting for mig-manager to shutdown")
		if err := dm.waitForPodTermination("nvidia-mig-manager", defaultGracePeriod); err != nil {
			dm.log.Warn("Failed to wait for mig-manager to shutdown")
		}
	}

	if dm.components.sandboxValidatorDeployed != "" {
		dm.log.Info("Waiting for sandbox-validator to shutdown")
		if err := dm.waitForPodTermination("nvidia-sandbox-validator", defaultGracePeriod); err != nil {
			dm.log.Warn("Failed to wait for sandbox-validator to shutdown")
		}
	}

	if dm.components.sandboxPluginDeployed != "" {
		dm.log.Info("Waiting for sandbox-device-plugin to shutdown")
		if err := dm.waitForPodTermination("nvidia-sandbox-device-plugin-daemonset", defaultGracePeriod); err != nil {
			dm.log.Warn("Failed to wait for sandbox-device-plugin to shutdown")
		}
	}

	if dm.components.vgpuDeviceManagerDeployed != "" {
		dm.log.Info("Waiting for vgpu-device-manager to shutdown")
		if err := dm.waitForPodTermination("nvidia-vgpu-device-manager", defaultGracePeriod); err != nil {
			dm.log.Warn("Failed to wait for vgpu-device-manager to shutdown")
		}
	}

	return nil
}

func (dm *DriverManager) waitForPodTermination(appLabel string, timeout time.Duration) error {
	selector := labels.SelectorFromSet(labels.Set{"app": appLabel})

	return wait.PollUntilContextTimeout(dm.ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := dm.clientset.CoreV1().Pods(dm.config.operatorNamespace).List(dm.ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
			FieldSelector: "spec.nodeName=" + dm.config.nodeName,
		})
		if err != nil {
			return false, err
		}

		// Return true if no pods are found (all terminated)
		return len(pods.Items) == 0, nil
	})
}

func (dm *DriverManager) handleGPUPodEviction() error {
	if err := dm.cordonK8sNode(); err != nil {
		return fmt.Errorf("failed to cordon node: %w", err)
	}

	if err := dm.nvdrainK8sNode(); err != nil {
		dm.log.Error("Failed to drain node of GPU pods")
		if !dm.isAutoDrainEnabled() {
			return fmt.Errorf("cannot proceed until all GPU pods are drained from the node")
		}
		dm.log.Info("Attempting node drain")
		if err := dm.drainK8sNode(); err != nil {
			return fmt.Errorf("failed to drain node: %w", err)
		}
		if err := dm.cleanupDriver(); err != nil {
			return fmt.Errorf("failed to cleanup NVIDIA driver: %w", err)
		}
	}

	return nil
}

func (dm *DriverManager) cordonK8sNode() error {
	dm.log.Infof("Cordoning node %s", dm.config.nodeName)

	// Get the node
	node, err := dm.clientset.CoreV1().Nodes().Get(dm.ctx, dm.config.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", dm.config.nodeName, err)
	}

	// Set the unschedulable flag
	node.Spec.Unschedulable = true

	// Update the node
	_, err = dm.clientset.CoreV1().Nodes().Update(dm.ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to cordon node %s: %w", dm.config.nodeName, err)
	}

	return nil
}

func (dm *DriverManager) uncordonK8sNode() error {
	dm.log.Infof("Uncordoning node %s", dm.config.nodeName)

	// Get the node
	node, err := dm.clientset.CoreV1().Nodes().Get(dm.ctx, dm.config.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", dm.config.nodeName, err)
	}

	// Clear the unschedulable flag
	node.Spec.Unschedulable = false

	// Update the node
	_, err = dm.clientset.CoreV1().Nodes().Update(dm.ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to uncordon node %s: %w", dm.config.nodeName, err)
	}

	return nil
}

func (dm *DriverManager) nvdrainK8sNode() error {
	dm.log.Infof("Draining node %s of any GPU pods", dm.config.nodeName)

	customDrainFilter := func(pod corev1.Pod) drain.PodDeleteStatus {
		deletePod := gpuPodSpecFilter(pod)
		if !deletePod {
			return drain.MakePodDeleteStatusSkip()
		}
		return drain.MakePodDeleteStatusOkay()
	}

	drainHelper := drain.Helper{
		Ctx:                 dm.ctx,
		Client:              dm.clientset,
		Out:                 os.Stdout,
		ErrOut:              os.Stderr,
		ChunkSize:           cmdutil.DefaultChunkSize,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  dm.config.drainDeleteEmptyDirData,
		Force:               dm.config.drainUseForce,
		Timeout:             dm.config.drainTimeout,
		AdditionalFilters:   []drain.PodFilter{customDrainFilter},
	}

	dm.log.Infof("Identifying GPU pods to delete")

	// List all pods
	podList, err := dm.clientset.CoreV1().Pods("").List(
		dm.ctx,
		metav1.ListOptions{FieldSelector: "spec.nodeName=" + dm.config.nodeName},
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
		dm.log.Infof("No GPU pods to delete. Exiting.")
		return nil
	}

	podDeleteList, errs := drainHelper.GetPodsForDeletion(dm.config.nodeName)
	numPodsCanDelete := len(podDeleteList.Pods())
	if numPodsCanDelete != numPodsToDelete {
		dm.log.Error("Cannot delete all GPU pods")
		for _, err := range errs {
			dm.log.Errorf("error reported by drain helper: %v", err)
		}
		return fmt.Errorf("failed to delete all GPU pods")
	}

	for _, p := range podDeleteList.Pods() {
		dm.log.Infof("GPU pod - %s/%s", p.Namespace, p.Name)
	}

	dm.log.Info("Deleting GPU pods...")
	err = drainHelper.DeleteOrEvictPods(podDeleteList.Pods())
	if err != nil {
		return fmt.Errorf("failed to delete all GPU pods: %w", err)
	}

	return nil
}

func (dm *DriverManager) drainK8sNode() error {
	dm.log.Infof("Draining node %s", dm.config.nodeName)

	drainHelper := &drain.Helper{
		Ctx:                dm.ctx,
		Client:             dm.clientset,
		Force:              dm.config.drainUseForce,
		DeleteEmptyDirData: dm.config.drainDeleteEmptyDirData,
		Timeout:            dm.config.drainTimeout,
	}

	if dm.config.drainPodSelectorLabel != "" {
		drainHelper.PodSelector = dm.config.drainPodSelectorLabel
	}

	return drain.RunNodeDrain(drainHelper, dm.config.nodeName)
}

func (dm *DriverManager) isDriverLoaded() bool {
	_, err := os.Stat("/sys/module/nvidia/refcnt")
	return err == nil
}

func (dm *DriverManager) isNouveauLoaded() bool {
	_, err := os.Stat("/sys/module/nouveau/refcnt")
	return err == nil
}

func (dm *DriverManager) unloadNouveau() error {
	dm.log.Info("Unloading nouveau driver")
	return unix.DeleteModule("nouveau", 0)
}

func (dm *DriverManager) cleanupDriver() error {
	dm.log.Info("Cleaning up NVIDIA driver")

	// Unload driver modules
	if err := dm.unloadDriver(); err != nil {
		return fmt.Errorf("failed to unload driver: %w", err)
	}

	// Unmount rootfs
	if err := dm.unmountRootfs(); err != nil {
		return fmt.Errorf("failed to unmount rootfs: %w", err)
	}

	// Remove PID file
	if _, err := os.Stat(driverPIDFile); err == nil {
		if err := os.Remove(driverPIDFile); err != nil {
			dm.log.Warnf("Failed to remove PID file %s: %v", driverPIDFile, err)
		}
	}

	return nil
}

func (dm *DriverManager) unloadDriver() error {
	dm.log.Info("Unloading NVIDIA driver kernel modules")

	modules := []string{
		"nvidia_modeset",
		"nvidia_uvm",
		"nvidia_peermem",
		"nvidia_fs",
		"nvidia_vgpu_vfio",
		"gdrdrv",
		"nvidia",
	}

	for _, module := range modules {
		if _, err := os.Stat(fmt.Sprintf("/sys/module/%s/refcnt", module)); err == nil {
			if err := unix.DeleteModule(module, 0); err != nil {
				dm.log.Warnf("Failed to unload kernel module %s: %v", module, err)
				return err
			}
		}
	}

	return nil
}

func (dm *DriverManager) unmountRootfs() error {
	dm.log.Info("Unmounting NVIDIA driver rootfs")

	// Check if the mount point exists
	if _, err := os.Stat(driverRoot); os.IsNotExist(err) {
		dm.log.Info("Driver root directory does not exist, nothing to unmount")
		return nil
	}

	// Recursively unmount all mounts under the driver root
	if err := mount.RecursiveUnmount(driverRoot); err != nil {
		return fmt.Errorf("failed to recursively unmount %s: %w", driverRoot, err)
	}

	dm.log.Infof("Successfully unmounted %s and all its submounts", driverRoot)
	return nil
}

func (dm *DriverManager) unbindVfioPCI() error {
	dm.log.Info("Unbinding vfio-pci driver from all devices")
	cmd := exec.Command("vfio-manage", "unbind", "--all")
	return cmd.Run()
}

func (dm *DriverManager) isGPUDirectRDMAEnabled() bool {
	if !dm.config.gpuDirectRDMAEnabled {
		return false
	}
	return dm.mellanoxDevicesPresent()
}

func (dm *DriverManager) mellanoxDevicesPresent() bool {
	entries, err := os.ReadDir("/sys/bus/pci/devices")
	if err != nil {
		return false
	}

	for _, entry := range entries {
		vendorFile := filepath.Join("/sys/bus/pci/devices", entry.Name(), "vendor")
		if data, err := os.ReadFile(vendorFile); err == nil {
			if strings.TrimSpace(string(data)) == "0x15b3" {
				dm.log.Infof("Mellanox device found at %s", entry.Name())
				return true
			}
		}
	}

	dm.log.Info("No Mellanox devices were found")
	return false
}

func (dm *DriverManager) waitForMofedDriver() error {
	dm.log.Info("Waiting for MOFED to be installed")

	var mofedCheck func() bool
	if dm.config.useHostMofed {
		mofedCheck = func() bool {
			_, err := os.Stat("/run/mellanox/drivers/.driver-ready")
			return err == nil
		}
	} else {
		mofedCheck = func() bool {
			loadedModules, err := os.ReadFile("/proc/modules")
			if err != nil {
				dm.log.Warnf("Failed to read /proc/modules: %v", err)
				return false
			}
			return strings.Contains(string(loadedModules), "mlx5_core")
		}
	}

	for !mofedCheck() {
		dm.log.Info("Waiting for MOFED to be installed...")
		time.Sleep(5 * time.Second)
	}

	return nil
}

func (dm *DriverManager) rescheduleGPUOperatorComponents() error {
	dm.log.Info("Rescheduling all GPU clients on the current node by enabling their component-specific nodeSelector labels")

	// Get the node
	node, err := dm.clientset.CoreV1().Nodes().Get(dm.ctx, dm.config.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", dm.config.nodeName, err)
	}

	// Prepare labels to update
	labels := map[string]string{
		"nvidia.com/gpu.deploy.operator-validator":    dm.maybeSetTrue(dm.components.validatorDeployed),
		"nvidia.com/gpu.deploy.container-toolkit":     dm.maybeSetTrue(dm.components.toolkitDeployed),
		"nvidia.com/gpu.deploy.device-plugin":         dm.maybeSetTrue(dm.components.pluginDeployed),
		"nvidia.com/gpu.deploy.gpu-feature-discovery": dm.maybeSetTrue(dm.components.gfdDeployed),
		"nvidia.com/gpu.deploy.dcgm-exporter":         dm.maybeSetTrue(dm.components.dcgmExporterDeployed),
		"nvidia.com/gpu.deploy.dcgm":                  dm.maybeSetTrue(dm.components.dcgmDeployed),
		"nvidia.com/gpu.deploy.nvsm":                  dm.maybeSetTrue(dm.components.nvsmDeployed),
		"nvidia.com/gpu.deploy.sandbox-validator":     dm.maybeSetTrue(dm.components.sandboxValidatorDeployed),
		"nvidia.com/gpu.deploy.sandbox-device-plugin": dm.maybeSetTrue(dm.components.sandboxPluginDeployed),
		"nvidia.com/gpu.deploy.vgpu-device-manager":   dm.maybeSetTrue(dm.components.vgpuDeviceManagerDeployed),
	}

	if dm.components.migManagerDeployed != "" {
		labels["nvidia.com/gpu.deploy.mig-manager"] = dm.maybeSetTrue(dm.components.migManagerDeployed)
	}

	// Update node labels
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	for k, v := range labels {
		node.Labels[k] = v
	}

	// Handle custom operand node selector label
	if dm.components.customOperandNodeLabelValue != "" {
		labelValue := dm.maybeSetTrue(dm.components.customOperandNodeLabelValue)
		node.Labels[dm.config.nodeLabelForGPUPodEviction] = labelValue
	}

	// Update the node
	_, err = dm.clientset.CoreV1().Nodes().Update(dm.ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node labels: %w", err)
	}

	return nil
}

func (dm *DriverManager) maybeSetTrue(currentValue string) string {
	switch currentValue {
	case "false":
		return "false"
	case pausedStr:
		return "true"
	default:
		re := regexp.MustCompile(pausedStr + "_?")
		result := re.ReplaceAllString(currentValue, "")
		return strings.Trim(result, "_")
	}
}

// Policy and feature check methods

func (dm *DriverManager) isAutoDrainEnabled() bool {
	if dm.isDriverAutoUpgradePolicyEnabled() {
		dm.log.Info("Auto drain of the node is disabled by the upgrade policy")
		return false
	}
	return dm.config.enableAutoDrain
}

func (dm *DriverManager) isGPUPodEvictionEnabled() bool {
	if dm.isDriverAutoUpgradePolicyEnabled() {
		dm.log.Info("Auto eviction of GPU pods on node is disabled by the upgrade policy")
		return false
	}
	return dm.config.enableGPUPodEviction
}

func (dm *DriverManager) isDriverAutoUpgradePolicyEnabled() bool {
	if dm.components.autoUpgradePolicyEnabled == "true" {
		return true
	}
	dm.log.Info("Auto upgrade policy of the GPU driver on the node is disabled")
	return false
}

func (dm *DriverManager) cleanupOnFailure() {
	dm.log.Info("Performing cleanup on failure")

	if dm.isGPUPodEvictionEnabled() || dm.isAutoDrainEnabled() {
		if err := dm.uncordonK8sNode(); err != nil {
			dm.log.Warn("Failed to uncordon node during cleanup")
		}
	}

	if err := dm.rescheduleGPUOperatorComponents(); err != nil {
		dm.log.Warn("Failed to reschedule GPU operator components during cleanup")
	}
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
