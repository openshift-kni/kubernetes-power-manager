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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSSA_ProfileFieldManagerOwnership(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	logger := ctrl.Log.WithName("testing")

	// Add two profiles via the production function.
	err := addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("performance", false), nil, &logger)
	require.NoError(t, err)

	err = addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("balance-power", false), nil, &logger)
	require.NoError(t, err)

	// Both profiles should coexist.
	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	assert.Len(t, pns.Status.PowerProfiles, 2, "both profiles should be present")
	profileNames := []string{pns.Status.PowerProfiles[0].Name, pns.Status.PowerProfiles[1].Name}
	assert.Contains(t, profileNames, "performance")
	assert.Contains(t, profileNames, "balance-power")

	// Verify SSA field manager ownership for both profiles.
	managers := map[string]bool{}
	for _, mf := range pns.ManagedFields {
		managers[mf.Manager] = true
	}
	assert.True(t, managers[powerProfileFieldManager("performance")], "expected field manager for performance")
	assert.True(t, managers[powerProfileFieldManager("balance-power")], "expected field manager for balance-power")
}

func TestSSA_ProfileRemovalPreservesOtherProfiles(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	logger := ctrl.Log.WithName("testing")

	// Add two profiles.
	err := addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("performance", false), nil, &logger)
	require.NoError(t, err)

	err = addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("balance-power", false), nil, &logger)
	require.NoError(t, err)

	// Remove profile 1 via the production removal function.
	err = removePowerNodeStatusProfileEntry(ctx, cl, nodeName, "performance", &logger)
	require.NoError(t, err)

	// Only profile 2 should remain.
	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	assert.Len(t, pns.Status.PowerProfiles, 1, "only balance-power should remain")
	assert.Equal(t, "balance-power", pns.Status.PowerProfiles[0].Name)
}

func TestSSA_ProfileUpdatePreservesEntry(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	logger := ctrl.Log.WithName("testing")

	// Apply initial profile.
	err := addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("performance", false), nil, &logger)
	require.NoError(t, err)

	// Update the same profile with errors.
	err = addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("performance", false), fmt.Errorf("epp invalid"), &logger)
	require.NoError(t, err)

	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	assert.Len(t, pns.Status.PowerProfiles, 1)
	assert.Equal(t, []string{"epp invalid"}, pns.Status.PowerProfiles[0].Errors)
}

// TestSSA_RemoveLastProfileDoesNotFailWithEmptyStatus verifies that removing the last
// profile from PowerNodeState does not fail with an empty status error.
// The PowerProfiles field uses omitzero so the empty non-nil slice serializes as
// "powerProfiles": [] — this keeps the status non-null after SSA prunes the last entry.
// The field manager is also cleanly removed (empty map-type list → zero entries → nothing to own).
func TestSSA_RemoveLastProfileDoesNotFailWithEmptyStatus(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)
	logger := ctrl.Log.WithName("testing")

	// Add a single profile via the production function.
	err := addPowerNodeStatusProfileEntry(ctx, cl, nodeName, newTestPowerProfile("test-profile", false), nil, &logger)
	require.NoError(t, err, "failed to add profile")

	// Remove the last profile.
	err = removePowerNodeStatusProfileEntry(ctx, cl, nodeName, "test-profile", &logger)
	require.NoError(t, err, "removing last profile should not fail")

	// Verify the profile was removed.
	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)
	assert.Empty(t, pns.Status.PowerProfiles, "profiles should be empty after removing last one")

	// Verify the field manager was cleanly removed (omitzero serializes [] which
	// produces zero entries in the map-type list -> empty fieldset -> manager removed).
	removedManager := powerProfileFieldManager("test-profile")
	for _, mf := range pns.ManagedFields {
		assert.NotEqual(t, removedManager, mf.Manager,
			"field manager %q should be removed after profile deletion", removedManager)
	}
}
