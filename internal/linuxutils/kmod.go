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

package linuxutils

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

const (
	procModules = "/proc/modules"
)

type KernelModules struct {
	log *logrus.Logger

	root string
}

func NewKernelModules(log *logrus.Logger, options ...func(modules *KernelModules)) *KernelModules {
	km := &KernelModules{
		log: log,
	}
	for _, option := range options {
		option(km)
	}
	if km.root == "" {
		km.root = "/"
	}
	return km
}

func WithRoot(root string) func(modules *KernelModules) {
	return func(km *KernelModules) {
		km.root = root
	}
}

func (km *KernelModules) List(searchKey string) error {
	modsFilePath := filepath.Join(km.root, procModules)
	file, err := os.Open(modsFilePath)
	if err != nil {
		return fmt.Errorf("error opening file %s: %w", modsFilePath, err)
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			km.log.Warnf("error closing file %s: %v", modsFilePath, err)
		}
	}(file)

	scanner := bufio.NewScanner(file)
	km.log.Infof("%-20s %-10s %-15s %s\n", "Module", "Size", "Ref Count", "Used by") // Header

	for scanner.Scan() {
		line := scanner.Text()

		if len(searchKey) > 0 && !strings.Contains(line, searchKey) {
			continue
		}

		fields := strings.Fields(line)

		if len(fields) >= 4 {
			name := fields[0]

			size, err := strconv.Atoi(fields[1])
			if err != nil {
				km.log.Warnf("error parsing module size %s: %v", fields[1], err)
				continue
			}

			refCnt, err := strconv.Atoi(fields[2])
			if err != nil {
				km.log.Warnf("error parsing module ref count %s: %v", fields[2], err)
				continue
			}

			usedBy := fields[3]

			km.log.Printf("%-20s %-10d %-15d %s\n", name, size, refCnt, usedBy)
		}
	}

	if err := scanner.Err(); err != nil {
		km.log.Errorf("Error reading /proc/modules: %v\n", err)
		return err
	}
	return nil
}

func (km *KernelModules) Load(module string) error {
	cmd := exec.Command("chroot", km.root, "modprobe", module)
	return cmd.Run()
}
