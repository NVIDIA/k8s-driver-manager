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

import (
	"context"
	"errors"
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

	"github.com/NVIDIA/k8s-driver-manager/internal/info"
	kube "github.com/NVIDIA/k8s-driver-manager/internal/kubernetes"
	"github.com/NVIDIA/k8s-driver-manager/internal/linuxutils"
)

const (
	driverRoot          = "/run/nvidia/driver"
	driverPIDFile       = "/run/nvidia/nvidia-driver.pid"
	operatorNamespace   = "gpu-operator"
	pausedStr           = "paused-for-driver-upgrade"
	defaultDrainTimeout = time.Second * 0
	defaultGracePeriod  = 5 * time.Minute

	nvidiaDomainPrefix = "nvidia.com"

	nvidiaDriverDeployLabel              = nvidiaDomainPrefix + "/" + "gpu.deploy.driver"
	nvidiaOperatorValidatorDeployLabel   = nvidiaDomainPrefix + "/" + "gpu.deploy.operator-validator"
	nvidiaContainerToolkitDeployLabel    = nvidiaDomainPrefix + "/" + "gpu.deploy.container-toolkit"
	nvidiaDevicePluginDeployLabel        = nvidiaDomainPrefix + "/" + "gpu.deploy.device-plugin"
	nvidiaGFDDeployLabel                 = nvidiaDomainPrefix + "/" + "gpu.deploy.gpu-feature-discovery"
	nvidiaDCGMExporterDeployLabel        = nvidiaDomainPrefix + "/" + "gpu.deploy.dcgm-exporter"
	nvidiaDCGMDeployLabel                = nvidiaDomainPrefix + "/" + "gpu.deploy.dcgm"
	nvidiaMIGManagerDeployLabel          = nvidiaDomainPrefix + "/" + "gpu.deploy.mig-manager"
	nvidiaNVSMDeployLabel                = nvidiaDomainPrefix + "/" + "gpu.deploy.nvsm"
	nvidiaSandboxValidatorDeployLabel    = nvidiaDomainPrefix + "/" + "gpu.deploy.sandbox-validator"
	nvidiaSandboxDevicePluginDeployLabel = nvidiaDomainPrefix + "/" + "gpu.deploy.sandbox-device-plugin"
	nvidiaVGPUDeviceManagerDeployLabel   = nvidiaDomainPrefix + "/" + "gpu.deploy.vgpu-device-manager"
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
	driverVersion              string
	forceReinstall             bool
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
	kubeClient *kube.Client
	log        *logrus.Logger
}

func main() {
	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		DisableQuote:  true,
	})

	cfg := &config{}
	components := &componentState{}

	app := cli.NewApp()
	app.Name = "driver-manager"
	app.Usage = "The NVIDIA Driver Manager is a Kubernetes component which assists in the seamless upgrades of NVIDIA  " +
		"GPU Driver Container on each node of the cluster. This component ensures that all pre-requisites are met before driver " +
		"upgrades can be performed on the gpu driver container."
	app.Version = info.GetVersionString()

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
			Usage:       "Namespace where the GPU operator is installed in",
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
		&cli.StringFlag{
			Name:        "driver-version",
			Usage:       "Desired NVIDIA driver version",
			Destination: &cfg.driverVersion,
			EnvVars:     []string{"DRIVER_VERSION"},
			Value:       "",
		},
		&cli.BoolFlag{
			Name:        "force-reinstall",
			Usage:       "Force driver reinstall regardless of current state",
			Destination: &cfg.forceReinstall,
			EnvVars:     []string{"FORCE_REINSTALL"},
			Value:       false,
		},
	}

	app.Commands = []*cli.Command{
		{
			Name:  "uninstall_driver",
			Usage: "Uninstall NVIDIA driver and manage GPU operator components",
			Action: func(c *cli.Context) error {
				dm, err := newDriverManager(c.Context, cfg, components, log)
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
				dm, err := newDriverManager(c.Context, cfg, components, log)
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

func newDriverManager(ctx context.Context, cfg *config, components *componentState, log *logrus.Logger) (*DriverManager, error) {
	driverManager := &DriverManager{
		ctx:        ctx,
		config:     cfg,
		components: components,
		log:        log,
	}

	kubeClient, err := kube.NewClient(ctx, cfg.kubeconfig, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	driverManager.kubeClient = kubeClient

	return driverManager, nil
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

	if skip, reason := dm.shouldSkipUninstall(); skip {
		dm.log.Infof("Skipping driver uninstall: %s", reason)
		return nil
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

	drainOpts := kube.DrainOptions{
		Force:              dm.config.drainUseForce,
		DeleteEmptyDirData: dm.config.drainDeleteEmptyDirData,
		Timeout:            dm.config.drainTimeout,
		PodSelector:        dm.config.drainPodSelectorLabel,
	}

	// Delete any GPU pods running on the node
	if dm.isGPUPodEvictionEnabled() {
		if err := dm.kubeClient.CordonNode(dm.config.nodeName); err != nil {
			return fmt.Errorf("failed to cordon node: %w", err)
		}

		if err := dm.nvDrainNode(); err != nil {
			dm.log.Info("Failed to drain node of GPU pods")
			if !dm.isAutoDrainEnabled() {
				dm.cleanupOnFailure()
				return fmt.Errorf("cannot proceed until all GPU pods are drained from the node")
			}
			dm.log.Info("Attempting node drain")
			if err := dm.kubeClient.DrainNode(dm.config.nodeName, drainOpts); err != nil {
				dm.cleanupOnFailure()
				return fmt.Errorf("failed to drain node: %w", err)
			}
			if err := dm.cleanupDriver(); err != nil {
				dm.cleanupOnFailure()
				return fmt.Errorf("failed to cleanup NVIDIA driver: %w", err)
			}
		}
	}

	// Check if driver is loaded and cleanup if needed
	if dm.isDriverLoaded() {
		if err := dm.cleanupDriver(); err != nil {
			if dm.isAutoDrainEnabled() {
				dm.log.Info("Unable to cleanup driver modules, attempting again with node drain...")

				if err := dm.kubeClient.DrainNode(dm.config.nodeName, drainOpts); err != nil {
					dm.cleanupOnFailure()
					return fmt.Errorf("failed to drain node: %w", err)
				}
				if err := dm.cleanupDriver(); err != nil {
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
	// When GPUDirectRDMA is enabled, wait until MOFED driver has finished installing
	if dm.isGPUDirectRDMAEnabled() {
		dm.log.Info("GPUDirectRDMA is enabled, validating MOFED driver installation")
		if err := dm.waitForMofedDriver(); err != nil {
			return fmt.Errorf("failed to wait for MOFED driver: %w", err)
		}
	}

	// Cleanup and reschedule components
	if dm.isGPUPodEvictionEnabled() || dm.isAutoDrainEnabled() {
		if err := dm.kubeClient.UncordonNode(dm.config.nodeName); err != nil {
			dm.log.Warn("Failed to uncordon node")
		}
	}

	if err := dm.rescheduleGPUOperatorComponents(); err != nil {
		dm.log.Warnf("Failed to reschedule GPU operator components: %v", err)
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
	cmd := exec.Command("chroot", "/host", "nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	if len(out) > 0 {
		dm.log.Infof("Host driver detected: %s", out)
		return true
	}
	return false
}

func (dm *DriverManager) disableContainerizedDriver() error {
	dm.log.Infof("Labeling node %s with %s=%s", dm.config.nodeName, nvidiaDriverDeployLabel, "pre-installed")

	// Add the label
	operandLabels := map[string]string{
		nvidiaDriverDeployLabel: "pre-installed",
	}

	return dm.kubeClient.UpdateNodeLabels(dm.config.nodeName, operandLabels)
}

func (dm *DriverManager) fetchCurrentLabels() error {
	dm.log.Info("Fetching current component labels")

	operandLabels := []string{
		nvidiaOperatorValidatorDeployLabel,
		nvidiaContainerToolkitDeployLabel,
		nvidiaDevicePluginDeployLabel,
		nvidiaGFDDeployLabel,
		nvidiaDCGMExporterDeployLabel,
		nvidiaDCGMDeployLabel,
		nvidiaMIGManagerDeployLabel,
		nvidiaNVSMDeployLabel,
		nvidiaSandboxValidatorDeployLabel,
		nvidiaSandboxDevicePluginDeployLabel,
		nvidiaVGPUDeviceManagerDeployLabel,
	}

	for _, label := range operandLabels {
		dm.log.Infof("Getting current value of the %q node label", label)
		value, err := dm.kubeClient.GetNodeLabelValue(dm.config.nodeName, label)
		if err != nil {
			return fmt.Errorf("failed to get label %s: %w", label, err)
		}
		dm.log.Infof("Current value of %q=%s", label, value)
		dm.setComponentState(label, value)
	}

	// Handle custom operand node label
	if dm.config.nodeLabelForGPUPodEviction != "" {
		dm.log.Infof("Getting current value of the %q node label used by custom operands", dm.config.nodeLabelForGPUPodEviction)
		value, err := dm.kubeClient.GetNodeLabelValue(dm.config.nodeName, dm.config.nodeLabelForGPUPodEviction)
		if err != nil {
			return fmt.Errorf("failed to get custom operand label %s: %w", dm.config.nodeLabelForGPUPodEviction, err)
		}
		dm.log.Infof("Current value of %q=%s", dm.config.nodeLabelForGPUPodEviction, value)
		dm.components.customOperandNodeLabelValue = value
	}

	return nil
}

func (dm *DriverManager) setComponentState(label, value string) {
	switch label {
	case nvidiaOperatorValidatorDeployLabel:
		dm.components.validatorDeployed = value
	case nvidiaContainerToolkitDeployLabel:
		dm.components.toolkitDeployed = value
	case nvidiaDevicePluginDeployLabel:
		dm.components.pluginDeployed = value
	case nvidiaGFDDeployLabel:
		dm.components.gfdDeployed = value
	case nvidiaDCGMExporterDeployLabel:
		dm.components.dcgmExporterDeployed = value
	case nvidiaDCGMDeployLabel:
		dm.components.dcgmDeployed = value
	case nvidiaMIGManagerDeployLabel:
		dm.components.migManagerDeployed = value
	case nvidiaNVSMDeployLabel:
		dm.components.nvsmDeployed = value
	case nvidiaSandboxValidatorDeployLabel:
		dm.components.sandboxValidatorDeployed = value
	case nvidiaSandboxDevicePluginDeployLabel:
		dm.components.sandboxPluginDeployed = value
	case nvidiaVGPUDeviceManagerDeployLabel:
		dm.components.vgpuDeviceManagerDeployed = value
	}
}

func (dm *DriverManager) fetchAutoUpgradeAnnotation() error {
	annotationValue, err := dm.kubeClient.GetNodeAnnotationValue(dm.config.nodeName,
		"nvidia.com/gpu-driver-upgrade-enabled")
	if err != nil {
		return fmt.Errorf("failed to get node %s annotation: %w", dm.config.nodeName, err)
	}

	dm.components.autoUpgradePolicyEnabled = annotationValue

	dm.log.Infof("Current value of AUTO_UPGRADE_POLICY_ENABLED=%s", dm.components.autoUpgradePolicyEnabled)
	return nil
}

func (dm *DriverManager) evictAllGPUOperatorComponents() error {
	dm.log.Info("Shutting down all GPU clients on the current node by disabling their component-specific nodeSelector labels")

	// Prepare labels to update
	operandLabels := map[string]string{
		nvidiaOperatorValidatorDeployLabel:   dm.maybeSetPaused(dm.components.validatorDeployed),
		nvidiaContainerToolkitDeployLabel:    dm.maybeSetPaused(dm.components.toolkitDeployed),
		nvidiaDevicePluginDeployLabel:        dm.maybeSetPaused(dm.components.pluginDeployed),
		nvidiaGFDDeployLabel:                 dm.maybeSetPaused(dm.components.gfdDeployed),
		nvidiaDCGMExporterDeployLabel:        dm.maybeSetPaused(dm.components.dcgmExporterDeployed),
		nvidiaDCGMDeployLabel:                dm.maybeSetPaused(dm.components.dcgmDeployed),
		nvidiaNVSMDeployLabel:                dm.maybeSetPaused(dm.components.nvsmDeployed),
		nvidiaSandboxValidatorDeployLabel:    dm.maybeSetPaused(dm.components.sandboxValidatorDeployed),
		nvidiaSandboxDevicePluginDeployLabel: dm.maybeSetPaused(dm.components.sandboxPluginDeployed),
		nvidiaVGPUDeviceManagerDeployLabel:   dm.maybeSetPaused(dm.components.vgpuDeviceManagerDeployed),
	}

	if dm.components.migManagerDeployed != "" {
		operandLabels[nvidiaMIGManagerDeployLabel] = dm.maybeSetPaused(dm.components.migManagerDeployed)
	}

	// Handle custom operand node selector label
	if dm.components.customOperandNodeLabelValue != "" {
		dm.log.Infof("Shutting down GPU clients using node selector label %q=%s", dm.config.nodeLabelForGPUPodEviction, dm.components.customOperandNodeLabelValue)
		operandLabels[dm.config.nodeLabelForGPUPodEviction] = dm.maybeSetPaused(dm.components.customOperandNodeLabelValue)
	}

	// Update the node
	err := dm.kubeClient.UpdateNodeLabels(dm.config.nodeName, operandLabels)
	if err != nil {
		return err
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

	namespace := dm.config.operatorNamespace
	nodeName := dm.config.nodeName

	for _, selector := range podSelectors {
		dm.log.Infof("Waiting for %s to shutdown", selector.app)
		selectorMap := map[string]string{
			"app": selector.app,
		}
		if err := dm.kubeClient.WaitForPodTermination(selectorMap, namespace, nodeName, selector.timeout); err != nil {
			dm.log.Errorf("Failed to wait for %s to shutdown: %v", selector.app, err)
			return err
		}
	}

	// Handle optional components
	if dm.components.migManagerDeployed != "" {
		dm.log.Info("Waiting for mig-manager to shutdown")
		selectorMap := map[string]string{
			"app": "nvidia-mig-manager",
		}
		if err := dm.kubeClient.WaitForPodTermination(selectorMap, namespace, nodeName, defaultGracePeriod); err != nil {
			dm.log.Errorf("Failed to wait for mig-manager to shutdown: %v", err)
			return err
		}
	}

	if dm.components.sandboxValidatorDeployed != "" {
		dm.log.Info("Waiting for sandbox-validator to shutdown")
		selectorMap := map[string]string{
			"app": "nvidia-sandbox-validator",
		}
		if err := dm.kubeClient.WaitForPodTermination(selectorMap, namespace, nodeName, defaultGracePeriod); err != nil {
			dm.log.Errorf("Failed to wait for sandbox-validator to shutdown: %v", err)
			return err
		}
	}

	if dm.components.sandboxPluginDeployed != "" {
		dm.log.Info("Waiting for sandbox-device-plugin to shutdown")
		selectorMap := map[string]string{
			"app": "nvidia-sandbox-device-plugin-daemonset",
		}
		if err := dm.kubeClient.WaitForPodTermination(selectorMap, namespace, nodeName, defaultGracePeriod); err != nil {
			dm.log.Errorf("Failed to wait for sandbox-device-plugin to shutdown: %v", err)
			return err
		}
	}

	if dm.components.vgpuDeviceManagerDeployed != "" {
		dm.log.Info("Waiting for vgpu-device-manager to shutdown")
		selectorMap := map[string]string{
			"app": "nvidia-vgpu-device-manager",
		}
		if err := dm.kubeClient.WaitForPodTermination(selectorMap, namespace, nodeName, defaultGracePeriod); err != nil {
			dm.log.Errorf("Failed to wait for vgpu-device-manager to shutdown: %v", err)
			return err
		}
	}

	return nil
}

func (dm *DriverManager) isDriverLoaded() bool {
	_, err := os.Stat("/sys/module/nvidia/refcnt")
	return err == nil
}

func (dm *DriverManager) shouldSkipUninstall() (bool, string) {
	if dm.config.forceReinstall {
		dm.log.Info("Force reinstall is enabled, proceeding with driver uninstall")
		return false, ""
	}

	if !dm.isDriverLoaded() {
		return false, ""
	}

	if dm.config.driverVersion == "" {
		return false, ""
	}

	version, err := dm.detectCurrentDriverVersion()
	if err != nil {
		dm.log.Warnf("Unable to determine installed driver version: %v", err)
		// If driver is loaded but we can't detect version, proceed with reinstall to ensure correct version
		dm.log.Info("Cannot verify driver version, proceeding with reinstall to ensure correct version is installed")
		return false, ""
	}

	if version != dm.config.driverVersion {
		dm.log.Infof("Installed driver version %s does not match desired %s, proceeding with uninstall", version, dm.config.driverVersion)
		return false, ""
	}

	dm.log.Infof("Installed driver version %s matches desired version, skipping uninstall", version)
	return true, "desired version already present"
}

func (dm *DriverManager) detectCurrentDriverVersion() (string, error) {
	baseCtx := dm.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	ctx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
	defer cancel()

	// Try chroot to /run/nvidia/driver for containerized driver
	cmd := exec.CommandContext(ctx, "chroot", "/run/nvidia/driver", "modinfo", "-F", "version", "nvidia")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmdOutput, chrootErr := cmd.Output()
	if chrootErr == nil {
		version := strings.TrimSpace(string(cmdOutput))
		if version != "" {
			dm.log.Infof("Driver version detected via chroot: %s", version)
			return version, nil
		}
	}

	// Second try to read from /sys/module/nvidia/version if available
	if versionData, err := os.ReadFile("/sys/module/nvidia/version"); err == nil {
		version := strings.TrimSpace(string(versionData))
		if version != "" {
			dm.log.Infof("Driver version detected from /sys/module/nvidia/version: %s", version)
			return version, nil
		}
	}

	return "", fmt.Errorf("all version detection methods failed: chroot: %v", chrootErr)
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

	var moduleErrs error
	for _, module := range modules {
		if _, err := os.Stat(fmt.Sprintf("/sys/module/%s/refcnt", module)); err == nil {
			if err := unix.DeleteModule(module, 0); err != nil {
				dm.log.Warnf("Failed to unload kernel module %s: %v", module, err)
				moduleErrs = errors.Join(err)
			}
		}
	}

	if moduleErrs != nil {
		dm.log.Info("Could not unload NVIDIA driver kernel modules, driver is in use")
		km := linuxutils.NewKernelModules(dm.log)
		err := km.List("nvidia")
		if err != nil {
			dm.log.Warnf("Failed to list kernel modules: %v", err)
		}
		return moduleErrs
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

// If vfio-pci driver is in use, ensure we unbind it from all devices.
// If vfio-pci driver is not in use, and we have reached this point, all devices will not be bound to any driver,
// so the below unbind operation will be a no-op.
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

	var isMofedLoaded func() bool
	if dm.config.useHostMofed {
		isMofedLoaded = func() bool {
			_, err := os.Stat("/run/mellanox/drivers/.driver-ready")
			return err == nil
		}
	} else {
		isMofedLoaded = func() bool {
			loadedModules, err := os.ReadFile("/proc/modules")
			if err != nil {
				dm.log.Warnf("Failed to read /proc/modules: %v", err)
				return false
			}
			return strings.Contains(string(loadedModules), "mlx5_core")
		}
	}

	for !isMofedLoaded() {
		dm.log.Info("Waiting for MOFED to be installed...")
		time.Sleep(5 * time.Second)
	}

	return nil
}

func (dm *DriverManager) rescheduleGPUOperatorComponents() error {
	dm.log.Info("Rescheduling all GPU clients on the current node by enabling their component-specific nodeSelector labels")

	// Prepare labels for update
	operandLabels := map[string]string{
		nvidiaOperatorValidatorDeployLabel:   dm.maybeSetTrue(dm.components.validatorDeployed),
		nvidiaContainerToolkitDeployLabel:    dm.maybeSetTrue(dm.components.toolkitDeployed),
		nvidiaDevicePluginDeployLabel:        dm.maybeSetTrue(dm.components.pluginDeployed),
		nvidiaGFDDeployLabel:                 dm.maybeSetTrue(dm.components.gfdDeployed),
		nvidiaDCGMExporterDeployLabel:        dm.maybeSetTrue(dm.components.dcgmExporterDeployed),
		nvidiaDCGMDeployLabel:                dm.maybeSetTrue(dm.components.dcgmDeployed),
		nvidiaNVSMDeployLabel:                dm.maybeSetTrue(dm.components.nvsmDeployed),
		nvidiaSandboxValidatorDeployLabel:    dm.maybeSetTrue(dm.components.sandboxValidatorDeployed),
		nvidiaSandboxDevicePluginDeployLabel: dm.maybeSetTrue(dm.components.sandboxPluginDeployed),
		nvidiaVGPUDeviceManagerDeployLabel:   dm.maybeSetTrue(dm.components.vgpuDeviceManagerDeployed),
	}

	if dm.components.migManagerDeployed != "" {
		operandLabels[nvidiaMIGManagerDeployLabel] = dm.maybeSetTrue(dm.components.migManagerDeployed)
	}

	// Handle custom operand node selector label
	if dm.components.customOperandNodeLabelValue != "" {
		operandLabels[dm.config.nodeLabelForGPUPodEviction] = dm.maybeSetTrue(dm.components.customOperandNodeLabelValue)
	}

	// Update the node
	return dm.kubeClient.UpdateNodeLabels(dm.config.nodeName, operandLabels)
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
		dm.log.Infof("Auto eviction of GPU pods on node %s is disabled by the upgrade policy", dm.config.nodeName)
		return false
	}
	return dm.config.enableGPUPodEviction
}

func (dm *DriverManager) nvDrainNode() error {
	dm.log.Infof("Draining node %s of any GPU pods...", dm.config.nodeName)
	drainOpts := kube.DrainOptions{
		Force:              dm.config.drainUseForce,
		DeleteEmptyDirData: dm.config.drainDeleteEmptyDirData,
		Timeout:            dm.config.drainTimeout,
		PodSelector:        dm.config.drainPodSelectorLabel,
	}

	return dm.kubeClient.DeleteOrEvictPods(dm.config.nodeName, drainOpts)
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
		if err := dm.kubeClient.UncordonNode(dm.config.nodeName); err != nil {
			dm.log.Warn("Failed to uncordon node during cleanup")
		}
	}

	if err := dm.rescheduleGPUOperatorComponents(); err != nil {
		dm.log.Warn("Failed to reschedule GPU operator components during cleanup")
	}
}
