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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestVIPFileWritesOnlyChanges(t *testing.T) {
	reloads := 0
	v := &vipFile{path: filepath.Join(t.TempDir(), "vips.conf"), iface: "eth2", prefix: 24, addrs: map[string]bool{}}
	v.reload = func() error { reloads++; return nil }

	if err := v.flush(); err != nil || reloads != 1 {
		t.Fatalf("first flush: %v, %d reloads", err, reloads)
	}
	if err := v.flush(); err != nil || reloads != 1 {
		t.Fatalf("an unchanged file must not reload: %v, %d reloads", err, reloads)
	}
	v.addrs["192.168.1.41"] = true
	v.addrs["192.168.1.40"] = true
	if err := v.flush(); err != nil || reloads != 2 {
		t.Fatalf("flush: %v, %d reloads", err, reloads)
	}
	data, _ := os.ReadFile(v.path)
	if want := "192.168.1.40/24 dev eth2\n192.168.1.41/24 dev eth2\n"; !strings.HasSuffix(string(data), want) {
		t.Errorf("unexpected file:\n%s", data)
	}
	delete(v.addrs, "192.168.1.40")
	_ = v.flush()
	data, _ = os.ReadFile(v.path)
	if strings.Contains(string(data), ".40/") || reloads != 3 {
		t.Errorf("address not removed (%d reloads):\n%s", reloads, data)
	}
}

// TestHandoffToKeepalived needs root and the ip_vs modules.
func TestHandoffToKeepalived(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vips.conf")
	k := testConfig(t, "  keepalived:", "    vipsFile: "+path, "    pidFile: /nonexistent", "    reserved: [192.168.87.205]")
	srv, err := NewServer(k)
	if err != nil {
		t.Skipf("cannot create the load balancer server (needs root and ip_vs): %v", err)
	}
	defer srv.Close()
	if err := srv.Restore(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := srv.Create(ctx, &CreateLoadBalancer{Name: "r", IpAddr: asPtr("192.168.87.205")}); err == nil {
		t.Error("the container's own address must be refused")
	}
	lb, err := srv.Create(ctx, &CreateLoadBalancer{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), lb.IpAddr+"/24 dev dummy0") {
		t.Errorf("address missing from the file:\n%s", data)
	}
	// keepalived owns it: it must not be on the interface
	impl := srv.(*loadBalancerServer)
	addrs, _ := impl.nlHandle.AddrList(impl.externalLink, netlink.FAMILY_ALL)
	for _, a := range addrs {
		if a.IP.String() == lb.IpAddr {
			t.Errorf("%s was configured on the interface", lb.IpAddr)
		}
	}
	if _, err := srv.Delete(ctx, &LoadBalancerName{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), lb.IpAddr) {
		t.Errorf("address still in the file:\n%s", data)
	}
}
