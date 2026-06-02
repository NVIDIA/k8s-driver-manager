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
	"fmt"

	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/NVIDIA/go-nvlib/pkg/nvpassthrough"
)

type bindCommand struct {
	logger        *logrus.Logger
	nvpci         nvpci.Interface
	nvpassthrough nvpassthrough.Interface
	options       bindOptions
}

type bindOptions struct {
	all            bool
	deviceID       string
	libModulesRoot string
	bindNVSwitches bool
}

// newBindCommand constructs a bind command with the specified logger
func newBindCommand(logger *logrus.Logger) *cli.Command {
	c := bindCommand{
		logger: logger,
	}
	return c.build()
}

// build the bind command
func (m bindCommand) build() *cli.Command {
	c := cli.Command{
		Name:  "bind",
		Usage: "Bind device(s) to vfio-pci driver",
		Before: func(c *cli.Context) error {
			return m.validateFlags()
		},
		Action: func(c *cli.Context) error {
			return m.run()
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "all",
				Aliases:     []string{"a"},
				Destination: &m.options.all,
				Usage:       "Bind all NVIDIA devices to vfio-pci",
			},
			&cli.StringFlag{
				Name:        "device-id",
				Aliases:     []string{"d"},
				Destination: &m.options.deviceID,
				Usage:       "Specific device ID to bind (e.g., 0000:01:00.0)",
			},
			&cli.StringFlag{
				Name:    "host-root",
				EnvVars: []string{"HOST_ROOT"},
				Usage:   "DEPRECATED: the host root is no longer required to load the vfio-pci module, please use --lib-modules-root instead",
			},
			&cli.BoolFlag{
				Name:        "bind-nvswitches",
				Destination: &m.options.bindNVSwitches,
				EnvVars:     []string{"BIND_NVSWITCHES"},
				Usage:       "Also bind NVSwitches to vfio-pci (default: false)",
			},
			&cli.StringFlag{
				Name:        "lib-modules-root",
				Destination: &m.options.libModulesRoot,
				EnvVars:     []string{"LIB_MODULES_ROOT"},
				Value:       "/lib/modules",
				Usage:       "Path to the /lib/modules. This is used when loading the vfio-pci module.",
			},
		},
	}

	return &c
}

func (m bindCommand) validateFlags() error {
	if !m.options.all && m.options.deviceID == "" {
		return fmt.Errorf("either --all or --device-id must be specified")
	}

	if m.options.all && m.options.deviceID != "" {
		return fmt.Errorf("cannot specify both --all and --device-id")
	}

	return nil
}

func (m bindCommand) run() error {
	m.nvpci = nvpci.New(
		nvpci.WithLogger(m.logger),
	)

	m.nvpassthrough = nvpassthrough.New(
		nvpassthrough.WithLogger(m.logger),
		nvpassthrough.WithLibModulesRoot(m.options.libModulesRoot),
		nvpassthrough.WithNvpciLib(m.nvpci),
		nvpassthrough.WithLoadKernelModules(true),
	)

	if m.options.deviceID != "" {
		return m.bindDevice()
	}

	return m.bindAll()
}

func (m bindCommand) bindAll() error {
	devices, err := m.nvpci.GetGPUs()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPUs: %w", err)
	}

	if m.options.bindNVSwitches {
		nvswitches, err := m.nvpci.GetNVSwitches()
		if err != nil {
			return fmt.Errorf("failed to get NVIDIA NVSwitches: %w", err)
		}
		devices = append(devices, nvswitches...)
	}

	for _, dev := range devices {
		m.logger.Infof("Binding device %s", dev.Address)
		if err := m.nvpassthrough.BindToVFIODriver(dev.Address); err != nil {
			m.logger.Warnf("Failed to bind device %s: %v", dev.Address, err)
		}
	}

	return nil
}

func (m bindCommand) bindDevice() error {
	device := m.options.deviceID
	// Note: Despite its name, GetGPUByPciBusID returns any NVIDIA PCI device
	// (GPU, NVSwitch, etc.) at the specified address, not just GPUs.
	nvdev, err := m.nvpci.GetGPUByPciBusID(device)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA device: %w", err)
	}
	if nvdev == nil {
		m.logger.Infof("Device %s is not an NVIDIA device", device)
		return nil
	}
	if nvdev.IsNVSwitch() && !m.options.bindNVSwitches {
		m.logger.Infof("Skipping NVSwitch %s (BIND_NVSWITCHES not set)", device)
		return nil
	}
	if !nvdev.IsGPU() && !nvdev.IsNVSwitch() {
		m.logger.Infof("Device %s is not an NVIDIA GPU or NVSwitch", device)
		return nil
	}

	m.logger.Infof("Binding device %s", device)

	if err := m.nvpassthrough.BindToVFIODriver(device); err != nil {
		return fmt.Errorf("failed to bind device %s to vfio driver: %w", device, err)
	}

	return nil
}
