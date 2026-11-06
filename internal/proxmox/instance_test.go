package proxmox

import (
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
