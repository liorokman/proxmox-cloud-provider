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

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/liorokman/proxmox-cloud-provider/internal/certtest"
	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
	"github.com/liorokman/proxmox-cloud-provider/internal/nstest"
)

func TestNewGRPCCreds(t *testing.T) {
	dir := t.TempDir()
	f := certtest.Write(t, dir)
	if c, err := newGRPCCreds(f.Cert, f.Key, f.CA); err != nil || c == nil {
		t.Fatalf("valid files: %v %v", c, err)
	}
	if _, err := newGRPCCreds(filepath.Join(dir, "none"), f.Key, f.CA); err == nil {
		t.Error("a missing certificate must be an error")
	}
	if _, err := newGRPCCreds(f.Cert, f.Key, filepath.Join(dir, "none")); err == nil {
		t.Error("a missing CA must be an error")
	}
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := newGRPCCreds(f.Cert, f.Key, junk); err == nil || c != nil {
		t.Errorf("an unusable CA must be an error: %v %v", c, err)
	}
}

func TestLoadConfigFile(t *testing.T) {
	k = koanf.New(".")
	dir := t.TempDir()
	path := filepath.Join(dir, "lbmanager.yaml")
	if err := os.WriteFile(path, []byte("grpc:\n  port: 9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if k.Int("grpc.port") != 9999 {
		t.Errorf("port %d", k.Int("grpc.port"))
	}
	if err := loadConfigFile(filepath.Join(dir, "missing.yaml")); !os.IsNotExist(err) {
		t.Errorf("a missing file must be reported as such: %v", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("a: [unclosed"), 0o600)
	if err := loadConfigFile(bad); err == nil {
		t.Error("invalid YAML must be an error")
	}

	// A change to the file is merged into the configuration
	if err := os.WriteFile(path, []byte("grpc:\n  port: 7777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for k.Int("grpc.port") != 7777 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if k.Int("grpc.port") != 7777 {
		t.Error("the changed file was not picked up")
	}
}

func TestLoadConfig(t *testing.T) {
	if _, err := os.Stat("/etc/lbmanager/lbmanager.yaml"); err == nil {
		t.Skip("this machine has a system-wide lbmanager.yaml")
	}
	dir := t.TempDir()
	t.Chdir(dir)

	k = koanf.New(".")
	if err := loadConfig(); err == nil {
		t.Error("without any configuration file, loading must fail")
	}

	if err := os.WriteFile("lbmanager.yaml", []byte("dbDir: /var/lib/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k = koanf.New(".")
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if k.String("dbDir") != "/var/lib/x" {
		t.Errorf("dbDir %q", k.String("dbDir"))
	}

	if err := os.WriteFile("lbmanager.yaml", []byte("a: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	k = koanf.New(".")
	if err := loadConfig(); err == nil {
		t.Error("an invalid file must be an error")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeConfig(t *testing.T, ns string, f certtest.Files, grpcPort, metricsPort int) *koanf.Koanf {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`
grpc:
  listen: 127.0.0.1
  port: %d
  auth:
    cert: %s
    key: %s
    ca: %s
dbDir: %s
loadbalancer:
  namespace: %s
  externalInterface: dummy0
  ipam:
    cidr: 192.168.87.0/24
    dynamicRange:
      startAt: 192.168.87.200
      endAt: 192.168.87.225
metrics:
  listen: 127.0.0.1:%d
`, grpcPort, f.Cert, f.Key, f.CA, filepath.Join(dir, "db"), ns, metricsPort)
	path := filepath.Join(dir, "lbmanager.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c := koanf.New(".")
	if err := c.Load(file.Provider(path), yaml.Parser()); err != nil {
		t.Fatal(err)
	}
	return c
}

// run needs root and the ip_vs module; it uses a private network namespace.
func TestRunServesRequests(t *testing.T) {
	ns := nstest.New(t)
	f := certtest.Write(t, t.TempDir())
	grpcPort, metricsPort := freePort(t), freePort(t)
	config := writeConfig(t, ns, f, grpcPort, metricsPort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, config) }()

	// An mTLS client
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := os.ReadFile(f.CA)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool})
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", grpcPort), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := loadbalancer.NewLoadBalancerClient(conn)

	var info *loadbalancer.LoadBalancerInformation
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Skipf("the server could not start (needs root and ip_vs): %v", err)
		default:
		}
		rctx, rcancel := context.WithTimeout(ctx, time.Second)
		info, err = client.Create(rctx, &loadbalancer.CreateLoadBalancer{Name: "e2e"})
		rcancel()
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Name != "e2e" || info.IpAddr == "" {
		t.Errorf("unexpected info %+v", info)
	}
	stream, err := client.GetLoadBalancers(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := stream.Recv()
	if err != nil || lb.Name != "e2e" {
		t.Errorf("list: %+v %v", lb, err)
	}

	// A client without a certificate is refused
	plain := credentials.NewTLS(&tls.Config{RootCAs: pool})
	bad, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", grpcPort), grpc.WithTransportCredentials(plain))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	if _, err := loadbalancer.NewLoadBalancerClient(bad).GetLoadBalancers(rctx, &emptypb.Empty{}); err == nil {
		// The refusal can show up on the first receive
		s, _ := loadbalancer.NewLoadBalancerClient(bad).GetLoadBalancers(rctx, &emptypb.Empty{})
		if _, rerr := s.Recv(); rerr == nil {
			t.Error("a client without a certificate must be refused")
		}
	}

	// Stopping the context shuts the server down cleanly
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop")
	}
}

func TestRunFailures(t *testing.T) {
	ctx := context.Background()
	// No credentials configured
	if err := run(ctx, koanf.New(".")); err == nil {
		t.Error("missing credentials must be an error")
	}
	f := certtest.Write(t, t.TempDir())
	// A port that is already taken
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := writeConfig(t, "does-not-exist", f, l.Addr().(*net.TCPAddr).Port, 0)
	if err := run(ctx, cfg); err == nil {
		t.Error("a port in use must be an error")
	}
	// A namespace that does not exist
	cfg = writeConfig(t, "does-not-exist", f, freePort(t), 0)
	if err := run(ctx, cfg); err == nil {
		t.Error("a missing namespace must be an error")
	}
}
