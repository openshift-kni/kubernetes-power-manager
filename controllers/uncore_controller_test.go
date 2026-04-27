package controllers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// createUncoreReconciler builds a reconciler with a fake client and the given objects.
func createUncoreReconciler(objs []runtime.Object, powerLib power.Host) *UncoreReconciler {
	log.SetLogger(zap.New(
		zap.UseDevMode(true),
		func(opts *zap.Options) { opts.TimeEncoder = zapcore.ISO8601TimeEncoder },
	))
	s := scheme.Scheme
	_ = powerv1.AddToScheme(s)
	cl := fake.NewClientBuilder().WithRuntimeObjects(objs...).WithScheme(s).WithStatusSubresource(&powerv1.PowerNodeState{}).Build()
	return &UncoreReconciler{
		Client:       cl,
		Log:          ctrl.Log.WithName("testing"),
		Scheme:       s,
		PowerLibrary: powerLib,
	}
}

// newUncore creates an Uncore CR for testing.
func newUncore(name string, spec powerv1.UncoreSpec, nodeSelectorMatchLabels map[string]string, creationTime time.Time) *powerv1.Uncore {
	u := &powerv1.Uncore{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         PowerNamespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: spec,
	}
	if nodeSelectorMatchLabels != nil {
		u.Spec.NodeSelector = powerv1.NodeSelector{
			LabelSelector: metav1.LabelSelector{MatchLabels: nodeSelectorMatchLabels},
		}
	}
	return u
}

// newUncorePowerNodeState creates a PowerNodeState with optional uncore status.
func newUncorePowerNodeState(nodeName, activeUncore string) *powerv1.PowerNodeState {
	pns := &powerv1.PowerNodeState{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName + "-power-state", Namespace: PowerNamespace},
	}
	if activeUncore != "" {
		pns.Status = powerv1.PowerNodeStateStatus{
			Uncore: &powerv1.NodeUncoreStatus{
				Name:   activeUncore,
				Config: "SysMin: 1200000, SysMax: 2400000",
			},
		}
	}
	return pns
}

// --- selectUncore ---

func TestSelectActiveOrOldest_Uncore(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)
	sysMax := uint(2400000)
	sysMin := uint(1200000)
	spec := powerv1.UncoreSpec{SysMax: &sysMax, SysMin: &sysMin}

	tcases := []struct {
		name             string
		matches          []powerv1.Uncore
		activeUncoreName string
		expectedName     string
		expectNil        bool
	}{
		{
			name:      "no matches returns nil",
			matches:   nil,
			expectNil: true,
		},
		{
			name:         "single match, no active",
			matches:      []powerv1.Uncore{*newUncore("uncore-a", spec, nil, now)},
			expectedName: "uncore-a",
		},
		{
			name:             "single match, is active",
			matches:          []powerv1.Uncore{*newUncore("uncore-a", spec, nil, now)},
			activeUncoreName: "uncore-a",
			expectedName:     "uncore-a",
		},
		{
			name:             "single match, different active",
			matches:          []powerv1.Uncore{*newUncore("uncore-a", spec, nil, now)},
			activeUncoreName: "uncore-b",
			expectedName:     "uncore-a",
		},
		{
			name: "two matches, active still matches — sticky",
			matches: []powerv1.Uncore{
				*newUncore("uncore-a", spec, nil, older),
				*newUncore("uncore-b", spec, nil, now),
			},
			activeUncoreName: "uncore-b",
			expectedName:     "uncore-b",
		},
		{
			name: "two matches, active gone — oldest wins",
			matches: []powerv1.Uncore{
				*newUncore("uncore-b", spec, nil, now),
				*newUncore("uncore-a", spec, nil, older),
			},
			activeUncoreName: "uncore-gone",
			expectedName:     "uncore-a",
		},
		{
			name: "two matches same time — name tiebreaker",
			matches: []powerv1.Uncore{
				*newUncore("uncore-b", spec, nil, now),
				*newUncore("uncore-a", spec, nil, now),
			},
			expectedName: "uncore-a",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			logger := testLogger()
			result := selectActiveOrOldest(tc.matches, tc.activeUncoreName, uncoreMeta, &logger)
			if tc.expectNil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, tc.expectedName, result.Name)
			}
		})
	}
}

// tests invalid uncore spec fields
func TestUncore_Reconcile_InvalidSpecs(t *testing.T) {
	nodeName := "TestNode"
	t.Setenv("NODE_NAME", nodeName)

	host, teardown, err := fullDummySystem()
	assert.Nil(t, err)
	defer teardown()

	max := uint(2400000)
	min := uint(1200000)
	pkg := uint(0)
	die := uint(0)

	tcases := []struct {
		name            string
		spec            powerv1.UncoreSpec
		errContains string
	}{
		{
			name:        "empty spec",
			spec:        powerv1.UncoreSpec{},
			errContains: "no valid uncore configuration: requires either both sysMin and sysMax, or non-empty dieSelectors",
		},
		{
			name:        "missing min in die selector",
			spec:        powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{{Package: &pkg, Max: &max}}},
			errContains: "max, min and package fields must not be empty",
		},
		{
			name:        "missing max in die selector",
			spec:        powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{{Package: &pkg, Die: &die, Min: &min}}},
			errContains: "max, min and package fields must not be empty",
		},
		{
			name:        "missing package in die selector",
			spec:        powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{{Die: &die, Max: &max, Min: &min}}},
			errContains: "max, min and package fields must not be empty",
		},
		{
			name: "invalid package ID",
			spec: powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{
				{Package: uintPtr(100000), Die: &die, Max: &max, Min: &min},
			}},
			errContains: "invalid package",
		},
		{
			name: "invalid package ID (package-level tuning)",
			spec: powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{
				{Package: uintPtr(100000), Max: &max, Min: &min},
			}},
			errContains: "invalid package",
		},
		{
			name: "invalid die ID",
			spec: powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{
				{Package: &pkg, Die: uintPtr(100000), Max: &max, Min: &min},
			}},
			errContains: "invalid die",
		},
		{
			name:        "frequency exceeds hardware limits (system-wide)",
			spec:        powerv1.UncoreSpec{SysMax: uintPtr(20000000000), SysMin: &min},
			errContains: "specified Max frequency is higher than",
		},
		{
			name: "frequency exceeds hardware limits (package-level)",
			spec: powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{
				{Package: &pkg, Max: uintPtr(20000000000), Min: &min},
			}},
			errContains: "specified Max frequency is higher than",
		},
		{
			name: "frequency exceeds hardware limits (die-level)",
			spec: powerv1.UncoreSpec{DieSelectors: &[]powerv1.DieSelector{
				{Package: &pkg, Die: &die, Max: uintPtr(20000000000), Min: &min},
			}},
			errContains: "specified Max frequency is higher than",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{
				newUncore("invalid-uncore", tc.spec, nil, time.Now()),
				newTestNode(nodeName, nil),
				newUncorePowerNodeState(nodeName, ""),
			}
			r := createUncoreReconciler(objs, host)

			req := reconcile.Request{NamespacedName: client.ObjectKey{Name: "invalid-uncore", Namespace: PowerNamespace}}
			_, err := r.Reconcile(context.TODO(), req)
			assert.ErrorContains(t, err, tc.errContains)

			// Verify error was recorded in PowerNodeState.
			pns := &powerv1.PowerNodeState{}
			assert.Nil(t, r.Get(context.TODO(), client.ObjectKey{Name: nodeName + "-power-state", Namespace: PowerNamespace}, pns))
			assert.NotNil(t, pns.Status.Uncore)
			assert.NotEmpty(t, pns.Status.Uncore.Errors)
		})
	}
}

// tests requests for the wrong namespace
func TestUncore_Reconcile_InvalidNamespace(t *testing.T) {
	nodeName := "TestNode"
	t.Setenv("NODE_NAME", nodeName)

	r := createUncoreReconciler([]runtime.Object{}, nil)
	req := reconcile.Request{NamespacedName: client.ObjectKey{Name: "some-uncore", Namespace: "somespace"}}
	_, err := r.Reconcile(context.TODO(), req)
	assert.Nil(t, err) // just ignored, not an error
}

// tests for a file system with missing files
func TestUncore_Reconcile_InvalidFileSystem(t *testing.T) {
	nodeName := "TestNode"
	t.Setenv("NODE_NAME", nodeName)
	max := uint(2400000)
	min := uint(1200000)
	pkg := uint(0)
	die := uint(1)

	host, teardown, err := fullDummySystem()
	assert.Nil(t, err)
	defer teardown()

	objs := []runtime.Object{
		newUncore("fs-uncore", powerv1.UncoreSpec{
			DieSelectors: &[]powerv1.DieSelector{
				{Package: &pkg, Die: &die, Max: &max, Min: &min},
			},
		}, nil, time.Now()),
		newTestNode(nodeName, nil),
		newUncorePowerNodeState(nodeName, ""),
	}
	r := createUncoreReconciler(objs, host)

	err = os.RemoveAll("./testing")
	assert.Nil(t, err)
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "fs-uncore", Namespace: PowerNamespace}})
	assert.ErrorContains(t, err, "no such file or directory")
}

// tests positive and negative cases for SetupWithManager function
func TestUncore_SetupPass(t *testing.T) {
	r := createUncoreReconciler([]runtime.Object{}, nil)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("SetFields", mock.Anything).Return(nil)
	mgr.On("Add", mock.Anything).Return(nil)
	mgr.On("GetCache").Return(new(cacheMk))
	err := (&UncoreReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Nil(t, err)
}

func TestUncore_SetupFail(t *testing.T) {
	r := createUncoreReconciler([]runtime.Object{}, nil)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("Add", mock.Anything).Return(fmt.Errorf("setup fail"))

	err := (&UncoreReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Error(t, err)
}

// tests NODE_NAME not set
func TestUncore_Reconcile_NoNodeName(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	r := createUncoreReconciler([]runtime.Object{}, nil)
	req := reconcile.Request{NamespacedName: client.ObjectKey{Name: "some-uncore", Namespace: PowerNamespace}}
	_, err := r.Reconcile(context.TODO(), req)
	assert.ErrorContains(t, err, "NODE_NAME environment variable not set")
}

// --- helpers ---

func uintPtr(v uint) *uint { return &v }

// checkUncoreValues validates uncore values written to the filesystem.
func checkUncoreValues(basepath string, pkg string, die string, max string, min string) error {
	realMax, err := os.ReadFile(fmt.Sprintf("%s/intel_uncore_frequency/package_%s_die_%s/max_freq_khz", basepath, pkg, die))
	if err != nil {
		return err
	}
	realMin, err := os.ReadFile(fmt.Sprintf("%s/intel_uncore_frequency/package_%s_die_%s/min_freq_khz", basepath, pkg, die))
	if err != nil {
		return err
	}
	if max != string(realMax) || min != string(realMin) {
		return fmt.Errorf("min/max values in filesystem are unexpected: got max=%s min=%s, want max=%s min=%s", string(realMax), string(realMin), max, min)
	}
	return nil
}
