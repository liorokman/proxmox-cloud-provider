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

package loadbalancer

import (
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/knadh/koanf/v2"
)

// vipFile hands the virtual IPs to keepalived instead of configuring them on
// the interface. Only the active container of an HA pair may answer ARP for a
// virtual IP, and keepalived moves the addresses when the roles change.
type vipFile struct {
	path     string // included from keepalived.conf (virtual_ipaddress block)
	pidFile  string // keepalived's main process; gets SIGHUP when the file changes
	iface    string // the interface the addresses are added to
	prefix   int
	reserved []net.IP // addresses that belong to the container itself

	addrs   map[string]bool
	written string
	reload  func() error
}

func newVIPFile(config *koanf.Koanf, iface string, prefix int) *vipFile {
	path := config.String("loadbalancer.keepalived.vipsFile")
	if path == "" {
		return nil
	}
	v := &vipFile{
		path:    path,
		pidFile: config.String("loadbalancer.keepalived.pidFile"),
		iface:   iface,
		prefix:  prefix,
		addrs:   map[string]bool{},
	}
	if v.pidFile == "" {
		v.pidFile = "/run/lb-keepalived.pid"
	}
	for _, s := range config.Strings("loadbalancer.keepalived.reserved") {
		if ip := net.ParseIP(s); ip != nil {
			v.reserved = append(v.reserved, ip)
		}
	}
	v.reload = v.signalKeepalived
	// Whatever is in the file now was written by an earlier run
	if data, err := os.ReadFile(path); err == nil {
		v.written = string(data)
	}
	return v
}

func (v *vipFile) isReserved(ip net.IP) bool {
	for _, r := range v.reserved {
		if r.Equal(ip) {
			return true
		}
	}
	return false
}

func (v *vipFile) render() string {
	list := make([]string, 0, len(v.addrs))
	for a := range v.addrs {
		list = append(list, a)
	}
	sort.Strings(list)
	var b strings.Builder
	b.WriteString("# Written by lbmanager. Do not edit.\n")
	for _, a := range list {
		fmt.Fprintf(&b, "%s/%d dev %s\n", a, v.prefix, v.iface)
	}
	return b.String()
}

// flush writes the file and tells keepalived, when the contents changed.
func (v *vipFile) flush() error {
	content := v.render()
	if content == v.written {
		return nil
	}
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, v.path); err != nil {
		return err
	}
	v.written = content
	return v.reload()
}

func (v *vipFile) signalKeepalived() error {
	data, err := os.ReadFile(v.pidFile)
	if err != nil {
		// keepalived is not running (yet): it reads the file when it starts
		log.Printf("keepalived is not running (%v); %s is applied when it starts", err, v.path)
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("bad pid in %s: %v", v.pidFile, err)
	}
	return syscall.Kill(pid, syscall.SIGHUP)
}
