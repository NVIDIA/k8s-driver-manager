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

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/NVIDIA/k8s-driver-manager/internal/nvpci"
)

type unbindCommand struct {
	logger   *logrus.Logger
	nvpciLib nvpci.Interface
	options  unbindOptions
}

type unbindOptions struct {
	all      bool
	deviceID string
}

// newUnbindCommand constructs an unbind command with the specified logger
func newUnbindCommand(logger *logrus.Logger) *cli.Command {
	c := unbindCommand{
		logger: logger,
		nvpciLib: nvpci.New(
			nvpci.WithLogger(logger),
		),
	}
	return c.build()
}

// build the unbind command
func (m unbindCommand) build() *cli.Command {
	c := cli.Command{
		Name:  "unbind",
		Usage: "Unbind device(s) from their current driver",
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
		},
	}

	return &c
}

func (m unbindCommand) validateFlags() error {
	if !m.options.all && m.options.deviceID == "" {
		return fmt.Errorf("either --all or --device-id must be specified")
	}

	if m.options.all && m.options.deviceID != "" {
		return fmt.Errorf("cannot specify both --all and --device-id")
	}

	return nil
}

func (m unbindCommand) run() error {
	if m.options.deviceID != "" {
		return m.unbindDevice()
	}

	return m.unbindAll()
}

func (m unbindCommand) unbindAll() error {
	devices, err := m.nvpciLib.GetGPUs()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPUs: %w", err)
	}

	nvswitches, err := m.nvpciLib.GetNVSwitches()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA NVSwitches: %w", err)
	}
	devices = append(devices, nvswitches...)

	for _, dev := range devices {
		m.logger.Infof("Unbinding device %s", dev.Address)
		// (cdesiniotis) ideally this should be replaced by a call to nvdev.UnbindFromDriver()
		if err := m.nvpciLib.UnbindFromDriver(dev); err != nil {
			m.logger.Warnf("Failed to unbind device %s: %v", dev.Address, err)
		}
	}
	return nil
}

func (m unbindCommand) unbindDevice() error {
	device := m.options.deviceID
	nvdev, err := m.nvpciLib.GetGPUByPciBusID(device)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPU device: %w", err)
	}
	if nvdev == nil || !nvdev.IsGPU() {
		m.logger.Infof("Device %s is not a GPU", device)
		return nil
	}

	m.logger.Infof("Unbinding device %s", device)

	// (cdesiniotis) ideally this should be replaced by a call to nvdev.UnbindFromDriver()
	if err := m.nvpciLib.UnbindFromDriver(nvdev); err != nil {
		return fmt.Errorf("failed to unbind device %s from driver: %w", device, err)
	}

	return nil
}
