//go:build envtest

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
	"testing"

	powerv1alpha1 "github.com/cluster-power-manager/cluster-power-manager/api/v1alpha1"
	power "github.com/intel/power-optimization-library/pkg/power"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// createNodeConfigReconcilerWithEnvTest creates a PowerNodeConfigReconciler using a real envtest client.
// PowerLibrary is mocked since it depends on node-local hardware.
func createNodeConfigReconcilerWithEnvTest(t *testing.T, cl client.Client, mockHost power.Host) *PowerNodeConfigReconciler {
	t.Helper()
	s := scheme.Scheme
	_ = powerv1alpha1.AddToScheme(s)
	return &PowerNodeConfigReconciler{
		Client:       cl,
		Log:          ctrl.Log.WithName("testing"),
		Scheme:       s,
		PowerLibrary: mockHost,
	}
}

// setupMockHostForSharedPool creates a mock host that supports shared pool configuration
// with no reserved CPUs.
func setupMockHostForSharedPool(profileName string, sharedCPUs []uint) *hostMock {
	h := new(hostMock)
	ep := new(poolMock)
	sp := createMockPoolWithCPUs(sharedCPUs)
	rp := new(poolMock)
	pm := new(profMock)
	h.On("GetExclusivePool", profileName).Return(ep)
	h.On("GetSharedPool").Return(sp)
	h.On("GetReservedPool").Return(rp)
	h.On("GetAllExclusivePools").Return(&power.PoolList{})
	ep.On("GetPowerProfile").Return(pm)
	sp.On("SetPowerProfile", pm).Return(nil)
	rp.On("SetCpuIDs", []uint{}).Return(nil)
	return h
}

func TestEnvTest_Reconcile_NoMatchingConfig(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)

	// Create node with labels that won't match any config.
	createTestNode(t, cl, nodeName, map[string]string{"arch": "amd64"})
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))

	// Create a config that targets a different node group.
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("shared-prof", true)))
	createTestPowerNodeConfig(t, cl, "other-config", "shared-prof", map[string]string{"arch": "arm64"}, nil)

	h := new(hostMock)
	r := createNodeConfigReconcilerWithEnvTest(t, cl, h)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "other-config", Namespace: PowerNamespace}})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// PowerNodeState should have no shared pool status.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}, pns))
	assert.Nil(t, pns.Status.CPUPools, "no pools should be configured when no config matches")
}

func TestEnvTest_Reconcile_SingleMatchingConfig(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)

	createTestNode(t, cl, nodeName, map[string]string{"arch": "amd64"})
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("shared-prof", true)))
	createTestPowerNodeConfig(t, cl, "my-config", "shared-prof", map[string]string{"arch": "amd64"}, nil)

	h := setupMockHostForSharedPool("shared-prof", []uint{2, 3, 4, 5})
	r := createNodeConfigReconcilerWithEnvTest(t, cl, h)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "my-config", Namespace: PowerNamespace}})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify PowerNodeState has shared pool status.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.NotNil(t, pns.Status.CPUPools.Shared)
	assert.Equal(t, "shared-prof", pns.Status.CPUPools.Shared.PowerProfile)
	assert.Equal(t, "my-config", pns.Status.CPUPools.Shared.PowerNodeConfig)
	assert.Equal(t, "2-5", pns.Status.CPUPools.Shared.CPUIDs)
	assert.Empty(t, pns.Status.CPUPools.Shared.Errors)
	assert.Empty(t, pns.Status.CPUPools.Reserved, "no reserved pools should be configured")
}

func TestEnvTest_Reconcile_WithReservedCPUs(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)

	createTestNode(t, cl, nodeName, map[string]string{"arch": "amd64"})
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("shared-prof", true)))
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("perf-prof", false)))
	createTestPowerNodeConfig(t, cl, "my-config", "shared-prof",
		map[string]string{"arch": "amd64"},
		[]powerv1alpha1.ReservedSpec{{Cores: []uint{0, 1}, PowerProfile: "perf-prof"}})

	// Mock setup for shared + reserved pool configuration.
	h := new(hostMock)
	sharedPoolEP := new(poolMock) // pool for shared profile
	perfPoolEP := new(poolMock)   // pool for reserved profile
	sp := createMockPoolWithCPUs([]uint{2, 3, 4, 5})
	rp := new(poolMock)
	pseudoPool := new(poolMock)
	sharedPM := new(profMock)
	perfPM := new(profMock)

	// configureSharedPool mocks
	h.On("GetExclusivePool", "shared-prof").Return(sharedPoolEP)
	h.On("GetSharedPool").Return(sp)
	h.On("GetReservedPool").Return(rp)
	sharedPoolEP.On("GetPowerProfile").Return(sharedPM)
	sp.On("SetPowerProfile", sharedPM).Return(nil)
	rp.On("SetCpuIDs", []uint{}).Return(nil)

	// configureReservedPools mocks
	h.On("GetAllExclusivePools").Return(&power.PoolList{})
	sp.On("MoveCpuIDs", []uint{0, 1}).Return(nil)
	h.On("AddExclusivePool", fmt.Sprintf("%s-reserved-%v", nodeName, []uint{0, 1})).Return(pseudoPool, nil)
	h.On("GetExclusivePool", "perf-prof").Return(perfPoolEP)
	perfPoolEP.On("GetPowerProfile").Return(perfPM)
	pseudoPool.On("SetPowerProfile", perfPM).Return(nil)
	pseudoPool.On("SetCpuIDs", []uint{0, 1}).Return(nil)

	r := createNodeConfigReconcilerWithEnvTest(t, cl, h)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "my-config", Namespace: PowerNamespace}})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify PowerNodeState has both shared and reserved pool status.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.CPUPools)

	// Verify shared pool.
	require.NotNil(t, pns.Status.CPUPools.Shared)
	assert.Equal(t, "shared-prof", pns.Status.CPUPools.Shared.PowerProfile)
	assert.Equal(t, "my-config", pns.Status.CPUPools.Shared.PowerNodeConfig)
	assert.Equal(t, "2-5", pns.Status.CPUPools.Shared.CPUIDs)

	// Verify reserved pool.
	require.Len(t, pns.Status.CPUPools.Reserved, 1)
	assert.Equal(t, "my-config", pns.Status.CPUPools.Reserved[0].PowerNodeConfig)
	require.Len(t, pns.Status.CPUPools.Reserved[0].PowerProfileCPUs, 1)
	assert.Equal(t, "perf-prof", pns.Status.CPUPools.Reserved[0].PowerProfileCPUs[0].PowerProfile)
	assert.Equal(t, "0-1", pns.Status.CPUPools.Reserved[0].PowerProfileCPUs[0].CPUIDs)
	assert.Empty(t, pns.Status.CPUPools.Reserved[0].PowerProfileCPUs[0].Errors)
}

func TestEnvTest_Reconcile_ConflictResolution_OldestWins(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)

	createTestNode(t, cl, nodeName, map[string]string{"arch": "amd64"})
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("shared-prof-a", true)))
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("shared-prof-b", true)))

	// Create two configs. With equal timestamps, tiebreaker is alphabetical by name.
	// "config-a" < "config-b", so "config-a" wins.
	createTestPowerNodeConfig(t, cl, "config-a", "shared-prof-a", map[string]string{"arch": "amd64"}, nil)
	createTestPowerNodeConfig(t, cl, "config-b", "shared-prof-b", map[string]string{"arch": "amd64"}, nil)

	h := setupMockHostForSharedPool("shared-prof-a", []uint{2, 3, 4, 5})
	r := createNodeConfigReconcilerWithEnvTest(t, cl, h)

	// Reconcile triggered by config-b — config-a should still win (oldest/alphabetical).
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "config-b", Namespace: PowerNamespace}})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify config-a was selected.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.NotNil(t, pns.Status.CPUPools.Shared)
	assert.Equal(t, "shared-prof-a", pns.Status.CPUPools.Shared.PowerProfile)
	assert.Equal(t, "config-a", pns.Status.CPUPools.Shared.PowerNodeConfig)
	// Should have a conflict error for config-b.
	assert.NotEmpty(t, pns.Status.CPUPools.Shared.Errors)
	assert.Contains(t, pns.Status.CPUPools.Shared.Errors[0], "config-b")
}

func TestEnvTest_Reconcile_ProfileValidationFailure(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)

	createTestNode(t, cl, nodeName, map[string]string{"arch": "amd64"})
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))
	// Create profile but NOT marked as shared.
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("not-shared-prof", false)))
	createTestPowerNodeConfig(t, cl, "bad-config", "not-shared-prof", map[string]string{"arch": "amd64"}, nil)

	h := new(hostMock)
	h.On("GetExclusivePool", "not-shared-prof").Return(new(poolMock))
	r := createNodeConfigReconcilerWithEnvTest(t, cl, h)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "bad-config", Namespace: PowerNamespace},
	})
	assert.NoError(t, err)
	assert.Equal(t, queuetime, result.RequeueAfter, "should requeue on validation failure")

	// Verify the error is recorded in PowerNodeState status.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.NotNil(t, pns.Status.CPUPools.Shared)
	assert.NotEmpty(t, pns.Status.CPUPools.Shared.Errors)
	assert.Contains(t, pns.Status.CPUPools.Shared.Errors[0], "not a shared profile")
}

func TestEnvTest_Reconcile_CleanupWhenConfigDeleted(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)

	createTestNode(t, cl, nodeName, map[string]string{"arch": "amd64"})
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("shared-prof", true)))
	createTestPowerNodeConfig(t, cl, "my-config", "shared-prof", map[string]string{"arch": "amd64"}, nil)

	h := setupMockHostForSharedPool("shared-prof", []uint{2, 3, 4, 5})
	r := createNodeConfigReconcilerWithEnvTest(t, cl, h)

	// First reconcile — apply the config.
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "my-config", Namespace: PowerNamespace}})
	require.NoError(t, err)

	// Verify status was written.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.NotNil(t, pns.Status.CPUPools.Shared)

	// Delete the config.
	config := &powerv1alpha1.PowerNodeConfig{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "my-config", Namespace: PowerNamespace}, config))
	require.NoError(t, cl.Delete(ctx, config))

	// Set up a fresh mock for cleanup — the cleanup path calls GetSharedPool().Cpus(),
	// GetAllExclusivePools(), and GetReservedPool().MoveCpus().
	h2 := new(hostMock)
	sp2 := createMockPoolWithCPUs([]uint{2, 3, 4, 5})
	rp2 := new(poolMock)
	rp2.On("MoveCpus", mock.Anything).Return(nil)
	h2.On("GetSharedPool").Return(sp2)
	h2.On("GetReservedPool").Return(rp2)
	h2.On("GetAllExclusivePools").Return(&power.PoolList{})
	r.PowerLibrary = h2

	// Second reconcile — should clean up.
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "my-config", Namespace: PowerNamespace}})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify shared pool status was cleared after cleanup.
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}, pns))
	if pns.Status.CPUPools != nil {
		assert.Nil(t, pns.Status.CPUPools.Shared, "shared pool status should be cleared after cleanup")
		assert.Empty(t, pns.Status.CPUPools.Reserved, "reserved pool status should be cleared after cleanup")
	}
}

func TestEnvTest_Reconcile_PowerNodeStateNotFound(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)

	createTestNode(t, cl, nodeName, map[string]string{"arch": "amd64"})
	// Deliberately do NOT create the PowerNodeState.
	require.NoError(t, cl.Create(ctx, newTestPowerProfile("shared-prof", true)))
	createTestPowerNodeConfig(t, cl, "my-config", "shared-prof", map[string]string{"arch": "amd64"}, nil)

	h := new(hostMock)
	r := createNodeConfigReconcilerWithEnvTest(t, cl, h)

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "my-config", Namespace: PowerNamespace}})
	// Should requeue with info log, not error.
	assert.NoError(t, err)
	assert.Equal(t, queuetime, result.RequeueAfter, "should requeue when PowerNodeState not found")
}
