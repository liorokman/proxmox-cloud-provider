package loadbalancer

import (
	"context"
	"testing"
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
	l.probe = func(addr string) bool { return alive[addr] }

	weights := func() map[string]int {
		lb, _ := l.getLB("h")
		dests, err := l.ipvsHandle.GetDestinations(toIPVSService(lb, 80, Protocol_TCP))
		if err != nil {
			t.Fatal(err)
		}
		w := map[string]int{}
		for _, d := range dests {
			w[d.Address.String()] = d.Weight
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
