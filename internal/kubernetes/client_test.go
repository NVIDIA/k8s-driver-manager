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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

func discardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

func newHTTPTestClient(t *testing.T, api *flakyNodeAPI) *Client {
	t.Helper()

	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)

	return &Client{ctx: context.Background(), log: discardLogger(), clientset: clientset}
}

func (f *flakyNodeAPI) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patches
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
	testNamespace = "gpu-operator"
	testClaimName = "gpu-claim"
)

func newTestClientWithObjects(objects ...runtime.Object) *Client {
	return &Client{
		ctx:       context.Background(),
		log:       discardLogger(),
		clientset: fake.NewSimpleClientset(objects...),
	}
}

func newTestClaim(name, driver string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{{Driver: driver}},
				},
			},
		},
	}
}

// newStoppedClaimHolder returns a terminating pod that references the claim and whose
// containers have stopped.
func newStoppedClaimHolder(name, claimName string) *corev1.Pod {
	deletionTimestamp := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         testNamespace,
			Name:              name,
			UID:               types.UID(name + "-uid"),
			Labels:            map[string]string{"app": "claim-holder"},
			DeletionTimestamp: &deletionTimestamp,
			Finalizers:        []string{"test/keep"},
		},
		Spec: corev1.PodSpec{
			NodeName:       testNodeName,
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: &claimName}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "main",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
			}},
		},
	}
}

func TestIsPodWaitingForGPUClaimUnprepare(t *testing.T) {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}

	testCases := []struct {
		description string
		mutate      func(*corev1.Pod)
		claimDriver string
		expected    bool
	}{
		{
			description: "terminating pod with stopped containers and a GPU claim",
			claimDriver: nvidiaDRADriverName,
			expected:    true,
		},
		{
			description: "containers have not started",
			mutate: func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			},
			claimDriver: nvidiaDRADriverName,
			expected:    true,
		},
		{
			description: "pod is not terminating",
			mutate:      func(pod *corev1.Pod) { pod.DeletionTimestamp = nil },
			claimDriver: nvidiaDRADriverName,
			expected:    false,
		},
		{
			description: "pod is in a terminal phase",
			mutate:      func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodSucceeded },
			claimDriver: nvidiaDRADriverName,
			expected:    false,
		},
		{
			description: "container is still running",
			mutate:      func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].State = running },
			claimDriver: nvidiaDRADriverName,
			expected:    false,
		},
		{
			description: "sidecar container is still running",
			mutate: func(pod *corev1.Pod) {
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "sidecar", State: running}}
			},
			claimDriver: nvidiaDRADriverName,
			expected:    false,
		},
		{
			description: "ephemeral container is still running",
			mutate: func(pod *corev1.Pod) {
				pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", State: running}}
			},
			claimDriver: nvidiaDRADriverName,
			expected:    false,
		},
		{
			description: "claim is allocated by another driver",
			claimDriver: "other.example.com",
			expected:    false,
		},
		{
			description: "pod has no claims",
			mutate:      func(pod *corev1.Pod) { pod.Spec.ResourceClaims = nil },
			claimDriver: nvidiaDRADriverName,
			expected:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			pod := newStoppedClaimHolder("claim-holder", testClaimName)
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			client := newTestClientWithObjects(newTestClaim(testClaimName, tc.claimDriver))

			waiting, err := client.IsPodWaitingForGPUClaimUnprepare(*pod)
			require.NoError(t, err)
			require.Equal(t, tc.expected, waiting)
		})
	}
}

func TestIsAnyPodReady(t *testing.T) {
	selector := map[string]string{"app": "kubelet-plugin"}

	newPod := func(name, appLabel string, ready corev1.ConditionStatus, terminating bool) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: testNamespace,
				Name:      name,
				Labels:    map[string]string{"app": appLabel},
			},
			Spec: corev1.PodSpec{NodeName: testNodeName},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
			},
		}
		if terminating {
			deletionTimestamp := metav1.Now()
			pod.DeletionTimestamp = &deletionTimestamp
			pod.Finalizers = []string{"test/keep"}
		}
		return pod
	}

	testCases := []struct {
		description string
		pods        []runtime.Object
		expected    bool
	}{
		{
			description: "no pods",
			expected:    false,
		},
		{
			description: "ready pod",
			pods:        []runtime.Object{newPod("plugin", "kubelet-plugin", corev1.ConditionTrue, false)},
			expected:    true,
		},
		{
			description: "pod is not ready",
			pods:        []runtime.Object{newPod("plugin", "kubelet-plugin", corev1.ConditionFalse, false)},
			expected:    false,
		},
		{
			description: "ready pod is terminating",
			pods:        []runtime.Object{newPod("plugin", "kubelet-plugin", corev1.ConditionTrue, true)},
			expected:    false,
		},
		{
			description: "ready pod does not match the selector",
			pods:        []runtime.Object{newPod("other", "other", corev1.ConditionTrue, false)},
			expected:    false,
		},
		{
			description: "terminating pod and ready replacement",
			pods: []runtime.Object{
				newPod("old-plugin", "kubelet-plugin", corev1.ConditionTrue, true),
				newPod("new-plugin", "kubelet-plugin", corev1.ConditionTrue, false),
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			client := newTestClientWithObjects(tc.pods...)

			ready, err := client.IsAnyPodReady(selector, testNamespace, testNodeName)
			require.NoError(t, err)
			require.Equal(t, tc.expected, ready)
		})
	}
}

func TestWaitForPodTerminationExcept(t *testing.T) {
	selector := map[string]string{"app": "claim-holder"}
	ignoreFirst := func(pod corev1.Pod) bool { return pod.Name == "first" }

	testCases := []struct {
		description string
		pods        []runtime.Object
		ignore      func(corev1.Pod) bool
		expectError bool
	}{
		{
			description: "no pods",
			expectError: false,
		},
		{
			description: "pod remains",
			pods:        []runtime.Object{newStoppedClaimHolder("first", testClaimName)},
			expectError: true,
		},
		{
			description: "every remaining pod is ignored",
			pods:        []runtime.Object{newStoppedClaimHolder("first", testClaimName)},
			ignore:      ignoreFirst,
			expectError: false,
		},
		{
			description: "a remaining pod is not ignored",
			pods: []runtime.Object{
				newStoppedClaimHolder("first", testClaimName),
				newStoppedClaimHolder("second", testClaimName),
			},
			ignore:      ignoreFirst,
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			client := newTestClientWithObjects(tc.pods...)

			err := client.WaitForPodTerminationExcept(selector, testNamespace, testNodeName, 10*time.Millisecond, tc.ignore)
			if tc.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestGetGPUResourceClaimHoldersIgnoring(t *testing.T) {
	completed := newStoppedClaimHolder("completed", "completed-claim")
	completed.Status.Phase = corev1.PodSucceeded

	client := newTestClientWithObjects(
		newTestClaim("ignored-claim", nvidiaDRADriverName),
		newTestClaim("holder-claim", nvidiaDRADriverName),
		newTestClaim("completed-claim", nvidiaDRADriverName),
		newStoppedClaimHolder("ignored", "ignored-claim"),
		newStoppedClaimHolder("holder", "holder-claim"),
		completed,
	)

	holders, err := client.GetGPUResourceClaimHolders(testNodeName, func(pod corev1.Pod) bool {
		return pod.UID == "ignored-uid"
	})
	require.NoError(t, err)
	require.Equal(t, []string{testNamespace + "/holder"}, holders)

	holders, err = client.GetGPUResourceClaimHolders(testNodeName, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{testNamespace + "/ignored", testNamespace + "/holder"}, holders)
}

// newTestComputeDomainClaim returns a claim allocated by the NVIDIA ComputeDomain DRA
// driver with an opaque config of the given kind, as the ComputeDomain controller creates it.
func newTestComputeDomainClaim(name, configKind string) *resourcev1.ResourceClaim {
	claim := newTestClaim(name, computeDomainDRADriverName)
	claim.Status.Allocation.Devices.Config = []resourcev1.DeviceAllocationConfiguration{{
		Source:   resourcev1.AllocationConfigSourceClaim,
		Requests: []string{"daemon"},
		DeviceConfiguration: resourcev1.DeviceConfiguration{
			Opaque: &resourcev1.OpaqueDeviceConfiguration{
				Driver: computeDomainDRADriverName,
				Parameters: runtime.RawExtension{
					Raw: []byte(`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"` + configKind + `","domainID":"test-domain"}`),
				},
			},
		},
	}}
	return claim
}

func TestIsAllocatedComputeDomainDaemonClaim(t *testing.T) {
	testCases := []struct {
		description string
		claim       *resourcev1.ResourceClaim
		expected    bool
	}{
		{
			description: "daemon claim",
			claim:       newTestComputeDomainClaim(testClaimName, computeDomainDaemonConfigKind),
			expected:    true,
		},
		{
			description: "channel claim",
			claim:       newTestComputeDomainClaim(testClaimName, "ComputeDomainChannelConfig"),
			expected:    false,
		},
		{
			description: "GPU claim",
			claim:       newTestClaim(testClaimName, nvidiaDRADriverName),
			expected:    false,
		},
		{
			description: "daemon claim is not allocated",
			claim: func() *resourcev1.ResourceClaim {
				claim := newTestComputeDomainClaim(testClaimName, computeDomainDaemonConfigKind)
				claim.Status.Allocation = nil
				return claim
			}(),
			expected: false,
		},
		{
			description: "daemon config is for another driver",
			claim: func() *resourcev1.ResourceClaim {
				claim := newTestComputeDomainClaim(testClaimName, computeDomainDaemonConfigKind)
				claim.Status.Allocation.Devices.Config[0].Opaque.Driver = "other.example.com"
				return claim
			}(),
			expected: false,
		},
		{
			description: "config parameters are not valid JSON",
			claim: func() *resourcev1.ResourceClaim {
				claim := newTestComputeDomainClaim(testClaimName, computeDomainDaemonConfigKind)
				claim.Status.Allocation.Devices.Config[0].Opaque.Parameters.Raw = []byte("not json")
				return claim
			}(),
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			require.Equal(t, tc.expected, isAllocatedComputeDomainDaemonClaim(tc.claim))
		})
	}
}

func TestWaitForComputeDomainDaemons(t *testing.T) {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	// newDaemonPod returns a running IMEX daemon pod that is not terminating.
	newDaemonPod := func() *corev1.Pod {
		pod := newStoppedClaimHolder("imex-daemon", "daemon-claim")
		pod.DeletionTimestamp = nil
		pod.Finalizers = nil
		pod.Status.ContainerStatuses[0].State = running
		return pod
	}

	testCases := []struct {
		description string
		pods        []runtime.Object
		expectError bool
	}{
		{
			description: "no pods",
			expectError: false,
		},
		{
			description: "daemon pod is running",
			pods:        []runtime.Object{newDaemonPod()},
			expectError: true,
		},
		{
			description: "daemon pod is terminating and its container is still running",
			pods: []runtime.Object{func() *corev1.Pod {
				pod := newStoppedClaimHolder("imex-daemon", "daemon-claim")
				pod.Status.ContainerStatuses[0].State = running
				return pod
			}()},
			expectError: true,
		},
		{
			description: "daemon pod is terminating and its containers have stopped",
			pods:        []runtime.Object{newStoppedClaimHolder("imex-daemon", "daemon-claim")},
			expectError: false,
		},
		{
			description: "daemon pod is in a terminal phase",
			pods: []runtime.Object{func() *corev1.Pod {
				pod := newDaemonPod()
				pod.Status.Phase = corev1.PodSucceeded
				return pod
			}()},
			expectError: false,
		},
		{
			description: "running pods hold only channel and GPU claims",
			pods: []runtime.Object{func() *corev1.Pod {
				pod := newDaemonPod()
				pod.Name = "workload"
				channelClaim, gpuClaim := "channel-claim", "gpu-claim"
				pod.Spec.ResourceClaims = []corev1.PodResourceClaim{
					{Name: "channel", ResourceClaimName: &channelClaim},
					{Name: "gpu", ResourceClaimName: &gpuClaim},
				}
				return pod
			}()},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			objects := append([]runtime.Object{
				newTestComputeDomainClaim("daemon-claim", computeDomainDaemonConfigKind),
				newTestComputeDomainClaim("channel-claim", "ComputeDomainChannelConfig"),
				newTestClaim("gpu-claim", nvidiaDRADriverName),
			}, tc.pods...)
			client := newTestClientWithObjects(objects...)

			err := client.WaitForComputeDomainDaemons(testNodeName, 10*time.Millisecond)
			if tc.expectError {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDeleteOrEvictPodsIgnoring(t *testing.T) {
	setController := func(pod *corev1.Pod, kind, name string) *corev1.Pod {
		controller := true
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, Controller: &controller}}
		return pod
	}
	daemonSet := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "dra-validator"}}
	// A GPU Operator pod that waitForPodsToTerminate stopped waiting for: a terminating
	// DaemonSet pod that still holds a GPU claim.
	newSkippedPod := func() runtime.Object {
		return setController(newStoppedClaimHolder("validator", "validator-claim"), "DaemonSet", daemonSet.Name)
	}
	// A running user pod that holds a GPU claim.
	newWorkloadPod := func() runtime.Object {
		pod := setController(newStoppedClaimHolder("workload", "workload-claim"), "ReplicaSet", "workload")
		pod.DeletionTimestamp = nil
		pod.Finalizers = nil
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		return pod
	}
	ignoreSkippedPod := func(pod corev1.Pod) bool { return pod.UID == "validator-uid" }

	testCases := []struct {
		description   string
		pods          []runtime.Object
		ignore        func(corev1.Pod) bool
		expectError   bool
		remainingPods []string
	}{
		{
			description:   "skipped pod is counted, but the drain helper does not delete DaemonSet pods",
			pods:          []runtime.Object{newSkippedPod()},
			expectError:   true,
			remainingPods: []string{"validator"},
		},
		{
			description:   "skipped pod is ignored",
			pods:          []runtime.Object{newSkippedPod()},
			ignore:        ignoreSkippedPod,
			expectError:   false,
			remainingPods: []string{"validator"},
		},
		{
			description:   "user pod is deleted while the skipped pod is ignored",
			pods:          []runtime.Object{newSkippedPod(), newWorkloadPod()},
			ignore:        ignoreSkippedPod,
			expectError:   false,
			remainingPods: []string{"validator"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			objects := append([]runtime.Object{
				daemonSet,
				newTestClaim("validator-claim", nvidiaDRADriverName),
				newTestClaim("workload-claim", nvidiaDRADriverName),
			}, tc.pods...)
			clientset := fake.NewSimpleClientset(objects...)
			// Discovery without the eviction subresource makes the drain helper delete pods.
			clientset.Resources = []*metav1.APIResourceList{{GroupVersion: "v1"}}
			client := &Client{ctx: context.Background(), log: discardLogger(), clientset: clientset}

			err := client.DeleteOrEvictPods(testNodeName, DrainOptions{}, tc.ignore)
			if tc.expectError {
				require.ErrorContains(t, err, "failed to delete all GPU pods")
			} else {
				require.NoError(t, err)
			}

			pods, err := clientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			var remaining []string
			for _, pod := range pods.Items {
				remaining = append(remaining, pod.Name)
			}
			require.ElementsMatch(t, tc.remainingPods, remaining)
		})
	}
}
