package controllers

import (
	"context"
	"fmt"
	"testing"

	powerv1alpha1 "github.com/openshift-kni/kubernetes-power-manager/api/v1alpha1"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap/zapcore"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func createConfigReconcilerObject(objs []client.Object) (*PowerConfigReconciler, error) {
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
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&powerv1alpha1.PowerConfig{}).Build()

	state := state.NewPowerNodeData()

	r := &PowerConfigReconciler{cl, ctrl.Log.WithName("testing"), s, state}

	return r, nil
}

func TestPowerConfig_Reconcile_Creation(t *testing.T) {
	tcases := []struct {
		testCase   string
		nodeName   string
		configName string
		clientObjs []client.Object
	}{
		{
			testCase:   "basic config creation",
			nodeName:   "TestNode",
			configName: "test-config",
			clientObjs: []client.Object{
				&powerv1alpha1.PowerConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-config",
						Namespace: PowerNamespace,
					},
					Spec: powerv1alpha1.PowerConfigSpec{
						PowerNodeSelector: map[string]string{
							"feature.node.kubernetes.io/power-node": "true",
						},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
						Labels: map[string]string{
							"feature.node.kubernetes.io/power-node": "true",
						},
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
		NodeAgentDaemonSetPath = "../build/manifests/power-node-agent-ds.yaml"

		r, err := createConfigReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating reconciler object", tc.testCase)
		}

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.configName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error reconciling object", tc.testCase)
		}

		ds := &appsv1.DaemonSet{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name:      NodeAgentDSName,
			Namespace: PowerNamespace,
		}, ds)
		if err != nil {
			t.Errorf("%s failed: expected daemonSet '%s' to have been created", tc.testCase, NodeAgentDSName)
		}

		powerNodeState := &powerv1alpha1.PowerNodeState{}
		powerNodeStateName := fmt.Sprintf("%s-power-state", tc.nodeName)
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name:      powerNodeStateName,
			Namespace: PowerNamespace,
		}, powerNodeState)
		if err != nil {
			t.Errorf("%s failed: expected PowerNodeState '%s' to have been created", tc.testCase, powerNodeStateName)
		}
	}
}

func TestPowerConfig_Reconcile_Deletion(t *testing.T) {
	tcases := []struct {
		testCase                string
		nodeName                string
		configName              string
		clientObjs              []client.Object
		expectedNumberOfObjects int
	}{
		{
			testCase:   "Test Case 1",
			nodeName:   "TestNode",
			configName: "test-config",
			clientObjs: []client.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
						Labels: map[string]string{
							"feature.node.kubernetes.io/power-node": "true",
						},
					},
					Status: corev1.NodeStatus{
						Capacity: map[corev1.ResourceName]resource.Quantity{
							CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
						},
					},
				},
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
				&powerv1alpha1.PowerNodeState{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "TestNode-power-state",
						Namespace: PowerNamespace,
					},
				},
				&appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      NodeAgentDSName,
						Namespace: PowerNamespace,
					},
				},
			},
			expectedNumberOfObjects: 0,
		},
	}
	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)
		NodeAgentDaemonSetPath = "../build/manifests/power-node-agent-ds.yaml"

		r, err := createConfigReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.configName,
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error reconciling object", tc.testCase)
		}

		profiles := &powerv1alpha1.PowerProfileList{}
		err = r.Client.List(context.TODO(), profiles)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error retrieving the power profile objects", tc.testCase)
		}

		if len(profiles.Items) != tc.expectedNumberOfObjects {
			t.Errorf("%s failed: expected number of power profile objects is %v, got %v", tc.testCase, tc.expectedNumberOfObjects, len(profiles.Items))
		}

		powerNodeStates := &powerv1alpha1.PowerNodeStateList{}
		err = r.Client.List(context.TODO(), powerNodeStates)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error retrieving PowerNodeState objects", tc.testCase)
		}

		if len(powerNodeStates.Items) != tc.expectedNumberOfObjects {
			t.Errorf("%s failed: expected number of PowerNodeState objects is %v, got %v", tc.testCase, tc.expectedNumberOfObjects, len(powerNodeStates.Items))
		}

		ds := &appsv1.DaemonSet{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name:      NodeAgentDSName,
			Namespace: PowerNamespace,
		}, ds)
		if err == nil {
			t.Errorf("%s failed: expected daemonSet '%s' to have been deleted", tc.testCase, NodeAgentDSName)
		}
	}
}

// go test -fuzz FuzzPowerConfigController -run=FuzzPowerConfigController
func FuzzPowerConfigController(f *testing.F) {
	f.Add("sample-config", "feature.node.kubernetes.io/power-node")
	f.Fuzz(func(t *testing.T, name string, label string) {
		clientObjs := []client.Object{
			&powerv1alpha1.PowerConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: PowerNamespace,
				},
				Spec: powerv1alpha1.PowerConfigSpec{
					PowerNodeSelector: map[string]string{
						label: "true",
					},
				},
			},
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "TestNode",
					Labels: map[string]string{
						label: "true",
					},
				},
				Status: corev1.NodeStatus{
					Capacity: map[corev1.ResourceName]resource.Quantity{
						CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
					},
				},
			},
		}
		NodeAgentDaemonSetPath = "../build/manifests/power-node-agent-ds.yaml"
		r, err := createConfigReconcilerObject(clientObjs)
		assert.Nil(t, err)
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      name,
				Namespace: PowerNamespace,
			},
		}

		r.Reconcile(context.TODO(), req)

	})
}

// tests positive and negative cases for SetupWithManager function
func TestPowerConfig_Reconcile_SetupPass(t *testing.T) {
	r, err := createConfigReconcilerObject([]client.Object{})
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("SetFields", mock.Anything).Return(nil)
	mgr.On("Add", mock.Anything).Return(nil)
	mgr.On("GetCache").Return(new(cacheMk))
	err = (&PowerConfigReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Nil(t, err)

}
func TestPowerConfig_Reconcile_SetupFail(t *testing.T) {
	r, err := createConfigReconcilerObject([]client.Object{})
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("Add", mock.Anything).Return(fmt.Errorf("setup fail"))

	err = (&PowerConfigReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Error(t, err)

}
