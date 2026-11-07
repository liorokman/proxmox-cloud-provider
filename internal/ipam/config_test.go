package ipam

import (
	"net"
	"testing"

	"github.com/knadh/koanf/v2"
)

func config(values map[string]any) *koanf.Koanf {
	k := koanf.New(".")
	for key, v := range values {
		_ = k.Set(key, v)
	}
	return k
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	good := map[string]any{
		"loadbalancer.ipam.cidr":                 "192.168.64.0/22",
		"loadbalancer.ipam.dynamicRange.startAt": "192.168.67.101",
		"loadbalancer.ipam.dynamicRange.endAt":   "192.168.67.110",
	}
	with := func(key string, v any) map[string]any {
		m := map[string]any{}
		for k, val := range good {
			m[k] = val
		}
		m[key] = v
		return m
	}
	if _, err := New(config(good), nil); err != nil {
		t.Fatalf("the good configuration is refused: %v", err)
	}
	for name, cfg := range map[string]map[string]any{
		"bad cidr":            with("loadbalancer.ipam.cidr", "not-a-cidr"),
		"bad start":           with("loadbalancer.ipam.dynamicRange.startAt", "nonsense"),
		"bad end":             with("loadbalancer.ipam.dynamicRange.endAt", "nonsense"),
		"reversed range":      with("loadbalancer.ipam.dynamicRange.startAt", "192.168.67.120"),
		"range outside cidr":  with("loadbalancer.ipam.dynamicRange.endAt", "10.0.0.1"),
		"bad static range":    with("loadbalancer.ipam.staticRange.startAt", "nonsense"),
		"static outside cidr": with("loadbalancer.ipam.staticRange.startAt", "10.0.0.1"),
	} {
		if cfg["loadbalancer.ipam.staticRange.startAt"] != nil {
			cfg["loadbalancer.ipam.staticRange.endAt"] = "192.168.67.200"
		}
		if _, err := New(config(cfg), nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRangeContains(t *testing.T) {
	r := Range{From: net.ParseIP("192.168.1.10"), To: net.ParseIP("192.168.1.20")}
	for ip, want := range map[string]bool{
		"192.168.1.10": true, "192.168.1.15": true, "192.168.1.20": true,
		"192.168.1.9": false, "192.168.1.21": false, "10.0.0.1": false, "0.0.0.0": false,
	} {
		if got := r.Contains(net.ParseIP(ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
	if (Range{}).Contains(net.ParseIP("192.168.1.15")) {
		t.Error("an empty range contains nothing")
	}
}

func TestReserveRejectsNil(t *testing.T) {
	i := newTest(t)
	if err := i.Reserve(nil); err == nil {
		t.Error("a nil address must be refused")
	}
}
