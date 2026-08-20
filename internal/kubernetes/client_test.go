/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package kubernetes

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
)

// fastBackoff keeps retries near-instant so tests do not sleep.
func fastBackoff(steps int) wait.Backoff {
	return wait.Backoff{Duration: time.Millisecond, Factor: 1.0, Steps: steps}
}

func TestRetryOnAnyErrorRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	err := retryOnAnyError(fastBackoff(5), func() error {
		attempts++
		if attempts < 3 {
			return fmt.Errorf("api-server timeout")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, attempts)
}

func TestRetryOnAnyErrorReturnsLastErrorWhenExhausted(t *testing.T) {
	attempts := 0
	err := retryOnAnyError(fastBackoff(3), func() error {
		attempts++
		return fmt.Errorf("api-server down")
	})
	require.Error(t, err)
	require.Equal(t, 3, attempts)
}

func TestRetryBackoffAlwaysAttemptsAtLeastOnce(t *testing.T) {
	require.GreaterOrEqual(t, RetryBackoff(0).Steps, 1)
	require.Equal(t, 5, RetryBackoff(5).Steps)
}
