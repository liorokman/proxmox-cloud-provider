package ipam

import (
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/knadh/koanf/v2"
	"github.com/vishvananda/netlink"
)

var _ IPAM = &SimpleIpam{}

type Range struct {
	From net.IP
	To   net.IP
}

type SimpleIpam struct {
	cidr         *net.IPNet
	allowedRange Range
	link         netlink.Link
	nlHandle     *netlink.Handle
}

func New(config *koanf.Koanf, nlHandle *netlink.Handle, link netlink.Link) (IPAM, error) {
	_, cidr, err := net.ParseCIDR(config.MustString("loadbalancer.ipam.cidr"))
	if err != nil {
		return nil, err
	}
	dynamicRange := Range{
		From: net.ParseIP(config.MustString("loadbalancer.ipam.dynamicRange.startAt")),
		To:   net.ParseIP(config.MustString("loadbalancer.ipam.dynamicRange.endAt")),
	}
	if bytesCompare(dynamicRange.From, dynamicRange.To) >= 0 {
		return nil, fmt.Errorf("invalid dynamic range provided: (%s, %s)", dynamicRange.From, dynamicRange.To)
	}
	if !cidr.Contains(dynamicRange.From) || !cidr.Contains(dynamicRange.To) {
		return nil, fmt.Errorf("dynamic range (%s, %s) is not entirely contained in the cidr (%s)",
			dynamicRange.From, dynamicRange.To, cidr)
	}
	return &SimpleIpam{
		cidr:         cidr,
		allowedRange: dynamicRange,
		link:         link,
		nlHandle:     nlHandle,
	}, nil
}

func (r Range) Contains(ip net.IP) bool {
	if ip.IsUnspecified() {
		return false
	}
	if r.From.Equal(ip) || r.To.Equal(ip) {
		return true
	}

	return bytesCompare(ip, r.From) >= 0 && bytesCompare(ip, r.To) <= 0
}

func (s *SimpleIpam) Contains(ip net.IP) bool {
	return s.cidr.Contains(ip)
}

func (s *SimpleIpam) IsUsed(ip net.IP) bool {
	addrs, err := s.nlHandle.AddrList(s.link, netlink.FAMILY_ALL)
	if err != nil {
		return true
	}
	return isUsed(ip, addrs)
}

func (s *SimpleIpam) Allocate() (net.IP, error) {
	addrs, err := s.nlHandle.AddrList(s.link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err
	}
	ipTrie := newIPTrie()
	for _, addr := range addrs {
		ipTrie.Insert(addr.IP.To16())
	}
	currCandidate := s.allowedRange.From
	for bytesCompare(currCandidate, s.allowedRange.To) <= 0 {
		if !ipTrie.Contains(currCandidate) {
			return currCandidate, nil
		}
		currCandidate = getNextIP(currCandidate)
	}
	return nil, fmt.Errorf("no free IP found in dynamic range")
}

func (s *SimpleIpam) Release(ip net.IP) error {
	// Not needed in the simple ipam, if the IP is released from the link
	// then it will be available to be allocated
	return nil
}

// Helper function to compare two net.IP addresses in byte form.
// It returns -1 if a < b, 0 if a == b, and 1 if a > b.
func bytesCompare(left, right net.IP) int {
	leftBytes := left.To16()
	rightBytes := right.To16()

	for i := 0; i < net.IPv6len; i++ {
		if leftBytes[i] < rightBytes[i] {
			return -1
		} else if leftBytes[i] > rightBytes[i] {
			return 1
		}
	}
	return 0
}

func isUsed(ip net.IP, addrs []netlink.Addr) bool {
	for _, addr := range addrs {
		if ip.Equal(addr.IP) {
			return true
		}
	}
	return false
}

func getNextIP(ip net.IP) net.IP {
	nextIP := make(net.IP, len(ip))
	copy(nextIP, ip)

	for i := len(nextIP) - 1; i >= 0; i-- {
		nextIP[i]++
		if nextIP[i] > 0 {
			break
		}
	}

	return nextIP
}

type ipNode struct {
	children map[byte]*ipNode
	isEnd    bool
}

type ipTrie struct {
	root *ipNode
}

// newIPTrie creates a new IPTrie.
func newIPTrie() *ipTrie {
	return &ipTrie{
		root: &ipNode{
			children: make(map[byte]*ipNode),
		},
	}
}

func (n *ipNode) printToLog(lvl int) {
	if n == nil {
		return
	}
	for k, v := range n.children {
		log.Printf("%s %+v", strings.Repeat(" ", lvl), k)
		v.printToLog(lvl + 1)
	}
}

func (t *ipTrie) PrintToLog() {
	t.root.printToLog(0)
}

// Insert adds an IP address to the trie.
func (t *ipTrie) Insert(ip net.IP) {
	node := t.root
	for _, b := range ip {
		if node.children[b] == nil {
			node.children[b] = &ipNode{
				children: make(map[byte]*ipNode),
			}
		}
		node = node.children[b]
	}
	node.isEnd = true
}

// Contains checks if an IP address is present in the trie.
func (t *ipTrie) Contains(ip net.IP) bool {
	node := t.root
	ip = ip.To16()
	for _, b := range ip {
		if node.children[b] == nil {
			return false
		}
		node = node.children[b]
	}
	return node.isEnd
}
