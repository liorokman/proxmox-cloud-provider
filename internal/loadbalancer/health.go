package loadbalancer

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netns"

	"github.com/liorokman/proxmox-cloud-provider/internal/ipvs"
)

// Health checking.
//
// Every TCP target is probed with a plain TCP connect from inside the load
// balancer's network namespace. A target that fails is not removed: its IPVS
// weight is set to 0, which stops new connections while established ones
// carry on, and it goes back to weight 1 once it answers again. UDP targets
// can't be probed this way and are always considered healthy.

const (
	defaultHealthInterval = 5 * time.Second
	defaultHealthTimeout  = time.Second
	defaultHealthFall     = 3
	defaultHealthRise     = 2
	probeWorkers          = 8
)

type healthConfig struct {
	enabled  bool
	interval time.Duration
	timeout  time.Duration
	fall     int // consecutive failures before a target is taken out
	rise     int // consecutive successes before it comes back
}

type healthState struct {
	healthy   bool
	failures  int
	successes int
}

// healthTracker applies the fall/rise hysteresis. A target that was never seen
// before and fails its first probe is marked down immediately.
type healthTracker struct {
	fall, rise int
	state      map[string]*healthState
}

func newHealthTracker(fall, rise int) *healthTracker {
	return &healthTracker{fall: fall, rise: rise, state: map[string]*healthState{}}
}

// observe records the result of a probe and returns whether the target is healthy now.
func (h *healthTracker) observe(key string, ok bool) bool {
	s, found := h.state[key]
	if !found {
		s = &healthState{healthy: ok}
		h.state[key] = s
		return s.healthy
	}
	if ok {
		s.failures = 0
		s.successes++
		if !s.healthy && s.successes >= h.rise {
			s.healthy = true
		}
	} else {
		s.successes = 0
		s.failures++
		if s.healthy && s.failures >= h.fall {
			s.healthy = false
		}
	}
	return s.healthy
}

// isHealthy returns false only for targets known to be down.
func (h *healthTracker) isHealthy(key string) bool {
	s, found := h.state[key]
	return !found || s.healthy
}

// forget drops the state of targets that are no longer in the set.
func (h *healthTracker) forget(keep map[string]bool) {
	for k := range h.state {
		if !keep[k] {
			delete(h.state, k)
		}
	}
}

func healthKey(t *Target) string {
	return fmt.Sprintf("%d/%s/%d", t.Protocol, net.JoinHostPort(t.DstIP, fmt.Sprint(t.DstPort)), t.HealthCheckPort)
}

// weightFor is the IPVS weight a target should have right now. Caller holds l.mu.
func (l *loadBalancerServer) weightFor(t *Target) int {
	if l.health == nil {
		return 1
	}
	key := healthKey(t)
	if !l.health.isHealthy(key) {
		return 0
	}
	// A target that is checked through a health check port (externalTrafficPolicy
	// Local) only gets traffic after it reported that it has endpoints
	if t.HealthCheckPort != 0 {
		if _, known := l.health.state[key]; !known {
			return 0
		}
	}
	return 1
}

// kickHealthChecks asks for a probe round right away, e.g. after targets were added.
func (l *loadBalancerServer) kickHealthChecks() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// tcpProber connects to targets from inside the namespace, using a fixed set
// of goroutines that each own an OS thread moved into the namespace.
type tcpProber struct {
	jobs    chan probeJob
	timeout time.Duration
}

type probeJob struct {
	target *Target
	result chan<- bool
}

func newTCPProber(ns netns.NsHandle, timeout time.Duration) *tcpProber {
	p := &tcpProber{jobs: make(chan probeJob), timeout: timeout}
	for i := 0; i < probeWorkers; i++ {
		go func() {
			// The thread is never unlocked: if the namespace can't be entered the
			// goroutine exits and Go discards the thread instead of reusing it.
			runtime.LockOSThread()
			if err := netns.Set(ns); err != nil {
				log.Printf("health: cannot enter the namespace: %v", err)
				return
			}
			for job := range p.jobs {
				job.result <- p.check(job.target)
			}
		}()
	}
	return p
}

// check runs on a goroutine whose thread is inside the namespace, so everything
// that needs a socket has to happen here and not in net/http.
func (p *tcpProber) check(t *Target) bool {
	port := t.DstPort
	if t.HealthCheckPort != 0 {
		port = t.HealthCheckPort
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(t.DstIP, fmt.Sprint(port)), p.timeout)
	if err != nil {
		return false
	}
	defer c.Close()
	if t.HealthCheckPort == 0 {
		return true
	}
	c.SetDeadline(time.Now().Add(p.timeout))
	if _, err := fmt.Fprintf(c, "GET /healthz HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n", t.DstIP); err != nil {
		return false
	}
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return false
	}
	fields := strings.Fields(status)
	return len(fields) >= 2 && strings.HasPrefix(fields[0], "HTTP/") && fields[1] == "200"
}

func (p *tcpProber) probe(t *Target) bool {
	res := make(chan bool, 1)
	p.jobs <- probeJob{target: t, result: res}
	return <-res
}

// runHealthChecks probes all the targets every interval until ctx is done.
func (l *loadBalancerServer) runHealthChecks(ctx context.Context) {
	ticker := time.NewTicker(l.healthCfg.interval)
	defer ticker.Stop()
	for {
		l.checkHealthOnce()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-l.kick:
		}
	}
}

// checkHealthOnce probes every TCP target once and updates the IPVS weights of
// those whose state changed.
func (l *loadBalancerServer) checkHealthOnce() {
	// Snapshot the targets, so the probes run without holding the lock
	l.mu.Lock()
	toProbe := map[string]*Target{}
	l.forEachLB(func(lb *LoadBalancerInformation) {
		for _, tl := range lb.Targets {
			for _, t := range tl.GetTarget() {
				if t.Protocol != Protocol_TCP {
					continue
				}
				toProbe[healthKey(t)] = t
			}
		}
	})
	l.mu.Unlock()

	var wg sync.WaitGroup
	results := make(map[string]bool, len(toProbe))
	var rmu sync.Mutex
	for key, pt := range toProbe {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := l.probe(pt)
			rmu.Lock()
			results[key] = ok
			rmu.Unlock()
		}()
	}
	wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	keep := map[string]bool{}
	for k := range toProbe {
		keep[k] = true
	}
	changed := map[string]bool{}
	for k, ok := range results {
		before := l.health.isHealthy(k)
		if after := l.health.observe(k, ok); after != before {
			changed[k] = true
		}
	}
	l.health.forget(keep)
	// Apply the weights. Re-applying an unchanged weight is harmless, so a failed
	// update is simply retried on the next round.
	l.forEachLB(func(lb *LoadBalancerInformation) {
		for port, tl := range lb.Targets {
			for _, t := range tl.GetTarget() {
				if t.Protocol != Protocol_TCP || !keep[healthKey(t)] {
					continue
				}
				if err := l.setWeight(lb, port, t); err != nil {
					log.Printf("health: cannot set the weight of %s:%d for %s: %v", t.DstIP, t.DstPort, lb.Name, err)
				}
			}
		}
	})
	for k := range changed {
		if l.health.isHealthy(k) {
			log.Printf("health: target %s is up", k)
		} else {
			log.Printf("health: target %s is down", k)
		}
	}
}

func (l *loadBalancerServer) setWeight(lb *LoadBalancerInformation, port int32, t *Target) error {
	dest, err := toIPVSDestination(t)
	if err != nil {
		return err
	}
	dest.Weight = l.weightFor(t)
	return l.ipvsHandle.UpdateDestination(toIPVSService(lb, port, t.Protocol), dest)
}

func toIPVSDestination(t *Target) (*ipvs.Destination, error) {
	ip := net.ParseIP(t.DstIP)
	if ip == nil {
		return nil, fmt.Errorf("invalid target address %q", t.DstIP)
	}
	var fam uint16 = syscall.AF_INET
	if ip.To4() == nil {
		fam = syscall.AF_INET6
	}
	return &ipvs.Destination{
		Address:         ip,
		Port:            uint16(t.DstPort),
		ConnectionFlags: ipvs.ConnFwdMasq,
		AddressFamily:   fam,
		Weight:          1,
	}, nil
}
