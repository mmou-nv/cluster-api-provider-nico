// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
)

func TestAutoNetworkRequestIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*nicosdk.Instance, *infrav1.NicoMachineSpec, *string)
	}{
		{"missing instance ID", func(i *nicosdk.Instance, _ *infrav1.NicoMachineSpec, _ *string) { i.SetId("") }},
		{"missing machine ID", func(i *nicosdk.Instance, _ *infrav1.NicoMachineSpec, _ *string) { i.SetMachineId("") }},
		{"different machine ID", func(i *nicosdk.Instance, _ *infrav1.NicoMachineSpec, _ *string) { i.SetMachineId("other-machine") }},
		{"missing instance site", func(i *nicosdk.Instance, _ *infrav1.NicoMachineSpec, _ *string) { i.SetSiteId("") }},
		{"missing requested site", func(_ *nicosdk.Instance, _ *infrav1.NicoMachineSpec, s *string) { *s = "" }},
		{"different site", func(i *nicosdk.Instance, _ *infrav1.NicoMachineSpec, _ *string) { i.SetSiteId("other-site") }},
		{"different VPC", func(i *nicosdk.Instance, _ *infrav1.NicoMachineSpec, _ *string) { i.SetVpcId("other-vpc") }},
		{"different instance type", func(_ *nicosdk.Instance, s *infrav1.NicoMachineSpec, _ *string) {
			s.MachineID = ""
			s.InstanceTypeID = "other-type"
		}},
		{"not automatic", func(i *nicosdk.Instance, _ *infrav1.NicoMachineSpec, _ *string) { i.SetAutoNetwork(false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := autoNetworkIdentityInstance()
			spec := infrav1.NicoMachineSpec{AutoNetwork: true, MachineID: "machine-1", VPCID: "vpc-1"}
			site := "site-1"
			require.NoError(t, autoNetworkRequestIdentity(instance, spec, site))
			tc.change(instance, &spec, &site)
			require.Error(t, autoNetworkRequestIdentity(instance, spec, site))
		})
	}
	require.Error(t, autoNetworkRequestIdentity(nil, infrav1.NicoMachineSpec{}, "site-1"))
}

func autoNetworkIdentityInstance() *nicosdk.Instance {
	instance := nicosdk.NewInstance()
	instance.SetId("instance-1")
	instance.SetName("owner-1")
	instance.SetMachineId("machine-1")
	instance.SetSiteId("site-1")
	instance.SetVpcId("vpc-1")
	instance.SetTenantId("tenant-1")
	instance.SetAutoNetwork(true)
	instance.SetInterfaces([]nicosdk.Interface{})
	instance.SetStatus(nicosdk.INSTANCESTATUS_READY)
	return instance
}

func TestAutoNetworkReconcileRequestBinding(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		adopt, conflict, ready, requestMismatch bool
		change                                  func(*nicosdk.Instance, *infrav1.NicoMachine, *infrav1.NicoCluster)
	}{
		{name: "bound ready host", ready: true},
		{name: "matching adoption", adopt: true, ready: true},
		{name: "bound machine mismatch", requestMismatch: true, change: func(_ *nicosdk.Instance, m *infrav1.NicoMachine, _ *infrav1.NicoCluster) {
			m.Spec.MachineID = "requested-other-machine"
		}},
		{name: "bound site mismatch", requestMismatch: true, change: func(_ *nicosdk.Instance, _ *infrav1.NicoMachine, c *infrav1.NicoCluster) {
			c.Spec.SiteID = "requested-other-site"
		}},
		{name: "adoption machine mismatch", adopt: true, requestMismatch: true, change: func(_ *nicosdk.Instance, m *infrav1.NicoMachine, _ *infrav1.NicoCluster) {
			m.Spec.MachineID = "requested-other-machine"
		}},
		{name: "adoption labels mismatch", adopt: true, requestMismatch: true, change: func(_ *nicosdk.Instance, m *infrav1.NicoMachine, _ *infrav1.NicoCluster) {
			m.Spec.Labels = map[string]string{"required": "label"}
		}},
		{name: "adoption boot script mismatch", adopt: true, requestMismatch: true, change: func(_ *nicosdk.Instance, m *infrav1.NicoMachine, _ *infrav1.NicoCluster) {
			m.Spec.IpxeScript = "requested-script"
		}},
		{name: "adoption mode mismatch", adopt: true, requestMismatch: true, change: func(i *nicosdk.Instance, _ *infrav1.NicoMachine, _ *infrav1.NicoCluster) { i.SetAutoNetwork(false) }},
		{name: "conflicting create refuses other machine", adopt: true, conflict: true, requestMismatch: true, change: func(_ *nicosdk.Instance, m *infrav1.NicoMachine, _ *infrav1.NicoCluster) {
			m.Spec.MachineID = "requested-other-machine"
		}},
		{name: "pending ignores legacy address", change: func(i *nicosdk.Instance, _ *infrav1.NicoMachine, _ *infrav1.NicoCluster) {
			i.SetStatus(nicosdk.INSTANCESTATUS_PENDING)
			iface := nicosdk.NewInterface()
			iface.SetIpAddresses([]string{"10.0.0.88"})
			i.SetInterfaces([]nicosdk.Interface{*iface})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := fake.New()
			api.SeedToken("synthetic-token")
			instance := autoNetworkIdentityInstance()
			namespace := "autonetwork-" + strings.ReplaceAll(tc.name, " ", "-")
			cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: namespace},
				Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "NicoCluster", Name: "nico-1"}}}
			nicoCluster := &infrav1.NicoCluster{ObjectMeta: metav1.ObjectMeta{Name: "nico-1", Namespace: namespace},
				Spec: infrav1.NicoClusterSpec{SiteID: "site-1", IdentityRef: corev1.LocalObjectReference{Name: "credentials"}}}
			owner := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "owner-1", Namespace: namespace, UID: "owner-uid",
				Labels: map[string]string{clusterv1.ClusterNameLabel: "cluster-1"}}}
			machine := &infrav1.NicoMachine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Namespace: namespace,
				Finalizers: []string{nicoMachineFinalizer}, OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: owner.Name, UID: owner.UID}}},
				Spec:   infrav1.NicoMachineSpec{VPCID: "vpc-1", MachineID: "machine-1", AutoNetwork: true, ProviderID: nico.ProviderID("instance-1")},
				Status: infrav1.NicoMachineStatus{InstanceID: "instance-1", Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.99"}}}}
			if tc.adopt {
				machine.Spec.ProviderID = ""
				machine.Status.InstanceID = ""
			}
			if tc.change != nil {
				tc.change(instance, machine, nicoCluster)
			}
			if !tc.conflict {
				api.SeedInstance("org-1", *instance)
			}
			host := nicosdk.NewMachine()
			host.SetId(instance.GetMachineId())
			host.SetInstanceId(instance.GetId())
			host.SetSiteId(instance.GetSiteId())
			host.SetAssociatedDpuMachineIds([]string{})
			iface := nicosdk.NewMachineInterface()
			iface.SetMachineId(host.GetId())
			iface.SetIsPrimary(true)
			iface.SetIpAddresses([]string{"10.0.0.15"})
			host.SetMachineInterfaces([]nicosdk.MachineInterface{*iface})
			api.SeedMachine("org-1", *host)
			tenant := nicosdk.NewTenant()
			tenant.SetId("tenant-1")
			api.SeedTenant("org-1", *tenant)
			postCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.conflict && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/instance") {
					postCalls++
					api.SeedInstance("org-1", *instance)
					http.Error(w, `{"message":"instance already exists"}`, http.StatusConflict)
					return
				}
				api.Handler().ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			bootstrapName := "bootstrap"
			owner.Spec.Bootstrap.DataSecretName = &bootstrapName
			bootstrap := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bootstrapName, Namespace: namespace}, Data: map[string][]byte{"value": []byte("#cloud-config\n")}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: namespace},
				Data: map[string][]byte{"endpoint": []byte(server.URL), "orgID": []byte("org-1"), "token": []byte("synthetic-token")}}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, clusterv1.AddToScheme(scheme))
			require.NoError(t, infrav1.AddToScheme(scheme))
			kubeClient := k8sfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&infrav1.NicoMachine{}).
				WithObjects(cluster, nicoCluster, owner, machine, secret, bootstrap).Build()
			reconciler := &NicoMachineReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme}
			key := client.ObjectKeyFromObject(machine)
			_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			if tc.conflict {
				require.Error(t, err)
				require.Equal(t, 1, postCalls)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, kubeClient.Get(t.Context(), key, machine))
			require.Equal(t, tc.ready, machine.Status.Ready)
			if tc.ready {
				require.Equal(t, []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.15"}}, machine.Status.Addresses)
			} else {
				require.Empty(t, machine.Status.Addresses)
				condition := conditions.Get(machine, infrav1.MachineProvisionedCondition)
				require.NotNil(t, condition)
				require.Equal(t, metav1.ConditionFalse, condition.Status)
			}
			if tc.adopt && tc.requestMismatch {
				require.Empty(t, machine.Spec.ProviderID, "rejected adoption must not claim the instance")
				require.Empty(t, machine.Status.InstanceID)
			}
		})
	}
}
