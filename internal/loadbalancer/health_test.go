package loadbalancer

import (
	"context"
	"fmt"
	"github.com/liorokman/proxmox-cloud-provider/internal/ipvsns"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHealthTrackerHysteresis(t *testing.T) {
	h := newHealthTracker(3, 2)
	const k = "t"
	if !h.isHealthy(k) {
		t.Error("unknown targets are considered healthy")
	}
	if !h.observe(k, true) {
		t.Fatal("first success must be healthy")
	}
	// Two failures are tolerated, the third takes it out
	for i, want := range []bool{true, true, false, false} {
		if got := h.observe(k, false); got != want {
			t.Errorf("failure %d: healthy=%v, want %v", i+1, got, want)
		}
	}
	// One success is not enough to bring it back
	if h.observe(k, true) {
		t.Error("one success must not be enough")
	}
	if !h.observe(k, true) {
		t.Error("two successes must bring it back")
	}
	// A flapping probe never reaches the threshold
	for i := 0; i < 10; i++ {
		h.observe(k, false)
		h.observe(k, false)
		if !h.observe(k, true) {
			t.Fatal("flapping target must stay up")
		}
	}
}

func TestHealthTrackerFirstFailureIsDown(t *testing.T) {
	h := newHealthTracker(3, 2)
	if h.observe("new", false) {
		t.Error("a target that fails its first probe must start down")
	}
}

func TestHealthTrackerForget(t *testing.T) {
	h := newHealthTracker(1, 1)
	h.observe("a", false)
	h.observe("b", false)
	h.forget(map[string]bool{"a": true})
	if h.isHealthy("a") || !h.isHealthy("b") {
		t.Error("only the kept key should keep its state")
	}
}

// Needs the same environment as TestLoadBalancerLifecycle.
func TestHealthChecksSetWeights(t *testing.T) {
	s := newTestServer(t)
	l := s.(*loadBalancerServer)
	ctx := context.Background()
	if _, err := s.Create(ctx, &CreateLoadBalancer{Name: "h", IpAddr: asPtr("192.168.87.234")}); err != nil {
		t.Fatal(err)
	}
	defer s.Delete(ctx, &LoadBalancerName{Name: "h"})
	up := &Target{Protocol: Protocol_TCP, DstIP: "192.168.100.5", DstPort: 30080}
	down := &Target{Protocol: Protocol_TCP, DstIP: "192.168.100.6", DstPort: 30080}
	for _, tgt := range []*Target{up, down} {
		if res, err := s.AddTarget(ctx, &AddTargetRequest{LbName: "h", SrcPort: 80, Target: tgt}); err != nil || res.Code != ErrSuccess {
			t.Fatalf("%v %+v", err, res)
		}
	}
	alive := map[string]bool{"192.168.100.5:30080": true}
	l.probe = func(t *Target) bool { return alive[fmt.Sprintf("%s:%d", t.DstIP, t.DstPort)] }

	weights := func() map[string]int {
		lb, _ := l.getLB("h")
		dests, err := ipvsns.Destinations(l.ipvsHandle, toIPVSService(lb, 80, Protocol_TCP))
		if err != nil {
			t.Fatal(err)
		}
		w := map[string]int{}
		for _, d := range dests {
			w[d.Address.String()] = int(d.Weight)
		}
		return w
	}

	l.checkHealthOnce()
	if w := weights(); w["192.168.100.5"] != 1 || w["192.168.100.6"] != 0 {
		t.Errorf("after the first round: %v", w)
	}
	// The dead target recovers (rise = 2 consecutive successes)
	alive["192.168.100.6:30080"] = true
	l.checkHealthOnce()
	if w := weights(); w["192.168.100.6"] != 0 {
		t.Errorf("one success must not be enough: %v", w)
	}
	l.checkHealthOnce()
	if w := weights(); w["192.168.100.6"] != 1 {
		t.Errorf("target should be back: %v", w)
	}
	// A target added while it is known to be down starts with weight 0
	alive["192.168.100.5:30080"] = false
	for i := 0; i < defaultHealthFall; i++ {
		l.checkHealthOnce()
	}
	if w := weights(); w["192.168.100.5"] != 0 {
		t.Errorf("target should be out: %v", w)
	}
	// Restore keeps the weights
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if w := weights(); w["192.168.100.5"] != 0 || w["192.168.100.6"] != 1 {
		t.Errorf("weights after Restore: %v", w)
	}
}

func destWeights(t *testing.T, l *loadBalancerServer, name string, port int32) map[string]int {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	lb, err := l.getLB(name)
	if err != nil {
		t.Fatal(err)
	}
	dests, err := ipvsns.Destinations(l.ipvsHandle, toIPVSService(lb, port, Protocol_TCP))
	if err != nil {
		t.Fatal(err)
	}
	w := map[string]int{}
	for _, d := range dests {
		w[fmt.Sprintf("%s:%d", d.Address, d.Port)] = int(d.Weight)
	}
	return w
}

// Targets with a health check port start with weight 0 and only get traffic
// once the check passes (externalTrafficPolicy: Local).
func TestHealthCheckPortTargets(t *testing.T) {
	s := newTestServer(t)
	l := s.(*loadBalancerServer)
	ctx := context.Background()
	checked := map[string]bool{}
	l.probe = func(tg *Target) bool {
		if tg.HealthCheckPort == 0 {
			t.Errorf("expected a health check port for %+v", tg)
		}
		return checked[tg.DstIP]
	}
	targets := map[int32]*TargetList{80: {Target: []*Target{
		{DstIP: "192.168.100.5", DstPort: 31000, HealthCheckPort: 32000},
		{DstIP: "192.168.100.6", DstPort: 31000, HealthCheckPort: 32000},
	}}}
	if _, err := s.Apply(ctx, &ApplyRequest{Name: "local", IpAddr: asPtr("192.168.87.235"), Targets: targets}); err != nil {
		t.Fatal(err)
	}
	defer s.Delete(ctx, &LoadBalancerName{Name: "local"})
	if w := destWeights(t, l, "local", 80); w["192.168.100.5:31000"] != 0 || w["192.168.100.6:31000"] != 0 {
		t.Errorf("targets must wait for their first check: %v", w)
	}
	checked["192.168.100.5"] = true
	l.checkHealthOnce()
	if w := destWeights(t, l, "local", 80); w["192.168.100.5:31000"] != 1 || w["192.168.100.6:31000"] != 0 {
		t.Errorf("only the node with endpoints should get traffic: %v", w)
	}
}

func TestProberHTTPCheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	p := &tcpProber{timeout: time.Second}
	tg := &Target{DstIP: host, DstPort: 1, HealthCheckPort: int32(port)}
	if !p.check(tg) {
		t.Error("200 must be healthy")
	}
	status = http.StatusServiceUnavailable
	if p.check(tg) {
		t.Error("503 must be unhealthy (no local endpoints)")
	}
	// A plain TCP check uses the destination port
	if !p.check(&Target{DstIP: host, DstPort: int32(port)}) {
		t.Error("TCP check should connect")
	}
	srv.Close()
	if p.check(tg) {
		t.Error("closed port must be unhealthy")
	}
}

func TestMetrics(t *testing.T) {
	s := newTestServer(t)
	l := s.(*loadBalancerServer)
	ctx := context.Background()
	l.probe = func(*Target) bool { return true }
	if _, err := s.Apply(ctx, &ApplyRequest{Name: "m", IpAddr: asPtr("192.168.87.238"), Targets: map[int32]*TargetList{
		80: {Target: []*Target{{DstIP: "192.168.100.5", DstPort: 30080}}},
	}}); err != nil {
		t.Fatal(err)
	}
	defer s.Delete(ctx, &LoadBalancerName{Name: "m"})
	var b strings.Builder
	l.WriteMetrics(&b)
	out := b.String()
	for _, want := range []string{
		"lbmanager_loadbalancers 1",
		`lbmanager_service_connections_total{loadbalancer="m",vip="192.168.87.238",port="80",protocol="TCP"} 0`,
		`lbmanager_destination_weight{loadbalancer="m",vip="192.168.87.238",port="80",protocol="TCP",target="192.168.100.5:30080"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
