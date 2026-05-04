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

package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultKPMNamespace = "power-manager"

// GetKPMNamespace returns the operator namespace from the KPM_NAMESPACE env var,
// defaulting to "power-manager" if unset.
func GetKPMNamespace() string {
	if ns := os.Getenv("KPM_NAMESPACE"); ns != "" {
		return ns
	}
	return defaultKPMNamespace
}

// validateNamespace rejects the request if the resource is not in the operator namespace.
func validateNamespace(resourceNamespace, expectedNamespace, kind string) *field.Error {
	if resourceNamespace != expectedNamespace {
		return field.Invalid(
			field.NewPath("metadata", "namespace"), resourceNamespace,
			fmt.Sprintf("%s must be created in namespace %q", kind, expectedNamespace))
	}
	return nil
}

// findNodeSelectorConflicts checks whether selfSelector overlaps with any peer
// CR's nodeSelector. It returns field errors for each conflict found.
func findNodeSelectorConflicts(
	ctx context.Context,
	c client.Client,
	selfSelector NodeSelector,
	peerSelectors map[string]NodeSelector,
) field.ErrorList {
	var allErrs field.ErrorList
	var conflicts []string
	var needNodeCheck []string
	fldPath := field.NewPath("spec", "nodeSelector")

	selfLabelSelector, err := metav1.LabelSelectorAsSelector(&selfSelector.LabelSelector)
	if err != nil {
		return append(allErrs, field.Invalid(
			fldPath.Child("labelSelector"), selfSelector.LabelSelector,
			fmt.Sprintf("invalid label selector: %v", err)))
	}

	for peerName, peerSelector := range peerSelectors {
		if reflect.DeepEqual(selfSelector, peerSelector) {
			conflicts = append(conflicts, fmt.Sprintf(
				"conflicts with %q: identical nodeSelector", peerName))
		} else {
			needNodeCheck = append(needNodeCheck, peerName)
		}
	}

	if len(needNodeCheck) > 0 {
		nodeList := &corev1.NodeList{}
		if err := c.List(ctx, nodeList, client.MatchingLabelsSelector{Selector: selfLabelSelector}); err != nil {
			return append(allErrs, field.InternalError(fldPath, fmt.Errorf("listing nodes: %w", err)))
		}

		for _, peerName := range needNodeCheck {
			peerSelector := peerSelectors[peerName]
			peerLabelSelector, err := metav1.LabelSelectorAsSelector(&peerSelector.LabelSelector)
			if err != nil {
				return append(allErrs, field.InternalError(fldPath, fmt.Errorf("parsing nodeSelector for %s: %w", peerName, err)))
			}

			var matchingNodes []string
			for _, node := range nodeList.Items {
				nodeLabels := labels.Set(node.Labels)
				if peerLabelSelector.Matches(nodeLabels) {
					matchingNodes = append(matchingNodes, node.Name)
				}
			}
			if len(matchingNodes) > 0 {
				conflicts = append(conflicts, fmt.Sprintf(
					"conflicts with %q: both select node(s) %s", peerName, joinQuoted(matchingNodes)))
			}
		}
	}

	if len(conflicts) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, selfSelector, strings.Join(conflicts, "; ")))
	}
	return allErrs
}

func joinQuoted(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}
