#!/bin/bash

VMID=$1; shift
PHASE=$1; shift

urldecode () { 
    : "${*//+/ }";
    echo -e "${_//%/\\x}"
}

writeFile() {
	local filename=$1; shift

	local ct_notes=$(pct config $VMID | grep ^description | cut -f2- -d: )
	ct_notes=$(urldecode $ct_notes)
	ext_ip=$(echo $ct_notes | jq -r .ip)
	ext_mask=$(echo $ct_notes | jq -r .mask)
	ext_end=$(echo $ct_notes | jq -r .end)
	ext_start=$(echo $ct_notes | jq -r .start)

	pct exec $VMID -- tee $filename << EOF
#!/bin/bash

# Install the necessary tools
apt-get -y update
apt-get -y upgrade
apt-get -y install ipvsadm iptables jq bridge-utils wget golang-cfssl

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

# 4. Create a new network bridge and a pair of veth devices
brctl addbr br0
ip link add veth1 type veth peer name external0 netns LB
ip link set veth1 up
brctl addif br0 veth1
ip link set br0 up

# 5. Move the ip address from eth0 to br0 and add eth0 to br0
ETH0_ADDR=\$(ip -j addr show dev eth0 | jq -r '.[0].addr_info[] | select( .family == "inet") | (.local) + "/" + (.prefixlen|tostring)')
ETH0_GW=\$(ip -j route | jq -r ' .[] | select( .dst == "default" ) | .gateway ' )

ip addr del \$ETH0_ADDR dev eth0
brctl addif br0 eth0
ip addr add \$ETH0_ADDR dev br0
ip route add default via \$ETH0_GW

# 6. Setup the bridged device in the LB network namespace
ip netns exec LB ip link set external0 up
ip netns exec LB ip addr add $ext_ip/$ext_mask dev external0
ip netns exec LB ip route add default via \$ETH0_GW

# 7. Setup ip forwarding in the LB namespace
ip netns exec LB sysctl net.ipv4.ip_forward=1

# 8. Setup SNAT so this container can serve as an internet gateway
ip netns exec LB iptables -t nat -A POSTROUTING -o external0 -j SNAT --to-source $ext_ip

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
wget -q -O /usr/local/bin/lbmanager https://github.com/liorokman/proxmox-cloud-provider/releases/download/v0.0.1/lbmanager
wget -q -O /usr/local/bin/lbctl https://github.com/liorokman/proxmox-cloud-provider/releases/download/v0.0.1/lbctl
chmod +x /usr/local/bin/lbmanager 
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
dbDir: /var/run/lbmanager
loadbalancer:
  namespace: LB
  externalInterface: external0
  internalInterface: eth1
  ipam:
    cidr: $ext_ip/$ext_mask
    dynamicRange:
      startAt: $ext_start
      endAt: $ext_end
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
	pct exec $VMID chmod +x /tmp/install.sh
}

case "$PHASE"  in
   pre-start)
   ;;
   post-start)
	   writeFile /tmp/install.sh
	   pct exec $VMID -- /tmp/install.sh
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


