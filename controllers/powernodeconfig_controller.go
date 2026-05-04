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
	"os"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	powerv1alpha1 "github.com/openshift-kni/kubernetes-power-manager/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// FieldOwnerPowerNodeConfigController is the SSA field manager for shared and reserved pool status.
const FieldOwnerPowerNodeConfigController = "powernodeconfig-controller"

// PowerNodeConfigReconciler reconciles PowerNodeConfig objects to configure
// shared and reserved CPU pools on nodes matching the config's nodeSelector.
type PowerNodeConfigReconciler struct {
	client.Client
	Log          logr.Logger
	Scheme       *runtime.Scheme
	PowerLibrary power.Host
}

// +kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=powernodeconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=powernodestates,verbs=get;list;watch
// +kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=powernodestates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

// Reconcile evaluates all PowerNodeConfigs matching this node, resolves conflicts,
// and configures or cleans up shared and reserved CPU pools accordingly.
func (r *PowerNodeConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("powernodeconfig", req.NamespacedName)

	if req.Namespace != PowerNamespace {
		logger.V(5).Info("ignoring resource outside power-manager namespace")
		return ctrl.Result{}, nil
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return ctrl.Result{}, fmt.Errorf("NODE_NAME environment variable not set")
	}

	matches, err := r.getMatchingPowerNodeConfigs(ctx, nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}

	activeConfigName, err := getActiveResourceName(ctx, r.Client, nodeName, powerNodeConfigActiveName)
	if err != nil {
		if errors.IsNotFound(err) {
			// PowerNodeState may not exist yet at startup (created by PowerConfig controller).
			logger.Info("PowerNodeState not found, requeueing")
			return ctrl.Result{RequeueAfter: queuetime}, nil
		}
		return ctrl.Result{}, err
	}

	selected := selectActiveOrOldest(matches, activeConfigName, powerNodeConfigMeta, &logger)

	// No matching config — clean up if something was active.
	if selected == nil {
		if activeConfigName != "" {
			logger.Info("no matching PowerNodeConfig, cleaning up", "previousActive", activeConfigName)
			if err := r.cleanupPowerNodeConfigPools(ctx, nodeName, &logger); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Build conflict errors for status reporting, including validation details for each conflicting config.
	var conflictErrors []string
	if len(matches) > 1 {
		for _, m := range matches {
			if m.Name != selected.Name {
				conflictErrors = append(conflictErrors, r.describeConflictingConfig(ctx, &m))
			}
		}
	}

	return r.applyPowerNodeConfig(ctx, selected, nodeName, conflictErrors, &logger)
}

// getMatchingPowerNodeConfigs returns all PowerNodeConfigs whose nodeSelector matches this node.
func (r *PowerNodeConfigReconciler) getMatchingPowerNodeConfigs(ctx context.Context, nodeName string) ([]powerv1alpha1.PowerNodeConfig, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	configList := &powerv1alpha1.PowerNodeConfigList{}
	if err := r.List(ctx, configList, client.InNamespace(PowerNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list PowerNodeConfigs: %w", err)
	}

	return filterByNodeSelector(configList.Items, node.Labels, powerNodeConfigSelector, powerNodeConfigMeta, r.Log), nil
}

// powerNodeConfigActiveName extracts the active PowerNodeConfig name from PowerNodeState status.
func powerNodeConfigActiveName(s *powerv1alpha1.PowerNodeStateStatus) string {
	if s.CPUPools != nil && s.CPUPools.Shared != nil {
		return s.CPUPools.Shared.PowerNodeConfig
	}
	return ""
}

// powerNodeConfigMeta extracts ObjectMeta from a PowerNodeConfig for use with generic helpers.
func powerNodeConfigMeta(c powerv1alpha1.PowerNodeConfig) metav1.ObjectMeta { return c.ObjectMeta }

// powerNodeConfigSelector extracts the LabelSelector from a PowerNodeConfig for use with filterByNodeSelector.
func powerNodeConfigSelector(c powerv1alpha1.PowerNodeConfig) metav1.LabelSelector {
	return c.Spec.NodeSelector.LabelSelector
}

// applyPowerNodeConfig validates profiles, configures shared and reserved CPU pools
// in the Power Library, and updates PowerNodeState status via SSA.
func (r *PowerNodeConfigReconciler) applyPowerNodeConfig(
	ctx context.Context,
	config *powerv1alpha1.PowerNodeConfig,
	nodeName string,
	conflictErrors []string,
	logger *logr.Logger,
) (ctrl.Result, error) {
	logger.Info("applying PowerNodeConfig", "config", config.Name)

	// TODO: Add a validating admission webhook to block deletion of PowerProfiles referenced by
	// PowerNodeConfigs (spec.sharedPowerProfile or spec.reservedCPUs[].powerProfile) or running pods.
	// Without the webhook, deleting a referenced profile leaves the pools configured with stale
	// settings — the controller records an error and requeues, but does not tear down the pools.
	if err := r.validatePowerNodeConfigProfiles(ctx, config, nodeName, logger); err != nil {
		// Record the validation error in PowerNodeState so users can see why the config isn't applied.
		if statusErr := r.updatePowerNodeStatusPools(ctx, nodeName, config.Name, config.Spec.SharedPowerProfile, "", nil, []string{err.Error()}, logger); statusErr != nil {
			logger.Error(statusErr, "failed to update PowerNodeState with validation error")
		}
		logger.Error(err, "profile validation failed, requeueing")
		return ctrl.Result{RequeueAfter: queuetime}, nil
	}

	if err := r.configureSharedPool(config, logger); err != nil {
		return ctrl.Result{}, err
	}

	reservedProfileCPUs, reservedErrors := r.configureReservedPools(config, nodeName, logger)

	// Collect all status errors.
	var statusErrors []string
	statusErrors = append(statusErrors, conflictErrors...)
	for _, err := range reservedErrors {
		statusErrors = append(statusErrors, err.Error())
	}

	// Read current shared CPUs from POL and update status.
	sharedCPUIDs := prettifyCoreList(r.PowerLibrary.GetSharedPool().Cpus().IDs())
	if err := r.updatePowerNodeStatusPools(ctx, nodeName, config.Name, config.Spec.SharedPowerProfile, sharedCPUIDs, reservedProfileCPUs, statusErrors, logger); err != nil {
		return ctrl.Result{}, err
	}

	if len(reservedErrors) > 0 {
		return ctrl.Result{}, fmt.Errorf("errors configuring reserved pools")
	}
	return ctrl.Result{}, nil
}

// validatePowerNodeConfigProfiles checks that all PowerProfiles referenced by the config
// exist in the cluster, are available on this node, and that the shared pool profile
// has spec.shared set to true.
func (r *PowerNodeConfigReconciler) validatePowerNodeConfigProfiles(
	ctx context.Context,
	config *powerv1alpha1.PowerNodeConfig,
	nodeName string,
	logger *logr.Logger,
) error {
	// Validate the shared pool profile is marked as shared.
	sharedProfile := &powerv1alpha1.PowerProfile{}
	if err := r.Get(ctx, client.ObjectKey{Name: config.Spec.SharedPowerProfile, Namespace: PowerNamespace}, sharedProfile); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("PowerProfile '%s' not found", config.Spec.SharedPowerProfile)
		}
		return err
	}
	if !sharedProfile.Spec.Shared {
		return fmt.Errorf("PowerProfile '%s' is not a shared profile (spec.shared must be true)", config.Spec.SharedPowerProfile)
	}

	// Validate reserved CPU entries are disjoint (no core appears in multiple entries).
	seenCores := map[uint]struct{}{}
	for _, rc := range config.Spec.ReservedCPUs {
		for _, core := range rc.Cores {
			if _, exists := seenCores[core]; exists {
				return fmt.Errorf("reserved CPU %d is listed in multiple reservedCPUs entries", core)
			}
			seenCores[core] = struct{}{}
		}
	}

	// Validate all referenced profiles are available on this node.
	profileNames := []string{config.Spec.SharedPowerProfile}
	for _, rc := range config.Spec.ReservedCPUs {
		if rc.PowerProfile != "" {
			profileNames = append(profileNames, rc.PowerProfile)
		}
	}
	for _, name := range profileNames {
		available, err := validateProfileAvailabilityOnNode(ctx, r.Client, name, nodeName, r.PowerLibrary, logger)
		if err != nil {
			return err
		}
		if !available {
			return fmt.Errorf("PowerProfile '%s' not available on node %s", name, nodeName)
		}
	}
	return nil
}

// configureSharedPool sets the shared pool's power profile from the config's referenced profile.
func (r *PowerNodeConfigReconciler) configureSharedPool(config *powerv1alpha1.PowerNodeConfig, logger *logr.Logger) error {
	pool := r.PowerLibrary.GetExclusivePool(config.Spec.SharedPowerProfile)
	if pool == nil {
		return fmt.Errorf("pool for profile '%s' not found", config.Spec.SharedPowerProfile)
	}
	profile := pool.GetPowerProfile()
	if profile == nil {
		return fmt.Errorf("profile for pool '%s' not found", config.Spec.SharedPowerProfile)
	}
	if err := r.PowerLibrary.GetSharedPool().SetPowerProfile(profile); err != nil {
		return fmt.Errorf("failed to set shared pool profile: %w", err)
	}
	// Move all reserved CPUs into the shared pool so they inherit the profile.
	// configureReservedPools will then move specific CPUs back to reserved.
	if err := r.PowerLibrary.GetReservedPool().SetCpuIDs([]uint{}); err != nil {
		return fmt.Errorf("failed to clear reserved pool: %w", err)
	}
	logger.V(5).Info("configured shared pool profile", "profile", config.Spec.SharedPowerProfile)
	return nil
}

// configureReservedPools removes existing pseudo-reserved pools, then creates new ones
// based on the config's reservedCPUs spec. Returns per-group status and any errors.
func (r *PowerNodeConfigReconciler) configureReservedPools(
	config *powerv1alpha1.PowerNodeConfig,
	nodeName string,
	logger *logr.Logger,
) ([]powerv1alpha1.PowerProfileCPUs, []error) {
	// Remove existing pseudo-reserved pools.
	pools := r.PowerLibrary.GetAllExclusivePools()
	for _, p := range *pools {
		if strings.HasPrefix(p.Name(), nodeName+"-reserved-") {
			if err := p.Remove(); err != nil {
				return nil, []error{fmt.Errorf("failed to remove reserved pool %s: %w", p.Name(), err)}
			}
		}
	}

	// Process each reserved CPU group.
	var reservedErrors []error
	var reservedProfileCPUs []powerv1alpha1.PowerProfileCPUs
	for _, rc := range config.Spec.ReservedCPUs {
		// Move cores to shared first to prevent exclusive→reserved conflicts.
		if err := r.PowerLibrary.GetSharedPool().MoveCpuIDs(rc.Cores); err != nil {
			return reservedProfileCPUs, []error{fmt.Errorf("failed to move reserved cores to shared: %w", err)}
		}
		if rc.PowerProfile != "" {
			if err := r.createReservedPool(rc, nodeName); err != nil {
				reservedErrors = append(reservedErrors, err)
				// Fallback: move to default reserved pool.
				if err := r.PowerLibrary.GetReservedPool().MoveCpuIDs(rc.Cores); err != nil {
					return reservedProfileCPUs, []error{fmt.Errorf("failed to move cores to reserved: %w", err)}
				}
				reservedProfileCPUs = append(reservedProfileCPUs, powerv1alpha1.PowerProfileCPUs{
					PowerProfile: rc.PowerProfile,
					CPUIDs:       prettifyCoreList(rc.Cores),
					Errors:       []string{err.Error()},
				})
			} else {
				reservedProfileCPUs = append(reservedProfileCPUs, powerv1alpha1.PowerProfileCPUs{
					PowerProfile: rc.PowerProfile,
					CPUIDs:       prettifyCoreList(rc.Cores),
				})
			}
		} else {
			if err := r.PowerLibrary.GetReservedPool().MoveCpuIDs(rc.Cores); err != nil {
				return reservedProfileCPUs, []error{fmt.Errorf("failed to move cores to reserved: %w", err)}
			}
			reservedProfileCPUs = append(reservedProfileCPUs, powerv1alpha1.PowerProfileCPUs{
				CPUIDs: prettifyCoreList(rc.Cores),
			})
		}
	}
	return reservedProfileCPUs, reservedErrors
}

// cleanupPowerNodeConfigPools moves all shared and reserved CPUs back to the default
// reserved pool, removes pseudo-reserved pools, and clears PowerNodeState status.
func (r *PowerNodeConfigReconciler) cleanupPowerNodeConfigPools(ctx context.Context, nodeName string, logger *logr.Logger) error {
	movedCores := *r.PowerLibrary.GetSharedPool().Cpus()
	pools := r.PowerLibrary.GetAllExclusivePools()
	for _, p := range *pools {
		if strings.HasPrefix(p.Name(), nodeName+"-reserved-") {
			movedCores = append(movedCores, *p.Cpus()...)
			if err := p.Remove(); err != nil {
				return fmt.Errorf("failed to remove reserved pool %s: %w", p.Name(), err)
			}
		}
	}
	if err := r.PowerLibrary.GetReservedPool().MoveCpus(movedCores); err != nil {
		return fmt.Errorf("failed to move cores to reserved: %w", err)
	}
	return r.removePowerNodeStatusPools(ctx, nodeName, logger)
}

// createReservedPool creates a pseudo-reserved exclusive pool for reserved CPUs
// that have a specific PowerProfile assigned.
func (r *PowerNodeConfigReconciler) createReservedPool(rc powerv1alpha1.ReservedSpec, nodeName string) error {
	poolName := fmt.Sprintf("%s-reserved-%v", nodeName, rc.Cores)
	pseudoPool, err := r.PowerLibrary.AddExclusivePool(poolName)
	if err != nil {
		return fmt.Errorf("failed to create reserved pool for cores %v: %w", rc.Cores, err)
	}

	corePool := r.PowerLibrary.GetExclusivePool(rc.PowerProfile)
	if corePool == nil {
		_ = pseudoPool.Remove()
		return fmt.Errorf("profile '%s' has no existing pool", rc.PowerProfile)
	}
	if err := pseudoPool.SetPowerProfile(corePool.GetPowerProfile()); err != nil {
		_ = pseudoPool.Remove()
		return fmt.Errorf("failed to set profile for reserved cores: %w", err)
	}
	if err := pseudoPool.SetCpuIDs(rc.Cores); err != nil {
		_ = pseudoPool.Remove()
		return fmt.Errorf("failed to move cores to reserved pool: %w", err)
	}
	return nil
}

// updatePowerNodeStatusPools writes shared and reserved pool status to PowerNodeState
// in a single SSA apply under the powernodeconfig-controller field manager.
func (r *PowerNodeConfigReconciler) updatePowerNodeStatusPools(
	ctx context.Context,
	nodeName string,
	configName string,
	profileName string,
	sharedCPUIDs string,
	reservedProfileCPUs []powerv1alpha1.PowerProfileCPUs,
	statusErrors []string,
	logger *logr.Logger,
) error {
	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)

	cpuPools := &powerv1alpha1.CPUPoolsStatus{
		Shared: &powerv1alpha1.SharedCPUPoolStatus{
			PowerProfile:    profileName,
			PowerNodeConfig: configName,
			CPUIDs:          sharedCPUIDs,
			Errors:          statusErrors,
		},
	}
	if len(reservedProfileCPUs) > 0 {
		cpuPools.Reserved = []powerv1alpha1.ReservedCPUPoolStatus{
			{
				PowerNodeConfig:  configName,
				PowerProfileCPUs: reservedProfileCPUs,
			},
		}
	}

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
			CPUPools: cpuPools,
		},
	}

	if err := r.Status().Patch(ctx, patchNodeState, client.Apply,
		client.FieldOwner(FieldOwnerPowerNodeConfigController), client.ForceOwnership); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("PowerNodeState %s not found, requeueing", powerNodeStateName)
		}
		return fmt.Errorf("failed to update PowerNodeState pool status: %w", err)
	}

	logger.Info("updated PowerNodeState pool status", "config", configName)
	return nil
}

// removePowerNodeStatusPools removes shared and reserved pool status from PowerNodeState.
func (r *PowerNodeConfigReconciler) removePowerNodeStatusPools(ctx context.Context, nodeName string, logger *logr.Logger) error {
	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)

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
			CPUPools: &powerv1alpha1.CPUPoolsStatus{
				// Empty reserved list (omitzero → "[]") triggers SSA field manager cascade cleanup.
				// Shared is nil → released by prune. Both are removed, field manager is cleaned up.
				Reserved: []powerv1alpha1.ReservedCPUPoolStatus{},
			},
		},
	}

	if err := r.Status().Patch(ctx, patchNodeState, client.Apply,
		client.FieldOwner(FieldOwnerPowerNodeConfigController), client.ForceOwnership); err != nil {
		if errors.IsNotFound(err) {
			logger.V(5).Info("PowerNodeState not found, nothing to remove")
			return nil
		}
		return fmt.Errorf("failed to remove pool status: %w", err)
	}

	logger.Info("removed pool status from PowerNodeState")
	return nil
}

// enqueuePowerNodeConfigReconcile returns a single reconcile request to trigger
// a full re-evaluation of all PowerNodeConfigs for this node.
func (r *PowerNodeConfigReconciler) enqueuePowerNodeConfigReconcile(ctx context.Context, _ client.Object) []reconcile.Request {
	configList := &powerv1alpha1.PowerNodeConfigList{}
	if err := r.List(ctx, configList, client.InNamespace(PowerNamespace)); err != nil {
		r.Log.Error(err, "failed to list PowerNodeConfigs")
		return nil
	}
	if len(configList.Items) == 0 {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name:      configList.Items[0].Name,
			Namespace: configList.Items[0].Namespace,
		},
	}}
}

// enqueueMatchingPowerNodeConfigReconcile returns a single reconcile request for the first
// PowerNodeConfig that matches this node's labels. Since the reconciler always
// re-evaluates all matching configs regardless of which key triggered it,
// one request is sufficient to trigger a full reconciliation.
func (r *PowerNodeConfigReconciler) enqueueMatchingPowerNodeConfigReconcile(ctx context.Context, _ client.Object) []reconcile.Request {
	nodeName := os.Getenv("NODE_NAME")
	configs, err := r.getMatchingPowerNodeConfigs(ctx, nodeName)
	if err != nil {
		r.Log.Error(err, "failed to get matching PowerNodeConfigs")
		return nil
	}
	if len(configs) == 0 {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name:      configs[0].Name,
			Namespace: configs[0].Namespace,
		},
	}}
}

// describeConflictingConfig returns a conflict error string for a non-selected PowerNodeConfig,
// including validation details (e.g., non-shared profile) to help users diagnose misconfiguration.
func (r *PowerNodeConfigReconciler) describeConflictingConfig(ctx context.Context, config *powerv1alpha1.PowerNodeConfig) string {
	profile := &powerv1alpha1.PowerProfile{}
	if err := r.Get(ctx, client.ObjectKey{Name: config.Spec.SharedPowerProfile, Namespace: PowerNamespace}, profile); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Sprintf("conflicting PowerNodeConfig: %s (profile '%s' not found)", config.Name, config.Spec.SharedPowerProfile)
		}
		return fmt.Sprintf("conflicting PowerNodeConfig: %s", config.Name)
	}
	if !profile.Spec.Shared {
		return fmt.Sprintf("conflicting PowerNodeConfig: %s (profile '%s' is not a shared profile)", config.Name, config.Spec.SharedPowerProfile)
	}
	return fmt.Sprintf("conflicting PowerNodeConfig: %s", config.Name)
}

// getExclusiveEntries safely extracts the exclusive pool entries from a PowerNodeState.
func getExclusiveEntries(pns *powerv1alpha1.PowerNodeState) []powerv1alpha1.ExclusiveCPUPoolStatus {
	if pns.Status.CPUPools == nil {
		return nil
	}
	return pns.Status.CPUPools.Exclusive
}

// SetupWithManager registers the controller and configures watches for
// PowerNodeConfigs, Node label changes, and PowerNodeState exclusive pool changes.
func (r *PowerNodeConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	nodeName := os.Getenv("NODE_NAME")
	return ctrl.NewControllerManagedBy(mgr).
		// PowerNodeConfig CRUD: re-evaluate which config should be active.
		For(&powerv1alpha1.PowerNodeConfig{}).
		// No PowerProfile watch: the PowerProfile controller handles pool reconfiguration
		// on profile updates/deletes. Watching profiles here would cause a redundant
		// tear-down/rebuild of reserved pools and a race on profile deletion.
		// Node label changes: labels determine which configs match this node.
		// Enqueues any config (not filtered by match) because a label change can cause
		// a previously matching config to stop matching, requiring cleanup.
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.enqueuePowerNodeConfigReconcile),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(e event.CreateEvent) bool { return false },
				DeleteFunc:  func(e event.DeleteEvent) bool { return false },
				GenericFunc: func(e event.GenericEvent) bool { return false },
				UpdateFunc: func(e event.UpdateEvent) bool {
					if e.ObjectNew.GetName() != nodeName {
						return false
					}
					oldNode := e.ObjectOld.(*corev1.Node)
					newNode := e.ObjectNew.(*corev1.Node)
					return !reflect.DeepEqual(oldNode.Labels, newNode.Labels)
				},
			})).
		// PowerNodeState exclusive pool changes: when the PowerPod controller moves CPUs
		// between exclusive and shared pools (via SSA), the shared CPU list in status needs
		// to be refreshed. Only fires when status.cpuPools.exclusive changes on this node's
		// PowerNodeState — changes to shared/reserved (written by this controller) are ignored
		// to prevent a reconcile loop.
		Watches(&powerv1alpha1.PowerNodeState{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueMatchingPowerNodeConfigReconcile),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(e event.CreateEvent) bool { return false },
				DeleteFunc:  func(e event.DeleteEvent) bool { return false },
				GenericFunc: func(e event.GenericEvent) bool { return false },
				UpdateFunc: func(e event.UpdateEvent) bool {
					if e.ObjectNew.GetName() != nodeName+"-power-state" {
						return false
					}
					oldPNS := e.ObjectOld.(*powerv1alpha1.PowerNodeState)
					newPNS := e.ObjectNew.(*powerv1alpha1.PowerNodeState)
					oldExclusive := getExclusiveEntries(oldPNS)
					newExclusive := getExclusiveEntries(newPNS)
					return !reflect.DeepEqual(oldExclusive, newExclusive)
				},
			})).
		Complete(r)
}
