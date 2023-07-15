package loadbalancer

import (
	context "context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"syscall"

	"github.com/knadh/koanf/v2"
	"github.com/moby/ipvs"
	"github.com/rosedblabs/rosedb/v2"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
)

// Error codes that can appear in the Error.Code field
const (
	ErrSuccess              = 0b0000000
	ErrNoSuchLB             = 0b0000001
	ErrNoSuchService        = 0b0000010
	ErrNoSuchIP             = 0b0000100
	ErrAddDestinationFailed = 0b0001000
	ErrDelDestinationFailed = 0b0010000
	ErrTargetAlreadyMapped  = 0b0100000
	ErrTargetNotMapped      = 0b1000000
)

var (
	errNoSuchLoadbalancer = fmt.Errorf("no such loadbalancer")
)

type loadBalancerServer struct {
	UnimplementedLoadBalancerServer

	cidr         *netlink.Addr
	db           *rosedb.DB
	externalLink netlink.Link
	ns           netns.NsHandle
	nlHandle     *netlink.Handle
	ipvsHandle   *ipvs.Handle
}

type SingleLoadBalancer struct {
	Name     string            `json:"name"`
	IP       net.IP            `json:"ip"`
	Port     uint16            `json:"port"`
	Protocol Protocol          `json:"protocol"`
	Mappings map[uint32]net.IP `json:"mappings"`
}

type Intf interface {
	LoadBalancerServer
	Close()
}

func (l *loadBalancerServer) Close() {
	l.ipvsHandle.Close()
	l.nlHandle.Delete()
	l.ns.Close()
	l.db.Close()
}

func NewServer(config *koanf.Koanf) (Intf, error) {

	cidr, err := netlink.ParseAddr(config.MustString("loadbalancer.ipam.cidr"))
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
	ipvsHandle, err := ipvs.NewInNamespace(ns)
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
		ipvsHandle:   ipvsHandle,
	}
	return lbServer, nil
}

func (s SingleLoadBalancer) AsLBInformation() *LoadBalancerInformation {
	targets := make([]*Target, len(s.Mappings))
	count := 0
	for k, v := range s.Mappings {
		targets[count] = &Target{
			DstIP:   v.String(),
			DstPort: k,
		}
		count++
	}

	return &LoadBalancerInformation{
		Name:     s.Name,
		IpAddr:   s.IP.String(),
		Port:     uint32(s.Port),
		Protocol: s.Protocol,
		Targets:  targets,
	}
}

func (s SingleLoadBalancer) toIPVSService() *ipvs.Service {
	var fam uint16 = syscall.AF_INET
	if s.IP.To4() == nil {
		fam = syscall.AF_INET6
	}
	var protocol uint16 = syscall.IPPROTO_TCP
	if s.Protocol == Protocol_UDP {
		protocol = syscall.IPPROTO_UDP
	}
	return &ipvs.Service{
		Address:       s.IP,
		Port:          s.Port,
		Protocol:      protocol,
		AddressFamily: fam,
		SchedName:     "rr",
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
	if _, err := l.db.Get([]byte(clb.Name)); err != nil && err != rosedb.ErrKeyNotFound {
		return nil, err
	} else if err == nil {
		return nil, fmt.Errorf("loadbalancer called %s already exists", clb.Name)
	}
	// TODO: Verify that clb.IpAddr is contained in the current configured CIDR

	slb := SingleLoadBalancer{
		Name:     clb.Name,
		IP:       net.ParseIP(*clb.IpAddr),
		Port:     uint16(clb.Port),
		Protocol: clb.Protocol,
		Mappings: map[uint32]net.IP{},
	}
	log.Printf("slb is %+v", slb)
	// 1. Create a new alias on the interface that matches the required IP
	//    There could already be such an interface, since there might be another LB with the same IP and different port
	requiredAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   slb.IP,
			Mask: l.cidr.Mask,
		},
	}
	addrs, err := l.nlHandle.AddrList(l.externalLink, netlink.FAMILY_ALL)
	addrFound := false
	for _, addr := range addrs {
		addrFound = requiredAddr.Equal(addr)
		if addrFound {
			break
		}
	}

	// 2. Create an IPVS service for the ip:port combination
	srv := slb.toIPVSService()
	if l.ipvsHandle.IsServicePresent(srv) {
		return nil, fmt.Errorf("Service is already defined in ipvs")
	}

	// 3. Actually create everything
	if !addrFound {
		err = l.nlHandle.AddrAdd(l.externalLink, requiredAddr)
		if err != nil {
			return nil, err
		}
	}
	if err = l.ipvsHandle.NewService(srv); err != nil {
		if !addrFound {
			// Remove the address that was just added
			err2 := l.nlHandle.AddrDel(l.externalLink, requiredAddr)
			if err2 != nil {
				log.Printf("cleaning up failed: %v", err2)
			}
		}
		return nil, err
	}

	slbData, err := json.Marshal(slb)
	if err != nil {
		return nil, err
	}
	err = l.db.Put([]byte(clb.Name), slbData)
	return slb.AsLBInformation(), err
}

func (l *loadBalancerServer) Delete(ctx context.Context, lbName *LoadBalancerName) (*Error, error) {
	slb, err := l.getSLB(lbName.Name)
	if err != nil {
		if err == errNoSuchLoadbalancer {
			return &Error{Message: fmt.Sprintf("no such loadbalancer %s", lbName.Name), Code: ErrNoSuchLB}, nil
		}
		return nil, err
	}

	retErr := &Error{}
	// 1. Remove the IPVS service
	srv := slb.toIPVSService()
	if err := l.ipvsHandle.DelService(srv); err != nil {
		log.Printf("Error removing the ipvs service: %+v", err)
		retErr.Message = err.Error()
		retErr.Code = ErrNoSuchService
	}

	// 2. If there are no other IPVS services using this IP address, remove the IP alias
	allServices, err := l.ipvsHandle.GetServices()
	if err != nil {
		return nil, err
	}
	stillUsed := false
	for _, currSrv := range allServices {
		if srv.Address.Equal(currSrv.Address) {
			stillUsed = true
			break
		}
	}
	if !stillUsed {
		addrToDelete := &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   slb.IP,
				Mask: l.cidr.Mask,
			},
		}
		if err := l.nlHandle.AddrDel(l.externalLink, addrToDelete); err != nil {
			log.Printf("error removing the ip alias: %+v", err)
			retErr.Message = retErr.Message + "\n" + err.Error()
			retErr.Code |= ErrNoSuchIP
		}
	}

	if err := l.db.Delete([]byte(lbName.Name)); err != nil {
		return nil, err
	}

	return retErr, nil
}

func (l *loadBalancerServer) AddTarget(ctx context.Context, atr *AddTargetRequest) (*Error, error) {
	slb, err := l.getSLB(atr.LbName)
	if err != nil {
		if err == errNoSuchLoadbalancer {
			return &Error{Message: fmt.Sprintf("no such loadbalancer %s", atr.LbName), Code: ErrNoSuchLB}, nil
		}
		return nil, err
	}
	if _, found := slb.Mappings[atr.Target.DstPort]; found {
		return &Error{
			Message: fmt.Sprintf("target %+v already mapped to load balancer %s", atr.Target, atr.LbName),
			Code:    ErrTargetAlreadyMapped,
		}, nil
	}

	targetIP := net.ParseIP(atr.Target.DstIP)
	var fam uint16 = syscall.AF_INET
	if targetIP.To4() == nil {
		fam = syscall.AF_INET6
	}
	srv := slb.toIPVSService()
	newDestination := &ipvs.Destination{
		Address:         targetIP,
		Port:            uint16(atr.Target.DstPort),
		ConnectionFlags: ipvs.ConnFwdMasq,
		AddressFamily:   fam,
		Weight:          1,
	}
	if err := l.ipvsHandle.NewDestination(srv, newDestination); err != nil {
		log.Printf("error adding a new destination %+v to service %+v: %+v", atr.Target, slb, err)
		return &Error{
			Message: err.Error(),
			Code:    ErrAddDestinationFailed,
		}, nil
	}
	slb.Mappings[atr.Target.DstPort] = targetIP

	slbData, err := json.Marshal(slb)
	if err != nil {
		return nil, err
	}
	if err := l.db.Put([]byte(atr.LbName), slbData); err != nil {
		return nil, err
	}
	return &Error{}, err
}

func (l *loadBalancerServer) DelTarget(ctx context.Context, dtr *DelTargetRequest) (*Error, error) {
	slb, err := l.getSLB(dtr.LbName)
	if err != nil {
		if err == errNoSuchLoadbalancer {
			return &Error{Message: fmt.Sprintf("no such loadbalancer %s", dtr.LbName), Code: ErrNoSuchLB}, nil
		}
		return nil, err
	}
	if _, found := slb.Mappings[dtr.Target.DstPort]; !found {
		return &Error{
			Message: fmt.Sprintf("target %+v not mapped to load balancer %s", dtr.Target, dtr.LbName),
			Code:    ErrTargetNotMapped,
		}, nil
	}

	targetIP := net.ParseIP(dtr.Target.DstIP)
	var fam uint16 = syscall.AF_INET
	if targetIP.To4() == nil {
		fam = syscall.AF_INET6
	}
	srv := slb.toIPVSService()
	dest := &ipvs.Destination{
		Address:         targetIP,
		Port:            uint16(dtr.Target.DstPort),
		ConnectionFlags: ipvs.ConnFwdMasq,
		AddressFamily:   fam,
		Weight:          1,
	}
	if err := l.ipvsHandle.DelDestination(srv, dest); err != nil {
		log.Printf("error removing a destination %+v from service %+v: %+v", dtr.Target, slb, err)
		return &Error{
			Message: err.Error(),
			Code:    ErrDelDestinationFailed,
		}, nil
	}
	delete(slb.Mappings, dtr.Target.DstPort)

	slbData, err := json.Marshal(slb)
	if err != nil {
		return nil, err
	}
	if err := l.db.Put([]byte(dtr.LbName), slbData); err != nil {
		return nil, err
	}
	return &Error{}, nil
}

func (l *loadBalancerServer) getSLB(name string) (SingleLoadBalancer, error) {
	slbRaw, err := l.db.Get([]byte(name))
	if err != nil {
		if err == rosedb.ErrKeyNotFound {
			return SingleLoadBalancer{}, errNoSuchLoadbalancer
		}
		return SingleLoadBalancer{}, err
	}
	var slb SingleLoadBalancer
	if err := json.Unmarshal(slbRaw, &slb); err != nil {
		return SingleLoadBalancer{}, err
	}
	return slb, nil
}

func asPtr[T any](s T) *T {
	return &s
}
