#!/bin/bash

# Copyright 2026 Lior Okman
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# SPDX-License-Identifier: Apache-2.0

#
# Bootstrap a complete Kubernetes cluster on a Proxmox VE node:
# SDN zone, VNet and subnet, the load balancer container, the API token for the
# cloud provider, the control plane and worker VMs, the CNI, and the cloud
# provider itself (Helm chart).
#
# Run it as root on the Proxmox node, from a checkout of this repository:
#
#   scripts/bootstrap-cluster.sh -c cluster.env up
#   scripts/bootstrap-cluster.sh -c cluster.env status
#   scripts/bootstrap-cluster.sh -c cluster.env destroy --yes
#
# Every step checks whether its result exists already, so a failed run can be
# repeated. See scripts/cluster.env.example for the settings.

set -euo pipefail

SCRIPT_DIR=$(dirname "$(readlink -f "$0")")
REPO_DIR=$(dirname "$SCRIPT_DIR")

log()  { printf '\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
	cat >&2 <<EOF
Usage: $0 [-c settings-file] {up|status|destroy [--yes]}

  up        create whatever is missing, until the cluster is running
  status    show what exists
  destroy   delete the VMs, the container and the SDN objects of this cluster
EOF
	exit 2
}

SETTINGS=
while getopts "c:h" opt; do
	case $opt in
		c) SETTINGS=$OPTARG ;;
		*) usage ;;
	esac
done
shift $((OPTIND - 1))
CMD=${1:-}; shift || true
[ -n "$CMD" ] || usage

# ----------------------------------------------------------------------------
# Settings (everything can be set in the settings file or the environment)
# ----------------------------------------------------------------------------
if [ -n "$SETTINGS" ]; then
	[ -r "$SETTINGS" ] || die "cannot read $SETTINGS"
	# shellcheck disable=SC1090
	. "$SETTINGS"
fi

: "${CLUSTER_NAME:=k8s1}"

# Proxmox
: "${NODE:=$(hostname)}"
: "${STORAGE:=local-lvm}"                 # VM and container disks
: "${SNIPPET_STORAGE:=local}"             # must allow "snippets"
: "${LAN_BRIDGE:=vmbr0}"
: "${PVE_API_HOST:=}"                     # how the pods reach the Proxmox API (default: the address of $LAN_BRIDGE)

# Network
: "${ZONE:=k8s}"                          # SDN zone (created as VXLAN if it doesn't exist)
: "${VNET:=$CLUSTER_NAME}"                # at most 8 characters, letters and digits
: "${VNET_TAG:=}"                         # VXLAN network identifier, default: derived from the subnet
: "${NODE_SUBNET:=192.168.50.0/24}"       # must be a /24, the gateway is .1 (the load balancer)
: "${POD_CIDR:=10.244.0.0/16}"
: "${SERVICE_CIDR:=10.96.0.0/12}"
: "${DNS_SERVER:=}"                       # for the VMs, default: $LAN_GW
: "${LAN_CIDR_BITS:=24}"                  # prefix length of the LAN
: "${LAN_GW:=}"                           # LAN default gateway
: "${LB_MGMT_IP:=}"                       # load balancer management address (LAN)
: "${LB_EXT_IP:=}"                        # load balancer's own address on the LAN (used for NAT)
: "${LB_EXT_START:=}"                     # first virtual IP; the API server uses this one
: "${LB_EXT_END:=}"                       # last virtual IP
: "${API_VIP:=$LB_EXT_START}"             # address of the Kubernetes API server

# Load balancer container
: "${LB_VMID:=1100}"
: "${LB_MODE:=appliance}"                  # appliance (a prebuilt template, built when missing) or hookscript (stock Debian + installLB.sh)
: "${LB_STATE_GB:=1}"                     # appliance: size of the volume that keeps the CA and the database
: "${LB_TEMPLATE:=debian-13-standard_13.6-1_amd64.tar.zst}"   # hookscript mode only
: "${LB_TEMPLATE_STORAGE:=local}"
: "${LB_ROOT_PASSWORD:=}"                 # default: random, not stored
: "${LBMANAGER_VERSION:=}"                # release to download (default: the hookscript's)
: "${LBMANAGER_BIN_DIR:=}"                # directory with self built lbmanager and lbctl

# Kubernetes nodes
: "${K8S_TEMPLATE_ID:=}"                  # VM template with kubeadm, kubelet and containerd
: "${CONTROL_PLANE_VMID:=1101}"
: "${WORKER_VMID_START:=1102}"
: "${WORKERS:=1}"
: "${NODE_IP_START:=3}"                   # last octet of the first node, the others follow
: "${NODE_CORES:=2}"
: "${NODE_MEMORY:=4096}"
: "${NODE_DISK_GB:=20}"
: "${SSH_PUBLIC_KEY_FILE:=}"              # for the "debian" user
: "${CNI_MANIFEST:=https://github.com/flannel-io/flannel/releases/download/v0.28.0/kube-flannel.yml}"

# Cloud provider
: "${PVE_USER:=ccm-$CLUSTER_NAME@pve}"    # one user and one token per cluster
: "${PVE_TOKEN_NAME:=ccm-$CLUSTER_NAME}"
: "${PVE_ROLE:=K8sCCM}"                   # on the cluster's pool: see the VMs and ask their guest agents
: "${PVE_NODE_ROLE:=K8sCCMNode}"          # on /: Sys.Audit, for the node and cluster status
: "${POOL:=k8s-$CLUSTER_NAME}"            # the cluster's VMs and container. The token only sees these
: "${HA:=no}"                             # yes: make the VMs and the container HA resources, with anti-affinity rules (needs a Proxmox cluster)
: "${TAGS:=k8s;$CLUSTER_NAME}"            # Proxmox tags of the VMs and the container; the VMs' become node labels
: "${CHART_DIR:=$REPO_DIR/chart}"
: "${IMAGE_TAG:=}"                        # default: the chart's appVersion
: "${IMAGE_ARCHIVE:=}"                    # optional: a docker/OCI image archive to load into every node (for an image that is not published yet)
: "${HELM_VERSION:=v3.19.0}"              # downloaded if "helm" isn't installed
: "${KUBECONFIG_OUT:=/root/.kube/$CLUSTER_NAME.yaml}"

STATE_DIR=/var/lib/pve-k8s-bootstrap
CLUSTER_STATE=$STATE_DIR/$CLUSTER_NAME

# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || die "run as root on a Proxmox node"
for c in pvesh pct qm pveum pveam pvesm jq curl; do
	command -v "$c" > /dev/null || die "$c not found: this must run on a Proxmox VE node (jq: apt install jq)"
done

pve() { pvesh "$@" --output-format json 2> /dev/null; }

require() {
	local missing=()
	for v in "$@"; do [ -n "${!v}" ] || missing+=("$v"); done
	[ ${#missing[@]} -eq 0 ] || die "missing settings: ${missing[*]} (see scripts/cluster.env.example)"
}

# Addresses derived from the node subnet
derive_network() {
	[[ $NODE_SUBNET =~ ^([0-9]+\.[0-9]+\.[0-9]+)\.0/24$ ]] || die "NODE_SUBNET must be a /24 ending in .0/24"
	SUBNET_PREFIX=${BASH_REMATCH[1]}
	GATEWAY_IP=$SUBNET_PREFIX.1
	if [ -z "$VNET_TAG" ]; then
		# Derived from the 2nd and 3rd octets, so subnets of different clusters differ
		IFS=. read -r _ o2 o3 _ <<< "$SUBNET_PREFIX.0"
		VNET_TAG=$((100 + o2 * 256 + o3))
	fi
	[ -n "$DNS_SERVER" ] || DNS_SERVER=$LAN_GW
	[ ${#VNET} -le 8 ] || die "VNET name '$VNET' is longer than 8 characters"
}

ip2int() { local IFS=.; read -r a b c d <<< "$1"; echo $(((a << 24) + (b << 16) + (c << 8) + d)); }
same_subnet() { # ip1 ip2 bits
	local mask=$(((0xFFFFFFFF << (32 - $3)) & 0xFFFFFFFF))
	[ $(($(ip2int "$1") & mask)) -eq $(($(ip2int "$2") & mask)) ]
}

node_ip()   { echo "$SUBNET_PREFIX.$((NODE_IP_START + $1))"; }   # 0 = control plane, 1.. = workers
worker_id() { echo $((WORKER_VMID_START + $1 - 1)); }
node_name() { if [ "$1" -eq 0 ]; then echo "$CLUSTER_NAME-cp1"; else echo "$CLUSTER_NAME-w$1"; fi; }
node_vmid() { if [ "$1" -eq 0 ]; then echo "$CONTROL_PLANE_VMID"; else worker_id "$1"; fi; }

vm_exists() { qm status "$1" > /dev/null 2>&1; }
ct_exists() { pct status "$1" > /dev/null 2>&1; }

# Run a command in a VM through the guest agent. Prints the output and returns
# the command's exit code.
vm_exec() {
	local id=$1; shift
	local out rc
	out=$(qm guest exec "$id" --timeout "${EXEC_TIMEOUT:-900}" -- bash -c "$*" 2>&1) \
		|| { echo "$out" >&2; return 1; }
	jq -j '."out-data" // empty' <<< "$out"
	jq -j '."err-data" // empty' <<< "$out" >&2
	if [ "$(jq -r '.exited // 0' <<< "$out")" != 1 ]; then echo "timed out: $*" >&2; return 1; fi
	rc=$(jq -r '.exitcode // 1' <<< "$out")
	return "$rc"
}

wait_for() { # description, tries, command...
	local what=$1 tries=$2; shift 2
	local i
	for ((i = 0; i < tries; i++)); do
		if "$@" > /dev/null 2>&1; then return 0; fi
		sleep 5
	done
	die "timed out waiting for $what"
}

# kubectl on the control plane (the Proxmox node doesn't need it)
kc() { vm_exec "$(node_vmid 0)" "kubectl --kubeconfig /etc/kubernetes/admin.conf $*"; }

lbctl() { pct exec "$LB_VMID" -- /usr/local/bin/lbctl "$@" 2>&1; }

# ----------------------------------------------------------------------------
# Steps
# ----------------------------------------------------------------------------
preflight() {
	log "Checking the environment"
	require LAN_GW LB_MGMT_IP LB_EXT_IP LB_EXT_START LB_EXT_END K8S_TEMPLATE_ID SSH_PUBLIC_KEY_FILE
	derive_network
	[ -r "$SSH_PUBLIC_KEY_FILE" ] || die "cannot read SSH_PUBLIC_KEY_FILE=$SSH_PUBLIC_KEY_FILE"
	qm config "$K8S_TEMPLATE_ID" 2> /dev/null | grep -q '^template: 1' \
		|| die "VM $K8S_TEMPLATE_ID is not a template. Build one first, see the README"
	pvesm status --content snippets 2> /dev/null | awk 'NR>1{print $1}' | grep -qx "$SNIPPET_STORAGE" \
		|| die "storage $SNIPPET_STORAGE doesn't allow snippets"
	ip link show "$LAN_BRIDGE" > /dev/null 2>&1 || die "no bridge $LAN_BRIDGE"
	[ -n "$PVE_API_HOST" ] || PVE_API_HOST=$(ip -j -4 addr show dev "$LAN_BRIDGE" | jq -r '.[0].addr_info[0].local')
	# The load balancer addresses must not be in use already, unless they are ours
	if ! ct_exists "$LB_VMID"; then
		for a in $LB_MGMT_IP $LB_EXT_IP $API_VIP; do
			if ping -c1 -W1 "$a" > /dev/null 2>&1; then die "$a answers to ping: pick an unused address"; fi
		done
	fi
	same_subnet "$LB_EXT_IP" "$LAN_GW" "$LAN_CIDR_BITS" || die "LAN_GW $LAN_GW is not in the same /$LAN_CIDR_BITS network as LB_EXT_IP $LB_EXT_IP (is LAN_CIDR_BITS right?)"
	for a in $LB_MGMT_IP $LB_EXT_START $LB_EXT_END $API_VIP; do
		same_subnet "$LB_EXT_IP" "$a" "$LAN_CIDR_BITS" || die "$a is not in the same /$LAN_CIDR_BITS network as LB_EXT_IP $LB_EXT_IP"
	done
	info "node $NODE, template $K8S_TEMPLATE_ID, API server $API_VIP:6443"

	# The IPVS kernel modules have to be loaded on the host
	if [ ! -f /etc/modules-load.d/ipvs.conf ]; then
		printf 'ip_vs\nip_vs_rr\nnf_conntrack\n' > /etc/modules-load.d/ipvs.conf
	fi
	modprobe -a ip_vs ip_vs_rr nf_conntrack
	mkdir -p "$CLUSTER_STATE"; chmod 700 "$STATE_DIR" "$CLUSTER_STATE"
}

step_sdn() {
	log "SDN: zone $ZONE, VNet $VNET, subnet $NODE_SUBNET"
	local changed=0
	if ! pve get /cluster/sdn/zones | jq -e --arg z "$ZONE" '.[] | select(.zone == $z)' > /dev/null; then
		local peers
		peers=$(pve get /cluster/status | jq -r '[.[] | select(.type == "node") | .ip] | join(",")')
		[ -n "$peers" ] || peers=$PVE_API_HOST
		pvesh create /cluster/sdn/zones --type vxlan --zone "$ZONE" --peers "$peers" --ipam pve > /dev/null
		info "created VXLAN zone $ZONE (peers $peers)"; changed=1
	fi
	if ! pve get /cluster/sdn/vnets | jq -e --arg v "$VNET" '.[] | select(.vnet == $v)' > /dev/null; then
		pvesh create /cluster/sdn/vnets --vnet "$VNET" --zone "$ZONE" --tag "$VNET_TAG" > /dev/null
		info "created VNet $VNET (VXLAN id $VNET_TAG)"; changed=1
	fi
	if ! pve get "/cluster/sdn/vnets/$VNET/subnets" | jq -e --arg c "$NODE_SUBNET" '.[] | select(.cidr == $c)' > /dev/null; then
		pvesh create "/cluster/sdn/vnets/$VNET/subnets" --type subnet --subnet "$NODE_SUBNET" --gateway "$GATEWAY_IP" > /dev/null
		info "created subnet $NODE_SUBNET"; changed=1
	fi
	if [ "$changed" -eq 1 ] || ! ip link show "$VNET" > /dev/null 2>&1; then
		pvesh set /cluster/sdn > /dev/null
		wait_for "the bridge $VNET" 24 ip link show "$VNET"
	fi
}

lb_desc() {
	jq -nc --arg ip "$LB_EXT_IP" --argjson mask "$LAN_CIDR_BITS" --arg s "$LB_EXT_START" --arg e "$LB_EXT_END" \
		'{ip:$ip, mask:$mask, start:$s, end:$e}'
}

lb_nics() {
	echo "-net0 name=eth0,bridge=$LAN_BRIDGE,firewall=1,ip=$LB_MGMT_IP/$LAN_CIDR_BITS,gw=$LAN_GW,type=veth"
	echo "-net1 name=eth1,bridge=$VNET,firewall=1,ip=$GATEWAY_IP/24,type=veth"
	echo "-net2 name=eth2,bridge=$LAN_BRIDGE,firewall=1,type=veth"
}

create_lb_hookscript() {
	local snippets
	snippets=$(dirname "$(pvesm path "$SNIPPET_STORAGE:snippets/installLB.sh")")
	mkdir -p "$snippets"
	install -m 0755 "$SCRIPT_DIR/installLB.sh" "$snippets/installLB.sh"
	if [ -n "$LBMANAGER_VERSION" ]; then
		sed -i "s|^LBMANAGER_VERSION=.*|LBMANAGER_VERSION=\${LBMANAGER_VERSION:-$LBMANAGER_VERSION}|" "$snippets/installLB.sh"
	fi
	if [ -n "$LBMANAGER_BIN_DIR" ]; then
		for b in lbmanager lbctl; do install -m 0755 "$LBMANAGER_BIN_DIR/$b" "$snippets/$b"; done
	fi
	if ! pveam list "$LB_TEMPLATE_STORAGE" 2> /dev/null | grep -q "$LB_TEMPLATE"; then
		pveam update > /dev/null
		pveam download "$LB_TEMPLATE_STORAGE" "$LB_TEMPLATE"
	fi
	local pw=${LB_ROOT_PASSWORD:-$(head -c 24 /dev/urandom | base64)}
	# shellcheck disable=SC2046
	pct create "$LB_VMID" "$LB_TEMPLATE_STORAGE:vztmpl/$LB_TEMPLATE" \
		-unprivileged 0 -cores 2 -swap 0 -memory 512 -hostname "$CLUSTER_NAME-lb" $(lb_nics) \
		-ostype debian -features nesting=1 -password "$pw" -storage "$STORAGE" \
		-description "$(lb_desc)" -onboot 1 -tags "$TAGS;k8s-lb" -pool "$POOL" > /dev/null
	pct set "$LB_VMID" -hookscript "$SNIPPET_STORAGE:snippets/installLB.sh"
}

create_lb_appliance() {
	local name
	if [ -n "$LBMANAGER_BIN_DIR" ]; then name=lbmanager-dev
	else name=lbmanager-${LBMANAGER_VERSION:-$(sed -n 's/^LBMANAGER_VERSION=\${LBMANAGER_VERSION:-\(.*\)}/\1/p' "$SCRIPT_DIR/installLB.sh")}; fi
	local tmpl=${name}_amd64.tar.zst
	if ! pveam list "$LB_TEMPLATE_STORAGE" 2> /dev/null | grep -q "$tmpl"; then
		info "building the load balancer appliance $tmpl"
		STORAGE=$STORAGE TEMPLATE_STORAGE=$LB_TEMPLATE_STORAGE BUILD_BRIDGE=$LAN_BRIDGE \
			LBMANAGER_VERSION=$LBMANAGER_VERSION LBMANAGER_BIN_DIR=$LBMANAGER_BIN_DIR "$SCRIPT_DIR/build-appliance.sh" > /dev/null
	fi
	# shellcheck disable=SC2046
	pct create "$LB_VMID" "$LB_TEMPLATE_STORAGE:vztmpl/$tmpl" \
		-unprivileged 0 -cores 2 -swap 0 -memory 512 -hostname "$CLUSTER_NAME-lb" $(lb_nics) \
		-ostype debian -features nesting=1 -rootfs "$STORAGE:4" -mp0 "$STORAGE:$LB_STATE_GB,mp=/var/lib/lbstate,backup=1" \
		-description "$(lb_desc)" -onboot 1 -tags "$TAGS;k8s-lb" -pool "$POOL" > /dev/null
}

step_loadbalancer() {
	log "Load balancer container $LB_VMID ($LB_MODE)"
	if ! ct_exists "$LB_VMID"; then
		case "$LB_MODE" in
			appliance) create_lb_appliance ;;
			hookscript) create_lb_hookscript ;;
			*) die "LB_MODE must be appliance or hookscript" ;;
		esac
		info "created container $LB_VMID"
	fi
	if [ "$(pct status "$LB_VMID" | awk '{print $2}')" != running ]; then
		pct start "$LB_VMID"
	fi
	if [ "$LB_MODE" = appliance ] && ! pct exec "$LB_VMID" -- test -f /etc/lbmanager/appliance.env; then
		# The units in the image wait for this file
		local env; env=$(mktemp)
		printf 'EXT_IP=%s\nEXT_MASK=%s\nRANGE_START=%s\nRANGE_END=%s\n' "$LB_EXT_IP" "$LAN_CIDR_BITS" "$LB_EXT_START" "$LB_EXT_END" > "$env"
		wait_for "the container to boot" 24 pct exec "$LB_VMID" -- true
		pct push "$LB_VMID" "$env" /etc/lbmanager/appliance.env --perms 0644
		rm -f "$env"
		pct exec "$LB_VMID" -- systemctl start lb-network lbmanager
	fi
	wait_for "lbmanager to start" 60 pct exec "$LB_VMID" -- systemctl is-active --quiet lbmanager
	# The first start of lbmanager can be before its certificates are readable
	wait_for "lbctl to connect" 24 pct exec "$LB_VMID" -- /usr/local/bin/lbctl -op list
	if ! lbctl -op list | grep -q 'name:"apiserver"'; then
		lbctl -op addSrv -name apiserver -srv "$API_VIP" > /dev/null
		info "load balancer entry for the API server at $API_VIP"
	fi
}

# The pool exists before the first guest, which is created in it
step_create_pool() {
	log "Pool $POOL"
	if ! pve get /pools | jq -e --arg p "$POOL" '.[] | select(.poolid == $p)' > /dev/null; then
		pvesh create /pools --poolid "$POOL" --comment "Kubernetes cluster $CLUSTER_NAME" > /dev/null
		info "created pool $POOL"
	fi
}

step_credentials() {
	log "Proxmox pool and API token for the cloud provider"
	# The token sees the VMs of this cluster only: the role is granted on its pool. Sys.Audit is
	# needed on / (for the cluster name and the node status), and gives no access to any VM.
	pveum role list --output-format json | jq -e --arg r "$PVE_ROLE" '.[] | select(.roleid == $r)' > /dev/null \
		|| pveum role add "$PVE_ROLE" -privs "VM.Audit VM.GuestAgent.Audit"
	pveum role list --output-format json | jq -e --arg r "$PVE_NODE_ROLE" '.[] | select(.roleid == $r)' > /dev/null \
		|| pveum role add "$PVE_NODE_ROLE" -privs "Sys.Audit"
	pveum user list --output-format json | jq -e --arg u "$PVE_USER" '.[] | select(.userid == $u)' > /dev/null \
		|| pveum user add "$PVE_USER" --comment "Kubernetes cloud controller of $CLUSTER_NAME"
	pveum acl modify "/pool/$POOL" --users "$PVE_USER" --roles "$PVE_ROLE"
	pveum acl modify / --users "$PVE_USER" --roles "$PVE_NODE_ROLE"
	local secret=$STATE_DIR/$PVE_USER.$PVE_TOKEN_NAME.token
	local exists
	exists=$(pveum user token list "$PVE_USER" --output-format json | jq -r --arg t "$PVE_TOKEN_NAME" '[.[] | select(.tokenid == $t)] | length')
	if [ "$exists" != 0 ] && [ ! -s "$secret" ]; then
		# The secret is shown only once. It may be in use by something else, so never replace it
		die "the token $PVE_USER!$PVE_TOKEN_NAME exists but its secret is not in $secret. Set PVE_TOKEN_NAME to a new name, or remove the token yourself (pveum user token remove)"
	fi
	if [ "$exists" = 0 ]; then
		(umask 077; pveum user token add "$PVE_USER" "$PVE_TOKEN_NAME" --privsep 0 --output-format json | jq -r .value > "$secret")
		info "created token $PVE_USER!$PVE_TOKEN_NAME"
	fi
}

step_vms() {
	log "Virtual machines"
	local i id name ip
	for ((i = 0; i <= WORKERS; i++)); do
		id=$(node_vmid "$i"); name=$(node_name "$i"); ip=$(node_ip "$i")
		if ! vm_exists "$id"; then
			qm clone "$K8S_TEMPLATE_ID" "$id" --full --name "$name" --storage "$STORAGE" --pool "$POOL" > /dev/null
			qm set "$id" --cores "$NODE_CORES" --memory "$NODE_MEMORY" --agent enabled=1 \
				--net0 "virtio,bridge=$VNET" --ipconfig0 "ip=$ip/24,gw=$GATEWAY_IP" \
				--nameserver "$DNS_SERVER" --sshkeys "$SSH_PUBLIC_KEY_FILE" --onboot 1 --tags "$TAGS" > /dev/null
			qm disk resize "$id" virtio0 "${NODE_DISK_GB}G" > /dev/null
			qm cloudinit update "$id"
			info "created $name ($id) at $ip"
		fi
		if [ "$(qm status "$id" | awk '{print $2}')" != running ]; then qm start "$id"; fi
	done
	for ((i = 0; i <= WORKERS; i++)); do
		wait_for "the guest agent of $(node_name "$i")" 60 qm agent "$(node_vmid "$i")" ping
	done
}

# Guests that were created before the pool (earlier versions of this script) are added too
step_pool() {
	local members id i
	members=$(pve get "/pools/$POOL" | jq -r '.members[]?.vmid' 2> /dev/null || true)
	for id in "$LB_VMID" $(for ((i = 0; i <= WORKERS; i++)); do node_vmid "$i"; done); do
		grep -qx "$id" <<< "$members" && continue
		pvesh set "/pools/$POOL" --vms "$id" > /dev/null
		info "added $id to the pool"
	done
}

# HA resources, and rules that keep the load balancer away from the control plane, and the
# workers away from each other (when there are hosts enough for that). Proxmox does
# the placing, and the HA manager may migrate the VMs: the cloud provider updates the node labels.
step_ha() {
	[ "$HA" = yes ] || return 0
	log "High availability"
	local nodes i id
	nodes=$(pve get /nodes | jq 'length')
	[ "$nodes" -ge 2 ] || { info "HA=yes needs a Proxmox cluster of 2 nodes or more, skipping"; return 0; }
	pve get /cluster/ha/resources > /dev/null || die "cannot read the HA resources (is this node in a cluster?)"
	for id in "vm:$(node_vmid 0)" $(for ((i = 1; i <= WORKERS; i++)); do echo "vm:$(node_vmid "$i")"; done) "ct:$LB_VMID"; do
		pve get /cluster/ha/resources | jq -e --arg s "$id" '.[] | select(.sid == $s)' > /dev/null \
			|| { pvesh create /cluster/ha/resources --sid "$id" --state started > /dev/null; info "HA resource $id"; }
	done
	ha_rule "ha-$CLUSTER_NAME-lb" "ct:$LB_VMID,vm:$(node_vmid 0)"
	if [ "$WORKERS" -ge 2 ] && [ "$WORKERS" -le "$nodes" ]; then
		ha_rule "ha-$CLUSTER_NAME-workers" "$(for ((i = 1; i <= WORKERS; i++)); do printf 'vm:%s,' "$(node_vmid "$i")"; done | sed 's/,$//')"
	fi
}

ha_rule() { # name resources: keep the resources on different hosts
	pve get /cluster/ha/rules | jq -e --arg r "$1" '.[] | select(.rule == $r)' > /dev/null \
		|| { pvesh create /cluster/ha/rules --rule "$1" --type resource-affinity --affinity negative --resources "$2" > /dev/null; info "HA rule $1"; }
}

prepare_kubelet() { # vmid
	vm_exec "$1" 'grep -qs -- "--cloud-provider=external" /etc/default/kubelet || { echo KUBELET_EXTRA_ARGS=--cloud-provider=external > /etc/default/kubelet; }' > /dev/null
}

step_control_plane() {
	log "Control plane"
	local id ip
	id=$(node_vmid 0); ip=$(node_ip 0)
	# kubeadm checks the API server through the load balancer, so the target must exist before it runs
	if ! lbctl -op list | grep 'name:"apiserver"' | grep -q "dstIP:\"$ip\""; then
		lbctl -op addTgt -name apiserver -srv "$ip" -sport 6443 -dport 6443 > /dev/null
		info "API server target $ip:6443"
	fi
	prepare_kubelet "$id"
	if vm_exec "$id" 'test -f /etc/kubernetes/admin.conf' > /dev/null 2>&1; then
		info "already initialized"
	else
		info "kubeadm init (a few minutes)"
		vm_exec "$id" "kubeadm init --control-plane-endpoint $API_VIP:6443 --pod-network-cidr $POD_CIDR --service-cidr $SERVICE_CIDR > /var/log/kubeadm-init.log 2>&1 || { tail -30 /var/log/kubeadm-init.log; exit 1; }"
	fi
	if ! vm_exec "$id" 'kubectl --kubeconfig /etc/kubernetes/admin.conf get ns kube-flannel' > /dev/null 2>&1; then
		info "installing the CNI"
		vm_exec "$id" "kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f $CNI_MANIFEST" > /dev/null
	fi
}

step_workers() {
	log "Workers"
	local i id name cmd
	for ((i = 1; i <= WORKERS; i++)); do
		id=$(node_vmid "$i"); name=$(node_name "$i")
		if vm_exec "$id" 'test -f /etc/kubernetes/kubelet.conf' > /dev/null 2>&1; then
			info "$name already joined"; continue
		fi
		prepare_kubelet "$id"
		cmd=$(vm_exec "$(node_vmid 0)" 'kubeadm token create --ttl 1h --print-join-command' | grep '^kubeadm join' | tail -1)
		[ -n "$cmd" ] || die "could not get the join command from the control plane"
		info "$name: kubeadm join"
		vm_exec "$id" "$cmd > /var/log/kubeadm-join.log 2>&1 || { tail -30 /var/log/kubeadm-join.log; exit 1; }"
	done
}

step_kubeconfig() {
	log "Kubeconfig: $KUBECONFIG_OUT"
	mkdir -p "$(dirname "$KUBECONFIG_OUT")"
	(umask 077; vm_exec "$(node_vmid 0)" 'cat /etc/kubernetes/admin.conf' \
		| sed "s#server: .*#server: https://$API_VIP:6443#" > "$KUBECONFIG_OUT")
	export KUBECONFIG=$KUBECONFIG_OUT
}

# Copy an image archive into the nodes' containerd. The nodes are only reachable
# through the load balancer, so the Proxmox node gets a route through it.
step_load_image() {
	[ -n "$IMAGE_ARCHIVE" ] || return 0
	log "Loading $IMAGE_ARCHIVE into the nodes"
	[ -r "$IMAGE_ARCHIVE" ] || die "cannot read IMAGE_ARCHIVE=$IMAGE_ARCHIVE"
	local key=${SSH_PUBLIC_KEY_FILE%.pub}
	[ -r "$key" ] || die "copying the image needs the private key $key (next to SSH_PUBLIC_KEY_FILE)"
	local ssh_opts=(-i "$key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10)
	ip route replace "$NODE_SUBNET" via "$LB_EXT_IP"
	local i ip
	for ((i = 0; i <= WORKERS; i++)); do
		ip=$(node_ip "$i")
		wait_for "ssh to $ip" 36 ssh "${ssh_opts[@]}" "debian@$ip" true
		scp "${ssh_opts[@]}" -q "$IMAGE_ARCHIVE" "debian@$ip:/tmp/image.tar"
		ssh "${ssh_opts[@]}" "debian@$ip" 'sudo ctr -n k8s.io images import /tmp/image.tar > /dev/null && rm -f /tmp/image.tar'
		info "$(node_name "$i")"
	done
}

ensure_helm() {
	command -v helm > /dev/null && return 0
	local dir=$STATE_DIR/helm-$HELM_VERSION
	if [ ! -x "$dir/helm" ]; then
		info "downloading helm $HELM_VERSION"
		local tmp; tmp=$(mktemp -d)
		local f=helm-$HELM_VERSION-linux-amd64.tar.gz
		curl -fsSL -o "$tmp/$f" "https://get.helm.sh/$f"
		(cd "$tmp" && echo "$(curl -fsSL "https://get.helm.sh/$f.sha256sum" | cut -d' ' -f1)  $f" | sha256sum -c --quiet -)
		mkdir -p "$dir"; tar -xzf "$tmp/$f" -C "$tmp"; install -m 0755 "$tmp/linux-amd64/helm" "$dir/helm"
		rm -rf "$tmp"
	fi
	PATH=$dir:$PATH
}

step_cloud_provider() {
	log "Cloud provider"
	ensure_helm
	local work; work=$(mktemp -d)
	local f
	for f in lbctl.pem lbctl-key.pem ca.pem; do
		pct exec "$LB_VMID" -- cat "/etc/lbmanager/$f" > "$work/$f"
	done
	cp /etc/pve/pve-root-ca.pem "$work/pve-root-ca.pem"
	local token; token=$(cat "$STATE_DIR/$PVE_USER.$PVE_TOKEN_NAME.token")
	local args=()
	[ -z "$IMAGE_TAG" ] || args+=(--set "image.tag=$IMAGE_TAG")
	helm upgrade --install proxmox-ccm "$CHART_DIR" --namespace kube-system \
		--set "lbmanager.hostname=$LB_MGMT_IP" \
		--set-file "lbmanager.tls.key=$work/lbctl-key.pem" \
		--set-file "lbmanager.tls.cert=$work/lbctl.pem" \
		--set-file "lbmanager.tls.caCert=$work/ca.pem" \
		--set "proxmox.apiToken=$token" \
		--set "proxmox.user=$PVE_USER!$PVE_TOKEN_NAME" \
		--set "proxmox.apiURL=https://$PVE_API_HOST:8006/api2/json" \
		--set-file "proxmox.caCert=$work/pve-root-ca.pem" \
		"${args[@]}" > /dev/null
	rm -rf "$work"
}

step_wait() {
	log "Waiting for the nodes"
	local want=$((WORKERS + 1))
	wait_ready "$want"
	kc get nodes -o wide
}

wait_ready() { # count
	local i n
	for ((i = 0; i < 120; i++)); do
		n=$(kc get nodes --no-headers 2> /dev/null | grep -cw Ready || true)
		[ "$n" -ge "$1" ] && break
		sleep 5
	done
	[ "$n" -ge "$1" ] || die "timed out waiting for $1 Ready nodes"
	for ((i = 0; i < 60; i++)); do
		kc -n kube-system get pods --no-headers 2> /dev/null | grep proxmox-ccm | grep -qw Running \
			&& ! kc get nodes -o json | grep -q node.cloudprovider.kubernetes.io/uninitialized && return 0
		sleep 5
	done
	die "timed out waiting for the cloud provider to initialize the nodes"
}

cmd_up() {
	preflight
	step_sdn
	step_create_pool
	step_loadbalancer
	step_credentials
	step_vms
	step_pool
	step_ha
	step_control_plane
	step_workers
	step_load_image
	step_kubeconfig
	step_cloud_provider
	step_wait
	log "Done"
	cat <<EOF
    export KUBECONFIG=$KUBECONFIG_OUT
    kubectl get nodes

    The API server is at https://$API_VIP:6443. LoadBalancer Services get an
    address from $LB_EXT_START - $LB_EXT_END (excluded from DHCP?).
EOF
}

cmd_status() {
	require K8S_TEMPLATE_ID
	derive_network
	local i
	printf '%-20s %s\n' "SDN VNet $VNET" "$(ip link show "$VNET" > /dev/null 2>&1 && echo present || echo missing)"
	printf '%-20s %s\n' "container $LB_VMID" "$(pct status "$LB_VMID" 2> /dev/null || echo missing)"
	for ((i = 0; i <= WORKERS; i++)); do
		printf '%-20s %s\n' "$(node_name "$i") ($(node_vmid "$i"))" "$(qm status "$(node_vmid "$i")" 2> /dev/null || echo missing)"
	done
	kc get nodes 2> /dev/null || true
}

cmd_destroy() {
	[ "${1:-}" = "--yes" ] || die "destroy deletes the VMs, the container and the SDN objects of $CLUSTER_NAME: add --yes"
	derive_network
	log "Destroying cluster $CLUSTER_NAME"
	local i id
	# HA resources first: the HA manager would start the guests again
	if pve get /cluster/ha/rules 2> /dev/null | jq -e '.' > /dev/null 2>&1; then
		for r in "ha-$CLUSTER_NAME-lb" "ha-$CLUSTER_NAME-workers"; do
			pve get /cluster/ha/rules | jq -e --arg r "$r" '.[] | select(.rule == $r)' > /dev/null && pvesh delete "/cluster/ha/rules/$r" > /dev/null
		done
		for id in "ct:$LB_VMID" $(for ((i = 0; i <= WORKERS; i++)); do echo "vm:$(node_vmid "$i")"; done); do
			pve get /cluster/ha/resources | jq -e --arg s "$id" '.[] | select(.sid == $s)' > /dev/null && pvesh delete "/cluster/ha/resources/$id" > /dev/null
		done
	fi
	for ((i = WORKERS; i >= 0; i--)); do
		id=$(node_vmid "$i")
		if vm_exists "$id"; then
			qm stop "$id" --skiplock 1 > /dev/null 2>&1 || true
			qm destroy "$id" --purge 1 --destroy-unreferenced-disks 1 > /dev/null
			info "deleted VM $id"
		fi
	done
	if ct_exists "$LB_VMID"; then
		pct stop "$LB_VMID" > /dev/null 2>&1 || true
		pct destroy "$LB_VMID" --purge 1 > /dev/null
		info "deleted container $LB_VMID"
	fi
	if pve get /cluster/sdn/vnets | jq -e --arg v "$VNET" '.[] | select(.vnet == $v)' > /dev/null; then
		pve get "/cluster/sdn/vnets/$VNET/subnets" | jq -r '.[].subnet' | while read -r s; do
			pvesh delete "/cluster/sdn/vnets/$VNET/subnets/$s" > /dev/null
		done
		pvesh delete "/cluster/sdn/vnets/$VNET" > /dev/null
		pvesh set /cluster/sdn > /dev/null
		info "deleted VNet $VNET (the zone $ZONE and the roles are kept)"
	fi
	# The token is named after the cluster, so it isn't used by anything else
	if [ -s "$STATE_DIR/$PVE_USER.$PVE_TOKEN_NAME.token" ]; then
		pveum user token remove "$PVE_USER" "$PVE_TOKEN_NAME" > /dev/null 2>&1 || true
		rm -f "$STATE_DIR/$PVE_USER.$PVE_TOKEN_NAME.token"
	fi
	if pve get /pools | jq -e --arg p "$POOL" '.[] | select(.poolid == $p)' > /dev/null; then
		pvesh delete "/pools/$POOL" > /dev/null && info "deleted pool $POOL"
	fi
	# A user that is named after the cluster is not shared (the ACLs go with it)
	if [ "$PVE_USER" = "ccm-$CLUSTER_NAME@pve" ]; then
		pveum user delete "$PVE_USER" > /dev/null 2>&1 || true
	fi
	ip route del "$NODE_SUBNET" via "$LB_EXT_IP" 2> /dev/null || true
	rm -f "$KUBECONFIG_OUT"
	rm -rf "$CLUSTER_STATE"
}

case "$CMD" in
	up) cmd_up ;;
	status) cmd_status ;;
	destroy) cmd_destroy "$@" ;;
	*) usage ;;
esac
