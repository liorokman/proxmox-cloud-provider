package loadbalancer

import (
	"errors"
	"testing"
	"time"

	"github.com/cloudflare/ipvs"
	"github.com/knadh/koanf/v2"
)

func TestToIPVSService(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ip       string
		proto    Protocol
		wantAF   ipvs.AddressFamily
		wantProt ipvs.Protocol
		wantBits int
		wantAddr string
	}{
		{"ipv4 tcp", "192.168.1.10", Protocol_TCP, ipvs.INET, ipvs.TCP, 32, "192.168.1.10"},
		{"ipv4 udp", "192.168.1.10", Protocol_UDP, ipvs.INET, ipvs.UDP, 32, "192.168.1.10"},
		{"ipv6 tcp", "2001:db8::10", Protocol_TCP, ipvs.INET6, ipvs.TCP, 128, "2001:db8::10"},
		{"ipv4 mapped in ipv6", "::ffff:192.168.1.10", Protocol_TCP, ipvs.INET, ipvs.TCP, 32, "192.168.1.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := toIPVSService(&LoadBalancerInformation{IpAddr: tc.ip}, 8443, tc.proto)
			if s.Address.String() != tc.wantAddr || s.Family != tc.wantAF || s.Protocol != tc.wantProt {
				t.Errorf("got %+v", s)
			}
			if s.Port != 8443 || s.Scheduler != "rr" {
				t.Errorf("got port %d, scheduler %q", s.Port, s.Scheduler)
			}
			if s.Netmask.Bits() != tc.wantBits {
				t.Errorf("netmask has %d bits, want %d", s.Netmask.Bits(), tc.wantBits)
			}
		})
	}
}

func TestToIPVSDestination(t *testing.T) {
	d, err := toIPVSDestination(&Target{DstIP: "10.1.2.3", DstPort: 30080})
	if err != nil {
		t.Fatal(err)
	}
	if d.Address.String() != "10.1.2.3" || d.Port != 30080 || d.Family != ipvs.INET || d.FwdMethod != ipvs.Masquerade || d.Weight != 1 {
		t.Errorf("got %+v", d)
	}
	d, err = toIPVSDestination(&Target{DstIP: "fd00::5", DstPort: 443})
	if err != nil || d.Family != ipvs.INET6 || d.Address.String() != "fd00::5" {
		t.Errorf("ipv6: %+v %v", d, err)
	}
	for _, bad := range []string{"", "not-an-ip", "10.1.2", "10.1.2.3/24"} {
		if _, err := toIPVSDestination(&Target{DstIP: bad}); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestErrText(t *testing.T) {
	if got := errText(&Error{Message: "from the result"}, nil); got != "from the result" {
		t.Errorf("got %q", got)
	}
	if got := errText(&Error{Message: "ignored"}, errors.New("boom")); got != "boom" {
		t.Errorf("an error wins over the result: got %q", got)
	}
}

func TestReadHealthConfig(t *testing.T) {
	cfg := readHealthConfig(koanf.New("."))
	if !cfg.enabled || cfg.interval != defaultHealthInterval || cfg.timeout != defaultHealthTimeout ||
		cfg.fall != defaultHealthFall || cfg.rise != defaultHealthRise {
		t.Errorf("defaults: %+v", cfg)
	}

	k := koanf.New(".")
	for key, v := range map[string]any{
		"loadbalancer.healthCheck.enabled":  false,
		"loadbalancer.healthCheck.interval": "250ms",
		"loadbalancer.healthCheck.timeout":  "2s",
		"loadbalancer.healthCheck.fall":     5,
		"loadbalancer.healthCheck.rise":     4,
	} {
		if err := k.Set(key, v); err != nil {
			t.Fatal(err)
		}
	}
	cfg = readHealthConfig(k)
	if cfg.enabled || cfg.interval != 250*time.Millisecond || cfg.timeout != 2*time.Second || cfg.fall != 5 || cfg.rise != 4 {
		t.Errorf("overrides: %+v", cfg)
	}

	// Values that make no sense fall back to the defaults
	k = koanf.New(".")
	_ = k.Set("loadbalancer.healthCheck.interval", "-1s")
	_ = k.Set("loadbalancer.healthCheck.fall", 0)
	cfg = readHealthConfig(k)
	if cfg.interval != defaultHealthInterval || cfg.fall != defaultHealthFall {
		t.Errorf("invalid values: %+v", cfg)
	}
}
