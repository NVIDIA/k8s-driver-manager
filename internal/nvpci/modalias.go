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
	"math"
	"reflect"
	"strings"

	"golang.org/x/sys/unix"
)

// modAlias is a decomposed version of string like this
//
// vNNNNNNNNdNNNNNNNNsvNNNNNNNNsdNNNNNNNNbcNNscNNiNN
//
// The "NNNN" are always of the length in the example
// unless replaced with a wildcard ("*")
type modAlias struct {
	vendor     string // v
	device     string // d
	subvendor  string // sv
	subdevice  string // sd
	baseClass  string // bc
	subClass   string // sc
	interface_ string // i
}

// vfioAlias represents an entry from the modules.alias file for a vfio driver
type vfioAlias struct {
	modAlias *modAlias // The modalias pattern
	driver   string    // The vfio driver name
}

func parseModAliasString(input string) (*modAlias, error) {
	if input == "" {
		return nil, fmt.Errorf("modalias string is empty")
	}

	input = strings.TrimSpace(input)

	// Trim the leading "pci:" prefix in the modalias file
	split := strings.SplitN(input, ":", 2)
	if len(split) != 2 {
		return nil, fmt.Errorf("unexpected number of parts in modalias after trimming 'pci:' prefix: %s", input)
	}
	input = split[1]

	if !strings.HasPrefix(input, "v") {
		return nil, fmt.Errorf("modalias must start with 'v', got: %s", input)
	}

	ma := &modAlias{}
	remaining := input[1:] // skip 'v'

	vendor, remaining, err := extractField(remaining, "d")
	if err != nil {
		return nil, fmt.Errorf("failed to parse vendor: %w", err)
	}
	ma.vendor = vendor

	device, remaining, err := extractField(remaining, "sv")
	if err != nil {
		return nil, fmt.Errorf("failed to parse device: %w", err)
	}
	ma.device = device

	subvendor, remaining, err := extractField(remaining, "sd")
	if err != nil {
		return nil, fmt.Errorf("failed to parse subvendor: %w", err)
	}
	ma.subvendor = subvendor

	subdevice, remaining, err := extractField(remaining, "bc")
	if err != nil {
		return nil, fmt.Errorf("failed to parse subdevice: %w", err)
	}
	ma.subdevice = subdevice

	baseClass, remaining, err := extractField(remaining, "sc")
	if err != nil {
		return nil, fmt.Errorf("failed to parse base class: %w", err)
	}
	ma.baseClass = baseClass

	subClass, remaining, err := extractField(remaining, "i")
	if err != nil {
		return nil, fmt.Errorf("failed to parse subclass: %w", err)
	}
	ma.subClass = subClass

	ma.interface_ = remaining

	return ma, nil
}

// extractField extracts the value before the next delimiter from the input string.
// Returns the extracted value, the remaining string (without the delimiter), and any error.
func extractField(input, delimiter string) (string, string, error) {
	idx := strings.Index(input, delimiter)
	if idx == -1 {
		return "", "", fmt.Errorf("failed to find index of the first instance of %q in string %q", delimiter, input)
	}

	value := input[:idx]
	remaining := input[idx+len(delimiter):]

	return value, remaining, nil
}

func getKernelVersion() (string, error) {
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		return "", err
	}

	// Convert C-style byte array to Go string
	release := make([]byte, 0, len(uname.Release))
	for _, c := range uname.Release {
		if c == 0 {
			break
		}
		release = append(release, c)
	}

	return string(release), nil
}

// getVFIOAliases returns the vfio driver aliases from the input string.
// The input string is expected to be the content of a modules.alias file.
// Only lines that begin with 'alias vfio_pci:' are parsed, with the
// format being:
//
// alias vfio_pci:<modalias string> <driver_name>
func getVFIOAliases(input string) []vfioAlias {
	var aliases []vfioAlias

	lines := strings.Split(input, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if !strings.HasPrefix(line, "alias vfio_pci:") {
			continue
		}

		split := strings.SplitN(line, " ", 3)
		if len(split) != 3 {
			continue
		}
		modAliasStr := split[1]
		modAlias, err := parseModAliasString(modAliasStr)
		if err != nil {
			continue
		}

		driver := split[2]
		aliases = append(aliases, vfioAlias{
			modAlias: modAlias,
			driver:   driver,
		})
	}

	return aliases
}

// findBestMatch finds the best matching VFIO driver for the given modalias
// by comparing against all available vfio alias patterns. The best match
// is the one with the fewest wildcard characters.
func findBestMatch(deviceModAlias *modAlias, aliases []vfioAlias) string {
	var bestDriver string
	bestWildcardCount := math.MaxInt

	for _, alias := range aliases {
		if matches, wildcardCount := matchModalias(deviceModAlias, alias.modAlias); matches {
			if wildcardCount < bestWildcardCount {
				bestDriver = alias.driver
				bestWildcardCount = wildcardCount
			}
		}
	}

	return bestDriver
}

// matchModalias checks if a device modalias matches a pattern from modules.alias
// Returns true if it matches and the number of wildcards
func matchModalias(deviceModAlias, patternModAlias *modAlias) (bool, int) {
	wildcardCount := 0

	modAliasType := reflect.TypeOf(*deviceModAlias)
	deviceModAliasValue := reflect.ValueOf(*deviceModAlias)
	patternModAliasValue := reflect.ValueOf(*patternModAlias)

	// iterate over both modAlias structs, comparing each field
	for i := 0; i < modAliasType.NumField(); i++ {
		deviceValue := deviceModAliasValue.Field(i).String()
		patternValue := patternModAliasValue.Field(i).String()

		if patternValue == "*" {
			wildcardCount++
			continue
		}

		if deviceValue != patternValue {
			return false, wildcardCount
		}
	}
	return true, wildcardCount
}
