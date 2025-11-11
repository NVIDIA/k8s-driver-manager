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
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/NVIDIA/k8s-driver-manager/internal/info"
)

const (
	sysBusPCIDevices    = "/sys/bus/pci/devices"
	vfioPCIDriverPath   = "/sys/bus/pci/drivers/vfio-pci"
	nvidiaVendorID      = "0x10de"
	gpuClass3D          = "0x030000"
	gpuClassVGA         = "0x030200"
	vfioPCIDriverName   = "vfio-pci"
	consumerPrefix      = "consumer:pci:"
)

func main() {
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
					Name:    "all",
					Aliases: []string{"a"},
					Usage:   "Bind all NVIDIA devices to vfio-pci",
				},
				&cli.StringFlag{
					Name:    "device-id",
					Aliases: []string{"d"},
					Usage:   "Specific device ID to bind (e.g., 0000:01:00.0)",
				},
			},
			Action: func(c *cli.Context) error {
				return handleBind(c, log)
			},
		},
		{
			Name:  "unbind",
			Usage: "Unbind device(s) from their current driver",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:    "all",
					Aliases: []string{"a"},
					Usage:   "Unbind all NVIDIA devices",
				},
				&cli.StringFlag{
					Name:    "device-id",
					Aliases: []string{"d"},
					Usage:   "Specific device ID to unbind (e.g., 0000:01:00.0)",
				},
			},
			Action: func(c *cli.Context) error {
				return handleUnbind(c, log)
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func handleBind(c *cli.Context, log *logrus.Logger) error {
	allDevices := c.Bool("all")
	deviceID := c.String("device-id")

	if !allDevices && deviceID == "" {
		return fmt.Errorf("either --all or --device-id must be specified")
	}

	if allDevices && deviceID != "" {
		return fmt.Errorf("cannot specify both --all and --device-id")
	}

	if deviceID != "" {
		return bindDevice(deviceID, log)
	}

	return bindAll(log)
}

func handleUnbind(c *cli.Context, log *logrus.Logger) error {
	allDevices := c.Bool("all")
	deviceID := c.String("device-id")

	if !allDevices && deviceID == "" {
		return fmt.Errorf("either --all or --device-id must be specified")
	}

	if allDevices && deviceID != "" {
		return fmt.Errorf("cannot specify both --all and --device-id")
	}

	if deviceID != "" {
		return unbindDevice(deviceID, log)
	}

	return unbindAll(log)
}

func bindAll(log *logrus.Logger) error {
	devices, err := getNVIDIADevices()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA devices: %w", err)
	}

	for _, dev := range devices {
		if err := bindDevice(dev, log); err != nil {
			log.Warnf("Failed to bind device %s: %v", dev, err)
		}
	}

	return nil
}

func unbindAll(log *logrus.Logger) error {
	devices, err := getNVIDIADevices()
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA devices: %w", err)
	}

	for _, dev := range devices {
		if err := unbindDevice(dev, log); err != nil {
			log.Warnf("Failed to unbind device %s: %v", dev, err)
		}
	}

	return nil
}

func bindDevice(gpu string, log *logrus.Logger) error {
	if !isNVIDIAGPUDevice(gpu) {
		log.Infof("Device %s is not a GPU", gpu)
		return nil
	}

	// Check if already bound to vfio-pci
	if isBoundToVFIO(gpu, log) {
		log.Infof("Device %s already bound to vfio-pci", gpu)
		return nil
	}

	// Bind the PCI device
	if err := bindPCIDevice(gpu, log); err != nil {
		return err
	}

	// For graphics mode, bind the auxiliary device as well
	auxDev, err := getGraphicsAuxDev(gpu)
	if err != nil {
		return fmt.Errorf("failed to get auxiliary device for %s: %w", gpu, err)
	}

	if auxDev != "" {
		log.Infof("GPU %s is in graphics mode, aux_dev %s", gpu, auxDev)
		if err := bindPCIDevice(auxDev, log); err != nil {
			return fmt.Errorf("failed to bind auxiliary device %s: %w", auxDev, err)
		}
	}

	return nil
}

func unbindDevice(gpu string, log *logrus.Logger) error {
	if !isNVIDIAGPUDevice(gpu) {
		return nil
	}

	log.Infof("Unbinding device %s", gpu)
	if err := unbindFromDriver(gpu, log); err != nil {
		return err
	}

	// For graphics mode, unbind the auxiliary device as well
	auxDev, err := getGraphicsAuxDev(gpu)
	if err != nil {
		return fmt.Errorf("failed to get auxiliary device for %s: %w", gpu, err)
	}

	if auxDev != "" {
		log.Infof("GPU %s is in graphics mode, aux_dev %s", gpu, auxDev)
		if err := unbindFromDriver(auxDev, log); err != nil {
			return fmt.Errorf("failed to unbind auxiliary device %s: %w", auxDev, err)
		}
	}

	return nil
}

func bindPCIDevice(gpu string, log *logrus.Logger) error {
	// Unbind from other (non-vfio-pci) drivers first
	if err := unbindFromOtherDriver(gpu, log); err != nil {
		return err
	}

	log.Infof("Binding device %s", gpu)

	// Set driver override
	driverOverridePath := filepath.Join(sysBusPCIDevices, gpu, "driver_override")
	if err := os.WriteFile(driverOverridePath, []byte(vfioPCIDriverName), 0644); err != nil {
		return fmt.Errorf("failed to set driver_override for %s: %w", gpu, err)
	}

	// Bind to vfio-pci
	bindPath := filepath.Join(vfioPCIDriverPath, "bind")
	if err := os.WriteFile(bindPath, []byte(gpu), 0644); err != nil {
		return fmt.Errorf("failed to bind %s to vfio-pci: %w", gpu, err)
	}

	return nil
}

func unbindFromDriver(gpu string, log *logrus.Logger) error {
	driverPath := filepath.Join(sysBusPCIDevices, gpu, "driver")

	// Check if device is bound to any driver
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		return nil
	}

	// Get the driver name
	driverLink, err := os.Readlink(driverPath)
	if err != nil {
		return fmt.Errorf("failed to read driver link for %s: %w", gpu, err)
	}
	driverName := filepath.Base(driverLink)

	log.Infof("Unbinding device %s from driver %s", gpu, driverName)

	// Unbind the device
	unbindPath := filepath.Join(driverPath, "unbind")
	if err := os.WriteFile(unbindPath, []byte(gpu), 0644); err != nil {
		return fmt.Errorf("failed to unbind %s from %s: %w", gpu, driverName, err)
	}

	// Clear driver override
	driverOverridePath := filepath.Join(sysBusPCIDevices, gpu, "driver_override")
	if err := os.WriteFile(driverOverridePath, []byte("\n"), 0644); err != nil {
		return fmt.Errorf("failed to clear driver_override for %s: %w", gpu, err)
	}

	return nil
}

func unbindFromOtherDriver(gpu string, log *logrus.Logger) error {
	driverPath := filepath.Join(sysBusPCIDevices, gpu, "driver")

	// Check if device is bound to any driver
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		return nil
	}

	// Get the driver name
	driverLink, err := os.Readlink(driverPath)
	if err != nil {
		return fmt.Errorf("failed to read driver link for %s: %w", gpu, err)
	}
	driverName := filepath.Base(driverLink)

	// Return if already bound to vfio-pci
	if driverName == vfioPCIDriverName {
		return nil
	}

	log.Infof("Unbinding device %s from driver %s", gpu, driverName)

	// Unbind the device
	unbindPath := filepath.Join(driverPath, "unbind")
	if err := os.WriteFile(unbindPath, []byte(gpu), 0644); err != nil {
		return fmt.Errorf("failed to unbind %s from %s: %w", gpu, driverName, err)
	}

	// Clear driver override
	driverOverridePath := filepath.Join(sysBusPCIDevices, gpu, "driver_override")
	if err := os.WriteFile(driverOverridePath, []byte("\n"), 0644); err != nil {
		return fmt.Errorf("failed to clear driver_override for %s: %w", gpu, err)
	}

	return nil
}

func isNVIDIAGPUDevice(gpu string) bool {
	classFile := filepath.Join(sysBusPCIDevices, gpu, "class")
	data, err := os.ReadFile(classFile)
	if err != nil {
		return false
	}

	class := strings.TrimSpace(string(data))
	return class == gpuClass3D || class == gpuClassVGA
}

func isBoundToVFIO(gpu string, log *logrus.Logger) bool {
	driverPath := filepath.Join(sysBusPCIDevices, gpu, "driver")

	// Check if device is bound to any driver
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		return false
	}

	// Get the driver name
	driverLink, err := os.Readlink(driverPath)
	if err != nil {
		return false
	}
	driverName := filepath.Base(driverLink)

	log.Infof("Existing driver is %s", driverName)

	return driverName == vfioPCIDriverName
}

func getGraphicsAuxDev(gpu string) (string, error) {
	// Check device class
	classFile := filepath.Join(sysBusPCIDevices, gpu, "class")
	data, err := os.ReadFile(classFile)
	if err != nil {
		return "", err
	}

	class := strings.TrimSpace(string(data))
	if class != gpuClass3D {
		return "", nil
	}

	// Look for consumer symlink
	deviceDir := filepath.Join(sysBusPCIDevices, gpu)
	entries, err := os.ReadDir(deviceDir)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "consumer") {
			// Extract aux device name from consumer:pci:XXXX:XX:XX.X format
			parts := strings.Split(entry.Name(), consumerPrefix)
			if len(parts) != 2 {
				continue
			}

			auxDev := parts[1]
			if auxDev == "" {
				continue
			}

			// Check if aux device exists
			auxDevPath := filepath.Join(sysBusPCIDevices, auxDev)
			if _, err := os.Stat(auxDevPath); err == nil {
				return auxDev, nil
			}
		}
	}

	return "", nil
}

func getNVIDIADevices() ([]string, error) {
	var devices []string

	entries, err := os.ReadDir(sysBusPCIDevices)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", sysBusPCIDevices, err)
	}

	for _, entry := range entries {
		vendorFile := filepath.Join(sysBusPCIDevices, entry.Name(), "vendor")
		data, err := os.ReadFile(vendorFile)
		if err != nil {
			continue
		}

		vendor := strings.TrimSpace(string(data))
		if vendor == nvidiaVendorID {
			devices = append(devices, entry.Name())
		}
	}
	return devices, nil
}


