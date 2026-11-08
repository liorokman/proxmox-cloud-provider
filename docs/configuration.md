# Configuration reference

Everything that can be configured, by component. The setup steps are in the
[README](../README.md), the design in [architecture.md](architecture.md).

## Cloud provider (the Helm chart)

The chart renders the cloud provider's `cloud-config.yaml` from these values
(`chart/values.yaml`). Values without a default are required.

| Value | Default | |
|---|---|---|
| `lbmanager.hostname` | | Management address of the load balancer container |
| `lbmanager.additionalHostnames` | none | The management addresses of the other containers of a load balancer pair (`LB_REPLICAS=2`). The first one picks the address of a new Service; every Service is applied to all of them, and a Service that can't reach one of them is retried |
| `lbmanager.port` | `9999` | gRPC port of `lbmanager` |
| `lbmanager.tls.key`, `.cert`, `.caCert` | | mTLS credentials (PEM) of the `lbctl` client of the container |
| `proxmox.apiURL` | | For example `https://pve:8006/api2/json` |
| `proxmox.user` | | The full token ID, for example `ccm-red@pve!ccm` |
| `proxmox.apiToken` | | The token's secret |
| `proxmox.caCert` | | The CA that signed the Proxmox API certificate (`/etc/pve/pve-root-ca.pem`) |
| `proxmox.insecure` | `false` | Skip verifying the Proxmox certificate |
| `proxmox.timeout` | `5` | Seconds, per Proxmox API call |
| `proxmox.labelSyncInterval` | `60` | Seconds between updates of the node labels from the VMs. Negative: off |
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/liorokman/proxmox-cloud-provider`, the chart's `appVersion`, `IfNotPresent` | |
| `serviceAccount.create`, `.name`, `.annotations` | `true` | |
| `resources`, `nodeSelector`, `podAnnotations`, `podSecurityContext`, `securityContext`, `imagePullSecrets` | | Passed to the DaemonSet. The pods run on the control plane nodes |

The Proxmox token needs `VM.Audit` and `VM.GuestAgent.Audit` on the VMs (best on the
cluster's resource pool) and `Sys.Audit` on `/`.
With the roles on a pool, every node's VM must be in that pool: a VM the token can't see is
reported as gone, and Kubernetes deletes its Node object (see the README).

### Node labels

| Label | Source |
|---|---|
| `topology.kubernetes.io/region` (and the beta `failure-domain`) | The Proxmox cluster name, `proxmox` for a host that isn't in a cluster |
| `topology.kubernetes.io/zone` (and the beta one) | The Proxmox node that runs the VM |
| `node.kubernetes.io/instance-type` (and the beta one) | `<cpus>c-<memory>g`, for example `4c-8g` |
| `tag.proxmox.liorokman.github.io/<tag>` = `true` | One for each Proxmox tag of the VM |

The provider ID is `proxmox://<cluster name>/<VM ID>` (older nodes: `proxmox:///<Proxmox node>/<VM ID>`).

## lbmanager (`/etc/lbmanager/lbmanager.yaml`)

`lbmanager` reads `/etc/lbmanager/lbmanager.yaml`, then `lbmanager.yaml` in its
working directory. The appliance writes the file from `/etc/lbmanager/appliance.env`
(`lb-prepare`), the hookscript on every start. Changes need a restart.

| Key | Default | |
|---|---|---|
| `grpc.listen`, `grpc.port` | | Address and port of the API (the management address, 9999) |
| `grpc.auth.cert`, `.key`, `.ca` | | The server's certificate and key, and the CA that signs the clients' certificates (mutual TLS) |
| `dbDir` | | Where the load balancers are stored (the state volume in the appliance) |
| `metrics.listen` | off | `address:port` of the Prometheus endpoint (`/metrics`), without authentication |
| `loadbalancer.namespace` | | Network namespace of the data plane (`LB`) |
| `loadbalancer.externalInterface` | | The LAN interface in the namespace (`eth2`); carries the virtual IPs |
| `loadbalancer.internalInterface` | | The interface of the nodes' network (`eth1`) |
| `loadbalancer.ipam.cidr` | | The LAN, with the container's own address: `192.168.1.51/24` |
| `loadbalancer.ipam.dynamicRange.startAt`, `.endAt` | | Addresses handed out to Services. Must be inside the CIDR |
| `loadbalancer.ipam.staticRange.startAt`, `.endAt` | none | Optional. Addresses that are only given when a Service asks for them (`loadBalancerIP`) |
| `loadbalancer.keepalived.vipsFile` | off | HA pair: the virtual IPs are listed in this file (`/etc/keepalived/vips.conf`), which keepalived includes, instead of being configured on the interface. keepalived then adds them on the active container only |
| `loadbalancer.keepalived.pidFile` | `/run/lb-keepalived.pid` | keepalived gets SIGHUP from `lbmanager` when the file changes |
| `loadbalancer.keepalived.reserved` | | The container's own addresses; Services can't claim them (they aren't on the interface of the passive container) |
| `loadbalancer.healthCheck.enabled` | `true` | |
| `loadbalancer.healthCheck.interval` | `5s` | Between probes of a healthy target |
| `loadbalancer.healthCheck.downInterval` | `1s` | Between probes while a target is down |
| `loadbalancer.healthCheck.timeout` | `1s` | |
| `loadbalancer.healthCheck.fall` | `3` | Failed probes in a row before a target gets weight 0 |
| `loadbalancer.healthCheck.rise` | `2` | Successful probes in a row before it gets weight 1 again |

Exclude both ranges from the DHCP server of the LAN. UDP targets are not probed.

## lbctl

`lbctl` is the client of the `lbmanager` API, on the container (`/usr/local/bin/lbctl`, with
`/etc/lbmanager/lbctl.yaml` for the address and the credentials).

```bash
lbctl -op list
lbctl -op addSrv -name apiserver -srv 192.168.1.100                          # a virtual IP (TCP)
lbctl -op addTgt -name apiserver -srv 192.168.50.3 -sport 6443 -dport 6443   # a target
lbctl -op delTgt -name apiserver -srv 192.168.50.3 -sport 6443 -dport 6443
lbctl -op delSrv -name apiserver
```

| Flag | |
|---|---|
| `-op` | `addSrv`, `delSrv`, `addTgt`, `delTgt`, `list` |
| `-name` | The name of the load balancer |
| `-srv` | The virtual IP (`addSrv`, `delSrv`), or the target's IP (`addTgt`, `delTgt`) |
| `-sport`, `-dport` | The port on the virtual IP, and on the target |
| `-tcp` | `true` (default) for TCP, `false` for UDP |
| `-addr`, `-cert`, `-key`, `-ca` | Server address and mTLS files, when not in `lbctl.yaml` (`addr`, `auth.clientCert`, `auth.clientKey`, `auth.caFile`) |

Load balancers that the cloud provider manages are named `<cluster>%<namespace>%<service>`
(the `apiserver` entry of the bootstrap script is the exception). Don't change them by hand:
the next `Apply` call undoes it.

## Bootstrap script

`scripts/bootstrap-cluster.sh -c cluster.env up|status|destroy --yes`. Every setting, with
its default, is at the top of the script; [`scripts/cluster.env.example`](../scripts/cluster.env.example)
shows the common ones. Settings that are not obvious:

| Setting | Default | |
|---|---|---|
| `CLUSTER_NAME` | `k8s1` | Names the VMs, the VNet, the pool, the user and the token |
| `POOL` | `k8s-<CLUSTER_NAME>` | The cluster's pool; its VMs and container go in it |
| `PVE_USER` | `ccm-<CLUSTER_NAME>@pve` | The cloud provider's user. Deleted by `destroy` when it has this name |
| `PVE_TOKEN_NAME` | `ccm-<CLUSTER_NAME>` | Its secret is kept in `/var/lib/pve-k8s-bootstrap` |
| `PVE_ROLE`, `PVE_NODE_ROLE` | `K8sCCM`, `K8sCCMNode` | The roles on the pool and on `/` |
| `TAGS` | `k8s;<CLUSTER_NAME>` | Proxmox tags of the VMs (they become node labels) and the container |
| `CONTROL_PLANES` | `1` | Control plane VMs (stacked etcd). Use 1 or an odd number: 2 etcd members stop as soon as either is down. The extra ones join through `kubeadm join --control-plane` and become targets of the `apiserver` entry of the load balancer |
| `CP_STORAGE`, `CP_NODES` | | Storage that is local to each Proxmox node for the control plane disks, and the node of each control plane (default: the cluster's nodes by name). etcd needs low fsync latency, which shared or replicated storage on slow disks doesn't give: the API server times out. Control planes on `CP_STORAGE` are not HA resources; with three of them, one per node, a node failure leaves the etcd majority standing |
| `EXTRA_CP_VMID_START`, `EXTRA_CP_IP_START` | `CONTROL_PLANE_VMID + 50`, `NODE_IP_START + 50` | VM ID and last address octet of the second control plane, the others follow. Apart from the workers' numbers, so that adding workers doesn't renumber them |
| `HA` | `no` | `yes`: HA resources; needs 2 or more Proxmox nodes. With 3 or more there are anti-affinity rules too (load balancer away from the control plane, workers from each other), as far as the guests still fit on the nodes left after one node fails. With 3 or more control planes, one per Proxmox node, which keeps etcd's majority through a node failure |
| `LB_MODE` | `appliance` | Or `hookscript` |
| `LB_REPLICAS` | `1` | `2`: a second load balancer container (an active/passive pair: keepalived floats the addresses and the IPVS connection state is synchronized; the cloud provider applies every Service to both). Needs `LB_MODE=appliance`, and `LB_OWN_EXT_IP`, `LB2_OWN_EXT_IP` (each container's own LAN address; `LB_EXT_IP` becomes the shared egress address) and `LB2_MGMT_IP`. `LB2_VMID` defaults to `LB_VMID + 1`. The node network gets the gateway address on whichever container is active, and the containers take the last two addresses (`.253`, `.254`) |
| `IMAGE_TAG`, `IMAGE_ARCHIVE` | | A cloud provider image other than the chart's, or one loaded from a local archive |

## Appliance (`/etc/lbmanager/appliance.env`)

| Key | |
|---|---|
| `EXT_IP`, `EXT_MASK` | The container's own address on the LAN |
| `RANGE_START`, `RANGE_END` | The dynamic range of virtual IPs |
| `EXT_GW` | The LAN's gateway (default: the management interface's) |
| `HA` | `yes`: one container of a pair. `lb-keepalived.service` then runs keepalived in the `LB` namespace (VRRP over unicast on `eth1`, and the IPVS connection synchronization daemon, so established connections survive a switch). Without it, the unit is skipped |
| `HA_EXT_IP` | This container's own LAN address; `EXT_IP` is shared and moves with the active container |
| `HA_PEER`, `HA_PRIORITY`, `HA_GATEWAY`, `HA_VRID` | The other container's address on `eth1`; 150 for the first and 100 for the second; the node network's gateway with its prefix (moves with the active container); VRRP and sync id (`51`) |

## Hookscript container (`scripts/installLB.sh`)

The settings are read from the container's description in Proxmox, as JSON:
`{"ip":"<EXT_IP>","mask":24,"start":"<first VIP>","end":"<last VIP>"}`. `LBMANAGER_VERSION`
(top of the script, or in the environment) is the release to download.
