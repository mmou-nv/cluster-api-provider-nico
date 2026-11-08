// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"fmt"
	"net"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
)

func autoNetworkHostIPv4(instance *nicosdk.Instance, machine *nicosdk.Machine) (string, error) {
	if instance == nil || machine == nil || !instance.GetAutoNetwork() ||
		instance.GetId() == "" || instance.GetMachineId() == "" || instance.GetSiteId() == "" ||
		machine.GetId() != instance.GetMachineId() || machine.GetInstanceId() != instance.GetId() ||
		machine.GetSiteId() != instance.GetSiteId() {
		return "", fmt.Errorf("autoNetwork host identity does not match the instance")
	}
	if !machine.HasAssociatedDpuMachineIds() || len(machine.GetAssociatedDpuMachineIds()) != 0 {
		return "", fmt.Errorf("autoNetwork host-IP discovery requires confirmed zero-DPU hardware")
	}
	var primary []nicosdk.MachineInterface
	for _, iface := range machine.GetMachineInterfaces() {
		if iface.GetIsPrimary() {
			primary = append(primary, iface)
		}
	}
	if len(primary) != 1 || primary[0].GetMachineId() != machine.GetId() || primary[0].GetAttachedDpuMachineID() != "" {
		return "", fmt.Errorf("autoNetwork host needs exactly one bound primary host interface")
	}
	addresses := map[string]struct{}{}
	for _, value := range primary[0].GetIpAddresses() {
		ip := net.ParseIP(value)
		if ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() {
			addresses[ip.To4().String()] = struct{}{}
		}
	}
	if len(addresses) != 1 {
		return "", fmt.Errorf("autoNetwork primary host interface needs exactly one usable IPv4 address")
	}
	for ip := range addresses {
		return ip, nil
	}
	return "", fmt.Errorf("autoNetwork host address is unavailable")
}
