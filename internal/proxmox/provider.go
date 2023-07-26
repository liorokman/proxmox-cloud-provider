package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

type ProxmoxProvider struct {
	lbManager ProxmoxLoadBalancer
}

var _ cloudprovider.Interface = &ProxmoxProvider{}

func init() {
	cloudprovider.RegisterCloudProvider("proxmox", New)
}

type ProxmoxProviderConfig struct {
	Security    Security `yaml:"security"`
	ManagerHost string   `yaml:"managerHost"`
	ManagerPort int      `yaml:"managerPort"`
}

type Security struct {
	CertFile string `yaml:"cert"`
	KeyFile  string `yaml:"key"`
	CAFile   string `yaml:"ca"`
}

func New(config io.Reader) (cloudprovider.Interface, error) {
	c, err := io.ReadAll(config)
	if err != nil {
		return nil, err
	}
	providerConfig := ProxmoxProviderConfig{}
	err = yaml.Unmarshal(c, &providerConfig)
	if err != nil {
		return nil, err
	}

	creds, err := loadKeypair(providerConfig.Security)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}
	serverAddr := fmt.Sprintf("%s:%d", providerConfig.ManagerHost, providerConfig.ManagerPort)
	conn, err := grpc.Dial(serverAddr, opts...)
	if err != nil {
		return nil, err
	}

	return &ProxmoxProvider{
		lbManager: ProxmoxLoadBalancer{
			client: loadbalancer.NewLoadBalancerClient(conn),
		},
	}, nil
}

type ProxmoxLoadBalancer struct {
	client loadbalancer.LoadBalancerClient
}

var _ cloudprovider.LoadBalancer = &ProxmoxLoadBalancer{}

// TODO: Break this up into different interfaces (LB, etc) when we have more than one type of service
// GetLoadBalancer returns whether the specified load balancer exists, and
// if so, what its status is.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *ProxmoxLoadBalancer) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	portStatus := []v1.PortStatus{}
	ip := ""
	for _, port := range service.Spec.Ports {
		lbName := &loadbalancer.LoadBalancerName{
			Name: fmt.Sprintf("%s-%s-%s-%s-%d",
				clusterName,
				service.Namespace,
				service.Name,
				port.Protocol,
				port.Port),
		}
		lbInfo, err := p.client.GetLoadBalancer(ctx, lbName)
		if err != nil {
			return nil, false, err
		}
		currPortError := ""
		if lbInfo.Name == "" {
			currPortError = fmt.Sprintf("port %d isn't configured on the loadbalancer", port.Port)
		}
		if lbInfo.IpAddr != "" {
			ip = lbInfo.IpAddr
		}
		portStatus = append(portStatus, v1.PortStatus{
			Port:     int32(lbInfo.Port),
			Protocol: "TCP",
			Error:    &currPortError,
		})
	}
	retval := &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{
				IP:    ip,
				Ports: portStatus,
			},
		},
	}

	return retval, ip != "", nil
}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (p *ProxmoxLoadBalancer) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return fmt.Sprintf("%s-%s-%s",
		clusterName,
		service.Namespace,
		service.Name)
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *ProxmoxLoadBalancer) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	log.Printf("nodes: %+v, service: %+v\n", nodes, service)
	lbRequest := &loadbalancer.CreateLoadBalancer{}
	if service.Spec.LoadBalancerIP != "" {
		lbRequest.IpAddr = &service.Spec.LoadBalancerIP
	}
	ports := []v1.PortStatus{}
	ipAddr := ""
	for _, port := range service.Spec.Ports {
		switch port.Protocol {
		case v1.ProtocolTCP:
			lbRequest.Protocol = loadbalancer.Protocol_TCP
		case v1.ProtocolUDP:
			lbRequest.Protocol = loadbalancer.Protocol_UDP
		default:
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
				Error:    asPtr("unsupported protocol"),
			})
			continue
		}
		name := fmt.Sprintf("%s-%s-%s-%s-%d",
			clusterName,
			service.Namespace,
			service.Name,
			port.Protocol,
			port.Port)
		lbinfo, err := p.client.GetLoadBalancer(ctx, &loadbalancer.LoadBalancerName{
			Name: name,
		})
		if err != nil {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
				Error:    asPtr(err.Error()),
			})
			continue
		}
		if lbinfo.Name == "" {
			// No such loadbalancer exists
			lbRequest.Name = name
			lbRequest.Port = uint32(port.Port)
			if ipAddr != "" {
				lbRequest.IpAddr = &ipAddr
			}
			lbinfo, err = p.client.Create(ctx, lbRequest)
			log.Printf("Calling create for %+v", lbRequest)
			if err != nil {
				log.Printf("Create failed: %+v", err)
				ports = append(ports, v1.PortStatus{
					Port:     port.Port,
					Protocol: port.Protocol,
					Error:    asPtr(err.Error()),
				})
				continue
			}
		}
		log.Printf("LBInfo: %+v", lbinfo)
		ipAddr = lbinfo.IpAddr
		existingTargets := map[string]*loadbalancer.Target{}
		for i := range lbinfo.Targets {
			key := fmt.Sprintf("%s:%d", lbinfo.Targets[i].DstIP, lbinfo.Targets[i].DstPort)
			existingTargets[key] = lbinfo.Targets[i]
		}
		log.Printf("Existing mappings: %+v", existingTargets)
		for _, currNode := range nodes {
			nodeIP := ""
			for _, currAddr := range currNode.Status.Addresses {
				if currAddr.Type == v1.NodeInternalIP {
					nodeIP = currAddr.Address
					break
				}
			}
			if nodeIP == "" {
				continue
			}
			stat := v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
			}
			reqDest := fmt.Sprintf("%s:%d", nodeIP, port.NodePort)
			if _, found := existingTargets[reqDest]; !found {
				log.Printf("Calling AddTarget on port %d nodeIp %s", port.NodePort, nodeIP)
				code, err := p.client.AddTarget(ctx, &loadbalancer.AddTargetRequest{
					LbName: lbinfo.Name,
					Target: &loadbalancer.Target{
						DstIP:   nodeIP,
						DstPort: uint32(port.NodePort),
					},
				})
				log.Printf("err: %+v code: %+v", err, code)
				if err != nil {
					stat.Error = asPtr(err.Error())
				} else if code.Code != loadbalancer.ErrSuccess {
					stat.Error = asPtr(code.Message)
				}
			} else {
				delete(existingTargets, reqDest)
			}
			ports = append(ports, stat)
		}
		for _, toBeRemoved := range existingTargets {
			log.Printf("Calling DelTarget on port %d nodeIp %s", toBeRemoved.DstPort, toBeRemoved.DstIP)
			code, err := p.client.DelTarget(ctx, &loadbalancer.DelTargetRequest{
				LbName: lbinfo.Name,
				Target: toBeRemoved,
			})
			if err != nil {
				ports = append(ports, v1.PortStatus{
					Port:     int32(toBeRemoved.DstPort),
					Protocol: port.Protocol,
					Error:    asPtr(err.Error()),
				})
			} else if code.Code != loadbalancer.ErrSuccess {
				ports = append(ports, v1.PortStatus{
					Port:     int32(toBeRemoved.DstPort),
					Protocol: port.Protocol,
					Error:    asPtr(code.Message),
				})
			}
		}
	}
	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{
				IP:    ipAddr,
				Ports: ports,
			},
		},
	}, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *ProxmoxLoadBalancer) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	log.Printf("UpdateLoadBalancer called: %+v +%v %+v", clusterName, service, nodes)
	return nil
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it
// exists, returning nil if the load balancer specified either didn't exist or
// was successfully deleted.
// This construction is useful because many cloud providers' load balancers
// have multiple underlying components, meaning a Get could say that the LB
// doesn't exist even if some part of it is still laying around.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *ProxmoxLoadBalancer) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	log.Printf("EnsureLoadBalancerDeleted called: %+v +%v", clusterName, service)
	return nil
}

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping or run custom controllers specific to the cloud provider.
// Any tasks started here should be cleaned up when the stop channel closes.
func (p *ProxmoxProvider) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	// TODO: add a goroutine that will stop the client when the provider needs to stop
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
func (p *ProxmoxProvider) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return &p.lbManager, true
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (p *ProxmoxProvider) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

// InstancesV2 is an implementation for instances and should only be implemented by external cloud providers.
// Implementing InstancesV2 is behaviorally identical to Instances but is optimized to significantly reduce
// API calls to the cloud provider when registering and syncing nodes. Implementation of this interface will
// disable calls to the Zones interface. Also returns true if the interface is supported, false otherwise.
func (p *ProxmoxProvider) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return nil, false
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
// DEPRECATED: Zones is deprecated in favor of retrieving zone/region information from InstancesV2.
// This interface will not be called if InstancesV2 is enabled.
func (p *ProxmoxProvider) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (p *ProxmoxProvider) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (p *ProxmoxProvider) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (p *ProxmoxProvider) ProviderName() string {
	return "proxmox"
}

// HasClusterID returns true if a ClusterID is required and set
func (p *ProxmoxProvider) HasClusterID() bool {
	return true
}

func loadKeypair(security Security) (credentials.TransportCredentials, error) {
	if security.KeyFile == "" {
		security.KeyFile = security.CertFile
	}
	cert, err := tls.LoadX509KeyPair(security.CertFile, security.KeyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(security.CAFile)
	if err != nil {
		return nil, err
	}
	capool := x509.NewCertPool()
	if !capool.AppendCertsFromPEM(ca) {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      capool,
	}), nil
}

func asPtr[T any](v T) *T {
	return &v
}
