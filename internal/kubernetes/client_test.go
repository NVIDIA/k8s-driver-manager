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

package kubernetes

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestClient(node *corev1.Node) *Client {
	return &Client{
		ctx:       context.Background(),
		log:       logrus.New(),
		clientset: fake.NewSimpleClientset(node),
	}
}

func getTestNode(t *testing.T, c *Client, nodeName string) *corev1.Node {
	node, err := c.clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	return node
}

func TestCordonUncordonNode(t *testing.T) {
	testCases := []struct {
		description           string
		unschedulable         bool
		annotations           map[string]string
		cordon                bool
		expectedUnschedulable bool
	}{
		{
			description:           "schedulable node is cordoned and uncordoned",
			unschedulable:         false,
			cordon:                true,
			expectedUnschedulable: false,
		},
		{
			description:           "already cordoned node stays cordoned after uncordon",
			unschedulable:         true,
			cordon:                true,
			expectedUnschedulable: true,
		},
		{
			description:           "restart after cordon keeps the recorded initial state and uncordons",
			unschedulable:         true,
			annotations:           map[string]string{nodeInitialStateAnnotation: "false"},
			cordon:                true,
			expectedUnschedulable: false,
		},
		{
			description:           "stale recording on a schedulable node is reconciled before cordon",
			unschedulable:         false,
			annotations:           map[string]string{nodeInitialStateAnnotation: "true"},
			cordon:                true,
			expectedUnschedulable: false,
		},
		{
			description:           "uncordon without prior cordon leaves an external cordon in place",
			unschedulable:         true,
			cordon:                false,
			expectedUnschedulable: true,
		},
		{
			description:           "uncordon without prior cordon leaves a schedulable node schedulable",
			unschedulable:         false,
			cordon:                false,
			expectedUnschedulable: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: tc.annotations,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: tc.unschedulable,
				},
			}
			c := newTestClient(node)

			if tc.cordon {
				require.NoError(t, c.CordonNode("test-node"))
				cordoned := getTestNode(t, c, "test-node")
				require.True(t, cordoned.Spec.Unschedulable)
			}

			require.NoError(t, c.UncordonNode("test-node"))
			uncordoned := getTestNode(t, c, "test-node")
			require.Equal(t, tc.expectedUnschedulable, uncordoned.Spec.Unschedulable)
			require.NotContains(t, uncordoned.Annotations, nodeInitialStateAnnotation)
		})
	}
}

func TestCordonNodeRecordsInitialState(t *testing.T) {
	testCases := []struct {
		description        string
		unschedulable      bool
		annotations        map[string]string
		expectedAnnotation string
	}{
		{
			description:        "schedulable node is recorded as schedulable",
			unschedulable:      false,
			expectedAnnotation: "false",
		},
		{
			description:        "already cordoned node is recorded as unschedulable",
			unschedulable:      true,
			expectedAnnotation: "true",
		},
		{
			description:        "an existing recording is not overwritten while the node is unschedulable",
			unschedulable:      true,
			annotations:        map[string]string{nodeInitialStateAnnotation: "false"},
			expectedAnnotation: "false",
		},
		{
			description:        "a stale recording on a schedulable node is overwritten",
			unschedulable:      false,
			annotations:        map[string]string{nodeInitialStateAnnotation: "true"},
			expectedAnnotation: "false",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: tc.annotations,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: tc.unschedulable,
				},
			}
			c := newTestClient(node)

			require.NoError(t, c.CordonNode("test-node"))
			cordoned := getTestNode(t, c, "test-node")
			require.True(t, cordoned.Spec.Unschedulable)
			require.Equal(t, tc.expectedAnnotation, cordoned.Annotations[nodeInitialStateAnnotation])
		})
	}
}
