/*
Copyright 2026.

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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ---------------------------------------------------------------------------
// Defaulter
// ---------------------------------------------------------------------------

// NodePoolDefaulter implements admission.CustomDefaulter for NodePool.
type NodePoolDefaulter struct{}

var _ admission.CustomDefaulter = &NodePoolDefaulter{}

func (d *NodePoolDefaulter) Default(_ context.Context, obj runtime.Object) error {
	np := obj.(*NodePool)

	if np.Spec.Limits.MinNodes == nil {
		np.Spec.Limits.MinNodes = ptr.To[int32](0)
	}

	if np.Spec.Disruption.TTLSecondsAfterEmpty == 0 {
		np.Spec.Disruption.TTLSecondsAfterEmpty = 300
	}

	if len(np.Spec.Requirements.Arch) == 0 {
		np.Spec.Requirements.Arch = []string{"x86_64"}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Validator
// ---------------------------------------------------------------------------

// NodePoolValidator implements admission.CustomValidator for NodePool.
type NodePoolValidator struct{}

var _ admission.CustomValidator = &NodePoolValidator{}

func (v *NodePoolValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	np := obj.(*NodePool)
	return nil, validateNodePoolSpec(&np.Spec).ToAggregate()
}

func (v *NodePoolValidator) ValidateUpdate(_ context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	np := newObj.(*NodePool)
	return nil, validateNodePoolSpec(&np.Spec).ToAggregate()
}

func (v *NodePoolValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateNodePoolSpec(spec *NodePoolSpec) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")

	if spec.Limits.MinNodes != nil && spec.Limits.MaxNodes < *spec.Limits.MinNodes {
		errs = append(errs, field.Invalid(
			specPath.Child("limits", "maxNodes"),
			spec.Limits.MaxNodes,
			"must be >= limits.minNodes",
		))
	}

	if spec.Requirements.MaxPriceEurPerHour <= 0 {
		errs = append(errs, field.Invalid(
			specPath.Child("requirements", "maxPriceEurPerHour"),
			spec.Requirements.MaxPriceEurPerHour,
			"must be > 0",
		))
	}

	if spec.Disruption.TTLSecondsAfterEmpty < 0 {
		errs = append(errs, field.Invalid(
			specPath.Child("disruption", "ttlSecondsAfterEmpty"),
			spec.Disruption.TTLSecondsAfterEmpty,
			"must be >= 0",
		))
	}

	return errs
}
