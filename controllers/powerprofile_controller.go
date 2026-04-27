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
	"strings"

	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// performance          ===>  priority level 0
// balance_performance  ===>  priority level 1
// balance_power        ===>  priority level 2
// power                ===>  priority level 3

// PowerProfileReconciler reconciles a PowerProfile object
type PowerProfileReconciler struct {
	client.Client
	Log          logr.Logger
	Scheme       *runtime.Scheme
	PowerLibrary power.Host
}

// +kubebuilder:rbac:groups=power.openshift.io,resources=powerprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=power.openshift.io,resources=powerprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=power.openshift.io,resources=powernodestates,verbs=get;list;watch
// +kubebuilder:rbac:groups=power.openshift.io,resources=powernodestates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

// Reconcile method that implements the reconcile loop
func (r *PowerProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := r.Log.WithValues("powerprofile", req.NamespacedName)
	logger.Info("Reconciling the power profile")

	var err error
	// Set to true if the PowerProfile matches this node.
	var shouldUpdatePowerNodeState bool

	if req.Namespace != PowerNamespace {
		err = fmt.Errorf("incorrect namespace")
		logger.Error(err, "resource is not in the power-manager namespace, ignoring")
		return ctrl.Result{}, err
	}

	// Node name is passed down via the downwards API and used to make sure the PowerProfile is for this node
	nodeName := os.Getenv("NODE_NAME")
	profile := &powerv1.PowerProfile{}

	// Defer the function to update the PowerNodeState with any errors.
	defer func() {
		// Only update PowerNodeState if the profile's nodeSelector matches this node.
		if !shouldUpdatePowerNodeState {
			return
		}

		// Write any errors (validation or infrastructure) to PowerNodeState.
		// If the status update fails and the main reconcile succeeded, requeue after a short delay to retry.
		// We use RequeueAfter instead of returning an error because the profile reconcile itself succeeded.
		if updateErr := addPowerNodeStatusProfileEntry(ctx, r.Client, nodeName, profile, err, &logger); updateErr != nil {
			logger.Error(updateErr, "failed to update PowerNodeState with profile errors")
			if retErr == nil {
				result = ctrl.Result{RequeueAfter: queuetime}
			}
		}
	}()
	err = r.Client.Get(ctx, req.NamespacedName, profile)
	logger.V(5).Info("retrieving the power profile instances")
	if err != nil {
		if errors.IsNotFound(err) {
			// First we need to remove the profile from the Power library, this will in turn remove the pool,
			// which will also move the cores back to the Shared/Default pool and reconfigure them. We then
			// need to remove the Power Workload from the cluster, which in this case will do nothing as
			// everything has already been removed. Finally, we remove the Extended Resources from the Node
			// first we make sure the profile isn't the one used by the shared pool
			if r.PowerLibrary.GetSharedPool().GetPowerProfile() != nil && req.Name == r.PowerLibrary.GetSharedPool().GetPowerProfile().Name() {
				err := r.PowerLibrary.GetSharedPool().SetPowerProfile(nil)
				if err != nil {
					logger.Error(err, "error setting nil profile")
					return ctrl.Result{}, err
				}
				pool := r.PowerLibrary.GetExclusivePool(req.Name)
				if pool == nil {
					notFoundErr := fmt.Errorf("pool not found")
					logger.Error(notFoundErr, fmt.Sprintf("attempted to remove the non existing pool %s", req.Name))
					return ctrl.Result{}, notFoundErr
				}
				err = pool.Remove()
				if err != nil {
					logger.Error(err, "error deleting the power profile from the library")
					return ctrl.Result{}, err
				}

				// Remove the profile from PowerNodeState.
				err = removePowerNodeStatusProfileEntry(ctx, r.Client, nodeName, req.Name, &logger)
				if err != nil {
					logger.Error(err, "error removing profile from PowerNodeState")
					// Pool was already removed, but requeue to retry the status cleanup.
					return ctrl.Result{RequeueAfter: queuetime}, nil
				}

				return ctrl.Result{}, nil
			}

			// Remove the extended resources for this power profile from the node.
			err = r.removeExtendedResources(ctx, nodeName, req.NamespacedName.Name, &logger)
			if err != nil {
				logger.Error(err, "error removing the extended resources from the node")
				return ctrl.Result{}, err
			}

			// Remove the profile from PowerNodeState.
			err = removePowerNodeStatusProfileEntry(ctx, r.Client, nodeName, req.NamespacedName.Name, &logger)
			if err != nil {
				logger.Error(err, "error removing profile from PowerNodeState")
				// Extended resources were already cleaned up, but requeue to retry the status cleanup.
				return ctrl.Result{RequeueAfter: queuetime}, nil
			}

			return ctrl.Result{}, nil
		}

		// Requeue the request.
		return ctrl.Result{}, err
	}

	// Check if this profile should be applied to this node. The check applies to both shared and non-shared profiles.
	match, err := nodeMatchesPowerProfile(ctx, r.Client, profile, nodeName, &logger)
	if err != nil {
		logger.Error(err, "error checking if node matches power profile selector")
		return ctrl.Result{}, err
	}
	if !match {
		logger.V(5).Info("Profile not applicable to this node due to node selector", "nodeName", nodeName, "nodeSelector", profile.Spec.NodeSelector)

		// Clean up resources if they exist on this node but shouldn't anymore.
		err = r.cleanupProfileFromNode(ctx, profile, nodeName, &logger)
		if err != nil {
			logger.Error(err, "error cleaning up profile resources from node, will retry")
			// Requeue to retry status cleanup; extended resources were already removed.
			return ctrl.Result{RequeueAfter: queuetime}, nil
		}

		// Don't update PowerNodeState since this profile doesn't apply to this node.
		return ctrl.Result{}, nil
	}

	// Profile matches this node - PowerNodeState should be updated.
	shouldUpdatePowerNodeState = true

	// Make sure the EPP value is one of the correct ones or empty in the case of a user-created profile.
	logger.V(5).Info("confirming EPP value is one of the correct values")
	if profile.Spec.PStates.Epp != "" {
		isValid := isValidEpp(profile.Spec.PStates.Epp)

		if !isValid {
			err = errors.NewServiceUnavailable(fmt.Sprintf("EPP value not allowed: %v", profile.Spec.PStates.Epp))
			logger.Error(err, "error reconciling the power profile")

			return ctrl.Result{}, err
		}
	}

	// Validate the EPP value.
	actualEpp := profile.Spec.PStates.Epp
	if !power.IsFeatureSupported(power.EPPFeature) && actualEpp != "" {
		err = fmt.Errorf("EPP is not supported but %s provides one, setting EPP to ''", profile.Name)
		logger.Error(err, "invalid EPP")
		actualEpp = ""
	}

	// Create and validate power profile in the power library
	powerProfile, err := power.NewPowerProfile(
		profile.Name, profile.Spec.PStates.Min, profile.Spec.PStates.Max,
		profile.Spec.PStates.Governor, actualEpp,
		profile.Spec.CStates.Names, profile.Spec.CStates.MaxLatencyUs)
	if err != nil {
		logger.Error(err, "could not create the power profile")
		return ctrl.Result{}, err
	}
	// An exclusive pool should be created for both shared and non-shared profiles.
	profileFromLibrary := r.PowerLibrary.GetExclusivePool(profile.Name)
	if profileFromLibrary == nil {
		pool, err := r.PowerLibrary.AddExclusivePool(profile.Name)
		if err != nil {
			logger.Error(err, "failed to create the power profile")
			return ctrl.Result{}, err
		}
		err = pool.SetPowerProfile(powerProfile)
		if err != nil {
			logger.Error(err, fmt.Sprintf("error adding the profile '%s' to the power library for host '%s'", profile.Name, nodeName))
			return ctrl.Result{}, err
		}

		logger.V(5).Info("power profile successfully created", "profile", profile.Name)
	} else {
		// Exclusive pool for this profile already exists, update it and all the other pools that use this profile
		err = r.PowerLibrary.GetExclusivePool(profile.Name).SetPowerProfile(powerProfile)
		msg := fmt.Sprintf("updating the power profile '%s' to the power library for node '%s'", profile.Name, nodeName)
		logger.V(5).Info(msg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("error %s: %v", msg, err)
		}

		// Update shared pool if it uses this profile
		sharedPool := r.PowerLibrary.GetSharedPool()
		if sharedPool.GetPowerProfile() != nil && sharedPool.GetPowerProfile().Name() == profile.Name {
			msg := fmt.Sprintf("updating shared pool in power library with updated profile '%s' for node '%s'", profile.Name, nodeName)
			logger.V(5).Info(msg)
			err := sharedPool.SetPowerProfile(powerProfile)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("error %s: %v", msg, err)
			}
		}

		// Update any special reserved pools created for reservedCPUs that use this profile
		exclusivePools := r.PowerLibrary.GetAllExclusivePools()
		for _, pool := range *exclusivePools {
			if strings.Contains(pool.Name(), nodeName+"-reserved-") &&
				pool.GetPowerProfile() != nil &&
				pool.GetPowerProfile().Name() == profile.Name {
				msg := fmt.Sprintf("updating special reserved pool '%s' in power library with updated profile '%s' for node '%s'", pool.Name(), profile.Name, nodeName)
				logger.V(5).Info(msg)
				err := pool.SetPowerProfile(powerProfile)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("error %s: %v", msg, err)
				}
			}
		}

		logger.V(5).Info(fmt.Sprintf(
			"power profile successfully updated: name - %s max - %d min - %d EPP - %s",
			profile.Name, powerProfile.GetPStates().GetMaxFreq().IntVal, powerProfile.GetPStates().GetMinFreq().IntVal, actualEpp))
	}

	if profile.Spec.Shared {
		// Return for shared profiles, as extended resources and workloads are not created for them
		return ctrl.Result{}, nil
	}

	// Create or update the extended resources for the profile.
	err = r.ensureExtendedResources(ctx, nodeName, profile, &logger)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error creating or updating the extended resources for the base profile: %v", err)
	}

	// If the workload already exists then the power profile was just updated and the power library will take care of reconfiguring cores
	return ctrl.Result{}, nil
}

func (r *PowerProfileReconciler) ensureExtendedResources(ctx context.Context, nodeName string, profile *powerv1.PowerProfile, logger *logr.Logger) error {
	node := &corev1.Node{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Name: nodeName,
	}, node)
	if err != nil {
		return err
	}

	totalCPUs := len(*r.PowerLibrary.GetAllCpus())
	logger.V(0).Info("configuring based on the capacity associated to the specific power profile")

	// Calculate CPU count based on profile's CpuCapacity or default to all CPUs.
	var numExtendedResources int64
	if profile.Spec.CpuCapacity.String() != "" {
		// Use the standard library function to handle IntOrString properly.
		absoluteCPUs, err := intstr.GetScaledValueFromIntOrPercent(&profile.Spec.CpuCapacity, totalCPUs, false)
		if err == nil && absoluteCPUs > 0 {
			numExtendedResources = int64(absoluteCPUs)
		} else {
			// Fallback to all CPUs if parsing fails
			logger.Error(err, "could not parse power profile cpu capacity, using total CPUs", "error", err, "absoluteCPUs", absoluteCPUs)
			numExtendedResources = int64(totalCPUs)
		}
	} else {
		// Default to all CPUs if no configuration found.
		numExtendedResources = int64(totalCPUs)
	}

	profilesAvailable := resource.NewQuantity(numExtendedResources, resource.DecimalSI)
	extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, profile.Name))
	node.Status.Capacity[extendedResourceName] = *profilesAvailable

	err = r.Client.Status().Update(ctx, node)
	if err != nil {
		return err
	}

	return nil
}

func (r *PowerProfileReconciler) removeExtendedResources(ctx context.Context, nodeName string, profileName string, logger *logr.Logger) error {
	node := &corev1.Node{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Name: nodeName,
	}, node)
	if err != nil {
		return err
	}

	logger.V(5).Info("removing the extended resources")
	newNodeCapacityList := make(map[corev1.ResourceName]resource.Quantity)
	extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, profileName))
	for resourceFromNode, numberOfResources := range node.Status.Capacity {
		if resourceFromNode == extendedResourceName {
			continue
		}
		newNodeCapacityList[resourceFromNode] = numberOfResources
	}

	node.Status.Capacity = newNodeCapacityList
	err = r.Client.Status().Update(ctx, node)
	if err != nil {
		return err
	}

	return nil
}

// cleanupProfileFromNode removes only extended resources from a node when it no longer matches the PowerProfile selector.
func (r *PowerProfileReconciler) cleanupProfileFromNode(ctx context.Context, profile *powerv1.PowerProfile, nodeName string, logger *logr.Logger) error {
	logger.V(5).Info("Cleaning up PowerProfile extended resources from node", "profile", profile.Name, "nodeName", nodeName)

	// Only remove extended resources from the node.
	// Keep pools, workloads, and shared PowerProfile configurations as pods/services may depend on them.
	err := r.removeExtendedResources(ctx, nodeName, profile.Name, logger)
	if err != nil {
		logger.Error(err, "error removing extended resources")
		return err
	}

	// Remove the profile from PowerNodeState since it no longer applies to this node.
	err = removePowerNodeStatusProfileEntry(ctx, r.Client, nodeName, profile.Name, logger)
	if err != nil {
		logger.Error(err, "error removing profile from PowerNodeState")
		// Return the error so the caller can requeue to retry status cleanup.
		// Extended resources were already cleaned up successfully.
		return err
	}

	logger.V(5).Info("Successfully cleaned up PowerProfile extended resources from node", "profile", profile.Name, "nodeName", nodeName)
	return nil
}

// SetupWithManager specifies how the controller is built and watch a CR and other resources that are owned and managed by the controller
func (r *PowerProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&powerv1.PowerProfile{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToProfileRequests),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					// Filter out the nodes that are not the current node.
					if e.ObjectOld.GetName() != os.Getenv("NODE_NAME") {
						return false
					}
					// Filter for label updates only.
					return !equality.Semantic.DeepEqual(e.ObjectNew.GetLabels(), e.ObjectOld.GetLabels())
				},
				CreateFunc:  func(e event.CreateEvent) bool { return true },
				GenericFunc: func(ge event.GenericEvent) bool { return false },
				DeleteFunc: func(de event.DeleteEvent) bool {
					// Filter the current node.
					return de.Object.GetName() == os.Getenv("NODE_NAME")
				},
			})).
		Complete(r)
}

// nodeToProfileRequests maps Node events to PowerProfile reconciliation requests
func (r *PowerProfileReconciler) nodeToProfileRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	node := obj.(*corev1.Node)
	r.Log.V(5).Info("Node change detected, checking PowerProfiles", "nodeName", node.Name)

	var requests []reconcile.Request

	// List all PowerProfiles.
	powerProfiles := &powerv1.PowerProfileList{}
	if err := r.Client.List(ctx, powerProfiles); err != nil {
		r.Log.Error(err, "Failed to list PowerProfiles for node event handling")
		return requests
	}

	// Enqueue reconciliation for all PowerProfiles that might be affected.
	for _, profile := range powerProfiles.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      profile.Name,
				Namespace: profile.Namespace,
			},
		})
	}

	r.Log.V(5).Info("Enqueuing PowerProfile reconciliation requests", "count", len(requests), "nodeName", node.Name)
	return requests
}
