// Copyright 2023-2026 Lior Okman
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package ipam

import "net"

// IPAM hands out the virtual IP addresses of the load balancers.
//
// Every address that is in use is reference counted, since several load
// balancers may deliberately share one address (on different ports).
type IPAM interface {
	// Allocate picks a free address from the dynamic range and reserves it.
	Allocate() (net.IP, error)
	// Reserve takes a reference on an explicitly requested address. The address
	// must be inside the dynamic or static range, and must not be in use by
	// something else.
	Reserve(ip net.IP) error
	// Adopt records an address that is already recorded in the database,
	// without checking it against the configured ranges.
	Adopt(ip net.IP)
	// Release drops one reference on the address, and returns how many remain.
	Release(ip net.IP) int
	// IsReserved reports whether any load balancer holds the address.
	IsReserved(ip net.IP) bool
}
