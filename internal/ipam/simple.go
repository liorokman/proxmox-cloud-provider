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

package ipam

import (
	"fmt"
	"net"
	"sync"

	"github.com/knadh/koanf/v2"
)

var _ IPAM = &SimpleIpam{}

// Range is an inclusive range of addresses.
type Range struct {
	From net.IP
	To   net.IP
}

// Contains reports whether ip is inside the range (inclusive).
func (r Range) Contains(ip net.IP) bool {
	if r.From == nil || r.To == nil || ip.IsUnspecified() {
		return false
	}
	return bytesCompare(ip, r.From) >= 0 && bytesCompare(ip, r.To) <= 0
}

// SimpleIpam allocates addresses from configured ranges. Which addresses are
// taken is tracked in memory with reference counts, and rebuilt from the
// load balancer records when the server starts. isBusy lets the caller veto
// addresses that are in use by something else (for example, configured on the
// interface by the host, such as the container's own address).
type SimpleIpam struct {
	mu           sync.Mutex
	cidr         *net.IPNet
	dynamicRange Range
	staticRange  *Range
	isBusy       func(net.IP) bool
	refs         map[string]int
}

// New builds the allocator from the "loadbalancer.ipam" section of the
// configuration:
//
//	cidr: the network of the addresses
//	dynamicRange: addresses handed out automatically; also usable explicitly
//	staticRange: (optional) more addresses that can only be requested explicitly
func New(config *koanf.Koanf, isBusy func(net.IP) bool) (IPAM, error) {
	_, cidr, err := net.ParseCIDR(config.MustString("loadbalancer.ipam.cidr"))
	if err != nil {
		return nil, err
	}
	dynamic, err := parseRange(config, "loadbalancer.ipam.dynamicRange", cidr)
	if err != nil {
		return nil, err
	}
	var static *Range
	if config.Exists("loadbalancer.ipam.staticRange.startAt") {
		r, err := parseRange(config, "loadbalancer.ipam.staticRange", cidr)
		if err != nil {
			return nil, err
		}
		static = &r
	}
	if isBusy == nil {
		isBusy = func(net.IP) bool { return false }
	}
	return &SimpleIpam{
		cidr:         cidr,
		dynamicRange: dynamic,
		staticRange:  static,
		isBusy:       isBusy,
		refs:         map[string]int{},
	}, nil
}

func parseRange(config *koanf.Koanf, path string, cidr *net.IPNet) (Range, error) {
	r := Range{
		From: net.ParseIP(config.MustString(path + ".startAt")),
		To:   net.ParseIP(config.MustString(path + ".endAt")),
	}
	if r.From == nil || r.To == nil || bytesCompare(r.From, r.To) > 0 {
		return Range{}, fmt.Errorf("invalid range provided in %s: (%v, %v)", path, r.From, r.To)
	}
	if !cidr.Contains(r.From) || !cidr.Contains(r.To) {
		return Range{}, fmt.Errorf("%s (%s, %s) is not entirely contained in the cidr (%s)", path, r.From, r.To, cidr)
	}
	return r, nil
}

func (s *SimpleIpam) allowed(ip net.IP) bool {
	return s.dynamicRange.Contains(ip) || (s.staticRange != nil && s.staticRange.Contains(ip))
}

func (s *SimpleIpam) Allocate() (net.IP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ip := s.dynamicRange.From; bytesCompare(ip, s.dynamicRange.To) <= 0; ip = getNextIP(ip) {
		if s.refs[ip.String()] == 0 && !s.isBusy(ip) {
			s.refs[ip.String()] = 1
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no free IP found in the dynamic range (%s - %s)", s.dynamicRange.From, s.dynamicRange.To)
}

func (s *SimpleIpam) Reserve(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("invalid IP address")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed(ip) {
		return fmt.Errorf("requested IP %s is not inside the configured dynamic or static range", ip)
	}
	// An address already held by a load balancer is shared. Otherwise it must
	// not be in use by something else.
	if s.refs[ip.String()] == 0 && s.isBusy(ip) {
		return fmt.Errorf("requested IP %s is already configured on the interface", ip)
	}
	s.refs[ip.String()]++
	return nil
}

func (s *SimpleIpam) Adopt(ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[ip.String()]++
}

func (s *SimpleIpam) Release(ip net.IP) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ip.String()
	if s.refs[key] <= 1 {
		delete(s.refs, key)
		return 0
	}
	s.refs[key]--
	return s.refs[key]
}

func (s *SimpleIpam) IsReserved(ip net.IP) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[ip.String()] > 0
}

// bytesCompare compares two addresses in 16 byte form: -1 if left < right,
// 0 if equal, 1 if left > right.
func bytesCompare(left, right net.IP) int {
	l, r := left.To16(), right.To16()
	for i := 0; i < net.IPv6len; i++ {
		if l[i] < r[i] {
			return -1
		} else if l[i] > r[i] {
			return 1
		}
	}
	return 0
}

func getNextIP(ip net.IP) net.IP {
	next := make(net.IP, len(ip))
	copy(next, ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] > 0 {
			break
		}
	}
	return next
}
