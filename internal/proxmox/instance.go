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
	_, err := i.findVM(ctx, node)
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
	vm, err := i.findVM(ctx, node)
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
	vm, err := i.findVM(ctx, node)
	if err != nil {
		return nil, err
	}

	retval := &cloudprovider.InstanceMetadata{
		ProviderID:    getProviderID(vm),
		InstanceType:  "qemu",
		NodeAddresses: getAddresses(vm.VirtualMachineConfig),
		Zone:          vm.Node,
	}

	log.Printf("Metadata returns %+v", retval)
	return retval, nil
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
// and, failing that, by the node name. The returned VM includes its current status and configuration.
func (i *Instances) findVM(ctx context.Context, node *v1.Node) (*proxmox.VirtualMachine, error) {
	if i.client == nil {
		return nil, fmt.Errorf("not connected to any proxmox cluster")
	}
	cluster, err := i.client.Cluster(ctx)
	if err != nil {
		return nil, err
	}
	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("%w: node %s (providerID %q)", errVMNotFound, node.Name, node.Spec.ProviderID)
	}

	pveNode, err := i.client.Node(ctx, match.Node)
	if err != nil {
		return nil, err
	}
	return pveNode.VirtualMachine(ctx, int(match.VMID))
}
