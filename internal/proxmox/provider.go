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

type targetKey struct {
	IP       string
	DstPort  int32
	SrcPort  int32
	Protocol loadbalancer.Protocol
}

func (t *targetKey) String() string {
	return fmt.Sprintf("(%d) %d->%s:%d", t.Protocol, t.SrcPort, t.IP, t.DstPort)
}

func newTargetKey(t *loadbalancer.Target, srcPort int32) targetKey {
	return targetKey{
		IP:       t.DstIP,
		DstPort:  t.DstPort,
		SrcPort:  srcPort,
		Protocol: t.Protocol,
	}
}

func (t *targetKey) toTarget() *loadbalancer.Target {
	return &loadbalancer.Target{
		Protocol: t.Protocol,
		DstIP:    t.IP,
		DstPort:  t.DstPort,
	}
}

type Provider struct {
	lbManager ProxmoxLoadBalancer
}

var _ cloudprovider.Interface = &Provider{}

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

	return &Provider{
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
	lbName := p.GetLoadBalancerName(ctx, clusterName, service)
	lbInfo, err := p.client.GetLoadBalancer(ctx, &loadbalancer.LoadBalancerName{
		Name: lbName,
	})
	if err != nil || lbInfo.Name == "" {
		return nil, false, err
	}
	for _, port := range service.Spec.Ports {
		currPortError := ""
		if tgt, found := lbInfo.Targets[port.Port]; !found || len(tgt.Target) == 0 {
			currPortError = fmt.Sprintf("Port %d is not mapped", port.Port)
		}
		portStatus = append(portStatus, v1.PortStatus{
			Port:     port.Port,
			Protocol: "TCP",
			Error:    &currPortError,
		})
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
func (p *ProxmoxLoadBalancer) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return fmt.Sprintf("%s%%%s%%%s",
		clusterName,
		service.Namespace,
		service.Name)
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (p *ProxmoxLoadBalancer) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	log.Printf("EnsureLoadBalancer called for service %+v", service)
	name := p.GetLoadBalancerName(ctx, clusterName, service)
	lbRequest := &loadbalancer.CreateLoadBalancer{
		Name: name,
	}
	if service.Spec.LoadBalancerIP != "" {
		lbRequest.IpAddr = &service.Spec.LoadBalancerIP
	}
	lbinfo, err := p.client.GetLoadBalancer(ctx, &loadbalancer.LoadBalancerName{
		Name: name,
	})
	if lbinfo == nil || lbinfo.Name == "" {
		// No such loadbalancer exists
		lbinfo, err = p.client.Create(ctx, lbRequest)
		log.Printf("Calling create for %+v", lbRequest)
		if err != nil {
			log.Printf("Create failed: %+v", err)
			return nil, err
		}
	}
	log.Printf("LBInfo: %+v", lbinfo)
	ports := p.handleMappings(ctx, lbinfo, service, nodes)
	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{
				IP:    lbinfo.IpAddr,
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
	name := p.GetLoadBalancerName(ctx, clusterName, service)
	lbinfo, err := p.client.GetLoadBalancer(ctx, &loadbalancer.LoadBalancerName{
		Name: name,
	})
	if lbinfo.Name == "" {
		// No such loadbalancer exists
		return err
	}
	log.Printf("LBInfo: %+v", lbinfo)
	p.handleMappings(ctx, lbinfo, service, nodes)
	return nil
}

func (p *ProxmoxLoadBalancer) handleMappings(ctx context.Context, lbinfo *loadbalancer.LoadBalancerInformation, service *v1.Service, nodes []*v1.Node) []v1.PortStatus {

	currentMappings := map[targetKey]int{}
	for port, targetList := range lbinfo.Targets {
		if targetList.Target != nil {
			for i, v := range targetList.Target {
				currentMappings[newTargetKey(v, port)] = i
			}
		}
	}
	log.Printf("Current mappings: %+v", currentMappings)
	ports := []v1.PortStatus{}
	for _, port := range service.Spec.Ports {
		if toLBProtocol(port.Protocol) < 0 {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
				Error:    asPtr(fmt.Sprintf("unsupported protocol: %s", port.Protocol)),
			})
			continue
		}
		atr := &loadbalancer.AddTargetRequest{
			LbName:  lbinfo.Name,
			SrcPort: port.Port,
		}
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
			target := &loadbalancer.Target{
				DstIP:    nodeIP,
				DstPort:  port.NodePort,
				Protocol: toLBProtocol(port.Protocol),
			}
			currTargetKey := newTargetKey(target, port.Port)
			if _, found := currentMappings[currTargetKey]; !found {
				atr.Target = target
				stat := v1.PortStatus{
					Port:     port.Port,
					Protocol: port.Protocol,
				}
				log.Printf("Adding a mapping for srcPort: %d %+v", port.Port, target)
				code, err := p.client.AddTarget(ctx, atr)
				log.Printf("err: %+v code: %+v", err, code)
				if err != nil {
					stat.Error = asPtr(err.Error())
				} else if code.Code != loadbalancer.ErrSuccess {
					stat.Error = asPtr(code.Message)
				}
				ports = append(ports, stat)
			} else {
				log.Printf("Mapping already exists for %+v", target)
				ports = append(ports, v1.PortStatus{
					Port:     port.Port,
					Protocol: port.Protocol,
				})
				// Mark this protocol/ip/port combination as handled
				delete(currentMappings, currTargetKey)
			}
		}
		// Whatever is left in currentMappings should be removed
		for k := range currentMappings {
			log.Printf("removing mapping for %+v", k)
			dtr := &loadbalancer.DelTargetRequest{
				LbName:  lbinfo.Name,
				SrcPort: k.SrcPort,
				Target:  k.toTarget(),
			}
			code, err := p.client.DelTarget(ctx, dtr)
			if err != nil {
				log.Printf("Failed to remove unused mapping %s: %s", k.String(), err)
			} else if code.Code != loadbalancer.ErrSuccess {
				log.Printf("Failed to remove unused mapping %s: (%d) %s", k.String(), code.Code, code.Message)
			}
		}
	}
	log.Printf("finished handling mappings. Ports: %+v", ports)
	return ports
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

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping or run custom controllers specific to the cloud provider.
// Any tasks started here should be cleaned up when the stop channel closes.
func (p *Provider) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	// TODO: add a goroutine that will stop the client when the provider needs to stop
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
func (p *Provider) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return &p.lbManager, true
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (p *Provider) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

// InstancesV2 is an implementation for instances and should only be implemented by external cloud providers.
// Implementing InstancesV2 is behaviorally identical to Instances but is optimized to significantly reduce
// API calls to the cloud provider when registering and syncing nodes. Implementation of this interface will
// disable calls to the Zones interface. Also returns true if the interface is supported, false otherwise.
func (p *Provider) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return nil, false
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
// DEPRECATED: Zones is deprecated in favor of retrieving zone/region information from InstancesV2.
// This interface will not be called if InstancesV2 is enabled.
func (p *Provider) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (p *Provider) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (p *Provider) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (p *Provider) ProviderName() string {
	return "proxmox"
}

// HasClusterID returns true if a ClusterID is required and set
func (p *Provider) HasClusterID() bool {
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

func toLBProtocol(p v1.Protocol) loadbalancer.Protocol {
	switch p {
	case v1.ProtocolTCP:
		return loadbalancer.Protocol_TCP
	case v1.ProtocolUDP:
		return loadbalancer.Protocol_UDP
	}
	return -1
}
