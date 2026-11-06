package proxmox

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

// fakeLBClient records the target changes requested by the provider
type fakeLBClient struct {
	loadbalancer.LoadBalancerClient
	added   []targetKey
	removed []targetKey
}

func (f *fakeLBClient) AddTarget(_ context.Context, in *loadbalancer.AddTargetRequest, _ ...grpc.CallOption) (*loadbalancer.Error, error) {
	f.added = append(f.added, newTargetKey(in.Target, in.SrcPort))
	return &loadbalancer.Error{}, nil
}

func (f *fakeLBClient) DelTarget(_ context.Context, in *loadbalancer.DelTargetRequest, _ ...grpc.CallOption) (*loadbalancer.Error, error) {
	f.removed = append(f.removed, newTargetKey(in.Target, in.SrcPort))
	return &loadbalancer.Error{}, nil
}

func testNode(ip string) *v1.Node {
	return &v1.Node{Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: ip}}}}
}

func testService() *v1.Service {
	return &v1.Service{Spec: v1.ServiceSpec{Ports: []v1.ServicePort{
		{Port: 80, NodePort: 30080, Protocol: v1.ProtocolTCP},
		{Port: 443, NodePort: 30443, Protocol: v1.ProtocolTCP},
	}}}
}

func lbInfoWith(targets map[int32][]*loadbalancer.Target) *loadbalancer.LoadBalancerInformation {
	info := &loadbalancer.LoadBalancerInformation{Name: "lb", IpAddr: "192.168.87.210", Targets: map[int32]*loadbalancer.TargetList{}}
	for port, list := range targets {
		info.Targets[port] = &loadbalancer.TargetList{Target: list}
	}
	return info
}

// A resync of an already configured multi-port service must not touch anything
func TestHandleMappingsIsStable(t *testing.T) {
	fake := &fakeLBClient{}
	lb := &LoadBalancer{client: fake}
	info := lbInfoWith(map[int32][]*loadbalancer.Target{
		80:  {{Protocol: loadbalancer.Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30080}},
		443: {{Protocol: loadbalancer.Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30443}},
	})
	lb.handleMappings(context.Background(), info, testService(), []*v1.Node{testNode("192.168.100.5")})
	if len(fake.added) != 0 || len(fake.removed) != 0 {
		t.Errorf("expected no changes, got added=%v removed=%v", fake.added, fake.removed)
	}
}

// A node that left the cluster must be removed from every port, and nothing else
func TestHandleMappingsRemovesStaleNode(t *testing.T) {
	fake := &fakeLBClient{}
	lb := &LoadBalancer{client: fake}
	info := lbInfoWith(map[int32][]*loadbalancer.Target{
		80: {
			{Protocol: loadbalancer.Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30080},
			{Protocol: loadbalancer.Protocol_TCP, DstIP: "192.168.100.6", DstPort: 30080},
		},
		443: {
			{Protocol: loadbalancer.Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30443},
			{Protocol: loadbalancer.Protocol_TCP, DstIP: "192.168.100.6", DstPort: 30443},
		},
	})
	lb.handleMappings(context.Background(), info, testService(), []*v1.Node{testNode("192.168.100.5")})
	if len(fake.added) != 0 {
		t.Errorf("unexpected additions: %v", fake.added)
	}
	if len(fake.removed) != 2 {
		t.Errorf("expected the two mappings of the departed node to be removed, got %v", fake.removed)
	}
	for _, r := range fake.removed {
		if r.IP != "192.168.100.6" {
			t.Errorf("removed a mapping of a node that is still present: %v", r)
		}
	}
}
