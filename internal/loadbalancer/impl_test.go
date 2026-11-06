package loadbalancer

import (
	"context"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
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

	// An address outside the configured CIDR is refused
	if _, err := lbServer.Create(ctx, &CreateLoadBalancer{Name: "bad", IpAddr: asPtr("10.0.0.1")}); err == nil {
		t.Error("expected an error for an IP outside the configured CIDR")
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
