#!/bin/bash

# Copyright 2023-2026 Lior Okman
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

VMID=$1; shift
PHASE=$1; shift

# The release to download lbmanager and lbctl from, unless they are provided next to this script
LBMANAGER_VERSION=${LBMANAGER_VERSION:-v0.3.1}
SCRIPT_DIR=$(dirname "$(readlink -f "$0")")

writeFile() {
	local filename=$1; shift

	# The container's description holds the settings as JSON; the API returns it decoded
	local ct_notes
	ct_notes=$(pvesh get /nodes/$(hostname)/lxc/$VMID/config --output-format json | jq -r .description)
	ext_ip=$(echo $ct_notes | jq -r .ip)
	ext_mask=$(echo $ct_notes | jq -r .mask)
	ext_end=$(echo $ct_notes | jq -r .end)
	ext_start=$(echo $ct_notes | jq -r .start)

	pct exec $VMID tee $filename > /dev/null << EOF
#!/bin/bash

# Install the necessary tools, unless they are all there already
PACKAGES="ipvsadm iptables jq wget golang-cfssl"
if ! dpkg -s \$PACKAGES > /dev/null 2>&1; then
  apt-get -y update
  apt-get -y install \$PACKAGES
fi

# Create the required network environment:
# 1. Create a new namespace for the LB
ip netns add LB

# 2. Start the loopback in the new namespace
ip netns exec LB ip link set lo up

# 3. Move eth1 into it, copy the address it has into the new namespace
ETH1_ADDR=\$(ip -j addr show dev eth1 | jq -r '.[0].addr_info[] | select( .family == "inet") | (.local) + "/" + (.prefixlen|tostring)')

ip link set eth1 netns LB
ip netns exec LB ip addr add \$ETH1_ADDR dev eth1
ip netns exec LB ip link set eth1 up

# 4. The LAN side. With a third network interface (eth2) it moves into the
# namespace as it is, and eth0 stays outside as the management interface.
# Otherwise, fall back to bridging eth0 to a veth pair whose other end is in
# the namespace.
ETH0_ADDR=\$(ip -j addr show dev eth0 | jq -r '.[0].addr_info[] | select( .family == "inet") | (.local) + "/" + (.prefixlen|tostring)')
ETH0_GW=\$(ip -j route | jq -r ' .[] | select( .dst == "default" ) | .gateway ' )

if ip link show dev eth2 > /dev/null 2>&1; then
  EXT_IF=eth2
  ip link set eth2 netns LB
else
  apt-get -y install bridge-utils
  EXT_IF=external0
  brctl addbr br0
  ip link add veth1 type veth peer name external0 netns LB
  ip link set veth1 up
  brctl addif br0 veth1
  ip link set br0 up

  # Move the ip address from eth0 to br0 and add eth0 to br0
  ip addr del \$ETH0_ADDR dev eth0
  brctl addif br0 eth0
  ip addr add \$ETH0_ADDR dev br0
  ip route add default via \$ETH0_GW
fi

# 5. Setup the LAN device in the LB network namespace. IPv6 is not supported,
# and the LAN's router advertisements must not give it an address.
ip netns exec LB sysctl -q net.ipv6.conf.\$EXT_IF.disable_ipv6=1
# Announce the interface address when it comes up (lbmanager announces every virtual IP it adds)
ip netns exec LB sysctl -q net.ipv4.conf.\$EXT_IF.arp_notify=1
ip netns exec LB ip link set \$EXT_IF up
ip netns exec LB ip addr add $ext_ip/$ext_mask dev \$EXT_IF
ip netns exec LB ip route add default via \$ETH0_GW

# 7. Setup ip forwarding in the LB namespace
ip netns exec LB sysctl net.ipv4.ip_forward=1
ip netns exec LB sysctl net.ipv4.vs.conntrack=1

# 8. Setup SNAT so this container can serve as an internet gateway
# Connections from inside the VNet to a load balancer address must also have their source
# rewritten. Otherwise the target answers the client directly, bypassing this container.
ETH1_NET=\$(ip netns exec LB ip -j route show dev eth1 scope link | jq -r '.[0].dst')
ip netns exec LB iptables -t nat -A POSTROUTING -s \$ETH1_NET -d \$ETH1_NET -m ipvs --ipvs -j MASQUERADE
ip netns exec LB iptables -t nat -A POSTROUTING -o \$EXT_IF -j SNAT --to-source $ext_ip

# 9. Setup a CA for lbmanager
plain_ip=\$(echo \$ETH0_ADDR | cut -f1 -d/)
mkdir -p /var/lib/lbmanagerca
if [ ! -f /var/lib/lbmanagerca/ca.pem ]; then
  pushd /var/lib/lbmanagerca > /dev/null
  # Certificate for lbmanager
  echo '{"key":{"algo":"rsa","size":2048},"names":[{"O":"HomeLab","CN":"Root LB CA"}]}' | \
        cfssl genkey -initca - | \
        cfssljson -bare ca
  echo "{\"hosts\": [\"\$plain_ip\",\"127.0.0.1\",\"localhost\"],\"key\":{\"algo\":\"rsa\",\"size\":2048},\"names\":[{\"O\":\"Homelab\",\"CN\":\"\$plain_ip\"}]}" | \
        cfssl gencert -ca ca.pem  -ca-key ca-key.pem  -  | \
        cfssljson -bare lbmanager
  echo "{\"key\":{\"algo\":\"rsa\",\"size\":2048},\"names\":[{\"O\":\"Homelab\",\"CN\":\"lbctl\"}]}" | \
        cfssl gencert -ca ca.pem  -ca-key ca-key.pem  -  | \
        cfssljson -bare lbctl
  popd > /dev/null
fi

# 10. Install lbmanager 
# The binaries are pushed from the snippets directory when they are present there,
# otherwise they are downloaded from the release.
# Downloads are verified against the SHA256SUMS of the release.
RELEASE_URL=https://github.com/liorokman/proxmox-cloud-provider/releases/download/$LBMANAGER_VERSION
for bin in lbmanager lbctl; do
  if [ ! -x /usr/local/bin/\$bin ]; then
    SUMS=\$(mktemp)
    wget -q -O \$SUMS \$RELEASE_URL/SHA256SUMS && \
      wget -q -O /usr/local/bin/\$bin.download \$RELEASE_URL/\$bin && \
      awk -v b="\$bin" '\$2 == b { print \$1 "  /usr/local/bin/" b ".download" }' \$SUMS | sha256sum -c --quiet - || \
      { echo "ERROR: could not download and verify \$bin $LBMANAGER_VERSION" >&2; rm -f /usr/local/bin/\$bin.download \$SUMS; exit 1; }
    mv /usr/local/bin/\$bin.download /usr/local/bin/\$bin
    rm -f \$SUMS
  fi
done
chmod +x /usr/local/bin/lbmanager /usr/local/bin/lbctl
mkdir -p /etc/lbmanager
cp /var/lib/lbmanagerca/lbmanager.pem /etc/lbmanager/cert.pem
cp /var/lib/lbmanagerca/lbmanager-key.pem /etc/lbmanager/key.pem
cp /var/lib/lbmanagerca/lbctl.pem /etc/lbmanager/lbctl.pem
cp /var/lib/lbmanagerca/lbctl-key.pem /etc/lbmanager/lbctl-key.pem
cp /var/lib/lbmanagerca/ca.pem /etc/lbmanager/ca.pem

cat > /etc/lbmanager/lbmanager.yaml << END
---
grpc:
  listen: 0.0.0.0
  port: 9999
  auth:
    cert: /etc/lbmanager/cert.pem
    key: /etc/lbmanager/key.pem
    ca: /etc/lbmanager/ca.pem
dbDir: /var/lib/lbmanager
loadbalancer:
  namespace: LB
  externalInterface: \$EXT_IF
  internalInterface: eth1
  ipam:
    cidr: $ext_ip/$ext_mask
    dynamicRange:
      startAt: $ext_start
      endAt: $ext_end
metrics:
  listen: \$plain_ip:9100
END

cat > /etc/lbmanager/lbctl.yaml << END
---
addr: \$plain_ip:9999
auth:
  clientKey: /etc/lbmanager/lbctl-key.pem
  clientCert: /etc/lbmanager/lbctl.pem
  caFile: /etc/lbmanager/ca.pem
END

cat > /etc/systemd/system/lbmanager.service << END
[Unit]
Description=LoadBalancer Manager
ConditionPathExists=/etc/lbmanager/cert.pem

[Service]
ExecStart=/usr/local/bin/lbmanager
Restart=on-failure

[Install]
Alias=lbmanager.service

END

systemctl daemon-reload
systemctl start lbmanager


EOF
	pct exec $VMID chmod +x $filename
}

case "$PHASE"  in
   pre-start)
   ;;
   post-start)
	   for bin in lbmanager lbctl; do
		   if [ -f "$SCRIPT_DIR/$bin" ]; then
			   # pct push can't be used here: the container's config is locked while the hookscript runs
			   pct exec $VMID -- tee /usr/local/bin/$bin < "$SCRIPT_DIR/$bin" > /dev/null
			   pct exec $VMID -- chmod 0755 /usr/local/bin/$bin
		   fi
	   done
	   writeFile /install.sh
	   pct exec $VMID /install.sh
   ;;
   pre-stop)
   ;;
   post-stop)
   ;;
   *)
	    echo "Unknown phase $PHASE"
	    exit 1
   ;;
esac


