// Copyright 2026 Lior Okman
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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/liorokman/proxmox-cloud-provider/internal/nstest"
)

// testConfig returns an lbmanager configuration for a private namespace and a
// temporary database. extra lines (indented by two spaces already) are added
// to the loadbalancer section.
func testConfig(t *testing.T, extra ...string) *koanf.Koanf {
	t.Helper()
	ns := nstest.New(t)
	cfg := fmt.Sprintf(`
dbDir: %s
loadbalancer:
  namespace: %s
  externalInterface: dummy0
  internalInterface: eth1
  ipam:
    cidr: 192.168.87.0/24
    dynamicRange:
      startAt: 192.168.87.200
      endAt: 192.168.87.225
    staticRange:
      startAt: 192.168.87.230
      endAt: 192.168.87.240
%s`, t.TempDir(), ns, strings.Join(extra, "\n")+"\n")
	path := filepath.Join(t.TempDir(), "lbmanager.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		t.Fatal(err)
	}
	return k
}
