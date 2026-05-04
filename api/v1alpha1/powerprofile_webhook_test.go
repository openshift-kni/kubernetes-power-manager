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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testPod(name, namespace string, resources corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "busybox",
					Resources: corev1.ResourceRequirements{
						Requests: resources,
						Limits:   resources,
					},
				},
			},
		},
	}
}

func TestPowerProfileValidatePowerNodeConfigReferences(t *testing.T) {
	tests := []struct {
		name       string
		profile    *PowerProfile
		objs       []client.Object
		sharedOnly bool
		wantRef    bool
		refMsg     string
	}{
		{
			name:    "no PowerNodeConfigs - allow",
			profile: testPowerProfile("my-prof", false),
		},
		{
			name:    "referenced as sharedPowerProfile - reject",
			profile: testPowerProfile("shared-prof", true),
			objs: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", nil, nil),
			},
			wantRef: true,
			refMsg:  "cfg-a (sharedPowerProfile)",
		},
		{
			name:    "referenced in reservedCPUs - reject",
			profile: testPowerProfile("res-prof", false),
			objs: []client.Object{
				testPowerNodeConfig("cfg-a", "other-prof", nil, []ReservedSpec{
					{Cores: []uint{0, 1}, PowerProfile: "res-prof"},
				}),
			},
			wantRef: true,
			refMsg:  "cfg-a (reservedCPUs[0])",
		},
		{
			name:    "referenced by multiple PowerNodeConfigs",
			profile: testPowerProfile("shared-prof", true),
			objs: []client.Object{
				testPowerNodeConfig("cfg-a", "shared-prof", nil, nil),
				testPowerNodeConfig("cfg-b", "shared-prof", nil, nil),
			},
			wantRef: true,
			refMsg:  "cfg-a (sharedPowerProfile)",
		},
		{
			name:    "not referenced - allow",
			profile: testPowerProfile("unused-prof", false),
			objs: []client.Object{
				testPowerNodeConfig("cfg-a", "other-prof", nil, nil),
			},
		},
		{
			name:       "sharedOnly: referenced as sharedPowerProfile - reject",
			profile:    testPowerProfile("prof", true),
			sharedOnly: true,
			objs: []client.Object{
				testPowerNodeConfig("cfg-a", "prof", nil, nil),
			},
			wantRef: true,
			refMsg:  "cfg-a (sharedPowerProfile)",
		},
		{
			name:       "sharedOnly: referenced only in reservedCPUs - allow",
			profile:    testPowerProfile("prof", false),
			sharedOnly: true,
			objs: []client.Object{
				testPowerNodeConfig("cfg-a", "other", nil, []ReservedSpec{
					{Cores: []uint{0, 1}, PowerProfile: "prof"},
				}),
			},
		},
		{
			name:       "sharedOnly: no references - allow",
			profile:    testPowerProfile("prof", true),
			sharedOnly: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &powerProfileValidator{Client: newFakeClient(tc.objs...), Namespace: testNamespace}
			err := v.validatePowerNodeConfigReferences(context.TODO(), tc.profile, tc.sharedOnly)
			if tc.wantRef {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.refMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPowerProfileValidatePodReferences(t *testing.T) {
	tests := []struct {
		name    string
		profile *PowerProfile
		objs    []client.Object
		wantRef bool
		refMsg  string
	}{
		{
			name:    "no pods - allow delete",
			profile: testPowerProfile("my-prof", false),
		},
		{
			name:    "pod requests extended resource - reject",
			profile: testPowerProfile("performance", false),
			objs: []client.Object{
				testPod("my-pod", testNamespace, corev1.ResourceList{
					corev1.ResourceCPU: *resource.NewQuantity(2, resource.DecimalSI),
					corev1.ResourceName(extendedResourcePrefix + "performance"): *resource.NewQuantity(2, resource.DecimalSI),
				}),
			},
			wantRef: true,
			refMsg:  "power-manager/my-pod",
		},
		{
			name:    "pod requests different profile - allow delete",
			profile: testPowerProfile("performance", false),
			objs: []client.Object{
				testPod("my-pod", testNamespace, corev1.ResourceList{
					corev1.ResourceCPU: *resource.NewQuantity(2, resource.DecimalSI),
					corev1.ResourceName(extendedResourcePrefix + "power-saving"): *resource.NewQuantity(2, resource.DecimalSI),
				}),
			},
		},
		{
			name:    "completed pod requests profile - allow delete",
			profile: testPowerProfile("performance", false),
			objs: []client.Object{
				func() client.Object {
					p := testPod("done-pod", testNamespace, corev1.ResourceList{
						corev1.ResourceName(extendedResourcePrefix + "performance"): *resource.NewQuantity(2, resource.DecimalSI),
					})
					p.Status.Phase = corev1.PodSucceeded
					return p
				}(),
			},
		},
		{
			name:    "failed pod requests profile - allow delete",
			profile: testPowerProfile("performance", false),
			objs: []client.Object{
				func() client.Object {
					p := testPod("failed-pod", testNamespace, corev1.ResourceList{
						corev1.ResourceName(extendedResourcePrefix + "performance"): *resource.NewQuantity(2, resource.DecimalSI),
					})
					p.Status.Phase = corev1.PodFailed
					return p
				}(),
			},
		},
		{
			name:    "multiple pods reference profile",
			profile: testPowerProfile("performance", false),
			objs: []client.Object{
				testPod("pod-a", testNamespace, corev1.ResourceList{
					corev1.ResourceName(extendedResourcePrefix + "performance"): *resource.NewQuantity(2, resource.DecimalSI),
				}),
				testPod("pod-b", "other-ns", corev1.ResourceList{
					corev1.ResourceName(extendedResourcePrefix + "performance"): *resource.NewQuantity(1, resource.DecimalSI),
				}),
			},
			wantRef: true,
			refMsg:  "pod-a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &powerProfileValidator{Client: newFakeClient(tc.objs...), Namespace: testNamespace}
			err := v.validatePodReferences(context.TODO(), tc.profile)
			if tc.wantRef {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.refMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
