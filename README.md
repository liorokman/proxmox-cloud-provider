
# Proxmox Cloud Controller Provider

This projects provides all that is needed to create Kubernetes clusters on
Proxmox where the Kubernetes clusters rely on the SDN feature in Proxmox. 

The Proxmox SDN functionality allows creating an isolated network for the
Kubernetes nodes, and this project provides the glue required to add a load balancer 
and allow Kubernetes to configure it when `LoadBalancer` services are created.

# Proxmox Setup

The following steps are required to prepare Proxmox for Kubernetes clusters
using this cloud controller provider:

1. Enable the SDN functionality
   
   Full documentation is available [here](https://pve.proxmox.com/pve-docs/chapter-pvesdn.html).

1. Configure an SDN zone for Kubernetes clusters

   Create a VXLAN or VLAN zone that will contain VNets for your Kubernetes clusters. 

   Make sure that the zone is configured correctly to allow for all Proxmox
   nodes in the cluster to communicate. 

1. Create an SDN VNet in the newly created zone.

   This VNet will contain subnets for each Kubernetes cluster. After the VNet
   is created, the Proxmox nodes will contain a bridge device called by the
   name given to the VNet. For the rest of this README, the VNet name is
   assumed to be `k8s`.

1. Prepare a Proxmox Storage that allows Snippets.

   This storage will be used for storing the load-balancer hookscript. Called
   `$SNIPPET_STORAGE` in this README.

1. Prepare a Proxmox Storage for VMs and LXC Containers.

   Called `$STORAGE` in this README.

# Creating a Kubernetes Cluster

## Required Information

1. As a convenience, choose a range of VM IDs to be used for the Kubernetes
   cluster. In this README, all Kubernetes related VMs and container will be in
   the range 1100-1200 .

1. Allocate a management IP address in the main network bridge (`vmbr0`) for
   the loadbalancer. 

   This IP will be used for connecting to the load-balancer container for
   management purposes. Called `$LB_MNG_IP` in this README.

1. Allocate an IP address in the main network bridge (`vmbr0`) for the
   Kubernetes API server. Called `$LB_EXT_IP` in this README.

1. Find out the main network's CIDR and gateway. Respectivly `$EXT_CIDR` and
   `$EXT_GW`.

1. Reserve a range of IPs for the loadbalancer to allocate when services are
   created. Called `$LB_EXT_START` and `$LB_EXT_END` respectively.

   For example, the following allocates 150 IPs for the loadbalancer 
   ```bash
      LB_EXT_START=192.168.50.50
      LB_EXT_END=192.168.50.200
   ```

## Setup a LoadBalancer LXC Container

Each Kubernetes cluster requires a lightweight LXC container that will be used
to configure an `IPVS` based load-balancer. 

 > Note: The current implementation supports only one load balancer container - this
will be fixed in future versions of this project.

1. Create a subnet for the new cluster in the `k8s` VNet. Enable SNAT, and set
   the gateway IP to the first IP in the subnet.

   For this README, the the subnet is `192.168.50.0/24`, and the gateway IP is
   `192.168.50.1`.

1. Download the latest Debian template.

   As of this writing, that template was version 12.0-1.

1. Create the container

   The container is also the default gateway to the main network bridge, so the
   `net1` interface should use the same IP that was allocated for the gateway
   in the Kubernetes nodes subnet.

   Make sure to run the container as a privileged container - this is required
   for `ipvs` to work from inside the container. 

   ```bash
      DESCRIPTION="{\"ip\":\"$LB_EXT_IP\",\"mask\":24,\"start\":\"$LB_EXT_START\",\"end\":\"$LB_EXT_END\"}"
      pct create 1100  local:vztmpl/debian-12-standard_12.0-1_amd64.tar.zst  \
          -unprivileged 0 \
          -cores 2 -swap 0 -memory 256 -hostname k8slb1  \
          -net0 name=eth0,bridge=vmbr0,firewall=1,ip=$LB_EXT_IP/$EXT_CIDR,gw=$EXT_GW,type=veth \
          -net1 name=eth1,bridge=k8s,firewall=1,ip=192.168.50.1/24,type=veth \
          -ostype debian  -features nesting=1 \
          -password=$PASSWORD  -storage $STORAGE \
          -description $DESCRIPTION
   ```

1. Copy the configuration hookscript `scripts/installLB.sh` to
   `$SNIPPET_STORAGE/snippets` on the Proxmox node where the load balancer
   container was created.

1. Enable the hookscript for the load balancer container.

   This script installs all the needed utilities on the container, and
   configures the network correctly. 

   ```bash
      pct set 1100 -hookscript $SNIPPET_STORAGE:snippets/installLB.sh
   ```

1. Start the container.

   ```bash
      pct start 1100
   ```

At this point the load balancer container will not yet start to work since the
configuration is not complete. The mTLS configuration for allowing the provider
to authenticate to the loadbalancer manager will be added in a later step.

## Prepare a VM Template for Kubernetes Nodes


Follow the instructions in [this](https://github.com/liorokman/packer-Debian#use-in-proxmox)
github repository to prepare a template that can be used for creating Kubernetes nodes.

This README assumes that the template's ID is `$K8S_TEMPLATE_ID`

## Create the first node

1. Clone the template
1. Update the cloud-init parameters
   1. Set a valid ssh key for the `debian` user
   1. Set a valid IP address. The default gateway should be the loadbalancer's
      ip in the `k8s` subnet.
1. Regenerate the cloud-init volume
1. Resize the disk to at least 20Gb

## Configure Kubeadm and create the cluster

## Create additional master nodes

## Create worker nodes

