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
// Some of the comments in this file are taken from k8s.io/cloud-provider,
// Copyright The Kubernetes Authors, licensed under the Apache License, Version 2.0.
//
// SPDX-License-Identifier: Apache-2.0

package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v2"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

// Provider implements the CCM interface
type Provider struct {
	config    ProviderConfig
	lbManager *LoadBalancer
	instances cloudprovider.InstancesV2
}

var _ cloudprovider.Interface = &Provider{}

func init() {
	cloudprovider.RegisterCloudProvider("proxmox", New)
}

// ProviderConfig contains the configuration values required by the proxmox provider
type ProviderConfig struct {
	Security      Security      `yaml:"security"`
	ManagerHost   string        `yaml:"managerHost"`
	ManagerPort   int           `yaml:"managerPort"`
	ClusterConfig ClusterConfig `yaml:"proxmoxConfig"`
}

// Security contains the mTLS configuration for connecting to the loadbalancer manager
type Security struct {
	CertFile string `yaml:"cert"`
	KeyFile  string `yaml:"key"`
	CAFile   string `yaml:"ca"`
}

// ClusterConfig contains the proxmox cluster configuration information
type ClusterConfig struct {
	URL         string `yaml:"url"`
	Timeout     int    `yaml:"timeout"`
	APIToken    string `yaml:"apiToken"`
	Username    string `yaml:"username"`
	InsecureTLS bool   `yaml:"insecureTLS"`
	CACert      string `yaml:"caCert"`
	Debug       bool   `yaml:"debug"`
	// LabelSyncInterval is how often (in seconds) the node labels that come from
	// the VMs (zone, instance type, tags) are brought up to date. 0 means 60,
	// and a negative value turns the synchronization off.
	LabelSyncInterval int `yaml:"labelSyncInterval"`
}

// New returns a new instance of the proxmox cloud provider
func New(config io.Reader) (cloudprovider.Interface, error) {
	c, err := io.ReadAll(config)
	if err != nil {
		return nil, err
	}
	providerConfig := ProviderConfig{}
	err = yaml.Unmarshal(c, &providerConfig)
	if err != nil {
		return nil, err
	}

	return &Provider{
		config: providerConfig,
	}, nil
}

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping or run custom controllers specific to the cloud provider.
// Any tasks started here should be cleaned up when the stop channel closes.
func (p *Provider) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {

	instances, err := newInstances(p.config.ClusterConfig)
	if err != nil {
		log.Fatalf("failed to connect to proxmox: %s", err.Error())
	}
	p.instances = instances

	if interval := labelSyncInterval(p.config.ClusterConfig); interval > 0 && clientBuilder != nil {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-stop
			cancel()
		}()
		syncer := &labelSyncer{
			instances: instances.(*Instances),
			client:    clientBuilder.ClientOrDie("proxmox-label-sync"),
			interval:  interval,
		}
		go syncer.run(ctx)
	}

	creds, err := loadKeypair(p.config.Security)
	if err != nil {
		log.Fatalf("failed to load the lbmanager credentials: %s", err.Error())
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}
	serverAddr := fmt.Sprintf("%s:%d", p.config.ManagerHost, p.config.ManagerPort)
	conn, err := grpc.NewClient(serverAddr, opts...)
	if err != nil {
		log.Fatalf("failed to connect to the lbmanager: %s", err.Error())
	}
	p.lbManager = &LoadBalancer{
		client: loadbalancer.NewLoadBalancerClient(conn),
	}
	go func() {
		<-stop
		_ = conn.Close()
	}()
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
func (p *Provider) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return p.lbManager, true
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (p *Provider) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

// InstancesV2 is an implementation for instances and should only be implemented by external cloud providers.
// Implementing InstancesV2 is behaviorally identical to Instances but is optimized to significantly reduce
// API calls to the cloud provider when registering and syncing nodes. Implementation of this interface will
// disable calls to the Zones interface. Also returns true if the interface is supported, false otherwise.
func (p *Provider) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return p.instances, true
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
// DEPRECATED: Zones is deprecated in favor of retrieving zone/region information from InstancesV2.
// This interface will not be called if InstancesV2 is enabled.
func (p *Provider) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (p *Provider) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (p *Provider) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (p *Provider) ProviderName() string {
	return "proxmox"
}

// HasClusterID returns true if a ClusterID is required and set
func (p *Provider) HasClusterID() bool {
	return true
}

// labelSyncInterval returns how often to synchronize node labels, 0 when it is turned off.
func labelSyncInterval(c ClusterConfig) time.Duration {
	switch {
	case c.LabelSyncInterval < 0:
		return 0
	case c.LabelSyncInterval == 0:
		return time.Minute
	}
	return time.Duration(c.LabelSyncInterval) * time.Second
}

func loadKeypair(security Security) (credentials.TransportCredentials, error) {
	if security.KeyFile == "" {
		security.KeyFile = security.CertFile
	}
	cert, err := tls.LoadX509KeyPair(security.CertFile, security.KeyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(security.CAFile)
	if err != nil {
		return nil, err
	}
	capool := x509.NewCertPool()
	if !capool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificate found in %s", security.CAFile)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      capool,
	}), nil
}

func asPtr[T any](v T) *T {
	return &v
}
