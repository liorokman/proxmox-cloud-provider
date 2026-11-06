package ipam

import (
	"net"
	"testing"

	"github.com/knadh/koanf/v2"
)

func newTest(t *testing.T, busy ...string) IPAM {
	t.Helper()
	k := koanf.New(".")
	for key, v := range map[string]any{
		"loadbalancer.ipam.cidr":                 "192.168.64.0/22",
		"loadbalancer.ipam.dynamicRange.startAt": "192.168.67.101",
		"loadbalancer.ipam.dynamicRange.endAt":   "192.168.67.103",
		"loadbalancer.ipam.staticRange.startAt":  "192.168.67.201",
		"loadbalancer.ipam.staticRange.endAt":    "192.168.67.210",
	} {
		k.Set(key, v)
	}
	busySet := map[string]bool{}
	for _, b := range busy {
		busySet[b] = true
	}
	i, err := New(k, func(ip net.IP) bool { return busySet[ip.String()] })
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func ip(s string) net.IP { return net.ParseIP(s) }

func TestAllocateSkipsTakenAndBusy(t *testing.T) {
	i := newTest(t, "192.168.67.102")
	var got []string
	for n := 0; n < 2; n++ {
		a, err := i.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, a.String())
	}
	if got[0] != "192.168.67.101" || got[1] != "192.168.67.103" {
		t.Errorf("unexpected allocations %v", got)
	}
	if _, err := i.Allocate(); err == nil {
		t.Error("expected exhaustion")
	}
}

// Allocations must be recorded immediately: two allocations in a row never
// return the same address, even before anything is configured on the interface.
func TestAllocateIsRecorded(t *testing.T) {
	i := newTest(t)
	a, _ := i.Allocate()
	b, _ := i.Allocate()
	if a.Equal(b) {
		t.Errorf("same address twice: %s", a)
	}
}

func TestReserveRanges(t *testing.T) {
	i := newTest(t, "192.168.67.202")
	for _, ok := range []string{"192.168.67.101", "192.168.67.201", "192.168.67.210"} {
		if err := i.Reserve(ip(ok)); err != nil {
			t.Errorf("%s should be allowed: %v", ok, err)
		}
	}
	// Outside the ranges (the router, another host), or busy on the interface
	for _, bad := range []string{"192.168.64.1", "192.168.67.104", "192.168.67.211", "192.168.67.202"} {
		if err := i.Reserve(ip(bad)); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
}

func TestSharedAddressIsReferenceCounted(t *testing.T) {
	// The address is on the interface once the first holder configured it
	i := newTest(t, "192.168.67.201")
	i.Adopt(ip("192.168.67.201"))
	if err := i.Reserve(ip("192.168.67.201")); err != nil {
		t.Fatalf("sharing a held address should work: %v", err)
	}
	if n := i.Release(ip("192.168.67.201")); n != 1 {
		t.Errorf("expected 1 holder left, got %d", n)
	}
	if !i.IsReserved(ip("192.168.67.201")) {
		t.Error("still held")
	}
	if n := i.Release(ip("192.168.67.201")); n != 0 {
		t.Errorf("expected 0 holders left, got %d", n)
	}
	if i.IsReserved(ip("192.168.67.201")) {
		t.Error("should be free")
	}
	if n := i.Release(ip("192.168.67.201")); n != 0 {
		t.Errorf("releasing a free address must be harmless, got %d", n)
	}
}

func TestInvalidConfig(t *testing.T) {
	k := koanf.New(".")
	k.Set("loadbalancer.ipam.cidr", "192.168.64.0/22")
	k.Set("loadbalancer.ipam.dynamicRange.startAt", "10.0.0.1")
	k.Set("loadbalancer.ipam.dynamicRange.endAt", "10.0.0.5")
	if _, err := New(k, nil); err == nil {
		t.Error("range outside the cidr must be refused")
	}
}
