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

package nvml

import (
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/sirupsen/logrus"
)

type Client struct {
	nvml.Interface
	log *logrus.Logger
}

func NewClient(libraryPath string, log *logrus.Logger) *Client {
	var opts []nvml.LibraryOption
	if libraryPath != "" {
		opts = append(opts, nvml.WithLibraryPath(libraryPath))
	}

	nvmllib := nvml.New(opts...)

	return &Client{
		log:       log,
		Interface: nvmllib,
	}
}

func (n Client) ValidateDriver() error {
	if ret := n.Init(); ret != nvml.SUCCESS {
		n.log.Infof("Failed to initialize NVML : %v", ret)
		return ret
	}
	defer func() {
		_ = n.Shutdown()
	}()

	version, ret := n.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		n.log.Infof("NVML library returned an error: %v", ret)
		return ret
	}

	n.log.Infof("Host driver detected: %s", version)
	return nil
}
