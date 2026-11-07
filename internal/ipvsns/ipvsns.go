// Package ipvsns opens an IPVS client inside a network namespace.
//
// It is a thin layer on github.com/cloudflare/ipvs, which always talks to the
// namespace of the thread that created the client.
package ipvsns

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"runtime"

	"github.com/cloudflare/ipvs"
	"github.com/cloudflare/ipvs/netmask"
	"github.com/vishvananda/netns"
)

// Client is an IPVS client that must be closed.
type Client interface {
	ipvs.Client
	io.Closer
}

// Open returns an IPVS client for the network namespace ns. A netlink socket
// belongs to the namespace that it was created in, so the client is created on
// a thread that has been moved into ns for that moment.
func Open(ns netns.NsHandle) (Client, error) {
	type result struct {
		c   ipvs.Client
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			// Nothing was changed, so the thread can be reused
			runtime.UnlockOSThread()
			done <- result{nil, fmt.Errorf("reading the current network namespace: %w", err)}
			return
		}
		defer orig.Close()
		if err := netns.Set(ns); err != nil {
			runtime.UnlockOSThread()
			done <- result{nil, fmt.Errorf("entering the network namespace: %w", err)}
			return
		}
		c, cerr := ipvs.New()
		if err := netns.Set(orig); err != nil {
			// The thread is stuck in the wrong namespace: leave it locked, so that
			// the Go runtime throws it away when this goroutine ends.
			done <- result{c, fmt.Errorf("leaving the network namespace: %w", err)}
			return
		}
		runtime.UnlockOSThread()
		done <- result{c, cerr}
	}()
	r := <-done
	if r.err != nil {
		if cl, ok := r.c.(io.Closer); ok && r.c != nil {
			cl.Close()
		}
		return nil, r.err
	}
	c, ok := r.c.(Client)
	if !ok {
		return nil, fmt.Errorf("the IPVS client can't be closed")
	}
	return c, nil
}

// ServicePresent reports whether the IPVS service s exists.
func ServicePresent(c ipvs.Client, s ipvs.Service) bool {
	_, err := c.Service(s)
	return err == nil
}

// Services lists the IPVS services. The client reports an empty list as
// os.ErrNotExist, which is not an error here.
func Services(c ipvs.Client) ([]ipvs.ServiceExtended, error) {
	svcs, err := c.Services()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return svcs, err
}

// Destinations lists the destinations of s, reporting none as an empty list.
func Destinations(c ipvs.Client, s ipvs.Service) ([]ipvs.DestinationExtended, error) {
	dests, err := c.Destinations(s)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return dests, err
}

// FullMask returns the netmask that makes a service match a single address.
func FullMask(addr netip.Addr) netmask.Mask {
	if addr.Is4() {
		return netmask.MaskFrom(32, 32)
	}
	return netmask.MaskFrom(128, 128)
}
