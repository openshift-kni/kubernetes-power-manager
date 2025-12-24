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
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const queuetime = time.Second * 5

// ValidEppValues defines the valid EPP (Energy Performance Preference) values
var ValidEppValues = []string{"performance", "balance_performance", "balance_power", "power"}

// write errors to the status filed, pass nil to clear errors, will only do update resource is valid and not being deleted
// if object already has the correct errors it will not be updated in the API
func writeUpdatedStatusErrsIfRequired(ctx context.Context, statusWriter client.SubResourceWriter, object powerv1.PowerCRWithStatusErrors, objectErrors error) error {
	var err error
	// if invalid or marked for deletion don't do anything
	if object.GetUID() == "" || object.GetDeletionTimestamp() != nil {
		return err
	}
	errList := util.UnpackErrsToStrings(objectErrors)
	// no updates are needed
	if equality.Semantic.DeepEqual(*errList, *object.GetStatusErrors()) {
		return err
	}

	// Use Patch for safer status update across multiple agents
	orig, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object does not implement client.Object")
	}
	statusPatch := client.MergeFrom(orig)

	object.SetStatusErrors(errList)
	err = statusWriter.Patch(ctx, object, statusPatch)
	if err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "failed to write status update")
	}
	return err
}

// isValidEpp checks if a certain name corresponds to a valid EPP value.
func isValidEpp(inputName string) bool {
	for _, validEpp := range ValidEppValues {
		if inputName == validEpp {
			return true
		}
	}
	return false
}

// doesNodeMatchPowerProfileSelector checks if a PowerProfile should be applied to a specific node.
func doesNodeMatchPowerProfileSelector(c client.Client, profile *powerv1.PowerProfile, nodeName string, logger *logr.Logger) (bool, error) {
	logger.V(5).Info("Checking if PowerProfile should be applied to node", "profile", profile.Spec.Name, "nodeName", nodeName)
	// If no label selector is specified, apply to all nodes.
	labelSelector := profile.Spec.NodeSelector.LabelSelector
	if len(labelSelector.MatchLabels) == 0 && len(labelSelector.MatchExpressions) == 0 {
		logger.V(5).Info("No label selector specified, applying PowerProfile to all nodes", "profile", profile.Spec.Name, "nodeName", nodeName)
		return true, nil
	}

	// Get the node to check its labels.
	node := &corev1.Node{}
	err := c.Get(context.TODO(), client.ObjectKey{Name: nodeName}, node)
	if err != nil {
		// If we can't get the node, don't apply the profile.
		logger.Error(err, "Failed to get node for selector validation", "nodeName", nodeName)
		return false, err
	}

	// Convert the label selector to a Selector and check if it matches the node.
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		logger.Error(err, "Failed to convert label selector", "selector", labelSelector)
		return false, err
	}

	// Check if the node's labels match the selector.
	res := selector.Matches(labels.Set(node.Labels))
	logger.V(5).Info("Node label check result", "nodeName", nodeName, "selector", selector, "nodeLabels", node.Labels, "result", res)
	return res, nil
}

// validateProfileAvailabilityOnNode validates that a PowerProfile exists in the cluster and is available to be used on the node
func validateProfileAvailabilityOnNode(ctx context.Context, c client.Client, profileName string, nodeName string, powerLibrary power.Host, logger *logr.Logger) (bool, error) {
	if profileName == "" {
		return true, nil
	}

	powerProfile := &powerv1.PowerProfile{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      profileName,
		Namespace: IntelPowerNamespace,
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
	match, err := doesNodeMatchPowerProfileSelector(c, powerProfile, nodeName, logger)
	if err != nil {
		logger.Error(err, "error checking if node matches power profile selector")
		return false, err
	}
	return match, nil
}

// formatIntOrStringValue extracts the actual value from IntOrString as a string
// Returns the integer value as string for Int type, or the string value for String type
func formatIntOrStringValue(value intstr.IntOrString) string {
	if value.Type == intstr.Int {
		return strconv.Itoa(int(value.IntVal))
	}
	return value.StrVal
}
