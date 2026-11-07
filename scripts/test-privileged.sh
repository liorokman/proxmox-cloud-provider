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
# Run the unit tests, including the ones that need root (IPVS, network
# namespaces), and report the combined coverage.
#
#   scripts/test-privileged.sh              # unprivileged tests only, on this machine
#   scripts/test-privileged.sh devstack     # also run every package as root on that host (ssh)
#
# The test binaries are built here and copied over, so the host needs no Go
# toolchain, only the ip_vs kernel module (modprobe ip_vs).

set -euo pipefail
cd "$(dirname "$(readlink -f "$0")")/.."

HOST=${1:-}
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

PKGS=$(go list ./...)
mkdir -p "$OUT/local" "$OUT/remote"

i=0
for p in $PKGS; do
	i=$((i + 1))
	dir=${p#github.com/liorokman/proxmox-cloud-provider}
	dir=.${dir}
	go test -count=1 -covermode=atomic -coverprofile="$OUT/local/$i.out" "$p" 2>&1 | grep -v "no test files" || true
	if [ -n "$HOST" ]; then
		go test -c -covermode=atomic -o "$OUT/remote/$i.test" "$p" 2> /dev/null || continue
		[ -e "$OUT/remote/$i.test" ] || continue
		ssh "$HOST" "rm -rf /tmp/pt && mkdir -p /tmp/pt"
		scp -q "$OUT/remote/$i.test" "$HOST:/tmp/pt/t"
		# Run from a copy of the package directory, as `go test` does, so relative paths work
		tar -C "$dir" -c . | ssh "$HOST" "mkdir -p /tmp/pt/src && tar -x -C /tmp/pt/src"
		echo "--- $p (as root on $HOST)"
		ssh "$HOST" "cd /tmp/pt/src && /tmp/pt/t -test.count=1 -test.coverprofile=/tmp/pt/c.out -test.v=false 2>&1 | tail -15" || true
		scp -q "$HOST:/tmp/pt/c.out" "$OUT/remote/$i.out" 2> /dev/null || true
	fi
done

# Merge: the same block seen in several runs counts as covered if any run covered it
cat "$OUT"/local/*.out "$OUT"/remote/*.out 2> /dev/null | awk '
	/^mode:/ { mode = $0; next }
	{ key = $1 " " $2; if (!(key in n) || $3 > n[key]) n[key] = $3; if (!(key in c)) c[key] = 0; c[key] += $3 }
	END { print mode; for (k in c) print k, (c[k] > 0 ? 1 : 0) }' | sort -k1,1 > "$OUT/merged.raw"
# Generated protobuf code and the test helper packages are not worth measuring
{ echo "mode: atomic"; grep -v '^mode' "$OUT/merged.raw" | grep -v -E '\.pb\.go:|/certtest/|/nstest/'; } > "${COVER_PROFILE:-/tmp/cover.out}"
go tool cover -func="${COVER_PROFILE:-/tmp/cover.out}" | tail -1
echo "profile: ${COVER_PROFILE:-/tmp/cover.out}   (go tool cover -html=... to browse)"
