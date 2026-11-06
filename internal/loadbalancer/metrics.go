package loadbalancer

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Metrics are served in the Prometheus text format, without any dependency.
// The counters come from the IPVS statistics: per virtual service, and per
// destination with its weight, health and connections.

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

type metricWriter struct {
	w     io.Writer
	typed map[string]bool
}

func (m *metricWriter) sample(name, kind, help string, value any, labels ...string) {
	if !m.typed[name] {
		m.typed[name] = true
		fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	parts := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, labels[i], escapeLabel(labels[i+1])))
	}
	if len(parts) > 0 {
		name += "{" + strings.Join(parts, ",") + "}"
	}
	fmt.Fprintf(m.w, "%s %v\n", name, value)
}

// WriteMetrics renders all the metrics.
func (l *loadBalancerServer) WriteMetrics(w io.Writer) {
	m := &metricWriter{w: w, typed: map[string]bool{}}
	l.mu.Lock()
	defer l.mu.Unlock()

	all := []*LoadBalancerInformation{}
	l.forEachLB(func(lb *LoadBalancerInformation) { all = append(all, lb) })
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	m.sample("lbmanager_loadbalancers", "gauge", "Number of defined load balancers.", len(all))
	for _, lb := range all {
		ports := make([]int, 0, len(lb.Targets))
		for p := range lb.Targets {
			ports = append(ports, int(p))
		}
		sort.Ints(ports)
		for _, p := range ports {
			protocols := map[Protocol]bool{}
			for _, t := range lb.Targets[int32(p)].GetTarget() {
				protocols[t.Protocol] = true
			}
			for proto := range protocols {
				l.serviceMetrics(m, lb, int32(p), proto)
			}
		}
	}
}

func (l *loadBalancerServer) serviceMetrics(m *metricWriter, lb *LoadBalancerInformation, port int32, proto Protocol) {
	srv := toIPVSService(lb, port, proto)
	base := []string{"loadbalancer", lb.Name, "vip", lb.IpAddr, "port", fmt.Sprint(port), "protocol", proto.String()}
	if cur, err := l.ipvsHandle.GetService(srv); err == nil {
		st := cur.Stats
		m.sample("lbmanager_service_connections_total", "counter", "Connections accepted by the virtual service.", st.Connections, base...)
		m.sample("lbmanager_service_packets_in_total", "counter", "Packets received by the virtual service.", st.PacketsIn, base...)
		m.sample("lbmanager_service_packets_out_total", "counter", "Packets sent by the virtual service.", st.PacketsOut, base...)
		m.sample("lbmanager_service_bytes_in_total", "counter", "Bytes received by the virtual service.", st.BytesIn, base...)
		m.sample("lbmanager_service_bytes_out_total", "counter", "Bytes sent by the virtual service.", st.BytesOut, base...)
	}
	dests, err := l.ipvsHandle.GetDestinations(srv)
	if err != nil {
		return
	}
	for _, d := range dests {
		labels := append(append([]string{}, base...), "target", net.JoinHostPort(d.Address.String(), fmt.Sprint(d.Port)))
		m.sample("lbmanager_destination_weight", "gauge", "IPVS weight of the destination; 0 when it failed its health check.", d.Weight, labels...)
		m.sample("lbmanager_destination_active_connections", "gauge", "Active connections to the destination.", d.ActiveConnections, labels...)
		m.sample("lbmanager_destination_inactive_connections", "gauge", "Inactive connections to the destination.", d.InactiveConnections, labels...)
		m.sample("lbmanager_destination_connections_total", "counter", "Connections sent to the destination.", d.Stats.Connections, labels...)
		m.sample("lbmanager_destination_bytes_in_total", "counter", "Bytes sent to the destination.", d.Stats.BytesIn, labels...)
		m.sample("lbmanager_destination_bytes_out_total", "counter", "Bytes received from the destination.", d.Stats.BytesOut, labels...)
	}
}

// ServeMetrics serves /metrics on addr until ctx is done. An empty addr disables it.
func (l *loadBalancerServer) ServeMetrics(ctx context.Context, addr string) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		l.WriteMetrics(w)
	})
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics: %v", err)
		}
	}()
	log.Printf("Serving metrics on %s", lis.Addr())
	return nil
}
