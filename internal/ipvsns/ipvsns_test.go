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

package ipvsns

import (
	"net/netip"
	"testing"

	"github.com/cloudflare/ipvs"
	"github.com/liorokman/proxmox-cloud-provider/internal/nstest"
	"github.com/vishvananda/netns"
)

func TestFullMask(t *testing.T) {
	if m := FullMask(netip.MustParseAddr("10.1.2.3")); !m.Is4() || m.Bits() != 32 {
		t.Errorf("IPv4: got %v", m)
	}
	if m := FullMask(netip.MustParseAddr("2001:db8::1")); !m.Is6() || m.Bits() != 128 {
		t.Errorf("IPv6: got %v", m)
	}
}

func TestOpenRejectsInvalidNamespace(t *testing.T) {
	if _, err := Open(netns.NsHandle(-1)); err == nil {
		t.Fatal("expected an error for an invalid namespace handle")
	}
}

// namespace returns a private namespace with routes to the addresses the tests use as real servers.
func namespace(t *testing.T) string {
	name := nstest.New(t)
	nstest.AddAddr(t, name, "10.0.0.1/8", "fd00::1/64")
	return name
}

func openIn(t *testing.T, name string) Client {
	t.Helper()
	ns, err := netns.GetFromName(name)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	c, err := Open(ns)
	if err != nil {
		t.Skipf("cannot open IPVS (needs root and the ip_vs module): %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func service(addr string, port uint16) ipvs.Service {
	a := netip.MustParseAddr(addr)
	fam := ipvs.INET
	if a.Is6() {
		fam = ipvs.INET6
	}
	return ipvs.Service{Address: a, Netmask: FullMask(a), Port: port, Protocol: ipvs.TCP, Family: fam, Scheduler: "rr"}
}

func dest(addr string, port uint16, weight uint32) ipvs.Destination {
	a := netip.MustParseAddr(addr)
	fam := ipvs.INET
	if a.Is6() {
		fam = ipvs.INET6
	}
	return ipvs.Destination{Address: a, Port: port, Family: fam, FwdMethod: ipvs.Masquerade, Weight: weight}
}

// Services and destinations are created in the namespace that was opened, in both address families.
func TestServicesAndDestinations(t *testing.T) {
	c := openIn(t, namespace(t))

	for _, tc := range []struct{ vip, real string }{
		{"192.0.2.10", "10.0.0.5"},
		{"2001:db8::10", "fd00::5"},
	} {
		t.Run(tc.vip, func(t *testing.T) {
			s := service(tc.vip, 80)
			if ServicePresent(c, s) {
				t.Fatal("the service exists before it was created")
			}
			if err := c.CreateService(s); err != nil {
				t.Fatal(err)
			}
			if !ServicePresent(c, s) {
				t.Fatal("the service is missing after it was created")
			}
			if err := c.CreateService(s); err == nil {
				t.Error("creating a service twice should fail")
			}

			d := dest(tc.real, 8080, 1)
			if err := c.CreateDestination(s, d); err != nil {
				t.Fatal(err)
			}
			if err := c.CreateDestination(s, d); err == nil {
				t.Error("creating a destination twice should fail")
			}
			d.Weight = 0
			if err := c.UpdateDestination(s, d); err != nil {
				t.Fatal(err)
			}
			dests, err := Destinations(c, s)
			if err != nil || len(dests) != 1 {
				t.Fatalf("Destinations: %v %v", dests, err)
			}
			if got := dests[0]; got.Address != d.Address || got.Port != 8080 || got.Weight != 0 || got.FwdMethod != ipvs.Masquerade {
				t.Errorf("unexpected destination %+v", got)
			}

			if err := c.RemoveDestination(s, d); err != nil {
				t.Fatal(err)
			}
			if dests, err := Destinations(c, s); err != nil || len(dests) != 0 {
				t.Errorf("the destination is still there: %v", dests)
			}
			if err := c.RemoveService(s); err != nil {
				t.Fatal(err)
			}
			if ServicePresent(c, s) {
				t.Error("the service is still there after it was removed")
			}
			if err := c.RemoveService(s); err == nil {
				t.Error("removing a missing service should fail")
			}
		})
	}
}

// A client only sees the namespace it was opened in, and opening one does not
// leave the calling thread in that namespace.
func TestNamespacesAreIsolated(t *testing.T) {
	before, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	a := openIn(t, namespace(t))
	b := openIn(t, namespace(t))

	s := service("192.0.2.20", 443)
	if err := a.CreateService(s); err != nil {
		t.Fatal(err)
	}
	if !ServicePresent(a, s) {
		t.Error("namespace a lost its service")
	}
	if ServicePresent(b, s) {
		t.Error("namespace b sees the service of namespace a")
	}
	svcs, err := Services(b)
	if err != nil || len(svcs) != 0 {
		t.Errorf("namespace b: %v %v", svcs, err)
	}

	// The caller's own namespace did not change, and neither did the one of the test's thread
	for i := 0; i < 20; i++ {
		after, err := netns.Get()
		if err != nil {
			t.Fatal(err)
		}
		same := after.Equal(before)
		after.Close()
		if !same {
			t.Fatal("the calling thread was left in another network namespace")
		}
	}
}

// Counters are reported with 64 bits.
func TestStatsAreAvailable(t *testing.T) {
	c := openIn(t, namespace(t))
	s := service("192.0.2.30", 80)
	if err := c.CreateService(s); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateDestination(s, dest("10.0.0.9", 80, 1)); err != nil {
		t.Fatal(err)
	}
	ext, err := c.Service(s)
	if err != nil {
		t.Fatal(err)
	}
	if ext.Stats64.Connections != 0 {
		t.Errorf("a new service should have no connections: %+v", ext.Stats64)
	}
	dests, err := Destinations(c, s)
	if err != nil || len(dests) != 1 || dests[0].ActiveConnections != 0 {
		t.Errorf("Destinations: %v %v", dests, err)
	}
}
