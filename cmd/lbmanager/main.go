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
// The configuration reload follows the example in the koanf documentation,
// Copyright (c) 2019 Kailash Nadh, MIT License (https://github.com/knadh/koanf).
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var k = koanf.New(".")

// loadConfig reads the configuration from /etc/lbmanager and the current directory.
func loadConfig() error {
	log.Printf("Loading configuration...")
	somethingLoaded := false
	err := loadConfigFile("/etc/lbmanager/lbmanager.yaml")
	if err == nil {
		somethingLoaded = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("error loading config: %v", err)
	}

	err = loadConfigFile("lbmanager.yaml")
	if err == nil {
		somethingLoaded = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("error loading local config: %v", err)
	}

	if !somethingLoaded {
		return errors.New("no configuration found, cowardly refusing to continue")
	}
	log.Printf("done\n")
	return nil
}

func loadConfigFile(configFile string) error {
	mainConfigFile := file.Provider(configFile)
	if err := k.Load(mainConfigFile, yaml.Parser()); err != nil {
		return err
	}
	if err := mainConfigFile.Watch(func(event any, err error) {
		if err != nil {
			log.Printf("watch error: %v\n", err)
			return
		}
		log.Println("config changed, reloading ...")
		tmpK := koanf.New(".")
		if err := tmpK.Load(mainConfigFile, yaml.Parser()); err != nil {
			log.Printf("error loading the new config: %v\n", err)
			return
		}
		if err := k.Merge(tmpK); err != nil {
			log.Printf("error merging the new config: %v\n", err)
		}
	}); err != nil {
		log.Printf("not watching %s for changes: %v", configFile, err)
	}
	return nil
}

func newGRPCCreds(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	capool := x509.NewCertPool()
	if !capool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no certificate found in %s", caFile)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    capool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}), nil
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, k); err != nil {
		log.Fatalf("%v", err)
	}
}

// run starts the load balancer manager and serves requests until ctx is done.
func run(ctx context.Context, config *koanf.Koanf) error {
	creds, err := newGRPCCreds(config.String("grpc.auth.cert"),
		config.String("grpc.auth.key"),
		config.String("grpc.auth.ca"))
	if err != nil {
		return fmt.Errorf("error initializing TLS: %v", err)
	}

	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", config.String("grpc.listen"), config.Int("grpc.port")))
	if err != nil {
		return fmt.Errorf("failed listening: %v", err)
	}
	defer func() { _ = lis.Close() }()
	lbServer, err := loadbalancer.NewServer(config)
	if err != nil {
		return fmt.Errorf("failed starting the loadbalancer manager: %v", err)
	}
	defer lbServer.Close()
	if err := lbServer.Restore(); err != nil {
		return fmt.Errorf("failed restoring the loadbalancer configuration: %v", err)
	}
	lbServer.StartHealthChecks(ctx)
	if err := lbServer.ServeMetrics(ctx, config.String("metrics.listen")); err != nil {
		return fmt.Errorf("failed starting the metrics endpoint: %v", err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	loadbalancer.RegisterLoadBalancerServer(grpcServer, lbServer)
	log.Println("Listening...")
	go func() {
		<-ctx.Done()
		log.Println("Shutting down...")
		stopped := make(chan struct{})
		go func() { grpcServer.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			grpcServer.Stop()
		}
	}()
	if err := grpcServer.Serve(lis); err != nil {
		log.Printf("serve: %v", err)
	}
	return nil
}
