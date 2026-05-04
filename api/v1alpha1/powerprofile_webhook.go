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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var extendedResourcePrefix = GroupVersion.Group + "/"

const podPowerProfileIndex = ".spec.containers.resources.powerProfile"

// +kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=powernodeconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// +kubebuilder:webhook:path=/validate-power-cluster-power-manager-github-io-v1alpha1-powerprofile,mutating=false,failurePolicy=fail,sideEffects=None,groups=power.cluster-power-manager.github.io,resources=powerprofiles,verbs=create;update;delete,versions=v1alpha1,name=vpowerprofile.kb.io,admissionReviewVersions=v1

var powerprofilelog = logf.Log.WithName("powerprofile-webhook")

// powerProfileValidator implements admission.CustomValidator for PowerProfile.
type powerProfileValidator struct {
	Client    client.Client
	Namespace string
}

// SetupPowerProfileWebhookWithManager registers the validating webhook for PowerProfile.
func SetupPowerProfileWebhookWithManager(mgr ctrl.Manager) error {
	// Index pods by power profile name for fast lookup during delete validation.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, podPowerProfileIndex, extractPowerProfiles); err != nil {
		return fmt.Errorf("setting up pod power profile index: %w", err)
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&PowerProfile{}).
		WithValidator(&powerProfileValidator{Client: mgr.GetClient(), Namespace: GetKPMNamespace()}).
		Complete()
}

// extractPowerProfiles returns the power profile names requested by an active pod's containers.
// Completed or failed pods no longer hold CPU resources, so they are excluded.
func extractPowerProfiles(obj client.Object) []string {
	pod := obj.(*corev1.Pod)
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return nil
	}
	var profiles []string
	containers := make([]corev1.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	containers = append(containers, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)
	for _, c := range containers {
		for rName := range c.Resources.Requests {
			if strings.HasPrefix(string(rName), extendedResourcePrefix) {
				profiles = append(profiles, strings.TrimPrefix(string(rName), extendedResourcePrefix))
			}
		}
	}
	return profiles
}

var _ webhook.CustomValidator = &powerProfileValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *powerProfileValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	profile, ok := obj.(*PowerProfile)
	if !ok {
		return nil, fmt.Errorf("expected PowerProfile, got %T", obj)
	}
	powerprofilelog.Info("validating create", "name", profile.Name)
	if err := validateNamespace(profile.Namespace, v.Namespace, "PowerProfile"); err != nil {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "PowerProfile"},
			profile.Name, field.ErrorList{err})
	}
	return nil, nil
}

// ValidateUpdate implements admission.CustomValidator.
func (v *powerProfileValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldProfile, ok := oldObj.(*PowerProfile)
	if !ok {
		return nil, fmt.Errorf("expected PowerProfile, got %T", oldObj)
	}
	newProfile, ok := newObj.(*PowerProfile)
	if !ok {
		return nil, fmt.Errorf("expected PowerProfile, got %T", newObj)
	}
	powerprofilelog.Info("validating update", "name", newProfile.Name)

	if oldProfile.Spec.Shared && !newProfile.Spec.Shared {
		if err := v.validatePowerNodeConfigReferences(ctx, newProfile, true); err != nil {
			return nil, apierrors.NewForbidden(
				schema.GroupResource{Group: GroupVersion.Group, Resource: "powerprofiles"},
				newProfile.Name,
				fmt.Errorf("cannot change spec.shared from true to false, %s", err.Error()))
		}
	}
	return nil, nil
}

// ValidateDelete implements admission.CustomValidator.
func (v *powerProfileValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	profile, ok := obj.(*PowerProfile)
	if !ok {
		return nil, fmt.Errorf("expected PowerProfile, got %T", obj)
	}
	powerprofilelog.Info("validating delete", "name", profile.Name)

	var reasons []string
	if err := v.validatePowerNodeConfigReferences(ctx, profile, false); err != nil {
		reasons = append(reasons, err.Error())
	}
	if err := v.validatePodReferences(ctx, profile); err != nil {
		reasons = append(reasons, err.Error())
	}

	if len(reasons) > 0 {
		return nil, apierrors.NewForbidden(
			schema.GroupResource{Group: GroupVersion.Group, Resource: "powerprofiles"},
			profile.Name, fmt.Errorf("cannot delete PowerProfile, %s", strings.Join(reasons, "; ")))
	}
	return nil, nil
}

// validatePowerNodeConfigReferences returns an error if any PowerNodeConfig references the profile.
// When sharedOnly is true, only sharedPowerProfile references are checked.
func (v *powerProfileValidator) validatePowerNodeConfigReferences(ctx context.Context, profile *PowerProfile, sharedOnly bool) error {
	list := &PowerNodeConfigList{}
	if err := v.Client.List(ctx, list, client.InNamespace(v.Namespace)); err != nil {
		return fmt.Errorf("listing PowerNodeConfigs: %w", err)
	}

	var refs []string
	for _, pnc := range list.Items {
		if pnc.Spec.SharedPowerProfile == profile.Name {
			refs = append(refs, fmt.Sprintf("%s (sharedPowerProfile)", pnc.Name))
			continue
		}
		if sharedOnly {
			continue
		}
		for i, rc := range pnc.Spec.ReservedCPUs {
			if rc.PowerProfile == profile.Name {
				refs = append(refs, fmt.Sprintf("%s (reservedCPUs[%d])", pnc.Name, i))
				break
			}
		}
	}
	if len(refs) > 0 {
		return fmt.Errorf("referenced by PowerNodeConfig(s): %s", strings.Join(refs, ", "))
	}
	return nil
}

// validatePodReferences returns an error if any active Pod requests the profile as an extended resource.
func (v *powerProfileValidator) validatePodReferences(ctx context.Context, profile *PowerProfile) error {
	podList := &corev1.PodList{}
	if err := v.Client.List(ctx, podList, client.MatchingFields{podPowerProfileIndex: profile.Name}); err != nil {
		return fmt.Errorf("listing Pods: %w", err)
	}

	var refs []string
	for _, pod := range podList.Items {
		refs = append(refs, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
	}
	if len(refs) > 0 {
		return fmt.Errorf("referenced by Pod(s): %s", strings.Join(refs, ", "))
	}
	return nil
}
