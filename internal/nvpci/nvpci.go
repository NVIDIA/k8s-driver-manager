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

package nvpci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
)

const (
	pciRootDir        = "/sys/bus/pci/"
	pciDevicesRoot    = pciRootDir + "devices"
	pciDriversRoot    = pciRootDir + "drivers"
	vfioPCIDriverName = "vfio-pci"
	consumerPrefix    = "consumer:pci:"
)

type nvpciWrapper struct {
	nvpci.Interface
}

type nvidiaPCIDevice struct {
	*nvpci.NvidiaPCIDevice
}

type nvidiaPCIAuxDevice struct {
	Path    string
	Address string
	Driver  string
}

func New() *nvpciWrapper {
	return &nvpciWrapper{
		Interface: nvpci.New(),
	}
}

// (cdesiniotis) ideally this method would be attached to the nvcpi.NvidiaPCIDevice struct
// which removes the need for this wrapper
func (w *nvpciWrapper) BindToVFIODriver(dev *nvpci.NvidiaPCIDevice) error {
	nvdev := &nvidiaPCIDevice{dev}
	return nvdev.bindToVFIODriver()
}

// (cdesiniotis) ideally this method would be attached to the nvcpi.NvidiaPCIDevice struct
// which removes the need for this wrapper
func (w *nvpciWrapper) UnbindFromDriver(dev *nvpci.NvidiaPCIDevice) error {
	nvdev := &nvidiaPCIDevice{dev}
	return nvdev.unbindFromDriver()
}

func (d *nvidiaPCIDevice) bindToVFIODriver() error {
	// TODO: Instead of always binding to vfio-pci, check if a vfio variant module
	// should be used instead. This is required for GB200 where the nvgrace-gpu-vfio-pci
	// module must be used instead of vfio-pci.
	if d.Driver != vfioPCIDriverName {
		if err := unbind(d.Address); err != nil {
			return fmt.Errorf("failed to unbind device %s: %w", d.Address, err)
		}
		if err := bind(d.Address, vfioPCIDriverName); err != nil {
			return fmt.Errorf("failed to bind device %s to %s: %w", d.Address, vfioPCIDriverName, err)
		}
	}

	// For graphics mode, bind the auxiliary device as well
	auxDev, err := d.getGraphicsAuxDev()
	if err != nil {
		return fmt.Errorf("failed to get graphics auxiliary device for %s: %w", d.Address, err)
	}
	if auxDev == nil {
		return nil
	}
	if auxDev.Driver == vfioPCIDriverName {
		return nil
	}

	if err := unbind(auxDev.Address); err != nil {
		return fmt.Errorf("failed to unbind graphics auxiliary device %s: %w", auxDev.Address, err)
	}
	if err := bind(auxDev.Address, vfioPCIDriverName); err != nil {
		return fmt.Errorf("failed to bind graphics auxiliary device %s to %s: %w", auxDev.Address, vfioPCIDriverName, err)
	}

	return nil
}

func (d *nvidiaPCIDevice) unbindFromDriver() error {
	if err := unbind(d.Address); err != nil {
		return fmt.Errorf("failed to unbind device %s: %w", d.Address, err)
	}

	// For graphics mode, unbind the auxiliary device as well
	auxDev, err := d.getGraphicsAuxDev()
	if err != nil {
		return fmt.Errorf("failed to get graphics auxiliary device for %s: %w", d.Address, err)
	}
	if auxDev != nil {
		if err := unbind(auxDev.Address); err != nil {
			return fmt.Errorf("failed to unbind graphics auxiliary device %s: %w", auxDev.Address, err)
		}
	}

	return nil
}

func (d *nvidiaPCIDevice) getGraphicsAuxDev() (*nvidiaPCIAuxDevice, error) {
	if d.Class != nvpci.PCI3dControllerClass {
		return nil, nil
	}

	// Look for consumer symlink
	entries, err := os.ReadDir(d.Path)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "consumer") {
			// Extract aux device name from consumer:pci:XXXX:XX:XX.X format
			parts := strings.Split(entry.Name(), consumerPrefix)
			if len(parts) != 2 {
				continue
			}

			address := parts[1]
			if address == "" {
				continue
			}

			// Check if aux device exists
			path := filepath.Join(pciDevicesRoot, address)
			if _, err := os.Stat(path); err != nil {
				continue
			}

			auxDev := &nvidiaPCIAuxDevice{
				Path:    path,
				Address: address,
			}

			driver, err := getDriver(path)
			if err != nil {
				return nil, fmt.Errorf("failed to get driver for graphics auxiliary device %s: %w", address, err)
			}
			auxDev.Driver = driver
			return auxDev, nil
		}
	}

	return nil, nil
}

func getDriver(devicePath string) (string, error) {
	driver, err := filepath.EvalSymlinks(filepath.Join(devicePath, "driver"))
	switch {
	case os.IsNotExist(err):
		return "", nil
	case err == nil:
		return filepath.Base(driver), nil
	}
	return "", err
}

func bind(device string, driver string) error {
	driverOverridePath := filepath.Join(pciDevicesRoot, device, "driver_override")
	if err := os.WriteFile(driverOverridePath, []byte(driver), 0644); err != nil {
		return fmt.Errorf("failed to set driver_override for %s: %w", device, err)
	}

	bindPath := filepath.Join(pciDriversRoot, driver, "bind")
	if err := os.WriteFile(bindPath, []byte(device), 0644); err != nil {
		return fmt.Errorf("failed to bind %s to %s: %w", device, driver, err)
	}

	return nil
}

func unbind(device string) error {
	driverPath := filepath.Join(pciDevicesRoot, device, "driver")
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		return nil
	}

	driverLink, err := os.Readlink(driverPath)
	if err != nil {
		return fmt.Errorf("failed to read driver link for %s: %w", device, err)
	}
	driverName := filepath.Base(driverLink)

	unbindPath := filepath.Join(driverPath, "unbind")
	if err := os.WriteFile(unbindPath, []byte(device), 0644); err != nil {
		return fmt.Errorf("failed to unbind %s from %s: %w", device, driverName, err)
	}

	return nil
}
