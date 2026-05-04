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
	"maps"
	"os"
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

// FieldOwnerUncoreController is the SSA field manager for uncore status in PowerNodeState.
const FieldOwnerUncoreController = "uncore-controller"

// UncoreReconciler reconciles Uncore objects to configure uncore frequency
// settings on nodes matching the config's nodeSelector.
type UncoreReconciler struct {
	client.Client
	Log          logr.Logger
	Scheme       *runtime.Scheme
	PowerLibrary power.Host
}

//+kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=uncores,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=uncores/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=uncores/finalizers,verbs=update
//+kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=powernodestates,verbs=get;list;watch
//+kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=powernodestates/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

// Reconcile evaluates all Uncore CRs matching this node, resolves conflicts,
// and configures or cleans up uncore frequency settings accordingly.
func (r *UncoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("uncore", req.NamespacedName)
	logger.Info("reconciling uncore")

	if req.Namespace != PowerNamespace {
		logger.V(5).Info("ignoring resource outside power-manager namespace")
		return ctrl.Result{}, nil
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return ctrl.Result{}, fmt.Errorf("NODE_NAME environment variable not set")
	}

	if r.PowerLibrary.Topology() == nil {
		return ctrl.Result{}, fmt.Errorf("power library topology not available")
	}

	// Get all Uncore CRs matching this node's labels.
	matches, err := r.getMatchingUncores(ctx, nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Read currently active uncore config from PowerNodeState.
	activeUncoreName, err := getActiveResourceName(ctx, r.Client, nodeName, uncoreActiveName)
	if err != nil {
		if errors.IsNotFound(err) {
			// PowerNodeState may not exist yet at startup (created by PowerConfig controller).
			return ctrl.Result{RequeueAfter: queuetime}, nil
		}
		return ctrl.Result{}, err
	}

	// Resolve which config to apply (if multiple match).
	selected := selectActiveOrOldest(matches, activeUncoreName, uncoreMeta, &logger)

	// No matching Uncore CR — reset settings and clear status.
	if selected == nil {
		if activeUncoreName != "" {
			logger.Info("no matching Uncore config, cleaning up")
			if err := r.resetUncoreSettings(&logger); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.removeUncoreFromPowerNodeState(ctx, nodeName, &logger)
		}
		return ctrl.Result{}, nil
	}

	// Build conflict errors for non-selected configs.
	var conflictErrors []string
	if len(matches) > 1 {
		for _, m := range matches {
			if m.Name != selected.Name {
				conflictErrors = append(conflictErrors, fmt.Sprintf("conflicting Uncore: %s", m.Name))
			}
		}
	}

	return r.applyUncoreConfig(ctx, selected, nodeName, conflictErrors, &logger)
}

// getMatchingUncores returns all Uncore CRs whose nodeSelector matches this node.
func (r *UncoreReconciler) getMatchingUncores(ctx context.Context, nodeName string) ([]powerv1alpha1.Uncore, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	uncoreList := &powerv1alpha1.UncoreList{}
	if err := r.List(ctx, uncoreList, client.InNamespace(PowerNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list Uncores: %w", err)
	}

	return filterByNodeSelector(uncoreList.Items, node.Labels, uncoreSelector, uncoreMeta, r.Log), nil
}

// uncoreActiveName extracts the active Uncore config name from PowerNodeState status.
func uncoreActiveName(s *powerv1alpha1.PowerNodeStateStatus) string {
	if s.Uncore != nil {
		return s.Uncore.Name
	}
	return ""
}

// uncoreMeta extracts ObjectMeta from an Uncore for use with generic helpers.
func uncoreMeta(u powerv1alpha1.Uncore) metav1.ObjectMeta { return u.ObjectMeta }

// uncoreSelector extracts the LabelSelector from an Uncore for use with filterByNodeSelector.
func uncoreSelector(u powerv1alpha1.Uncore) metav1.LabelSelector {
	return u.Spec.NodeSelector.LabelSelector
}

// applyUncoreConfig resets uncore settings, applies the selected Uncore config,
// and updates PowerNodeState status via SSA.
func (r *UncoreReconciler) applyUncoreConfig(
	ctx context.Context,
	uncore *powerv1alpha1.Uncore,
	nodeName string,
	conflictErrors []string,
	logger *logr.Logger,
) (ctrl.Result, error) {
	logger.Info("applying Uncore config", "config", uncore.Name)

	spec := &uncore.Spec

	// Validate spec before resetting.
	hasSysWide := spec.SysMax != nil && spec.SysMin != nil
	hasDieSelectors := spec.DieSelectors != nil && len(*spec.DieSelectors) > 0
	if !hasSysWide && !hasDieSelectors {
		validationErr := "no valid uncore configuration: requires either both sysMin and sysMax, or non-empty dieSelectors"
		if err := r.updateUncoreInPowerNodeState(ctx, nodeName, uncore.Name, "", []string{validationErr}, logger); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, fmt.Errorf("%s", validationErr)
	}

	// Reset all uncore settings before applying new config.
	if err := r.resetUncoreSettings(logger); err != nil {
		return ctrl.Result{}, err
	}

	var applyErrors []string
	var configParts []string

	// Apply system-wide uncore settings.
	if spec.SysMax != nil && spec.SysMin != nil {
		p_uncore, err := power.NewUncore(*spec.SysMin, *spec.SysMax)
		if err != nil {
			applyErrors = append(applyErrors, fmt.Sprintf("error creating system uncore: %v", err))
		} else if err := r.PowerLibrary.Topology().SetUncore(p_uncore); err != nil {
			applyErrors = append(applyErrors, fmt.Sprintf("error setting system uncore: %v", err))
		} else {
			configParts = append(configParts, fmt.Sprintf("SysMin: %d, SysMax: %d", *spec.SysMin, *spec.SysMax))
		}
	}

	// Apply die/package-specific uncore settings.
	if spec.DieSelectors != nil {
		for _, dieselect := range *spec.DieSelectors {
			if dieselect.Max == nil || dieselect.Min == nil || dieselect.Package == nil {
				applyErrors = append(applyErrors, "die selector max, min and package fields must not be empty")
				continue
			}
			p_uncore, err := power.NewUncore(*dieselect.Min, *dieselect.Max)
			if err != nil {
				applyErrors = append(applyErrors, fmt.Sprintf("error creating uncore for package %d: %v", *dieselect.Package, err))
				continue
			}
			if dieselect.Die == nil {
				// Package-level tuning.
				pkg := r.PowerLibrary.Topology().Package(*dieselect.Package)
				if pkg == nil {
					applyErrors = append(applyErrors, fmt.Sprintf("invalid package: %d", *dieselect.Package))
					continue
				}
				if err := pkg.SetUncore(p_uncore); err != nil {
					applyErrors = append(applyErrors, fmt.Sprintf("error setting uncore for package %d: %v", *dieselect.Package, err))
					continue
				}
				configParts = append(configParts, fmt.Sprintf("Package %d: Min %d, Max %d", *dieselect.Package, *dieselect.Min, *dieselect.Max))
			} else {
				// Die-level tuning.
				pkg := r.PowerLibrary.Topology().Package(*dieselect.Package)
				if pkg == nil {
					applyErrors = append(applyErrors, fmt.Sprintf("invalid package: %d", *dieselect.Package))
					continue
				}
				die := pkg.Die(*dieselect.Die)
				if die == nil {
					applyErrors = append(applyErrors, fmt.Sprintf("invalid die: %d", *dieselect.Die))
					continue
				}
				if err := die.SetUncore(p_uncore); err != nil {
					applyErrors = append(applyErrors, fmt.Sprintf("error setting uncore for package %d die %d: %v", *dieselect.Package, *dieselect.Die, err))
					continue
				}
				configParts = append(configParts, fmt.Sprintf("Package %d Die %d: Min %d, Max %d", *dieselect.Package, *dieselect.Die, *dieselect.Min, *dieselect.Max))
			}
		}
	}

	// Merge conflict errors and apply errors.
	var statusErrors []string
	statusErrors = append(statusErrors, conflictErrors...)
	statusErrors = append(statusErrors, applyErrors...)

	configString := strings.Join(configParts, "; ")
	if err := r.updateUncoreInPowerNodeState(ctx, nodeName, uncore.Name, configString, statusErrors, logger); err != nil {
		return ctrl.Result{}, err
	}

	if len(applyErrors) > 0 {
		return ctrl.Result{}, fmt.Errorf("errors applying uncore config: %s", strings.Join(applyErrors, "; "))
	}
	return ctrl.Result{}, nil
}

// resetUncoreSettings clears all uncore frequency settings from the topology.
func (r *UncoreReconciler) resetUncoreSettings(logger *logr.Logger) error {
	if err := r.PowerLibrary.Topology().SetUncore(nil); err != nil {
		logger.Error(err, "could not reset uncore for topology")
		return err
	}
	packages := r.PowerLibrary.Topology().Packages()
	if packages == nil {
		return nil
	}
	for _, pkg := range *packages {
		if err := pkg.SetUncore(nil); err != nil {
			logger.Error(err, "could not reset uncore on package")
			return err
		}
		dies := pkg.Dies()
		if dies == nil {
			continue
		}
		for _, die := range *dies {
			if err := die.SetUncore(nil); err != nil {
				logger.Error(err, "could not reset uncore on die")
				return err
			}
		}
	}
	return nil
}

// updateUncoreInPowerNodeState writes uncore status to PowerNodeState via SSA.
func (r *UncoreReconciler) updateUncoreInPowerNodeState(
	ctx context.Context,
	nodeName string,
	crName string,
	configString string,
	statusErrors []string,
	logger *logr.Logger,
) error {
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
			Uncore: &powerv1alpha1.NodeUncoreStatus{
				Name:   crName,
				Config: configString,
				Errors: statusErrors,
			},
		},
	}

	if err := r.Status().Patch(ctx, patchNodeState, client.Apply,
		client.FieldOwner(FieldOwnerUncoreController), client.ForceOwnership); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("PowerNodeState %s not found, requeueing", powerNodeStateName)
		}
		return fmt.Errorf("failed to update PowerNodeState uncore status: %w", err)
	}

	logger.Info("updated PowerNodeState uncore status", "config", crName)
	return nil
}

// removeUncoreFromPowerNodeState removes uncore status from PowerNodeState via SSA.
// Setting Uncore to nil causes it to be omitted from the JSON patch (omitempty),
// which makes SSA prune the field from this manager's ownership.
func (r *UncoreReconciler) removeUncoreFromPowerNodeState(ctx context.Context, nodeName string, logger *logr.Logger) error {
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
			// Uncore is nil → omitted from JSON → SSA prunes the field.
		},
	}

	if err := r.Status().Patch(ctx, patchNodeState, client.Apply,
		client.FieldOwner(FieldOwnerUncoreController), client.ForceOwnership); err != nil {
		if errors.IsNotFound(err) {
			logger.V(5).Info("PowerNodeState not found, nothing to remove")
			return nil
		}
		return fmt.Errorf("failed to remove uncore status: %w", err)
	}

	logger.Info("removed uncore status from PowerNodeState")
	return nil
}

// enqueueUncoreReconcile returns a single reconcile request to trigger
// a full re-evaluation of all Uncore CRs for this node.
func (r *UncoreReconciler) enqueueUncoreReconcile(ctx context.Context, _ client.Object) []reconcile.Request {
	uncoreList := &powerv1alpha1.UncoreList{}
	if err := r.List(ctx, uncoreList, client.InNamespace(PowerNamespace)); err != nil {
		r.Log.Error(err, "failed to list Uncores")
		return nil
	}
	if len(uncoreList.Items) == 0 {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name:      uncoreList.Items[0].Name,
			Namespace: uncoreList.Items[0].Namespace,
		},
	}}
}

// SetupWithManager registers the controller and configures watches for
// Uncore CRs and Node label changes.
func (r *UncoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	nodeName := os.Getenv("NODE_NAME")
	return ctrl.NewControllerManagedBy(mgr).
		// Uncore CRUD: re-evaluate which config should be active.
		For(&powerv1alpha1.Uncore{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// Node label changes: labels determine which configs match this node.
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueUncoreReconcile),
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
					return !maps.Equal(oldNode.Labels, newNode.Labels)
				},
			})).
		Complete(r)
}
