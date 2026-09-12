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
	"errors"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

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
	annotations   map[string]string
	labels        map[string]string
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
		if r.Method == http.MethodPut {
			var node corev1.Node
			if err := json.Unmarshal(body, &node); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.unschedulable = node.Spec.Unschedulable
			f.annotations = node.Annotations
		} else {
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
			if meta, ok := patch["metadata"].(map[string]any); ok {
				if ann, ok := meta["annotations"].(map[string]any); ok {
					if f.annotations == nil {
						f.annotations = make(map[string]string)
					}
					for key, value := range ann {
						if value == nil {
							delete(f.annotations, key)
							continue
						}
						if s, ok := value.(string); ok {
							f.annotations[key] = s
						}
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	node := &corev1.Node{
		TypeMeta: metav1.TypeMeta{Kind: "Node", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        f.nodeName,
			Annotations: f.annotations,
			Labels:      f.labels,
		},
		Spec: corev1.NodeSpec{Unschedulable: f.unschedulable},
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

func (f *flakyNodeAPI) annotation(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.annotations[key]
}

func (f *flakyNodeAPI) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patches
}

func (f *flakyNodeAPI) setUnschedulable(unschedulable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unschedulable = unschedulable
}

func (f *flakyNodeAPI) setAnnotation(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.annotations == nil {
		f.annotations = make(map[string]string)
	}
	f.annotations[key] = value
}

func (f *flakyNodeAPI) setLabel(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labels == nil {
		f.labels = make(map[string]string)
	}
	f.labels[key] = value
}

func discardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

func newHTTPTestClient(t *testing.T, api *flakyNodeAPI) *Client {
	t.Helper()

	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{
		Host: server.URL,
		ContentConfig: rest.ContentConfig{
			ContentType: runtime.ContentTypeJSON,
		},
	})
	require.NoError(t, err)

	return &Client{ctx: context.Background(), log: discardLogger(), clientset: clientset}
}

// The client makes one attempt per call: the retry policy lives in the
// driver-manager command, not here.
func TestCordonNodeCordonsTheNode(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node"}
	c := newHTTPTestClient(t, api)

	require.NoError(t, c.CordonNode("gpu-node"))
	require.True(t, api.cordoned())
	// One read followed by one write.
	require.Equal(t, 1, api.patchCount())
	require.Equal(t, 2, api.requestCount())
}

func TestUncordonNodeUncordonsTheNode(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", unschedulable: true}
	c := newHTTPTestClient(t, api)

	require.NoError(t, c.UncordonNode("gpu-node"))
	require.False(t, api.cordoned())
	require.Equal(t, 1, api.patchCount())
	require.Equal(t, 2, api.requestCount())
}

func TestCordonNodeReturnsErrorWhenGetFails(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", getFailures: 1}
	c := newHTTPTestClient(t, api)

	require.Error(t, c.CordonNode("gpu-node"))
	require.False(t, api.cordoned())
	require.Equal(t, 1, api.requestCount())
}

func TestCordonNodeReturnsErrorWhenPatchFails(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", patchFailures: 1}
	c := newHTTPTestClient(t, api)

	require.Error(t, c.CordonNode("gpu-node"))
	require.False(t, api.cordoned())
	require.Equal(t, 1, api.patchCount())
}

func TestUncordonNodeReturnsErrorWhenGetFails(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", getFailures: 1, unschedulable: true}
	c := newHTTPTestClient(t, api)

	require.Error(t, c.UncordonNode("gpu-node"))
	require.True(t, api.cordoned())
	require.Equal(t, 1, api.requestCount())
}

func TestUncordonNodeReturnsErrorWhenPatchFails(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", patchFailures: 1, unschedulable: true}
	c := newHTTPTestClient(t, api)

	require.Error(t, c.UncordonNode("gpu-node"))
	require.True(t, api.cordoned())
	require.Equal(t, 1, api.patchCount())
}

const testNodeName = "test-node"

func newTestClient(node *corev1.Node) (*Client, *fake.Clientset) {
	clientset := fake.NewSimpleClientset(node)
	return &Client{
		ctx:       context.Background(),
		log:       logrus.New(),
		clientset: clientset,
	}, clientset
}

func newTestNode(unschedulable bool, annotations map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            testNodeName,
			ResourceVersion: "1",
			Annotations:     annotations,
		},
		Spec: corev1.NodeSpec{Unschedulable: unschedulable},
	}
}

func getTestNode(t *testing.T, clientset *fake.Clientset) *corev1.Node {
	t.Helper()
	node, err := clientset.CoreV1().Nodes().Get(t.Context(), testNodeName, metav1.GetOptions{})
	require.NoError(t, err)
	return node
}

func getNodePatches(t *testing.T, clientset *fake.Clientset) []map[string]any {
	t.Helper()

	var patches []map[string]any
	for _, action := range clientset.Actions() {
		patchAction, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetResource().Resource != "nodes" {
			continue
		}

		var patch map[string]any
		require.NoError(t, json.Unmarshal(patchAction.GetPatch(), &patch))
		patches = append(patches, patch)
	}
	return patches
}

func TestCordonUncordonNode(t *testing.T) {
	testCases := []struct {
		name          string
		unschedulable bool
		annotations   map[string]string
		cordon        bool
	}{
		{
			name:   "schedulable node is uncordoned",
			cordon: true,
		},
		{
			name:          "pre-existing cordon is uncordoned",
			unschedulable: true,
			cordon:        true,
		},
		{
			name:          "recording survives a restart until uncordon",
			unschedulable: true,
			annotations:   map[string]string{nodeInitialUnschedulableAnnotationKey: "false"},
			cordon:        true,
		},
		{
			name:        "stale recording on schedulable node is replaced",
			annotations: map[string]string{nodeInitialUnschedulableAnnotationKey: "true"},
			cordon:      true,
		},
		{
			name:          "external cordon without recording is uncordoned",
			unschedulable: true,
		},
		{
			name: "schedulable node without recording is unchanged",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, clientset := newTestClient(newTestNode(tc.unschedulable, tc.annotations))
			if tc.cordon {
				require.NoError(t, client.CordonNode(testNodeName))
				require.True(t, getTestNode(t, clientset).Spec.Unschedulable)
			}

			require.NoError(t, client.UncordonNode(testNodeName))
			node := getTestNode(t, clientset)
			require.False(t, node.Spec.Unschedulable)
			require.NotContains(t, node.Annotations, nodeInitialUnschedulableAnnotationKey)
		})
	}
}

func TestCordonNodeRecordsInitialState(t *testing.T) {
	testCases := []struct {
		name               string
		unschedulable      bool
		annotations        map[string]string
		expectedAnnotation string
		expectedPatches    int
	}{
		{
			name:               "schedulable",
			expectedAnnotation: "false",
			expectedPatches:    1,
		},
		{
			name:               "already cordoned",
			unschedulable:      true,
			expectedAnnotation: "true",
			expectedPatches:    1,
		},
		{
			name:               "existing recording is retained while cordoned",
			unschedulable:      true,
			annotations:        map[string]string{nodeInitialUnschedulableAnnotationKey: "false"},
			expectedAnnotation: "false",
		},
		{
			name:               "stale recording is replaced while schedulable",
			annotations:        map[string]string{nodeInitialUnschedulableAnnotationKey: "true"},
			expectedAnnotation: "false",
			expectedPatches:    1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, clientset := newTestClient(newTestNode(tc.unschedulable, tc.annotations))
			require.NoError(t, client.CordonNode(testNodeName))

			node := getTestNode(t, clientset)
			require.True(t, node.Spec.Unschedulable)
			require.Equal(t, tc.expectedAnnotation, node.Annotations[nodeInitialUnschedulableAnnotationKey])
			require.Len(t, getNodePatches(t, clientset), tc.expectedPatches)
		})
	}
}

func TestNodeSchedulingStatePatchesAreAtomic(t *testing.T) {
	t.Run("cordon", func(t *testing.T) {
		client, clientset := newTestClient(newTestNode(false, nil))
		require.NoError(t, client.CordonNode(testNodeName))

		patches := getNodePatches(t, clientset)
		require.Len(t, patches, 1)
		require.Equal(t, true, patches[0]["spec"].(map[string]any)["unschedulable"])
		metadata := patches[0]["metadata"].(map[string]any)
		require.Equal(t, "1", metadata["resourceVersion"])
		require.Equal(t, "false", metadata["annotations"].(map[string]any)[nodeInitialUnschedulableAnnotationKey])
	})

	t.Run("uncordon", func(t *testing.T) {
		client, clientset := newTestClient(newTestNode(
			true,
			map[string]string{nodeInitialUnschedulableAnnotationKey: "false"},
		))
		require.NoError(t, client.UncordonNode(testNodeName))

		patches := getNodePatches(t, clientset)
		require.Len(t, patches, 1)
		require.Equal(t, false, patches[0]["spec"].(map[string]any)["unschedulable"])
		metadata := patches[0]["metadata"].(map[string]any)
		require.Equal(t, "1", metadata["resourceVersion"])
		require.Nil(t, metadata["annotations"].(map[string]any)[nodeInitialUnschedulableAnnotationKey])
	})
}

func TestNodeSchedulingStatePatchPreservesOtherAnnotations(t *testing.T) {
	const (
		existingAnnotation = "example.com/existing"
		existingValue      = "value"
	)
	client, clientset := newTestClient(newTestNode(false, map[string]string{
		existingAnnotation: existingValue,
	}))

	require.NoError(t, client.CordonNode(testNodeName))
	require.NoError(t, client.UncordonNode(testNodeName))

	node := getTestNode(t, clientset)
	require.Equal(t, existingValue, node.Annotations[existingAnnotation])
	require.NotContains(t, node.Annotations, nodeInitialUnschedulableAnnotationKey)
}

func TestUncordonNodePatchFailurePreservesState(t *testing.T) {
	client, clientset := newTestClient(newTestNode(
		true,
		map[string]string{nodeInitialUnschedulableAnnotationKey: "false"},
	))
	clientset.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("patch failed")
	})

	require.Error(t, client.UncordonNode(testNodeName))
	node := getTestNode(t, clientset)
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, "false", node.Annotations[nodeInitialUnschedulableAnnotationKey])
}

func TestRecordedStateIsNotEnforced(t *testing.T) {
	t.Run("cordon retains existing recording", func(t *testing.T) {
		client, clientset := newTestClient(newTestNode(
			true,
			map[string]string{nodeInitialUnschedulableAnnotationKey: "invalid"},
		))

		require.NoError(t, client.CordonNode(testNodeName))
		node := getTestNode(t, clientset)
		require.True(t, node.Spec.Unschedulable)
		require.Equal(t, "invalid", node.Annotations[nodeInitialUnschedulableAnnotationKey])
		require.Empty(t, getNodePatches(t, clientset))
	})

	for _, recordedState := range []string{"false", "true", "invalid"} {
		t.Run("uncordon ignores recording "+recordedState, func(t *testing.T) {
			client, clientset := newTestClient(newTestNode(
				true,
				map[string]string{nodeInitialUnschedulableAnnotationKey: recordedState},
			))

			require.NoError(t, client.UncordonNode(testNodeName))
			node := getTestNode(t, clientset)
			require.False(t, node.Spec.Unschedulable)
			require.NotContains(t, node.Annotations, nodeInitialUnschedulableAnnotationKey)
		})
	}
}

const (
	testOwnerClaim = "example.com/owner"
	testPeerClaim  = "example.com/peer"
)

func TestAcquireNodeCordon(t *testing.T) {
	tests := []struct {
		name             string
		unschedulable    bool
		annotations      map[string]string
		labels           map[string]string
		wantAcquired     bool
		wantCordoned     bool
		wantOwner        string
		wantInitialState string
		wantPatches      int
	}{
		{
			name:             "claims a schedulable node",
			wantAcquired:     true,
			wantCordoned:     true,
			wantOwner:        "true",
			wantInitialState: "false",
			wantPatches:      1,
		},
		{
			name:          "leaves an external cordon unclaimed",
			unschedulable: true,
			wantAcquired:  false,
			wantCordoned:  true,
			wantPatches:   0,
		},
		{
			name:             "claims a node already recorded by driver-manager",
			unschedulable:    true,
			annotations:      map[string]string{nodeInitialUnschedulableAnnotationKey: "false"},
			wantAcquired:     true,
			wantCordoned:     true,
			wantOwner:        "true",
			wantInitialState: "false",
			wantPatches:      1,
		},
		{
			name:             "claims a node already claimed by the peer",
			unschedulable:    true,
			annotations:      map[string]string{testPeerClaim: "true"},
			wantAcquired:     true,
			wantCordoned:     true,
			wantOwner:        "true",
			wantInitialState: "true",
			wantPatches:      1,
		},
		{
			name:          "is a no-op when this component already claimed the cordon",
			unschedulable: true,
			annotations: map[string]string{
				testOwnerClaim:                        "true",
				nodeInitialUnschedulableAnnotationKey: "false",
			},
			wantAcquired:     true,
			wantCordoned:     true,
			wantOwner:        "true",
			wantInitialState: "false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &flakyNodeAPI{
				nodeName:      "gpu-node",
				unschedulable: tt.unschedulable,
				annotations:   tt.annotations,
				labels:        tt.labels,
			}
			c := newHTTPTestClient(t, api)

			acquired, err := c.AcquireNodeCordon("gpu-node", testOwnerClaim, testPeerClaim)
			require.NoError(t, err)
			require.Equal(t, tt.wantAcquired, acquired)
			require.Equal(t, tt.wantCordoned, api.cordoned())
			require.Equal(t, tt.wantOwner, api.annotation(testOwnerClaim))
			require.Equal(t, tt.wantInitialState, api.annotation(nodeInitialUnschedulableAnnotationKey))
			require.Equal(t, tt.wantPatches, api.patchCount())
		})
	}
}

func TestWaitUntilNoExternalCordonReturnsImmediately(t *testing.T) {
	tests := []struct {
		name          string
		unschedulable bool
		annotations   map[string]string
		labels        map[string]string
	}{
		{name: "when the node is schedulable"},
		{
			name:          "when a GPU component has claimed the cordon",
			unschedulable: true,
			annotations:   map[string]string{testOwnerClaim: "true"},
		},
		{
			name:          "when driver-manager recorded initial state",
			unschedulable: true,
			annotations:   map[string]string{nodeInitialUnschedulableAnnotationKey: "false"},
		},
		{
			name:          "when the upgrade controller recorded initial state",
			unschedulable: true,
			annotations:   map[string]string{gpuUpgradeInitialUnschedulableAnnotationKey: "false"},
		},
		{
			name:          "when the upgrade controller is mid-upgrade",
			unschedulable: true,
			labels:        map[string]string{gpuUpgradeStateLabelKey: "pod-restart-required"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &flakyNodeAPI{
				nodeName:      "gpu-node",
				unschedulable: tt.unschedulable,
				annotations:   tt.annotations,
				labels:        tt.labels,
			}
			c := newHTTPTestClient(t, api)
			c.pollInterval = time.Millisecond

			require.NoError(t, c.WaitUntilNoExternalCordon("gpu-node", testOwnerClaim, testPeerClaim))
		})
	}
}

func TestWaitUntilNoExternalCordonReturnsOnceCleared(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*flakyNodeAPI)
	}{
		{
			name:   "when the node is uncordoned",
			mutate: func(api *flakyNodeAPI) { api.setUnschedulable(false) },
		},
		{
			name:   "when a GPU claim appears",
			mutate: func(api *flakyNodeAPI) { api.setAnnotation(testPeerClaim, "true") },
		},
		{
			name:   "when the upgrade controller records initial state",
			mutate: func(api *flakyNodeAPI) { api.setAnnotation(gpuUpgradeInitialUnschedulableAnnotationKey, "false") },
		},
		{
			name:   "when the upgrade controller enters an in-progress state",
			mutate: func(api *flakyNodeAPI) { api.setLabel(gpuUpgradeStateLabelKey, "pod-restart-required") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &flakyNodeAPI{nodeName: "gpu-node", unschedulable: true}
			c := newHTTPTestClient(t, api)
			c.pollInterval = time.Millisecond

			done := make(chan error, 1)
			go func() {
				done <- c.WaitUntilNoExternalCordon("gpu-node", testOwnerClaim, testPeerClaim)
			}()

			time.Sleep(20 * time.Millisecond)
			tt.mutate(api)

			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for external cordon to clear")
			}
		})
	}
}

func TestWaitUntilNoExternalCordonStopsWhenContextCancelled(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", unschedulable: true}
	c := newHTTPTestClient(t, api)
	c.pollInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx

	done := make(chan error, 1)
	go func() {
		done <- c.WaitUntilNoExternalCordon("gpu-node", testOwnerClaim, testPeerClaim)
	}()

	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled wait to return")
	}
}

func TestWaitUntilNoExternalCordonRetriesGetFailures(t *testing.T) {
	api := &flakyNodeAPI{nodeName: "gpu-node", getFailures: 2}
	c := newHTTPTestClient(t, api)
	c.pollInterval = time.Millisecond

	require.NoError(t, c.WaitUntilNoExternalCordon("gpu-node", testOwnerClaim, testPeerClaim))
}

func TestReleaseNodeCordonPreservesPeerClaim(t *testing.T) {
	api := &flakyNodeAPI{
		nodeName:      "gpu-node",
		unschedulable: true,
		annotations: map[string]string{
			testOwnerClaim:                        "true",
			testPeerClaim:                         "true",
			nodeInitialUnschedulableAnnotationKey: "false",
		},
	}
	c := newHTTPTestClient(t, api)

	require.NoError(t, c.ReleaseNodeCordon("gpu-node", testOwnerClaim, testPeerClaim))
	require.True(t, api.cordoned())
	require.Empty(t, api.annotation(testOwnerClaim))
	require.Equal(t, "true", api.annotation(testPeerClaim))
	require.Equal(t, "false", api.annotation(nodeInitialUnschedulableAnnotationKey))
}

func TestReleaseNodeCordonUncordonsWhenSoleOwner(t *testing.T) {
	api := &flakyNodeAPI{
		nodeName:      "gpu-node",
		unschedulable: true,
		annotations: map[string]string{
			testOwnerClaim:                        "true",
			nodeInitialUnschedulableAnnotationKey: "false",
		},
	}
	c := newHTTPTestClient(t, api)

	require.NoError(t, c.ReleaseNodeCordon("gpu-node", testOwnerClaim, testPeerClaim))
	require.False(t, api.cordoned())
	require.Empty(t, api.annotation(testOwnerClaim))
	require.Empty(t, api.annotation(nodeInitialUnschedulableAnnotationKey))
}

func TestReleaseNodeCordonPreservesUpgradeControllerOwnership(t *testing.T) {
	api := &flakyNodeAPI{
		nodeName:      "gpu-node",
		unschedulable: true,
		annotations: map[string]string{
			testOwnerClaim: "true",
			gpuUpgradeInitialUnschedulableAnnotationKey: "false",
		},
	}
	c := newHTTPTestClient(t, api)

	require.NoError(t, c.ReleaseNodeCordon("gpu-node", testOwnerClaim, testPeerClaim))
	require.True(t, api.cordoned())
	require.Empty(t, api.annotation(testOwnerClaim))
	require.Equal(t, "false", api.annotation(gpuUpgradeInitialUnschedulableAnnotationKey))
}

func TestAcquireNodeCordonPatchIsAtomic(t *testing.T) {
	client, clientset := newTestClient(newTestNode(false, nil))

	acquired, err := client.AcquireNodeCordon(testNodeName, testOwnerClaim, testPeerClaim)
	require.NoError(t, err)
	require.True(t, acquired)

	patches := getNodePatches(t, clientset)
	require.Len(t, patches, 1)
	require.Equal(t, true, patches[0]["spec"].(map[string]any)["unschedulable"])
	metadata := patches[0]["metadata"].(map[string]any)
	require.Equal(t, "1", metadata["resourceVersion"])
	annotations := metadata["annotations"].(map[string]any)
	require.Equal(t, "true", annotations[testOwnerClaim])
	require.Equal(t, "false", annotations[nodeInitialUnschedulableAnnotationKey])

	node := getTestNode(t, clientset)
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, "true", node.Annotations[testOwnerClaim])
	require.Equal(t, "false", node.Annotations[nodeInitialUnschedulableAnnotationKey])
}

func TestGPUCordonClaimed(t *testing.T) {
	tests := []struct {
		name        string
		node        *corev1.Node
		wantClaimed bool
		wantPeer    bool
	}{
		{name: "nil node"},
		{
			name:        "owner claim",
			node:        &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{testOwnerClaim: "true"}}},
			wantClaimed: true,
		},
		{
			name:        "peer claim",
			node:        &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{testPeerClaim: "true"}}},
			wantClaimed: true,
			wantPeer:    true,
		},
		{
			name:        "driver-manager initial state is a claim but not a remaining peer",
			node:        &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{nodeInitialUnschedulableAnnotationKey: "false"}}},
			wantClaimed: true,
		},
		{
			name:        "upgrade controller initial state",
			node:        &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{gpuUpgradeInitialUnschedulableAnnotationKey: "false"}}},
			wantClaimed: true,
			wantPeer:    true,
		},
		{
			name:        "upgrade controller mid-upgrade label",
			node:        &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{gpuUpgradeStateLabelKey: "pod-restart-required"}}},
			wantClaimed: true,
			wantPeer:    true,
		},
		{
			name: "upgrade-done is not a claim",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{gpuUpgradeStateLabelKey: "upgrade-done"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantClaimed, gpuCordonClaimed(tt.node, testOwnerClaim, testPeerClaim))
			require.Equal(t, tt.wantPeer, gpuPeerOwnsCordon(tt.node, testPeerClaim))
		})
	}
}
