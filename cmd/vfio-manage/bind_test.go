//go:build !darwin && !windows

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAdditionalVFIOAliases(t *testing.T) {
	testCases := []struct {
		description   string
		input         string
		expected      map[uint16]string
		expectedError string
	}{
		{
			description: "empty input",
			input:       "",
			expected:    map[uint16]string{},
		},
		{
			description: "single entry with 0x prefix",
			input:       "0x2331=nvgrace_gpu_vfio_pci",
			expected:    map[uint16]string{0x2331: "nvgrace_gpu_vfio_pci"},
		},
		{
			description: "unprefixed device ID is interpreted as decimal, matching nvpci",
			input:       "2331=foo",
			expected:    map[uint16]string{2331: "foo"},
		},
		{
			description: "uppercase hex digits and 0X prefix",
			input:       "0X23AB=foo",
			expected:    map[uint16]string{0x23ab: "foo"},
		},
		{
			description: "multiple entries with whitespace and trailing comma",
			input:       "0x2331=nvgrace_gpu_vfio_pci, 0x2330=foo,",
			expected:    map[uint16]string{0x2331: "nvgrace_gpu_vfio_pci", 0x2330: "foo"},
		},
		{
			description: "duplicate device ID uses the last entry",
			input:       "0x2331=foo,0x2331=bar",
			expected:    map[uint16]string{0x2331: "bar"},
		},
		{
			description:   "missing separator",
			input:         "2331",
			expectedError: "expected <device-id>=<module>",
		},
		{
			description:   "missing device ID",
			input:         "=foo",
			expectedError: "expected <device-id>=<module>",
		},
		{
			description:   "missing module",
			input:         "0x2331=",
			expectedError: "expected <device-id>=<module>",
		},
		{
			description:   "non-hex device ID",
			input:         "zzzz=foo",
			expectedError: "invalid device ID",
		},
		{
			description:   "hex digits without 0x prefix are rejected, matching nvpci",
			input:         "23AB=foo",
			expectedError: "invalid device ID",
		},
		{
			description:   "device ID out of uint16 range",
			input:         "0x10000=foo",
			expectedError: "invalid device ID",
		},
		{
			description:   "valid entry followed by invalid entry",
			input:         "0x2331=foo,bar",
			expectedError: "expected <device-id>=<module>",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			aliases, err := parseAdditionalVFIOAliases(tc.input)
			if tc.expectedError != "" {
				require.ErrorContains(t, err, tc.expectedError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, aliases)
		})
	}
}
