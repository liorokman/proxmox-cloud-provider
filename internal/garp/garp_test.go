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

package garp

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/liorokman/proxmox-cloud-provider/internal/nstest"
	"github.com/vishvananda/netns"
)

func TestFrame(t *testing.T) {
	mac := net.HardwareAddr{0xbc, 0x24, 0x11, 0x01, 0x02, 0x03}
	got, err := Frame(mac, netip.MustParseAddr("192.168.67.101"))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // to everybody
		0xbc, 0x24, 0x11, 0x01, 0x02, 0x03, // from the interface
		0x08, 0x06, // ARP
		0x00, 0x01, 0x08, 0x00, 6, 4, 0x00, 0x01, // Ethernet, IPv4, request
		0xbc, 0x24, 0x11, 0x01, 0x02, 0x03, 192, 168, 67, 101, // sender: the address and its owner
		0, 0, 0, 0, 0, 0, 192, 168, 67, 101, // target: the same address
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got  % x\nwant % x", got, want)
	}
	// An IPv4 address in IPv6 notation is the same address
	if mapped, err := Frame(mac, netip.MustParseAddr("::ffff:192.168.67.101")); err != nil || !bytes.Equal(mapped, want) {
		t.Errorf("mapped address: %v", err)
	}
}

func TestFrameRejectsWhatItCannotAnnounce(t *testing.T) {
	mac := net.HardwareAddr{0xbc, 0x24, 0x11, 0x01, 0x02, 0x03}
	if _, err := Frame(mac, netip.MustParseAddr("2001:db8::1")); err == nil {
		t.Error("IPv6 addresses use neighbour discovery, not ARP")
	}
	if _, err := Frame(net.HardwareAddr{1, 2, 3}, netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Error("a short hardware address must be refused")
	}
	if _, err := Frame(nil, netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Error("a missing hardware address must be refused")
	}
}

// Send puts the announcement on the interface of the namespace, three times.
func TestSendAnnouncesOnTheInterface(t *testing.T) {
	name := nstest.New(t)
	capture := nstest.CaptureARP(t, name)
	ns, err := netns.GetFromName(name)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()

	ip := netip.MustParseAddr("192.168.67.150")
	if err := Send(ns, "dummy0", ip); err != nil {
		t.Fatal(err)
	}
	want, err := Frame(nstest.MAC(t, name, "dummy0"), ip)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		got := capture.Next(3 * time.Second)
		if got == nil {
			t.Fatalf("announcement %d never arrived", i)
		}
		if !bytes.Equal(got[:len(want)], want) {
			t.Errorf("announcement %d:\n got  % x\n want % x", i, got, want)
		}
	}
}

func TestSendReportsProblems(t *testing.T) {
	name := nstest.New(t)
	ns, err := netns.GetFromName(name)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	if err := Send(ns, "no-such-interface", netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Error("a missing interface must be an error")
	}
	if err := Send(ns, "dummy0", netip.MustParseAddr("2001:db8::1")); err == nil {
		t.Error("an IPv6 address must be an error")
	}
	// The thread that sent is back in its own namespace: it can still see its original interfaces
	if _, err := net.InterfaceByName("lo"); err != nil {
		t.Errorf("the caller's namespace changed: %v", err)
	}
	if _, err := net.InterfaceByName("dummy0"); err == nil {
		t.Error("the caller can see the namespace's interface: it was left in the wrong namespace")
	}
}
