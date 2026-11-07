// Package nstest creates private network namespaces for tests that need root.
package nstest

import (
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
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
