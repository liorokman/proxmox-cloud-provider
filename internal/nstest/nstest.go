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

// Package nstest creates private network namespaces for tests that need root.
package nstest

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

var counter atomic.Int32

// New creates a network namespace with up lo and dummy0 interfaces, and removes it
// when the test ends. It returns the namespace's name. It skips the test if it
// can't create the namespace, which needs root.
func New(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("nstest%d-%d", os.Getpid(), counter.Add(1))

	// Creating a named namespace switches the calling thread into it
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Skipf("cannot read the current network namespace: %v", err)
	}
	defer orig.Close()
	ns, err := netns.NewNamed(name)
	if err != nil {
		t.Skipf("cannot create a network namespace (needs root): %v", err)
	}
	defer ns.Close()
	defer netns.Set(orig) //nolint:errcheck
	t.Cleanup(func() { netns.DeleteNamed(name) })

	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}}); err != nil {
		t.Skipf("cannot create a dummy interface: %v", err)
	}
	for _, ifname := range []string{"lo", "dummy0"} {
		link, err := netlink.LinkByName(ifname)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

// AddAddr configures addresses (CIDR notation) on dummy0 of the namespace
// created by New. IPVS refuses real servers that the namespace has no route to,
// so tests that add destinations need an address in their range.
func AddAddr(t testing.TB, name string, cidrs ...string) {
	t.Helper()
	ns, err := netns.GetFromName(name)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	link, err := h.LinkByName("dummy0")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cidrs {
		addr, err := netlink.ParseAddr(c)
		if err != nil {
			t.Fatal(err)
		}
		// Skip duplicate address detection, so that IPv6 addresses are usable at once
		addr.Flags |= 0x02 // IFA_F_NODAD
		if err := h.AddrAdd(link, addr); err != nil {
			t.Fatal(err)
		}
	}
}

// ARPCapture receives the ARP frames that a namespace sends or receives.
type ARPCapture struct {
	fd int
}

// CaptureARP starts capturing ARP frames in the namespace.
func CaptureARP(t testing.TB, name string) *ARPCapture {
	t.Helper()
	ns, err := netns.GetFromName(name)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	if err := netns.Set(ns); err != nil {
		t.Fatal(err)
	}
	defer netns.Set(orig) //nolint:errcheck

	// A socket for one protocol only sees received frames; ETH_P_ALL also shows
	// what the namespace sends. The protocol is given in network byte order.
	proto := uint16(unix.ETH_P_ALL)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto<<8|proto>>8))
	if err != nil {
		t.Skipf("cannot open a packet socket: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return &ARPCapture{fd: fd}
}

// Next returns the next ARP frame, or nil if none arrives within the timeout.
func (c *ARPCapture) Next(timeout time.Duration) []byte {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			return nil
		}
		buf := make([]byte, 128)
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			return nil
		}
		// Ethernet type 0x0806 at bytes 12 and 13
		if n >= 14 && buf[12] == 0x08 && buf[13] == 0x06 {
			return buf[:n]
		}
	}
}

// MAC returns the hardware address of an interface of the namespace.
func MAC(t testing.TB, name, ifname string) net.HardwareAddr {
	t.Helper()
	ns, err := netns.GetFromName(name)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	link, err := h.LinkByName(ifname)
	if err != nil {
		t.Fatal(err)
	}
	return link.Attrs().HardwareAddr
}
