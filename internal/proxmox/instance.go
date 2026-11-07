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
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
)

/*

	ProviderID for Proxmox based kubernetes nodes uses the following schema:

	proxmox://[cluster name]/[node name]/[vm id]

*/

// Instances implements the cloudprovider.InstancesV2 interface
type Instances struct {
	client *proxmox.Client
}

var _ cloudprovider.InstancesV2 = &Instances{}

func newInstances(config ClusterConfig) (cloudprovider.InstancesV2, error) {
	username, err := os.ReadFile(config.Username)
	if err != nil {
		return nil, err
	}
	apiToken, err := os.ReadFile(config.APIToken)
	if err != nil {
		return nil, err
	}
	opts := []proxmox.Option{
		proxmox.WithAPIToken(strings.TrimSpace(string(username)), strings.TrimSpace(string(apiToken))),
	}
	if config.Timeout > 0 {
		opts = append(opts, proxmox.WithTimeout(time.Duration(config.Timeout)*time.Second))
	}
	if config.InsecureTLS {
		opts = append(opts, proxmox.WithInsecureSkipVerify())
	} else if config.CACert != "" {
		data, err := os.ReadFile(config.CACert)
		if err != nil {
			return nil, err
		}
		capool := x509.NewCertPool()
		if !capool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("failed to load ca certificate")
		}
		opts = append(opts, proxmox.WithRootCAs(capool))
	}
	if config.Debug {
		opts = append(opts, proxmox.WithLogger(&proxmox.LeveledLogger{Level: proxmox.LevelDebug}))
	}
	c := proxmox.NewClient(config.URL, opts...)
	if _, err := c.Version(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to authenticate to proxmox at %s: %w", config.URL, err)
	}
	return &Instances{
		client: c,
	}, nil
}

// errVMNotFound is returned when no virtual machine matches a Kubernetes node
var errVMNotFound = errors.New("virtual machine not found")

// InstanceExists returns true if the instance for the given node exists according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *Instances) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	log.Printf("InstanceExists called for node %s ", node.Name)
	_, _, err := i.findVM(ctx, node)
	if errors.Is(err, errVMNotFound) {
		return false, nil
	}
	// Any other error (API unreachable, bad credentials, ...) must not be reported as
	// "the VM is gone", since that causes the node object to be deleted from the cluster.
	return err == nil, err
}

// InstanceShutdown returns true if the instance is shutdown according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *Instances) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	log.Printf("InstanceShutdown called for node %s ", node.Name)
	vm, _, err := i.findVM(ctx, node)
	if err != nil {
		return false, err
	}
	if vm.Status == "" {
		return false, fmt.Errorf("unexpected vm info - no status field returned for vm %d", uint64(vm.VMID))
	}
	return vm.Status != "running", nil
}

// InstanceMetadata returns the instance's metadata. The values returned in InstanceMetadata are
// translated into specific fields and labels in the Node object on registration.
// Implementations should always check node.spec.providerID first when trying to discover the instance
// for a given node. In cases where node.spec.providerID is empty, implementations can use other
// properties of the node like its name, labels and annotations.
func (i *Instances) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	log.Printf("InstanceMetadata called for node %s ", node.Name)
	vm, clusterName, err := i.findVM(ctx, node)
	if err != nil {
		return nil, err
	}

	// The addresses the guest agent reports on the VM's own network interfaces
	// work for static and DHCP configurations. Without an agent, use the static
	// cloud-init settings.
	var agentIfaces []*proxmox.AgentNetworkIface
	if vm.Status == "running" {
		if agentIfaces, err = vm.AgentGetNetworkIFaces(ctx); err != nil {
			log.Printf("guest agent of %s is not available, using the cloud-init settings: %v", vm.Name, err)
		}
	}

	if clusterName == "" {
		clusterName = defaultRegion
	}
	retval := &cloudprovider.InstanceMetadata{
		ProviderID:    getProviderID(vm),
		InstanceType:  instanceType(vm.CPUs, vm.MaxMem),
		NodeAddresses: nodeAddresses(vm.VirtualMachineConfig, agentIfaces),
		Zone:          vm.Node,
		Region:        clusterName,
	}

	log.Printf("Metadata returns %+v", retval)
	return retval, nil
}

// defaultRegion is the region of nodes whose Proxmox host is not in a cluster
const defaultRegion = "proxmox"

// instanceType describes the size of the VM, for example "4c-8g".
func instanceType(cpus int, maxMem uint64) string {
	if cpus <= 0 || maxMem == 0 {
		return "qemu"
	}
	if maxMem%(1<<30) == 0 {
		return fmt.Sprintf("%dc-%dg", cpus, maxMem>>30)
	}
	return fmt.Sprintf("%dc-%dm", cpus, maxMem>>20)
}

// nodeAddresses prefers the addresses reported by the guest agent, and falls back
// to the static cloud-init settings. The hostname is always included.
func nodeAddresses(config *proxmox.VirtualMachineConfig, ifaces []*proxmox.AgentNetworkIface) []v1.NodeAddress {
	static := getAddresses(config)
	fromAgent := agentAddresses(config, ifaces)
	if len(fromAgent) == 0 {
		return static
	}
	for _, a := range static {
		if a.Type == v1.NodeHostName {
			fromAgent = append(fromAgent, a)
		}
	}
	return fromAgent
}

// agentAddresses returns the IPv4 addresses of the interfaces that carry one of the
// VM's own MAC addresses. Interfaces created inside the guest (pod networks, bridges,
// docker, ...) have other MACs and are ignored, as are loopback and link-local addresses.
func agentAddresses(config *proxmox.VirtualMachineConfig, ifaces []*proxmox.AgentNetworkIface) []v1.NodeAddress {
	if config == nil {
		return nil
	}
	macs := map[string]bool{}
	for _, nic := range config.Nets {
		for _, part := range strings.Split(nic, ",") {
			// model=MAC, for example virtio=BC:24:11:AA:BB:CC
			if _, v, found := strings.Cut(part, "="); found {
				if hw, err := net.ParseMAC(v); err == nil {
					macs[hw.String()] = true
				}
			}
		}
	}
	retval := []v1.NodeAddress{}
	seen := map[string]bool{}
	for _, iface := range ifaces {
		hw, err := net.ParseMAC(iface.HardwareAddress)
		if err != nil || !macs[hw.String()] {
			continue
		}
		for _, a := range iface.IPAddresses {
			ip := net.ParseIP(a.IPAddress)
			if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			retval = append(retval, v1.NodeAddress{Type: v1.NodeInternalIP, Address: ip.String()})
		}
	}
	return retval
}

func getAddresses(config *proxmox.VirtualMachineConfig) []v1.NodeAddress {
	retval := []v1.NodeAddress{}
	if config == nil {
		return retval
	}
	for k, v := range config.IPConfigs {
		// Sometimes there's an ipconfig? entry without a net? entry.
		if _, found := config.Nets["net"+strings.TrimPrefix(k, "ipconfig")]; !found {
			continue
		}
		for _, currPart := range strings.Split(v, ",") {
			if before, after, found := strings.Cut(currPart, "="); found && before == "ip" {
				ip, _, err := net.ParseCIDR(after)
				if err != nil {
					log.Printf("unexpected ip address %s: %s", currPart, err.Error())
					continue
				}
				retval = append(retval, v1.NodeAddress{
					Type:    v1.NodeInternalIP,
					Address: ip.String(),
				})
				// already found the IP, no need to go on in this configuration key
				break
			}
		}
	}
	if config.Name != "" {
		retval = append(retval, v1.NodeAddress{
			Type:    v1.NodeHostName,
			Address: config.Name,
		})
	}
	return retval
}

func getProviderID(vm *proxmox.VirtualMachine) string {
	// TODO: the cluster name is not retrieved, leave it empty for now.
	return fmt.Sprintf("proxmox://%s/%s/%d", "", vm.Node, uint64(vm.VMID))
}

// parseProviderID extracts the VM ID out of a providerID of the form proxmox:///<node>/<vmid>
func parseProviderID(providerID string) (int, bool) {
	if providerID == "" {
		return 0, false
	}
	providerURL, err := url.Parse(providerID)
	if err != nil {
		log.Printf("ignoring invalid providerID: %s", providerID)
		return 0, false
	}
	parts := strings.Split(providerURL.Path, "/")
	// The expected format is "/node name/vm id"
	if len(parts) != 3 {
		log.Printf("ignoring invalid providerID: %s, path should be '<node>/<id>'", providerID)
		return 0, false
	}
	id, err := strconv.Atoi(parts[2])
	if err != nil {
		log.Printf("ignoring invalid providerID: %s, id part is not numeric: %s", providerID, err.Error())
		return 0, false
	}
	return id, true
}

// findVM locates the virtual machine backing a Kubernetes node, first by the providerID
// and, failing that, by the node name. The returned VM includes its current status and configuration,
// and the name of the Proxmox cluster is returned as well.
func (i *Instances) findVM(ctx context.Context, node *v1.Node) (*proxmox.VirtualMachine, string, error) {
	if i.client == nil {
		return nil, "", fmt.Errorf("not connected to any proxmox cluster")
	}
	cluster, err := i.client.Cluster(ctx)
	if err != nil {
		return nil, "", err
	}
	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return nil, "", err
	}

	vmid, byID := parseProviderID(node.Spec.ProviderID)
	var match *proxmox.ClusterResource
	for _, r := range resources {
		if r.Type != "qemu" || r.Template != 0 {
			continue
		}
		if (byID && r.VMID == uint64(vmid)) || (!byID && r.Name == node.Name) {
			match = r
			break
		}
	}
	if match == nil {
		return nil, "", fmt.Errorf("%w: node %s (providerID %q)", errVMNotFound, node.Name, node.Spec.ProviderID)
	}

	pveNode, err := i.client.Node(ctx, match.Node)
	if err != nil {
		return nil, "", err
	}
	vm, err := pveNode.VirtualMachine(ctx, int(match.VMID))
	return vm, cluster.Name, err
}
