package loadbalancer

import (
	context "context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/knadh/koanf/v2"
	"github.com/rosedblabs/rosedb/v2"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

type loadBalancerServer struct {
	UnimplementedLoadBalancerServer

	cidr         *netlink.Addr
	db           *rosedb.DB
	externalLink netlink.Link
	ns           netns.NsHandle
	nlHandle     *netlink.Handle
}

type SingleLoadBalancer struct {
	Name        string               `json:"name"`
	IP          net.IP               `json:"ip"`
	TCPMappings map[uint32]ipAndPort `json:"tcp"`
	UDPMappings map[uint32]ipAndPort `json:"udp"`
}

type ipAndPort struct {
	IP   net.IP `json:"ip"`
	Port uint32 `json:"port"`
}

type Intf interface {
	LoadBalancerServer
	Close()
}

func (l *loadBalancerServer) Close() {
	l.nlHandle.Delete()
	l.ns.Close()
	l.db.Close()
}

func NewServer(config *koanf.Koanf) (Intf, error) {

	cidr, err := netlink.ParseAddr(config.MustString("loadbalancer.cidr"))
	if err != nil {
		return nil, err
	}
	ns, err := netns.GetFromName(config.MustString("loadbalancer.namespace"))
	if err != nil {
		return nil, err
	}
	netlinkHandle, err := netlink.NewHandleAt(ns)
	if err != nil {
		ns.Close()
		return nil, err
	}
	netIf, err := netlinkHandle.LinkByName(config.MustString("loadbalancer.externalInterface"))
	if err != nil {
		netlinkHandle.Delete()
		ns.Close()
		return nil, err
	}
	options := rosedb.DefaultOptions
	options.DirPath = config.MustString("dbDir")
	// open a database
	db, err := rosedb.Open(options)
	if err != nil {
		netlinkHandle.Delete()
		ns.Close()
		return nil, err
	}
	lbServer := &loadBalancerServer{
		cidr:         cidr,
		externalLink: netIf,
		db:           db,
		ns:           ns,
		nlHandle:     netlinkHandle,
	}
	return lbServer, nil
}

func (s SingleLoadBalancer) AsLBInformation() *LoadBalancerInformation {
	tcpTargets := map[uint32]*Target{}
	udpTargets := map[uint32]*Target{}
	for k, v := range s.TCPMappings {
		tcpTargets[k] = &Target{
			DstIP:   v.IP.String(),
			DstPort: v.Port,
		}
	}
	for k, v := range s.UDPMappings {
		udpTargets[k] = &Target{
			DstIP:   v.IP.String(),
			DstPort: v.Port,
		}
	}
	return &LoadBalancerInformation{
		Name:       s.Name,
		IpAddr:     s.IP.String(),
		TcpTargets: tcpTargets,
		UdpTargets: udpTargets,
	}
}

// Get all information about all defined Load Balancers
func (l *loadBalancerServer) GetLoadBalancers(_ *emptypb.Empty, stream LoadBalancer_GetLoadBalancersServer) error {
	iterOptions := rosedb.DefaultIteratorOptions
	iter := l.db.NewIterator(iterOptions)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		val, err := iter.Value()
		if err != nil {
			return err
		}
		var curr SingleLoadBalancer
		if err := json.Unmarshal(val, &curr); err != nil {
			return err
		}
		if err := stream.Send(curr.AsLBInformation()); err != nil {
			return err
		}
	}
	return nil
}

// Get information about a specific Load Balancer
func (l *loadBalancerServer) GetLoadBalancer(ctx context.Context, name *LoadBalancerName) (*LoadBalancerInformation, error) {

	slb, err := l.db.Get([]byte(name.Name))
	if err != nil {
		return nil, err
	}
	var curr SingleLoadBalancer
	if err := json.Unmarshal(slb, &curr); err != nil {
		return nil, err
	}
	return curr.AsLBInformation(), nil
}

func (l *loadBalancerServer) Create(ctx context.Context, clb *CreateLoadBalancer) (*LoadBalancerInformation, error) {

	if clb.IpAddr == nil {
		// TODO: Get the next available IP from IPAM
		clb.IpAddr = asPtr("192.168.78.30")
	}

	slb := SingleLoadBalancer{
		Name:        clb.Name,
		IP:          net.ParseIP(*clb.IpAddr),
		TCPMappings: map[uint32]ipAndPort{},
		UDPMappings: map[uint32]ipAndPort{},
	}
	if _, err := l.db.Get([]byte(clb.Name)); err != nil && err != rosedb.ErrKeyNotFound {
		return nil, err
	} else if err == nil {
		return nil, fmt.Errorf("loadbalancer called %s already exists", clb.Name)
	}

	// TODO: Actually create a load balancer

	// 1. Create a new alias on the interface that matches the required IP
	//    There could already be such an interface, since there might be another LB with the same IP and different port
	requiredAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   net.ParseIP(*clb.IpAddr),
			Mask: l.cidr.Mask,
		},
	}
	addrs, err := netlink.AddrList(l.externalLink, netlink.FAMILY_ALL)
	found := false
	for _, addr := range addrs {
		found = requiredAddr.Equal(addr)
		if found {
			break
		}
	}
	if !found {
		err = netlink.AddrAdd(l.externalLink, requiredAddr)
		if err != nil {
			return nil, err
		}
	}
	// 2. Create an IPVS service for the ip:port combination

	slbData, err := json.Marshal(slb)
	if err != nil {
		return nil, err
	}
	err = l.db.Put([]byte(clb.Name), slbData)
	return slb.AsLBInformation(), err
}

func (l *loadBalancerServer) Delete(ctx context.Context, lbName *LoadBalancerName) (*Error, error) {
	slb, err := l.db.Get([]byte(lbName.Name))
	if err != nil {
		if err == rosedb.ErrKeyNotFound {
			return &Error{}, nil
		}
		return nil, err
	}
	var curr SingleLoadBalancer
	if err := json.Unmarshal(slb, &curr); err != nil {
		return nil, err
	}

	// TODO: Actually delete the loadbalancer

	if err := l.db.Delete([]byte(lbName.Name)); err != nil {
		return nil, err
	}

	return &Error{}, nil
}

func (l *loadBalancerServer) AddTarget(_ context.Context, _ *AddTargetRequest) (*Error, error) {
	panic("not implemented") // TODO: Implement
}

func (l *loadBalancerServer) DelTarget(_ context.Context, _ *DelTargetRequest) (*Error, error) {
	panic("not implemented") // TODO: Implement
}

func asPtr[T any](s T) *T {
	return &s
}
