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

// +kubebuilder:webhook:path=/validate-power-cluster-power-manager-github-io-v1alpha1-uncore,mutating=false,failurePolicy=fail,sideEffects=None,groups=power.cluster-power-manager.github.io,resources=uncores,verbs=create;update,versions=v1alpha1,name=vuncore.kb.io,admissionReviewVersions=v1

var uncorelog = logf.Log.WithName("uncore-webhook")

// uncoreValidator implements admission.CustomValidator for Uncore.
type uncoreValidator struct {
	Client    client.Client
	Namespace string
}

// SetupUncoreWebhookWithManager registers the validating webhook for Uncore.
func SetupUncoreWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&Uncore{}).
		WithValidator(&uncoreValidator{Client: mgr.GetClient(), Namespace: GetKPMNamespace()}).
		Complete()
}

var _ webhook.CustomValidator = &uncoreValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *uncoreValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	uncore, ok := obj.(*Uncore)
	if !ok {
		return nil, fmt.Errorf("expected Uncore, got %T", obj)
	}
	uncorelog.Info("validating create", "name", uncore.Name)
	if err := validateNamespace(uncore.Namespace, v.Namespace, "Uncore"); err != nil {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Uncore"},
			uncore.Name, field.ErrorList{err})
	}
	return v.validateCreateOrUpdate(ctx, uncore)
}

// ValidateUpdate implements admission.CustomValidator.
func (v *uncoreValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	uncore, ok := newObj.(*Uncore)
	if !ok {
		return nil, fmt.Errorf("expected Uncore, got %T", newObj)
	}
	uncorelog.Info("validating update", "name", uncore.Name)
	return v.validateCreateOrUpdate(ctx, uncore)
}

// ValidateDelete implements admission.CustomValidator.
func (v *uncoreValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *uncoreValidator) validateCreateOrUpdate(ctx context.Context, uncore *Uncore) (admission.Warnings, error) {
	var allErrs field.ErrorList

	if uncore.Spec.SysMin != nil && uncore.Spec.SysMax != nil && *uncore.Spec.SysMin > *uncore.Spec.SysMax {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "sysMin"),
			*uncore.Spec.SysMin,
			fmt.Sprintf("sysMin (%d) must be <= sysMax (%d)", *uncore.Spec.SysMin, *uncore.Spec.SysMax)))
	}

	if uncore.Spec.DieSelectors != nil {
		allErrs = append(allErrs, validateDieSelectors(*uncore.Spec.DieSelectors)...)
	}

	allErrs = append(allErrs, v.validateNodeSelectorConflicts(ctx, uncore)...)

	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Uncore"},
			uncore.Name, allErrs)
	}
	return nil, nil
}

// validateDieSelectors checks frequency ordering and duplicate (package, die) entries.
func validateDieSelectors(selectors []DieSelector) field.ErrorList {
	var errs field.ErrorList
	fldPath := field.NewPath("spec", "dieSelector")
	// Track package-level (die=nil) and die-specific selectors separately.
	// Package-level + die-specific for the same package is a valid override.
	type pkgDie struct{ pkg, die uint }
	seenPkgLevel := map[uint]int{}
	seenDieLevel := map[pkgDie]int{}

	for i, ds := range selectors {
		idxPath := fldPath.Index(i)

		if ds.Min != nil && ds.Max != nil && *ds.Min > *ds.Max {
			errs = append(errs, field.Invalid(idxPath.Child("min"),
				*ds.Min,
				fmt.Sprintf("min (%d) must be <= max (%d)", *ds.Min, *ds.Max)))
		}

		if ds.Package != nil {
			if ds.Die == nil {
				if prevIdx, exists := seenPkgLevel[*ds.Package]; exists {
					errs = append(errs, field.Duplicate(idxPath,
						fmt.Sprintf("duplicate: package %d already specified in dieSelector[%d]", *ds.Package, prevIdx)))
				} else {
					seenPkgLevel[*ds.Package] = i
				}
			} else {
				key := pkgDie{pkg: *ds.Package, die: *ds.Die}
				if prevIdx, exists := seenDieLevel[key]; exists {
					errs = append(errs, field.Duplicate(idxPath,
						fmt.Sprintf("duplicate: package %d, die %d already specified in dieSelector[%d]", *ds.Package, *ds.Die, prevIdx)))
				} else {
					seenDieLevel[key] = i
				}
			}
		}
	}
	return errs
}

// validateNodeSelectorConflicts lists all Uncores and checks for nodeSelector overlap.
func (v *uncoreValidator) validateNodeSelectorConflicts(ctx context.Context, uncore *Uncore) field.ErrorList {
	list := &UncoreList{}
	if err := v.Client.List(ctx, list, client.InNamespace(v.Namespace)); err != nil {
		return field.ErrorList{field.InternalError(field.NewPath("spec", "nodeSelector"), err)}
	}

	peerSelectors := map[string]NodeSelector{}
	for _, item := range list.Items {
		if item.Name == uncore.Name {
			continue
		}
		peerSelectors[item.Name] = item.Spec.NodeSelector
	}

	return findNodeSelectorConflicts(ctx, v.Client, uncore.Spec.NodeSelector, peerSelectors)
}
