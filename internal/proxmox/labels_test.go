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

package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestTagLabels(t *testing.T) {
	got := tagLabels("gpu;k8s-red, fast_ssd bad+tag")
	want := map[string]string{
		TagLabelPrefix + "gpu":      "true",
		TagLabelPrefix + "k8s-red":  "true",
		TagLabelPrefix + "fast_ssd": "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if len(tagLabels("")) != 0 {
		t.Error("no tags must give no labels")
	}
}

func TestLabelPatch(t *testing.T) {
	wanted := map[string]string{"a": "1", TagLabelPrefix + "gpu": "true"}
	if p := labelPatch(map[string]string{"a": "1", TagLabelPrefix + "gpu": "true", "unrelated": "x"}, wanted); p != nil {
		t.Errorf("nothing differs, got %s", p)
	}
	p := labelPatch(map[string]string{"a": "0", TagLabelPrefix + "old": "true", "unrelated": "x"}, wanted)
	var got struct {
		Metadata struct{ Labels map[string]any }
	}
	if err := json.Unmarshal(p, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": "1", TagLabelPrefix + "gpu": "true", TagLabelPrefix + "old": nil}
	if !reflect.DeepEqual(got.Metadata.Labels, want) {
		t.Errorf("got %v, want %v", got.Metadata.Labels, want)
	}
}

func TestMatchResourceAmbiguousName(t *testing.T) {
	res := func(vmid uint64, name string) *proxmox.ClusterResource {
		return &proxmox.ClusterResource{Type: "qemu", VMID: vmid, Name: name}
	}
	both := []*proxmox.ClusterResource{res(1101, "cp1"), res(1201, "cp1")}
	if _, err := matchResource(both, nodeWithID("cp1", "")); err == nil || errors.Is(err, errVMNotFound) {
		t.Errorf("a name that two VMs have must be an error, got %v", err)
	}
	// The provider ID is unambiguous
	if r, err := matchResource(both, nodeWithID("cp1", "proxmox:///pve1/1201")); err != nil || r.VMID != 1201 {
		t.Errorf("got %v, %v", r, err)
	}
}

func TestLabelSyncer(t *testing.T) {
	i, pve := fakeCluster(t)
	// k8s-cp1 moved to pve2, was resized, and got a tag
	pve.set("/cluster/resources", []any{
		map[string]any{"id": "qemu/1101", "type": "qemu", "vmid": 1101, "node": "pve2", "name": "k8s-cp1", "status": "running",
			"template": 0, "maxcpu": 8, "maxmem": 16 << 30, "tags": "gpu;k8s"},
	})
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k8s-cp1", Labels: map[string]string{
		v1.LabelTopologyZone: "pve1", v1.LabelFailureDomainBetaZone: "pve1", TagLabelPrefix + "old": "true", "keep": "me",
	}}, Spec: v1.NodeSpec{ProviderID: "proxmox:///pve1/1101"}}
	other := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "not-ours"}}
	client := fake.NewSimpleClientset(node, other)

	s := &labelSyncer{instances: i, client: client}
	if err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := client.CoreV1().Nodes().Get(context.Background(), "k8s-cp1", metav1.GetOptions{})
	want := map[string]string{
		v1.LabelTopologyZone: "pve2", v1.LabelFailureDomainBetaZone: "pve2",
		v1.LabelTopologyRegion: "homelab", v1.LabelFailureDomainBetaRegion: "homelab",
		v1.LabelInstanceTypeStable: "8c-16g", v1.LabelInstanceType: "8c-16g",
		TagLabelPrefix + "gpu": "true", TagLabelPrefix + "k8s": "true", "keep": "me",
	}
	if !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("got %v\nwant %v", got.Labels, want)
	}
	if o, _ := client.CoreV1().Nodes().Get(context.Background(), "not-ours", metav1.GetOptions{}); len(o.Labels) != 0 {
		t.Errorf("a node without a Proxmox provider ID was changed: %v", o.Labels)
	}
}

func TestInstanceMetadataTags(t *testing.T) {
	i, pve := fakeCluster(t)
	pve.set("/nodes/pve1/qemu/1101/config", map[string]any{"name": "k8s-cp1", "tags": "gpu;k8s"})
	md, err := i.InstanceMetadata(context.Background(), nodeWithID("k8s-cp1", "proxmox:///pve1/1101"))
	if err != nil {
		t.Fatal(err)
	}
	if md.AdditionalLabels[TagLabelPrefix+"gpu"] != "true" || len(md.AdditionalLabels) != 2 {
		t.Errorf("got %v", md.AdditionalLabels)
	}
}

func TestLabelSyncInterval(t *testing.T) {
	for in, want := range map[int]time.Duration{0: time.Minute, 30: 30 * time.Second, -1: 0} {
		if got := labelSyncInterval(ClusterConfig{LabelSyncInterval: in}); got != want {
			t.Errorf("%d: got %v, want %v", in, got, want)
		}
	}
}
