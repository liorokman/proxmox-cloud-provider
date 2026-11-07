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
	"log"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

// TagLabelPrefix is the prefix of the node labels that come from Proxmox VM tags:
// the tag "gpu" becomes the label "tag.proxmox.liorokman.github.io/gpu=true".
const TagLabelPrefix = "tag.proxmox.liorokman.github.io/"

// tagLabels converts the tags of a VM (separated by ';', ',' or spaces) to node
// labels. Tags that cannot be a label name are ignored.
func tagLabels(tags string) map[string]string {
	retval := map[string]string{}
	for _, tag := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if len(validation.IsQualifiedName(TagLabelPrefix+tag)) == 0 {
			retval[TagLabelPrefix+tag] = "true"
		} else {
			log.Printf("ignoring the tag %q: it is not a valid label name", tag)
		}
	}
	return retval
}

// placementLabels are the labels that describe where the VM runs and how big it is.
// The beta labels are the source of truth for the cloud node controller, so both
// are set.
func placementLabels(zone, region, instanceType string) map[string]string {
	return map[string]string{
		v1.LabelTopologyZone:            zone,
		v1.LabelFailureDomainBetaZone:   zone,
		v1.LabelTopologyRegion:          region,
		v1.LabelFailureDomainBetaRegion: region,
		v1.LabelInstanceTypeStable:      instanceType,
		v1.LabelInstanceType:            instanceType,
	}
}

// labelPatch returns the JSON merge patch that makes the labels of a node equal to
// the wanted ones, or nil when nothing differs. Labels with TagLabelPrefix that are
// no longer wanted are removed; all other labels are left alone.
func labelPatch(current, wanted map[string]string) []byte {
	changes := map[string]any{}
	for k, v := range wanted {
		if cur, found := current[k]; !found || cur != v {
			changes[k] = v
		}
	}
	for k := range current {
		if _, keep := wanted[k]; !keep && strings.HasPrefix(k, TagLabelPrefix) {
			changes[k] = nil
		}
	}
	if len(changes) == 0 {
		return nil
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": changes}})
	return patch
}

// labelSyncer keeps the labels of the Kubernetes nodes in step with their VMs. The
// cloud node controller only sets them when a node registers, but a VM moves to
// another Proxmox node (a migration, or the HA manager's load balancing), is
// resized, or gets new tags later.
type labelSyncer struct {
	instances *Instances
	client    kubernetes.Interface
	interval  time.Duration
}

func (s *labelSyncer) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		if err := s.syncOnce(ctx); err != nil {
			log.Printf("synchronizing node labels: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *labelSyncer) syncOnce(ctx context.Context) error {
	nodes, err := s.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	cluster, err := s.instances.client.Cluster(ctx)
	if err != nil {
		return err
	}
	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return err
	}
	region := cluster.Name
	if region == "" {
		region = defaultRegion
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		// Only nodes that registered through this provider have the labels
		if !strings.HasPrefix(node.Spec.ProviderID, "proxmox:") {
			continue
		}
		r, err := matchResource(resources, node)
		if err != nil {
			log.Printf("not synchronizing the labels of %s: %v", node.Name, err)
			continue
		}
		wanted := placementLabels(r.Node, region, instanceType(int(r.MaxCPU), r.MaxMem))
		for k, v := range tagLabels(r.Tags) {
			wanted[k] = v
		}
		patch := labelPatch(node.Labels, wanted)
		if patch == nil {
			continue
		}
		if _, err := s.client.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			log.Printf("updating the labels of %s: %v", node.Name, err)
			continue
		}
		log.Printf("updated the labels of %s: %s", node.Name, patch)
	}
	return nil
}
