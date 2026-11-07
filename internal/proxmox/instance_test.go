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
	"reflect"
	"testing"

	"github.com/luthermonson/go-proxmox"
	v1 "k8s.io/api/core/v1"
)

func TestParseProviderID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		id   int
		good bool
	}{
		{"proxmox:///pve1/1101", 1101, true},
		{"", 0, false},
		{"proxmox:///pve1", 0, false},
		{"proxmox:///pve1/abc", 0, false},
		{"proxmox:///a/b/1101", 0, false},
	} {
		id, ok := parseProviderID(tc.in)
		if id != tc.id || ok != tc.good {
			t.Errorf("parseProviderID(%q) = %d, %v; want %d, %v", tc.in, id, ok, tc.id, tc.good)
		}
	}
}

func TestGetAddresses(t *testing.T) {
	cfg := &proxmox.VirtualMachineConfig{
		Name: "k8s-master1",
		Nets: map[string]string{"net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=k8s"},
		IPConfigs: map[string]string{
			"ipconfig0": "ip=192.168.50.3/24,gw=192.168.50.1",
			// has no matching net1 entry, must be ignored
			"ipconfig1": "ip=10.0.0.5/24",
		},
	}
	got := getAddresses(cfg)
	want := map[v1.NodeAddress]bool{
		{Type: v1.NodeInternalIP, Address: "192.168.50.3"}: true,
		{Type: v1.NodeHostName, Address: "k8s-master1"}:    true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, a := range got {
		if !want[a] {
			t.Errorf("unexpected address %+v", a)
		}
	}
	if len(getAddresses(nil)) != 0 {
		t.Error("nil config should return no addresses")
	}
}

func TestGetProviderID(t *testing.T) {
	vm := &proxmox.VirtualMachine{Node: "pve1", VMID: 1101}
	if got := getProviderID(vm); got != "proxmox:///pve1/1101" {
		t.Errorf("got %s", got)
	}
}

func TestAgentAddresses(t *testing.T) {
	cfg := &proxmox.VirtualMachineConfig{
		Name: "node1",
		Nets: map[string]string{
			"net0": "virtio=BC:24:11:AA:BB:CC,bridge=cluster1",
			"net1": "virtio=bc:24:11:00:00:01,bridge=vmbr0,firewall=1",
		},
	}
	ifaces := []*proxmox.AgentNetworkIface{
		{Name: "ens18", HardwareAddress: "bc:24:11:aa:bb:cc", IPAddresses: []*proxmox.AgentNetworkIPAddress{
			{IPAddressType: "ipv4", IPAddress: "192.168.20.3"},
			{IPAddressType: "ipv6", IPAddress: "fe80::1"},
			{IPAddressType: "ipv4", IPAddress: "169.254.1.1"},
		}},
		{Name: "flannel.1", HardwareAddress: "aa:00:00:00:00:01", IPAddresses: []*proxmox.AgentNetworkIPAddress{{IPAddressType: "ipv4", IPAddress: "10.244.0.0"}}},
		{Name: "ens19", HardwareAddress: "BC:24:11:00:00:01", IPAddresses: []*proxmox.AgentNetworkIPAddress{{IPAddressType: "ipv4", IPAddress: "192.168.67.9"}}},
	}
	got := nodeAddresses(cfg, ifaces)
	want := []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "192.168.20.3"},
		{Type: v1.NodeInternalIP, Address: "192.168.67.9"},
		{Type: v1.NodeHostName, Address: "node1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// Without agent data the static settings are used
	cfg.IPConfigs = map[string]string{"ipconfig0": "ip=192.168.20.7/24,gw=192.168.20.1"}
	got = nodeAddresses(cfg, nil)
	if len(got) != 2 || got[0].Address != "192.168.20.7" {
		t.Errorf("fallback: %+v", got)
	}
}

func TestInstanceType(t *testing.T) {
	for _, c := range []struct {
		cpus int
		mem  uint64
		want string
	}{{4, 8 << 30, "4c-8g"}, {2, 1536 << 20, "2c-1536m"}, {0, 0, "qemu"}} {
		if got := instanceType(c.cpus, c.mem); got != c.want {
			t.Errorf("instanceType(%d,%d) = %s, want %s", c.cpus, c.mem, got, c.want)
		}
	}
}
