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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func uintPtr(v uint) *uint { return &v }

func testUncore(name string, spec UncoreSpec) *Uncore {
	return &Uncore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       spec,
	}
}

func testUncoreWithSelector(name string, spec UncoreSpec, matchLabels map[string]string) *Uncore {
	u := testUncore(name, spec)
	if matchLabels != nil {
		u.Spec.NodeSelector = NodeSelector{
			LabelSelector: metav1.LabelSelector{MatchLabels: matchLabels},
		}
	}
	return u
}

func TestUncoreValidation_SystemWide(t *testing.T) {
	tests := []struct {
		name    string
		sysMin  *uint
		sysMax  *uint
		wantErr bool
		errMsg  string
	}{
		{
			name:   "valid sys uncore",
			sysMin: uintPtr(800), sysMax: uintPtr(2400),
		},
		{
			name:   "equal min and max",
			sysMin: uintPtr(1200), sysMax: uintPtr(1200),
		},
		{
			name:   "sysMin > sysMax",
			sysMin: uintPtr(2400), sysMax: uintPtr(800),
			wantErr: true,
			errMsg:  "sysMin (2400) must be <= sysMax (800)",
		},
		{
			name:   "sysMin only - valid",
			sysMin: uintPtr(800),
		},
		{
			name:   "sysMax only - valid",
			sysMax: uintPtr(2400),
		},
		{
			name: "both nil - valid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &uncoreValidator{Client: newFakeClient(), Namespace: testNamespace}
			uncore := testUncore("test", UncoreSpec{SysMin: tc.sysMin, SysMax: tc.sysMax})
			_, err := v.validateCreateOrUpdate(context.TODO(), uncore)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestUncoreValidation_DieSelectors(t *testing.T) {
	tests := []struct {
		name    string
		dies    []DieSelector
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid single die selector",
			dies: []DieSelector{
				{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
			},
		},
		{
			name: "valid package-level selector (no die)",
			dies: []DieSelector{
				{Package: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
			},
		},
		{
			name: "min > max",
			dies: []DieSelector{
				{Package: uintPtr(0), Min: uintPtr(2400), Max: uintPtr(800)},
			},
			wantErr: true,
			errMsg:  "min (2400) must be <= max (800)",
		},
		{
			name: "duplicate package-level selectors",
			dies: []DieSelector{
				{Package: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
				{Package: uintPtr(0), Min: uintPtr(1000), Max: uintPtr(2000)},
				{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(1200), Max: uintPtr(1800)},
			},
			wantErr: true,
			errMsg:  "duplicate: package 0 already specified in dieSelector[0]",
		},
		{
			name: "duplicate (package, die) selectors",
			dies: []DieSelector{
				{Package: uintPtr(0), Die: uintPtr(1), Min: uintPtr(800), Max: uintPtr(2400)},
				{Package: uintPtr(0), Die: uintPtr(1), Min: uintPtr(1000), Max: uintPtr(2000)},
			},
			wantErr: true,
			errMsg:  "duplicate: package 0, die 1 already specified in dieSelector[0]",
		},
		{
			name: "package-level with die-specific override - no conflict",
			dies: []DieSelector{
				{Package: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
				{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(1000), Max: uintPtr(2000)},
			},
		},
		{
			name: "package-level with multiple die-specific overrides - no conflict",
			dies: []DieSelector{
				{Package: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
				{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(1000), Max: uintPtr(2000)},
				{Package: uintPtr(0), Die: uintPtr(1), Min: uintPtr(1200), Max: uintPtr(1800)},
			},
		},
		{
			name: "different dies on same package - no conflict",
			dies: []DieSelector{
				{Package: uintPtr(0), Die: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
				{Package: uintPtr(0), Die: uintPtr(1), Min: uintPtr(1000), Max: uintPtr(2000)},
			},
		},
		{
			name: "different packages - no conflict",
			dies: []DieSelector{
				{Package: uintPtr(0), Min: uintPtr(800), Max: uintPtr(2400)},
				{Package: uintPtr(1), Min: uintPtr(1000), Max: uintPtr(2000)},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateDieSelectors(tc.dies)
			if tc.wantErr {
				require.NotEmpty(t, errs)
				assert.Contains(t, errs.ToAggregate().Error(), tc.errMsg)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}

func TestUncoreConflictDetection(t *testing.T) {
	tests := []struct {
		name     string
		uncore   *Uncore
		existing []client.Object
		nodes    []client.Object
		wantErr  bool
		errMsg   string
	}{
		{
			name:   "no existing uncores - no conflict",
			uncore: testUncoreWithSelector("uc-a", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-east"}),
			nodes:  []client.Object{testNode("node1", map[string]string{"zone": "us-east"})},
		},
		{
			name:   "non-overlapping selectors - no conflict",
			uncore: testUncoreWithSelector("uc-b", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-west"}),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
				testNode("node2", map[string]string{"zone": "us-west"}),
			},
			existing: []client.Object{
				testUncoreWithSelector("uc-a", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-east"}),
			},
		},
		{
			name:   "identical selectors - conflict",
			uncore: testUncoreWithSelector("uc-b", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-east"}),
			existing: []client.Object{
				testUncoreWithSelector("uc-a", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-east"}),
			},
			wantErr: true,
			errMsg:  "identical nodeSelector",
		},
		{
			name:   "different selectors - conflict on matching node",
			uncore: testUncoreWithSelector("uc-b", UncoreSpec{SysMax: uintPtr(2400)}, nil),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
			},
			existing: []client.Object{
				testUncoreWithSelector("uc-a", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-east"}),
			},
			wantErr: true,
			errMsg:  "both select node(s)",
		},
		{
			name:   "update self - no self-conflict",
			uncore: testUncoreWithSelector("uc-a", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-east"}),
			nodes: []client.Object{
				testNode("node1", map[string]string{"zone": "us-east"}),
			},
			existing: []client.Object{
				testUncoreWithSelector("uc-a", UncoreSpec{SysMax: uintPtr(2400)}, map[string]string{"zone": "us-east"}),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(tc.existing, tc.nodes...)
			v := &uncoreValidator{Client: newFakeClient(objs...), Namespace: testNamespace}
			errs := v.validateNodeSelectorConflicts(context.TODO(), tc.uncore)
			if tc.wantErr {
				require.NotEmpty(t, errs)
				assert.Contains(t, errs.ToAggregate().Error(), tc.errMsg)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}
