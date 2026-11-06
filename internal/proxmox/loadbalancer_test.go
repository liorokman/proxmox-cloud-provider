package proxmox

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

type fakeLBClient struct {
	loadbalancer.LoadBalancerClient
	applied []*loadbalancer.ApplyRequest
	errors  map[int32]string
}

func (f *fakeLBClient) Apply(_ context.Context, in *loadbalancer.ApplyRequest, _ ...grpc.CallOption) (*loadbalancer.ApplyResult, error) {
	f.applied = append(f.applied, in)
	return &loadbalancer.ApplyResult{
		Info:       &loadbalancer.LoadBalancerInformation{Name: in.Name, IpAddr: "192.168.87.210"},
		PortErrors: f.errors,
	}, nil
}

func (f *fakeLBClient) GetLoadBalancer(_ context.Context, in *loadbalancer.LoadBalancerName, _ ...grpc.CallOption) (*loadbalancer.LoadBalancerInformation, error) {
	return &loadbalancer.LoadBalancerInformation{}, nil
}

func testNode(ip string) *v1.Node {
	return &v1.Node{Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: ip}}}}
}

func testService() *v1.Service {
	return &v1.Service{Spec: v1.ServiceSpec{Ports: []v1.ServicePort{
		{Port: 80, NodePort: 30080, Protocol: v1.ProtocolTCP},
		{Port: 53, NodePort: 30053, Protocol: v1.ProtocolUDP},
		{Port: 9, NodePort: 30009, Protocol: v1.ProtocolSCTP},
	}}}
}

func TestDesiredState(t *testing.T) {
	req, ports := desiredState("lb", testService(), []*v1.Node{testNode("192.168.100.5"), testNode("192.168.100.6"), {}})
	if len(req.Targets) != 2 || len(req.Targets[80].Target) != 2 || len(req.Targets[53].Target) != 2 {
		t.Fatalf("unexpected targets: %+v", req.Targets)
	}
	if req.Targets[53].Target[0].Protocol != loadbalancer.Protocol_UDP || req.Targets[80].Target[1].DstIP != "192.168.100.6" ||
		req.Targets[80].Target[0].DstPort != 30080 || req.Targets[80].Target[0].HealthCheckPort != 0 {
		t.Errorf("unexpected target: %+v", req.Targets)
	}
	if len(ports) != 3 || ports[2].Error == nil || ports[0].Error != nil {
		t.Errorf("unexpected port status: %+v", ports)
	}
}

func TestDesiredStateLocalPolicy(t *testing.T) {
	svc := testService()
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	svc.Spec.HealthCheckNodePort = 32000
	req, _ := desiredState("lb", svc, []*v1.Node{testNode("192.168.100.5")})
	if got := req.Targets[80].Target[0].HealthCheckPort; got != 32000 {
		t.Errorf("health check port = %d", got)
	}
}

func TestEnsureLoadBalancerAppliesOnceAndReportsPortErrors(t *testing.T) {
	fake := &fakeLBClient{errors: map[int32]string{80: "conflict"}}
	lb := &LoadBalancer{client: fake}
	svc := testService()
	svc.Spec.LoadBalancerIP = "192.168.87.220"
	status, err := lb.EnsureLoadBalancer(context.Background(), "c", svc, []*v1.Node{testNode("192.168.100.5")})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.applied) != 1 || fake.applied[0].GetIpAddr() != "192.168.87.220" {
		t.Fatalf("unexpected apply calls: %+v", fake.applied)
	}
	ing := status.Ingress[0]
	if ing.IP != "192.168.87.210" || ing.Ports[0].Error == nil || *ing.Ports[0].Error != "conflict" || ing.Ports[1].Error != nil {
		t.Errorf("unexpected status %+v", ing)
	}
}
