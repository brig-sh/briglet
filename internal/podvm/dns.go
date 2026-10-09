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

package podvm

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// DNS is the cluster DNS setup a pod's resolv.conf is built from.
type DNS struct {
	// ClusterDNS is the kube-dns Service address.
	ClusterDNS string
	// Domain is the cluster domain, cluster.local.
	Domain string
	// Upstream is the resolver hull gives the guest: the gateway.
	Upstream string
}

// The kubelet's limits on resolv.conf (validation.MaxDNSNameservers and
// MaxDNSSearchPaths).
const (
	maxNameservers = 3
	maxSearches    = 32
)

// ResolvConf returns the guest's /etc/resolv.conf for the pod's dnsPolicy
// and dnsConfig, as the kubelet's dns.Configurer builds it. It returns ""
// when the guest keeps the file hull wrote (dnsPolicy Default without a
// dnsConfig).
//
//   - ClusterFirst (the default) and ClusterFirstWithHostNet: the cluster
//     DNS, the <ns>.svc, svc and cluster domain searches, ndots:5.
//   - Default: the gateway, which resolves through the Mac.
//   - None: only what dnsConfig gives.
//
// dnsConfig nameservers and searches are appended, and its options
// override options of the same name.
func ResolvConf(pod *corev1.Pod, d DNS) string {
	var servers, searches []string
	var opts []corev1.PodDNSConfigOption
	switch pod.Spec.DNSPolicy {
	case corev1.DNSDefault:
		if pod.Spec.DNSConfig == nil {
			return ""
		}
		servers = []string{d.Upstream}
	case corev1.DNSNone:
	default:
		servers = []string{d.ClusterDNS}
		searches = []string{pod.Namespace + ".svc." + d.Domain, "svc." + d.Domain, d.Domain}
		opts = []corev1.PodDNSConfigOption{{Name: "ndots", Value: ptrTo("5")}}
	}
	if c := pod.Spec.DNSConfig; c != nil {
		servers = appendUnique(servers, c.Nameservers...)
		searches = appendUnique(searches, c.Searches...)
		for _, o := range c.Options {
			replaced := false
			for i := range opts {
				if opts[i].Name == o.Name {
					opts[i], replaced = o, true
				}
			}
			if !replaced {
				opts = append(opts, o)
			}
		}
	}
	if len(servers) > maxNameservers {
		servers = servers[:maxNameservers]
	}
	if len(searches) > maxSearches {
		searches = searches[:maxSearches]
	}
	var b strings.Builder
	for _, s := range servers {
		b.WriteString("nameserver " + s + "\n")
	}
	if len(searches) > 0 {
		b.WriteString("search " + strings.Join(searches, " ") + "\n")
	}
	if len(opts) > 0 {
		parts := make([]string, 0, len(opts))
		for _, o := range opts {
			if o.Value != nil && *o.Value != "" {
				parts = append(parts, o.Name+":"+*o.Value)
			} else {
				parts = append(parts, o.Name)
			}
		}
		b.WriteString("options " + strings.Join(parts, " ") + "\n")
	}
	return b.String()
}

func ptrTo[T any](v T) *T { return &v }

func appendUnique(list []string, add ...string) []string {
	for _, a := range add {
		seen := false
		for _, l := range list {
			if l == a {
				seen = true
			}
		}
		if !seen && a != "" {
			list = append(list, a)
		}
	}
	return list
}
