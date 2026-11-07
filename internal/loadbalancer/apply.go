package loadbalancer

import (
	"context"
	"fmt"
	"log"
	"net"
)

type targetID struct {
	protocol Protocol
	ip       string
	port     int32
}

func idOf(t *Target) targetID {
	return targetID{protocol: t.Protocol, ip: t.DstIP, port: t.DstPort}
}

// Apply makes the load balancer match the request, creating it when needed.
// Repeating a request is harmless. A problem with one port does not stop the
// other ports from being applied; it is reported in ApplyResult.PortErrors.
func (l *loadBalancerServer) Apply(ctx context.Context, req *ApplyRequest) (*ApplyResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if req.Name == "" {
		return nil, fmt.Errorf("a load balancer name is required")
	}
	lb, err := l.getLB(req.Name)
	if err == errNoSuchLoadbalancer {
		if lb, err = l.createLocked(req.Name, req.IpAddr); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if req.IpAddr != nil && *req.IpAddr != "" && net.ParseIP(*req.IpAddr).String() != lb.IpAddr {
		return nil, fmt.Errorf("load balancer %s has the address %s, which can't be changed to %s", req.Name, lb.IpAddr, *req.IpAddr)
	}

	result := &ApplyResult{PortErrors: map[int32]string{}}
	ports := map[int32]bool{}
	for p := range lb.Targets {
		ports[p] = true
	}
	for p := range req.Targets {
		ports[p] = true
	}
	for port := range ports {
		if msg := l.applyPort(lb, port, req.Targets[port].GetTarget()); msg != "" {
			log.Printf("apply %s: port %d: %s", lb.Name, port, msg)
			result.PortErrors[port] = msg
		}
	}
	if err := l.saveLB(lb); err != nil {
		return nil, err
	}
	l.kickHealthChecks()
	result.Info = lb
	return result, nil
}

// applyPort converges one source port of lb to the wanted targets, updating lb
// in memory. It returns a description of the problems, if any.
func (l *loadBalancerServer) applyPort(lb *LoadBalancerInformation, port int32, wanted []*Target) string {
	current := lb.Targets[port].GetTarget()
	if len(current) == 0 && len(wanted) > 0 {
		for _, t := range wanted {
			if other := l.portOwner(lb, port, t.Protocol); other != "" {
				return fmt.Sprintf("%s port %d/%s is already used by load balancer %s", lb.IpAddr, port, t.Protocol, other)
			}
		}
	}

	want := map[targetID]*Target{}
	for _, t := range wanted {
		want[idOf(t)] = t
	}
	var problems []string
	kept := []*Target{}
	protocols := map[Protocol]bool{}
	have := map[targetID]bool{}
	for _, t := range current {
		protocols[t.Protocol] = true
		id := idOf(t)
		if w, ok := want[id]; ok && !have[id] {
			have[id] = true
			if w.HealthCheckPort != t.HealthCheckPort {
				t = w
			}
			kept = append(kept, t)
			// Make sure IPVS agrees, e.g. after somebody ran ipvsadm by hand
			if res, err := l.mapTarget(lb, port, t); err != nil || res != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %s", t.DstIP, t.DstPort, errText(res, err)))
			}
			continue
		}
		// Not wanted anymore
		if err := l.unmapTarget(lb, port, t); err != nil {
			log.Printf("apply %s: removing %s:%d from port %d: %v", lb.Name, t.DstIP, t.DstPort, port, err)
			problems = append(problems, fmt.Sprintf("removing %s:%d: %v", t.DstIP, t.DstPort, err))
			kept = append(kept, t) // still programmed; try again next time
		}
	}
	for _, t := range wanted {
		id := idOf(t)
		if have[id] {
			continue
		}
		have[id] = true
		protocols[t.Protocol] = true
		if res, err := l.mapTarget(lb, port, t); err != nil || res != nil {
			problems = append(problems, fmt.Sprintf("%s:%d: %s", t.DstIP, t.DstPort, errText(res, err)))
			continue
		}
		kept = append(kept, t)
	}

	if len(kept) == 0 {
		// The service has no destinations left; remove it
		for p := range protocols {
			if err := l.ipvsHandle.RemoveService(toIPVSService(lb, port, p)); err != nil {
				log.Printf("apply %s: removing the ipvs service for port %d: %v", lb.Name, port, err)
			}
		}
		delete(lb.Targets, port)
	} else {
		lb.Targets[port] = &TargetList{Target: kept}
	}
	if len(problems) > 0 {
		return fmt.Sprintf("%v", problems)
	}
	return ""
}

func errText(res *Error, err error) string {
	if err != nil {
		return err.Error()
	}
	return res.Message
}

// unmapTarget removes the target's destination from its IPVS service.
func (l *loadBalancerServer) unmapTarget(lb *LoadBalancerInformation, port int32, t *Target) error {
	dest, err := toIPVSDestination(t)
	if err != nil {
		return err
	}
	return l.ipvsHandle.RemoveDestination(toIPVSService(lb, port, t.Protocol), dest)
}
