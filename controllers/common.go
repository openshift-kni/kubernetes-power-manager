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
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	powerv1alpha1 "github.com/cluster-power-manager/cluster-power-manager/api/v1alpha1"
	"github.com/cluster-power-manager/cluster-power-manager/pkg/util"
	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const queuetime = time.Second * 5

// FieldOwnerPowerProfileController is the base field manager name for PowerProfile controller.
const FieldOwnerPowerProfileController = "powerprofile-controller"

// powerProfileFieldManager returns the field manager name for a specific PowerProfile.
// Using per-profile field managers enables SSA to track ownership at the element level
// for the map-type PowerProfiles list (with listMapKey=name).
func powerProfileFieldManager(profileName string) string {
	return fmt.Sprintf("%s.%s", FieldOwnerPowerProfileController, profileName)
}

// ValidEppValues defines the valid EPP (Energy Performance Preference) values
var ValidEppValues = []string{"performance", "balance_performance", "balance_power", "power"}

// isValidEpp checks if a certain name corresponds to a valid EPP value.
func isValidEpp(inputName string) bool {
	for _, validEpp := range ValidEppValues {
		if inputName == validEpp {
			return true
		}
	}
	return false
}

// nodeMatchesSelector checks if a node's labels satisfy the given LabelSelector.
// An empty selector (no matchLabels and no matchExpressions) matches all nodes.
func nodeMatchesSelector(nodeLabels map[string]string, ls metav1.LabelSelector) (bool, error) {
	if len(ls.MatchLabels) == 0 && len(ls.MatchExpressions) == 0 {
		return true, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return false, err
	}
	return selector.Matches(labels.Set(nodeLabels)), nil
}

// getActiveResourceName reads the currently active resource name from PowerNodeState status.
// extractName extracts the active resource name from the PowerNodeState status — each controller
// reads from a different status field (e.g., Uncore.Name vs CPUPools.Shared.PowerNodeConfig).
func getActiveResourceName(ctx context.Context, c client.Reader, nodeName string, extractName func(*powerv1alpha1.PowerNodeStateStatus) string) (string, error) {
	pns := &powerv1alpha1.PowerNodeState{}
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	if err := c.Get(ctx, client.ObjectKey{
		Namespace: PowerNamespace,
		Name:      pnsName,
	}, pns); err != nil {
		return "", err
	}
	return extractName(&pns.Status), nil
}

// filterByNodeSelector returns the items whose nodeSelector matches the given node labels.
// getSelector extracts the LabelSelector from each item.
// Items with invalid selectors are skipped with a log message.
func filterByNodeSelector[T any](items []T, nodeLabels map[string]string, getSelector func(T) metav1.LabelSelector, getMeta func(T) metav1.ObjectMeta, logger logr.Logger) []T {
	var matches []T
	for _, item := range items {
		match, err := nodeMatchesSelector(nodeLabels, getSelector(item))
		if err != nil {
			logger.Error(err, "invalid label selector", "resource", getMeta(item).Name)
			continue
		}
		if match {
			matches = append(matches, item)
		}
	}
	return matches
}

// selectActiveOrOldest resolves which resource to apply when multiple candidates match a node.
// Rules:
//  1. If the currently active resource (by name) is still among matches, keep it (sticky).
//  2. Otherwise, pick the oldest by creation timestamp, with name as alphabetical tiebreaker.
//
// getMeta extracts the ObjectMeta from each item, allowing this function to work with
// any K8s resource type (Uncore, PowerNodeConfig, etc.).
func selectActiveOrOldest[T any](matches []T, activeName string, getMeta func(T) metav1.ObjectMeta, logger *logr.Logger) *T {
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > 1 {
		logger.Info("multiple configs match this node, resolving conflict", "count", len(matches))
	}

	// Keep the active config if it's still among matches.
	if activeName != "" {
		for i := range matches {
			if getMeta(matches[i]).Name == activeName {
				return &matches[i]
			}
		}
	}

	// No active or active no longer matches — pick oldest.
	sort.Slice(matches, func(i, j int) bool {
		ti := getMeta(matches[i]).CreationTimestamp.Time
		tj := getMeta(matches[j]).CreationTimestamp.Time
		if ti.Equal(tj) {
			return getMeta(matches[i]).Name < getMeta(matches[j]).Name
		}
		return ti.Before(tj)
	})
	return &matches[0]
}

// nodeMatchesPowerProfile checks if a PowerProfile should be applied to a specific node.
// Fetches the node to read its labels, then delegates to nodeMatchesSelector.
// Short-circuits if the selector is empty (matches all nodes) to avoid unnecessary node fetch.
func nodeMatchesPowerProfile(ctx context.Context, c client.Client, profile *powerv1alpha1.PowerProfile, nodeName string, logger *logr.Logger) (bool, error) {
	logger.V(5).Info("Checking if PowerProfile should be applied to node", "profile", profile.Name, "nodeName", nodeName)

	ls := profile.Spec.NodeSelector.LabelSelector
	if len(ls.MatchLabels) == 0 && len(ls.MatchExpressions) == 0 {
		logger.V(5).Info("No label selector specified, applying PowerProfile to all nodes", "profile", profile.Name, "nodeName", nodeName)
		return true, nil
	}

	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		logger.Error(err, "Failed to get node for selector validation", "nodeName", nodeName)
		return false, err
	}

	match, err := nodeMatchesSelector(node.Labels, ls)
	if err != nil {
		logger.Error(err, "Failed to convert label selector", "selector", ls)
		return false, err
	}
	logger.V(5).Info("Node label check result", "nodeName", nodeName, "result", match)
	return match, nil
}

// validateProfileAvailabilityOnNode validates that a PowerProfile exists in the cluster and is available to be used on the node
func validateProfileAvailabilityOnNode(ctx context.Context, c client.Client, profileName string, nodeName string, powerLibrary power.Host, logger *logr.Logger) (bool, error) {
	if profileName == "" {
		return true, nil
	}

	powerProfile := &powerv1alpha1.PowerProfile{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      profileName,
		Namespace: PowerNamespace,
	}, powerProfile)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if powerLibrary == nil {
		return false, fmt.Errorf("power library is nil")
	}

	// PowerProfile exists in the cluster and pool exists indicates profile is available to be used on the node.
	pool := powerLibrary.GetExclusivePool(profileName)
	if pool == nil {
		logger.Error(fmt.Errorf("pool '%s' not found", profileName), "pool not found")
		return false, nil
	}

	// Verify the node matches the PowerProfile node selector.
	match, err := nodeMatchesPowerProfile(ctx, c, powerProfile, nodeName, logger)
	if err != nil {
		logger.Error(err, "error checking if node matches power profile selector")
		return false, err
	}
	return match, nil
}

// formatIntOrString formats an IntOrString pointer as a string.
// Returns empty string if nil, the integer value as string for Int type,
// or the string value for String type.
func formatIntOrString(value *intstr.IntOrString) string {
	if value == nil {
		return ""
	}
	if value.Type == intstr.Int {
		return strconv.Itoa(int(value.IntVal))
	}
	return value.StrVal
}

// formatCpuScalingPolicy serializes a CpuScalingPolicy to its JSON representation.
// Returns empty string if policy is nil. Nil fields within the policy are omitted.
func formatCpuScalingPolicy(policy *powerv1alpha1.CpuScalingPolicy) (string, error) {
	if policy == nil || *policy == (powerv1alpha1.CpuScalingPolicy{}) {
		return "", nil
	}
	b, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("failed to marshal cpuScalingPolicy: %w", err)
	}
	return string(b), nil
}

// applyPowerNodeStateProfilesStatus applies the given PowerProfiles to the PowerNodeState status
// using Server-Side Apply. The fieldManager parameter enables per-profile ownership — each profile
// should use a unique field manager (e.g., "powerprofile-controller.profile-name") so that SSA can
// track ownership at the element level for the map-type PowerProfiles list.
func applyPowerNodeStateProfilesStatus(ctx context.Context, c client.Client, powerNodeStateName string, profiles []powerv1alpha1.PowerNodeProfileStatus, fieldManager string) error {
	patchNodeState := &powerv1alpha1.PowerNodeState{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "power.cluster-power-manager.github.io/v1alpha1",
			Kind:       "PowerNodeState",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      powerNodeStateName,
			Namespace: PowerNamespace,
		},
		Status: powerv1alpha1.PowerNodeStateStatus{
			PowerProfiles: profiles,
		},
	}

	return c.Status().Patch(ctx, patchNodeState, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership)
}

// addPowerNodeStatusProfileEntry updates the PowerNodeState CR for a given node with the validation
// results of a PowerProfile using Server-Side Apply (SSA).
//
// This function uses per-profile field managers (e.g., "powerprofile-controller.profile-name") to enable
// concurrent updates from different profile controllers. Since PowerProfiles is a map-type list keyed by
// name, SSA merges entries by key - each controller only owns its own profile entry.
func addPowerNodeStatusProfileEntry(ctx context.Context, c client.Client, nodeName string, profile *powerv1alpha1.PowerProfile, profileErrors error, logger *logr.Logger) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName cannot be empty")
	}

	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)

	// Serialize the PowerProfile spec to a readable config string.
	cstatesString := prettifyCStatesMap(profile.Spec.CStates.Names)
	if cstatesString == "" && profile.Spec.CStates.MaxLatencyUs != nil {
		cstatesString = fmt.Sprintf("maxLatency: %d", *profile.Spec.CStates.MaxLatencyUs)
	}
	config := fmt.Sprintf(
		"Min: %s, Max: %s, Governor: %s, EPP: %s, C-States: %s",
		formatIntOrString(profile.Spec.PStates.Min), formatIntOrString(profile.Spec.PStates.Max),
		profile.Spec.PStates.Governor, profile.Spec.PStates.Epp, cstatesString)

	scalingStr, err := formatCpuScalingPolicy(profile.Spec.CpuScalingPolicy)
	if err != nil {
		return err
	}
	if scalingStr != "" {
		config += ", CpuScalingPolicy: " + scalingStr
	}

	errList := util.UnpackErrsToStrings(profileErrors)
	profileStatus := powerv1alpha1.PowerNodeProfileStatus{Name: profile.Name, Config: config, Errors: *errList}
	fieldManager := powerProfileFieldManager(profile.Name)

	err = applyPowerNodeStateProfilesStatus(ctx, c, powerNodeStateName, []powerv1alpha1.PowerNodeProfileStatus{profileStatus}, fieldManager)
	if err != nil {
		if errors.IsNotFound(err) {
			// Profile was already created, but status wasn't recorded.
			// Return error to requeue so we retry once PowerConfig creates the CR.
			logger.Info("PowerNodeState not found, requeueing to record profile status", "powerNodeState", powerNodeStateName)
			return err
		}
		return err
	}

	logger.Info("Updated PowerNodeState with profile validation results",
		"powerNodeState", powerNodeStateName,
		"profile", profile.Name,
		"errors", len(*errList))

	return nil
}

// removePowerNodeStatusProfileEntry removes a PowerProfile entry from the PowerNodeState status.
//
// This function uses per-profile field managers to release ownership of the profile entry.
// Applying an empty PowerProfiles list causes SSA to prune the entry this manager previously
// owned, while preserving entries owned by other field managers.
func removePowerNodeStatusProfileEntry(ctx context.Context, c client.Client, nodeName string, profileName string, logger *logr.Logger) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName cannot be empty")
	}

	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)
	fieldManager := powerProfileFieldManager(profileName)

	if err := applyPowerNodeStateProfilesStatus(ctx, c, powerNodeStateName, []powerv1alpha1.PowerNodeProfileStatus{}, fieldManager); err != nil {
		if errors.IsNotFound(err) {
			logger.V(5).Info("PowerNodeState not found, nothing to remove", "powerNodeState", powerNodeStateName)
			return nil
		}
		return err
	}

	logger.Info("Removed profile from PowerNodeState",
		"powerNodeState", powerNodeStateName,
		"profile", profileName)

	return nil
}

// prettifyCoreList formats a list of CPU core IDs into a compact range string.
// Format: "0-3,5,7-9".
func prettifyCoreList(cores []uint) string {
	sorted := append([]uint(nil), cores...)
	prettified := ""
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i := 0; i < len(sorted); i++ {
		start := i
		end := i

		for end < len(sorted)-1 {
			if sorted[end+1]-sorted[end] == 1 {
				end++
			} else {
				break
			}
		}

		if end-start > 0 {
			prettified += fmt.Sprintf("%d-%d", sorted[start], sorted[end])
		} else {
			prettified += fmt.Sprintf("%d", sorted[start])
		}

		if end < len(sorted)-1 {
			prettified += ","
		}

		i = end
	}

	return prettified
}

// prettifyCStatesMap formats C-states map into a readable string showing enabled/disabled states.
// Format: "enabled: C1,C1E; disabled: C6"
func prettifyCStatesMap(states map[string]bool) string {
	if len(states) == 0 {
		return ""
	}

	var enabled, disabled []string
	for state, isEnabled := range states {
		if isEnabled {
			enabled = append(enabled, state)
		} else {
			disabled = append(disabled, state)
		}
	}

	sort.Strings(enabled)
	sort.Strings(disabled)

	var parts []string
	if len(enabled) > 0 {
		parts = append(parts, "enabled: "+strings.Join(enabled, ","))
	}
	if len(disabled) > 0 {
		parts = append(parts, "disabled: "+strings.Join(disabled, ","))
	}

	return strings.Join(parts, "; ")
}
