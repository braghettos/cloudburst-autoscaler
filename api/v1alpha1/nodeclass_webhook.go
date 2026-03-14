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
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var semverRegexp = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[\w.]+)?(\+[\w.]+)?$`)

// NodeClassValidator implements admission.CustomValidator for NodeClass.
type NodeClassValidator struct{}

var _ admission.CustomValidator = &NodeClassValidator{}

func (v *NodeClassValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	nc := obj.(*NodeClass)
	return nil, validateNodeClassSpec(&nc.Spec).ToAggregate()
}

func (v *NodeClassValidator) ValidateUpdate(_ context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	nc := newObj.(*NodeClass)
	return nil, validateNodeClassSpec(&nc.Spec).ToAggregate()
}

func (v *NodeClassValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateNodeClassSpec(spec *NodeClassSpec) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")

	if spec.Join.HostApiServer == "" {
		errs = append(errs, field.Required(specPath.Child("join", "hostApiServer"), "must be set"))
	}

	if spec.Bootstrap != nil && spec.Bootstrap.KubernetesVersion != "" {
		if !semverRegexp.MatchString(spec.Bootstrap.KubernetesVersion) {
			errs = append(errs, field.Invalid(
				specPath.Child("bootstrap", "kubernetesVersion"),
				spec.Bootstrap.KubernetesVersion,
				fmt.Sprintf("must be valid semver (e.g. 1.29.0 or v1.29.0)"),
			))
		}
	}

	return errs
}
