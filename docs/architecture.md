# Architecture

The project is an out-of-tree Kubernetes cloud controller manager (CCM) for
clusters whose nodes are Proxmox VMs in an isolated SDN VNet. It has two parts
that share one binary only at the Kubernetes side:

* The **cloud provider** (`proxmox-cloud-controller-manager`) runs in the
  cluster and implements the `InstancesV2` and `LoadBalancer` interfaces of
  `k8s.io/cloud-provider`.
* The **load balancer** (`lbmanager`, with the `lbctl` client) runs in an LXC
  container on a Proxmox node and programs the Linux kernel's IPVS.

```
                         LAN (vmbr0, e.g. 192.168.64.0/22)
                                        |
   Proxmox API  <--------------+        | VIPs (192.168.67.101-150)
   (read only token)           |        |
                               |   +----+----------------------------+
   +----------------------+    |   | LXC container (privileged)      |
   | CCM (DaemonSet)      |----+   |  netns "LB":                    |
   |  - InstancesV2       |  mTLS  |   external0  <-> LAN            |
   |  - LoadBalancer      |--gRPC->|   eth1       <-> k8s VNet       |
   +----------------------+  9999  |   IPVS (NAT, rr) + iptables     |
        Kubernetes nodes           |  lbmanager (state in rosedb)    |
   (VNet, e.g. 192.168.20.0/24) ---+---------------------------------+
        default gateway = the container
```

## Instances

`internal/proxmox/instance.go` looks a node up in the Proxmox cluster
resources, first by the `providerID` (`proxmox:///<pve node>/<vmid>`), and
otherwise by the VM name. It reports:

* the provider ID, the Proxmox node as the zone, the Proxmox cluster as the
  region, and an instance type derived from the VM's size (for example
  `4c-8g`). Labels are only set when a node registers;
* the node addresses: the IPv4 addresses that the QEMU guest agent reports on
  the VM's own network interfaces (matched by MAC address, so pod and bridge
  interfaces are ignored). This works with DHCP. Without a running agent (or
  the `VM.GuestAgent.Audit` privilege), the static `ipconfigN` cloud-init
  settings are used. The VM's name is always the hostname;
* whether the VM exists and whether it is running.

`InstanceExists` only returns `false` when the VM does not exist. Any other
failure (for example, the Proxmox API being unreachable) is returned as an
error, since a `false` answer makes Kubernetes delete the Node object.

The Proxmox client is [luthermonson/go-proxmox](https://github.com/luthermonson/go-proxmox).
The API token only needs the `VM.Audit`, `Sys.Audit` and `VM.GuestAgent.Audit` privileges.
The Helm chart gives the cloud provider a narrow cluster role (nodes, services
and events, plus leader election and delegated authentication) instead of
`cluster-admin`.

## Load balancing

Every `LoadBalancer` Service becomes one entry in `lbmanager`:

* a virtual IP, taken from a configured range (or the Service's
  `loadBalancerIP`), added as an address on `external0` in the `LB` network
  namespace;
* one IPVS service per Service port, with the InternalIP of each node and the
  Service's NodePort as destinations, using NAT forwarding and round robin
  scheduling.

The VIP range is configured in `lbmanager.yaml` (`loadbalancer.ipam`): a
`dynamicRange` for automatic allocation, and an optional `staticRange` for
addresses that can only be requested with `loadBalancerIP`. Addresses outside
both ranges, or in use by something else on the interface, are refused. Several
Services can share a VIP on different ports.

`lbmanager` probes every TCP target with a TCP connect from inside the `LB`
namespace. A target that fails `fall` probes in a row gets IPVS weight 0 (no
new connections, existing ones continue), and gets weight 1 again after `rise`
successful probes.

`EnsureLoadBalancer` and `UpdateLoadBalancer` compute the complete set of
wanted targets over all ports and nodes and send it to `lbmanager` in one
declarative `Apply` call. `lbmanager` creates the load balancer if needed, adds
the missing targets, removes everything else, and saves the result in one
step, under its mutex. Repeating a call changes nothing, and a problem with
one port (reported in `PortErrors`) does not stop the others. The older
`Create`, `AddTarget`, `DelTarget` calls remain for `lbctl`.

For `externalTrafficPolicy: Local` each target also carries the Service's
`healthCheckNodePort`. `lbmanager` then checks `GET /healthz` there instead of
a TCP connect to the NodePort, so only nodes with local endpoints get traffic
(weight 0 otherwise, and until the first check passes). Control plane nodes are not targets, because the Kubernetes
service controller does not pass nodes that carry the
`node.kubernetes.io/exclude-from-external-load-balancers` label.

### Network layout

The container is created with two interfaces. `scripts/installLB.sh` is run as
the container's hookscript, and on every start it:

1. creates the `LB` network namespace and moves the VNet interface (`eth1`) in;
2. bridges the LAN interface (`eth0`) to a veth pair whose other end,
   `external0`, is in the namespace;
3. enables forwarding and IPVS connection tracking in the namespace;
4. configures source NAT so the container is the default gateway of the
   cluster to the LAN and the internet;
5. creates a CA, the `lbmanager` and `lbctl` certificates, and a systemd unit.

Traffic from outside the cluster keeps its source address: IPVS rewrites only
the destination, and the replies return through the container because it is
the nodes' default gateway. Connections that start *inside* the VNet (including
the nodes talking to the API server's VIP) are masqueraded so that the reply
also passes through the container.

### Metrics

With `metrics.listen` set in `lbmanager.yaml` (the hookscript uses the
management address, port 9100), `lbmanager` serves Prometheus metrics on
`/metrics`, without authentication: per virtual service the connections,
packets and bytes, and per target its weight (0 = failed its health check),
active and inactive connections and traffic.

### Security

The gRPC API between the cloud provider and `lbmanager` uses mutual TLS with a
CA generated inside the container.

## How this relates to Proxmox SDN

Proxmox VE 9 SDN does not provide load balancing or service VIPs, so IPVS stays
necessary. What SDN can provide in each zone type:

| Zone | SNAT gateway | DHCP | Spans nodes |
|---|---|---|---|
| Simple | yes | yes (IPAM) | no |
| VLAN, QinQ, VXLAN | no, the container is the gateway | no | yes |
| EVPN | exit nodes (tech preview) | no | yes |

The development setup uses a VXLAN zone, where the container is the only
gateway.

## Known limitations

* Health checks are plain TCP connects, and UDP targets are not checked.
* A single load balancer container is both ingress and egress for the
  cluster, so it is a single point of failure.
* The VIP range is managed by `lbmanager` itself and is not known to Proxmox
  IPAM, the DHCP server of the LAN, or other hosts. Pick a range that is
  reserved.
* Only IPv4 and TCP/UDP are tested.
* The cloud provider needs an `lbmanager` that has the `Apply` call (this release or newer).

## Possible next steps

See [roadmap.md](roadmap.md) for the prioritized list.


1. Discover node addresses through the QEMU guest agent or Proxmox IPAM.
2. Support more than one load balancer, for high availability.
3. Publish container images and binaries with checksums, and download them
   verified in `installLB.sh`.
