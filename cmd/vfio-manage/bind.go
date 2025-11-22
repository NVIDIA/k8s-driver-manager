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

type bindCommand struct {
	logger   *logrus.Logger
	nvpciLib nvpci.Interface
}

type bindOptions struct {
	all      bool
	deviceID string
}

// newBindCommand constructs a bind command with the specified logger
func newBindCommand(logger *logrus.Logger) *cli.Command {
	c := bindCommand{
		logger:   logger,
		nvpciLib: nvpci.New(),
	}
	return c.build()
}

// build the bind command
func (m bindCommand) build() *cli.Command {
	cfg := bindOptions{}

	// Create the 'bind' command
	c := cli.Command{
		Name:  "bind",
		Usage: "Bind device(s) to vfio-pci driver",
		Before: func(c *cli.Context) error {
			return m.validateFlags(&cfg)
		},
		Action: func(c *cli.Context) error {
			return m.run(&cfg)
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "all",
				Aliases:     []string{"a"},
				Destination: &cfg.all,
				Usage:       "Bind all NVIDIA devices to vfio-pci",
			},
			&cli.StringFlag{
				Name:        "device-id",
				Aliases:     []string{"d"},
				Destination: &cfg.deviceID,
				Usage:       "Specific device ID to bind (e.g., 0000:01:00.0)",
			},
		},
	}

	return &c
}

func (m bindCommand) validateFlags(cfg *bindOptions) error {
	if !cfg.all && cfg.deviceID == "" {
		return fmt.Errorf("either --all or --device-id must be specified")
	}

	if cfg.all && cfg.deviceID != "" {
		return fmt.Errorf("cannot specify both --all and --device-id")
	}

	return nil
}

func (m bindCommand) run(cfg *bindOptions) error {
	if cfg.deviceID != "" {
		return m.bindDevice(cfg.deviceID)
	}

	return m.bindAll()
}

func (m bindCommand) bindAll() error {
	devices, err := m.nvpciLib.GetGPUs()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPUs: %w", err)
	}

	for _, dev := range devices {
		m.logger.Infof("Binding device %s", dev.Address)
		// (cdesiniotis) ideally this should be replaced by a call to nvdev.BindToVFIODriver()
		if err := m.nvpciLib.BindToVFIODriver(dev); err != nil {
			m.logger.Warnf("Failed to bind device %s: %v", dev.Address, err)
		}
	}

	return nil
}

func (m bindCommand) bindDevice(device string) error {
	nvdev, err := m.nvpciLib.GetGPUByPciBusID(device)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPU device: %w", err)
	}
	if nvdev == nil || !nvdev.IsGPU() {
		m.logger.Infof("Device %s is not a GPU", device)
		return nil
	}

	m.logger.Infof("Binding device %s", device)

	// (cdesiniotis) ideally this should be replaced by a call to nvdev.BindToVFIODriver()
	if err := m.nvpciLib.BindToVFIODriver(nvdev); err != nil {
		return fmt.Errorf("failed to bind device %s to vfio driver: %w", device, err)
	}

	return nil
}
