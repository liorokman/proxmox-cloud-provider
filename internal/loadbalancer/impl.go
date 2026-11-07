// Copyright 2023-2026 Lior Okman
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package loadbalancer

import (
	context "context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"syscall"

	"github.com/knadh/koanf/v2"
	"github.com/rosedblabs/rosedb/v2"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudflare/ipvs"
	"github.com/liorokman/proxmox-cloud-provider/internal/ipam"
	"github.com/liorokman/proxmox-cloud-provider/internal/ipvsns"
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
	ErrPortConflict         = (1 << 8)
)

var (
	errNoSuchLoadbalancer = fmt.Errorf("no such loadbalancer")
)

type loadBalancerServer struct {
	UnimplementedLoadBalancerServer

	// mu serializes every request: each one reads, changes IPVS and the
	// netlink state, and writes the database record.
	mu sync.Mutex

	cidr         *netlink.Addr
	db           *rosedb.DB
	externalLink netlink.Link
	ns           netns.NsHandle
	nlHandle     *netlink.Handle
	ipvsHandle   ipvsns.Client
	ipam         ipam.IPAM

	// Health checking; the tracker is protected by mu
	healthCfg healthConfig
	health    *healthTracker
	probe     func(t *Target) bool
	kick      chan struct{}
}

type Intf interface {
	LoadBalancerServer
	Close()
	Restore() error
	// StartHealthChecks probes the targets until ctx is done. It returns
	// immediately if health checking is disabled.
	StartHealthChecks(ctx context.Context)
	// ServeMetrics serves Prometheus metrics on addr (host:port) until ctx is
	// done. An empty addr does nothing.
	ServeMetrics(ctx context.Context, addr string) error
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
	ipvsHandle, err := ipvsns.Open(ns)
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
	// Addresses that are configured on the interface, but not held by a load
	// balancer, belong to somebody else (the container itself, for example)
	isBusy := func(ip net.IP) bool {
		addrs, err := netlinkHandle.AddrList(netIf, netlink.FAMILY_ALL)
		if err != nil {
			return true
		}
		for _, addr := range addrs {
			if addr.IP.Equal(ip) {
				return true
			}
		}
		return false
	}
	ipManagement, err := ipam.New(config, isBusy)
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
	lbServer.kick = make(chan struct{}, 1)
	lbServer.healthCfg = readHealthConfig(config)
	lbServer.health = newHealthTracker(lbServer.healthCfg.fall, lbServer.healthCfg.rise)
	// The addresses in use are exactly the ones recorded in the database
	if err := lbServer.forEachLB(func(lb *LoadBalancerInformation) {
		ipManagement.Adopt(net.ParseIP(lb.IpAddr))
	}); err != nil {
		lbServer.Close()
		return nil, err
	}
	return lbServer, nil
}

func readHealthConfig(config *koanf.Koanf) healthConfig {
	cfg := healthConfig{
		enabled:  true,
		interval: defaultHealthInterval,
		timeout:  defaultHealthTimeout,
		fall:     defaultHealthFall,
		rise:     defaultHealthRise,
	}
	if config.Exists("loadbalancer.healthCheck.enabled") {
		cfg.enabled = config.Bool("loadbalancer.healthCheck.enabled")
	}
	if d := config.Duration("loadbalancer.healthCheck.interval"); d > 0 {
		cfg.interval = d
	}
	if d := config.Duration("loadbalancer.healthCheck.timeout"); d > 0 {
		cfg.timeout = d
	}
	if n := config.Int("loadbalancer.healthCheck.fall"); n > 0 {
		cfg.fall = n
	}
	if n := config.Int("loadbalancer.healthCheck.rise"); n > 0 {
		cfg.rise = n
	}
	return cfg
}

func (l *loadBalancerServer) StartHealthChecks(ctx context.Context) {
	if !l.healthCfg.enabled {
		log.Printf("health checks are disabled")
		return
	}
	if l.probe == nil {
		l.probe = newTCPProber(l.ns, l.healthCfg.timeout).probe
	}
	go l.runHealthChecks(ctx)
}

// forEachLB calls f for every load balancer record in the database. Records
// that can't be decoded are logged and skipped.
func (l *loadBalancerServer) forEachLB(f func(*LoadBalancerInformation)) error {
	iter := l.db.NewIterator(rosedb.DefaultIteratorOptions)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		val, err := iter.Value()
		if err != nil {
			return err
		}
		var curr LoadBalancerInformation
		if err := json.Unmarshal(val, &curr); err != nil {
			log.Printf("skipping an unreadable load balancer record %q: %v", iter.Key(), err)
			continue
		}
		if curr.Targets == nil {
			curr.Targets = map[int32]*TargetList{}
		}
		f(&curr)
	}
	return nil
}

func toIPVSService(l *LoadBalancerInformation, srcPort int32, protocol Protocol) ipvs.Service {
	addr, _ := netip.ParseAddr(l.IpAddr)
	addr = addr.Unmap()
	fam := ipvs.INET
	if addr.Is6() {
		fam = ipvs.INET6
	}
	p := ipvs.TCP
	if protocol == Protocol_UDP {
		p = ipvs.UDP
	}
	return ipvs.Service{
		Address:   addr,
		Netmask:   ipvsns.FullMask(addr),
		Port:      uint16(srcPort),
		Protocol:  p,
		Family:    fam,
		Scheduler: "rr",
	}
}

// Restore applies all the settings stored in the database to the current
// state. A load balancer that can't be restored is logged and skipped, so that
// one bad record does not take down all the others.
func (l *loadBalancerServer) Restore() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.forEachLB(func(lb *LoadBalancerInformation) {
		if err := l.ensureAddrAlias(lb.IpAddr); err != nil {
			log.Printf("restore: load balancer %s: cannot configure address %s: %v", lb.Name, lb.IpAddr, err)
			return
		}
		for port, targets := range lb.Targets {
			for _, target := range targets.GetTarget() {
				if res, err := l.mapTarget(lb, port, target); err != nil {
					log.Printf("restore: load balancer %s: port %d target %+v: %v", lb.Name, port, target, err)
				} else if res != nil {
					log.Printf("restore: load balancer %s: port %d target %+v: %s", lb.Name, port, target, res.Message)
				}
			}
		}
	})
}

// Get all information about all defined Load Balancers
func (l *loadBalancerServer) GetLoadBalancers(_ *emptypb.Empty, stream LoadBalancer_GetLoadBalancersServer) error {
	l.mu.Lock()
	all := []*LoadBalancerInformation{}
	err := l.forEachLB(func(lb *LoadBalancerInformation) { all = append(all, lb) })
	l.mu.Unlock()
	if err != nil {
		return err
	}
	for _, lb := range all {
		if err := stream.Send(lb); err != nil {
			return err
		}
	}
	return nil
}

// Get information about a specific Load Balancer
func (l *loadBalancerServer) GetLoadBalancer(ctx context.Context, name *LoadBalancerName) (*LoadBalancerInformation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	curr, err := l.getLB(name.Name)
	if err == errNoSuchLoadbalancer {
		// An unknown load balancer comes back empty: the caller checks the name
		return &LoadBalancerInformation{}, nil
	}
	if err != nil {
		return &LoadBalancerInformation{}, err
	}
	return curr, nil
}

func (l *loadBalancerServer) Create(ctx context.Context, clb *CreateLoadBalancer) (*LoadBalancerInformation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	log.Printf("Incoming request: %+v", clb)
	return l.createLocked(clb.Name, clb.IpAddr)
}

// createLocked creates a new, empty load balancer. The caller holds l.mu.
func (l *loadBalancerServer) createLocked(name string, requestedIP *string) (*LoadBalancerInformation, error) {
	if _, err := l.db.Get([]byte(name)); err != nil && err != rosedb.ErrKeyNotFound {
		return nil, err
	} else if err == nil {
		return nil, fmt.Errorf("loadbalancer called %s already exists", name)
	}

	// Take a reference on the address, either a free one or the one requested.
	// An address that is already held by another load balancer is shared.
	var ip net.IP
	if requestedIP == nil || *requestedIP == "" {
		var err error
		if ip, err = l.ipam.Allocate(); err != nil {
			return nil, err
		}
	} else {
		ip = net.ParseIP(*requestedIP)
		if err := l.ipam.Reserve(ip); err != nil {
			return nil, err
		}
	}

	lbInfo := &LoadBalancerInformation{
		Name:    name,
		IpAddr:  ip.String(),
		Targets: map[int32]*TargetList{},
	}

	// Create a new alias on the interface that matches the required IP
	// There could already be such an interface, since there might be another LB with the same IP and different port
	if err := l.ensureAddrAlias(lbInfo.IpAddr); err != nil {
		log.Printf("error adding an alias for the loadbalancer %s: %s", lbInfo.Name, err.Error())
		l.releaseAddr(ip)
		return nil, err
	}

	if err := l.saveLB(lbInfo); err != nil {
		l.releaseAddr(ip)
		return nil, err
	}
	return lbInfo, nil
}

func (l *loadBalancerServer) saveLB(lb *LoadBalancerInformation) error {
	data, err := json.Marshal(lb)
	if err != nil {
		return err
	}
	return l.db.Put([]byte(lb.Name), data)
}

// releaseAddr drops a reference on the address, and removes it from the
// interface when no load balancer holds it anymore.
func (l *loadBalancerServer) releaseAddr(ip net.IP) error {
	if l.ipam.Release(ip) > 0 {
		return nil
	}
	return l.nlHandle.AddrDel(l.externalLink, &netlink.Addr{
		IPNet: &net.IPNet{IP: ip, Mask: l.cidr.Mask},
	})
}

// ensureAddrAlias makes sure the address is configured on the external interface.
func (l *loadBalancerServer) ensureAddrAlias(addrString string) error {
	externalIP := net.ParseIP(addrString)
	if externalIP == nil {
		return fmt.Errorf("invalid IP address %q", addrString)
	}
	requiredAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   externalIP,
			Mask: l.cidr.Mask,
		},
	}
	addrs, err := l.nlHandle.AddrList(l.externalLink, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		if requiredAddr.Equal(addr) {
			return nil
		}
	}
	return l.nlHandle.AddrAdd(l.externalLink, requiredAddr)
}

func (l *loadBalancerServer) Delete(ctx context.Context, lbName *LoadBalancerName) (*Error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

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
			if err := l.ipvsHandle.RemoveService(srv); err != nil {
				log.Printf("Error removing the ipvs service: %+v", err)
				retErr.Message = err.Error()
				retErr.Code = ErrNoSuchService
			}
		}
	}

	// 2. release the address; it leaves the interface when nobody else holds it
	if err := l.releaseAddr(net.ParseIP(lbInfo.IpAddr)); err != nil {
		log.Printf("error removing the ip alias: %+v", err)
		retErr.Message = retErr.Message + "\n" + err.Error()
		retErr.Code |= ErrNoSuchIP
	}

	if err := l.db.Delete([]byte(lbName.Name)); err != nil {
		return nil, err
	}

	return retErr, nil
}

func (l *loadBalancerServer) AddTarget(ctx context.Context, atr *AddTargetRequest) (*Error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

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
	if len(targetList.Target) == 0 {
		// The first target on this port creates the IPVS service. Another load
		// balancer that shares the address must not already own it.
		if other := l.portOwner(lbInfo, atr.SrcPort, atr.Target.Protocol); other != "" {
			return &Error{
				Message: fmt.Sprintf("%s port %d/%s is already used by load balancer %s", lbInfo.IpAddr, atr.SrcPort, atr.Target.Protocol, other),
				Code:    ErrPortConflict,
			}, nil
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

	ret, err := l.mapTarget(lbInfo, atr.SrcPort, atr.Target)
	if err != nil || ret != nil {
		return ret, err
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

// portOwner returns the name of another load balancer that has the same
// address and serves the given port and protocol, or "" if there is none.
func (l *loadBalancerServer) portOwner(lb *LoadBalancerInformation, port int32, protocol Protocol) string {
	owner := ""
	l.forEachLB(func(other *LoadBalancerInformation) {
		if owner != "" || other.Name == lb.Name || other.IpAddr != lb.IpAddr {
			return
		}
		for _, t := range other.Targets[port].GetTarget() {
			if t.Protocol == protocol {
				owner = other.Name
				return
			}
		}
	})
	return owner
}

// mapTarget maps a given target to the provided loadbalancer on the provided
// srcPort
func (l *loadBalancerServer) mapTarget(lbInfo *LoadBalancerInformation, srcPort int32, target *Target) (*Error, error) {
	newDestination, err := toIPVSDestination(target)
	if err != nil {
		return nil, err
	}
	newDestination.Weight = uint32(l.weightFor(target))
	srv := toIPVSService(lbInfo, srcPort, target.Protocol)
	if !ipvsns.ServicePresent(l.ipvsHandle, srv) {
		if err := l.ipvsHandle.CreateService(srv); err != nil {
			return nil, err
		}
	}
	err = l.ipvsHandle.CreateDestination(srv, newDestination)
	if errors.Is(err, syscall.EEXIST) {
		// Already programmed, as after a restart of lbmanager: make sure it matches
		err = l.ipvsHandle.UpdateDestination(srv, newDestination)
	}
	if err != nil {
		log.Printf("error adding a new destination %+v to service %+v: %+v", target, lbInfo, err)
		return &Error{
			Message: err.Error(),
			Code:    ErrAddDestinationFailed,
		}, nil
	}
	return nil, nil
}

func (l *loadBalancerServer) DelTarget(ctx context.Context, dtr *DelTargetRequest) (*Error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

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

	srv := toIPVSService(lbInfo, dtr.SrcPort, target.Protocol)
	dest, err := toIPVSDestination(target)
	if err != nil {
		return &Error{Message: err.Error(), Code: ErrDelDestinationFailed}, nil
	}
	if err := l.ipvsHandle.RemoveDestination(srv, dest); err != nil {
		log.Printf("error removing a destination %+v from service %+v: %+v", dtr.Target, lbInfo, err)
		return &Error{
			Message: err.Error(),
			Code:    ErrDelDestinationFailed,
		}, nil
	}
	targetList.Target = append(targetList.Target[:targetToRemove], targetList.Target[targetToRemove+1:]...)

	if len(targetList.Target) == 0 {
		// Delete the service
		if err := l.ipvsHandle.RemoveService(srv); err != nil {
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
