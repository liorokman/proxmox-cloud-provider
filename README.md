
# Proxmox Cloud Controller Provider

This projects provides all that is needed to create Kubernetes clusters on
Proxmox where the Kubernetes clusters hosts are in their own Proxmox isolated
SDN VNet.

The Proxmox SDN functionality allows creating an isolated network for the
Kubernetes nodes, and this project provides the glue required to add a load balancer 
and allow Kubernetes to configure it when `LoadBalancer` services are created.


Further reading: [architecture](docs/architecture.md), the
[configuration reference](docs/configuration.md) (chart values, `lbmanager.yaml`,
`lbctl`, the bootstrap script) and the [road-map](docs/roadmap.md).

# Tested versions

| Component | Version |
|---|---|
| Proxmox VE | 9.2 |
| Kubernetes | 1.37 |
| Node and load balancer OS | Debian 13 (trixie) |
| Go (to build) | 1.26 |

# Proxmox Setup

The following steps are required to prepare Proxmox for Kubernetes clusters
using this cloud controller provider:

1. Enable the SDN functionality
   
   Full documentation is available [here](https://pve.proxmox.com/pve-docs/chapter-pvesdn.html).

1. Load the IPVS kernel modules on every Proxmox node that can host the load balancer

   The load balancer runs inside a container, which cannot load kernel modules
   by itself. Load them on the host and make sure they are loaded after a reboot:

   ```bash
      printf 'ip_vs\nip_vs_rr\nnf_conntrack\n' > /etc/modules-load.d/ipvs.conf
      modprobe -a ip_vs ip_vs_rr nf_conntrack
   ```

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

   The cloud provider only reads information about virtual machines (and asks
   their QEMU guest agent for the network addresses), so it needs a read-only
   role. To keep one cluster from seeing the VMs of another, put the VMs and
   the load balancer container of the cluster in a Proxmox
   [resource pool](https://pve.proxmox.com/pve-docs/chapter-pveum.html#pveum_pools),
   and give the token its role on that pool only. `Sys.Audit` is also needed on
   `/` for the cluster name and the node status; it doesn't give access to
   any VM. The following creates the pool (`k8s-red` for a cluster called
   `red`), a dedicated user, the roles and a token (with privilege separation
   turned off, so that the token inherits the user's permissions):

   ```bash
   pveum role add K8sCCM -privs "VM.Audit VM.GuestAgent.Audit"
   pveum role add K8sCCMNode -privs "Sys.Audit"
   pvesh create /pools --poolid k8s-red
   pveum user add ccm-red@pve --comment "Kubernetes cloud controller of red"
   pveum acl modify /pool/k8s-red --users ccm-red@pve --roles K8sCCM
   pveum acl modify / --users ccm-red@pve --roles K8sCCMNode
   pveum user token add ccm-red@pve ccm --privsep 0
   ```

   The VMs and the container join the pool when they are created (`-pool k8s-red`
   in the commands below; `pvesh set /pools/k8s-red --vms <ids>` adds existing
   ones). Without a pool the cloud provider falls back
   to finding the node's VM by its name, among every VM it can see, and
   refuses to guess when two VMs have that name; a token with the roles on `/`
   still works.

   > **Every node's VM must be in the pool.** The token can't see a VM outside
   > it, so the cloud provider reports that the VM doesn't exist, and
   > Kubernetes' node lifecycle controller then **deletes the Node object**,
   > even though the VM is running. A node that is joining never finishes
   > registering, and keeps its `node.cloudprovider.kubernetes.io/uninitialized`
   > taint. This includes nodes you add later, by hand, with a script or with
   > an autoscaler: add the VM to the pool first, when you create it
   > (`qm clone ... --pool k8s-red`), or afterwards (`pvesh set /pools/k8s-red
   > --vms <VM ID>`). The bootstrap script does this for its own VMs, and `up`
   > adds any that are missing; it doesn't know about VMs that you made
   > yourself. If that is a problem, grant the roles on `/` instead of the
   > pool, as in earlier versions.

   Set `PROXMOX_API_TOKEN` to the generated token value, and set
   `PROXMOX_API_USERNAME` to the full token ID. In this example, the username
   would be `ccm-red@pve!ccm`.

# Creating a Kubernetes Cluster

## Quick start: the bootstrap script

`scripts/bootstrap-cluster.sh` does everything in the rest of this chapter on
a Proxmox node: the SDN zone, VNet and subnet, the load balancer container, the
cloud provider's API token, the control plane and worker VMs, `kubeadm`, the
CNI (flannel) and the cloud provider. It takes about ten minutes.

You need the [VM template](#prepare-a-vm-template-for-kubernetes-nodes) and the
Proxmox setup above (a storage that allows snippets, and a spare range of LAN
addresses). Run it as root on the Proxmox node, from a checkout of this
repository:

```bash
cp scripts/cluster.env.example cluster.env     # edit it
scripts/bootstrap-cluster.sh -c cluster.env up
scripts/bootstrap-cluster.sh -c cluster.env status
scripts/bootstrap-cluster.sh -c cluster.env destroy --yes
```

The VMs and the container of a cluster are put in a pool, `k8s-<CLUSTER_NAME>`,
and the cluster's token (of the user `ccm-<CLUSTER_NAME>@pve`) can only see
the guests in it. They also get the Proxmox tags `k8s` and the cluster name
(`TAGS`, see [Proxmox tags as node labels](#proxmox-tags-and-node-labels)). With `HA=yes` on a Proxmox
cluster of two nodes or more, the VMs and the container become HA resources. With three
nodes or more, anti-affinity rules also keep the load balancer container away from the
control plane VM and the workers away from each other, as far as they still fit on the
nodes that remain after one node fails. Proxmox treats these rules as strict: if the
only surviving node holds a guest that a failed guest must be kept away from, the HA
manager does not recover it at all, so on two nodes there are no rules. The control planes are
the exception: with three or more, one per node, so that a node failure leaves the etcd majority
standing (the lost control plane VM then waits for its node).

`CONTROL_PLANES=3` (or any odd number) adds control plane VMs to the cluster; the API server
address stays the load balancer's. With a single control plane VM, a Proxmox node failure takes
the API server down until the HA manager has recovered that VM. More control planes only help
when they are on different Proxmox nodes: with two Proxmox nodes one of them holds at least
half of the etcd members, so three Proxmox nodes are the minimum for an API server that
survives a node failure.

Put the control planes' disks on storage that is fast and local to each Proxmox node
(`CP_STORAGE`): etcd syncs its log on every write, and on Ceph over hard disks, or on NFS, the
sync takes seconds at times, the API server's health checks fail and writes time out. With
`CP_STORAGE` the control planes are created on separate nodes and are not HA resources, since
their disks are not reachable from the other nodes; etcd's own replication is their HA.

HA needs storage that all the nodes can reach (shared storage, or replicated storage
such as ZFS) for `STORAGE`, and `SNIPPET_STORAGE` and `LB_TEMPLATE_STORAGE` too, and the
IPVS kernel modules on every node, which `up` loads. The `qm` and `pct` calls of the
script follow the guests to the node that runs them, so `up` can be repeated after
the HA manager moved something.

`up` can be repeated: every step skips what exists already, so a failed run
resumes where it stopped. It writes the cluster's kubeconfig to
`~/.kube/<CLUSTER_NAME>.yaml` (pointing at the API server's virtual IP). The
Helm client is downloaded when the node has none. Every cluster gets its own
Proxmox API token (`ccm-<CLUSTER_NAME>`), whose secret is kept in
`/var/lib/pve-k8s-bootstrap`; the script never replaces a token that it didn't
create. `destroy` deletes the VMs, the container, the VNet and that token.

Limits: one control plane node, `/24` node subnets, and a VXLAN zone. The steps
below are what the script does, if you want to do them by hand.

## Required Information

1. Choose a range of VM IDs to be used for the Kubernetes
   cluster. In this README, all Kubernetes related VMs and container will be in
   the range 1100-1200 .

1. Allocate a management IP address in the main network bridge (`vmbr0`) for
   the loadbalancer. 

   This IP will be used for connecting to the load-balancer container for
   management purposes. Called `$LB_MGMT_IP` in this README.

1. Allocate an IP address in the main network bridge (`vmbr0`) for the
   Kubernetes API server. Called `$LB_EXT_IP` in this README.

1. Choose the networks used by the cluster, and make sure that none of them overlap
   with your LAN (the main network bridge) or with each other:

   * The subnet of the Kubernetes nodes in the `k8s` VNet (for example `192.168.50.0/24`).
   * The pod network CIDR (`--pod-network-cidr` for `kubeadm`, for example `10.244.0.0/16`).
   * The service CIDR (`kubeadm` defaults to `10.96.0.0/12`).

1. Find out the main network's CIDR and gateway. Respectivly `$EXT_CIDR` and
   `$EXT_GW`.

1. Reserve a range of IPs for the loadbalancer to allocate when services are
   created. Called `$LB_EXT_START` and `$LB_EXT_END` respectively.

   For example, the following allocates 150 IPs for the loadbalancer 
   ```bash
      LB_EXT_START=192.168.50.50
      LB_EXT_END=192.168.50.200
   ```

   **Exclude this range from the DHCP server of your LAN**, and do not use it
   for anything else. The load balancer allocates addresses on its own and
   nothing else on the network knows about them. It also refuses addresses
   outside the range (and the container's own addresses) when a Service asks
   for a specific `loadBalancerIP`.

## The load balancer appliance

Instead of the stock Debian container and hookscript described in the next
section, you can build a container template once, with `lbmanager`, `lbctl` and
all the packages installed, and the network set up by systemd units inside it:

```bash
scripts/build-appliance.sh      # -> <template storage>/vztmpl/lbmanager-<version>_amd64.tar.zst
```

The build downloads `lbmanager` from the release (verifying `SHA256SUMS`), or
uses your own binaries with `LBMANAGER_BIN_DIR=/path`. The build container needs
DHCP and internet access on `BUILD_BRIDGE` (default `vmbr0`). The bootstrap
script builds the template by itself when it is missing (`LB_MODE=appliance`, the
default; `LB_MODE=hookscript` keeps the older way).

To create the container by hand, use the same three interfaces as in the next
section, plus a volume for the state (the CA and the database), then give it its
settings and start the units:

```bash
pct create 1100 local:vztmpl/lbmanager-v0.3.1_amd64.tar.zst -unprivileged 0 -pool k8s-red \
    -hostname k8slb1 -memory 512 -features nesting=1 -rootfs $STORAGE:4 \
    -mp0 $STORAGE:1,mp=/var/lib/lbstate,backup=1 \
    -net0 name=eth0,bridge=vmbr0,ip=$LB_MGMT_IP/$EXT_CIDR,gw=$EXT_GW \
    -net1 name=eth1,bridge=k8s,ip=192.168.50.1/24 \
    -net2 name=eth2,bridge=vmbr0
pct start 1100
printf 'EXT_IP=%s\nEXT_MASK=%s\nRANGE_START=%s\nRANGE_END=%s\n' $LB_EXT_IP 24 $LB_EXT_START $LB_EXT_END > /tmp/appliance.env
pct push 1100 /tmp/appliance.env /etc/lbmanager/appliance.env
pct exec 1100 -- systemctl start lb-network lbmanager
```

After that, the container starts by itself, and survives reboots without any
hookscript. Upgrading `lbmanager` means building a newer template and replacing
the container, moving the `/var/lib/lbstate` volume across (untested; back it up
first). The appliance has no SSH server and no root password: use `pct enter`.

## Setup a LoadBalancer LXC Container

Each Kubernetes cluster requires a lightweight LXC container that will be used
to configure an `IPVS` based load-balancer. 

 > Note: Every cluster has its own load balancer container, with its own VNet and
address range. Several clusters on one Proxmox node work side by side (the
bootstrap script does this), but one container does not serve several clusters.

1. Create a subnet for the new cluster in the `k8s` VNet. Enable SNAT, and set
   the gateway IP to the first IP in the subnet.

   For this README, the the subnet is `192.168.50.0/24`, and the gateway IP is
   `192.168.50.1`.

1. Download the latest Debian template.

   As of this writing, that template was version 13.6-1.

   ```bash
      pveam update
      pveam download local debian-13-standard_13.6-1_amd64.tar.zst
   ```

1. Create the container

   The container is also the default gateway to the main network bridge, so the
   `net1` interface should use the same IP that was allocated for the gateway
   in the Kubernetes nodes subnet.

   Make sure to run the container as a privileged container - this is required
   for `ipvs` to work from inside the container.

   The container has three network interfaces: `eth0` for management (the
   address of `lbctl` and the gRPC API, with its own `$LB_MGMT_IP`), `eth1` in
   the Kubernetes nodes subnet, and `eth2` on the main network. `eth2` is moved
   into the load balancer's network namespace and carries `$LB_EXT_IP` and all
   the virtual IPs, so it has no address in the Proxmox configuration.
   (Without an `eth2`, the script falls back to the older layout: `eth0` has
   `$LB_EXT_IP` and is bridged into the namespace.)

   ```bash
      DESCRIPTION="{\"ip\":\"$LB_EXT_IP\",\"mask\":24,\"start\":\"$LB_EXT_START\",\"end\":\"$LB_EXT_END\"}"
      pct create 1100  local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst  \
          -unprivileged 0 -pool k8s-red -tags "k8s;red;k8s-lb" \
          -cores 2 -swap 0 -memory 512 -hostname k8slb1  \
          -net0 name=eth0,bridge=vmbr0,firewall=1,ip=$LB_MGMT_IP/$EXT_CIDR,gw=$EXT_GW,type=veth \
          -net1 name=eth1,bridge=k8s,firewall=1,ip=192.168.50.1/24,type=veth \
          -net2 name=eth2,bridge=vmbr0,firewall=1,type=veth \
          -ostype debian  -features nesting=1 \
          -password=$PASSWORD  -storage $STORAGE \
          -description $DESCRIPTION
   ```

1. Copy the configuration hookscript `scripts/installLB.sh` to
   `$SNIPPET_STORAGE/snippets` on the Proxmox node where the load balancer
   container was created, and make it executable.

   The script downloads the `lbmanager` and `lbctl` binaries from the GitHub
   release named by `LBMANAGER_VERSION` (edit the default at the top of the
   script), and refuses them unless they match the release's `SHA256SUMS`.
   Packages are only installed when they are missing, and nothing is upgraded.
   The download only happens when the container has no binary yet.

   To use binaries that you built yourself instead (`go build ./cmd/lbmanager
   ./cmd/lbctl`), place `lbmanager` and `lbctl` in the same snippets
   directory, next to the script. They are copied into the container on every
   start and take precedence over the download. Do this for any version that
   is newer than the latest release.

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

1. Check that it works.

   ```bash
      pct exec 1100 -- systemctl is-active lbmanager
      pct exec 1100 -- /usr/local/bin/lbctl -op list
      curl -s http://$LB_MGMT_IP:9100/metrics | head
   ```

   The container's `lbmanager.yaml` is generated by the hookscript on every
   start (edit the hookscript, not the file). The hookscript turns on the
   Prometheus endpoint, `http://$LB_MGMT_IP:9100/metrics`, without
   authentication. Only the management network can reach it.

## Prepare a VM Template for Kubernetes Nodes

Follow the instructions in [this](https://github.com/liorokman/packer-Debian#use-in-proxmox)
Github repository to prepare a template that can be used for creating Kubernetes nodes.

The image is built from Debian 13 with `kubeadm`, `kubelet`, and `kubectl` version 1.37, and a
containerd that is configured for the standard CNI plugin directory (`/opt/cni/bin`).

> Note: When creating the template, use `-rng0 source=/dev/urandom` unless the Proxmox host
> has a hardware random number generator. Virtual machines with `/dev/hwrng` will refuse to
> start on hosts that do not have one.

This README assumes that the template's ID is `$K8S_TEMPLATE_ID`

## Create and start the first node 

1. Clone the template

   Choose an unused ID for the new VM. This section of the README assumes `$K8S_NEW_ID` is 
   set to to the ID.

   ```bash
   qm clone $K8S_TEMPLATE_ID $K8S_NEW_ID -full -name k8s-master1 -storage $STORAGE -pool k8s-red
   qm set $K8S_NEW_ID -tags "k8s;red"
   ```

1. Update the cloud-init parameters
   1. Set a valid ssh key for the `debian` user

   ```bash
   qm set $K8S_NEW_ID -sshkeys /path/to/ssh/public/key/file
   ```

   1. Set a valid IP address. The default gateway should be the loadbalancer's
      ip in the `k8s` subnet (`192.168.50.1` in this README)

   ```bash
   qm set $K8S_NEW_ID -ipconfig0 ip=192.168.50.3/24,gw=192.168.50.1
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
   kubeadm init --control-plane-endpoint ${K8S_PUBLIC_APISERVER_DNSNAME}:6443 \
                --pod-network-cidr 10.244.0.0/16 --upload-certs

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
        --set lbmanager.hostname=$LB_MGMT_IP \
        --set-file lbmanager.tls.key=lbctl-key.pem   \
        --set-file lbmanager.tls.cert=lbctl.pem \
        --set-file lbmanager.tls.caCert=ca.pem \
        --set proxmox.apiToken=$PROXMOX_API_TOKEN \
        --set proxmox.user=$PROXMOX_API_USERNAME  \
        --set proxmox.apiURL=https://$PROXMOX_HOSTNAME:8006/api2/json \
        --set-file proxmox.caCert=pve-root-ca.pem
```

The image defaults to `ghcr.io/liorokman/proxmox-cloud-provider` at the chart's
`appVersion`. Use `--set image.tag=...` for another one. The chart gives the
cloud provider a narrow cluster role (nodes, services, events, leases), not
`cluster-admin`.

Then create a `LoadBalancer` Service. The Service gets an address from the
reserved range, which the load balancer then answers for on the LAN:

```bash
kubectl create deployment web --image=nginx
kubectl expose deployment web --type=LoadBalancer --port=80
kubectl get svc web
```

For `externalTrafficPolicy: Local` Services, the load balancer sends traffic
only to nodes whose `healthCheckNodePort` answers `/healthz` successfully.

## Proxmox tags and node labels

The cloud provider sets these labels on every node that is backed by a Proxmox VM:

| Label | Value |
|---|---|
| `topology.kubernetes.io/region` | the name of the Proxmox cluster |
| `topology.kubernetes.io/zone` | the Proxmox node that runs the VM |
| `node.kubernetes.io/instance-type` | the size of the VM, for example `4c-8g` |
| `tag.proxmox.liorokman.github.io/<tag>` | `true`, for each [tag](https://pve.proxmox.com/pve-docs/chapter-qm.html#qm_options) of the VM |

Tag a VM with `qm set 1102 --tags "gpu;fast-disk"` and the node gets the labels
`tag.proxmox.liorokman.github.io/gpu=true` and `.../fast-disk=true`, which `nodeSelector`
and affinity rules can use. Tags that are not valid label names (for example with a `+`)
are ignored.

Kubernetes sets labels from the cloud provider only when a node registers. A VM that moves
to another Proxmox node (a migration, or the HA manager balancing the load), is resized,
or is tagged later would keep stale labels, so the cloud provider also compares
all nodes with their VMs every `proxmox.labelSyncInterval` seconds (default 60, set a negative
value to turn it off) and updates the labels. Only the labels in the table are touched; labels
you add yourself are kept, and a tag that is removed from the VM loses its label.

The provider ID of new nodes is `proxmox://<cluster name>/<VM ID>`. Nodes that registered
with an earlier version have `proxmox:///<Proxmox node>/<VM ID>`, which is still understood
(a node keeps its provider ID for life).

# Persistent volumes: the Proxmox CSI plugin

Kubernetes volumes backed by Proxmox storage (LVM, LVM-thin, ZFS, directory, Ceph, NFS)
come from [sergelogvinov/proxmox-csi-plugin](https://github.com/sergelogvinov/proxmox-csi-plugin),
which this project doesn't include. It works with clusters made with this project, because the
cloud provider sets what the plugin needs on the nodes: the `topology.kubernetes.io/region` label (the
Proxmox cluster name), the `topology.kubernetes.io/zone` label (the Proxmox node), and a provider ID of the form
`proxmox://<cluster name>/<VM ID>`. Tested with plugin v0.22.0 (chart 0.5.15) on Proxmox VE 9.2 and
Kubernetes 1.37: dynamic provisioning on an LVM storage, a pod that moves to another worker and
finds its data, online expansion, and deleting the claim removes the disk. The same on Ceph (an RBD
pool as the storage): a pod that moves to a worker on another Proxmox node finds its data, because
the plugin detaches the disk from one VM and attaches it to the other.

The plugin attaches the volumes to the VMs as extra SCSI disks, so the VMs need the
`virtio-scsi-single` (or `virtio-scsi-pci`) SCSI controller, which the VMs made by the bootstrap script have. Local storage
(LVM, ZFS, ...) is bound to one Proxmox node: a pod can only move between VMs on that node. Shared storage
(Ceph, NFS) has no such limit.

1. Check the region the cloud provider chose; it goes into the plugin's configuration.

   ```bash
   kubectl get nodes -L topology.kubernetes.io/region,topology.kubernetes.io/zone
   ```

   A Proxmox host that is not part of a Proxmox cluster has no cluster name, and its region is `proxmox`.
   Nodes that registered with v0.3.0 or earlier (a provider ID of the form `proxmox:///<node>/<VM ID>`)
   work too (tested): the plugin logs "failed to get proxmox VMID from ProviderID" and finds the VM by
   the node's name, which is the VM's name. The region label is what has to match.

1. Create a Proxmox user for the plugin, with a role that only works on the cluster's pool and
   on the storage the volumes are created in (`vms` here). The plugin's documentation grants the
   role on `/`, which lets it attach disks to every VM.

   ```bash
   pveum role add K8sCSI -privs "VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit"
   pveum user add csi-red@pve --comment "Proxmox CSI of red"
   pveum acl modify /pool/k8s-red --users csi-red@pve --roles K8sCSI
   pveum acl modify /storage/vms --users csi-red@pve --roles K8sCSI
   pveum user token add csi-red@pve csi --privsep 0
   ```

   The volumes show up as disks of a VM with the number 9999 (the plugin's `controllerVMID`), which must not
   be a real VM.

1. Install the plugin. The namespace needs the `privileged` Pod Security level, and the plugin takes the
   token and the region from its Helm values:

   ```bash
   cat > proxmox-csi.yaml <<EOF
   config:
     clusters:
       - url: https://$PROXMOX_HOSTNAME:8006/api2/json
         insecure: false            # see the note below
         token_id: "csi-red@pve!csi"
         token_secret: "<the token value>"
         region: <the region of the nodes>
   storageClass:
     - name: proxmox-lvm
       storage: vms
       reclaimPolicy: Delete
       fstype: ext4
       cache: writethrough
   EOF
   kubectl create ns csi-proxmox
   kubectl label ns csi-proxmox pod-security.kubernetes.io/enforce=privileged
   helm upgrade -i -n csi-proxmox -f proxmox-csi.yaml proxmox-csi-plugin \
        oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin
   ```

   > Note: the plugin can't be given a CA certificate. With the Proxmox certificate from the cluster's own CA
   > (`/etc/pve/pve-root-ca.pem`, which the cloud provider gets in `proxmox.caCert`), set `insecure: true`
   > (the token is then sent to whatever answers at that address), or install a certificate on Proxmox that
   > the plugin image already trusts (for example from Let's Encrypt, through the ACME support of Proxmox).

1. Use it:

   ```bash
   kubectl apply -f - <<EOF
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata: {name: data}
   spec:
     accessModes: [ReadWriteOnce]
     storageClassName: proxmox-lvm
     resources: {requests: {storage: 2Gi}}
   EOF
   ```

   For Ceph, use `storage: <the pool's storage>` in the storage class. The plugin has one Proxmox
   endpoint (`url`): while that node is down the volumes can't be created, attached or detached.

   The storage class uses `WaitForFirstConsumer`: the disk is created when a pod that uses the claim is scheduled, on
   the Proxmox node of the worker that gets it.

# Upgrading

The cloud provider (in the cluster) and `lbmanager` (in the container) talk
over a versioned API, so **upgrade them together**: a cloud provider that uses
the `Apply` call needs an `lbmanager` that has it. Upgrade `lbmanager` first;
it still serves the older calls, so the old cloud provider keeps working until
the chart is upgraded. Load balancers and their state are kept in
`/var/lib/lbmanager` in the container and survive the upgrade.

1. Read the release notes for changes that need manual steps. For the changes
   since v0.3.0 see "Notes for upgrading from v0.3.0", since v0.2.0 see "Notes for upgrading from v0.2.0",
   and since v0.1.0 see "Notes for upgrading from v0.1.0", below.

1. Upgrade `lbmanager` and `lbctl`.

   * With binaries next to the hookscript: replace `lbmanager` and `lbctl` in
     the snippets directory, and replace `installLB.sh` with the new version.
   * With the download: set the new `LBMANAGER_VERSION` in the script. The
     binaries are only downloaded when the container has none, so delete the
     old ones from the container (`pct exec 1100 -- rm /usr/local/bin/lbmanager
     /usr/local/bin/lbctl`) before the restart.

   Then restart the container so the hookscript runs again:

   ```bash
      pct reboot 1100
   ```

   `lbmanager` re-reads its state on start and recreates the IPVS services.
   The restart interrupts traffic through the container (including the
   nodes' internet access) for a few seconds. Restarting only the service
   (`systemctl restart lbmanager`) is not enough to pick up a new hookscript.

1. Upgrade the cloud provider with the same values you installed with:

   ```bash
      helm upgrade --kubeconfig admin.conf proxmox-ccm ./chart \
           --namespace kube-system --reuse-values
   ```

   Add `--set image.tag=<new version>` when the chart's `appVersion` is not
   the version you want. When the chart is installed from the repository,
   `git pull` first. `--reuse-values` keeps the credentials you gave at
   install time; without it, you must repeat all the `--set` options.

1. Check that everything works.

   ```bash
      kubectl -n kube-system get ds,pods | grep proxmox
      kubectl -n kube-system logs ds/proxmox-ccm-proxmox-cloud-provider | tail
      kubectl get svc -A | grep LoadBalancer
   ```

   There should be no `Forbidden` or `Unavailable` errors, and each Service
   keeps the same address.

To roll back, put the previous binaries and hookscript back, reboot the
container, and run `helm rollback proxmox-ccm`. Rolling back has not been
tested across versions; back up `/var/lib/lbmanager` (or take a container
snapshot with `pct snapshot`) before upgrading.

## Notes for upgrading from v0.3.0

The API between the cloud provider and `lbmanager` is unchanged, so the order of the upgrade
doesn't matter. Nothing needs to be done by hand.

* **Node labels follow the VM:** the zone, region and instance type labels are kept up
  to date (every `proxmox.labelSyncInterval` seconds), and the VM's Proxmox tags become
  labels. See "Proxmox tags and node labels". The cloud provider's cluster role already allows
  patching nodes.
* **Provider ID:** new nodes get `proxmox://<cluster name>/<VM ID>` (the form the
  Proxmox CSI plugin expects). Existing nodes keep theirs.
* **Ambiguous names:** when a node has no provider ID and several VMs have its name, the
  cloud provider now reports an error instead of choosing one.
* **Scoped token (optional):** the token can be restricted to the VMs of the cluster, see
  "Prepare an API token". Existing tokens with the roles on `/` keep working. With a scoped
  token, every node's VM must be in the pool, or Kubernetes deletes its Node object.
* **Bootstrap script:** new clusters get a pool and their own Proxmox user,
  `ccm-<CLUSTER_NAME>@pve`, instead of the shared `ccm@pve`. Set `PVE_USER=ccm@pve` in the settings
  file of a cluster that was built before, or its next `up` creates a new user and token.
  `up` puts the existing VMs in the new pool.
* **Bootstrap script on several Proxmox nodes:** `qm` and `pct` follow the guests to the node that
  runs them, the IPVS modules are loaded on every node, the nodes that joined the Proxmox cluster
  later are added to the peers of the VXLAN zone, and the HA step runs last. New settings:
  `CONTROL_PLANES` (more than one control plane), `CP_STORAGE` and `CP_NODES` (control plane disks
  on storage that is local to each node, see "Quick start: the bootstrap script"). Anti-affinity rules are only
  created where recovery after a node failure still works. Nothing changes for a cluster that was
  built before.
* **One Proxmox endpoint:** the cloud provider talks to the single `proxmox.apiURL`. While that node
  is down it can't start, update labels or manage load balancers. Point it at a node that is up, or
  upgrade the release (see the roadmap).

## Notes for upgrading from v0.2.0

v0.3.0 does not change the API between the cloud provider and `lbmanager`, so
upgrading `lbmanager` first, as described above, is safe: a v0.2.0 cloud provider
works with a v0.3.0 `lbmanager` (tested). The other way around is untested, but
uses the same API. Nothing needs to be done by hand.

* **IPVS library:** `lbmanager` uses `github.com/cloudflare/ipvs` instead of a
  copy of `github.com/moby/ipvs`. Existing load balancers are re-created from
  the database on start, as before.
* **Appliance (optional):** see "The load balancer appliance". A hookscript
  based container keeps working; you don't have to move to the appliance.
* **Bootstrap script (optional):** `scripts/bootstrap-cluster.sh` builds a
  whole cluster. It doesn't touch existing ones.
* **Hookscript:** `installLB.sh` reads the container settings through the
  Proxmox API (`pvesh`), so replace it together with the binaries.
* **Fixed:** when a virtual IP moved to a container with another MAC address
  (a replaced load balancer), LAN hosts kept using the old MAC for several
  seconds. `lbmanager` now announces every virtual IP it adds with a gratuitous
  ARP.
* **Fixed:** a new `LoadBalancer` Service used to refuse connections for about
  7 seconds, because its targets were probed before kube-proxy opened the
  NodePort and then needed two probes five seconds apart to come back. Targets
  that are down are now probed every second (`loadbalancer.healthCheck.downInterval`).
* **Fixed:** an unusable CA file used to give a nil credential without an error
  in the cloud provider, `lbctl` and `lbmanager`.

## Notes for upgrading from v0.1.0

* **Proxmox role:** the token needs one more privilege, `VM.GuestAgent.Audit`,
  for the node addresses that the QEMU guest agent reports. Without it the
  cloud provider logs "not authorized" and uses the static cloud-init
  addresses.

  ```bash
     pveum role modify K8sCCM -privs "VM.Audit Sys.Audit VM.GuestAgent.Audit"
  ```

  The VMs need the guest agent installed and enabled (`agent: 1`).
* **Chart RBAC:** the chart's ClusterRole and ClusterRoleBinding have new names
  and the cloud provider no longer runs as `cluster-admin`. `helm upgrade`
  replaces the old objects; nothing else is needed. If you added your own
  permissions to the cloud provider, add them again.
* **Third network interface (optional):** the layout with a third NIC
  (`eth2` on the main network, no address in Proxmox) replaces the bridge that
  the old script built inside the container. To switch, stop the container and
  add `-net2 name=eth2,bridge=vmbr0,firewall=1,type=veth`. `$LB_EXT_IP` and the
  virtual IPs then move to `eth2` and `eth0` only keeps the management
  address. The old layout still works if you do nothing. Expect a short
  outage.
* **IPv6 is disabled** on the container's external interface.
* **Address ranges:** explicit `loadBalancerIP` addresses are now accepted only
  from the dynamic range (and an optional `staticRange` in `lbmanager.yaml`).
  A Service that asked for an address outside the range before will not be
  created again. Existing load balancers keep their address.
* **Node labels:** `topology.kubernetes.io/region` and
  `node.kubernetes.io/instance-type` are now set from the Proxmox cluster name
  and the VM size, but only when a node registers. Existing nodes keep the old
  labels; to update one, relabel it by hand (`kubectl label node ... --overwrite`).
* **Metrics** on `:9100` are enabled by the new hookscript only.



# Development

The project has a [Taskfile](https://taskfile.dev) (`task` lists the tasks):

```bash
task build                          # the three binaries, into ./bin
task test                           # the tests that don't need root
task test:privileged HOST=devstack  # also run every package as root on that host (ssh)
task lint                           # golangci-lint, configured in .golangci.yml
task vuln                           # govulncheck
task check                          # vet, test, lint, vuln, and the chart and shell script checks
```

`lint` and `vuln` run the tools at the versions pinned in the Taskfile, with `go run`, so
nothing needs to be installed. Without `task`, the same is `go build ./...`, `go vet ./...`,
`go test ./...` and `scripts/test-privileged.sh devstack`.

Most of the load balancer is only exercised by tests that configure IPVS in a
private network namespace, which needs real root (the kernel does not allow it
from a user namespace) and the `ip_vs` module. `scripts/test-privileged.sh`
builds the test binaries here, runs them as root on the host you give it (a
Proxmox node, or any Linux machine with `modprobe ip_vs ip_vs_rr`), and prints
the combined coverage, without counting the generated protobuf code. Without
root those tests skip themselves.

IPVS is programmed through [github.com/cloudflare/ipvs](https://github.com/cloudflare/ipvs)
(`internal/ipvsns` opens it inside the load balancer's network namespace). The
load balancer used to carry a copy of `github.com/moby/ipvs`, which has not had
a release since January 2023.

# License

Apache License, Version 2.0, see [LICENSE.txt](LICENSE.txt). The copyright of
the original work is held by Lior Okman. Files that contain material from other
projects keep those projects' notices; see [NOTICE](NOTICE).
