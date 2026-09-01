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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// fastBackoff keeps retries near-instant so tests do not sleep.
func fastBackoff(steps int) wait.Backoff {
	return wait.Backoff{Duration: time.Millisecond, Factor: 1.0, Steps: steps}
}

func TestRetryOnAnyErrorRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	err := retryOnAnyError(context.Background(), fastBackoff(5), func() error {
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
	err := retryOnAnyError(context.Background(), fastBackoff(3), func() error {
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

// flakyNodeAPI is a minimal stand-in for the Kubernetes API server that serves a
// single Node and fails the first getFailures reads and patchFailures writes
// with a 500, mimicking an API server that is not yet ready when the driver
// manager starts.
type flakyNodeAPI struct {
	mu            sync.Mutex
	nodeName      string
	getFailures   int
	patchFailures int
	requests      int
	patches       int
	unschedulable bool
}

func (f *flakyNodeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests++

	// The cordon path is a read followed by a write, and either half can fail
	// independently, so the failure budgets are tracked per verb.
	write := r.Method == http.MethodPatch || r.Method == http.MethodPut
	if write {
		f.patches++
		if f.patchFailures > 0 {
			f.patchFailures--
			http.Error(w, "the server rejected the update", http.StatusInternalServerError)
			return
		}
	} else if f.getFailures > 0 {
		f.getFailures--
		http.Error(w, "the server is currently unable to handle the request", http.StatusInternalServerError)
		return
	}

	if write {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A strategic merge patch clears spec.unschedulable with a null rather
		// than setting it to false, so key presence is what matters here.
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if spec, ok := patch["spec"].(map[string]any); ok {
			if value, present := spec["unschedulable"]; present {
				cordoned, _ := value.(bool)
				f.unschedulable = cordoned
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	node := &corev1.Node{
		TypeMeta:   metav1.TypeMeta{Kind: "Node", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: f.nodeName},
		Spec:       corev1.NodeSpec{Unschedulable: f.unschedulable},
	}
	if err := json.NewEncoder(w).Encode(node); err != nil {
		f.requests-- // the request never completed, do not count it
	}
}

func (f *flakyNodeAPI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *flakyNodeAPI) cordoned() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unschedulable
}

func newTestClient(t *testing.T, api *flakyNodeAPI) *Client {
	t.Helper()

	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)

	log := logrus.New()
	log.SetOutput(io.Discard)

	return &Client{ctx: context.Background(), log: log, clientset: clientset}
}

func TestCordonNodeRetriesUntilSuccess(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", getFailures: 2}
	c := newTestClient(t, api)

	require.NoError(t, c.CordonNode("gpu-node", fastBackoff(5)))
	require.True(t, api.cordoned())
	// Two failed gets, then a successful get plus the patch.
	require.Equal(t, 4, api.requestCount())
}

func TestCordonNodeReturnsErrorWhenRetriesExhausted(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", getFailures: 100}
	c := newTestClient(t, api)

	err := c.CordonNode("gpu-node", fastBackoff(3))
	require.Error(t, err)
	require.False(t, api.cordoned())
	require.Equal(t, 3, api.requestCount())
}

func TestUncordonNodeRetriesUntilSuccess(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", getFailures: 2, unschedulable: true}
	c := newTestClient(t, api)

	require.NoError(t, c.UncordonNode("gpu-node", fastBackoff(5)))
	require.False(t, api.cordoned())
	require.Equal(t, 4, api.requestCount())
}

func TestUncordonNodeReturnsErrorWhenRetriesExhausted(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", getFailures: 100, unschedulable: true}
	c := newTestClient(t, api)

	err := c.UncordonNode("gpu-node", fastBackoff(3))
	require.Error(t, err)
	require.True(t, api.cordoned())
	require.Equal(t, 3, api.requestCount())
}

func (f *flakyNodeAPI) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patches
}

func TestCordonNodeRetriesWhenPatchFails(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", patchFailures: 2}
	c := newTestClient(t, api)

	require.NoError(t, c.CordonNode("gpu-node", fastBackoff(5)))
	require.True(t, api.cordoned())
	// Every attempt re-reads the node, so three get/patch pairs.
	require.Equal(t, 3, api.patchCount())
	require.Equal(t, 6, api.requestCount())
}

func TestCordonNodeReturnsErrorWhenPatchKeepsFailing(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", patchFailures: 100}
	c := newTestClient(t, api)

	err := c.CordonNode("gpu-node", fastBackoff(3))
	require.Error(t, err)
	require.False(t, api.cordoned())
	require.Equal(t, 3, api.patchCount())
	require.Equal(t, 6, api.requestCount())
}

func TestUncordonNodeRetriesWhenPatchFails(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", patchFailures: 2, unschedulable: true}
	c := newTestClient(t, api)

	require.NoError(t, c.UncordonNode("gpu-node", fastBackoff(5)))
	require.False(t, api.cordoned())
	require.Equal(t, 3, api.patchCount())
	require.Equal(t, 6, api.requestCount())
}

func TestUncordonNodeReturnsErrorWhenPatchKeepsFailing(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", patchFailures: 100, unschedulable: true}
	c := newTestClient(t, api)

	err := c.UncordonNode("gpu-node", fastBackoff(3))
	require.Error(t, err)
	require.True(t, api.cordoned())
	require.Equal(t, 3, api.patchCount())
	require.Equal(t, 6, api.requestCount())
}

func TestRetryOnAnyErrorStopsPromptlyWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A backoff this long would hang the test if the remaining sleep were not
	// cut short by the cancelled context.
	backoff := wait.Backoff{Duration: time.Hour, Factor: 1.0, Steps: 5}

	attempts := 0
	start := time.Now()
	err := retryOnAnyError(ctx, backoff, func() error {
		attempts++
		cancel()
		return fmt.Errorf("api-server down")
	})

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, attempts)
	require.Less(t, time.Since(start), 30*time.Second)
}

func TestRetryOnAnyErrorReportsLastErrorAlongsideCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := retryOnAnyError(ctx, fastBackoff(5), func() error {
		cancel()
		return fmt.Errorf("api-server down")
	})

	require.ErrorIs(t, err, context.Canceled)
	require.Contains(t, err.Error(), "api-server down")
}
