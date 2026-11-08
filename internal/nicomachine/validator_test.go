// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nicomachine

import (
	"testing"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
	"github.com/stretchr/testify/require"

	infrav1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
)

func TestValidateInstanceAutoNetwork(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		requested, observed, omitObserved, wantError bool
	}{
		{name: "auto readback matches", requested: true, observed: true},
		{name: "explicit legacy readback matches", omitObserved: true},
		{name: "cannot adopt explicit as auto", requested: true, wantError: true},
		{name: "cannot adopt auto as explicit", observed: true, wantError: true},
		{name: "missing auto evidence fails closed", requested: true, omitObserved: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := nicosdk.NewInstance()
			instance.SetVpcId("vpc-1")
			instance.SetMachineId("machine-1")
			if !tc.omitObserved {
				instance.SetAutoNetwork(tc.observed)
			}
			machine := infrav1.NicoMachine{Spec: infrav1.NicoMachineSpec{VPCID: "vpc-1", MachineID: "machine-1", AutoNetwork: tc.requested}}
			err := ValidateInstance(instance, machine, nil)
			if tc.wantError {
				require.ErrorContains(t, err, "autoNetwork does not match")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
