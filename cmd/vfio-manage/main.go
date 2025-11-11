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
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/NVIDIA/k8s-driver-manager/internal/info"
	"github.com/NVIDIA/k8s-driver-manager/internal/nvpci"
)

type flags struct {
	allDevices bool
	deviceID   string
}

func main() {
	flags := flags{}

	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		DisableQuote:  true,
	})

	app := cli.NewApp()
	app.Name = "vfio-manage"
	app.Usage = "Manage VFIO driver binding for NVIDIA GPU devices"
	app.Version = info.GetVersionString()

	app.Commands = []*cli.Command{
		{
			Name:  "bind",
			Usage: "Bind device(s) to vfio-pci driver",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:        "all",
					Aliases:     []string{"a"},
					Destination: &flags.allDevices,
					Usage:       "Bind all NVIDIA devices to vfio-pci",
				},
				&cli.StringFlag{
					Name:        "device-id",
					Aliases:     []string{"d"},
					Destination: &flags.deviceID,
					Usage:       "Specific device ID to bind (e.g., 0000:01:00.0)",
				},
			},
			Before: func(c *cli.Context) error {
				return validateFlags(&flags)
			},
			Action: func(c *cli.Context) error {
				return handleBind(log, &flags)
			},
		},
		{
			Name:  "unbind",
			Usage: "Unbind device(s) from their current driver",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:        "all",
					Aliases:     []string{"a"},
					Destination: &flags.allDevices,
					Usage:       "Unbind all NVIDIA devices",
				},
				&cli.StringFlag{
					Name:        "device-id",
					Aliases:     []string{"d"},
					Destination: &flags.deviceID,
					Usage:       "Specific device ID to unbind (e.g., 0000:01:00.0)",
				},
			},
			Before: func(c *cli.Context) error {
				return validateFlags(&flags)
			},
			Action: func(c *cli.Context) error {
				return handleUnbind(log, &flags)
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func validateFlags(flags *flags) error {
	if !flags.allDevices && flags.deviceID == "" {
		return fmt.Errorf("either --all or --device-id must be specified")
	}

	if flags.allDevices && flags.deviceID != "" {
		return fmt.Errorf("cannot specify both --all and --device-id")
	}

	return nil
}

func handleBind(log *logrus.Logger, flags *flags) error {
	if flags.deviceID != "" {
		return bindDevice(flags.deviceID, log)
	}

	return bindAll(log)
}

func handleUnbind(log *logrus.Logger, flags *flags) error {
	if flags.deviceID != "" {
		return unbindDevice(flags.deviceID, log)
	}

	return unbindAll(log)
}

func bindAll(log *logrus.Logger) error {
	nvpciLib := nvpci.New()
	devices, err := nvpciLib.GetGPUs()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPUs: %w", err)
	}

	for _, dev := range devices {
		log.Infof("Binding device %s", dev.Address)
		// (cdesiniotis) ideally this should be replaced by a call to nvdev.BindToVFIODriver()
		if err := nvpciLib.BindToVFIODriver(dev); err != nil {
			log.Warnf("Failed to bind device %s: %v", dev.Address, err)
		}
	}

	return nil
}

func unbindAll(log *logrus.Logger) error {
	nvpciLib := nvpci.New()
	devices, err := nvpciLib.GetGPUs()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPUs: %w", err)
	}

	for _, dev := range devices {
		log.Infof("Unbinding device %s", dev.Address)
		// (cdesiniotis) ideally this should be replaced by a call to nvdev.UnbindFromDriver()
		if err := nvpciLib.UnbindFromDriver(dev); err != nil {
			log.Warnf("Failed to unbind device %s: %v", dev.Address, err)
		}
	}
	return nil
}

func bindDevice(device string, log *logrus.Logger) error {
	nvpciLib := nvpci.New()
	nvdev, err := nvpciLib.GetGPUByPciBusID(device)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPU device: %w", err)
	}
	if nvdev == nil || !nvdev.IsGPU() {
		log.Infof("Device %s is not a GPU", device)
		return nil
	}

	log.Infof("Binding device %s", device)

	// (cdesiniotis) ideally this should be replaced by a call to nvdev.BindToVFIODriver()
	if err := nvpciLib.BindToVFIODriver(nvdev); err != nil {
		return fmt.Errorf("failed to bind device %s to vfio driver: %w", device, err)
	}

	return nil
}

func unbindDevice(device string, log *logrus.Logger) error {
	nvpciLib := nvpci.New()
	nvdev, err := nvpciLib.GetGPUByPciBusID(device)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA GPU device: %w", err)
	}
	if nvdev == nil || !nvdev.IsGPU() {
		log.Infof("Device %s is not a GPU", device)
		return nil
	}

	log.Infof("Unbinding device %s", device)

	// (cdesiniotis) ideally this should be replaced by a call to nvdev.UnbindFromDriver()
	if err := nvpciLib.UnbindFromDriver(nvdev); err != nil {
		return fmt.Errorf("failed to unbind device %s from driver: %w", device, err)
	}

	return nil
}
