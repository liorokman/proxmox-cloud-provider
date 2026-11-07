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

package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/liorokman/proxmox-cloud-provider/internal/nstest"
	"github.com/vishvananda/netns"
	"google.golang.org/grpc"
)

// fakeStream collects what GetLoadBalancers sends.
type fakeStream struct {
	grpc.ServerStream
	sent   []*LoadBalancerInformation
	failAt int // fail the nth Send (1-based); 0 means never
}

func (f *fakeStream) Send(m *LoadBalancerInformation) error {
	if f.failAt > 0 && len(f.sent)+1 == f.failAt {
		return errors.New("stream closed")
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeStream) Context() context.Context { return context.Background() }

func TestGetLoadBalancersStreamsAll(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	for _, n := range []string{"one", "two", "three"} {
		if _, err := s.Create(ctx, &CreateLoadBalancer{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	stream := &fakeStream{}
	if err := s.GetLoadBalancers(nil, stream); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, lb := range stream.sent {
		names[lb.Name] = true
	}
	if len(stream.sent) != 3 || !names["one"] || !names["two"] || !names["three"] {
		t.Errorf("got %v", names)
	}

	// A broken stream stops the listing and reports why
	if err := s.GetLoadBalancers(nil, &fakeStream{failAt: 2}); err == nil {
		t.Error("expected the stream error")
	}
}

func TestGetUnknownLoadBalancer(t *testing.T) {
	s := newTestServer(t)
	lb, err := s.GetLoadBalancer(context.Background(), &LoadBalancerName{Name: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if lb == nil || lb.Name != "" || len(lb.Targets) != 0 {
		t.Errorf("an unknown load balancer should come back empty, got %+v", lb)
	}
}

func TestErrorPaths(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	tgt := &Target{Protocol: Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30080}

	// Everything on a load balancer that doesn't exist
	if res, err := s.Delete(ctx, &LoadBalancerName{Name: "nope"}); err != nil || res.Code != ErrNoSuchLB {
		t.Errorf("Delete: %v %+v", err, res)
	}
	if res, err := s.AddTarget(ctx, &AddTargetRequest{LbName: "nope", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrNoSuchLB {
		t.Errorf("AddTarget: %v %+v", err, res)
	}
	if res, err := s.DelTarget(ctx, &DelTargetRequest{LbName: "nope", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrNoSuchLB {
		t.Errorf("DelTarget: %v %+v", err, res)
	}

	if _, err := s.Create(ctx, &CreateLoadBalancer{Name: "lb"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, &CreateLoadBalancer{Name: "lb"}); err == nil {
		t.Error("creating a load balancer twice should fail")
	}
	if _, err := s.Create(ctx, &CreateLoadBalancer{Name: "bad", IpAddr: asPtr("not-an-address")}); err == nil {
		t.Error("an invalid address should be refused")
	}

	// Removing a target that was never added
	if res, err := s.DelTarget(ctx, &DelTargetRequest{LbName: "lb", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrTargetNotMapped {
		t.Errorf("DelTarget without any port: %v %+v", err, res)
	}
	if res, err := s.AddTarget(ctx, &AddTargetRequest{LbName: "lb", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrSuccess {
		t.Fatalf("AddTarget: %v %+v", err, res)
	}
	other := &Target{Protocol: Protocol_TCP, DstIP: "192.168.100.9", DstPort: 30080}
	if res, err := s.DelTarget(ctx, &DelTargetRequest{LbName: "lb", SrcPort: 80, Target: other}); err != nil || res.Code != ErrTargetNotMapped {
		t.Errorf("DelTarget of another target: %v %+v", err, res)
	}
	if res, err := s.DelTarget(ctx, &DelTargetRequest{LbName: "lb", SrcPort: 8080, Target: tgt}); err != nil || res.Code != ErrTargetNotMapped {
		t.Errorf("DelTarget on another port: %v %+v", err, res)
	}

	// A target with an unusable address is refused
	badTgt := &Target{Protocol: Protocol_TCP, DstIP: "not-an-ip", DstPort: 1}
	if res, err := s.AddTarget(ctx, &AddTargetRequest{LbName: "lb", SrcPort: 81, Target: badTgt}); err == nil && (res == nil || res.Code == ErrSuccess) {
		t.Errorf("a bad target address must not succeed: %+v", res)
	}

	// Removing the last target removes the IPVS service, and the load balancer can be deleted
	if res, err := s.DelTarget(ctx, &DelTargetRequest{LbName: "lb", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrSuccess {
		t.Errorf("DelTarget: %v %+v", err, res)
	}
	if svcs, _ := ipvsnServices(s); len(svcs) != 0 {
		t.Errorf("expected no IPVS services, got %d", len(svcs))
	}
	if res, err := s.Delete(ctx, &LoadBalancerName{Name: "lb"}); err != nil || res.Code != ErrSuccess {
		t.Errorf("Delete: %v %+v", err, res)
	}
}

func ipvsnServices(s Intf) (n []struct{}, err error) {
	svcs, err := s.(*loadBalancerServer).ipvsHandle.Services()
	for range svcs {
		n = append(n, struct{}{})
	}
	return n, err
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestServeMetricsServesOverHTTP(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.ServeMetrics(ctx, ""); err != nil {
		t.Errorf("an empty address disables the endpoint: %v", err)
	}
	if err := s.ServeMetrics(ctx, "256.0.0.1:1"); err == nil {
		t.Error("an invalid address should fail")
	}

	if _, err := s.Create(ctx, &CreateLoadBalancer{Name: "m", IpAddr: asPtr("192.168.87.211")}); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	if err := s.ServeMetrics(ctx, addr); err != nil {
		t.Fatal(err)
	}
	var body string
	for i := 0; i < 50; i++ {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
				t.Errorf("content type %q", resp.Header.Get("Content-Type"))
			}
			body = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, "lbmanager_loadbalancers 1") {
		t.Errorf("unexpected metrics:\n%s", body)
	}

	// Cancelling the context stops the server
	cancel()
	for i := 0; i < 50; i++ {
		if _, err := http.Get("http://" + addr + "/metrics"); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the metrics server kept running after the context was cancelled")
}

// listenIn opens a listener inside the network namespace.
func listenIn(t *testing.T, nsName, addr string) net.Listener {
	t.Helper()
	ns, err := netns.GetFromName(nsName)
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
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func acceptForever(l net.Listener) {
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
}

// The real prober connects from inside the namespace.
func TestProbeFromInsideTheNamespace(t *testing.T) {
	k := testConfig(t)
	ns := k.String("loadbalancer.namespace")
	l := listenIn(t, ns, "127.0.0.1:0")
	acceptForever(l)
	port := l.Addr().(*net.TCPAddr).Port

	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	p := newTCPProber(h, time.Second)
	if !p.probe(&Target{DstIP: "127.0.0.1", DstPort: int32(port)}) {
		t.Error("an open port inside the namespace must be healthy")
	}
	// The same address in the caller's namespace is not the one probed
	l.Close()
	if p.probe(&Target{DstIP: "127.0.0.1", DstPort: int32(port)}) {
		t.Error("a closed port must be unhealthy")
	}
}

// The health check loop follows a target going down and coming back.
func TestHealthCheckLoopFollowsTheTarget(t *testing.T) {
	k := testConfig(t,
		"  healthCheck:",
		"    interval: 50ms",
		"    timeout: 200ms",
		"    fall: 2",
		"    rise: 2")
	ns := k.String("loadbalancer.namespace")
	nstest.AddAddr(t, ns, "10.9.9.1/24")
	l := listenIn(t, ns, "10.9.9.1:0")
	acceptForever(l)
	port := l.Addr().(*net.TCPAddr).Port

	s := newTestServerWith(k, t)
	srv := s.(*loadBalancerServer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Create(ctx, &CreateLoadBalancer{Name: "hc", IpAddr: asPtr("192.168.87.212")}); err != nil {
		t.Fatal(err)
	}
	tgt := &Target{Protocol: Protocol_TCP, DstIP: "10.9.9.1", DstPort: int32(port)}
	if res, err := s.AddTarget(ctx, &AddTargetRequest{LbName: "hc", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrSuccess {
		t.Fatalf("%v %+v", err, res)
	}
	s.StartHealthChecks(ctx)

	weightIs := func(want int) bool {
		w := destWeights(t, srv, "hc", 80)
		return w[fmt.Sprintf("10.9.9.1:%d", port)] == want
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the live target to have weight 1", func() bool { return weightIs(1) })

	l.Close()
	waitFor("the dead target to have weight 0", func() bool { return weightIs(0) })

	l2 := listenIn(t, ns, fmt.Sprintf("10.9.9.1:%d", port))
	acceptForever(l2)
	waitFor("the recovered target to have weight 1", func() bool { return weightIs(1) })
}

func TestHealthChecksCanBeDisabled(t *testing.T) {
	s := newTestServerWith(testConfig(t, "  healthCheck:", "    enabled: false"), t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartHealthChecks(ctx)
	if s.(*loadBalancerServer).probe != nil {
		t.Error("no prober should be started when health checks are disabled")
	}
}

// NewServer must refuse unusable configurations without leaking anything.
func TestNewServerRejectsBadConfiguration(t *testing.T) {
	good := testConfig(t)
	if s, err := NewServer(good); err != nil {
		t.Skipf("cannot create a server here (needs root and ip_vs): %v", err)
	} else {
		s.Close()
	}

	broken := func(key string, value any) *koanf.Koanf {
		k := testConfig(t)
		if err := k.Set(key, value); err != nil {
			t.Fatal(err)
		}
		return k
	}
	for name, k := range map[string]*koanf.Koanf{
		"bad cidr":           broken("loadbalancer.ipam.cidr", "nonsense"),
		"missing namespace":  broken("loadbalancer.namespace", "no-such-namespace"),
		"missing interface":  broken("loadbalancer.externalInterface", "no-such-interface"),
		"unusable database":  broken("dbDir", "/proc/lbmanager-db"),
		"range outside cidr": broken("loadbalancer.ipam.dynamicRange.endAt", "10.0.0.1"),
	} {
		if s, err := NewServer(k); err == nil {
			s.Close()
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A new Service is probed before kube-proxy has opened its NodePort. The target
// then starts out down, and must come up soon after the port opens even though
// the normal interval is long.
func TestNewTargetComesUpQuicklyAfterItsPortOpens(t *testing.T) {
	k := testConfig(t,
		"  healthCheck:",
		"    interval: 30s",
		"    downInterval: 50ms",
		"    timeout: 200ms",
		"    fall: 2",
		"    rise: 2")
	ns := k.String("loadbalancer.namespace")
	nstest.AddAddr(t, ns, "10.9.9.1/24")

	// A port that nobody listens on yet
	l := listenIn(t, ns, "10.9.9.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	s := newTestServerWith(k, t)
	srv := s.(*loadBalancerServer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartHealthChecks(ctx)

	res, err := s.Apply(ctx, &ApplyRequest{
		Name:   "quick",
		IpAddr: asPtr("192.168.87.213"),
		Targets: map[int32]*TargetList{
			80: {Target: []*Target{{Protocol: Protocol_TCP, DstIP: "10.9.9.1", DstPort: int32(port)}}},
		},
	})
	if err != nil || len(res.PortErrors) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	key := fmt.Sprintf("10.9.9.1:%d", port)
	waitFor := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the closed port to be taken out", 3*time.Second, func() bool { return destWeights(t, srv, "quick", 80)[key] == 0 })

	start := time.Now()
	l2 := listenIn(t, ns, fmt.Sprintf("10.9.9.1:%d", port))
	acceptForever(l2)
	waitFor("the target to come up", 5*time.Second, func() bool { return destWeights(t, srv, "quick", 80)[key] == 1 })
	t.Logf("the target came up %v after its port opened", time.Since(start).Round(10*time.Millisecond))
}
