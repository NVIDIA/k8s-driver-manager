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

import "testing"

func TestShouldSkipHostDriver(t *testing.T) {
	tests := []struct {
		name                  string
		hostDriverDetected    bool
		forceHostDriverUnbind bool
		want                  bool
	}{
		{
			name:               "host driver defaults to skip",
			hostDriverDetected: true,
			want:               true,
		},
		{
			name:                  "explicit override continues uninstall",
			hostDriverDetected:    true,
			forceHostDriverUnbind: true,
			want:                  false,
		},
		{
			name:               "no host driver continues uninstall",
			hostDriverDetected: false,
			want:               false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipHostDriver(tt.hostDriverDetected, tt.forceHostDriverUnbind); got != tt.want {
				t.Fatalf("shouldSkipHostDriver(%t, %t) = %t, want %t", tt.hostDriverDetected, tt.forceHostDriverUnbind, got, tt.want)
			}
		})
	}
}
