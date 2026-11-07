// Copyright 2023-2026 Lior Okman
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
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

// options are the command line settings
type options struct {
	cert, key, ca string
	addr          string
	name          string
	op            string
	service       string
	tcp           bool
	dstPort       int
	srcPort       int
}

func parseFlags(args []string, output io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("lbctl", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&o.cert, "cert", "", "filename containing the client certificate")
	fs.StringVar(&o.key, "key", "", "filename containing the client certificate private key")
	fs.StringVar(&o.ca, "ca", "", "filename containing the CA that can verify the server")
	fs.StringVar(&o.addr, "addr", "", "server address")
	fs.StringVar(&o.name, "name", "", "service name")
	fs.StringVar(&o.op, "op", "addSrv", "addSrv, delSrv, addTgt, delTgt, list")
	fs.StringVar(&o.service, "srv", "", "in {add,del}Srv - service ip, in {add,del}Tgt - target ip")
	fs.BoolVar(&o.tcp, "tcp", true, "true == TCP, false == UDP")
	fs.IntVar(&o.dstPort, "dport", 8080, "destination port")
	fs.IntVar(&o.srcPort, "sport", 8080, "source port")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.name == "" && o.op != "list" {
		return nil, errors.New("no loadbalancer name provided")
	}
	return o, nil
}

func loadConfigFile(k *koanf.Koanf, configFile string) error {
	return k.Load(file.Provider(configFile), yaml.Parser())
}

// loadKeypair builds the client credentials from the flags, falling back to
// the "auth" section of the configuration.
func loadKeypair(o *options, k *koanf.Koanf) (credentials.TransportCredentials, error) {
	keyFile, certFile, caFile := o.key, o.cert, o.ca
	if keyFile == "" {
		keyFile = k.String("auth.clientKey")
	}
	if certFile == "" {
		certFile = k.String("auth.clientCert")
	}
	if keyFile == "" {
		// The key may be in the same file as the certificate
		keyFile = certFile
	}
	if caFile == "" {
		caFile = k.String("auth.caFile")
	}
	if keyFile == "" || certFile == "" || caFile == "" {
		return nil, fmt.Errorf("no mTLS configuration found")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	capool := x509.NewCertPool()
	if !capool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificate found in %s", caFile)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      capool,
	}), nil
}

// run connects to the server and performs the requested operation.
func run(ctx context.Context, o *options, k *koanf.Koanf) error {
	creds, err := loadKeypair(o, k)
	if err != nil {
		return fmt.Errorf("error loading mTLS configuration: %+v", err)
	}
	addr := o.addr
	if addr == "" {
		addr = k.String("addr")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("error connecting to lbmanager: %+v", err)
	}
	defer func() { _ = conn.Close() }()
	return execute(ctx, loadbalancer.NewLoadBalancerClient(conn), o)
}

// execute performs the operation on the server.
func execute(ctx context.Context, client loadbalancer.LoadBalancerClient, o *options) error {
	p := loadbalancer.Protocol_TCP
	if !o.tcp {
		p = loadbalancer.Protocol_UDP
	}
	reportResult := func(ret *loadbalancer.Error) {
		if len(ret.Message) != 0 {
			log.Printf("Message from server: %s", ret.Message)
		} else {
			log.Printf("Success!")
		}
	}

	switch o.op {
	case "addSrv":
		lbInfo, err := client.Create(ctx, &loadbalancer.CreateLoadBalancer{Name: o.name, IpAddr: &o.service})
		if err != nil {
			return fmt.Errorf("error during create: %s", err.Error())
		}
		log.Printf("LBInfo: %+v\n", lbInfo)

	case "delSrv":
		ret, err := client.Delete(ctx, &loadbalancer.LoadBalancerName{Name: o.name})
		if err != nil {
			return fmt.Errorf("error during delete: %s", err.Error())
		}
		reportResult(ret)

	case "addTgt":
		ret, err := client.AddTarget(ctx, &loadbalancer.AddTargetRequest{
			LbName:  o.name,
			SrcPort: int32(o.srcPort),
			Target:  &loadbalancer.Target{DstIP: o.service, DstPort: int32(o.dstPort), Protocol: p},
		})
		if err != nil {
			return fmt.Errorf("error adding destination: %s", err.Error())
		}
		reportResult(ret)

	case "delTgt":
		ret, err := client.DelTarget(ctx, &loadbalancer.DelTargetRequest{
			LbName:  o.name,
			SrcPort: int32(o.srcPort),
			Target:  &loadbalancer.Target{DstIP: o.service, DstPort: int32(o.dstPort), Protocol: p},
		})
		if err != nil {
			return fmt.Errorf("error deleting destination: %s", err.Error())
		}
		reportResult(ret)

	case "list":
		stream, err := client.GetLoadBalancers(ctx, &emptypb.Empty{})
		if err != nil {
			return fmt.Errorf("failed to list load balancers: %s", err.Error())
		}
		for {
			lb, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("error listing load balancers: %v", err)
			}
			log.Println(lb)
		}
	default:
		return fmt.Errorf("unknown operation %s", o.op)
	}
	return nil
}

func main() {
	k := koanf.New(".")
	if err := loadConfigFile(k, "/etc/lbmanager/lbctl.yaml"); err != nil && !os.IsNotExist(err) {
		log.Fatalf("failed loading config: %v", err)
	}
	o, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		log.Fatal(err)
	}
	if err := run(context.Background(), o, k); err != nil {
		log.Fatal(err)
	}
}
