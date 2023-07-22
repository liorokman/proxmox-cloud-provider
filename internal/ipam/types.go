package ipam

import "net"

type IPAM interface {
	Allocate() (net.IP, error)
	Release(ip net.IP) error
	IsUsed(ip net.IP) bool
	Contains(ip net.IP) bool
}
