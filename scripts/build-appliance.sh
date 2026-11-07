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
# Build the load balancer appliance: a Proxmox container template with
# lbmanager, lbctl and everything they need already installed, and the network
# setup as systemd units. Run as root on a Proxmox node. The result is
# <template storage>/vztmpl/lbmanager-<version>_amd64.tar.zst, which
# `pct create` can use like any other template.
#
#   scripts/build-appliance.sh                       # lbmanager from the release in LBMANAGER_VERSION
#   LBMANAGER_BIN_DIR=/root/build scripts/build-appliance.sh   # your own binaries
#
# Settings (environment): LBMANAGER_VERSION, LBMANAGER_BIN_DIR, STORAGE (disk of
# the temporary container), TEMPLATE_STORAGE, BASE_TEMPLATE, BUILD_BRIDGE (needs
# DHCP and internet access).

set -euo pipefail
SCRIPT_DIR=$(dirname "$(readlink -f "$0")")
APPLIANCE_DIR=$SCRIPT_DIR/appliance

: "${LBMANAGER_VERSION:=$(sed -n 's/^LBMANAGER_VERSION=\${LBMANAGER_VERSION:-\(.*\)}/\1/p' "$SCRIPT_DIR/installLB.sh")}"
: "${LBMANAGER_BIN_DIR:=}"
: "${STORAGE:=local-lvm}"
: "${TEMPLATE_STORAGE:=local}"
: "${BASE_TEMPLATE:=debian-13-standard_13.6-1_amd64.tar.zst}"
: "${BUILD_BRIDGE:=vmbr0}"
RELEASE_URL=https://github.com/liorokman/proxmox-cloud-provider/releases/download/$LBMANAGER_VERSION

die() { echo "ERROR: $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run as root on a Proxmox node"

if [ -n "$LBMANAGER_BIN_DIR" ]; then NAME=lbmanager-dev; else NAME=lbmanager-$LBMANAGER_VERSION; fi
OUT_DIR=$(pvesm path "$TEMPLATE_STORAGE:vztmpl/x.tar.zst" | xargs dirname)
OUT=$OUT_DIR/${NAME}_amd64.tar.zst

if [ -e "$OUT" ] && [ -z "${REBUILD:-}" ]; then echo "$OUT exists already (REBUILD=1 to replace it)"; exit 0; fi

if ! pveam list "$TEMPLATE_STORAGE" 2> /dev/null | grep -q "$BASE_TEMPLATE"; then
	pveam update > /dev/null
	pveam download "$TEMPLATE_STORAGE" "$BASE_TEMPLATE"
fi

ID=$(pvesh get /cluster/nextid)
cleanup() { pct unmount "$ID" > /dev/null 2>&1 || true; pct stop "$ID" > /dev/null 2>&1 || true; pct destroy "$ID" --purge 1 > /dev/null 2>&1 || true; }
trap cleanup EXIT

echo "==> Temporary container $ID"
pct create "$ID" "$TEMPLATE_STORAGE:vztmpl/$BASE_TEMPLATE" -unprivileged 0 -cores 2 -memory 1024 -swap 0 \
	-hostname lb-build -net0 "name=eth0,bridge=$BUILD_BRIDGE,ip=dhcp" -storage "$STORAGE" -ostype debian > /dev/null
pct start "$ID"
for i in $(seq 1 30); do pct exec "$ID" -- getent hosts deb.debian.org > /dev/null 2>&1 && break; sleep 2; done
pct exec "$ID" -- getent hosts deb.debian.org > /dev/null || die "the build container has no network (needs DHCP and internet on $BUILD_BRIDGE)"

echo "==> Packages"
pct exec "$ID" -- bash -c 'export DEBIAN_FRONTEND=noninteractive LC_ALL=C
	apt-get -y update > /dev/null
	apt-get -y install --no-install-recommends ipvsadm iptables jq golang-cfssl ca-certificates > /dev/null
	apt-get -y purge openssh-server > /dev/null 2>&1 || true
	apt-get -y autoremove --purge > /dev/null; apt-get clean; rm -rf /var/lib/apt/lists/*'

echo "==> lbmanager and lbctl"
WORK=$(mktemp -d)
if [ -n "$LBMANAGER_BIN_DIR" ]; then
	for b in lbmanager lbctl; do cp "$LBMANAGER_BIN_DIR/$b" "$WORK/$b"; done
else
	curl -fsSL -o "$WORK/SHA256SUMS" "$RELEASE_URL/SHA256SUMS"
	for b in lbmanager lbctl; do
		curl -fsSL -o "$WORK/$b" "$RELEASE_URL/$b"
		(cd "$WORK" && awk -v b="$b" '$2 == b {print $1 "  " b}' SHA256SUMS | sha256sum -c --quiet -) || die "checksum of $b does not match the release"
	done
fi
for b in lbmanager lbctl; do pct push "$ID" "$WORK/$b" "/usr/local/bin/$b" --perms 0755; done
rm -rf "$WORK"
for f in lb-network lb-prepare; do pct push "$ID" "$APPLIANCE_DIR/$f" "/usr/local/sbin/$f" --perms 0755; done
for f in lb-network.service lbmanager.service; do pct push "$ID" "$APPLIANCE_DIR/$f" "/etc/systemd/system/$f" --perms 0644; done
pct exec "$ID" -- bash -c 'mkdir -p /etc/lbmanager /var/lib/lbstate
	systemctl enable lb-network.service lbmanager.service > /dev/null 2>&1
	# a version marker, for `pct exec <id> cat /etc/lbmanager-version`
	echo "'"$NAME"'" > /etc/lbmanager-version
	: > /etc/machine-id; rm -f /etc/hostname.bak; rm -rf /tmp/* /var/log/*.log /var/log/apt/*'

echo "==> Packing"
pct stop "$ID"
mkdir -p "$OUT_DIR"
ROOT=$(pct mount "$ID" | sed -n "s/^mounted CT $ID in '\(.*\)'$/\1/p")
[ -n "$ROOT" ] || ROOT=/var/lib/lxc/$ID/rootfs
tar --numeric-owner --one-file-system -C "$ROOT" -I 'zstd -T0 -19' -cf "$OUT.tmp" .
mv "$OUT.tmp" "$OUT"
echo "Built $OUT ($(du -h "$OUT" | cut -f1))"
