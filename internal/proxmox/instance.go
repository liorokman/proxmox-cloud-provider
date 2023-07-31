package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Telmate/proxmox-api-go/proxmox"
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
	tls := &tls.Config{}
	if config.InsecureTLS {
		tls.InsecureSkipVerify = true
	} else {
		if config.CACert != "" {
			capool := x509.NewCertPool()
			if !capool.AppendCertsFromPEM([]byte(config.CACert)) {
				return nil, fmt.Errorf("Failed to load ca certificate")
			}
			tls.RootCAs = capool
		}
	}
	c, err := proxmox.NewClient(config.URL, nil, "", tls, "", config.Timeout)
	if err != nil {
		return nil, err
	}
	c.SetAPIToken(config.Username, config.APIToken)
	if _, err := c.GetVersion(); err != nil {
		return nil, fmt.Errorf("bad username password: %w", err)
	}
	return &Instances{
		client: c,
	}, nil

}

// InstanceExists returns true if the instance for the given node exists according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *Instances) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	log.Printf("InstanceExists called for node %s ", node.Name)
	vmRef, err := i.getVMRef(ctx, node)
	if err != nil {
		return false, nil
	}
	_, err = i.getVMInfo(vmRef)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// InstanceShutdown returns true if the instance is shutdown according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *Instances) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	log.Printf("InstanceShutdown called for node %s ", node.Name)
	vmRef, err := i.getVMRef(ctx, node)
	if err != nil {
		return false, err
	}
	vmInfo, err := i.getVMInfo(vmRef)
	if err != nil {
		return false, err
	}
	if status, found := vmInfo["status"]; found {
		return status != "running", nil
	}
	return false, fmt.Errorf("unexpected vm info - no status field returned: %+v", vmInfo)
}

// InstanceMetadata returns the instance's metadata. The values returned in InstanceMetadata are
// translated into specific fields and labels in the Node object on registration.
// Implementations should always check node.spec.providerID first when trying to discover the instance
// for a given node. In cases where node.spec.providerID is empty, implementations can use other
// properties of the node like its name, labels and annotations.
func (i *Instances) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	log.Printf("InstanceMetadata called for node %s ", node.Name)
	vmRef, err := i.getVMRef(ctx, node)
	if err != nil {
		return nil, err
	}
	vmInfo, err := i.getVMInfo(vmRef)
	if err != nil {
		return nil, err
	}

	vmConfig, err := i.client.GetVmConfig(vmRef)
	if err != nil {
		return nil, err
	}

	var nodeName string
	var vmType string
	var found bool
	if nodeName, found = vmInfo["node"].(string); !found {
		log.Printf("info doesn't contain the node name")
	}
	if vmType, found = vmInfo["type"].(string); !found {
		log.Printf("info doesn't contain the vm type")
	}

	retval := &cloudprovider.InstanceMetadata{
		ProviderID:    getProviderID(vmInfo),
		InstanceType:  vmType,
		NodeAddresses: getAddresses(vmConfig),
		Zone:          nodeName,
	}

	log.Printf("Metadata returns %+v", retval)
	return retval, nil
}

func getAddresses(config map[string]any) []v1.NodeAddress {
	retval := []v1.NodeAddress{}
	for k, v := range config {
		if strings.HasPrefix(k, "ipconfig") {
			// Sometimes there's an ipconfig? entry without a net? entry.
			if _, found := config["net"+k[8:]]; !found {
				continue
			}
			parts := strings.Split(v.(string), ",")
			for _, currPart := range parts {
				if before, after, found := strings.Cut(currPart, "="); found {
					if before == "ip" {
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
		} else if k == "name" {
			retval = append(retval, v1.NodeAddress{
				Type:    v1.NodeHostName,
				Address: v.(string),
			})
		}
	}
	return retval
}

func getProviderID(info map[string]any) string {
	// TODO: the current proxmox-go-api library doesn't provide an API to retrieve the cluster name. Ignore it for now.
	nodeName := ""
	vmid := 0.0
	found := false
	if nodeName, found = info["node"].(string); !found {
		log.Printf("info doesn't contain the node name")
	}
	if vmid, found = info["vmid"].(float64); !found {
		log.Printf("info doesn't contain the vmid")
	}
	return fmt.Sprintf("proxmox://%s/%s/%d", "", nodeName, int(vmid))
}

func (i *Instances) getVMRef(ctx context.Context, node *v1.Node) (*proxmox.VmRef, error) {
	if i.client == nil {
		return nil, fmt.Errorf("not connected to any proxmox cluster")
	}
	var vmRef *proxmox.VmRef
	if node.Spec.ProviderID != "" {
		if providerURL, err := url.Parse(node.Spec.ProviderID); err == nil {
			parts := strings.Split(providerURL.Path, "/")
			// The expected format is "/node name/vm id"
			if len(parts) == 3 {
				if id, err := strconv.Atoi(parts[2]); err == nil {
					vmRef = proxmox.NewVmRef(id)
				} else {
					log.Printf("ignoring invalid providerID: %s, id part is not numeric: %s", node.Spec.ProviderID, err.Error())
				}
			} else {
				log.Printf("ignoring invalid providerID: %s, path should be '<node>/<id>'", node.Spec.ProviderID)
			}
		} else {
			log.Printf("ignoring invalid providerID: %s", node.Spec.ProviderID)
		}
	}
	if vmRef == nil {
		// Failed to identify the node using ProviderID, try using the node.name
		var err error
		vmRef, err = i.client.GetVmRefByName(node.Name)
		if err != nil {
			return nil, err
		}
	}
	return vmRef, nil
}

func (i *Instances) getVMInfo(vmRef *proxmox.VmRef) (map[string]any, error) {
	if vmRef != nil {
		vmInfo, err := i.client.GetVmInfo(vmRef)
		if err != nil {
			return nil, err
		}
		return vmInfo, nil
	}
	return nil, fmt.Errorf("empty vmref provided")
}
