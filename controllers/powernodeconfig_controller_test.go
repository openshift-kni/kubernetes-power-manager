package controllers

import (
	"context"
	"testing"
	"time"

	powerv1alpha1 "github.com/cluster-power-manager/cluster-power-manager/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// createPowerNodeConfigReconciler builds a reconciler with a fake client and the given objects.
func createPowerNodeConfigReconciler(objs []runtime.Object, powerLib power.Host) *PowerNodeConfigReconciler {
	log.SetLogger(zap.New(
		zap.UseDevMode(true),
		func(opts *zap.Options) { opts.TimeEncoder = zapcore.ISO8601TimeEncoder },
	))
	s := scheme.Scheme
	_ = powerv1alpha1.AddToScheme(s)
	cl := fake.NewClientBuilder().WithRuntimeObjects(objs...).WithScheme(s).WithStatusSubresource(&powerv1alpha1.PowerNodeState{}).Build()
	return &PowerNodeConfigReconciler{
		Client:       cl,
		Log:          ctrl.Log.WithName("testing"),
		Scheme:       s,
		PowerLibrary: powerLib,
	}
}

// newPowerNodeConfig creates a PowerNodeConfig for testing.
func newPowerNodeConfig(name, profile string, nodeSelectorMatchLabels map[string]string, reserved []powerv1alpha1.ReservedSpec, creationTime time.Time) *powerv1alpha1.PowerNodeConfig {
	pnc := &powerv1alpha1.PowerNodeConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         PowerNamespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: powerv1alpha1.PowerNodeConfigSpec{
			SharedPowerProfile: profile,
			ReservedCPUs:       reserved,
		},
	}
	if nodeSelectorMatchLabels != nil {
		pnc.Spec.NodeSelector = powerv1alpha1.NodeSelector{
			LabelSelector: metav1.LabelSelector{MatchLabels: nodeSelectorMatchLabels},
		}
	}
	return pnc
}

// newTestNode creates a Node for testing.
func newTestNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// newPowerNodeState creates a PowerNodeState with optional shared pool status.
func newPowerNodeState(nodeName, activeConfig string) *powerv1alpha1.PowerNodeState {
	pns := &powerv1alpha1.PowerNodeState{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName + "-power-state", Namespace: PowerNamespace},
	}
	if activeConfig != "" {
		pns.Status = powerv1alpha1.PowerNodeStateStatus{
			CPUPools: &powerv1alpha1.CPUPoolsStatus{
				Shared: &powerv1alpha1.SharedCPUPoolStatus{
					PowerNodeConfig: activeConfig,
					PowerProfile:    "test-profile",
					CPUIDs:          "2-3",
				},
			},
		}
	}
	return pns
}

func testLogger() logr.Logger {
	return ctrl.Log.WithName("test")
}

// --- selectActiveOrOldest (PowerNodeConfig) ---

func TestSelectActiveOrOldest_PowerNodeConfig(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)

	tcases := []struct {
		name             string
		matches          []powerv1alpha1.PowerNodeConfig
		activeConfigName string
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
			matches:      []powerv1alpha1.PowerNodeConfig{*newPowerNodeConfig("config-a", "p", nil, nil, now)},
			expectedName: "config-a",
		},
		{
			name:             "single match, is active",
			matches:          []powerv1alpha1.PowerNodeConfig{*newPowerNodeConfig("config-a", "p", nil, nil, now)},
			activeConfigName: "config-a",
			expectedName:     "config-a",
		},
		{
			name:             "single match, different active",
			matches:          []powerv1alpha1.PowerNodeConfig{*newPowerNodeConfig("config-b", "p", nil, nil, now)},
			activeConfigName: "config-a",
			expectedName:     "config-b",
		},
		{
			name: "multiple matches, active among them",
			matches: []powerv1alpha1.PowerNodeConfig{
				*newPowerNodeConfig("config-a", "p", nil, nil, now),
				*newPowerNodeConfig("config-b", "p", nil, nil, older),
			},
			activeConfigName: "config-a",
			expectedName:     "config-a",
		},
		{
			name: "multiple matches, none active, oldest wins",
			matches: []powerv1alpha1.PowerNodeConfig{
				*newPowerNodeConfig("config-b", "p", nil, nil, now),
				*newPowerNodeConfig("config-a", "p", nil, nil, older),
			},
			expectedName: "config-a",
		},
		{
			name: "multiple matches, same timestamp, name tiebreaker",
			matches: []powerv1alpha1.PowerNodeConfig{
				*newPowerNodeConfig("config-c", "p", nil, nil, now),
				*newPowerNodeConfig("config-a", "p", nil, nil, now),
				*newPowerNodeConfig("config-b", "p", nil, nil, now),
			},
			expectedName: "config-a",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			logger := testLogger()
			result := selectActiveOrOldest(tc.matches, tc.activeConfigName, powerNodeConfigMeta, &logger)
			if tc.expectNil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, tc.expectedName, result.Name)
			}
		})
	}
}

// --- getMatchingPowerNodeConfigs ---

func TestGetMatchingPowerNodeConfigs(t *testing.T) {
	tcases := []struct {
		name          string
		nodeName      string
		nodeLabels    map[string]string
		configs       []*powerv1alpha1.PowerNodeConfig
		expectedCount int
		expectErr     bool
	}{
		{
			name:          "empty selector matches all nodes",
			nodeName:      "test-node",
			nodeLabels:    map[string]string{"role": "worker"},
			configs:       []*powerv1alpha1.PowerNodeConfig{newPowerNodeConfig("config-a", "p", nil, nil, time.Now())},
			expectedCount: 1,
		},
		{
			name:          "label match",
			nodeName:      "test-node",
			nodeLabels:    map[string]string{"role": "worker"},
			configs:       []*powerv1alpha1.PowerNodeConfig{newPowerNodeConfig("config-a", "p", map[string]string{"role": "worker"}, nil, time.Now())},
			expectedCount: 1,
		},
		{
			name:          "label mismatch",
			nodeName:      "test-node",
			nodeLabels:    map[string]string{"role": "worker"},
			configs:       []*powerv1alpha1.PowerNodeConfig{newPowerNodeConfig("config-a", "p", map[string]string{"role": "master"}, nil, time.Now())},
			expectedCount: 0,
		},
		{
			name:          "no configs",
			nodeName:      "test-node",
			nodeLabels:    map[string]string{"role": "worker"},
			configs:       nil,
			expectedCount: 0,
		},
		{
			name:      "node not found",
			nodeName:  "missing-node",
			configs:   nil,
			expectErr: true,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.nodeLabels != nil {
				objs = append(objs, newTestNode(tc.nodeName, tc.nodeLabels))
			}
			for _, c := range tc.configs {
				objs = append(objs, c)
			}
			r := createPowerNodeConfigReconciler(objs, nil)

			matches, err := r.getMatchingPowerNodeConfigs(context.TODO(), tc.nodeName)
			if tc.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Len(t, matches, tc.expectedCount)
			}
		})
	}
}

// --- getActiveResourceName (PowerNodeConfig) ---

func TestGetActiveResourceName_PowerNodeConfig(t *testing.T) {
	tcases := []struct {
		name         string
		objs         []runtime.Object
		expectedName string
		expectErr    bool
	}{
		{
			name:         "active config present",
			objs:         []runtime.Object{newPowerNodeState("test-node", "config-a")},
			expectedName: "config-a",
		},
		{
			name: "no shared pool",
			objs: []runtime.Object{&powerv1alpha1.PowerNodeState{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node-power-state", Namespace: PowerNamespace},
			}},
			expectedName: "",
		},
		{
			name:      "PowerNodeState not found",
			objs:      nil,
			expectErr: true,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			r := createPowerNodeConfigReconciler(tc.objs, nil)
			name, err := getActiveResourceName(context.TODO(), r.Client, "test-node", powerNodeConfigActiveName)
			if tc.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedName, name)
			}
		})
	}
}

// --- validatePowerNodeConfigProfiles ---

func TestValidatePowerNodeConfigProfiles(t *testing.T) {
	tcases := []struct {
		name        string
		config      *powerv1alpha1.PowerNodeConfig
		clientObjs  []runtime.Object
		setupMock   func() *hostMock
		expectErr   bool
		errContains string
	}{
		{
			name:   "all profiles available",
			config: newPowerNodeConfig("c", "shared-prof", nil, []powerv1alpha1.ReservedSpec{{Cores: []uint{0}, PowerProfile: "reserved-prof"}}, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "shared-prof", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{Shared: true}},
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "reserved-prof", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{}},
			},
			setupMock: func() *hostMock {
				h := new(hostMock)
				h.On("GetExclusivePool", "shared-prof").Return(new(poolMock))
				h.On("GetExclusivePool", "reserved-prof").Return(new(poolMock))
				return h
			},
		},
		{
			name:   "shared profile not marked as shared",
			config: newPowerNodeConfig("c", "not-shared", nil, nil, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "not-shared", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{Shared: false}},
			},
			setupMock: func() *hostMock {
				return new(hostMock)
			},
			expectErr:   true,
			errContains: "is not a shared profile",
		},
		{
			name:   "shared profile unavailable",
			config: newPowerNodeConfig("c", "missing", nil, nil, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{Shared: true}},
			},
			setupMock: func() *hostMock {
				h := new(hostMock)
				h.On("GetExclusivePool", "missing").Return(nil)
				return h
			},
			expectErr:   true,
			errContains: "missing",
		},
		{
			// This should be blocked by the CRD validation, but adding it here in case the fakeclient doesn't enforce it.
			name: "duplicate core within same entry",
			config: newPowerNodeConfig("c", "shared-prof", nil, []powerv1alpha1.ReservedSpec{
				{Cores: []uint{0, 1, 0}, PowerProfile: "perf"},
			}, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "shared-prof", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{Shared: true}},
			},
			setupMock: func() *hostMock {
				return new(hostMock)
			},
			expectErr:   true,
			errContains: "reserved CPU 0 is listed in multiple reservedCPUs entries",
		},
		{
			name: "overlapping reserved CPUs across entries",
			config: newPowerNodeConfig("c", "shared-prof", nil, []powerv1alpha1.ReservedSpec{
				{Cores: []uint{0, 1}, PowerProfile: "perf"},
				{Cores: []uint{1, 2}, PowerProfile: "balanced"},
			}, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "shared-prof", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{Shared: true}},
			},
			setupMock: func() *hostMock {
				return new(hostMock)
			},
			expectErr:   true,
			errContains: "reserved CPU 1 is listed in multiple reservedCPUs entries",
		},
		{
			name:   "reserved profile unavailable",
			config: newPowerNodeConfig("c", "shared-prof", nil, []powerv1alpha1.ReservedSpec{{Cores: []uint{0}, PowerProfile: "missing"}}, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "shared-prof", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{Shared: true}},
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{}},
			},
			setupMock: func() *hostMock {
				h := new(hostMock)
				h.On("GetExclusivePool", "shared-prof").Return(new(poolMock))
				h.On("GetExclusivePool", "missing").Return(nil)
				return h
			},
			expectErr:   true,
			errContains: "missing",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			hostMk := tc.setupMock()
			r := createPowerNodeConfigReconciler(tc.clientObjs, hostMk)
			logger := testLogger()
			err := r.validatePowerNodeConfigProfiles(context.TODO(), tc.config, "test-node", &logger)
			if tc.expectErr {
				assert.Error(t, err)
				if tc.errContains != "" {
					assert.Contains(t, err.Error(), tc.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- configureSharedPool ---

func TestConfigureSharedPool(t *testing.T) {
	tcases := []struct {
		name        string
		profileName string
		setupMock   func() *hostMock
		expectErr   bool
		errContains string
	}{
		{
			name:        "success",
			profileName: "test-profile",
			setupMock: func() *hostMock {
				h := new(hostMock)
				ep := new(poolMock)
				sp := new(poolMock)
				rp := new(poolMock)
				pm := new(profMock)
				h.On("GetExclusivePool", "test-profile").Return(ep)
				h.On("GetSharedPool").Return(sp)
				h.On("GetReservedPool").Return(rp)
				ep.On("GetPowerProfile").Return(pm)
				sp.On("SetPowerProfile", pm).Return(nil)
				rp.On("SetCpuIDs", []uint{}).Return(nil)
				return h
			},
		},
		{
			name:        "pool not found",
			profileName: "missing",
			setupMock: func() *hostMock {
				h := new(hostMock)
				h.On("GetExclusivePool", "missing").Return(nil)
				return h
			},
			expectErr:   true,
			errContains: "pool for profile",
		},
		{
			name:        "profile not found on pool",
			profileName: "test-profile",
			setupMock: func() *hostMock {
				h := new(hostMock)
				ep := new(poolMock)
				h.On("GetExclusivePool", "test-profile").Return(ep)
				ep.On("GetPowerProfile").Return(nil)
				return h
			},
			expectErr:   true,
			errContains: "profile for pool",
		},
		{
			name:        "set profile error",
			profileName: "test-profile",
			setupMock: func() *hostMock {
				h := new(hostMock)
				ep := new(poolMock)
				sp := new(poolMock)
				pm := new(profMock)
				h.On("GetExclusivePool", "test-profile").Return(ep)
				h.On("GetSharedPool").Return(sp)
				ep.On("GetPowerProfile").Return(pm)
				sp.On("SetPowerProfile", pm).Return(assert.AnError)
				return h
			},
			expectErr:   true,
			errContains: "failed to set shared pool profile",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			hostMk := tc.setupMock()
			config := newPowerNodeConfig("c", tc.profileName, nil, nil, time.Now())
			r := &PowerNodeConfigReconciler{PowerLibrary: hostMk}
			logger := testLogger()
			err := r.configureSharedPool(config, &logger)
			if tc.expectErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- configureReservedPools ---

func TestConfigureReservedPools(t *testing.T) {
	tcases := []struct {
		name             string
		reserved         []powerv1alpha1.ReservedSpec
		setupMock        func() *hostMock
		expectedCPUCount int
		expectedErrCount int
	}{
		{
			name: "no reserved CPUs",
			setupMock: func() *hostMock {
				h := new(hostMock)
				rp := new(poolMock)
				h.On("GetReservedPool").Return(rp)
				h.On("GetAllExclusivePools").Return(&power.PoolList{})
				rp.On("SetCpuIDs", []uint{}).Return(nil)
				return h
			},
		},
		{
			name:     "with profile",
			reserved: []powerv1alpha1.ReservedSpec{{Cores: []uint{0, 1}, PowerProfile: "perf"}},
			setupMock: func() *hostMock {
				h := new(hostMock)
				rp := new(poolMock)
				sp := new(poolMock)
				ep := new(poolMock)
				pp := new(poolMock)
				pm := new(profMock)
				h.On("GetReservedPool").Return(rp)
				h.On("GetSharedPool").Return(sp)
				h.On("GetAllExclusivePools").Return(&power.PoolList{})
				h.On("AddExclusivePool", mock.Anything).Return(pp, nil)
				h.On("GetExclusivePool", "perf").Return(ep)
				rp.On("SetCpuIDs", []uint{}).Return(nil)
				sp.On("MoveCpuIDs", []uint{0, 1}).Return(nil)
				ep.On("GetPowerProfile").Return(pm)
				pp.On("SetPowerProfile", pm).Return(nil)
				pp.On("SetCpuIDs", []uint{0, 1}).Return(nil)
				return h
			},
			expectedCPUCount: 1,
		},
		{
			name:     "without profile",
			reserved: []powerv1alpha1.ReservedSpec{{Cores: []uint{0, 1}}},
			setupMock: func() *hostMock {
				h := new(hostMock)
				rp := new(poolMock)
				sp := new(poolMock)
				h.On("GetReservedPool").Return(rp)
				h.On("GetSharedPool").Return(sp)
				h.On("GetAllExclusivePools").Return(&power.PoolList{})
				rp.On("SetCpuIDs", []uint{}).Return(nil)
				rp.On("MoveCpuIDs", []uint{0, 1}).Return(nil)
				sp.On("MoveCpuIDs", []uint{0, 1}).Return(nil)
				return h
			},
			expectedCPUCount: 1,
		},
		{
			name:     "create pool failure, fallback to reserved",
			reserved: []powerv1alpha1.ReservedSpec{{Cores: []uint{0, 1}, PowerProfile: "perf"}},
			setupMock: func() *hostMock {
				h := new(hostMock)
				rp := new(poolMock)
				sp := new(poolMock)
				h.On("GetReservedPool").Return(rp)
				h.On("GetSharedPool").Return(sp)
				h.On("GetAllExclusivePools").Return(&power.PoolList{})
				h.On("AddExclusivePool", mock.Anything).Return(nil, assert.AnError)
				rp.On("SetCpuIDs", []uint{}).Return(nil)
				rp.On("MoveCpuIDs", []uint{0, 1}).Return(nil)
				sp.On("MoveCpuIDs", []uint{0, 1}).Return(nil)
				return h
			},
			expectedCPUCount: 1,
			expectedErrCount: 1,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			hostMk := tc.setupMock()
			config := newPowerNodeConfig("c", "p", nil, tc.reserved, time.Now())
			r := &PowerNodeConfigReconciler{PowerLibrary: hostMk}
			logger := testLogger()
			cpus, errs := r.configureReservedPools(config, "test-node", &logger)
			assert.Len(t, cpus, tc.expectedCPUCount)
			assert.Len(t, errs, tc.expectedErrCount)
		})
	}
}

// --- createReservedPool ---

func TestCreateReservedPool(t *testing.T) {
	tcases := []struct {
		name        string
		reserved    powerv1alpha1.ReservedSpec
		setupMock   func() *hostMock
		expectErr   bool
		errContains string
	}{
		{
			name:     "success",
			reserved: powerv1alpha1.ReservedSpec{Cores: []uint{0, 1}, PowerProfile: "perf"},
			setupMock: func() *hostMock {
				h := new(hostMock)
				pp := new(poolMock)
				ep := new(poolMock)
				pm := new(profMock)
				h.On("AddExclusivePool", "node-reserved-[0 1]").Return(pp, nil)
				h.On("GetExclusivePool", "perf").Return(ep)
				ep.On("GetPowerProfile").Return(pm)
				pp.On("SetPowerProfile", pm).Return(nil)
				pp.On("SetCpuIDs", []uint{0, 1}).Return(nil)
				return h
			},
		},
		{
			name:     "add pool error",
			reserved: powerv1alpha1.ReservedSpec{Cores: []uint{0}, PowerProfile: "perf"},
			setupMock: func() *hostMock {
				h := new(hostMock)
				h.On("AddExclusivePool", mock.Anything).Return(nil, assert.AnError)
				return h
			},
			expectErr:   true,
			errContains: "failed to create reserved pool",
		},
		{
			name:     "profile pool not found",
			reserved: powerv1alpha1.ReservedSpec{Cores: []uint{0}, PowerProfile: "missing"},
			setupMock: func() *hostMock {
				h := new(hostMock)
				pp := new(poolMock)
				h.On("AddExclusivePool", mock.Anything).Return(pp, nil)
				h.On("GetExclusivePool", "missing").Return(nil)
				pp.On("Remove").Return(nil)
				return h
			},
			expectErr:   true,
			errContains: "has no existing pool",
		},
		{
			name:     "set profile error",
			reserved: powerv1alpha1.ReservedSpec{Cores: []uint{0}, PowerProfile: "perf"},
			setupMock: func() *hostMock {
				h := new(hostMock)
				pp := new(poolMock)
				ep := new(poolMock)
				pm := new(profMock)
				h.On("AddExclusivePool", mock.Anything).Return(pp, nil)
				h.On("GetExclusivePool", "perf").Return(ep)
				ep.On("GetPowerProfile").Return(pm)
				pp.On("SetPowerProfile", pm).Return(assert.AnError)
				pp.On("Remove").Return(nil)
				return h
			},
			expectErr:   true,
			errContains: "failed to set profile",
		},
		{
			name:     "set cpuIDs error",
			reserved: powerv1alpha1.ReservedSpec{Cores: []uint{0}, PowerProfile: "perf"},
			setupMock: func() *hostMock {
				h := new(hostMock)
				pp := new(poolMock)
				ep := new(poolMock)
				pm := new(profMock)
				h.On("AddExclusivePool", mock.Anything).Return(pp, nil)
				h.On("GetExclusivePool", "perf").Return(ep)
				ep.On("GetPowerProfile").Return(pm)
				pp.On("SetPowerProfile", pm).Return(nil)
				pp.On("SetCpuIDs", mock.Anything).Return(assert.AnError)
				pp.On("Remove").Return(nil)
				return h
			},
			expectErr:   true,
			errContains: "failed to move cores to reserved pool",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			hostMk := tc.setupMock()
			r := &PowerNodeConfigReconciler{PowerLibrary: hostMk}
			err := r.createReservedPool(tc.reserved, "node")
			if tc.expectErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- cleanupPowerNodeConfigPools ---

func TestCleanupPowerNodeConfigPools(t *testing.T) {
	tcases := []struct {
		name      string
		setupMock func() *hostMock
		objs      []runtime.Object
		expectErr bool
	}{
		{
			name: "success with shared and reserved CPUs",
			setupMock: func() *hostMock {
				h := new(hostMock)
				sp := createMockPoolWithCPUs([]uint{2, 3})
				rp := new(poolMock)
				prp := createMockPoolWithCPUs([]uint{0, 1})
				h.On("GetSharedPool").Return(sp)
				h.On("GetReservedPool").Return(rp)
				h.On("GetAllExclusivePools").Return(&power.PoolList{prp})
				prp.On("Name").Return("test-node-reserved-[0 1]")
				prp.On("Remove").Return(nil)
				rp.On("MoveCpus", mock.Anything).Return(nil)
				return h
			},
			objs: []runtime.Object{newPowerNodeState("test-node", "config-a")},
		},
		{
			name: "no pseudo-reserved pools",
			setupMock: func() *hostMock {
				h := new(hostMock)
				sp := createMockPoolWithCPUs([]uint{2, 3})
				rp := new(poolMock)
				ep := new(poolMock)
				h.On("GetSharedPool").Return(sp)
				h.On("GetReservedPool").Return(rp)
				h.On("GetAllExclusivePools").Return(&power.PoolList{ep})
				ep.On("Name").Return("some-other-pool")
				rp.On("MoveCpus", mock.Anything).Return(nil)
				return h
			},
			objs: []runtime.Object{newPowerNodeState("test-node", "config-a")},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			hostMk := tc.setupMock()
			r := createPowerNodeConfigReconciler(tc.objs, hostMk)
			logger := testLogger()
			err := r.cleanupPowerNodeConfigPools(context.TODO(), "test-node", &logger)
			if tc.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- applyPowerNodeConfig ---

func TestApplyPowerNodeConfig(t *testing.T) {
	tcases := []struct {
		name           string
		config         *powerv1alpha1.PowerNodeConfig
		conflictErrors []string
		clientObjs     []runtime.Object
		setupMock      func() *hostMock
		expectRequeue  bool
		expectErr      bool
	}{
		{
			name:   "validation failure records error and requeues",
			config: newPowerNodeConfig("config-a", "missing-prof", nil, nil, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				newPowerNodeState("test-node", ""),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "missing-prof", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{}},
			},
			setupMock: func() *hostMock {
				h := new(hostMock)
				h.On("GetExclusivePool", "missing-prof").Return(nil)
				return h
			},
			expectRequeue: true,
		},
		{
			name:   "success with no reserved CPUs",
			config: newPowerNodeConfig("config-a", "test-prof", nil, nil, time.Now()),
			clientObjs: []runtime.Object{
				newTestNode("test-node", map[string]string{}),
				newPowerNodeState("test-node", ""),
				&powerv1alpha1.PowerProfile{ObjectMeta: metav1.ObjectMeta{Name: "test-prof", Namespace: PowerNamespace}, Spec: powerv1alpha1.PowerProfileSpec{Shared: true}},
			},
			setupMock: func() *hostMock {
				h := new(hostMock)
				ep := new(poolMock)
				sp := createMockPoolWithCPUs([]uint{2, 3, 4, 5})
				rp := new(poolMock)
				pm := new(profMock)
				h.On("GetExclusivePool", "test-prof").Return(ep)
				h.On("GetSharedPool").Return(sp)
				h.On("GetReservedPool").Return(rp)
				h.On("GetAllExclusivePools").Return(&power.PoolList{})
				ep.On("GetPowerProfile").Return(pm)
				sp.On("SetPowerProfile", pm).Return(nil)
				rp.On("SetCpuIDs", []uint{}).Return(nil)
				return h
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			hostMk := tc.setupMock()
			r := createPowerNodeConfigReconciler(tc.clientObjs, hostMk)
			logger := testLogger()
			result, err := r.applyPowerNodeConfig(context.TODO(), tc.config, "test-node", tc.conflictErrors, &logger)
			if tc.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			if tc.expectRequeue {
				assert.Equal(t, queuetime, result.RequeueAfter)
			}
		})
	}
}
