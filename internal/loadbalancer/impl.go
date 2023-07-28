package loadbalancer

import (
	context "context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"syscall"

	"github.com/knadh/koanf/v2"
	"github.com/rosedblabs/rosedb/v2"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	"github.com/liorokman/proxmox-cloud-provider/internal/ipam"
	"github.com/liorokman/proxmox-cloud-provider/internal/ipvs"
)

// Error codes that can appear in the Error.Code field
const (
	ErrSuccess              = 0
	ErrNoSuchLB             = (1 << 0)
	ErrNoSuchService        = (1 << 1)
	ErrNoSuchIP             = (1 << 2)
	ErrAddDestinationFailed = (1 << 3)
	ErrDelDestinationFailed = (1 << 4)
	ErrTargetAlreadyMapped  = (1 << 5)
	ErrTargetNotMapped      = (1 << 6)
	ErrIPAMError            = (1 << 7)
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
	ipam         ipam.IPAM
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
		ipvsHandle.Close()
		netlinkHandle.Delete()
		ns.Close()
		return nil, err
	}
	ipManagement, err := ipam.New(config, netlinkHandle, netIf)
	if err != nil {
		db.Close()
		ipvsHandle.Close()
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
		ipam:         ipManagement,
	}
	return lbServer, nil
}

func toIPVSService(l *LoadBalancerInformation, srcPort int32, protocol Protocol) *ipvs.Service {
	srcIP := net.ParseIP(l.IpAddr)
	var fam uint16 = syscall.AF_INET
	if srcIP.To4() == nil {
		fam = syscall.AF_INET6
	}
	var p uint16 = syscall.IPPROTO_TCP
	if protocol == Protocol_UDP {
		p = syscall.IPPROTO_UDP
	}
	return &ipvs.Service{
		Address:       srcIP,
		Port:          uint16(srcPort),
		Protocol:      p,
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
		var curr LoadBalancerInformation
		if err := json.Unmarshal(val, &curr); err != nil {
			return err
		}
		if err := stream.Send(&curr); err != nil {
			return err
		}
	}
	return nil
}

// Get information about a specific Load Balancer
func (l *loadBalancerServer) GetLoadBalancer(ctx context.Context, name *LoadBalancerName) (*LoadBalancerInformation, error) {

	curr, err := l.getLB(name.Name)
	if err != nil && err != errNoSuchLoadbalancer {
		return &LoadBalancerInformation{}, err
	}
	return curr, nil
}

func (l *loadBalancerServer) Create(ctx context.Context, clb *CreateLoadBalancer) (*LoadBalancerInformation, error) {

	log.Printf("Incoming request: %+v", clb)
	if _, err := l.db.Get([]byte(clb.Name)); err != nil && err != rosedb.ErrKeyNotFound {
		return nil, err
	} else if err == nil {
		return nil, fmt.Errorf("loadbalancer called %s already exists", clb.Name)
	}
	if clb.IpAddr == nil || *clb.IpAddr == "" {
		nextIP, err := l.ipam.Allocate()
		if err != nil {
			return nil, err
		}
		clb.IpAddr = asPtr(nextIP.String())
	}

	lbInfo := &LoadBalancerInformation{
		Name:    clb.Name,
		IpAddr:  *clb.IpAddr,
		Targets: map[int32]*TargetList{},
	}
	log.Printf("slb is %+v", lbInfo)

	externalIP := net.ParseIP(lbInfo.IpAddr)
	if !l.ipam.Contains(externalIP) {
		return nil, fmt.Errorf("Requested IP %s is not contained in the configured CIDR (%s)", *clb.IpAddr, l.cidr)
	}

	// 1. Create a new alias on the interface that matches the required IP
	//    There could already be such an interface, since there might be another LB with the same IP and different port
	requiredAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   externalIP,
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
	if !addrFound {
		err = l.nlHandle.AddrAdd(l.externalLink, requiredAddr)
		if err != nil {
			return nil, err
		}
	}

	lbInfoData, err := json.Marshal(lbInfo)
	if err != nil {
		return nil, err
	}
	err = l.db.Put([]byte(clb.Name), lbInfoData)
	return lbInfo, err
}

func (l *loadBalancerServer) Delete(ctx context.Context, lbName *LoadBalancerName) (*Error, error) {
	lbInfo, err := l.getLB(lbName.Name)
	if err != nil {
		if err == errNoSuchLoadbalancer {
			return &Error{Message: fmt.Sprintf("no such loadbalancer %s", lbName.Name), Code: ErrNoSuchLB}, nil
		}
		return nil, err
	}

	retErr := &Error{}
	// 1. Remove the IPVS service
	for port, target := range lbInfo.Targets {
		protocols := map[Protocol]byte{}
		for _, v := range target.Target {
			protocols[v.Protocol] = 0
		}
		for p, _ := range protocols {
			srv := toIPVSService(lbInfo, port, p)
			if err := l.ipvsHandle.DelService(srv); err != nil {
				log.Printf("Error removing the ipvs service: %+v", err)
				retErr.Message = err.Error()
				retErr.Code = ErrNoSuchService
			}
		}
	}

	externalIP := net.ParseIP(lbInfo.IpAddr)
	// 2. remove the IP alias
	addrToDelete := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   externalIP,
			Mask: l.cidr.Mask,
		},
	}
	if err := l.nlHandle.AddrDel(l.externalLink, addrToDelete); err != nil {
		log.Printf("error removing the ip alias: %+v", err)
		retErr.Message = retErr.Message + "\n" + err.Error()
		retErr.Code |= ErrNoSuchIP
	}
	if err := l.ipam.Release(addrToDelete.IP); err != nil {
		log.Printf("error unallocating the ip from ipam: %+v", err)
		retErr.Message = retErr.Message + "\n" + err.Error()
		retErr.Code |= ErrIPAMError
	}

	if err := l.db.Delete([]byte(lbName.Name)); err != nil {
		return nil, err
	}

	return retErr, nil
}

func (l *loadBalancerServer) AddTarget(ctx context.Context, atr *AddTargetRequest) (*Error, error) {
	lbInfo, err := l.getLB(atr.LbName)
	if err != nil {
		if err == errNoSuchLoadbalancer {
			return &Error{Message: fmt.Sprintf("no such loadbalancer %s", atr.LbName), Code: ErrNoSuchLB}, nil
		}
		return nil, err
	}
	targetList, found := lbInfo.Targets[atr.SrcPort]
	if !found || targetList == nil {
		targetList = &TargetList{
			Target: []*Target{},
		}
	}
	for _, target := range targetList.Target {
		if target.Protocol == atr.Target.Protocol &&
			target.DstIP == atr.Target.DstIP &&
			target.DstPort == atr.Target.DstPort {

			return &Error{
				Message: fmt.Sprintf("target %+v already mapped to load balancer %s", atr.Target, atr.LbName),
				Code:    ErrTargetAlreadyMapped,
			}, nil
		}
	}

	targetIP := net.ParseIP(atr.Target.DstIP)
	var fam uint16 = syscall.AF_INET
	if targetIP.To4() == nil {
		fam = syscall.AF_INET6
	}
	srv := toIPVSService(lbInfo, atr.SrcPort, atr.Target.Protocol)
	if !l.ipvsHandle.IsServicePresent(srv) {
		if err = l.ipvsHandle.NewService(srv); err != nil {
			return nil, err
		}
	}
	newDestination := &ipvs.Destination{
		Address:         targetIP,
		Port:            uint16(atr.Target.DstPort),
		ConnectionFlags: ipvs.ConnFwdMasq,
		AddressFamily:   fam,
		Weight:          1,
	}
	if err := l.ipvsHandle.NewDestination(srv, newDestination); err != nil {
		log.Printf("error adding a new destination %+v to service %+v: %+v", atr.Target, lbInfo, err)
		return &Error{
			Message: err.Error(),
			Code:    ErrAddDestinationFailed,
		}, nil
	}
	targetList.Target = append(targetList.Target, atr.Target)
	lbInfo.Targets[atr.SrcPort] = targetList

	slbData, err := json.Marshal(lbInfo)
	if err != nil {
		return nil, err
	}
	if err := l.db.Put([]byte(atr.LbName), slbData); err != nil {
		return nil, err
	}
	return &Error{}, err
}

func (l *loadBalancerServer) DelTarget(ctx context.Context, dtr *DelTargetRequest) (*Error, error) {
	lbInfo, err := l.getLB(dtr.LbName)
	if err != nil {
		if err == errNoSuchLoadbalancer {
			return &Error{Message: fmt.Sprintf("no such loadbalancer %s", dtr.LbName), Code: ErrNoSuchLB}, nil
		}
		return nil, err
	}
	targetList, found := lbInfo.Targets[dtr.SrcPort]
	if !found || targetList == nil {
		return &Error{
			Message: fmt.Sprintf("target %+v not mapped to load balancer %s", dtr.Target, dtr.LbName),
			Code:    ErrTargetNotMapped,
		}, nil
	}
	targetToRemove := -1
	for target, _ := range targetList.Target {
		if targetList.Target[target].Protocol == dtr.Target.Protocol &&
			targetList.Target[target].DstIP == dtr.Target.DstIP &&
			targetList.Target[target].DstPort == dtr.Target.DstPort {

			targetToRemove = target
			break
		}
	}
	if targetToRemove == -1 {
		return &Error{
			Message: fmt.Sprintf("target %+v not mapped to load balancer %s on port %d", dtr.Target, dtr.LbName, dtr.SrcPort),
			Code:    ErrTargetNotMapped,
		}, nil
	}

	target := targetList.Target[targetToRemove]

	targetIP := net.ParseIP(dtr.Target.DstIP)
	var fam uint16 = syscall.AF_INET
	if targetIP.To4() == nil {
		fam = syscall.AF_INET6
	}
	srv := toIPVSService(lbInfo, dtr.SrcPort, target.Protocol)
	dest := &ipvs.Destination{
		Address:         targetIP,
		Port:            uint16(target.DstPort),
		ConnectionFlags: ipvs.ConnFwdMasq,
		AddressFamily:   fam,
		Weight:          1,
	}
	if err := l.ipvsHandle.DelDestination(srv, dest); err != nil {
		log.Printf("error removing a destination %+v from service %+v: %+v", dtr.Target, lbInfo, err)
		return &Error{
			Message: err.Error(),
			Code:    ErrDelDestinationFailed,
		}, nil
	}
	targetList.Target = append(targetList.Target[:targetToRemove], targetList.Target[targetToRemove+1:]...)

	if len(targetList.Target) == 0 {
		// Delete the service
		if err := l.ipvsHandle.DelService(srv); err != nil {
			log.Printf("error removing a destination %+v from service %+v: %+v", dtr.Target, lbInfo, err)
			return &Error{
				Message: err.Error(),
				Code:    ErrDelDestinationFailed,
			}, nil
		}
		delete(lbInfo.Targets, dtr.SrcPort)
	} else {
		lbInfo.Targets[dtr.SrcPort] = targetList
	}

	slbData, err := json.Marshal(lbInfo)
	if err != nil {
		return nil, err
	}
	if err := l.db.Put([]byte(dtr.LbName), slbData); err != nil {
		return nil, err
	}
	return &Error{}, nil
}

func (l *loadBalancerServer) getLB(name string) (*LoadBalancerInformation, error) {
	lbinfoRaw, err := l.db.Get([]byte(name))
	if err != nil {
		if err == rosedb.ErrKeyNotFound {
			return nil, errNoSuchLoadbalancer
		}
		return nil, err
	}
	var lbInfo LoadBalancerInformation
	if err := json.Unmarshal(lbinfoRaw, &lbInfo); err != nil {
		return nil, err
	}
	if lbInfo.Targets == nil {
		lbInfo.Targets = map[int32]*TargetList{}
	}
	return &lbInfo, nil
}

func asPtr[T any](s T) *T {
	return &s
}
