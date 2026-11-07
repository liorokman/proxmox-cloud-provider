package proxmox

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

type fakeLBClient struct {
	loadbalancer.LoadBalancerClient
	applied []*loadbalancer.ApplyRequest
	errors  map[int32]string

	info      *loadbalancer.LoadBalancerInformation // returned by GetLoadBalancer (default: none)
	getErr    error
	applyErr  error
	deleteRes *loadbalancer.Error // returned by Delete (default: success)
	deleteErr error
	deleted   []string
}

func (f *fakeLBClient) Delete(_ context.Context, in *loadbalancer.LoadBalancerName, _ ...grpc.CallOption) (*loadbalancer.Error, error) {
	f.deleted = append(f.deleted, in.Name)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if f.deleteRes != nil {
		return f.deleteRes, nil
	}
	return &loadbalancer.Error{}, nil
}

func (f *fakeLBClient) Apply(_ context.Context, in *loadbalancer.ApplyRequest, _ ...grpc.CallOption) (*loadbalancer.ApplyResult, error) {
	f.applied = append(f.applied, in)
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &loadbalancer.ApplyResult{
		Info:       &loadbalancer.LoadBalancerInformation{Name: in.Name, IpAddr: "192.168.87.210"},
		PortErrors: f.errors,
	}, nil
}

func (f *fakeLBClient) GetLoadBalancer(_ context.Context, in *loadbalancer.LoadBalancerName, _ ...grpc.CallOption) (*loadbalancer.LoadBalancerInformation, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.info != nil {
		return f.info, nil
	}
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

func TestGetLoadBalancerName(t *testing.T) {
	lb := &LoadBalancer{client: &fakeLBClient{}}
	svc := &v1.Service{}
	svc.Namespace, svc.Name = "default", "web"
	if got := lb.GetLoadBalancerName(context.Background(), "kubernetes", svc); got != "kubernetes%default%web" {
		t.Errorf("got %q", got)
	}
}

func TestToLBProtocol(t *testing.T) {
	if toLBProtocol(v1.ProtocolTCP) != loadbalancer.Protocol_TCP || toLBProtocol(v1.ProtocolUDP) != loadbalancer.Protocol_UDP {
		t.Error("TCP and UDP must map")
	}
	if toLBProtocol(v1.ProtocolSCTP) >= 0 {
		t.Error("SCTP is not supported")
	}
}

func TestGetLoadBalancer(t *testing.T) {
	ctx := context.Background()
	svc := testService()

	// None exists
	lb := &LoadBalancer{client: &fakeLBClient{}}
	if status, exists, err := lb.GetLoadBalancer(ctx, "c", svc); status != nil || exists || err != nil {
		t.Errorf("unknown: %+v %v %v", status, exists, err)
	}

	// An error from the manager is passed on
	lb = &LoadBalancer{client: &fakeLBClient{getErr: errors.New("down")}}
	if _, exists, err := lb.GetLoadBalancer(ctx, "c", svc); err == nil || exists {
		t.Errorf("error: %v %v", exists, err)
	}

	// It exists, with only one of the ports mapped
	lb = &LoadBalancer{client: &fakeLBClient{info: &loadbalancer.LoadBalancerInformation{
		Name: "c%%", IpAddr: "192.168.87.210",
		Targets: map[int32]*loadbalancer.TargetList{
			80: {Target: []*loadbalancer.Target{{DstIP: "10.0.0.1", DstPort: 30080}}},
			53: {},
		},
	}}}
	status, exists, err := lb.GetLoadBalancer(ctx, "c", svc)
	if err != nil || !exists {
		t.Fatalf("%v %v", exists, err)
	}
	ing := status.Ingress[0]
	if ing.IP != "192.168.87.210" || len(ing.Ports) != 3 {
		t.Fatalf("unexpected status %+v", ing)
	}
	if ing.Ports[0].Error != nil {
		t.Errorf("port 80 is mapped: %v", *ing.Ports[0].Error)
	}
	if ing.Ports[1].Error == nil || ing.Ports[2].Error == nil {
		t.Errorf("unmapped ports must report an error: %+v", ing.Ports)
	}
}

func TestUpdateLoadBalancer(t *testing.T) {
	ctx := context.Background()
	svc := testService()
	nodes := []*v1.Node{testNode("192.168.100.5")}
	existing := &loadbalancer.LoadBalancerInformation{Name: "c%%", IpAddr: "192.168.87.210"}

	// Nothing to update when the load balancer doesn't exist
	fake := &fakeLBClient{}
	if err := (&LoadBalancer{client: fake}).UpdateLoadBalancer(ctx, "c", svc, nodes); err != nil || len(fake.applied) != 0 {
		t.Errorf("unknown: %v %d", err, len(fake.applied))
	}

	fake = &fakeLBClient{info: existing}
	if err := (&LoadBalancer{client: fake}).UpdateLoadBalancer(ctx, "c", svc, nodes); err != nil || len(fake.applied) != 1 {
		t.Errorf("update: %v %d", err, len(fake.applied))
	}

	fake = &fakeLBClient{info: existing, errors: map[int32]string{80: "conflict"}}
	if err := (&LoadBalancer{client: fake}).UpdateLoadBalancer(ctx, "c", svc, nodes); err == nil {
		t.Error("port errors must be reported")
	}
	if err := (&LoadBalancer{client: &fakeLBClient{getErr: errors.New("down")}}).UpdateLoadBalancer(ctx, "c", svc, nodes); err == nil {
		t.Error("a lookup error must be reported")
	}
	if err := (&LoadBalancer{client: &fakeLBClient{info: existing, applyErr: errors.New("down")}}).UpdateLoadBalancer(ctx, "c", svc, nodes); err == nil {
		t.Error("an apply error must be reported")
	}
}

func TestEnsureLoadBalancerFailsWhenApplyFails(t *testing.T) {
	lb := &LoadBalancer{client: &fakeLBClient{applyErr: errors.New("down")}}
	if status, err := lb.EnsureLoadBalancer(context.Background(), "c", testService(), nil); err == nil || status != nil {
		t.Errorf("got %+v %v", status, err)
	}
}

func TestEnsureLoadBalancerDeleted(t *testing.T) {
	ctx := context.Background()
	svc := &v1.Service{}
	svc.Namespace, svc.Name = "default", "web"

	fake := &fakeLBClient{}
	if err := (&LoadBalancer{client: fake}).EnsureLoadBalancerDeleted(ctx, "c", svc); err != nil || len(fake.deleted) != 1 || fake.deleted[0] != "c%default%web" {
		t.Errorf("delete: %v %v", err, fake.deleted)
	}
	// A load balancer that is already gone is fine
	fake = &fakeLBClient{deleteRes: &loadbalancer.Error{Code: loadbalancer.ErrNoSuchLB}}
	if err := (&LoadBalancer{client: fake}).EnsureLoadBalancerDeleted(ctx, "c", svc); err != nil {
		t.Errorf("already deleted: %v", err)
	}
	fake = &fakeLBClient{deleteRes: &loadbalancer.Error{Code: loadbalancer.ErrNoSuchIP, Message: "no address"}}
	if err := (&LoadBalancer{client: fake}).EnsureLoadBalancerDeleted(ctx, "c", svc); err == nil {
		t.Error("other error codes must be reported")
	}
	fake = &fakeLBClient{deleteErr: errors.New("down")}
	if err := (&LoadBalancer{client: fake}).EnsureLoadBalancerDeleted(ctx, "c", svc); err == nil {
		t.Error("an rpc error must be reported")
	}
}
