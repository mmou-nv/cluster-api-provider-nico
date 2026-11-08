// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestAutoNetworkGeneratedCRDValidation(t *testing.T) {
	for _, kind := range []string{"nicomachines", "nicomachinetemplates"} {
		t.Run(kind, func(t *testing.T) {
			data, err := os.ReadFile("../../config/crd/bases/infrastructure.cluster.x-k8s.io_" + kind + ".yaml")
			require.NoError(t, err)
			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.Unmarshal(data, &crd))
			var props apiextensions.JSONSchemaProps
			require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &props, nil))
			structural, err := schema.NewStructural(&props)
			require.NoError(t, err)
			validator := schemacel.NewValidator(structural, true, celconfig.PerCallLimit)
			wrap := func(spec map[string]any) map[string]any {
				if kind == "nicomachinetemplates" {
					return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": spec}}}
				}
				return map[string]any{"spec": spec}
			}
			decode := func(extra string) map[string]any {
				var spec map[string]any
				require.NoError(t, json.Unmarshal([]byte(`{"vpcID":"vpc-1","machineID":"machine-1"`+extra+`}`), &spec))
				return spec
			}
			for _, tc := range []struct {
				name, fields string
				valid        bool
			}{
				{"legacy subnet", `,"interfaces":[{"subnetID":"subnet-1"}]`, true},
				{"legacy prefix", `,"interfaces":[{"vpcPrefixID":"prefix-1"}]`, true},
				{"explicit false", `,"autoNetwork":false,"interfaces":[{"subnetID":"subnet-1"}]`, true},
				{"auto omitted interfaces", `,"autoNetwork":true`, true},
				{"auto empty interfaces", `,"autoNetwork":true,"interfaces":[]`, true},
				{"legacy missing interfaces", ``, false},
				{"legacy empty interfaces", `,"interfaces":[]`, false},
				{"false empty interfaces", `,"autoNetwork":false,"interfaces":[]`, false},
				{"mixed auto and explicit", `,"autoNetwork":true,"interfaces":[{"subnetID":"subnet-1"}]`, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					errs, _ := validator.Validate(t.Context(), field.NewPath("resource"), structural, wrap(decode(tc.fields)), nil, celconfig.RuntimeCELCostBudget)
					if tc.valid {
						require.Empty(t, errs)
					} else {
						require.NotEmpty(t, errs)
					}
				})
			}
			old := decode(`,"autoNetwork":true`)
			old["providerID"] = "nico://instance-1"
			changed := decode(`,"interfaces":[{"subnetID":"subnet-1"}]`)
			changed["providerID"] = "nico://instance-1"
			errs, _ := validator.Validate(t.Context(), field.NewPath("resource"), structural, wrap(changed), wrap(old), celconfig.RuntimeCELCostBudget)
			require.NotEmpty(t, errs, "network mode cannot change after provisioning")
			errs, _ = validator.Validate(t.Context(), field.NewPath("resource"), structural, wrap(old), wrap(old), celconfig.RuntimeCELCostBudget)
			require.Empty(t, errs, "autoNetwork with omitted interfaces must remain updatable without a spec change")
			if kind == "nicomachines" {
				empty := decode(`,"autoNetwork":true,"interfaces":[]`)
				empty["providerID"] = "nico://instance-1"
				errs, _ = validator.Validate(t.Context(), field.NewPath("resource"), structural, wrap(old), wrap(empty), celconfig.RuntimeCELCostBudget)
				require.Empty(t, errs, "Go omitempty must not turn an empty interface list into an immutable-field change")
				errs, _ = validator.Validate(t.Context(), field.NewPath("resource"), structural, wrap(empty), wrap(old), celconfig.RuntimeCELCostBudget)
				require.Empty(t, errs, "explicitly empty and omitted automatic interfaces are equivalent")
			}
		})
	}
}
