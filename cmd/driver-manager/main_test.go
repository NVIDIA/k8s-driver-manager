//go:build !darwin && !windows

/*
 * Copyright (c) NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInvalidateStatusFilesRemovesReadinessFilesOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"driver-ready",
		"toolkit-ready",
		"cuda-ready",
		"plugin-ready",
		"vfio-pci-ready",
		".driver-ctr-ready",
		".driver-daemons-status",
		".cc-manager-ctr-ready",
		"workload-type",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}

	dm := &DriverManager{log: discardLogger()}
	dm.invalidateStatusFiles(dir)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var remaining []string
	for _, e := range entries {
		remaining = append(remaining, e.Name())
	}
	require.ElementsMatch(t, []string{".cc-manager-ctr-ready", "workload-type"}, remaining)
}
