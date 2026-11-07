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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/knadh/koanf/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/liorokman/proxmox-cloud-provider/internal/certtest"
	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

func TestParseFlagsDefaults(t *testing.T) {
	o, err := parseFlags([]string{"-name", "web"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.name != "web" || o.op != "addSrv" || !o.tcp || o.dstPort != 8080 || o.srcPort != 8080 || o.addr != "" {
		t.Errorf("unexpected defaults %+v", o)
	}
}

func TestParseFlagsAll(t *testing.T) {
	o, err := parseFlags([]string{
		"-cert", "c.pem", "-key", "k.pem", "-ca", "ca.pem", "-addr", "1.2.3.4:9999",
		"-name", "n", "-op", "addTgt", "-srv", "10.0.0.1", "-tcp=false", "-dport", "30080", "-sport", "80",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := options{cert: "c.pem", key: "k.pem", ca: "ca.pem", addr: "1.2.3.4:9999", name: "n", op: "addTgt",
		service: "10.0.0.1", tcp: false, dstPort: 30080, srcPort: 80}
	if *o != want {
		t.Errorf("got %+v, want %+v", *o, want)
	}
}

func TestParseFlagsErrors(t *testing.T) {
	if _, err := parseFlags([]string{"-op", "delSrv"}, io.Discard); err == nil {
		t.Error("a name is required")
	}
	if o, err := parseFlags([]string{"-op", "list"}, io.Discard); err != nil || o.op != "list" {
		t.Errorf("list needs no name: %v", err)
	}
	if _, err := parseFlags([]string{"-nonsense"}, io.Discard); err == nil {
		t.Error("an unknown flag must be refused")
	}
	if _, err := parseFlags([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}

func TestLoadConfigFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "lbctl.yaml")
	if err := os.WriteFile(good, []byte("addr: 10.1.1.1:9999\nauth:\n  caFile: /x/ca.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := koanf.New(".")
	if err := loadConfigFile(k, good); err != nil {
		t.Fatal(err)
	}
	if k.String("addr") != "10.1.1.1:9999" || k.String("auth.caFile") != "/x/ca.pem" {
		t.Errorf("unexpected values: %v", k.All())
	}
	if err := loadConfigFile(koanf.New("."), filepath.Join(dir, "missing.yaml")); !os.IsNotExist(err) {
		t.Errorf("a missing file must be reported as such: %v", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("a: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(koanf.New("."), bad); err == nil {
		t.Error("invalid YAML must be an error")
	}
}

func TestLoadKeypair(t *testing.T) {
	f := certtest.Write(t, t.TempDir())

	// Everything from the flags
	if c, err := loadKeypair(&options{cert: f.Cert, key: f.Key, ca: f.CA}, koanf.New(".")); err != nil || c == nil {
		t.Fatalf("flags: %v %v", c, err)
	}

	// Everything from the configuration
	k := koanf.New(".")
	_ = k.Set("auth.clientCert", f.Cert)
	_ = k.Set("auth.clientKey", f.Key)
	_ = k.Set("auth.caFile", f.CA)
	if c, err := loadKeypair(&options{}, k); err != nil || c == nil {
		t.Fatalf("config: %v %v", c, err)
	}

	// The key defaults to the certificate file, which may hold both
	both := filepath.Join(t.TempDir(), "both.pem")
	cert, _ := os.ReadFile(f.Cert)
	key, _ := os.ReadFile(f.Key)
	if err := os.WriteFile(both, append(cert, key...), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := loadKeypair(&options{cert: both, ca: f.CA}, koanf.New(".")); err != nil || c == nil {
		t.Errorf("combined file: %v %v", c, err)
	}

	if _, err := loadKeypair(&options{}, koanf.New(".")); err == nil || !strings.Contains(err.Error(), "no mTLS configuration") {
		t.Errorf("nothing configured: %v", err)
	}
	if _, err := loadKeypair(&options{cert: "/nonexistent", key: f.Key, ca: f.CA}, koanf.New(".")); err == nil {
		t.Error("a missing certificate must be an error")
	}
	if _, err := loadKeypair(&options{cert: f.Cert, key: f.Key, ca: "/nonexistent"}, koanf.New(".")); err == nil {
		t.Error("a missing CA must be an error")
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
	if c, err := loadKeypair(&options{cert: f.Cert, key: f.Key, ca: junk}, koanf.New(".")); err == nil || c != nil {
		t.Errorf("an unusable CA must be an error: %v %v", c, err)
	}
}

// stubServer records the requests it gets.
type stubServer struct {
	loadbalancer.UnimplementedLoadBalancerServer
	mu    sync.Mutex
	calls []string
	fail  bool
}

func (s *stubServer) record(c string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	if s.fail {
		return errors.New("stub failure")
	}
	return nil
}

func (s *stubServer) Create(_ context.Context, in *loadbalancer.CreateLoadBalancer) (*loadbalancer.LoadBalancerInformation, error) {
	if err := s.record("create " + in.Name + " " + in.GetIpAddr()); err != nil {
		return nil, err
	}
	return &loadbalancer.LoadBalancerInformation{Name: in.Name, IpAddr: in.GetIpAddr()}, nil
}

func (s *stubServer) Delete(_ context.Context, in *loadbalancer.LoadBalancerName) (*loadbalancer.Error, error) {
	if err := s.record("delete " + in.Name); err != nil {
		return nil, err
	}
	return &loadbalancer.Error{}, nil
}

func (s *stubServer) AddTarget(_ context.Context, in *loadbalancer.AddTargetRequest) (*loadbalancer.Error, error) {
	if err := s.record("add " + in.LbName + " " + in.Target.DstIP + " " + in.Target.Protocol.String()); err != nil {
		return nil, err
	}
	return &loadbalancer.Error{Message: "already there"}, nil
}

func (s *stubServer) DelTarget(_ context.Context, in *loadbalancer.DelTargetRequest) (*loadbalancer.Error, error) {
	if err := s.record("del " + in.LbName + " " + in.Target.DstIP); err != nil {
		return nil, err
	}
	return &loadbalancer.Error{}, nil
}

func (s *stubServer) GetLoadBalancers(_ *emptypb.Empty, stream loadbalancer.LoadBalancer_GetLoadBalancersServer) error {
	if err := s.record("list"); err != nil {
		return err
	}
	for _, n := range []string{"alpha", "beta"} {
		if err := stream.Send(&loadbalancer.LoadBalancerInformation{Name: n, IpAddr: "192.0.2.1"}); err != nil {
			return err
		}
	}
	return nil
}

// startStub serves the stub over mTLS and returns its address and the client files.
func startStub(t *testing.T, stub *stubServer) (string, certtest.Files) {
	t.Helper()
	f := certtest.Write(t, t.TempDir())
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := os.ReadFile(f.CA)
	pool := x509Pool(t, ca)
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(creds))
	loadbalancer.RegisterLoadBalancerServer(srv, stub)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), f
}

// capture returns what the code under test logs.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetFlags(log.LstdFlags) })
	return &buf
}

func TestRunAgainstAServer(t *testing.T) {
	stub := &stubServer{}
	addr, f := startStub(t, stub)
	out := capture(t)
	ctx := context.Background()
	base := options{cert: f.Cert, key: f.Key, ca: f.CA, addr: addr, tcp: true, srcPort: 80, dstPort: 30080}

	for _, tc := range []struct {
		op      string
		service string
		tcp     bool
		call    string
		output  string
	}{
		{"addSrv", "192.0.2.50", true, "create web 192.0.2.50", "LBInfo:"},
		{"addTgt", "10.0.0.5", true, "add web 10.0.0.5 TCP", "Message from server: already there"},
		{"addTgt", "10.0.0.6", false, "add web 10.0.0.6 UDP", "Message from server: already there"},
		{"delTgt", "10.0.0.5", true, "del web 10.0.0.5", "Success!"},
		{"delSrv", "", true, "delete web", "Success!"},
		{"list", "", true, "list", "alpha"},
	} {
		o := base
		o.name, o.op, o.service, o.tcp = "web", tc.op, tc.service, tc.tcp
		out.Reset()
		if err := run(ctx, &o, koanf.New(".")); err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
		if !strings.Contains(out.String(), tc.output) {
			t.Errorf("%s: output %q does not contain %q", tc.op, out.String(), tc.output)
		}
		if got := stub.calls[len(stub.calls)-1]; got != tc.call {
			t.Errorf("%s: server saw %q, want %q", tc.op, got, tc.call)
		}
	}
	if !strings.Contains(out.String(), "beta") {
		t.Errorf("list must show every load balancer: %q", out.String())
	}

	// The address can come from the configuration
	k := koanf.New(".")
	_ = k.Set("addr", addr)
	o := base
	o.addr, o.name, o.op = "", "web", "delSrv"
	if err := run(ctx, &o, k); err != nil {
		t.Errorf("address from the config: %v", err)
	}
}

func TestRunReportsFailures(t *testing.T) {
	stub := &stubServer{fail: true}
	addr, f := startStub(t, stub)
	capture(t)
	base := options{cert: f.Cert, key: f.Key, ca: f.CA, addr: addr, name: "web", tcp: true}
	for _, op := range []string{"addSrv", "delSrv", "addTgt", "delTgt", "list"} {
		o := base
		o.op = op
		if err := run(context.Background(), &o, koanf.New(".")); err == nil {
			t.Errorf("%s: the server's failure must be reported", op)
		}
	}
	o := base
	o.op = "bogus"
	if err := run(context.Background(), &o, koanf.New(".")); err == nil || !strings.Contains(err.Error(), "Unknown operation") {
		t.Errorf("unknown operation: %v", err)
	}
	// No credentials at all
	if err := run(context.Background(), &options{name: "x", op: "list"}, koanf.New(".")); err == nil {
		t.Error("missing credentials must be reported")
	}
}

func x509Pool(t *testing.T, pem []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("cannot parse the CA")
	}
	return pool
}
