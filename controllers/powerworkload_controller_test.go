package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zapcore"

	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/openshift-kni/kubernetes-power-manager/internal/scaling"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

func createWorkloadReconcilerObject(objs []runtime.Object) (*PowerWorkloadReconciler, error) {
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
	if err := powerv1.AddToScheme(s); err != nil {
		return nil, err
	}

	// Create a fake client to mock API calls.
	cl := fake.NewClientBuilder().
		WithRuntimeObjects(objs...).
		WithScheme(s).
		WithStatusSubresource(&powerv1.PowerWorkload{}).
		Build()

	// Create a ReconcileNode object with the scheme and fake client.
	r := &PowerWorkloadReconciler{cl, ctrl.Log.WithName("testing"), s, nil, nil, nil}

	return r, nil
}

type reservedPoolMocks struct {
	node             *hostMock
	shared           *poolMock
	performance      *poolMock
	exclusiveRserved *poolMock
	reserved         *poolMock
}

// used to remove a call from a predefined mock
func popCall(calls []*mock.Call, method string) []*mock.Call {
	for i, call := range calls {
		if call.Method == method {
			calls[i] = calls[len(calls)-1]
			return calls[:len(calls)-1]
		}
	}
	return calls
}

// creates a basic template for reaching reserved pool related segments
func mocktemplate() reservedPoolMocks {
	nodemk := new(hostMock)
	sharedPoolmk := new(poolMock)
	perfPoolmk := new(poolMock)
	exclusiveReservedmk := new(poolMock)
	reservedmk := new(poolMock)
	nodemk.On("GetReservedPool").Return(reservedmk)
	nodemk.On("GetSharedPool").Return(sharedPoolmk)
	nodemk.On("GetAllExclusivePools").Return(&power.PoolList{exclusiveReservedmk})
	nodemk.On("AddExclusivePool", mock.Anything).Return(exclusiveReservedmk, nil)
	nodemk.On("GetExclusivePool", mock.Anything).Return(perfPoolmk)
	sharedPoolmk.On("Cpus").Return(&power.CpuList{})
	sharedPoolmk.On("MoveCpuIDs", mock.Anything).Return(nil)
	sharedPoolmk.On("SetCpuIDs", mock.Anything).Return(nil)
	sharedPoolmk.On("SetPowerProfile", mock.Anything).Return(nil)
	reservedmk.On("MoveCpuIDs", mock.Anything).Return(nil)
	reservedmk.On("SetCpuIDs", mock.Anything).Return(nil)
	exclusiveReservedmk.On("Name").Return("TestNode-reserved-[0]")
	exclusiveReservedmk.On("Remove").Return(nil)
	exclusiveReservedmk.On("SetCpuIDs", mock.Anything).Return(nil)
	exclusiveReservedmk.On("SetPowerProfile", mock.Anything).Return(nil)
	perfPoolmk.On("GetPowerProfile").Return(new(profMock))
	return reservedPoolMocks{
		node:             nodemk,
		shared:           sharedPoolmk,
		performance:      perfPoolmk,
		exclusiveRserved: exclusiveReservedmk,
		reserved:         reservedmk,
	}
}

// mocktemplateCleanupSharedWorkload creates a minimal PowerLibrary mock for the cleanupSharedWorkloadPools() path.
// It avoids the extra expectations in mocktemplate() that aren't invoked during cleanup.
func mocktemplateCleanupSharedWorkload(nodeName string) *hostMock {
	nodemk := new(hostMock)
	sharedPoolmk := new(poolMock)
	reservedmk := new(poolMock)
	exclusiveReservedmk := new(poolMock)

	nodemk.On("GetSharedPool").Return(sharedPoolmk)
	sharedPoolmk.On("Cpus").Return(&power.CpuList{})

	nodemk.On("GetAllExclusivePools").Return(&power.PoolList{exclusiveReservedmk})
	exclusiveReservedmk.On("Name").Return(nodeName + "-reserved-[0]")
	exclusiveReservedmk.On("Cpus").Return(&power.CpuList{})
	exclusiveReservedmk.On("Remove").Return(nil)

	nodemk.On("GetReservedPool").Return(reservedmk)
	reservedmk.On("MoveCpus", mock.Anything).Return(nil)

	return nodemk
}
func TestPowerWorkload_Reconcile(t *testing.T) {
	testNode := "TestNode"
	nodeObj := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   testNode,
			Labels: map[string]string{"powernode": "selector"},
		},
		Status: corev1.NodeStatus{
			Capacity: map[corev1.ResourceName]resource.Quantity{
				CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
			},
		},
	}
	pwrProfileObj := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "performance",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "performance",
			PStates: powerv1.PStatesConfig{
				Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3600},
				Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: 3200},
				Epp:      "performance",
				Governor: "powersave",
			},
		},
	}
	pwrWorkloadObj := &powerv1.PowerWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "performance-TestNode",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerWorkloadSpec{
			Name:              "performance-TestNode",
			AllCores:          false,
			PowerNodeSelector: map[string]string{"powernode": "selector"},
			PowerProfile:      "performance",
		},
		Status: powerv1.PowerWorkloadStatus{
			WorkloadNodes: powerv1.WorkloadNode{
				Name:   testNode,
				CpuIds: []uint{4, 5},
			},
		},
	}
	sharedSkeleton := &powerv1.PowerWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-" + testNode,
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerWorkloadSpec{
			Name:              "shared-" + testNode,
			AllCores:          true,
			PowerNodeSelector: map[string]string{"powernode": "selector"},
			ReservedCPUs: []powerv1.ReservedSpec{
				{Cores: []uint{0, 1}, PowerProfile: "performance"},
				{Cores: []uint{2}, PowerProfile: "performance"},
			},
		},
	}
	tcases := []struct {
		testCase     string
		nodeName     string
		workloadName string
		clientObjs   []runtime.Object
		getNodemk    func() *hostMock
		validateErr  func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool
	}{
		{
			testCase:     "Test Case 1 - pool does not exist",
			workloadName: "performance-TestNode",
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "does not exist in the power library")
			},
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				nodemk.On("GetExclusivePool", mock.Anything).Return(nil)
				return nodemk
			},
			clientObjs: []runtime.Object{
				pwrWorkloadObj,
				nodeObj,
			},
		},
		{
			testCase:     "Test Case 2 - workload creation",
			workloadName: "shared-" + testNode,
			getNodemk:    func() *hostMock { return new(hostMock) },
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.Nil(t, e)
				req := reconcile.Request{
					NamespacedName: client.ObjectKey{
						Name:      "shared-TestNode",
						Namespace: IntelPowerNamespace,
					},
				}

				_, err := r.Reconcile(context.TODO(), req)
				return assert.NoError(t, err)
			},
			clientObjs: []runtime.Object{
				pwrWorkloadObj,
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:              "shared-" + testNode,
						AllCores:          true,
						ReservedCPUs:      []powerv1.ReservedSpec{{Cores: []uint{0, 1}}},
						PowerNodeSelector: map[string]string{"powernode": "selector"},
						PowerProfile:      "shared",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:   testNode,
							CpuIds: []uint{5, 3},
						},
					},
				},
				nodeObj,
			},
		},
		{
			testCase:     "Test Case 3 - pool deletion err",
			workloadName: "performance-TestNode",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				poolmk := new(poolMock)
				nodemk.On("GetExclusivePool", mock.Anything).Return(poolmk)
				poolmk.On("Remove").Return(errors.New("pool removal err"))
				return nodemk
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "pool removal err")
			},
		},
		{
			testCase:     "Test Case 4 - shared workload with missing shared profile",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				return nodemk
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.NoError(t, e)
				return assert.Equal(t, result.RequeueAfter, queuetime)
			},
			clientObjs: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:              "shared-" + testNode,
						AllCores:          true,
						PowerNodeSelector: map[string]string{"powernode": "selector"},
						PowerProfile:      "shared",
					},
				},
				nodeObj,
			},
		},
		{
			testCase:     "Test Case 5 - shared workload with unavailable shared profile on node",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				nodemk.On("GetExclusivePool", "shared").Return(nil)
				return nodemk
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.NoError(t, e)
				return assert.Equal(t, result.RequeueAfter, queuetime)
			},
			clientObjs: []runtime.Object{
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name:   "shared",
						Shared: true,
					},
				},
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:              "shared-" + testNode,
						AllCores:          true,
						PowerNodeSelector: map[string]string{"powernode": "selector"},
						PowerProfile:      "shared",
					},
				},
				nodeObj,
			},
		},
		{
			testCase:     "Test Case 6 - shared workload with missing profile for reserved CPUs",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				return new(hostMock)
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.NoError(t, e)
				return assert.Equal(t, result.RequeueAfter, queuetime)
			},
			clientObjs: []runtime.Object{sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 7 - shared workload with unavailable profile for reserved CPUs",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				nodemk.On("GetExclusivePool", "performance").Return(nil)
				return nodemk
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.NoError(t, e)
				return assert.Equal(t, result.RequeueAfter, queuetime)
			},
			clientObjs: []runtime.Object{sharedSkeleton, nodeObj, pwrProfileObj},
		},
		{
			testCase:     "Test Case 8 - shared workload deletion",
			workloadName: "shared",
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				reservedmk := new(poolMock)
				poolmk := new(poolMock)
				nodemk.On("GetSharedPool").Return(poolmk)
				poolmk.On("SetPowerProfile", nil).Return(nil)
				nodemk.On("GetReservedPool").Return(reservedmk)
				exclusiveReservedmk := new(poolMock)
				nodemk.On("GetAllExclusivePools").Return(&power.PoolList{exclusiveReservedmk})
				exclusiveReservedmk.On("Name").Return("TestNode-reserved-[0]")
				exclusiveReservedmk.On("Remove").Return(nil)
				exclusiveReservedmk.On("SetPowerProfile", mock.Anything).Return(nil)
				exclusiveReservedmk.On("Cpus").Return(&power.CpuList{})
				exclusiveReservedmk.On("Remove").Return(nil)
				reservedmk.On("MoveCpus", mock.Anything).Return(nil)
				poolmk.On("Cpus").Return(&power.CpuList{})
				sharedPowerWorkloadName = "shared"
				return nodemk
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.Nil(t, e)
				return assert.Empty(t, sharedPowerWorkloadName)
			},
		},
		{
			testCase:     "Test Case 9 - shared workload on wrong node",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				sharedPowerWorkloadName = "shared"
				return new(hostMock)
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.Nil(t, e)
			},
			clientObjs: []runtime.Object{sharedSkeleton},
		},
		{
			testCase:     "Test Case 10 - shared workload already exists",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				return new(hostMock)
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "a shared power workload already exists")
			},
			clientObjs: []runtime.Object{sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 11 - set cpu error",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				poolmk := new(poolMock)
				sharedPoolmk := new(poolMock)
				dummyPoolmk := new(poolMock)
				profmk := new(profMock)
				nodemk.On("GetReservedPool").Return(poolmk)
				nodemk.On("GetExclusivePool", mock.Anything).Return(dummyPoolmk)
				nodemk.On("GetSharedPool").Return(sharedPoolmk)
				dummyPoolmk.On("GetPowerProfile").Return(profmk)
				sharedPoolmk.On("SetPowerProfile", mock.Anything).Return(nil)
				poolmk.On("SetCpuIDs", mock.Anything).Return(fmt.Errorf("set cpu error"))
				sharedPowerWorkloadName = ""
				return nodemk
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "set cpu error")
			},
			clientObjs: []runtime.Object{pwrProfileObj, sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 12 - shared pool creation",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				template := mocktemplate()
				return template.node
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.Nil(t, e)
				return assert.Equal(t, "shared-"+testNode, sharedPowerWorkloadName)
			},
			clientObjs: []runtime.Object{pwrProfileObj, sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 13 - reserved setProfile error recovery failure",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				template := mocktemplate()
				// have 2 calls causign an issue
				template.exclusiveRserved.ExpectedCalls = popCall(template.exclusiveRserved.ExpectedCalls, "SetPowerProfile")
				template.reserved.ExpectedCalls = popCall(template.reserved.ExpectedCalls, "MoveCpuIDs")
				template.exclusiveRserved.On("SetPowerProfile", mock.Anything).Return(fmt.Errorf("set profile err"))
				template.reserved.On("MoveCpuIDs", mock.Anything).Return(fmt.Errorf("recovery failed"))
				return template.node
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "recovery failed")
			},
			clientObjs: []runtime.Object{pwrProfileObj, sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 14 - reserved setCpu error recovery failure",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				template := mocktemplate()
				template.exclusiveRserved.ExpectedCalls = popCall(template.exclusiveRserved.ExpectedCalls, "SetCpuIDs")
				template.reserved.ExpectedCalls = popCall(template.reserved.ExpectedCalls, "MoveCpuIDs")
				template.exclusiveRserved.On("SetCpuIDs", mock.Anything).Return(fmt.Errorf("set profile err"))
				template.reserved.On("MoveCpuIDs", mock.Anything).Return(fmt.Errorf("recovery failed"))
				return template.node
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "recovery failed")
			},
			clientObjs: []runtime.Object{pwrProfileObj, sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 15 - reserved SetCpuIDs() and pseudoReservedPool.Remove() errors",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				template := mocktemplate()
				template.exclusiveRserved.ExpectedCalls = popCall(template.exclusiveRserved.ExpectedCalls, "SetCpuIDs")
				template.reserved.ExpectedCalls = popCall(template.reserved.ExpectedCalls, "MoveCpuIDs")
				template.exclusiveRserved.On("SetCpuIDs", mock.Anything).Return(fmt.Errorf("set profile err"))
				template.exclusiveRserved.On("Remove", mock.Anything).Return(fmt.Errorf("remove pool error"))
				template.reserved.On("MoveCpuIDs", mock.Anything).Return(fmt.Errorf("recovery failed"))
				return template.node
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "recovery failed")
			},
			clientObjs: []runtime.Object{pwrProfileObj, sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 16 - reserved recovery",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				template := mocktemplate()
				exclusiveReservedmk2 := new(poolMock)
				template.exclusiveRserved.ExpectedCalls = popCall(template.exclusiveRserved.ExpectedCalls, "SetCpuIDs")
				template.reserved.ExpectedCalls = popCall(template.reserved.ExpectedCalls, "MoveCpuIDs")
				template.node.ExpectedCalls = popCall(template.node.ExpectedCalls, "GetAllExclusivePools")
				template.node.ExpectedCalls = popCall(template.node.ExpectedCalls, "AddExclusivePool")
				template.node.On("GetAllExclusivePools").Return(&power.PoolList{exclusiveReservedmk2, template.exclusiveRserved})
				template.node.On("AddExclusivePool", "TestNode-reserved-[2]").Return(exclusiveReservedmk2, nil)
				template.node.On("AddExclusivePool", "TestNode-reserved-[0 1]").Return(template.exclusiveRserved, nil)
				exclusiveReservedmk2.On("Name").Return("TestNode-reserved-[2]")
				exclusiveReservedmk2.On("Remove").Return(nil)
				exclusiveReservedmk2.On("SetCpuIDs", mock.Anything).Return(nil)
				exclusiveReservedmk2.On("SetPowerProfile", mock.Anything).Return(nil)
				template.exclusiveRserved.On("SetCpuIDs", mock.Anything).Return(fmt.Errorf("set profile err"))
				template.reserved.On("MoveCpuIDs", mock.Anything).Return(nil)
				return template.node
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "error(s) encountered establishing reserved pool")
			},
			clientObjs: []runtime.Object{pwrProfileObj, sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 17 - SetPowerProfile() and pseudoReservedPool.Remove() errors",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				template := mocktemplate()
				exclusiveReservedmk2 := new(poolMock)
				template.exclusiveRserved.ExpectedCalls = popCall(template.exclusiveRserved.ExpectedCalls, "SetCpuIDs")
				template.reserved.ExpectedCalls = popCall(template.reserved.ExpectedCalls, "MoveCpuIDs")
				template.node.ExpectedCalls = popCall(template.node.ExpectedCalls, "GetAllExclusivePools")
				template.node.ExpectedCalls = popCall(template.node.ExpectedCalls, "AddExclusivePool")
				template.node.On("GetAllExclusivePools").Return(&power.PoolList{exclusiveReservedmk2, template.exclusiveRserved})
				template.node.On("AddExclusivePool", "TestNode-reserved-[2]").Return(exclusiveReservedmk2, nil)
				template.node.On("AddExclusivePool", "TestNode-reserved-[0 1]").Return(template.exclusiveRserved, nil)
				exclusiveReservedmk2.On("Name").Return("TestNode-reserved-[2]")
				exclusiveReservedmk2.On("Remove").Return(nil).Once()
				exclusiveReservedmk2.On("Remove").Return(fmt.Errorf("remove error")).Once()
				exclusiveReservedmk2.On("SetCpuIDs", mock.Anything).Return(nil)
				exclusiveReservedmk2.On("SetPowerProfile", mock.Anything).Return(fmt.Errorf("set profile err"))
				template.exclusiveRserved.On("SetCpuIDs", mock.Anything).Return(fmt.Errorf("set CPU ids err"))
				template.exclusiveRserved.On("Remove").Return(nil)
				template.reserved.On("MoveCpuIDs", mock.Anything).Return(nil)
				return template.node
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				return assert.ErrorContains(t, e, "error(s) encountered establishing reserved pool")
			},
			clientObjs: []runtime.Object{pwrProfileObj, sharedSkeleton, nodeObj},
		},
		{
			testCase:     "Test Case 18 - ignore exclusive workload from other node",
			workloadName: "performance-OtherNode",
			getNodemk:    func() *hostMock { return new(hostMock) },
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.NoError(t, e)
				updated := &powerv1.PowerWorkload{}
				err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "performance-OtherNode", Namespace: IntelPowerNamespace}, updated)
				assert.NoError(t, err)
				// The status is not modified.
				return assert.Equal(t, "OtherNode", updated.Status.WorkloadNodes.Name)
			},
			clientObjs: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-OtherNode",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "performance-OtherNode",
						PowerProfile: "performance",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name: "OtherNode",
						},
					},
				},
			},
		},
		{
			testCase:     "Test Case 19 - ignore shared workload when claimed by another node",
			workloadName: "shared-" + testNode,
			getNodemk:    func() *hostMock { return new(hostMock) },
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.NoError(t, e)
				return assert.Equal(t, ctrl.Result{}, result)
			},
			clientObjs: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:              "shared-" + testNode,
						AllCores:          true,
						PowerNodeSelector: map[string]string{"powernode": "selector"},
						PowerProfile:      "shared",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name: "OtherNode",
						},
					},
				},
				nodeObj,
			},
		},
		{
			testCase:     "Test Case 20 - shared workload cleans up pools and ownership when node is no longer matched by selector",
			workloadName: "shared-" + testNode,
			getNodemk: func() *hostMock {
				nodemk := new(hostMock)
				sharedPoolmk := new(poolMock)
				reservedmk := new(poolMock)
				exclusiveReservedmk := new(poolMock)
				// Mock the PowerLibrary calls that are used in cleanupSharedWorkloadPools().
				nodemk.On("GetSharedPool").Return(sharedPoolmk)
				sharedPoolmk.On("Cpus").Return(&power.CpuList{})

				nodemk.On("GetAllExclusivePools").Return(&power.PoolList{exclusiveReservedmk})
				exclusiveReservedmk.On("Name").Return(testNode + "-reserved-[0]")
				exclusiveReservedmk.On("Cpus").Return(&power.CpuList{})
				exclusiveReservedmk.On("Remove").Return(nil)

				nodemk.On("GetReservedPool").Return(reservedmk)
				reservedmk.On("MoveCpus", mock.Anything).Return(nil)
				return nodemk
			},
			validateErr: func(r *PowerWorkloadReconciler, result ctrl.Result, e error) bool {
				assert.NoError(t, e)
				updated := &powerv1.PowerWorkload{}
				err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "shared-" + testNode, Namespace: IntelPowerNamespace}, updated)
				assert.NoError(t, err)
				return assert.Equal(t, "", updated.Status.WorkloadNodes.Name)
			},
			clientObjs: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "shared-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:              "shared-" + testNode,
						AllCores:          true,
						PowerNodeSelector: map[string]string{}, // selector is empty, so the current node is not matched anymore
						PowerProfile:      "shared",
						ReservedCPUs:      []powerv1.ReservedSpec{{Cores: []uint{0, 1}}},
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name: testNode,
						},
					},
				},
				nodeObj,
			},
		},
	}

	for _, tc := range tcases {
		t.Log(tc.testCase)
		t.Setenv("NODE_NAME", testNode)
		if tc.nodeName != "" {
			t.Setenv("NODE_NAME", tc.nodeName)
		}
		r, err := createWorkloadReconcilerObject(tc.clientObjs)
		if err != nil {
			t.Error(err)
			t.Fatalf("%s - error creating the reconciler object", tc.testCase)
		}
		nodemk := tc.getNodemk()
		if tc.testCase == "Test Case 2 - workload creation" {
			host, teardown, err := fullDummySystem()
			assert.Nil(t, err)
			defer teardown()
			sharedProf, err := power.NewPowerProfile("shared", &intstr.IntOrString{Type: intstr.Int, IntVal: 1000}, &intstr.IntOrString{Type: intstr.Int, IntVal: 1000}, "powersave", "power", map[string]bool{"C1": false}, nil)
			assert.Nil(t, err)
			assert.Nil(t, host.GetSharedPool().SetPowerProfile(sharedProf))
			perf, err := host.AddExclusivePool("performance")
			assert.Nil(t, err)
			pool, err := host.AddExclusivePool("shared")
			pool.SetPowerProfile(sharedProf)
			assert.Nil(t, err)
			assert.Nil(t, host.GetSharedPool().SetCpuIDs([]uint{2, 3, 4, 5, 6, 7}))
			assert.Nil(t, perf.SetCpuIDs([]uint{2, 3}))
			r.PowerLibrary = host
		} else {
			r.PowerLibrary = nodemk
		}
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.workloadName,
				Namespace: IntelPowerNamespace,
			},
		}
		result, err := r.Reconcile(context.TODO(), req)
		tc.validateErr(r, result, err)
		nodemk.AssertExpectations(t)
	}
}

func TestPowerWorkload_Reconcile_WithCpuScalingPolicy(t *testing.T) {
	testNode := "TestNode"
	t.Setenv("NODE_NAME", testNode)

	testcases := []struct {
		name                                                    string
		profileName                                             string
		podUID                                                  string
		cpuIDs                                                  []uint
		sample, cooldown                                        time.Duration
		targetUsage, usageDiff, freqDiff, fallbackPct, scalePct int
		initialConns                                            []scaling.DPDKTelemetryConnectionData
	}{
		{
			name:        "profile1-two-cpus",
			profileName: "scaling-profile-1",
			podUID:      "foo",
			cpuIDs:      []uint{0, 1},
			sample:      10 * time.Millisecond,
			cooldown:    30 * time.Millisecond,
			targetUsage: 100, usageDiff: 10, freqDiff: 25, fallbackPct: 50, scalePct: 100,
			initialConns: []scaling.DPDKTelemetryConnectionData{
				{PodUID: "baz", WatchedCPUs: []uint{5}},
			},
		},
		{
			name:        "profile2-one-cpu",
			profileName: "scaling-profile-2",
			podUID:      "bar",
			cpuIDs:      []uint{5},
			sample:      100 * time.Millisecond,
			cooldown:    100 * time.Millisecond,
			targetUsage: 50, usageDiff: 5, freqDiff: 45, fallbackPct: 0, scalePct: 130,
			initialConns: []scaling.DPDKTelemetryConnectionData{
				{PodUID: "foo", WatchedCPUs: []uint{0, 1}},
			},
		},
		{
			name:        "profile3-one-cpu",
			profileName: "scaling-profile-3",
			podUID:      "qux",
			cpuIDs:      []uint{7},
			sample:      300 * time.Millisecond,
			cooldown:    301 * time.Millisecond,
			targetUsage: 0, usageDiff: 0, freqDiff: 0, fallbackPct: 100, scalePct: 47,
			initialConns: []scaling.DPDKTelemetryConnectionData{
				{PodUID: "foo", WatchedCPUs: []uint{0, 1}},
				{PodUID: "bar", WatchedCPUs: []uint{5}},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Create objects
			policy := &powerv1.CpuScalingPolicy{
				WorkloadType:               "polling-dpdk",
				SamplePeriod:               &metav1.Duration{Duration: tc.sample},
				CooldownPeriod:             &metav1.Duration{Duration: tc.cooldown},
				TargetUsage:                intPtr(tc.targetUsage),
				AllowedUsageDifference:     intPtr(tc.usageDiff),
				AllowedFrequencyDifference: intPtr(tc.freqDiff),
				FallbackFreqPercent:        intPtr(tc.fallbackPct),
				ScalePercentage:            intPtr(tc.scalePct),
			}
			profile := &powerv1.PowerProfile{
				ObjectMeta: metav1.ObjectMeta{Name: tc.profileName, Namespace: IntelPowerNamespace},
				Spec:       powerv1.PowerProfileSpec{Name: tc.profileName, CpuScalingPolicy: policy},
			}
			workload := &powerv1.PowerWorkload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.profileName + "-" + testNode,
					Namespace: IntelPowerNamespace,
				},
				Spec: powerv1.PowerWorkloadSpec{
					Name:         tc.profileName + "-" + testNode,
					PowerProfile: tc.profileName,
				},
				Status: powerv1.PowerWorkloadStatus{
					WorkloadNodes: powerv1.WorkloadNode{
						Name:   testNode,
						CpuIds: tc.cpuIDs,
						Containers: []powerv1.Container{
							{ExclusiveCPUs: tc.cpuIDs, PodUID: types.UID(tc.podUID)},
						},
					},
				},
			}

			r, err := createWorkloadReconcilerObject([]runtime.Object{profile, workload})
			assert.NoError(t, err)

			// Setup environment
			host, teardown, err := fullDummySystem()
			assert.NoError(t, err)
			t.Cleanup(teardown)
			assert.NoError(t, host.GetSharedPool().SetCpuIDs(tc.cpuIDs))
			_, err = host.AddExclusivePool(tc.profileName)
			assert.NoError(t, err)
			r.PowerLibrary = host

			mgrmk := new(ScalingMgrMock)
			mgrmk.On("ManageCPUScaling", mock.Anything).Return().Once()
			r.CPUScalingManager = mgrmk

			dpdkmk := new(DPDKTelemetryClientMock)
			dpdkmk.On("ListConnections").Return(tc.initialConns).Once()
			for _, conn := range tc.initialConns {
				if conn.PodUID != tc.podUID {
					dpdkmk.On("CloseConnection", conn.PodUID).Return().Once()
				}
			}
			dpdkmk.On("CreateConnection", &scaling.DPDKTelemetryConnectionData{
				PodUID:      tc.podUID,
				WatchedCPUs: tc.cpuIDs,
			}).Return().Once()
			r.DPDKTelemetryClient = dpdkmk

			// Reconcile workload
			_, err = r.Reconcile(context.TODO(), reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      workload.Name,
					Namespace: IntelPowerNamespace,
				},
			})
			assert.NoError(t, err)

			// Verify scaling manager ManageCPUScaling was called and contains the correct values
			if assert.Equal(t, 1, len(mgrmk.Calls)) {
				actualScalingOpts := mgrmk.Calls[0].Arguments.Get(0).([]scaling.CPUScalingOpts)
				actualIDs := make([]uint, 0, len(actualScalingOpts))
				for _, o := range actualScalingOpts {
					actualIDs = append(actualIDs, o.CPU.GetID())
				}
				assert.ElementsMatch(t, tc.cpuIDs, actualIDs)
				assert.Equal(t, tc.sample, actualScalingOpts[0].SamplePeriod)
				assert.Equal(t, tc.cooldown, actualScalingOpts[0].CooldownPeriod)
				assert.Equal(t, tc.targetUsage, actualScalingOpts[0].TargetUsage)
				assert.Equal(t, tc.usageDiff, actualScalingOpts[0].AllowedUsageDifference)
				assert.Equal(t, tc.freqDiff*1000, actualScalingOpts[0].AllowedFrequencyDifference)
				assert.Equal(t, float64(tc.scalePct)/100, actualScalingOpts[0].ScaleFactor)

				cpu := host.GetAllCpus().ByID(tc.cpuIDs[0])
				minFreq, maxFreq := cpu.GetAbsMinMax()
				fallbackFreq := minFreq + (maxFreq-minFreq)*(uint(tc.fallbackPct))/100
				assert.Equal(t, int(fallbackFreq), actualScalingOpts[0].FallbackFreq)
			}

			dpdkmk.AssertExpectations(t)
			mgrmk.AssertExpectations(t)
		})
	}
}

func TestPowerWorkload_reconcileDPDKTelemetryClient(t *testing.T) {
	testcases := []struct {
		name          string
		existingConns []scaling.DPDKTelemetryConnectionData // connections from workload
		containers    []powerv1.Container                   // containers from workload
		expectCreates []scaling.DPDKTelemetryConnectionData // connections to create
		expectCloses  []string                              // podUIDs to close
	}{
		{
			name:          "Create new connections and merge per pod",
			existingConns: []scaling.DPDKTelemetryConnectionData{},
			containers: []powerv1.Container{
				{PodUID: types.UID("pod-1"), ExclusiveCPUs: []uint{1, 2}},
				{PodUID: types.UID("pod-1"), ExclusiveCPUs: []uint{4}},
				{PodUID: types.UID("pod-2"), ExclusiveCPUs: []uint{7}},
			},
			expectCreates: []scaling.DPDKTelemetryConnectionData{
				{PodUID: "pod-1", WatchedCPUs: []uint{1, 2, 4}},
				{PodUID: "pod-2", WatchedCPUs: []uint{7}},
			},
			expectCloses: nil,
		},
		{
			name: "Close stale and create missing, keep existing connections",
			existingConns: []scaling.DPDKTelemetryConnectionData{
				{PodUID: "pod-1", WatchedCPUs: []uint{9}},
				{PodUID: "pod-2", WatchedCPUs: []uint{5}},
			},
			containers: []powerv1.Container{
				{PodUID: types.UID("pod-2"), ExclusiveCPUs: []uint{5}},
				{PodUID: types.UID("pod-3"), ExclusiveCPUs: []uint{6}},
			},
			expectCreates: []scaling.DPDKTelemetryConnectionData{
				{PodUID: "pod-3", WatchedCPUs: []uint{6}},
			},
			expectCloses: []string{"pod-1"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			dpdkmk := new(DPDKTelemetryClientMock)
			dpdkmk.On("ListConnections").Return(tc.existingConns).Once()
			for _, podUID := range tc.expectCloses {
				dpdkmk.On("CloseConnection", podUID).Return().Once()
			}
			for _, createConn := range tc.expectCreates {
				dpdkmk.On("CreateConnection", mock.MatchedBy(func(d *scaling.DPDKTelemetryConnectionData) bool {
					return d.PodUID == createConn.PodUID && reflect.DeepEqual(d.WatchedCPUs, createConn.WatchedCPUs)
				})).Return().Once()
			}

			r := &PowerWorkloadReconciler{DPDKTelemetryClient: dpdkmk}
			r.reconcileDPDKTelemetryClient(tc.containers)
			dpdkmk.AssertExpectations(t)
		})
	}
}

func TestPowerWorkload_Reconcile_DetectCoresRemoved(t *testing.T) {
	orig := []uint{1, 2, 3, 4}
	updated := []uint{1, 2, 4, 5}

	expectedResult := []uint{3}
	result := detectCoresRemoved(orig, updated, &logr.Logger{})
	assert.ElementsMatch(t, result, expectedResult)
}

func TestPowerWorkload_Reconcile_DetectCoresAdded(t *testing.T) {
	orig := []uint{1, 2, 3, 4}
	updated := []uint{1, 2, 4, 5}

	expectedResult := []uint{5}
	result := detectCoresAdded(orig, updated, &logr.Logger{})
	assert.ElementsMatch(t, result, expectedResult)
}

func TestPowerWorkload_Reconcile_WrongNamespace(t *testing.T) {
	// ensure request for wrong namespace is ignored

	r, err := createWorkloadReconcilerObject([]runtime.Object{})
	if err != nil {
		t.Error(err)
		t.Fatalf("error creating reconciler object")
	}

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      "test-workload",
			Namespace: "MADE-UP",
		},
	}
	_, err = r.Reconcile(context.TODO(), req)
	assert.ErrorContains(t, err, "incorrect namespace")
}

func TestPowerWorkload_Reconcile_ClientErrs(t *testing.T) {
	// error getting power nodes
	testNode := "TestNode"
	workloadName := "performance-TestNode"
	pwrWorkloadObj := &powerv1.PowerWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName,
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerWorkloadSpec{
			Name:              "",
			AllCores:          true,
			ReservedCPUs:      []powerv1.ReservedSpec{{Cores: []uint{0, 1}}},
			PowerNodeSelector: map[string]string{"powernode": "selector"},
			PowerProfile:      "shared",
		},
		Status: powerv1.PowerWorkloadStatus{
			WorkloadNodes: powerv1.WorkloadNode{
				Name:   testNode,
				CpuIds: []uint{2, 3},
			},
		},
	}

	nodeObj := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   testNode,
			Labels: map[string]string{"powernode": "selector"},
		},
		Status: corev1.NodeStatus{
			Capacity: map[corev1.ResourceName]resource.Quantity{
				CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
			},
		},
	}
	nodesObj := &corev1.NodeList{
		Items: []corev1.Node{*nodeObj},
	}

	t.Setenv("NODE_NAME", testNode)

	r, err := createWorkloadReconcilerObject([]runtime.Object{})
	if err != nil {
		t.Error(err)
		t.Fatalf("error creating the reconciler object")
	}

	mkwriter := new(mockResourceWriter)
	mkwriter.On("Update", mock.Anything, mock.Anything).Return(nil)
	mkcl := new(errClient)
	mkcl.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		node := args.Get(2).(*powerv1.PowerWorkload)
		*node = *pwrWorkloadObj
	})
	mkcl.On("List", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("client list error"))
	mkcl.On("Status").Return(mkwriter)
	r.Client = mkcl
	nodemk := new(hostMock)

	r.PowerLibrary = nodemk

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      workloadName,
			Namespace: IntelPowerNamespace,
		},
	}

	_, err = r.Reconcile(context.TODO(), req)
	assert.ErrorContains(t, err, "client list error")
	nodemk.AssertExpectations(t)
	// error deleting duplicate shared pool
	r, err = createWorkloadReconcilerObject([]runtime.Object{pwrWorkloadObj, nodesObj})
	assert.Nil(t, err)
	r.PowerLibrary = new(hostMock)
	mkcl = new(errClient)
	mkcl.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		wload := args.Get(2).(*powerv1.PowerWorkload)
		*wload = *pwrWorkloadObj
	})
	mkcl.On("List", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		node := args.Get(1).(*corev1.NodeList)
		*node = *nodesObj
	})
	mkcl.On("Delete", mock.Anything, mock.Anything).Return(fmt.Errorf("client delete error"))
	mkcl.On("Status").Return(mkwriter)
	r.Client = mkcl

	sharedPowerWorkloadName = "shared"
	req.Name = workloadName
	_, err = r.Reconcile(context.TODO(), req)
	assert.ErrorContains(t, err, "client delete error")
}

// uses dummy sysfs so must be run in isolation from other fuzzers
// go test -fuzz FuzzPowerWorkloadController -run=FuzzPowerWorkloadController -parallel=1
func FuzzPowerWorkloadController(f *testing.F) {
	f.Add("TestNode", "performance", 3600, 3200, "performance", "powersave", false, false, uint(44), "performance", uint(1), uint(5), uint(2), uint(7))
	f.Fuzz(func(t *testing.T, nodeName string, prof string, maxVal int, minVal int, epp string, governor string, shared bool, allcores bool, reservedCore uint, reservedProfile string, wCore1 uint, wCore2 uint, nCore1, nCore2 uint) {
		nodeName = strings.ReplaceAll(nodeName, " ", "")
		nodeName = strings.ReplaceAll(nodeName, "\t", "")
		nodeName = strings.ReplaceAll(nodeName, "\000", "")
		if len(nodeName) == 0 {
			return
		}
		t.Setenv("NODE_NAME", nodeName)

		clientObjs := []runtime.Object{
			&powerv1.PowerProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      prof,
					Namespace: IntelPowerNamespace,
				},
				Spec: powerv1.PowerProfileSpec{
					Name: prof,
					PStates: powerv1.PStatesConfig{
						Max:      &intstr.IntOrString{Type: intstr.Int, IntVal: int32(maxVal)},
						Min:      &intstr.IntOrString{Type: intstr.Int, IntVal: int32(minVal)},
						Epp:      epp,
						Governor: governor,
					},
					Shared: shared,
				},
			},
			&powerv1.PowerWorkload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      prof + "-" + nodeName,
					Namespace: IntelPowerNamespace,
				},
				Spec: powerv1.PowerWorkloadSpec{
					Name:     prof + "-" + nodeName,
					AllCores: allcores,
					ReservedCPUs: []powerv1.ReservedSpec{
						{
							Cores:        []uint{reservedCore},
							PowerProfile: reservedProfile,
						},
					},
					PowerNodeSelector: map[string]string{"kubernetes.io/hostname": nodeName},
					PowerProfile:      prof,
				},
				Status: powerv1.PowerWorkloadStatus{
					WorkloadNodes: powerv1.WorkloadNode{
						Name: nodeName,
						Containers: []powerv1.Container{
							{
								Name:          "test-container-1",
								ExclusiveCPUs: []uint{wCore1, wCore2},
								PowerProfile:  prof,
							},
						},
						CpuIds: []uint{nCore1, nCore2},
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

		r, err := createWorkloadReconcilerObject(clientObjs)
		assert.Nil(t, err)
		host, teardown, err := fullDummySystem()
		assert.Nil(t, err)
		defer teardown()
		r.PowerLibrary = host
		host.GetReservedPool().SetCpuIDs([]uint{})
		host.AddExclusivePool(prof)
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      prof + "-" + nodeName,
				Namespace: IntelPowerNamespace,
			},
		}

		r.Reconcile(context.TODO(), req)

	})
}

func TestPowerWorkload_Reconcile_SetupPass(t *testing.T) {
	r, err := createPowerNodeReconcilerObject([]runtime.Object{})
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("SetFields", mock.Anything).Return(nil)
	mgr.On("Add", mock.Anything).Return(nil)
	mgr.On("GetCache").Return(new(cacheMk))
	err = (&PowerWorkloadReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Nil(t, err)

}
func TestPowerWorkload_Reconcile_SetupFail(t *testing.T) {
	r, err := createPowerNodeReconcilerObject([]runtime.Object{})
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("Add", mock.Anything).Return(fmt.Errorf("setup fail"))

	err = (&PowerWorkloadReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Error(t, err)

}

func TestPowerWorkload_ValidateNodeSelectorAndProfileMatching(t *testing.T) {
	testNode := "TestNode"

	baseNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNode,
			Labels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
				"env":       "production",
			},
		},
		Status: corev1.NodeStatus{
			Capacity: map[corev1.ResourceName]resource.Quantity{
				CPUResource: *resource.NewQuantity(42, resource.DecimalSI),
			},
		},
	}

	performanceProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "performance",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "performance",
			NodeSelector: powerv1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "worker",
					},
				},
			},
		},
	}

	restrictedProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restricted",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "restricted",
			NodeSelector: powerv1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "gpu-node",
					},
				},
			},
		},
	}

	sharedProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name:   "shared",
			Shared: true,
			// No node selector - should apply to all nodes
		},
	}

	testCases := []struct {
		name              string
		workloadSpec      powerv1.PowerWorkloadSpec
		nodeLabels        map[string]string
		profileObjects    []runtime.Object
		mockSetup         func() *hostMock
		expectError       bool
		expectRequeueTime bool
		errorContains     string
	}{
		{
			name: "Matching node selector and available profiles",
			workloadSpec: powerv1.PowerWorkloadSpec{
				Name:         "shared-" + testNode,
				AllCores:     true,
				PowerProfile: "shared",
				ReservedCPUs: []powerv1.ReservedSpec{
					{Cores: []uint{0, 1}, PowerProfile: "performance"},
				},
				PowerNodeSelector: map[string]string{"node-type": "worker"},
			},
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			profileObjects: []runtime.Object{performanceProfile, sharedProfile},
			mockSetup: func() *hostMock {
				template := mocktemplate()
				return template.node
			},
			expectError: false,
		},
		{
			name: "Non-matching node selector",
			workloadSpec: powerv1.PowerWorkloadSpec{
				Name:              "shared-" + testNode,
				AllCores:          true,
				PowerProfile:      "restricted",
				PowerNodeSelector: map[string]string{"node-type": "worker"},
			},
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			profileObjects: []runtime.Object{restrictedProfile},
			mockSetup: func() *hostMock {
				mock := new(hostMock)
				pool := new(poolMock)
				mock.On("GetExclusivePool", "restricted").Return(pool)
				pool.On("GetPowerProfile").Return(new(profMock))
				// Mock GetSharedPool to avoid panic
				mock.On("GetSharedPool").Return(new(poolMock))
				return mock
			},
			expectError:       false, // Controller returns nil error, not an actual error
			expectRequeueTime: true,
			// Note: restricted PowerProfile node selector is "node-type": "gpu-node" which doesn't match node labels
		},
		{
			name: "Missing profile in cluster",
			workloadSpec: powerv1.PowerWorkloadSpec{
				Name:              "shared-" + testNode,
				AllCores:          true,
				PowerProfile:      "nonexistent",
				PowerNodeSelector: map[string]string{"node-type": "worker"},
			},
			nodeLabels: map[string]string{
				"node-type": "worker",
			},
			profileObjects: []runtime.Object{},
			mockSetup: func() *hostMock {
				return new(hostMock)
			},
			expectError:       false, // Returns nil error but requeues
			expectRequeueTime: true,
		},
		{
			name: "Profile exists but pool unavailable",
			workloadSpec: powerv1.PowerWorkloadSpec{
				Name:              "shared-" + testNode,
				AllCores:          true,
				PowerProfile:      "performance",
				PowerNodeSelector: map[string]string{"node-type": "worker"},
			},
			nodeLabels: map[string]string{
				"node-type": "worker",
			},
			profileObjects: []runtime.Object{performanceProfile},
			mockSetup: func() *hostMock {
				mock := new(hostMock)
				mock.On("GetExclusivePool", "performance").Return(nil)
				return mock
			},
			expectError:       false,
			expectRequeueTime: true,
		},
		{
			name: "Reserved CPU with empty profile name",
			workloadSpec: powerv1.PowerWorkloadSpec{
				Name:         "shared-" + testNode,
				AllCores:     true,
				PowerProfile: "shared",
				ReservedCPUs: []powerv1.ReservedSpec{
					{Cores: []uint{0, 1}, PowerProfile: ""}, // Empty profile should be allowed
				},
				PowerNodeSelector: map[string]string{"node-type": "worker"},
			},
			nodeLabels: map[string]string{
				"node-type": "worker",
			},
			profileObjects: []runtime.Object{sharedProfile},
			mockSetup: func() *hostMock {
				template := mocktemplate()
				return template.node
			},
			expectError: false,
		},
		{
			name: "Multiple reserved CPUs with mixed profile availability",
			workloadSpec: powerv1.PowerWorkloadSpec{
				Name:         "shared-" + testNode,
				AllCores:     true,
				PowerProfile: "shared",
				ReservedCPUs: []powerv1.ReservedSpec{
					{Cores: []uint{0, 1}, PowerProfile: "performance"}, // Available
					{Cores: []uint{2, 3}, PowerProfile: "nonexistent"}, // Not available
				},
				PowerNodeSelector: map[string]string{"node-type": "worker"},
			},
			nodeLabels: map[string]string{
				"node-type": "worker",
			},
			profileObjects: []runtime.Object{sharedProfile, performanceProfile},
			mockSetup: func() *hostMock {
				mock := new(hostMock)
				sharedPool := new(poolMock)
				perfPool := new(poolMock)
				prof := new(profMock)

				mock.On("GetExclusivePool", "shared").Return(sharedPool)
				mock.On("GetExclusivePool", "performance").Return(perfPool)
				mock.On("GetExclusivePool", "nonexistent").Return(nil)
				mock.On("GetSharedPool").Return(sharedPool)

				sharedPool.On("GetPowerProfile").Return(prof)
				perfPool.On("GetPowerProfile").Return(prof)

				return mock
			},
			expectError:       false,
			expectRequeueTime: true,
		},
		{
			name: "No node selector - should apply to any node",
			workloadSpec: powerv1.PowerWorkloadSpec{
				Name:         "shared-" + testNode,
				AllCores:     true,
				PowerProfile: "shared",
				// No PowerNodeSelector specified
			},
			nodeLabels: map[string]string{
				"node-type": "worker",
			},
			profileObjects: []runtime.Object{sharedProfile},
			mockSetup: func() *hostMock {
				template := mocktemplate()
				return template.node
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NODE_NAME", testNode)

			// Reset global state before each test case
			sharedPowerWorkloadName = ""

			// Create node with specified labels
			node := baseNode.DeepCopy()
			node.Labels = tc.nodeLabels

			// Create workload with test spec
			workload := &powerv1.PowerWorkload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "shared-" + testNode,
					Namespace: IntelPowerNamespace,
				},
				Spec: tc.workloadSpec,
			}

			clientObjs := []runtime.Object{workload, node}
			clientObjs = append(clientObjs, tc.profileObjects...)

			r, err := createWorkloadReconcilerObject(clientObjs)
			assert.NoError(t, err)

			r.PowerLibrary = tc.mockSetup()

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      workload.Name,
					Namespace: IntelPowerNamespace,
				},
			}

			result, err := r.Reconcile(context.TODO(), req)

			if tc.expectError {
				assert.Error(t, err)
				if tc.errorContains != "" {
					assert.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				assert.NoError(t, err)
			}

			if tc.expectRequeueTime {
				assert.True(t, result.RequeueAfter > 0)
			}
		})
	}
}
