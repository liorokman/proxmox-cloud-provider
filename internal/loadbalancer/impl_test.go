// Copyright 2023-2026 Lior Okman
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

package loadbalancer

import (
	"context"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/liorokman/proxmox-cloud-provider/internal/ipvsns"
	"github.com/vishvananda/netlink"
)

// TestLoadBalancerLifecycle needs root and the ip_vs kernel modules
// (modprobe ip_vs ip_vs_rr). It creates its own network namespace.
func TestLoadBalancerLifecycle(t *testing.T) {
	k := testConfig(t)

	lbServer, err := NewServer(k)
	if err != nil {
		t.Skipf("cannot create the load balancer server (needs root and ip_vs): %v", err)
	}
	defer lbServer.Close()
	ctx := context.Background()

	// Addresses outside the dynamic and static ranges are refused
	for _, bad := range []string{"10.0.0.1", "192.168.87.1", "192.168.87.199"} {
		if _, err := lbServer.Create(ctx, &CreateLoadBalancer{Name: "bad", IpAddr: asPtr(bad)}); err == nil {
			t.Errorf("expected an error for %s", bad)
		}
	}

	lbi, err := lbServer.Create(ctx, &CreateLoadBalancer{Name: "test1", IpAddr: asPtr("192.168.87.210")})
	if err != nil {
		t.Fatal(err)
	}
	if lbi.IpAddr != "192.168.87.210" {
		t.Errorf("unexpected address %s", lbi.IpAddr)
	}
	defer lbServer.Delete(ctx, &LoadBalancerName{Name: "test1"})

	target := &Target{Protocol: Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30080}
	if res, err := lbServer.AddTarget(ctx, &AddTargetRequest{LbName: "test1", SrcPort: 80, Target: target}); err != nil || res.Code != ErrSuccess {
		t.Fatalf("AddTarget: %v %+v", err, res)
	}
	if res, _ := lbServer.AddTarget(ctx, &AddTargetRequest{LbName: "test1", SrcPort: 80, Target: target}); res.Code != ErrTargetAlreadyMapped {
		t.Errorf("expected ErrTargetAlreadyMapped, got %+v", res)
	}
	got, err := lbServer.GetLoadBalancer(ctx, &LoadBalancerName{Name: "test1"})
	if err != nil || len(got.Targets[80].GetTarget()) != 1 {
		t.Fatalf("GetLoadBalancer: %v %+v", err, got)
	}
	if res, err := lbServer.DelTarget(ctx, &DelTargetRequest{LbName: "test1", SrcPort: 80, Target: target}); err != nil || res.Code != ErrSuccess {
		t.Fatalf("DelTarget: %v %+v", err, res)
	}
	if res, err := lbServer.Delete(ctx, &LoadBalancerName{Name: "test1"}); err != nil || res.Code != ErrSuccess {
		t.Fatalf("Delete: %v %+v", err, res)
	}
}

func newTestServer(t *testing.T) Intf {
	t.Helper()
	return newTestServerWith(testConfig(t), t)
}

func newTestServerWith(k *koanf.Koanf, t *testing.T) Intf {
	t.Helper()
	s, err := NewServer(k)
	if err != nil {
		t.Skipf("cannot create the server (needs root and ip_vs): %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestAllocationsAreRecorded(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for _, name := range []string{"a", "b", "c"} {
		lb, err := s.Create(ctx, &CreateLoadBalancer{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if seen[lb.IpAddr] {
			t.Fatalf("address %s handed out twice", lb.IpAddr)
		}
		seen[lb.IpAddr] = true
	}
	for name := range map[string]bool{"a": true, "b": true, "c": true} {
		s.Delete(ctx, &LoadBalancerName{Name: name})
	}
	// Freed addresses are available again
	lb, err := s.Create(ctx, &CreateLoadBalancer{Name: "d"})
	if err != nil || !seen[lb.IpAddr] {
		t.Errorf("expected a reused address, got %v %v", lb, err)
	}
	s.Delete(ctx, &LoadBalancerName{Name: "d"})
}

func TestSharedAddress(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	ip := asPtr("192.168.87.231")
	for _, name := range []string{"x", "y"} {
		if _, err := s.Create(ctx, &CreateLoadBalancer{Name: name, IpAddr: ip}); err != nil {
			t.Fatal(err)
		}
	}
	tgt := &Target{Protocol: Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30080}
	if res, err := s.AddTarget(ctx, &AddTargetRequest{LbName: "x", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrSuccess {
		t.Fatalf("%v %+v", err, res)
	}
	// Same address, port and protocol: refused
	if res, _ := s.AddTarget(ctx, &AddTargetRequest{LbName: "y", SrcPort: 80, Target: tgt}); res.Code != ErrPortConflict {
		t.Errorf("expected ErrPortConflict, got %+v", res)
	}
	// Another port is fine
	if res, err := s.AddTarget(ctx, &AddTargetRequest{LbName: "y", SrcPort: 81, Target: tgt}); err != nil || res.Code != ErrSuccess {
		t.Errorf("%v %+v", err, res)
	}
	// Deleting one holder keeps the address configured for the other
	s.Delete(ctx, &LoadBalancerName{Name: "x"})
	l := s.(*loadBalancerServer)
	addrs, _ := l.nlHandle.AddrList(l.externalLink, 0)
	found := false
	for _, a := range addrs {
		found = found || a.IP.String() == "192.168.87.231"
	}
	if !found {
		t.Error("the shared address was removed while still in use")
	}
	s.Delete(ctx, &LoadBalancerName{Name: "y"})
	addrs, _ = l.nlHandle.AddrList(l.externalLink, 0)
	for _, a := range addrs {
		if a.IP.String() == "192.168.87.231" {
			t.Error("the address is still configured after the last holder was deleted")
		}
	}
}

func TestForeignAddressRefused(t *testing.T) {
	s := newTestServer(t)
	l := s.(*loadBalancerServer)
	own, _ := netlinkParse("192.168.87.232/24")
	if err := l.nlHandle.AddrAdd(l.externalLink, own); err != nil {
		t.Fatal(err)
	}
	defer l.nlHandle.AddrDel(l.externalLink, own)
	if _, err := s.Create(context.Background(), &CreateLoadBalancer{Name: "z", IpAddr: asPtr("192.168.87.232")}); err == nil {
		t.Error("an address configured by someone else must be refused")
	}
}

func TestRestoreSurvivesBadRecord(t *testing.T) {
	s := newTestServer(t)
	l := s.(*loadBalancerServer)
	ctx := context.Background()
	if _, err := s.Create(ctx, &CreateLoadBalancer{Name: "good", IpAddr: asPtr("192.168.87.233")}); err != nil {
		t.Fatal(err)
	}
	l.db.Put([]byte("bad"), []byte("{not json"))
	if err := s.Restore(); err != nil {
		t.Errorf("Restore must carry on: %v", err)
	}
	s.Delete(ctx, &LoadBalancerName{Name: "good"})
}

func netlinkParse(s string) (*netlink.Addr, error) { return netlink.ParseAddr(s) }

func tl(ip string, port int32) *Target {
	return &Target{Protocol: Protocol_TCP, DstIP: ip, DstPort: port}
}

func TestApplyConverges(t *testing.T) {
	s := newTestServer(t)
	l := s.(*loadBalancerServer)
	ctx := context.Background()
	req := &ApplyRequest{Name: "app", Targets: map[int32]*TargetList{
		80:   {Target: []*Target{tl("192.168.100.5", 30080), tl("192.168.100.6", 30080)}},
		8080: {Target: []*Target{tl("192.168.100.5", 30081)}},
	}}
	res, err := s.Apply(ctx, req)
	if err != nil || len(res.PortErrors) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	defer s.Delete(ctx, &LoadBalancerName{Name: "app"})
	ip := res.Info.IpAddr
	if ip == "" {
		t.Fatal("no address allocated")
	}
	// Repeating changes nothing, and keeps the address
	res, err = s.Apply(ctx, req)
	if err != nil || res.Info.IpAddr != ip || len(res.PortErrors) != 0 {
		t.Fatalf("second apply: %v %+v", err, res)
	}
	if w := destWeights(t, l, "app", 80); len(w) != 2 {
		t.Errorf("unexpected destinations %v", w)
	}
	// Remove a node, drop a port, add another
	req.Targets[80] = &TargetList{Target: []*Target{tl("192.168.100.6", 30080)}}
	delete(req.Targets, 8080)
	req.Targets[443] = &TargetList{Target: []*Target{tl("192.168.100.6", 30443)}}
	res, err = s.Apply(ctx, req)
	if err != nil || len(res.PortErrors) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if w := destWeights(t, l, "app", 80); len(w) != 1 || w["192.168.100.6:30080"] != 1 {
		t.Errorf("port 80: %v", w)
	}
	if len(res.Info.Targets) != 2 || res.Info.Targets[8080] != nil {
		t.Errorf("stored state: %+v", res.Info.Targets)
	}
	if svcs, _ := ipvsns.Services(l.ipvsHandle); len(svcs) != 2 {
		t.Errorf("expected 2 ipvs services, got %d", len(svcs))
	}
	// The address can't be changed
	if _, err := s.Apply(ctx, &ApplyRequest{Name: "app", IpAddr: asPtr("192.168.87.236")}); err == nil {
		t.Error("changing the address must fail")
	}
	// Applying nothing empties it
	res, err = s.Apply(ctx, &ApplyRequest{Name: "app"})
	if err != nil || len(res.Info.Targets) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if svcs, _ := ipvsns.Services(l.ipvsHandle); len(svcs) != 0 {
		t.Errorf("expected no ipvs services, got %d", len(svcs))
	}
}

func TestApplyPortConflictIsPerPort(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	ip := asPtr("192.168.87.237")
	if _, err := s.Apply(ctx, &ApplyRequest{Name: "one", IpAddr: ip, Targets: map[int32]*TargetList{80: {Target: []*Target{tl("192.168.100.5", 1)}}}}); err != nil {
		t.Fatal(err)
	}
	defer s.Delete(ctx, &LoadBalancerName{Name: "one"})
	res, err := s.Apply(ctx, &ApplyRequest{Name: "two", IpAddr: ip, Targets: map[int32]*TargetList{
		80: {Target: []*Target{tl("192.168.100.5", 2)}},
		81: {Target: []*Target{tl("192.168.100.5", 3)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Delete(ctx, &LoadBalancerName{Name: "two"})
	if res.PortErrors[80] == "" || res.PortErrors[81] != "" {
		t.Errorf("only port 80 should fail: %v", res.PortErrors)
	}
	if res.Info.Targets[80] != nil || res.Info.Targets[81] == nil {
		t.Errorf("unexpected state %+v", res.Info.Targets)
	}
}
