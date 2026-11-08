// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-api-provider-nico/internal/fake"
	"github.com/dsx-ai-factory/cluster-api-provider-nico/internal/nico"
	"github.com/dsx-ai-factory/cluster-api-provider-nico/internal/nicomachine"
)

func TestBuildInstanceCreateRequestAutoNetwork(t *testing.T) {
	for _, tc := range []struct {
		name       string
		auto       bool
		interfaces []infrav1.NicoMachineInterface
		wantError  bool
	}{
		{name: "auto omitted interfaces", auto: true},
		{name: "auto empty interfaces", auto: true, interfaces: []infrav1.NicoMachineInterface{}},
		{name: "legacy subnet", interfaces: []infrav1.NicoMachineInterface{{SubnetID: "subnet-1"}}},
		{name: "legacy prefix", interfaces: []infrav1.NicoMachineInterface{{VPCPrefixID: "prefix-1"}}},
		{name: "legacy missing interfaces", wantError: true},
		{name: "mixed auto and explicit", auto: true, interfaces: []infrav1.NicoMachineInterface{{SubnetID: "subnet-1"}}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := &infrav1.NicoMachine{Spec: infrav1.NicoMachineSpec{
				VPCID: "vpc-1", MachineID: "machine-1", AutoNetwork: tc.auto, Interfaces: tc.interfaces,
			}}
			request, err := buildInstanceCreateRequest("cp-1", "tenant-1", machine, "cluster-1", "#cloud-config\n", nicomachine.InstanceTypeCapabilities{})
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, request)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "machine-1", request.GetMachineId())
			require.Equal(t, "#cloud-config\n", request.GetUserData())
			body, err := json.Marshal(request)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body, &fields))
			if tc.auto {
				require.Equal(t, "true", string(fields["autoNetwork"]))
				require.Equal(t, "[]", string(fields["interfaces"]))
			} else {
				require.NotContains(t, fields, "autoNetwork", "legacy requests must remain unchanged")
				require.NotEmpty(t, request.GetInterfaces())
			}
		})
	}
}

func TestAutoNetworkSDKHTTPPayload(t *testing.T) {
	api := fake.New()
	api.SeedToken("synthetic-token")
	tenant := nicosdk.NewTenant()
	tenant.SetId("tenant-1")
	api.SeedTenant("org-1", *tenant)
	vpc := nicosdk.NewVPC()
	vpc.SetId("vpc-1")
	vpc.SetSiteId("site-1")
	api.SeedVPC("org-1", *vpc)
	instanceType := nicosdk.NewInstanceType()
	instanceType.SetId("type-1")
	api.SeedInstanceType("org-1", *instanceType)

	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodies <- body
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		api.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := nico.NewClient(t.Context(), nico.SecretConfig{Endpoint: server.URL, OrgID: "org-1", Token: "synthetic-token"})
	require.NoError(t, err)
	machine := &infrav1.NicoMachine{Spec: infrav1.NicoMachineSpec{VPCID: "vpc-1", InstanceTypeID: "type-1", AutoNetwork: true}}
	request, err := buildInstanceCreateRequest("cp-1", "tenant-1", machine, "cluster-1", "#cloud-config\n", nicomachine.InstanceTypeCapabilities{})
	require.NoError(t, err)
	instance, err := client.CreateInstance(t.Context(), *request, nico.InstancePlacement{})
	require.NoError(t, err)
	require.True(t, instance.GetAutoNetwork())
	require.Empty(t, instance.GetInterfaces())
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(<-bodies, &payload))
	require.Equal(t, "true", string(payload["autoNetwork"]))
	require.Equal(t, "[]", string(payload["interfaces"]))
	require.Equal(t, `"vpc-1"`, string(payload["vpcId"]))
	require.Equal(t, `"type-1"`, string(payload["instanceTypeId"]))
}

func TestAutoNetworkHostIPv4(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*nicosdk.Instance, *nicosdk.Machine)
		wantError bool
	}{
		{name: "confirmed primary zero-DPU host"},
		{name: "different machine", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.SetId("other-machine") }, wantError: true},
		{name: "different instance", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.SetInstanceId("other-instance") }, wantError: true},
		{name: "different site", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.SetSiteId("other-site") }, wantError: true},
		{name: "not automatic", change: func(i *nicosdk.Instance, _ *nicosdk.Machine) { i.SetAutoNetwork(false) }, wantError: true},
		{name: "unknown DPU inventory", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.AssociatedDpuMachineIds = nil }, wantError: true},
		{name: "DPU present", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.SetAssociatedDpuMachineIds([]string{"dpu-1"}) }, wantError: true},
		{name: "primary absent", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.MachineInterfaces[0].SetIsPrimary(false) }, wantError: true},
		{name: "primary ambiguous", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) {
			m.MachineInterfaces = append(m.MachineInterfaces, m.MachineInterfaces[0])
		}, wantError: true},
		{name: "interface identity mismatch", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.MachineInterfaces[0].SetMachineId("other-machine") }, wantError: true},
		{name: "interface attached to DPU", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) { m.MachineInterfaces[0].SetAttachedDpuMachineID("dpu-1") }, wantError: true},
		{name: "no usable IP", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) {
			m.MachineInterfaces[0].SetIpAddresses([]string{"bad", "127.0.0.1", "169.254.1.1", "::1"})
		}, wantError: true},
		{name: "ambiguous IPv4", change: func(_ *nicosdk.Instance, m *nicosdk.Machine) {
			m.MachineInterfaces[0].SetIpAddresses([]string{"10.0.0.15", "10.0.0.16"})
		}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := nicosdk.NewInstance()
			instance.SetId("instance-1")
			instance.SetMachineId("machine-1")
			instance.SetSiteId("site-1")
			instance.SetAutoNetwork(true)
			machine := nicosdk.NewMachine()
			machine.SetId("machine-1")
			machine.SetInstanceId("instance-1")
			machine.SetSiteId("site-1")
			machine.SetAssociatedDpuMachineIds([]string{})
			iface := nicosdk.NewMachineInterface()
			iface.SetMachineId("machine-1")
			iface.SetIsPrimary(true)
			iface.SetIpAddresses([]string{"10.0.0.15"})
			machine.SetMachineInterfaces([]nicosdk.MachineInterface{*iface})
			if tc.change != nil {
				tc.change(instance, machine)
			}
			address, err := autoNetworkHostIPv4(instance, machine)
			if tc.wantError {
				require.Error(t, err)
				require.Empty(t, address)
			} else {
				require.NoError(t, err)
				require.Equal(t, "10.0.0.15", address)
			}
		})
	}
}

func TestAutoNetworkReconcilePending(t *testing.T) {
	api := fake.New()
	api.SeedToken("synthetic-token")
	instance := nicosdk.NewInstance()
	instance.SetId("instance-1")
	instance.SetMachineId("machine-1")
	instance.SetSiteId("site-1")
	instance.SetVpcId("vpc-1")
	instance.SetAutoNetwork(true)
	instance.SetInterfaces([]nicosdk.Interface{})
	instance.SetStatus(nicosdk.INSTANCESTATUS_PENDING)
	api.SeedInstance("org-1", *instance)
	// No Machine is seeded: a pending instance must not attempt host-IP lookup.
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, infrav1.AddToScheme(scheme))
	const namespace = "autonetwork-pending"
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: namespace},
		Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
			APIGroup: infrav1.GroupVersion.Group, Kind: "NicoCluster", Name: "nico-1",
		}},
	}
	nicoCluster := &infrav1.NicoCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "nico-1", Namespace: namespace},
		Spec:       infrav1.NicoClusterSpec{SiteID: "site-1", IdentityRef: corev1.LocalObjectReference{Name: "credentials"}},
	}
	owner := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "owner-1", Namespace: namespace, UID: "owner-uid",
		Labels: map[string]string{clusterv1.ClusterNameLabel: "cluster-1"},
	}}
	machine := &infrav1.NicoMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-1", Namespace: namespace, Finalizers: []string{nicoMachineFinalizer},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: owner.Name, UID: owner.UID}},
		},
		Spec: infrav1.NicoMachineSpec{VPCID: "vpc-1", MachineID: "machine-1", AutoNetwork: true, ProviderID: nico.ProviderID("instance-1")},
		Status: infrav1.NicoMachineStatus{
			InstanceID: "instance-1",
			Addresses:  []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.99"}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: namespace},
		Data:       map[string][]byte{"endpoint": []byte(server.URL), "orgID": []byte("org-1"), "token": []byte("synthetic-token")},
	}
	kubeClient := k8sfake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&infrav1.NicoMachine{}).
		WithObjects(cluster, nicoCluster, owner, machine, secret).Build()
	reconciler := &NicoMachineReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme}
	key := client.ObjectKeyFromObject(machine)
	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Equal(t, machineRequeueSlow, result.RequeueAfter)
	require.NoError(t, kubeClient.Get(t.Context(), key, machine))
	require.False(t, machine.Status.Ready)
	require.Empty(t, machine.Status.Addresses, "a stale address must not survive the pending automatic-network state")
	condition := conditions.Get(machine, infrav1.MachineProvisionedCondition)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, infrav1.InstanceNotReadyReason, condition.Reason)
}
