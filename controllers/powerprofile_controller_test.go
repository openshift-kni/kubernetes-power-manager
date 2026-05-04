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

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"testing"

	"github.com/intel/power-optimization-library/pkg/power"
	powerv1alpha1 "github.com/openshift-kni/kubernetes-power-manager/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func createProfileReconcilerObject(objs []runtime.Object) (*PowerProfileReconciler, error) {
	log.SetLogger(zap.New(
		zap.UseDevMode(true),
		func(opts *zap.Options) {
			opts.TimeEncoder = zapcore.ISO8601TimeEncoder
		},
	),
	)
	// Register operator types with the runtime scheme.
	s := scheme.Scheme

	// Add route Openshift scheme
	if err := powerv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}

	// Create a fake client to mock API calls.
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&powerv1alpha1.PowerNodeState{}).
		Build()

	// Create a ReconcileNode object with the scheme and fake client.
	r := &PowerProfileReconciler{cl, ctrl.Log.WithName("testing"), s, nil}

	return r, nil
}

func TestPowerProfile_Reconcile_ExclusivePoolCreation(t *testing.T) {
	nodeName := "TestNode"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Status: corev1.NodeStatus{
			Capacity: map[corev1.ResourceName]resource.Quantity{
				CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
			},
		},
	}

	testCases := []struct {
		name         string
		powerprofile *powerv1alpha1.PowerProfile
	}{
		{
			name: "Exclusive pool creation",
			powerprofile: &powerv1alpha1.PowerProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "performance",
					Namespace: PowerNamespace,
				},
				Spec: powerv1alpha1.PowerProfileSpec{
					PStates: powerv1alpha1.PStatesConfig{
						Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
						Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
						Epp:      "performance",
						Governor: "powersave",
					},
					CStates: powerv1alpha1.CStatesConfig{
						Names: map[string]bool{
							"C0":  true,
							"C1":  true,
							"C1E": false,
						},
					},
				},
			},
		},
		{
			name: "Exclusive pool creation with cstates latency",
			powerprofile: &powerv1alpha1.PowerProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "performance",
					Namespace: PowerNamespace,
				},
				Spec: powerv1alpha1.PowerProfileSpec{
					PStates: powerv1alpha1.PStatesConfig{
						Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
						Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
						Epp:      "performance",
						Governor: "powersave",
					},
					CStates: powerv1alpha1.CStatesConfig{
						MaxLatencyUs: &[]int{10}[0],
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NODE_NAME", nodeName)

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      tc.powerprofile.Name,
					Namespace: PowerNamespace,
				},
			}

			r, err := createProfileReconcilerObject([]runtime.Object{node, tc.powerprofile})
			if err != nil {
				t.Error(err)
				t.Fatalf("error creating the reconciler object")
			}

			host, teardown, err := fullDummySystem()
			if err != nil {
				t.Error(err)
				t.Fatalf("error setting up dummy system: %v", err)
			}
			defer teardown()
			r.PowerLibrary = host

			_, err = r.Reconcile(context.TODO(), req)
			assert.Nil(t, err)

			exPool := host.GetExclusivePool(tc.powerprofile.Name)
			assert.NotNil(t, exPool, "Exclusive pool should be created")
			exProfile := exPool.GetPowerProfile()
			assert.Equal(t, tc.powerprofile.Name, exProfile.Name(), "pool should have the correct name")
			if tc.powerprofile.Spec.CStates.MaxLatencyUs != nil {
				assert.Equal(t, *tc.powerprofile.Spec.CStates.MaxLatencyUs, *exProfile.GetCStates().GetMaxLatencyUs())
				assert.Nil(t, exProfile.GetCStates().States())
			}
			if tc.powerprofile.Spec.CStates.Names != nil {
				assert.Equal(t, tc.powerprofile.Spec.CStates.Names, exProfile.GetCStates().States())
				assert.Nil(t, exProfile.GetCStates().GetMaxLatencyUs())
			}

			// Check extended resource creation on node
			updatedNode := &corev1.Node{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{Name: nodeName}, updatedNode)
			assert.NoError(t, err)
			extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, tc.powerprofile.Name))
			_, exists := updatedNode.Status.Capacity[extendedResourceName]
			assert.True(t, exists, "Extended resource should be created")

		})
	}
}

// basic shared pool scenario
func TestPowerProfile_Reconcile_SharedPoolCreation(t *testing.T) {
	clientObjs := []runtime.Object{
		&powerv1alpha1.PowerProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "shared",
				Namespace: PowerNamespace,
			},
			Spec: powerv1alpha1.PowerProfileSpec{
				Shared: true,
				PStates: powerv1alpha1.PStatesConfig{
					Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
					Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
					Epp: "",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "TestNode",
			},
			Status: corev1.NodeStatus{
				Capacity: map[corev1.ResourceName]resource.Quantity{
					CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
				},
			},
		},
	}
	// needed to create library using a dummy sysfs as it will call functions that can't be mocked
	_, teardown, err := fullDummySystem()
	assert.Nil(t, err)
	defer teardown()
	nodemk := new(hostMock)
	poolmk := new(poolMock)
	exPoolmmk := new(poolMock)
	freqSetmk := new(frequencySetMock)
	poolmk.On("SetPowerProfile", mock.Anything).Return(nil)
	nodemk.On("GetSharedPool").Return(poolmk)
	nodemk.On("GetExclusivePool", mock.Anything).Return(nil)
	nodemk.On("AddExclusivePool", mock.Anything).Return(exPoolmmk, nil)
	exPoolmmk.On("SetPowerProfile", mock.Anything).Return(nil)
	nodemk.On("GetFreqRanges").Return(power.CoreTypeList{freqSetmk})
	freqSetmk.On("GetMax").Return(uint(9000000))
	freqSetmk.On("GetMin").Return(uint(100000))
	// Mock GetAllCpus to return an empty CpuList
	nodemk.On("GetAllCpus").Return(new(power.CpuList))
	t.Setenv("NODE_NAME", "TestNode")
	r, err := createProfileReconcilerObject(clientObjs)
	assert.Nil(t, err)
	r.PowerLibrary = nodemk
	assert.Nil(t, err)
	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      "shared",
			Namespace: PowerNamespace,
		},
	}

	_, err = r.Reconcile(context.TODO(), req)
	assert.Nil(t, err)

}

func TestPowerProfile_Reconcile_NonPowerProfileNotInLibrary(t *testing.T) {
	tcases := []struct {
		testCase    string
		nodeName    string
		profileName string
		clientObjs  []runtime.Object
	}{
		{
			testCase:    "Test Case 1 - Max|Min non zero, epp performance",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 2 - Max|Min nil, epp performance",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: nil,
							Min: nil,
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 3 - Max|Min non zero, epp empty",
			nodeName:    "TestNode",
			profileName: "user-created",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "user-created",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)
		r, err := createProfileReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}

		host, teardown, err := fullDummySystem()
		assert.Nil(t, err)
		defer teardown()
		r.PowerLibrary = host

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error reconciling object", tc.testCase)
		}

		node := &corev1.Node{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name: tc.nodeName,
		}, node)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error retrieving the node object", tc.testCase)
		}

		resourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, tc.profileName))
		if _, exists := node.Status.Capacity[resourceName]; !exists {
			t.Errorf("%s - failed: expected the extended resource '%s' to be created", tc.testCase, fmt.Sprintf("%s%s", ExtendedResourcePrefix, tc.profileName))
		}
	}
}

func TestPowerProfile_Reconcile_NonPowerProfileInLibrary(t *testing.T) {
	tcases := []struct {
		testCase    string
		nodeName    string
		profileName string
		clientObjs  []runtime.Object
	}{
		{
			testCase:    "Test Case 1 - Max|Min non zero, epp performance",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 2 - Max|Min nil, epp performance",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: nil,
							Min: nil,
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 3 - Max|Min non zero, epp empty",
			nodeName:    "TestNode",
			profileName: "user-created",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "user-created",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		r, err := createProfileReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}

		host, teardown, err := fullDummySystem()
		assert.Nil(t, err)
		defer teardown()
		r.PowerLibrary = host

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error reconciling object", tc.testCase)
		}

	}
}

func TestPowerProfile_Reconcile_MaxMinFrequencyHandling(t *testing.T) {
	tcases := []struct {
		testCase    string
		nodeName    string
		profileName string
		max         *intstr.IntOrString
		min         *intstr.IntOrString
		epp         string
		governor    string
	}{
		// Partial defaults - max only
		{
			testCase:    "Max is nil, min specified, no EPP (use hardware maximum)",
			nodeName:    "TestNode",
			profileName: "max-default-profile",
			max:         nil,
			min:         &intstr.IntOrString{Type: intstr.Int, IntVal: 2000},
			epp:         "",
			governor:    "powersave",
		},
		{
			testCase:    "Max is nil, min specified, with EPP (use hardware maximum)",
			nodeName:    "TestNode",
			profileName: "max-default-epp-profile",
			max:         nil,
			min:         &intstr.IntOrString{Type: intstr.Int, IntVal: 1500},
			epp:         "performance",
			governor:    "powersave",
		},
		// Partial defaults - min only
		{
			testCase:    "Min is nil, max specified, no EPP (use hardware minimum)",
			nodeName:    "TestNode",
			profileName: "min-default-profile",
			max:         &intstr.IntOrString{Type: intstr.Int, IntVal: 3000}, // 3000 MHz
			min:         nil,
			epp:         "",
			governor:    "powersave",
		},
		{
			testCase:    "Min is nil, max specified as percentage, no EPP (use hardware minimum)",
			nodeName:    "TestNode",
			profileName: "min-default-profile",
			max:         &intstr.IntOrString{Type: intstr.String, StrVal: "75%"},
			min:         nil,
			epp:         "",
			governor:    "powersave",
		},
		{
			testCase:    "Min is nil, max specified, with EPP (use hardware minimum)",
			nodeName:    "TestNode",
			profileName: "min-default-epp-profile",
			max:         &intstr.IntOrString{Type: intstr.Int, IntVal: 2500},
			min:         nil,
			epp:         "balance_power",
			governor:    "powersave",
		},
		// Both zero - EPP-based calculation
		{
			testCase:    "Both nil with EPP (use EPP-based calculation)",
			nodeName:    "TestNode",
			profileName: "epp-based-profile",
			max:         nil,
			min:         nil,
			epp:         "balance_performance",
			governor:    "powersave",
		},
		// Both zero - hardware defaults
		{
			testCase:    "Both nil without EPP (use hardware defaults)",
			nodeName:    "TestNode",
			profileName: "hardware-default-profile",
			max:         nil,
			min:         nil,
			epp:         "",
			governor:    "powersave",
		},
		// Both specified - use as-is
		{
			testCase:    "Both values specified, no EPP (use as-is)",
			nodeName:    "TestNode",
			profileName: "explicit-values-profile",
			max:         &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
			min:         &intstr.IntOrString{Type: intstr.Int, IntVal: 1800},
			epp:         "",
			governor:    "powersave",
		},
		{
			testCase:    "Both values specified as percentage, with EPP (use as-is)",
			nodeName:    "TestNode",
			profileName: "explicit-values-epp-profile",
			max:         &intstr.IntOrString{Type: intstr.String, StrVal: "75%"},
			min:         &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
			epp:         "balance_performance",
			governor:    "powersave",
		},
		{
			testCase:    "Both values as 0% is a valid configuration",
			nodeName:    "TestNode",
			profileName: "explicit-values-epp-profile",
			max:         &intstr.IntOrString{Type: intstr.String, StrVal: "0%"},
			min:         &intstr.IntOrString{Type: intstr.String, StrVal: "0%"},
			epp:         "balance_performance",
			governor:    "powersave",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			t.Setenv("NODE_NAME", tc.nodeName)

			clientObjs := []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tc.profileName,
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max:      tc.max,
							Min:      tc.min,
							Epp:      tc.epp,
							Governor: tc.governor,
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: tc.nodeName,
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			}

			r, err := createProfileReconcilerObject(clientObjs)
			if err != nil {
				t.Fatalf("error creating the reconciler object: %v", err)
			}

			host, teardown, err := fullDummySystem()
			assert.Nil(t, err)
			defer teardown()
			r.PowerLibrary = host

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      tc.profileName,
					Namespace: PowerNamespace,
				},
			}

			_, err = r.Reconcile(context.TODO(), req)
			assert.NoError(t, err, "unexpected error: %v", err)

			// Verify the profile was created in the power library
			exclusivePool := host.GetExclusivePool(tc.profileName)
			assert.NotNil(t, exclusivePool, "exclusive pool should be created")
			assert.NotNil(t, exclusivePool.GetPowerProfile(), "pool should have a power profile")
		})
	}
}

func TestPowerProfile_Reconcile_MaxMinFrequencyValidationErrors(t *testing.T) {
	tcases := []struct {
		testCase      string
		nodeName      string
		profileName   string
		max           *intstr.IntOrString
		min           *intstr.IntOrString
		epp           string
		governor      string
		expectedError string
	}{
		{
			testCase:      "Max lower than min (both specified)",
			nodeName:      "TestNode",
			profileName:   "invalid-range-profile",
			max:           &intstr.IntOrString{Type: intstr.Int, IntVal: 2600},
			min:           &intstr.IntOrString{Type: intstr.Int, IntVal: 2800},
			epp:           "performance",
			governor:      "powersave",
			expectedError: "max frequency (2600) cannot be lower than the min frequency (2800)",
		},
		{
			testCase:      "Max lower than min as percentage (both specified)",
			nodeName:      "TestNode",
			profileName:   "invalid-range-profile",
			max:           &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
			min:           &intstr.IntOrString{Type: intstr.String, StrVal: "75%"},
			epp:           "performance",
			governor:      "powersave",
			expectedError: "max frequency (25%) cannot be lower than the min frequency (75%)",
		},
		{
			testCase:      "Max and min of different types (both specified)",
			nodeName:      "TestNode",
			profileName:   "invalid-range-profile",
			max:           &intstr.IntOrString{Type: intstr.Int, IntVal: 2500},
			min:           &intstr.IntOrString{Type: intstr.String, StrVal: "75%"},
			epp:           "performance",
			governor:      "powersave",
			expectedError: "max and min frequency must be either numeric or percentage",
		},
		{
			testCase:      "Values outside hardware range - too low",
			nodeName:      "TestNode",
			profileName:   "too-low-profile",
			max:           &intstr.IntOrString{Type: intstr.Int, IntVal: 100},
			min:           &intstr.IntOrString{Type: intstr.Int, IntVal: 50},
			epp:           "",
			governor:      "powersave",
			expectedError: "max and min frequency must be within the range",
		},
		{
			testCase:      "Values outside hardware range - too high",
			nodeName:      "TestNode",
			profileName:   "too-high-profile",
			max:           &intstr.IntOrString{Type: intstr.Int, IntVal: 9999999},
			min:           &intstr.IntOrString{Type: intstr.Int, IntVal: 9999998},
			epp:           "",
			governor:      "powersave",
			expectedError: "max and min frequency must be within the range",
		},
		{
			testCase:      "Min specified higher than hardware max when max=nil",
			nodeName:      "TestNode",
			profileName:   "min-too-high-profile",
			max:           nil,                                                 // Will use hardware max (3700 in our mock)
			min:           &intstr.IntOrString{Type: intstr.Int, IntVal: 5000}, // Higher than hardware max
			epp:           "",
			governor:      "powersave",
			expectedError: "max frequency (3700) cannot be lower than the min frequency (5000)",
		},
		{
			testCase:      "Shared profile with values outside hardware range",
			nodeName:      "TestNode",
			profileName:   "shared-too-low",
			max:           &intstr.IntOrString{Type: intstr.Int, IntVal: 100},
			min:           &intstr.IntOrString{Type: intstr.Int, IntVal: 100},
			epp:           "",
			governor:      "powersave",
			expectedError: "max and min frequency must be within the range",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			t.Setenv("NODE_NAME", tc.nodeName)

			clientObjs := []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tc.profileName,
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max:      tc.max,
							Min:      tc.min,
							Epp:      tc.epp,
							Governor: tc.governor,
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: tc.nodeName,
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			}

			r, err := createProfileReconcilerObject(clientObjs)
			if err != nil {
				t.Fatalf("error creating the reconciler object: %v", err)
			}

			host, teardown, err := fullDummySystem()
			assert.Nil(t, err)
			defer teardown()
			r.PowerLibrary = host

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      tc.profileName,
					Namespace: PowerNamespace,
				},
			}

			_, err = r.Reconcile(context.TODO(), req)
			assert.Error(t, err, "expected an error but got none")
			assert.ErrorContains(t, err, tc.expectedError)

		})
	}
}

func TestPowerProfile_Reconcile_IncorrectEppValue(t *testing.T) {
	tcases := []struct {
		testCase      string
		nodeName      string
		profileName   string
		clientObjs    []runtime.Object
		expectedError string
	}{
		{
			testCase:    "Test Case 1 - Epp value incorrect",
			nodeName:    "TestNode",
			profileName: "user-created",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "user-created",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "incorrect",
						},
					},
				},
			},
			expectedError: "EPP value not allowed",
		},
	}
	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			t.Setenv("NODE_NAME", tc.nodeName)

			r, err := createProfileReconcilerObject(tc.clientObjs)
			if err != nil {
				t.Fatalf("error creating the reconciler object: %v", err)
			}

			nodemk := new(hostMock)
			// Mock GetAllCpus to return an empty CpuList
			nodemk.On("GetAllCpus").Return(new(power.CpuList))
			r.PowerLibrary = nodemk

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      tc.profileName,
					Namespace: PowerNamespace,
				},
			}

			// Reconcile should return an error for invalid EPP values
			_, err = r.Reconcile(context.TODO(), req)
			assert.Error(t, err, "expected an error for invalid EPP value")
			assert.ErrorContains(t, err, tc.expectedError)

			// Profile should still exist (controller doesn't delete it, just returns error)
			profile := &powerv1alpha1.PowerProfile{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			}, profile)
			assert.NoError(t, err, "profile should still exist after invalid EPP error")

		})
	}
}

func TestPowerProfile_Reconcile_SharedProfileDoesNotExistInLibrary(t *testing.T) {
	tcases := []struct {
		testCase    string
		nodeName    string
		profileName string
		clientObjs  []runtime.Object
	}{
		{
			testCase:    "Test Case 1 - Profile does not exists in Power Library",
			nodeName:    "TestNode",
			profileName: "shared",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						Shared: true,
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 800},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 800},
						},
					},
				},
			},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		r, err := createProfileReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}
		host, teardown, err := fullDummySystem()
		assert.Nil(t, err)
		defer teardown()
		r.PowerLibrary = host
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.ErrorContains(t, err, "max and min frequency must be within the range")
	}
}

func TestPowerProfile_Reconcile_DeleteProfile(t *testing.T) {
	tcases := []struct {
		testCase    string
		nodeName    string
		profileName string
		clientObjs  []runtime.Object
	}{
		{
			testCase:    "Test Case 1 - Profile performance, ERs present",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
							"power.cluster-power-manager.github.io/performance": *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 2 - Profile user-created, ERs not present",
			nodeName:    "TestNode",
			profileName: "user-created",
			clientObjs: []runtime.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
	}
	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		r, err := createProfileReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}
		dummyShared := new(poolMock)
		dummyProf := new(profMock)
		pool := new(poolMock)
		pool.On("Remove").Return(nil)
		nodemk := new(hostMock)
		nodemk.On("GetExclusivePool", tc.profileName).Return(pool)
		nodemk.On("GetSharedPool").Return(dummyShared)
		dummyShared.On("GetPowerProfile").Return(dummyProf)
		dummyProf.On("Name").Return("shared")
		r.PowerLibrary = nodemk

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error reconciling object", tc.testCase)
		}

		node := &corev1.Node{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name: tc.nodeName,
		}, node)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error retrieving the node object", tc.testCase)
		}

		resourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, tc.profileName))
		if _, exists := node.Status.Capacity[resourceName]; exists {
			t.Errorf("%s - failed: expected the extended resource '%s' to have been deleted", tc.testCase, fmt.Sprintf("%s%s", ExtendedResourcePrefix, tc.profileName))
		}
	}
}

func TestPowerProfile_Reconcile_AcpiDriver(t *testing.T) {
	tcases := []struct {
		testCase    string
		nodeName    string
		profileName string
		clientObjs  []runtime.Object
	}{
		{
			testCase:    "Test Case 1 - Max|Min non zero, epp performance",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 2 - Max|Min nil, epp performance",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: nil,
							Min: nil,
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 3 - Max|Min non zero, epp empty",
			nodeName:    "TestNode",
			profileName: "user-created",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "user-created",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		r, err := createProfileReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}

		host, teardown, err := setupDummyFiles(86, 1, 2, map[string]string{
			"driver": "acpi-cpufreq", "max": "3700000", "min": "1000000",
			"epp": "performance", "governor": "performance",
			"package": "0", "die": "0", "available_governors": "powersave performance",
			"uncore_max": "2400000", "uncore_min": "1200000",
			"cstates": "intel_idle"})
		assert.Nil(t, err)
		defer teardown()
		r.PowerLibrary = host

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error reconciling object", tc.testCase)
		}

	}
}

// tests that force an error from a library function
// validateErr required as some instances result in nil being returned by reconciler
// to prevent requeueing
func TestPowerProfile_Reconcile_LibraryErrs(t *testing.T) {
	tcases := []struct {
		testCase      string
		profileName   string
		powerNodeName string
		getNodemk     func() *hostMock
		validateErr   func(e error) bool
		clientObjs    []runtime.Object
	}{
		{
			testCase:      "Test Case 1 - exclusive pool does not exist",
			profileName:   "",
			powerNodeName: "TestNode",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				dummyShared := new(poolMock)
				dummyProf := new(profMock)
				nodemk.On("GetExclusivePool", mock.Anything).Return(nil)
				nodemk.On("GetSharedPool").Return(dummyShared)
				nodemk.On("GetAllCpus").Return(new(power.CpuList))
				dummyShared.On("GetPowerProfile").Return(dummyProf)
				dummyProf.On("Name").Return("shared")
				return nodemk
			},
			validateErr: func(e error) bool {
				return assert.Error(t, e)
			},
			clientObjs: []runtime.Object{},
		},
		{
			testCase:    "Test Case 2 - Pool creation error",
			profileName: "performance",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				nodemk.On("GetExclusivePool", mock.Anything).Return(nil)
				nodemk.On("AddExclusivePool", mock.Anything).Return(nil, fmt.Errorf("Pool creation err"))
				nodemk.On("GetAllCpus").Return(new(power.CpuList))
				freqSetmk := new(frequencySetMock)
				nodemk.On("GetFreqRanges").Return(power.CoreTypeList{freqSetmk})
				freqSetmk.On("GetMax").Return(uint(9000000))
				freqSetmk.On("GetMin").Return(uint(100000))
				return nodemk
			},
			validateErr: func(e error) bool {
				return assert.ErrorContains(t, e, "Pool creation err")
			},
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 3 - Set power profile error",
			profileName: "performance",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				poolmk := new(poolMock)
				nodemk.On("GetExclusivePool", mock.Anything).Return(nil)
				nodemk.On("GetAllCpus").Return(new(power.CpuList))
				freqSetmk := new(frequencySetMock)
				nodemk.On("GetFreqRanges").Return(power.CoreTypeList{freqSetmk})
				freqSetmk.On("GetMax").Return(uint(9000000))
				freqSetmk.On("GetMin").Return(uint(100000))
				nodemk.On("AddExclusivePool", mock.Anything).Return(poolmk, nil)
				poolmk.On("SetPowerProfile", mock.Anything).Return(fmt.Errorf("Set profile err"))
				return nodemk
			},
			validateErr: func(e error) bool {
				return assert.ErrorContains(t, e, "Set profile err")
			},
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "performance",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 4 - reset shared profile error",
			profileName: "shared",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				poolmk := new(poolMock)
				profmk := new(profMock)
				nodemk.On("GetSharedPool").Return(poolmk)
				poolmk.On("GetPowerProfile").Return(profmk)
				nodemk.On("GetAllCpus").Return(new(power.CpuList))
				profmk.On("Name").Return("shared")
				poolmk.On("SetPowerProfile", mock.Anything).Return(fmt.Errorf("Set profile err"))
				return nodemk
			},
			validateErr: func(e error) bool {
				return assert.ErrorContains(t, e, "Set profile err")
			},
			clientObjs: []runtime.Object{},
		},
		{
			testCase:    "Test Case 5 - dummy pool retrieval error",
			profileName: "shared",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				poolmk := new(poolMock)
				profmk := new(profMock)
				nodemk.On("GetSharedPool").Return(poolmk)
				poolmk.On("GetPowerProfile").Return(profmk)
				nodemk.On("GetAllCpus").Return(new(power.CpuList))
				profmk.On("Name").Return("shared")
				poolmk.On("SetPowerProfile", mock.Anything).Return(nil)
				nodemk.On("GetExclusivePool", mock.Anything).Return(nil)
				return nodemk
			},
			validateErr: func(e error) bool {
				return assert.ErrorContains(t, e, "pool not found")
			},
			clientObjs: []runtime.Object{},
		},
		{
			testCase:    "Test Case 6 - dummy pool removal error",
			profileName: "shared",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				poolmk := new(poolMock)
				dummyPoolmk := new(poolMock)
				profmk := new(profMock)
				nodemk.On("GetSharedPool").Return(poolmk)
				nodemk.On("GetAllCpus").Return(new(power.CpuList))
				poolmk.On("GetPowerProfile").Return(profmk)
				profmk.On("Name").Return("shared")
				poolmk.On("SetPowerProfile", mock.Anything).Return(nil)
				nodemk.On("GetExclusivePool", mock.Anything).Return(dummyPoolmk)
				dummyPoolmk.On("Remove").Return(fmt.Errorf("pool removal err"))
				return nodemk
			},
			validateErr: func(e error) bool {
				return assert.ErrorContains(t, e, "pool removal err")
			},
			clientObjs: []runtime.Object{},
		},
	}

	_, teardown, err := fullDummySystem()
	assert.Nil(t, err)
	defer teardown()
	for _, tc := range tcases {
		t.Setenv("NODE_NAME", "TestNode")
		r, err := createProfileReconcilerObject(tc.clientObjs)
		assert.Nil(t, err)
		r.PowerLibrary = tc.getNodemk()
		assert.Nil(t, err)
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		tc.validateErr(err)
	}
	teardown()
	tc := tcases[1]
	t.Setenv("NODE_NAME", "TestNode")
	r, err := createProfileReconcilerObject(tc.clientObjs)
	assert.Nil(t, err)
	r.PowerLibrary = tc.getNodemk()
	assert.Nil(t, err)
	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      tc.profileName,
			Namespace: PowerNamespace,
		},
	}

	_, err = r.Reconcile(context.TODO(), req)
	tc.validateErr(err)
}

// covers epp not supported error logs
// does not result in returned error as this is recoverable
func TestPowerProfile_Reconcile_FeatureNotSupportedErr(t *testing.T) {
	tcases := []struct {
		testCase    string
		profileName string
		clientObjs  []runtime.Object
	}{
		{
			testCase:    "Test Case 1 - Shared profile",
			profileName: "shared",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						Shared: true,
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "power",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 2 - Exclusive profile",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max: &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min: &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp: "power",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
	}
	setupDummyFiles(1, 1, 1, map[string]string{
		"available_governors": "powersave performance",
		"epp":                 "performance",
	})
	t.Setenv("NODE_NAME", "TestNode")
	for _, tc := range tcases {
		r, err := createProfileReconcilerObject(tc.clientObjs)
		assert.Nil(t, err)
		nodemk := new(hostMock)
		freqSetmk := new(frequencySetMock)
		nodemk.On("GetFreqRanges").Return(power.CoreTypeList{freqSetmk})
		freqSetmk.On("GetMax").Return(uint(9000000))
		freqSetmk.On("GetMin").Return(uint(100000))
		nodemk.On("GetAllCpus").Return(new(power.CpuList))
		poolmk := new(poolMock)
		nodemk.On("GetExclusivePool", mock.Anything).Return(poolmk)
		r.PowerLibrary = nodemk
		assert.Nil(t, err)
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}
		_, err = r.Reconcile(context.TODO(), req)
		assert.ErrorContains(t, err, "Frequency-Scaling - failed to determine driver")
	}

}

// tests errors returned by the reconciler client using the errclient mock
func TestPowerProfile_Reconcile_ClientErrs(t *testing.T) {
	tcases := []struct {
		testCase      string
		profileName   string
		powerNodeName string
		convertClient func(client.Client) client.Client
		clientErr     string
	}{
		{
			testCase:      "Test Case 1 - Invalid Get requests",
			profileName:   "",
			powerNodeName: "TestNode",
			convertClient: func(c client.Client) client.Client {
				mkwriter := new(mockResourceWriter)
				mkwriter.On("Update", mock.Anything, mock.Anything).Return(nil)
				mkwriter.On("Patch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				mkcl := new(errClient)
				mkcl.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("client get error"))
				// mock status call in defer function call
				mkcl.On("Status").Return(mkwriter)
				return mkcl
			},
			clientErr: "client get error",
		},
		{
			testCase:      "Test Case 2 - Client get node error during extended resource removal",
			profileName:   "performance",
			powerNodeName: "TestNode",
			convertClient: func(c client.Client) client.Client {
				mkwriter := new(mockResourceWriter)
				mkwriter.On("Update", mock.Anything, mock.Anything).Return(nil)
				mkwriter.On("Patch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				mkcl := new(errClient)
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1alpha1.PowerProfile")).Return(errors.NewNotFound(schema.GroupResource{}, "profile"))
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.Node")).Return(fmt.Errorf("client get node error"))
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1alpha1.PowerNodeState")).Return(errors.NewNotFound(schema.GroupResource{}, "powernodestate"))
				mkcl.On("Status").Return(mkwriter)
				return mkcl
			},
			clientErr: "client get node error",
		},
	}

	dummyFilesystemHost, teardown, err := fullDummySystem()
	assert.Nil(t, err)
	defer teardown()
	dummyFilesystemHost.AddExclusivePool("performance")
	for _, tc := range tcases {
		t.Setenv("NODE_NAME", "TestNode")

		r, err := createProfileReconcilerObject([]runtime.Object{})
		assert.Nil(t, err)
		r.PowerLibrary = dummyFilesystemHost
		r.Client = tc.convertClient(r.Client)
		assert.Nil(t, err)
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.ErrorContains(t, err, tc.clientErr)
	}
}

// tests exclusive and shared profiles requesting invalid governors
func TestPowerProfile_Reconcile_UnsupportedGovernor(t *testing.T) {
	tcases := []struct {
		testCase    string
		nodeName    string
		profileName string
		clientObjs  []runtime.Object
	}{
		{
			testCase:    "Test Case 1 - invalid exclusive governor",
			nodeName:    "TestNode",
			profileName: "performance",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp:      "performance",
							Governor: "made up",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
		{
			testCase:    "Test Case 2 - invalid shared governor",
			nodeName:    "TestNode",
			profileName: "shared",
			clientObjs: []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						Shared: true,
						PStates: powerv1alpha1.PStatesConfig{
							Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 1000},
							Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 1000},
							Governor: "made up",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)
		r, err := createProfileReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}

		host, teardown, err := fullDummySystem()
		assert.Nil(t, err)
		defer teardown()
		r.PowerLibrary = host

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.profileName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.ErrorContains(t, err, "not supported")
	}

}

func TestPowerProfile_Wrong_Namespace(t *testing.T) {
	r, err := createProfileReconcilerObject([]runtime.Object{})
	assert.Nil(t, err)
	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      "shared",
			Namespace: "wrong-namespace",
		},
	}

	_, err = r.Reconcile(context.TODO(), req)
	assert.ErrorContains(t, err, "incorrect namespace")
}

// uses dummy sysfs so must be run in isolation from other fuzzers
// go test -fuzz FuzzPowerProfileController -run=FuzzPowerProfileController -parallel=1
func FuzzPowerProfileController(f *testing.F) {
	f.Add("TestNode", "performance", uint(3600), uint(3200), "performance", "powersave", false)
	f.Fuzz(func(t *testing.T, nodeName, prof string, maxVal uint, minVal uint, epp string, governor string, shared bool) {
		nodeName = strings.ReplaceAll(nodeName, " ", "")
		nodeName = strings.ReplaceAll(nodeName, "\t", "")
		nodeName = strings.ReplaceAll(nodeName, "\000", "")
		if len(nodeName) == 0 {
			return
		}
		t.Setenv("NODE_NAME", nodeName)

		clientObjs := []runtime.Object{
			&powerv1alpha1.PowerProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      prof,
					Namespace: PowerNamespace,
				},
				Spec: powerv1alpha1.PowerProfileSpec{
					PStates: powerv1alpha1.PStatesConfig{
						Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: int32(maxVal)},
						Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: int32(minVal)},
						Epp:      epp,
						Governor: governor,
					},
					Shared: shared,
				},
			},
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Status: corev1.NodeStatus{
					Capacity: map[corev1.ResourceName]resource.Quantity{
						CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
					},
				},
			},
		}
		r, err := createProfileReconcilerObject(clientObjs)
		assert.Nil(t, err)
		host, teardown, err := fullDummySystem()
		assert.Nil(t, err)
		defer teardown()
		r.PowerLibrary = host
		host.AddExclusivePool(prof)

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      prof,
				Namespace: PowerNamespace,
			},
		}

		r.Reconcile(context.TODO(), req)
		req = reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      "not-found",
				Namespace: PowerNamespace,
			},
		}

		r.Reconcile(context.TODO(), req)

	})
}

// tests positive and negative cases for SetupWithManager function
func TestPowerProfile_Reconcile_SetupPass(t *testing.T) {
	r, err := createProfileReconcilerObject([]runtime.Object{})
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("SetFields", mock.Anything).Return(nil)
	mgr.On("Add", mock.Anything).Return(nil)
	mgr.On("GetCache").Return(new(cacheMk))
	err = (&PowerProfileReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Nil(t, err)

}
func TestPowerProfile_Reconcile_SetupFail(t *testing.T) {
	r, err := createProfileReconcilerObject([]runtime.Object{})
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("Add", mock.Anything).Return(fmt.Errorf("setup fail"))

	err = (&PowerProfileReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Error(t, err)

}

// Test that updating a profile correctly updates all affected pools
func TestPowerProfile_Reconcile_ProfileUpdateAffectsAllPools(t *testing.T) {
	// Helper function to verify profile parameters match expected values
	verifyProfileParameters := func(profile power.Profile,
		expectedName string,
		expectedMaxMHz uint,
		expectedMinMHz uint,
		expectedGovernor string,
		expectedEpp string,
		expectedCStates map[string]bool,
	) {
		assert.Equal(t, expectedName, profile.Name())
		assert.Equal(t, expectedMaxMHz*1000, uint(profile.GetPStates().GetMaxFreq().IntVal))
		assert.Equal(t, expectedMinMHz*1000, uint(profile.GetPStates().GetMinFreq().IntVal))
		assert.Equal(t, expectedEpp, profile.GetPStates().GetEpp())
		assert.Equal(t, expectedGovernor, profile.GetPStates().GetGovernor())
		assert.Equal(t, expectedEpp, profile.GetPStates().GetEpp())
		assert.Equal(t, expectedCStates, profile.GetCStates().States())
	}

	type testCase struct {
		name                     string
		profileName              string
		otherProfileName         string
		setupSharedPoolProfile   string // "" means no profile set on shared pool
		setupReservedPoolProfile string // "" means no profile set on reserved pool
		expectSharedUpdate       bool   // whether shared pool should be updated
		expectReservedUpdate     bool   // whether reserved pool should be updated
	}

	testCases := []testCase{
		{
			name:                     "Update profile - shared pool uses profile",
			profileName:              "test-profile",
			setupSharedPoolProfile:   "test-profile",
			setupReservedPoolProfile: "",
			expectSharedUpdate:       true,
			expectReservedUpdate:     false,
		},
		{
			name:                     "Update profile - reserved pool uses profile",
			profileName:              "test-profile",
			setupSharedPoolProfile:   "",
			setupReservedPoolProfile: "test-profile",
			expectSharedUpdate:       false,
			expectReservedUpdate:     true,
		},
		{
			name:                     "Update profile - shared & reserved pools use different profile",
			profileName:              "test-profile",
			otherProfileName:         "other-profile",
			setupSharedPoolProfile:   "other-profile",
			setupReservedPoolProfile: "other-profile",
			expectSharedUpdate:       false,
			expectReservedUpdate:     false,
		},
		{
			name:                     "Create profile - no pools affected",
			profileName:              "test-profile",
			setupSharedPoolProfile:   "",
			setupReservedPoolProfile: "",
			expectSharedUpdate:       false,
			expectReservedUpdate:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nodeName := "TestNode"

			// Updated profile
			clientObjs := []runtime.Object{
				&powerv1alpha1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tc.profileName,
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerProfileSpec{
						PStates: powerv1alpha1.PStatesConfig{
							Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
							Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
							Epp:      "performance",
							Governor: "powersave",
						},
						CStates: powerv1alpha1.CStatesConfig{
							Names: map[string]bool{
								"C0":  true,
								"C1":  true,
								"C1E": false,
								"C3":  false,
							},
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
			}

			t.Setenv("NODE_NAME", nodeName)
			r, err := createProfileReconcilerObject(clientObjs)
			assert.Nil(t, err)

			// Use fullDummySystem for a real power library instance
			host, teardown, err := fullDummySystem()
			assert.Nil(t, err)
			defer teardown()

			var sharedPool power.Pool
			var reservedPool power.Pool
			var reservedPoolName string

			// Setup exclusive pool
			_, err = host.AddExclusivePool(tc.profileName)
			assert.Nil(t, err)

			// Setup initial shared pool profile if specified
			sharedPool = host.GetSharedPool()
			if tc.setupSharedPoolProfile != "" {
				profileName := tc.profileName
				if tc.setupSharedPoolProfile != tc.profileName {
					profileName = tc.otherProfileName
				}
				initialProfile, err := power.NewPowerProfile(
					profileName, &intstr.IntOrString{Type: intstr.Int, IntVal: 2000}, &intstr.IntOrString{Type: intstr.Int, IntVal: 3000}, "powersave", "power",
					map[string]bool{"C0": true, "C1": true, "C1E": false, "C3": true}, nil)
				assert.Nil(t, err)
				err = sharedPool.SetPowerProfile(initialProfile)
				assert.Nil(t, err)
			}

			// Setup initial reserved pool profile if specified
			if tc.setupReservedPoolProfile != "" {
				reservedPoolName = nodeName + "-reserved-[0,1,2,3]"
				reservedPool, err = host.AddExclusivePool(reservedPoolName)
				assert.Nil(t, err)
				initialProfile, err := power.NewPowerProfile(
					tc.setupReservedPoolProfile, &intstr.IntOrString{Type: intstr.Int, IntVal: 2000}, &intstr.IntOrString{Type: intstr.Int, IntVal: 3000}, "powersave", "balance_performance",
					map[string]bool{"C0": true, "C1": false, "C1E": false, "C3": false}, nil)
				assert.Nil(t, err)
				err = reservedPool.SetPowerProfile(initialProfile)
				assert.Nil(t, err)
			}

			r.PowerLibrary = host

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      tc.profileName,
					Namespace: PowerNamespace,
				},
			}

			// Execute the reconciliation
			_, err = r.Reconcile(context.TODO(), req)
			assert.Nil(t, err)

			// Verify expectations
			exclusivePool := host.GetExclusivePool(tc.profileName)
			assert.NotNil(t, exclusivePool, "Exclusive pool should be created/updated")
			currentProfile := exclusivePool.GetPowerProfile()
			assert.NotNil(t, currentProfile, "Exclusive pool should have a profile")
			verifyProfileParameters(currentProfile, tc.profileName, 3600, 3200,
				"powersave", "performance", map[string]bool{"C0": true, "C1": true, "C1E": false, "C3": false})

			if tc.expectSharedUpdate {
				currentProfile := sharedPool.GetPowerProfile()
				assert.NotNil(t, currentProfile, "Shared pool should have the updated profile")
				verifyProfileParameters(currentProfile, tc.profileName, 3600, 3200,
					"powersave", "performance", map[string]bool{"C0": true, "C1": true, "C1E": false, "C3": false})
			} else if tc.setupSharedPoolProfile == "" {
				currentProfile := sharedPool.GetPowerProfile()
				assert.Nil(t, currentProfile, "Shared pool should not have a profile")
			} else if tc.setupSharedPoolProfile == tc.otherProfileName {
				currentProfile := sharedPool.GetPowerProfile()
				assert.NotNil(t, currentProfile, "Shared pool should have the original profile")
				verifyProfileParameters(currentProfile, tc.otherProfileName, 3000, 2000,
					"powersave", "power", map[string]bool{"C0": true, "C1": true, "C1E": false, "C3": true})
			}

			if tc.expectReservedUpdate {
				assert.NotNil(t, reservedPool, "Reserved pool should exist")
				currentProfile := reservedPool.GetPowerProfile()
				assert.NotNil(t, currentProfile, "Reserved pool should have the updated profile")
				verifyProfileParameters(currentProfile, tc.profileName, 3600, 3200,
					"powersave", "performance", map[string]bool{"C0": true, "C1": true, "C1E": false, "C3": false})
			} else if tc.setupReservedPoolProfile != "" {
				currentProfile := reservedPool.GetPowerProfile()
				assert.NotNil(t, currentProfile, "Reserved pool should have the original profile")
				verifyProfileParameters(currentProfile, tc.setupReservedPoolProfile, 3000, 2000,
					"powersave", "balance_performance", map[string]bool{"C0": true, "C1": false, "C1E": false, "C3": false})
			}
		})
	}
}

func TestPowerProfile_Reconcile_NodeSelectorAndCapacity(t *testing.T) {
	testCases := []struct {
		name                    string
		profileName             string
		nodeSelector            *powerv1alpha1.NodeSelector
		cpuCapacity             string
		nodeLabels              map[string]string
		totalCPUs               int
		expectedResourcePercent float64
		expectMatch             bool
		expectExtendedResource  bool
	}{
		// NodeSelector scenarios
		{
			name:                    "No selector - applies to all nodes",
			profileName:             "no-selector-profile",
			nodeSelector:            nil,
			cpuCapacity:             "100%",
			nodeLabels:              map[string]string{"node-type": "worker"},
			totalCPUs:               86, // Power library has 86 CPUs
			expectedResourcePercent: 1.0,
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:        "Empty selector - applies to all nodes",
			profileName: "empty-selector-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{},
			},
			cpuCapacity:             "100%",
			nodeLabels:              map[string]string{"node-type": "worker"},
			totalCPUs:               86, // Power library has 86 CPUs
			expectedResourcePercent: 1.0,
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:        "Matching MatchLabels - should apply",
			profileName: "matching-labels-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "worker",
						"zone":      "us-west-1a",
					},
				},
			},
			cpuCapacity: "50%",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
				"extra":     "label",
			},
			totalCPUs:               86, // Power library has 86 CPUs
			expectedResourcePercent: 0.5,
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:        "Non-matching MatchLabels - should not apply",
			profileName: "non-matching-labels-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "master",
						"zone":      "us-west-1a",
					},
				},
			},
			cpuCapacity: "25%",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			totalCPUs:               86, // Power library has 86 CPUs
			expectedResourcePercent: 0.0,
			expectMatch:             false,
			expectExtendedResource:  false,
		},
		{
			name:        "Matching MatchExpressions - should apply",
			profileName: "matching-expressions-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "node-type",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"worker", "compute"},
						},
						{
							Key:      "zone",
							Operator: metav1.LabelSelectorOpExists,
						},
					},
				},
			},
			cpuCapacity: "75%",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			totalCPUs:               86,          // Power library has 86 CPUs
			expectedResourcePercent: 64.0 / 86.0, // 75% of 86 = 64.5 -> 64
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:        "Non-matching MatchExpressions - should not apply",
			profileName: "non-matching-expressions-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "node-type",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"master", "etcd"},
						},
					},
				},
			},
			cpuCapacity: "30%",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			totalCPUs:               86, // Power library has 86 CPUs
			expectedResourcePercent: 0.0,
			expectMatch:             false,
			expectExtendedResource:  false,
		},
		{
			name:        "Missing required label - should not apply",
			profileName: "missing-label-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"required-label": "value",
					},
				},
			},
			cpuCapacity: "40%",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			totalCPUs:               86, // Power library has 86 CPUs
			expectedResourcePercent: 0.0,
			expectMatch:             false,
			expectExtendedResource:  false,
		},

		// CpuCapacity scenarios
		{
			name:                    "Absolute CPU count",
			profileName:             "absolute-cpu-profile",
			nodeSelector:            nil,
			cpuCapacity:             "10",
			nodeLabels:              map[string]string{"node-type": "worker"},
			totalCPUs:               86,          // Power library has 86 CPUs
			expectedResourcePercent: 10.0 / 86.0, // 10 CPUs out of 86
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:                    "Percentage CPU capacity",
			profileName:             "percentage-cpu-profile",
			nodeSelector:            nil,
			cpuCapacity:             "25%",
			nodeLabels:              map[string]string{"node-type": "worker"},
			totalCPUs:               86,          // Power library has 86 CPUs
			expectedResourcePercent: 21.0 / 86.0, // 25% of 86 = 21.5 -> 21
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:                    "Default capacity (empty string)",
			profileName:             "default-capacity-profile",
			nodeSelector:            nil,
			cpuCapacity:             "",
			nodeLabels:              map[string]string{"node-type": "worker"},
			totalCPUs:               86,  // Power library has 86 CPUs
			expectedResourcePercent: 1.0, // Should default to 100%
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:        "High percentage capacity",
			profileName: "high-percentage-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"high-performance": "true",
					},
				},
			},
			cpuCapacity: "90%",
			nodeLabels: map[string]string{
				"node-type":        "worker",
				"high-performance": "true",
			},
			totalCPUs:               86,          // Power library has 86 CPUs
			expectedResourcePercent: 77.0 / 86.0, // 90% of 86 = 77.4 -> 77
			expectMatch:             true,
			expectExtendedResource:  true,
		},

		// Integration scenarios
		{
			name:        "Complex selector with custom capacity",
			profileName: "complex-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "worker",
					},
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "zone",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"us-west-1a", "us-west-1b"},
						},
						{
							Key:      "instance-type",
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{"micro", "nano"},
						},
					},
				},
			},
			cpuCapacity: "15",
			nodeLabels: map[string]string{
				"node-type":     "worker",
				"zone":          "us-west-1a",
				"instance-type": "large",
			},
			totalCPUs:               86, // Power library has 86 CPUs
			expectedResourcePercent: 15.0 / 86.0,
			expectMatch:             true,
			expectExtendedResource:  true,
		},
		{
			name:        "Shared profile with selector",
			profileName: "shared-selector-profile",
			nodeSelector: &powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"shared-eligible": "true",
					},
				},
			},
			cpuCapacity: "60%",
			nodeLabels: map[string]string{
				"node-type":       "worker",
				"shared-eligible": "true",
			},
			totalCPUs:               86,          // Power library has 86 CPUs
			expectedResourcePercent: 51.0 / 86.0, // 60% of 86 = 51.6 -> 51
			expectMatch:             true,
			expectExtendedResource:  false, // Shared profiles don't create extended resources
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nodeName := "TestNode"
			t.Setenv("NODE_NAME", nodeName)

			// Create node with specified labels
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   nodeName,
					Labels: tc.nodeLabels,
				},
				Status: corev1.NodeStatus{
					Capacity: map[corev1.ResourceName]resource.Quantity{
						CPUResource: *resource.NewQuantity(int64(tc.totalCPUs), resource.DecimalSI),
					},
				},
			}

			// Create PowerProfile
			profile := &powerv1alpha1.PowerProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.profileName,
					Namespace: PowerNamespace,
				},
				Spec: powerv1alpha1.PowerProfileSpec{
					PStates: powerv1alpha1.PStatesConfig{
						Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
						Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
						Epp:      "performance",
						Governor: "powersave",
					},
				},
			}

			// Set nodeSelector if provided
			if tc.nodeSelector != nil {
				profile.Spec.NodeSelector = *tc.nodeSelector
			}

			// Set cpuCapacity if provided
			if tc.cpuCapacity != "" {
				if strings.HasSuffix(tc.cpuCapacity, "%") {
					profile.Spec.CpuCapacity = intstr.FromString(tc.cpuCapacity)
				} else {
					// Parse as absolute integer value
					cpuCount, err := strconv.Atoi(tc.cpuCapacity)
					if err == nil {
						profile.Spec.CpuCapacity = intstr.FromInt32(int32(cpuCount))
					} else {
						profile.Spec.CpuCapacity = intstr.FromString(tc.cpuCapacity)
					}
				}
			}

			// Check if this is a shared profile based on the name
			if strings.Contains(tc.profileName, "shared") {
				profile.Spec.Shared = true
			}

			clientObjs := []runtime.Object{node, profile}

			r, err := createProfileReconcilerObject(clientObjs)
			assert.NoError(t, err)

			host, teardown, err := fullDummySystem()
			assert.Nil(t, err)
			defer teardown()
			r.PowerLibrary = host

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      tc.profileName,
					Namespace: PowerNamespace,
				},
			}

			// Execute reconciliation
			result, err := r.Reconcile(context.TODO(), req)
			assert.NoError(t, err)
			assert.Equal(t, reconcile.Result{}, result)

			// Verify nodeMatchesPowerProfile behavior
			match, err := nodeMatchesPowerProfile(context.TODO(), r.Client, profile, nodeName, &r.Log)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectMatch, match, "nodeMatchesPowerProfile result mismatch")

			// Verify extended resource creation/absence
			updatedNode := &corev1.Node{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{Name: nodeName}, updatedNode)
			assert.NoError(t, err)

			extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, tc.profileName))
			_, hasExtendedResource := updatedNode.Status.Capacity[extendedResourceName]

			if tc.expectExtendedResource {
				assert.True(t, hasExtendedResource, "Extended resource should be created")
				if hasExtendedResource {
					expectedQuantity := int64(float64(tc.totalCPUs) * tc.expectedResourcePercent)
					actualQuantity := updatedNode.Status.Capacity[extendedResourceName]
					assert.Equal(t, expectedQuantity, actualQuantity.Value(), "Extended resource quantity mismatch")
				}
			} else {
				assert.False(t, hasExtendedResource, "Extended resource should not be created")
			}

			// Verify exclusive pool creation
			exclusivePool := host.GetExclusivePool(tc.profileName)
			if tc.expectMatch {
				assert.NotNil(t, exclusivePool, "Exclusive pool should be created")
				assert.NotNil(t, exclusivePool.GetPowerProfile(), "Exclusive pool should have a power profile")
				assert.Equal(t, tc.profileName, exclusivePool.GetPowerProfile().Name(), "Exclusive pool should have the correct profile name")
			}
		})
	}
}

// Test cleanup behavior when node labels change
func TestPowerProfile_Reconcile_NodeSelectorCleanup(t *testing.T) {
	nodeName := "TestNode"
	profileName := "cleanup-test-profile"
	t.Setenv("NODE_NAME", nodeName)

	// Initial node with matching labels
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
		},
		Status: corev1.NodeStatus{
			Capacity: map[corev1.ResourceName]resource.Quantity{
				CPUResource: *resource.NewQuantity(86, resource.DecimalSI), // Power library has 86 CPUs
			},
		},
	}

	profile := &powerv1alpha1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      profileName,
			Namespace: PowerNamespace,
		},
		Spec: powerv1alpha1.PowerProfileSpec{
			NodeSelector: powerv1alpha1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "worker",
						"zone":      "us-west-1a",
					},
				},
			},
			CpuCapacity: intstr.FromString("50%"),
			PStates: powerv1alpha1.PStatesConfig{
				Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
				Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
				Epp:      "performance",
				Governor: "powersave",
			},
		},
	}

	clientObjs := []runtime.Object{node, profile}
	r, err := createProfileReconcilerObject(clientObjs)
	assert.NoError(t, err)

	host, teardown, err := fullDummySystem()
	assert.NoError(t, err)
	defer teardown()
	r.PowerLibrary = host

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      profileName,
			Namespace: PowerNamespace,
		},
	}

	// First reconciliation - should create resources
	_, err = r.Reconcile(context.TODO(), req)
	assert.NoError(t, err)

	// Verify resources were created
	updatedNode := &corev1.Node{}
	err = r.Client.Get(context.TODO(), client.ObjectKey{Name: nodeName}, updatedNode)
	assert.NoError(t, err)

	extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, profileName))
	_, hasExtendedResource := updatedNode.Status.Capacity[extendedResourceName]
	assert.True(t, hasExtendedResource, "Extended resource should be created initially")

	// Change node labels so they no longer match
	updatedNode.Labels = map[string]string{
		"node-type": "master", // Changed from worker
		"zone":      "us-west-1a",
	}
	err = r.Client.Update(context.TODO(), updatedNode)
	assert.NoError(t, err)

	// Second reconciliation - should clean up resources
	_, err = r.Reconcile(context.TODO(), req)
	assert.NoError(t, err)

	// Verify extended resource was removed
	finalNode := &corev1.Node{}
	err = r.Client.Get(context.TODO(), client.ObjectKey{Name: nodeName}, finalNode)
	assert.NoError(t, err)

	_, hasExtendedResource = finalNode.Status.Capacity[extendedResourceName]
	assert.False(t, hasExtendedResource, "Extended resource should be removed after cleanup")

	// Verify exclusive pool still exists (it's not cleaned up for safety)
	exclusivePool := host.GetExclusivePool(profileName)
	assert.NotNil(t, exclusivePool, "Exclusive pool should still exist after cleanup")
}
