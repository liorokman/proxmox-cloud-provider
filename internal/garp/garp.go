// Copyright 2026 Lior Okman
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

// Package garp announces IPv4 addresses on the local network with gratuitous
// ARP, so that hosts which still hold an old MAC address for an address (the
// address moved to another container, for example) update it at once instead
// of sending packets to the old one until their cache entry times out.
package garp

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Frame returns an Ethernet frame holding a gratuitous ARP request for ip,
// sent from the interface with the given MAC address to everybody.
func Frame(mac net.HardwareAddr, ip netip.Addr) ([]byte, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("unsupported hardware address %q", mac)
	}
	ip = ip.Unmap()
	if !ip.Is4() {
		return nil, fmt.Errorf("%s is not an IPv4 address", ip)
	}
	b := make([]byte, 0, 42)
	b = append(b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff) // destination: broadcast
	b = append(b, mac...)                             // source
	b = binary.BigEndian.AppendUint16(b, unix.ETH_P_ARP)
	b = binary.BigEndian.AppendUint16(b, 1)             // hardware type: Ethernet
	b = binary.BigEndian.AppendUint16(b, unix.ETH_P_IP) // protocol type: IPv4
	b = append(b, 6, 4)                                 // address lengths
	b = binary.BigEndian.AppendUint16(b, 1)             // operation: request
	b = append(b, mac...)                               // sender hardware address
	a := ip.As4()
	b = append(b, a[:]...)          // sender protocol address
	b = append(b, 0, 0, 0, 0, 0, 0) // target hardware address: unknown
	b = append(b, a[:]...)          // target protocol address: the same
	return b, nil
}

// Send announces ip on the interface ifname of the network namespace ns. The
// frame is sent three times, a little apart, because a single broadcast is easy
// to lose. It returns after the first one; the others follow in the background.
func Send(ns netns.NsHandle, ifname string, ip netip.Addr) error {
	type result = opened
	done := make(chan result, 1)
	// The socket belongs to the namespace of the thread that creates it
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			done <- result{err: err}
			return
		}
		defer orig.Close()
		if err := netns.Set(ns); err != nil {
			runtime.UnlockOSThread()
			done <- result{err: fmt.Errorf("entering the network namespace: %w", err)}
			return
		}
		r := open(ifname, ip)
		if err := netns.Set(orig); err != nil {
			// Leave the thread locked, so that Go discards it with this goroutine
			if r.fd > 0 {
				unix.Close(r.fd)
			}
			done <- result{err: fmt.Errorf("leaving the network namespace: %w", err)}
			return
		}
		runtime.UnlockOSThread()
		done <- r
	}()
	r := <-done
	if r.err != nil {
		return r.err
	}
	if err := write(r.fd, r.ifi, r.buf); err != nil {
		unix.Close(r.fd)
		return err
	}
	go func() {
		defer unix.Close(r.fd)
		for _, d := range []time.Duration{200 * time.Millisecond, time.Second} {
			time.Sleep(d)
			if write(r.fd, r.ifi, r.buf) != nil {
				return
			}
		}
	}()
	return nil
}

type opened = struct {
	fd  int
	buf []byte
	ifi int
	err error
}

// open creates the packet socket and the frame. It runs on a thread that is in the right namespace.
func open(ifname string, ip netip.Addr) opened {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return opened{err: err}
	}
	frame, err := Frame(ifi.HardwareAddr, ip)
	if err != nil {
		return opened{err: err}
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return opened{err: fmt.Errorf("opening a packet socket: %w", err)}
	}
	return opened{fd: fd, buf: frame, ifi: ifi.Index}
}

func write(fd, ifindex int, frame []byte) error {
	return unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  ifindex,
		Halen:    6,
		Addr:     [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	})
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
