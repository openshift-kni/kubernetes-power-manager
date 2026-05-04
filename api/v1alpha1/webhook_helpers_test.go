/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespace = "power-manager"

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = AddToScheme(scheme)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&corev1.Pod{}, podPowerProfileIndex, extractPowerProfiles).
		Build()
}

func nodeSelector(matchLabels map[string]string) NodeSelector {
	return NodeSelector{
		LabelSelector: metav1.LabelSelector{MatchLabels: matchLabels},
	}
}

func nodeSelectorWithExpressions(exprs []metav1.LabelSelectorRequirement) NodeSelector {
	return NodeSelector{
		LabelSelector: metav1.LabelSelector{MatchExpressions: exprs},
	}
}

func nodeSelectorMixed(matchLabels map[string]string, exprs []metav1.LabelSelectorRequirement) NodeSelector {
	return NodeSelector{
		LabelSelector: metav1.LabelSelector{
			MatchLabels:      matchLabels,
			MatchExpressions: exprs,
		},
	}
}

func TestValidateNamespace(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		wantErr   bool
		errMsg    string
	}{
		{
			name:      "correct namespace - no error",
			namespace: testNamespace,
		},
		{
			name:      "wrong namespace - error",
			namespace: "other-ns",
			wantErr:   true,
			errMsg:    fmt.Sprintf("must be created in namespace %q", testNamespace),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNamespace(tc.namespace, testNamespace, "TestResource")
			if tc.wantErr {
				require.NotNil(t, err)
				assert.Contains(t, err.Detail, tc.errMsg)
				assert.Equal(t, "metadata.namespace", err.Field)
			} else {
				assert.Nil(t, err)
			}
		})
	}
}

func TestFindNodeSelectorConflicts(t *testing.T) {
	tests := []struct {
		name         string
		selfSelector NodeSelector
		peers        map[string]NodeSelector
		nodes        []client.Object
		wantErr      bool
		errContains  []string
	}{
		// --- matchLabels tests ---
		{
			name:         "no peers - no conflict",
			selfSelector: nodeSelector(map[string]string{"zone": "us-east"}),
			peers:        map[string]NodeSelector{},
			nodes:        []client.Object{testNode("node1", map[string]string{"zone": "us-east"})},
		},
		{
			name:         "identical matchLabels - conflict without node lookup",
			selfSelector: nodeSelector(map[string]string{"zone": "us-east"}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"zone": "us-east"}),
			},
			nodes:       nil,
			wantErr:     true,
			errContains: []string{"identical nodeSelector", "peer-a"},
		},
		{
			name:         "identical empty selectors - conflict",
			selfSelector: NodeSelector{},
			peers: map[string]NodeSelector{
				"peer-a": {},
			},
			wantErr:     true,
			errContains: []string{"identical nodeSelector"},
		},
		{
			name:         "different selectors with node overlap",
			selfSelector: nodeSelector(map[string]string{"zone": "us-east"}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"role": "worker"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east", "role": "worker"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1"},
		},
		{
			name:         "different selectors no node overlap",
			selfSelector: nodeSelector(map[string]string{"zone": "us-east"}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"zone": "us-west"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"zone": "us-west"}),
			},
		},
		{
			name:         "multiple overlapping nodes shown",
			selfSelector: nodeSelector(map[string]string{"zone": "us-east"}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"role": "worker"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east", "role": "worker"}),
				testNode("node2", map[string]string{"zone": "us-east", "role": "worker"}),
				testNode("node3", map[string]string{"zone": "us-west", "role": "worker"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1", "node2"},
		},
		{
			name:         "multiple peers - one identical one node overlap",
			selfSelector: nodeSelector(map[string]string{"zone": "us-east"}),
			peers: map[string]NodeSelector{
				"peer-identical": nodeSelector(map[string]string{"zone": "us-east"}),
				"peer-overlap":   nodeSelector(map[string]string{"role": "worker"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east", "role": "worker"}),
			},
			wantErr:     true,
			errContains: []string{"identical nodeSelector", "both select node(s)"},
		},
		{
			name:         "multiple peers - no conflicts",
			selfSelector: nodeSelector(map[string]string{"zone": "us-east"}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"zone": "us-west"}),
				"peer-b": nodeSelector(map[string]string{"zone": "eu-west"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"zone": "us-west"}),
				testNode("node3", map[string]string{"zone": "eu-west"}),
			},
		},
		// --- matchExpressions tests ---
		{
			name: "matchExpressions In - identical selectors conflict",
			selfSelector: nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
				{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"us-east", "us-west"}},
			}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
					{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"us-east", "us-west"}},
				}),
			},
			wantErr:     true,
			errContains: []string{"identical nodeSelector"},
		},
		{
			name: "matchExpressions In - overlapping values with node match",
			selfSelector: nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
				{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"us-east", "us-west"}},
			}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
					{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"us-west", "eu-west"}},
				}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-west"}),
				testNode("node2", map[string]string{"zone": "eu-west"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1"},
		},
		{
			name: "matchExpressions In - no overlapping values",
			selfSelector: nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
				{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"us-east"}},
			}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
					{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"us-west"}},
				}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"zone": "us-west"}),
			},
		},
		{
			name: "matchExpressions NotIn - conflict on matching node",
			selfSelector: nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
				{Key: "zone", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"us-west"}},
			}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
					{Key: "zone", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"us-east"}},
				}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "eu-west"}),
				testNode("node2", map[string]string{"zone": "us-east"}),
				testNode("node3", map[string]string{"zone": "us-west"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1"},
		},
		{
			name: "matchExpressions Exists - conflict on node with label",
			selfSelector: nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
				{Key: "gpu", Operator: metav1.LabelSelectorOpExists},
			}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"zone": "us-east"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east", "gpu": "nvidia"}),
				testNode("node2", map[string]string{"zone": "us-east"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1"},
		},
		{
			name: "matchExpressions Exists - no conflict when label absent",
			selfSelector: nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
				{Key: "gpu", Operator: metav1.LabelSelectorOpExists},
			}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"zone": "us-east"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"gpu": "nvidia"}),
			},
		},
		{
			name: "matchExpressions DoesNotExist - conflict on node without label",
			selfSelector: nodeSelectorWithExpressions([]metav1.LabelSelectorRequirement{
				{Key: "gpu", Operator: metav1.LabelSelectorOpDoesNotExist},
			}),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"zone": "us-east"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"zone": "us-east", "gpu": "nvidia"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1"},
		},
		// --- mixed matchLabels and matchExpressions tests ---
		{
			name: "mixed matchLabels and matchExpressions - conflict",
			selfSelector: nodeSelectorMixed(
				map[string]string{"zone": "us-east"},
				[]metav1.LabelSelectorRequirement{
					{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"frontend", "backend"}},
				},
			),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"role": "worker"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east", "tier": "frontend", "role": "worker"}),
				testNode("node2", map[string]string{"zone": "us-east", "tier": "database", "role": "worker"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1"},
		},
		{
			name: "mixed matchLabels and matchExpressions - no conflict",
			selfSelector: nodeSelectorMixed(
				map[string]string{"zone": "us-east"},
				[]metav1.LabelSelectorRequirement{
					{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"frontend"}},
				},
			),
			peers: map[string]NodeSelector{
				"peer-a": nodeSelectorMixed(
					map[string]string{"zone": "us-east"},
					[]metav1.LabelSelectorRequirement{
						{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"backend"}},
					},
				),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east", "tier": "frontend"}),
				testNode("node2", map[string]string{"zone": "us-east", "tier": "backend"}),
			},
		},
		{
			name:         "empty self selector matches all nodes - conflict with any peer that matches",
			selfSelector: NodeSelector{},
			peers: map[string]NodeSelector{
				"peer-a": nodeSelector(map[string]string{"zone": "us-east"}),
			},
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
			},
			wantErr:     true,
			errContains: []string{"both select node(s)", "node1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{}, tc.nodes...)
			cl := newFakeClient(objs...)

			errs := findNodeSelectorConflicts(context.TODO(), cl, tc.selfSelector, tc.peers)
			if tc.wantErr {
				require.NotEmpty(t, errs, "expected validation errors")
				errStr := errs.ToAggregate().Error()
				for _, want := range tc.errContains {
					assert.Contains(t, errStr, want)
				}
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}
