package loadbalancer

import (
	"context"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestLoadBalancerLifecycle needs root, the ip_vs kernel modules and a network
// namespace with the interface named in lbmanager.yaml:
//
//	modprobe ip_vs ip_vs_rr
//	ip netns add LB && ip -n LB link add dummy0 type dummy && ip -n LB link set dummy0 up
func TestLoadBalancerLifecycle(t *testing.T) {
	k := koanf.New(".")
	if err := k.Load(file.Provider("../../lbmanager.yaml"), yaml.Parser()); err != nil {
		t.Fatal(err)
	}
	ns, err := netns.GetFromName(k.String("loadbalancer.namespace"))
	if err != nil {
		t.Skipf("network namespace %q is not available: %v", k.String("loadbalancer.namespace"), err)
	}
	ns.Close()
	if err := k.Set("dbDir", t.TempDir()); err != nil {
		t.Fatal(err)
	}

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
	k := koanf.New(".")
	if err := k.Load(file.Provider("../../lbmanager.yaml"), yaml.Parser()); err != nil {
		t.Fatal(err)
	}
	if _, err := netns.GetFromName(k.String("loadbalancer.namespace")); err != nil {
		t.Skipf("network namespace not available: %v", err)
	}
	if err := k.Set("dbDir", t.TempDir()); err != nil {
		t.Fatal(err)
	}
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
