package proxmox

import (
	"context"
	"fmt"
	"log"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
)

type LoadBalancer struct {
	client loadbalancer.LoadBalancerClient
}

var _ cloudprovider.LoadBalancer = &LoadBalancer{}

// GetLoadBalancer returns whether the specified load balancer exists, and
// if so, what its status is.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *LoadBalancer) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	portStatus := []v1.PortStatus{}
	lbName := p.GetLoadBalancerName(ctx, clusterName, service)
	lbInfo, err := p.client.GetLoadBalancer(ctx, &loadbalancer.LoadBalancerName{
		Name: lbName,
	})
	if err != nil || lbInfo.GetName() == "" {
		return nil, false, err
	}
	for _, port := range service.Spec.Ports {
		stat := v1.PortStatus{
			Port:     port.Port,
			Protocol: port.Protocol,
		}
		if tgt, found := lbInfo.Targets[port.Port]; !found || len(tgt.GetTarget()) == 0 {
			stat.Error = asPtr(fmt.Sprintf("Port %d is not mapped", port.Port))
		}
		portStatus = append(portStatus, stat)
	}
	retval := &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{
				IP:    lbInfo.IpAddr,
				Ports: portStatus,
			},
		},
	}

	return retval, lbInfo.IpAddr != "", nil
}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (p *LoadBalancer) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return fmt.Sprintf("%s%%%s%%%s",
		clusterName,
		service.Namespace,
		service.Name)
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *LoadBalancer) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	log.Printf("EnsureLoadBalancer called for service %s/%s", service.Namespace, service.Name)
	req, ports := desiredState(p.GetLoadBalancerName(ctx, clusterName, service), service, nodes)
	if service.Spec.LoadBalancerIP != "" {
		req.IpAddr = &service.Spec.LoadBalancerIP
	}
	res, err := p.client.Apply(ctx, req)
	if err != nil {
		log.Printf("Apply failed: %+v", err)
		return nil, err
	}
	for i := range ports {
		if msg := res.PortErrors[ports[i].Port]; msg != "" {
			ports[i].Error = &msg
		}
	}
	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{
				IP:    res.Info.IpAddr,
				Ports: ports,
			},
		},
	}, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *LoadBalancer) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	log.Printf("UpdateLoadBalancer called for service %s/%s", service.Namespace, service.Name)
	name := p.GetLoadBalancerName(ctx, clusterName, service)
	lbinfo, err := p.client.GetLoadBalancer(ctx, &loadbalancer.LoadBalancerName{Name: name})
	if err != nil {
		return err
	}
	if lbinfo.GetName() == "" {
		// No such loadbalancer exists
		return nil
	}
	req, _ := desiredState(name, service, nodes)
	res, err := p.client.Apply(ctx, req)
	if err != nil {
		return err
	}
	if len(res.PortErrors) > 0 {
		return fmt.Errorf("failed to update load balancer %s: %v", name, res.PortErrors)
	}
	return nil
}

// desiredState computes the complete set of targets wanted for the service: every
// node with an InternalIP, for every port with a supported protocol. For
// externalTrafficPolicy Local, the load balancer also checks each node's
// healthCheckNodePort, so only nodes that have endpoints get traffic.
// It also returns one PortStatus per service port.
func desiredState(name string, service *v1.Service, nodes []*v1.Node) (*loadbalancer.ApplyRequest, []v1.PortStatus) {
	req := &loadbalancer.ApplyRequest{
		Name:    name,
		Targets: map[int32]*loadbalancer.TargetList{},
	}
	var healthPort int32
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyLocal {
		healthPort = service.Spec.HealthCheckNodePort
	}
	ports := []v1.PortStatus{}
	for _, port := range service.Spec.Ports {
		stat := v1.PortStatus{
			Port:     port.Port,
			Protocol: port.Protocol,
		}
		if toLBProtocol(port.Protocol) < 0 {
			stat.Error = asPtr(fmt.Sprintf("unsupported protocol: %s", port.Protocol))
			ports = append(ports, stat)
			continue
		}
		list := &loadbalancer.TargetList{}
		for _, node := range nodes {
			for _, addr := range node.Status.Addresses {
				if addr.Type != v1.NodeInternalIP {
					continue
				}
				list.Target = append(list.Target, &loadbalancer.Target{
					DstIP:           addr.Address,
					DstPort:         port.NodePort,
					Protocol:        toLBProtocol(port.Protocol),
					HealthCheckPort: healthPort,
				})
				break
			}
		}
		req.Targets[port.Port] = list
		ports = append(ports, stat)
	}
	return req, ports
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it
// exists, returning nil if the load balancer specified either didn't exist or
// was successfully deleted.
// This construction is useful because many cloud providers' load balancers
// have multiple underlying components, meaning a Get could say that the LB
// doesn't exist even if some part of it is still laying around.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *LoadBalancer) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	log.Printf("EnsureLoadBalancerDeleted called: %+v +%v", clusterName, service)
	name := p.GetLoadBalancerName(ctx, clusterName, service)
	code, err := p.client.Delete(ctx, &loadbalancer.LoadBalancerName{
		Name: name,
	})
	if err != nil {
		return err
	} else if code.Code != loadbalancer.ErrSuccess && code.Code != loadbalancer.ErrNoSuchLB {
		return fmt.Errorf("Failed to delete loadbalancer %s: (%d) %s", name, code.Code, code.Message)
	}
	return nil
}

func toLBProtocol(p v1.Protocol) loadbalancer.Protocol {
	switch p {
	case v1.ProtocolTCP:
		return loadbalancer.Protocol_TCP
	case v1.ProtocolUDP:
		return loadbalancer.Protocol_UDP
	}
	return -1
}
