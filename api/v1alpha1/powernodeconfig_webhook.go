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

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=power.cluster-power-manager.github.io,resources=powerprofiles,verbs=get

// +kubebuilder:webhook:path=/validate-power-cluster-power-manager-github-io-v1alpha1-powernodeconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=power.cluster-power-manager.github.io,resources=powernodeconfigs,verbs=create;update,versions=v1alpha1,name=vpowernodeconfig.kb.io,admissionReviewVersions=v1

var powernodeconfiglog = logf.Log.WithName("powernodeconfig-webhook")

// powerNodeConfigValidator implements admission.CustomValidator for PowerNodeConfig.
type powerNodeConfigValidator struct {
	Client    client.Client
	Namespace string
}

// SetupPowerNodeConfigWebhookWithManager registers the validating webhook for PowerNodeConfig.
func SetupPowerNodeConfigWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&PowerNodeConfig{}).
		WithValidator(&powerNodeConfigValidator{Client: mgr.GetClient(), Namespace: GetKPMNamespace()}).
		Complete()
}

var _ webhook.CustomValidator = &powerNodeConfigValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *powerNodeConfigValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	config, ok := obj.(*PowerNodeConfig)
	if !ok {
		return nil, fmt.Errorf("expected PowerNodeConfig, got %T", obj)
	}
	powernodeconfiglog.Info("validating create", "name", config.Name)
	if err := validateNamespace(config.Namespace, v.Namespace, "PowerNodeConfig"); err != nil {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "PowerNodeConfig"},
			config.Name, field.ErrorList{err})
	}
	return v.validateCreateOrUpdate(ctx, config)
}

// ValidateUpdate implements admission.CustomValidator.
func (v *powerNodeConfigValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	config, ok := newObj.(*PowerNodeConfig)
	if !ok {
		return nil, fmt.Errorf("expected PowerNodeConfig, got %T", newObj)
	}
	powernodeconfiglog.Info("validating update", "name", config.Name)
	return v.validateCreateOrUpdate(ctx, config)
}

// ValidateDelete implements admission.CustomValidator.
func (v *powerNodeConfigValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *powerNodeConfigValidator) validateCreateOrUpdate(ctx context.Context, config *PowerNodeConfig) (admission.Warnings, error) {
	var allErrs field.ErrorList

	allErrs = append(allErrs, v.validateReservedCPUDisjoint(config.Spec.ReservedCPUs)...)
	allErrs = append(allErrs, v.validatePowerProfiles(ctx, config)...)
	allErrs = append(allErrs, v.validateNodeSelectorConflicts(ctx, config)...)

	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "PowerNodeConfig"},
			config.Name, allErrs)
	}
	return nil, nil
}

// validateReservedCPUDisjoint checks that no CPU core appears in multiple reservedCPUs entries.
func (v *powerNodeConfigValidator) validateReservedCPUDisjoint(reserved []ReservedSpec) field.ErrorList {
	var errs field.ErrorList
	seen := map[uint]int{}
	for i, rc := range reserved {
		for _, core := range rc.Cores {
			if prevIdx, exists := seen[core]; exists {
				errs = append(errs, field.Invalid(
					field.NewPath("spec", "reservedCPUs").Index(i).Child("cores"),
					core,
					fmt.Sprintf("CPU %d already appears in reservedCPUs[%d]", core, prevIdx)))
			} else {
				seen[core] = i
			}
		}
	}
	return errs
}

// validatePowerProfiles checks that the shared PowerProfile exists and is marked shared,
// and that all reserved PowerProfiles exist.
func (v *powerNodeConfigValidator) validatePowerProfiles(ctx context.Context, config *PowerNodeConfig) field.ErrorList {
	var allErrs field.ErrorList
	sharedFld := field.NewPath("spec", "sharedPowerProfile")
	reservedFld := field.NewPath("spec", "reservedCPUs")

	sharedProfile := &PowerProfile{}
	sharedKey := client.ObjectKey{Name: config.Spec.SharedPowerProfile, Namespace: v.Namespace}
	if err := v.Client.Get(ctx, sharedKey, sharedProfile); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.NotFound(sharedFld, config.Spec.SharedPowerProfile))
		} else {
			allErrs = append(allErrs, field.InternalError(sharedFld, err))
		}
	} else if !sharedProfile.Spec.Shared {
		allErrs = append(allErrs, field.Invalid(
			sharedFld,
			config.Spec.SharedPowerProfile,
			"referenced PowerProfile must have spec.shared set to true"))
	}

	for i, rc := range config.Spec.ReservedCPUs {
		profile := &PowerProfile{}
		key := client.ObjectKey{Name: rc.PowerProfile, Namespace: v.Namespace}
		if err := v.Client.Get(ctx, key, profile); err != nil {
			if apierrors.IsNotFound(err) {
				allErrs = append(allErrs, field.NotFound(reservedFld.Index(i).Child("powerProfile"), rc.PowerProfile))
			} else {
				allErrs = append(allErrs, field.InternalError(reservedFld.Index(i).Child("powerProfile"), err))
			}
		}
	}
	return allErrs
}

// validateNodeSelectorConflicts lists all PowerNodeConfigs and checks for nodeSelector overlap.
func (v *powerNodeConfigValidator) validateNodeSelectorConflicts(ctx context.Context, config *PowerNodeConfig) field.ErrorList {
	list := &PowerNodeConfigList{}
	if err := v.Client.List(ctx, list, client.InNamespace(v.Namespace)); err != nil {
		return field.ErrorList{field.InternalError(field.NewPath("spec", "nodeSelector"), err)}
	}

	peerSelectors := map[string]NodeSelector{}
	for _, item := range list.Items {
		if item.Name == config.Name {
			continue
		}
		peerSelectors[item.Name] = item.Spec.NodeSelector
	}

	return findNodeSelectorConflicts(ctx, v.Client, config.Spec.NodeSelector, peerSelectors)
}
