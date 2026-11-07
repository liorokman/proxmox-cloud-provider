// Copyright 2026 Lior Okman
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package proxmox

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func nodeWithID(name, providerID string) *v1.Node {
	n := &v1.Node{}
	n.Name = name
	n.Spec.ProviderID = providerID
	return n
}

func fakeCluster(t *testing.T) (*Instances, *fakePVE) {
	t.Helper()
	pve := newFakePVE(t)
	pve.set("/cluster/status", []any{map[string]any{"type": "cluster", "name": "homelab", "nodes": 1, "quorate": 1}})
	pve.set("/cluster/resources", []any{})
	pve.addVM("pve1", 1101, "k8s-cp1", "running",
		map[string]any{"net0": "virtio=BC:24:11:AA:BB:CC,bridge=cluster1", "ipconfig0": "ip=192.168.20.3/24,gw=192.168.20.1"},
		[]map[string]any{{"name": "ens18", "hardware-address": "bc:24:11:aa:bb:cc", "ip-addresses": []map[string]any{
			{"ip-address-type": "ipv4", "ip-address": "192.168.20.33", "prefix": 24},
		}}})
	pve.addVM("pve1", 1102, "k8s-w1", "stopped",
		map[string]any{"net0": "virtio=BC:24:11:AA:BB:DD,bridge=cluster1", "ipconfig0": "ip=192.168.20.4/24,gw=192.168.20.1"}, nil)
	return &Instances{client: pve.client()}, pve
}

func TestInstanceExists(t *testing.T) {
	i, pve := fakeCluster(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		node *v1.Node
		want bool
	}{
		{"by provider id", nodeWithID("whatever", "proxmox:///pve1/1101"), true},
		{"by name", nodeWithID("k8s-w1", ""), true},
		{"unknown id", nodeWithID("k8s-cp1", "proxmox:///pve1/9999"), false},
		{"unknown name", nodeWithID("nobody", ""), false},
	} {
		got, err := i.InstanceExists(ctx, tc.node)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}

	// An API failure is an error and never "the VM is gone", which would delete the Node
	pve.set("/cluster/resources", 500)
	if got, err := i.InstanceExists(ctx, nodeWithID("k8s-cp1", "")); err == nil || got {
		t.Errorf("API failure: got %v, %v", got, err)
	}
}

func TestInstanceExistsWithoutAClient(t *testing.T) {
	if got, err := (&Instances{}).InstanceExists(context.Background(), nodeWithID("n", "")); err == nil || got {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestInstanceShutdown(t *testing.T) {
	i, _ := fakeCluster(t)
	ctx := context.Background()
	if down, err := i.InstanceShutdown(ctx, nodeWithID("k8s-cp1", "")); err != nil || down {
		t.Errorf("running VM: %v, %v", down, err)
	}
	if down, err := i.InstanceShutdown(ctx, nodeWithID("k8s-w1", "")); err != nil || !down {
		t.Errorf("stopped VM: %v, %v", down, err)
	}
	if _, err := i.InstanceShutdown(ctx, nodeWithID("nobody", "")); !errors.Is(err, errVMNotFound) {
		t.Errorf("unknown VM: %v", err)
	}
}

func TestInstanceMetadata(t *testing.T) {
	i, _ := fakeCluster(t)
	ctx := context.Background()

	// A running VM: the guest agent's addresses win over the static settings
	md, err := i.InstanceMetadata(ctx, nodeWithID("k8s-cp1", "proxmox:///pve1/1101"))
	if err != nil {
		t.Fatal(err)
	}
	if md.ProviderID != "proxmox://homelab/1101" || md.Zone != "pve1" || md.Region != "homelab" || md.InstanceType != "4c-8g" {
		t.Errorf("unexpected metadata %+v", md)
	}
	var internal []string
	hostname := ""
	for _, a := range md.NodeAddresses {
		switch a.Type {
		case v1.NodeInternalIP:
			internal = append(internal, a.Address)
		case v1.NodeHostName:
			hostname = a.Address
		}
	}
	if len(internal) != 1 || internal[0] != "192.168.20.33" || hostname != "k8s-cp1" {
		t.Errorf("addresses %+v", md.NodeAddresses)
	}

	// A stopped VM has no agent: the cloud-init settings are used
	md, err = i.InstanceMetadata(ctx, nodeWithID("k8s-w1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(md.NodeAddresses) != 2 || md.NodeAddresses[0].Address != "192.168.20.4" {
		t.Errorf("addresses %+v", md.NodeAddresses)
	}

	if _, err := i.InstanceMetadata(ctx, nodeWithID("nobody", "")); err == nil {
		t.Error("an unknown VM must be an error")
	}
}

// A running VM whose guest agent doesn't answer falls back to the static settings.
func TestInstanceMetadataWithoutAgent(t *testing.T) {
	i, pve := fakeCluster(t)
	pve.set("/nodes/pve1/qemu/1101/agent/network-get-interfaces", 500)
	md, err := i.InstanceMetadata(context.Background(), nodeWithID("k8s-cp1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(md.NodeAddresses) < 1 || md.NodeAddresses[0].Address != "192.168.20.3" {
		t.Errorf("addresses %+v", md.NodeAddresses)
	}
}

// A node that is not in a cluster gets the default region.
func TestInstanceMetadataDefaultRegion(t *testing.T) {
	i, pve := fakeCluster(t)
	pve.set("/cluster/status", []any{map[string]any{"type": "node", "name": "pve1", "online": 1}})
	md, err := i.InstanceMetadata(context.Background(), nodeWithID("k8s-w1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if md.Region != defaultRegion {
		t.Errorf("region %q", md.Region)
	}
}
