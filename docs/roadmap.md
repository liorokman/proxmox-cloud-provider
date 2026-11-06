# Road-map

A prioritized list of improvements. Effort: **S** = an evening, **M** = a
weekend, **L** = several weekends. See [architecture.md](architecture.md) for
how the pieces fit together.

## Load balancer review

### Is it useful?

Yes, for a specific niche: Kubernetes nodes in an isolated Proxmox SDN VNet
(VXLAN or VLAN zone). Something has to route between the VNet and the LAN, and
the in-cluster alternatives do not remove that need:

* MetalLB in L2 mode announces VIPs inside the VNet, where the LAN cannot reach them.
* BGP based options (MetalLB, Cilium, kube-vip) need a router that peers with
  the cluster, or static routes, which home networks often can't provide.

The load balancer container solves both with one container and one reserved
range of LAN addresses, with no router configuration. ARP for the VIPs on the
LAN, IPVS NAT, and preservation of external client addresses were verified on
Proxmox VE 9.2.

### What works well

* IPVS is in the kernel, fast, simple, and handles TCP and UDP.
* Mutual TLS gRPC between the cloud provider and `lbmanager` is a clean boundary.
* The management address and gRPC are separated from the data plane by a
  network namespace.
* One container is both gateway and load balancer, so a new cluster needs very
  little setup.

### Problems, by severity

| # | Problem | Evidence | Effect |
|---|---|---|---|
| 1 | No health checks. NotReady or shut down nodes stay IPVS destinations. | `k8s.io/cloud-provider` v0.37 only filters deleted, excluded and to-be-deleted nodes | A dead node receives 1/N of new connections, which fail |
| 2 | No high availability. One container carries all ingress and egress. | Design | A restart takes the cluster off the LAN and the internet |
| 3 | Deleting a load balancer removes its address even when another load balancer shares it. | Code reading | Breaks the other Service |
| 4 | `Restore()` is all or nothing (`log.Fatalf`). | `cmd/lbmanager/main.go` | One bad record leaves every load balancer down in a restart loop |
| 5 | Imperative API: the cloud provider computes the difference and calls add or delete. | The multi-port bug fixed in v0.1.0 came from this | Fragile; each call is a separate, non-atomic database update |
| 6 | No locking around read-modify-write and address allocation. | `internal/loadbalancer/impl.go`, `internal/ipam/simple.go` | Low risk today (the service controller syncs one Service at a time by default), but `lbctl` or a second client can race |
| 7 | `externalTrafficPolicy: Local` is not supported. | No `healthCheckNodePort` handling | Traffic is sent to nodes without endpoints and dropped |
| 8 | The hookscript reinstalls everything on every start, including `apt-get upgrade` and unverified downloads. | `scripts/installLB.sh` | Slow, needs the internet to start, not reproducible |
| 9 | Configuration reload is a no-op: the file is watched and merged, but never read again. | `cmd/lbmanager/main.go` | Misleading |
| 10 | The VIP range is unknown to the LAN's DHCP server and to Proxmox IPAM. | Design | Address collisions |
| 11 | `external0` picks up a global IPv6 address from the LAN (SLAAC), without a firewall or IPv6 support. | Observed on Proxmox VE 9.2 | Unexpected exposure, although nothing listens there today |
| 12 | A bridge is built inside the container (`br0` over `eth0` and a veth). | `scripts/installLB.sh` | A third NIC from Proxmox would be simpler and needs no `brctl` |

### How to improve it

1. **Health checks in `lbmanager`.** TCP checks of every NodePort, and of
   `healthCheckNodePort` for `externalTrafficPolicy: Local`. Failed
   destinations get weight 0 instead of being removed (as keepalived does).
   Fixes 1 and 7.
2. **Declarative API.** `Apply(name, vip, ports → targets)` converges IPVS and
   the database atomically, under a mutex. Keep the current RPCs for `lbctl`.
   Fixes 5 and 6, and makes the cloud provider side trivial.
3. **Reference counted VIPs, and a `Restore` that logs errors per load balancer
   and carries on.** Fixes 3 and 4.
4. **A highly available pair.** Two containers running keepalived (VRRP over
   unicast) float the VNet gateway address and the VIPs, and conntrackd keeps
   connection state in sync. `lbmanager` runs on both, and the cloud provider
   applies to both. Fixes 2.
5. **Packaging.** Build the container once, as a template or as a Proxmox VE
   9.1+ OCI application container, and set the network up from a systemd unit.
   The hookscript only renders configuration. Fixes 8 and 12.
6. **IPAM.** Reserve VIPs in Proxmox IPAM, or at least document how to exclude
   the range from the LAN's DHCP server. Fixes 10.

## Priorities

### P0: correctness

1. Health checks, with weight 0 for failed destinations. **M**
2. Reference counted VIPs, a resilient `Restore`, a mutex, and the small bugs:
   the nil dereference in `UpdateLoadBalancer` and the hard coded TCP protocol
   in `GetLoadBalancer`. **S**
3. CI: build, vet, unit tests, the privileged IPVS test, and releases with
   checksums and a signed image. **S–M**

### P1: much more useful for little work

4. Node addresses from the QEMU guest agent, falling back to static cloud-init
   settings. Makes DHCP and IPAM based setups work. **S**
5. Declarative `Apply` API. **M**
6. Narrow RBAC in the chart instead of `cluster-admin`, and metrics: per-VIP
   connections and health from the IPVS statistics. **S**
7. `externalTrafficPolicy: Local` through `healthCheckNodePort` (after 1). **S**
8. Node labels: `topology.kubernetes.io/region` from the Proxmox cluster name,
   and an instance type derived from CPU and memory. **S**
9. Hookscript: verify `SHA256SUMS`, skip `apt-get upgrade` when already
   installed, and use a third NIC instead of the bridge. **S**

### P2: bigger steps

10. A highly available load balancer pair with keepalived and conntrackd. **L**
11. A prebuilt load balancer appliance (template or OCI image). **M**
12. VIPs reserved in Proxmox IPAM. **M**
13. Several clusters per load balancer container, one VNet each. **M**
14. Hand the gateway role to SDN where the zone supports it (simple zone SNAT,
    EVPN exit nodes). **M**

### P3: nice to have

15. IPv6 and dual-stack. **M–L**
16. A bootstrap script or Cluster API integration for the manual steps in the
    README. **L**

If only three things get done: health checks, guest agent addresses, and CI.
