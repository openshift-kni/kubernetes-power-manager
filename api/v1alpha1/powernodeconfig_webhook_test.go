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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testPowerProfile(name string, shared bool) *PowerProfile {
	return &PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       PowerProfileSpec{Shared: shared},
	}
}

func testPowerNodeConfig(name string, sharedProfile string, matchLabels map[string]string, reserved []ReservedSpec) *PowerNodeConfig {
	pnc := &PowerNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: PowerNodeConfigSpec{
			SharedPowerProfile: sharedProfile,
			ReservedCPUs:       reserved,
		},
	}
	if matchLabels != nil {
		pnc.Spec.NodeSelector = NodeSelector{
			LabelSelector: metav1.LabelSelector{MatchLabels: matchLabels},
		}
	}
	return pnc
}

func testNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestValidateReservedCPUDisjoint(t *testing.T) {
	tests := []struct {
		name     string
		reserved []ReservedSpec
		wantErr  bool
		errMsg   string
	}{
		{
			name:     "no reserved CPUs",
			reserved: nil,
			wantErr:  false,
		},
		{
			name: "single group",
			reserved: []ReservedSpec{
				{Cores: []uint{0, 1, 2}, PowerProfile: "prof-a"},
			},
			wantErr: false,
		},
		{
			name: "disjoint groups",
			reserved: []ReservedSpec{
				{Cores: []uint{0, 1}, PowerProfile: "prof-a"},
				{Cores: []uint{2, 3}, PowerProfile: "prof-b"},
			},
			wantErr: false,
		},
		{
			name: "overlapping groups",
			reserved: []ReservedSpec{
				{Cores: []uint{0, 1, 2}, PowerProfile: "prof-a"},
				{Cores: []uint{2, 3, 4}, PowerProfile: "prof-b"},
			},
			wantErr: true,
			errMsg:  "CPU 2 already appears in reservedCPUs[0]",
		},
		{
			name: "overlap across three groups",
			reserved: []ReservedSpec{
				{Cores: []uint{0, 1}, PowerProfile: "prof-a"},
				{Cores: []uint{2, 3}, PowerProfile: "prof-b"},
				{Cores: []uint{3, 4}, PowerProfile: "prof-c"},
			},
			wantErr: true,
			errMsg:  "CPU 3 already appears in reservedCPUs[1]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &powerNodeConfigValidator{Client: newFakeClient(), Namespace: testNamespace}
			errs := v.validateReservedCPUDisjoint(tc.reserved)
			if tc.wantErr {
				require.NotEmpty(t, errs)
				assert.Contains(t, errs[0].Detail, tc.errMsg)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}

func TestValidateProfiles(t *testing.T) {
	tests := []struct {
		name    string
		config  *PowerNodeConfig
		objs    []client.Object
		wantErr bool
		errMsg  string
	}{
		{
			name:   "shared profile exists and is shared",
			config: testPowerNodeConfig("cfg", "shared-prof", nil, nil),
			objs:   []client.Object{testPowerProfile("shared-prof", true)},
		},
		{
			name:    "shared profile not found",
			config:  testPowerNodeConfig("cfg", "missing-prof", nil, nil),
			objs:    []client.Object{},
			wantErr: true,
			errMsg:  "Not found",
		},
		{
			name:    "shared profile not marked shared",
			config:  testPowerNodeConfig("cfg", "perf-prof", nil, nil),
			objs:    []client.Object{testPowerProfile("perf-prof", false)},
			wantErr: true,
			errMsg:  "spec.shared set to true",
		},
		{
			name: "reserved profile not found",
			config: testPowerNodeConfig("cfg", "shared-prof", nil, []ReservedSpec{
				{Cores: []uint{0, 1}, PowerProfile: "missing-reserved"},
			}),
			objs:    []client.Object{testPowerProfile("shared-prof", true)},
			wantErr: true,
			errMsg:  "Not found",
		},
		{
			name: "all profiles valid",
			config: testPowerNodeConfig("cfg", "shared-prof", nil, []ReservedSpec{
				{Cores: []uint{0, 1}, PowerProfile: "perf-prof"},
			}),
			objs: []client.Object{
				testPowerProfile("shared-prof", true),
				testPowerProfile("perf-prof", false),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &powerNodeConfigValidator{Client: newFakeClient(tc.objs...), Namespace: testNamespace}
			errs := v.validatePowerProfiles(context.TODO(), tc.config)
			if tc.wantErr {
				require.NotEmpty(t, errs)
				assert.Contains(t, errs.ToAggregate().Error(), tc.errMsg)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}

func TestPowerNodeConfigConflictDetection(t *testing.T) {
	tests := []struct {
		name     string
		config   *PowerNodeConfig
		existing []client.Object
		nodes    []client.Object
		wantErr  bool
		errMsg   string
	}{
		{
			name:   "no existing configs - no conflict",
			config: testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			nodes:  []client.Object{testNode("node1", map[string]string{"zone": "us-east"})},
		},
		{
			name:   "non-overlapping selectors - no conflict",
			config: testPowerNodeConfig("cfg-b", "shared-prof", map[string]string{"zone": "us-west"}, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"zone": "us-west"}),
			},
			existing: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			},
		},
		{
			name:   "identical selectors - conflict without node lookup",
			config: testPowerNodeConfig("cfg-b", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
			},
			existing: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			},
			wantErr: true,
			errMsg:  "identical nodeSelector",
		},
		{
			name:   "identical selectors conflict even without matching nodes",
			config: testPowerNodeConfig("cfg-b", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-west"}),
			},
			existing: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			},
			wantErr: true,
			errMsg:  "identical nodeSelector",
		},
		{
			name:   "empty selector on both - identical conflict",
			config: testPowerNodeConfig("cfg-b", "shared-prof", nil, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{}),
			},
			existing: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", nil, nil),
			},
			wantErr: true,
			errMsg:  "identical nodeSelector",
		},
		{
			name:   "different selectors - conflict on matching node shows all nodes",
			config: testPowerNodeConfig("cfg-b", "shared-prof", nil, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"zone": "us-west"}),
			},
			existing: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			},
			wantErr: true,
			errMsg:  "both select node(s)",
		},
		{
			name:   "different selectors no node overlap - no conflict",
			config: testPowerNodeConfig("cfg-b", "shared-prof", map[string]string{"zone": "us-west"}, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
			},
			existing: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			},
		},
		{
			name:   "update self - no self-conflict",
			config: testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
			},
			existing: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", map[string]string{"zone": "us-east"}, nil),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(tc.existing, tc.nodes...)
			v := &powerNodeConfigValidator{Client: newFakeClient(objs...)}
			errs := v.validateNodeSelectorConflicts(context.TODO(), tc.config)
			if tc.wantErr {
				require.NotEmpty(t, errs)
				assert.Contains(t, errs.ToAggregate().Error(), tc.errMsg)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}
