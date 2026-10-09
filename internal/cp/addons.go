// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cp

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// CoreDNS settings. The image is the one kubeadm 1.37 pins
// (constants.CoreDNSVersion).
const (
	ClusterDNSIP   = "10.96.0.10"
	ClusterDomain  = "cluster.local"
	CoreDNSImage   = "registry.k8s.io/coredns/coredns:v1.14.6"
	coreDNSMemory  = "170Mi"
	addonsManager  = "briglet"
	addonsDeadline = 60 * time.Second
)

//go:embed addons/*.yaml
var addonFS embed.FS

// addonFiles are applied in this order.
var addonFiles = []string{"addons/node-proxier.yaml", "addons/coredns.yaml"}

// resources maps the kinds the addons use to their API resources.
var resources = map[string]struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}{
	"ServiceAccount":     {schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, true},
	"ConfigMap":          {schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, true},
	"Service":            {schema.GroupVersionResource{Version: "v1", Resource: "services"}, true},
	"Deployment":         {schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true},
	"ClusterRole":        {schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, false},
	"ClusterRoleBinding": {schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, false},
}

// addonObjects renders the embedded manifests for the node.
func addonObjects(nodeName string) ([]*unstructured.Unstructured, error) {
	r := strings.NewReplacer(
		"{{NODE}}", nodeName,
		"{{GATEWAY_IP}}", GatewayIP,
		"{{CLUSTER_DOMAIN}}", ClusterDomain,
		"{{CLUSTER_DNS_IP}}", ClusterDNSIP,
		"{{COREDNS_IMAGE}}", CoreDNSImage,
		"{{COREDNS_MEMORY}}", coreDNSMemory,
	)
	var out []*unstructured.Unstructured
	for _, f := range addonFiles {
		b, err := addonFS.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, doc := range strings.Split(r.Replace(string(b)), "\n---\n") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			j, err := yaml.YAMLToJSON([]byte(doc))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if string(j) == "null" {
				continue
			}
			u := &unstructured.Unstructured{}
			if err := u.UnmarshalJSON(j); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if _, ok := resources[u.GetKind()]; !ok {
				return nil, fmt.Errorf("%s: kind %s has no resource mapping", f, u.GetKind())
			}
			out = append(out, u)
		}
	}
	return out, nil
}

// ApplyAddons server-side applies the node-proxier binding and CoreDNS.
// Applying the same manifests again changes nothing, so it runs on every
// `up`. It retries while the apiserver finishes its own bootstrap
// (kube-system, the bootstrap roles).
func (c *Cluster) ApplyAddons(ctx context.Context, out io.Writer) error {
	objs, err := addonObjects(c.NodeName)
	if err != nil {
		return err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", c.Paths.AdminKubeconfig)
	if err != nil {
		return err
	}
	cfg.Timeout = 10 * time.Second
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, addonsDeadline)
	defer cancel()
	for _, u := range objs {
		m := resources[u.GetKind()]
		var ri dynamic.ResourceInterface = dc.Resource(m.gvr)
		if m.namespaced {
			ri = dc.Resource(m.gvr).Namespace(u.GetNamespace())
		}
		body, err := json.Marshal(u.Object)
		if err != nil {
			return err
		}
		for {
			_, err = ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{
				FieldManager: addonsManager, Force: ptr.To(true),
			})
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("apply %s %s: %w", u.GetKind(), u.GetName(), err)
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	fmt.Fprintf(out, "%-24s applied %d objects (CoreDNS %s at %s)\n", "addons", len(objs), CoreDNSImage, ClusterDNSIP)
	return nil
}
