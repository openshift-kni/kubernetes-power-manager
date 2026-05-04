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

	powerv1alpha1 "github.com/openshift-kni/kubernetes-power-manager/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// newTestUncoreReconciler creates an UncoreReconciler with just Client and Log
// for testing SSA status methods. PowerLibrary and Scheme are not needed by
// updateUncoreInPowerNodeState and removeUncoreFromPowerNodeState.
func newTestUncoreReconciler(cl client.Client) *UncoreReconciler {
	return &UncoreReconciler{
		Client: cl,
		Log:    testLogger(),
	}
}

func TestSSA_UncoreFieldManagerOwnership(t *testing.T) {
	cl := envTestClient
	r := newTestUncoreReconciler(cl)

	ctx := context.TODO()
	nodeName := "test-node-ownership"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)

	// Apply uncore status using production code.
	logger := testLogger()
	err := r.updateUncoreInPowerNodeState(ctx, nodeName, "uncore-config", "SysMin: 1200000, SysMax: 2400000", nil, &logger)
	require.NoError(t, err)

	// Verify status was written.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "uncore-config", pns.Status.Uncore.Name)
	assert.Equal(t, "SysMin: 1200000, SysMax: 2400000", pns.Status.Uncore.Config)
	assert.Empty(t, pns.Status.Uncore.Errors)

	// Verify SSA field manager ownership.
	managers := map[string]bool{}
	for _, mf := range pns.ManagedFields {
		managers[mf.Manager] = true
	}
	assert.True(t, managers[FieldOwnerUncoreController], "expected field manager %s", FieldOwnerUncoreController)
}

func TestSSA_UncoreRemovalPrunesField(t *testing.T) {
	cl := envTestClient
	r := newTestUncoreReconciler(cl)

	ctx := context.TODO()
	nodeName := "test-node-removal"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)

	// In production, the PowerConfig controller writes NodeInfo to PowerNodeState before
	// any other controller touches the status. This anchors the status so it's never null.
	applyNodeInfo(t, cl, pnsName)

	// Apply uncore status, then remove it using production code.
	logger := testLogger()
	err := r.updateUncoreInPowerNodeState(ctx, nodeName, "uncore-config", "SysMin: 1200000, SysMax: 2400000", nil, &logger)
	require.NoError(t, err)
	err = r.removeUncoreFromPowerNodeState(ctx, nodeName, &logger)
	require.NoError(t, err)

	// Verify the Uncore field was pruned while NodeInfo is preserved.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	assert.Nil(t, pns.Status.Uncore, "Uncore field should be pruned after removal")
	assert.NotNil(t, pns.Status.NodeInfo, "NodeInfo should be preserved as SSA anchor")
}

func TestSSA_UncoreDoesNotInterfereWithOtherControllers(t *testing.T) {
	cl := envTestClient
	r := newTestUncoreReconciler(cl)

	ctx := context.TODO()
	nodeName := "test-node-interference"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)

	// First, add a profile entry using the PowerProfile controller's SSA.
	logger := testLogger()
	err := addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("performance", false), nil, &logger)
	require.NoError(t, err)

	// Then apply uncore status using production code.
	err = r.updateUncoreInPowerNodeState(ctx, nodeName, "uncore-config", "SysMin: 1200000, SysMax: 2400000", nil, &logger)
	require.NoError(t, err)

	// Verify both coexist — each controller owns its own fields.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))

	// Profile should still be present.
	require.Len(t, pns.Status.PowerProfiles, 1)
	assert.Equal(t, "performance", pns.Status.PowerProfiles[0].Name)

	// Uncore should be present.
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "uncore-config", pns.Status.Uncore.Name)

	// Now remove uncore using production code — profile should be preserved.
	err = r.removeUncoreFromPowerNodeState(ctx, nodeName, &logger)
	require.NoError(t, err)

	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	assert.Nil(t, pns.Status.Uncore, "Uncore should be pruned")
	require.Len(t, pns.Status.PowerProfiles, 1, "Profile should be preserved after uncore removal")
	assert.Equal(t, "performance", pns.Status.PowerProfiles[0].Name)
}

func TestSSA_UncoreUpdateOverwritesPrevious(t *testing.T) {
	cl := envTestClient
	r := newTestUncoreReconciler(cl)

	ctx := context.TODO()
	nodeName := "test-node-update"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)

	// Apply initial config using production code.
	logger := testLogger()
	err := r.updateUncoreInPowerNodeState(ctx, nodeName, "config-a", "SysMin: 1200000, SysMax: 2400000", nil, &logger)
	require.NoError(t, err)

	// Apply different config — same field manager, should overwrite.
	err = r.updateUncoreInPowerNodeState(ctx, nodeName, "config-b", "Package 0: Min 1300000, Max 2200000",
		[]string{"conflicting Uncore: config-a"}, &logger)
	require.NoError(t, err)

	// Verify the latest config is present.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "config-b", pns.Status.Uncore.Name)
	assert.Equal(t, "Package 0: Min 1300000, Max 2200000", pns.Status.Uncore.Config)
	assert.Contains(t, pns.Status.Uncore.Errors, "conflicting Uncore: config-a")
}

// --- Reconcile envtest tests ---
// These tests exercise the full Reconcile path through a real API server,
// validating SSA status writes, conflict resolution, and cleanup behavior
// that unit tests with fake clients cannot properly cover.

// createEnvTestUncore creates an Uncore CR in the envtest API server.
func createEnvTestUncore(t *testing.T, cl client.Client, name string, spec powerv1alpha1.UncoreSpec, matchLabels map[string]string) {
	u := &powerv1alpha1.Uncore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PowerNamespace,
		},
		Spec: spec,
	}
	if matchLabels != nil {
		u.Spec.NodeSelector = powerv1alpha1.NodeSelector{
			LabelSelector: metav1.LabelSelector{MatchLabels: matchLabels},
		}
	}
	require.NoError(t, cl.Create(context.TODO(), u))
}

// deleteEnvTestUncore deletes an Uncore CR from the envtest API server.
func deleteEnvTestUncore(t *testing.T, cl client.Client, name string) {
	u := &powerv1alpha1.Uncore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PowerNamespace,
		},
	}
	require.NoError(t, cl.Delete(context.TODO(), u))
}

// deleteEnvTestNode deletes a Node from the envtest API server.
func deleteEnvTestNode(t *testing.T, cl client.Client, name string) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := cl.Delete(context.TODO(), node); err != nil {
		t.Logf("cleanup: failed to delete node %s: %v", name, err)
	}
}

// Test 1: Single Uncore CR end-to-end reconciliation
func TestEnvTest_UncoreReconcile_SingleCR(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-single"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	// Setup: node, PowerNodeState with NodeInfo, Uncore CR.
	createTestNode(t, cl, nodeName, map[string]string{"feature.node.kubernetes.io/uncore": "true"})
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create an Uncore CR with system wide min and max frequencies.
	// Use values different from hardware defaults (1200000/2400000) to verify configuration.
	sysMin := uint(1400000)
	sysMax := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-sys-uncore", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax},
		map[string]string{"feature.node.kubernetes.io/uncore": "true"})
	defer deleteEnvTestUncore(t, cl, "envtest-sys-uncore")

	// Reconcile and verify.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-sys-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2200000", "1400000"))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "envtest-sys-uncore", pns.Status.Uncore.Name)
	assert.Equal(t, "SysMin: 1400000, SysMax: 2200000", pns.Status.Uncore.Config)
	assert.Empty(t, pns.Status.Uncore.Errors)
	assert.NotNil(t, pns.Status.NodeInfo, "NodeInfo should be preserved")
}

// Test 2: Conflict resolution — oldest wins, conflicts reported in status
func TestEnvTest_UncoreReconcile_ConflictResolution(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-conflict"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	createTestNode(t, cl, nodeName, map[string]string{"zone": "us-east"})
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	sysMin := uint(1400000)
	sysMax := uint(2200000)
	// Both Uncores match the same node. The API server assigns creation timestamps,
	// so the first created will be older.
	createEnvTestUncore(t, cl, "envtest-conflict-a", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax},
		map[string]string{"zone": "us-east"})
	defer deleteEnvTestUncore(t, cl, "envtest-conflict-a")

	createEnvTestUncore(t, cl, "envtest-conflict-b", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax},
		map[string]string{"zone": "us-east"})
	defer deleteEnvTestUncore(t, cl, "envtest-conflict-b")

	// Reconcile — either CR name triggers a full re-evaluation.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-conflict-a", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2200000", "1400000"))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	// Oldest (envtest-conflict-a) should win since there's no prior active config.
	assert.Equal(t, "envtest-conflict-a", pns.Status.Uncore.Name)
	assert.NotEmpty(t, pns.Status.Uncore.Errors, "conflict error should be reported")
	assert.Contains(t, pns.Status.Uncore.Errors[0], "envtest-conflict-b")
}

// Test 3: Uncore deletion cleans up status
func TestEnvTest_UncoreReconcile_Deletion(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-deletion"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	createTestNode(t, cl, nodeName, map[string]string{"feature.node.kubernetes.io/uncore": "true"})
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create an Uncore CR with system wide min and max frequencies.
	sysMin := uint(1400000)
	sysMax := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-del-uncore", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax},
		map[string]string{"feature.node.kubernetes.io/uncore": "true"})

	// Reconcile — status should be written.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-del-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2200000", "1400000"))

	// Delete the Uncore CR and reconcile again.
	deleteEnvTestUncore(t, cl, "envtest-del-uncore")
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-del-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)

	// Status should be cleaned up, NodeInfo preserved.
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	assert.Nil(t, pns.Status.Uncore, "Uncore status should be removed after CR deletion")
	assert.NotNil(t, pns.Status.NodeInfo, "NodeInfo should be preserved")
	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2400000", "1200000"))
}

// Test 4: Non-matching nodeSelector — no status written, then labels updated to match
func TestEnvTest_UncoreReconcile_NonMatchingThenMatch(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-nomatch"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	// Node does NOT have the label the Uncore CR selects.
	createTestNode(t, cl, nodeName, map[string]string{"zone": "us-west"})
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create an Uncore CR with system wide min and max frequencies.
	sysMin := uint(1400000)
	sysMax := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-nomatch-uncore", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax},
		map[string]string{"zone": "us-east"})
	defer deleteEnvTestUncore(t, cl, "envtest-nomatch-uncore")

	// Reconcile — no match, no status.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-nomatch-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	assert.Nil(t, pns.Status.Uncore, "No uncore status should be written for non-matching node")

	// Update node labels to match.
	node := &corev1.Node{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: nodeName}, node))
	node.Labels["zone"] = "us-east"
	require.NoError(t, cl.Update(context.TODO(), node))

	// Reconcile again — now it should match.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-nomatch-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2200000", "1400000"))

	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore, "Uncore status should appear after labels match")
	assert.Equal(t, "envtest-nomatch-uncore", pns.Status.Uncore.Name)
}

// Test 5: Matching Uncore applied, then node labels changed so it no longer matches
func TestEnvTest_UncoreReconcile_LabelChangeRemovesMatch(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-labeldrop"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	// Node has matching label.
	createTestNode(t, cl, nodeName, map[string]string{"zone": "us-east"})
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create an Uncore CR with system wide min and max frequencies.
	sysMin := uint(1400000)
	sysMax := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-labeldrop-uncore", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax},
		map[string]string{"zone": "us-east"})
	defer deleteEnvTestUncore(t, cl, "envtest-labeldrop-uncore")

	// Reconcile — matches, status written.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-labeldrop-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2200000", "1400000"))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "envtest-labeldrop-uncore", pns.Status.Uncore.Name)

	// Change node labels so the Uncore CR no longer matches.
	node := &corev1.Node{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: nodeName}, node))
	node.Labels["zone"] = "eu-west"
	require.NoError(t, cl.Update(context.TODO(), node))

	// Reconcile again — no match, status should be cleaned up.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-labeldrop-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	assert.Nil(t, pns.Status.Uncore, "Uncore status should be removed after label change")
	assert.NotNil(t, pns.Status.NodeInfo, "NodeInfo should be preserved")
	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2400000", "1200000"))
}

// Test 6: Die-level tuning
func TestEnvTest_UncoreReconcile_DieTuning(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-die"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	createTestNode(t, cl, nodeName, nil)
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create an Uncore CR with die-level tuning.
	pkg := uint(0)
	die := uint(1)
	min := uint(1300000)
	max := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-die-uncore", powerv1alpha1.UncoreSpec{
		DieSelectors: &[]powerv1alpha1.DieSelector{
			{Package: &pkg, Die: &die, Min: &min, Max: &max},
		},
	}, nil)
	defer deleteEnvTestUncore(t, cl, "envtest-die-uncore")

	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-die-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "01", fmt.Sprint(max), fmt.Sprint(min)))
	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2400000", "1200000"))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "Package 0 Die 1: Min 1300000, Max 2200000", pns.Status.Uncore.Config)
}

// Test 7: Package-level tuning — all dies in the package get the config
func TestEnvTest_UncoreReconcile_PackageTuning(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-pkg"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	createTestNode(t, cl, nodeName, nil)
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create an Uncore CR with die-level tuning.
	pkg := uint(0)
	min := uint(1400000)
	max := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-pkg-uncore", powerv1alpha1.UncoreSpec{
		DieSelectors: &[]powerv1alpha1.DieSelector{
			{Package: &pkg, Min: &min, Max: &max},
		},
	}, nil)
	defer deleteEnvTestUncore(t, cl, "envtest-pkg-uncore")

	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-pkg-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", fmt.Sprint(max), fmt.Sprint(min)))
	require.NoError(t, checkUncoreValues("testing/cpus", "00", "01", fmt.Sprint(max), fmt.Sprint(min)))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "Package 0: Min 1400000, Max 2200000", pns.Status.Uncore.Config)
}

// Test 8: Empty nodeSelector matches all nodes
func TestEnvTest_UncoreReconcile_EmptySelectorMatchesAll(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-emptyselector"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	createTestNode(t, cl, nodeName, map[string]string{"role": "worker"})
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create an Uncore CR with system wide min and max frequencies.
	sysMin := uint(1400000)
	sysMax := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-global-uncore", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax}, nil)
	defer deleteEnvTestUncore(t, cl, "envtest-global-uncore")

	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-global-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", fmt.Sprint(sysMax), fmt.Sprint(sysMin)))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "envtest-global-uncore", pns.Status.Uncore.Name)
}

// Test 9: Sticky active config — active config stays even when a newer CR is added
func TestEnvTest_UncoreReconcile_StickyActiveConfig(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-sticky"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	createTestNode(t, cl, nodeName, nil)
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	sysMin := uint(1400000)
	sysMax := uint(2200000)

	// Create first Uncore CR with system wide min and max frequencies and reconcile so it becomes active.
	createEnvTestUncore(t, cl, "envtest-sticky-a", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax}, nil)
	defer deleteEnvTestUncore(t, cl, "envtest-sticky-a")

	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-sticky-a", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", fmt.Sprint(sysMax), fmt.Sprint(sysMin)))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "envtest-sticky-a", pns.Status.Uncore.Name)

	// Create second Uncore. sticky-a is already active and should stay.
	createEnvTestUncore(t, cl, "envtest-sticky-b", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax}, nil)
	defer deleteEnvTestUncore(t, cl, "envtest-sticky-b")

	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-sticky-b", Namespace: PowerNamespace}})
	require.NoError(t, err)

	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", fmt.Sprint(sysMax), fmt.Sprint(sysMin)))
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "envtest-sticky-a", pns.Status.Uncore.Name, "Active config should stick")
	assert.NotEmpty(t, pns.Status.Uncore.Errors, "Conflict error should be reported")
}

// Test 10: Updating min/max on the active Uncore CR applies the new values
func TestEnvTest_UncoreReconcile_UpdateActiveConfig(t *testing.T) {
	cl := envTestClient
	nodeName := "envtest-uncore-update"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	t.Setenv("NODE_NAME", nodeName)

	// Hardware min/max are set to 1200000 and 2400000.
	host, teardown, err := fullDummySystem()
	require.NoError(t, err)
	defer teardown()

	r := &UncoreReconciler{Client: cl, Log: testLogger(), PowerLibrary: host}

	createTestNode(t, cl, nodeName, nil)
	defer deleteEnvTestNode(t, cl, nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	defer deleteTestPowerNodeState(t, cl, pnsName)
	applyNodeInfo(t, cl, pnsName)

	// Create and reconcile with initial values.
	sysMin := uint(1400000)
	sysMax := uint(2200000)
	createEnvTestUncore(t, cl, "envtest-update-uncore", powerv1alpha1.UncoreSpec{SysMin: &sysMin, SysMax: &sysMax}, nil)
	defer deleteEnvTestUncore(t, cl, "envtest-update-uncore")

	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-update-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)
	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2200000", "1400000"))

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "SysMin: 1400000, SysMax: 2200000", pns.Status.Uncore.Config)

	// Update the Uncore CR with new frequency values.
	uncore := &powerv1alpha1.Uncore{}
	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: "envtest-update-uncore", Namespace: PowerNamespace}, uncore))
	newMin := uint(1300000)
	newMax := uint(2100000)
	uncore.Spec.SysMin = &newMin
	uncore.Spec.SysMax = &newMax
	require.NoError(t, cl.Update(context.TODO(), uncore))

	// Reconcile again — new values should be applied.
	_, err = r.Reconcile(context.TODO(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "envtest-update-uncore", Namespace: PowerNamespace}})
	require.NoError(t, err)
	require.NoError(t, checkUncoreValues("testing/cpus", "00", "00", "2100000", "1300000"))

	require.NoError(t, cl.Get(context.TODO(), client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns))
	require.NotNil(t, pns.Status.Uncore)
	assert.Equal(t, "SysMin: 1300000, SysMax: 2100000", pns.Status.Uncore.Config)
}
