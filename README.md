
# Proxmox Cloud Controller Provider

This projects provides all that is needed to create Kubernetes clusters on
Proxmox where the Kubernetes clusters hosts are in their own Proxmox isolated
SDN VNet.

The Proxmox SDN functionality allows creating an isolated network for the
Kubernetes nodes, and this project provides the glue required to add a load balancer 
and allow Kubernetes to configure it when `LoadBalancer` services are created.


# Proxmox Setup

The following steps are required to prepare Proxmox for Kubernetes clusters
using this cloud controller provider:

1. Enable the SDN functionality
   
   Full documentation is available [here](https://pve.proxmox.com/pve-docs/chapter-pvesdn.html).

1. Install the `jq` utility on Proxmox

   This utility is used by the hookscript for creating the loadbalancer.

   ```bash
      apt install jq
   ```

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

1. Prepare an API token for the Proxmox Cloud Provider

 > Note: The provided example command creates a very powerful API token, which is probably overkill. A future README will specify the exact privileges required.

   ```bash
   pveum user token add root@pam ccm -privsep=0
   ```
 
   Set `PROXMOX_API_TOKEN` to the generated token, and set `PROXMOX_API_USERNAME`
   to the correct username. In this example, the username would be
   `root@pam!ccm`

# Creating a Kubernetes Cluster

## Required Information

1. Choose a range of VM IDs to be used for the Kubernetes
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
          -cores 2 -swap 0 -memory 512 -hostname k8slb1  \
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

## Prepare a VM Template for Kubernetes Nodes

Follow the instructions in [this](https://github.com/liorokman/packer-Debian#use-in-proxmox)
Github repository to prepare a template that can be used for creating Kubernetes nodes.

A pre-built image is available in the [releases](https://github.com/liorokman/proxmox-cloud-provider/releases/tag/v0.0.2) section of this project with `kubeadm`, `kubelet`, and `kubectl` versions 1.27.4 .

This README assumes that the template's ID is `$K8S_TEMPLATE_ID`

## Create and start the first node 

1. Clone the template

   Choose an unused ID for the new VM. This section of the README assumes `$K8S_NEW_ID` is 
   set to to the ID.

   ```bash
   qm clone $K8S_TEMPLATE_ID $K8S_NEW_ID -full -name k8s-master1 -storage $STORAGE
   ```

1. Update the cloud-init parameters
   1. Set a valid ssh key for the `debian` user

   ```bash
   qm set $K8S_NEW_ID -sshkeys /path/to/ssh/public/key/file
   ```

   1. Set a valid IP address. The default gateway should be the loadbalancer's
      ip in the `k8s` subnet: `$LB_EXT_IP`

   ```bash
   qm set $K8S_NEW_ID -ipconfig0 ip=192.168.50.3/24,gw=$LB_EXT_IP
   ```
1. Regenerate the cloud-init volume

   ```bash
   qm cloudinit update $K8S_NEW_ID
   ```

1. Resize the disk to at least 20Gb

   ```bash
   qm disk resize $K8S_NEW_ID virtio0 20G
   ```

1. Start the vm

   ```bash
   qm start $K8S_NEW_ID
   ```

## Finalize the loadbalancer configuration

Enter the loadbalancer container and:

1. Configure a loadbalancer that reaches the master node's API server.

```bash
pct enter 1100
/usr/local/bin/lbctl -op addSrv -name apiserver -srv $PUBLIC_K8S_IP
/usr/local/bin/lbctl -op addTgt -name apiserver -srv $NODE_INTERNAL_IP -sport 6443 -dport 6443
```

1. Prepare credentials for the Proxmox CCM provider 

Reuse the credentials created for `lbctl` by copying `/etc/lbmanager/lbctl.pem`, 
`/etc/lbmanager/lbctl-key.pem`, and `/etc/lbmanager/ca.pem` to the master node being prepared.

## Configure Kubeadm and create the cluster

Login as `debian` to the new Kubernetes master node, and move to `root` using `sudo -s`.

1. Initialize Kubernetes and install a CNI

   Add any parameters required for your CNI of choice.

   ```bash
   kubeadm init --control-plane-endpoint ${K8S_PUBLIC_APISERVER_DNSNAME}:6443 --upload-certs

   # Install your choice of CNI

   ```

1. Install the Proxmox CCM provider

Prepare the following files: 

* From the kubernetes master node: `/etc/kubernetes/admin.conf` 

  The credentials to connect to Kubernetes, needed to allow Helm to connect to
  Kubernetes.

* From your Proxmox host: `/etc/pve/pve-root-ca.pem`

  The certificate authority that signs the Proxmox API certificate, needed to
  connect to the Proxmox API.

* From your loadbalancer: `/etc/lbmanager/lbctl.pem`, `/etc/lbmanager/lbctl-key.pem`, and `/etc/lbmanager/ca.pem`

  The mTLS credentials needed to connect to the load-balancer manager

Install the Proxmox cloud provider with the following helm command:
```bash
helm install  --kubeconfig admin.conf proxmox-ccm  ./chart \
        --namespace kube-system  \
        --set lbmanager.hostname=$LB_MNG_IP \
        --set-file lbmanager.tls.key=lbctl-key.pem   \
        --set-file lbmanager.tls.cert=lbctl.pem \
        --set-file lbmanager.tls.caCert=ca.pem \
        --set proxmox.apiToken=$PROXMOX_API_TOKEN \
        --set proxmox.user=$PROXMOX_API_USERNAME  \
        --set proxmox.apiURL=https://$PROXMOX_HOSTNAME:8006/api2/json \
        --set-file proxmox.caCert=pve-root-ca.pem
```


