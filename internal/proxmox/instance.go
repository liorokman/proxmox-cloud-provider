package proxmox

import (
	"context"

	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
)

type Instances struct {
}

var _ cloudprovider.InstancesV2 = &Instances{}

// InstanceExists returns true if the instance for the given node exists according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *Instances) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	panic("not implemented") // TODO: Implement
}

// InstanceShutdown returns true if the instance is shutdown according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *Instances) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	panic("not implemented") // TODO: Implement
}

// InstanceMetadata returns the instance's metadata. The values returned in InstanceMetadata are
// translated into specific fields and labels in the Node object on registration.
// Implementations should always check node.spec.providerID first when trying to discover the instance
// for a given node. In cases where node.spec.providerID is empty, implementations can use other
// properties of the node like its name, labels and annotations.
func (i *Instances) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	panic("not implemented") // TODO: Implement
}
