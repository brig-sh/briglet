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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolvConf(t *testing.T) {
	d := DNS{ClusterDNS: "10.96.0.10", Domain: "cluster.local", Upstream: "10.87.0.1"}
	pod := func(policy corev1.DNSPolicy, cfg *corev1.PodDNSConfig) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team"},
			Spec: corev1.PodSpec{DNSPolicy: policy, DNSConfig: cfg}}
	}
	two := "2"
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"default policy", pod("", nil),
			"nameserver 10.96.0.10\nsearch team.svc.cluster.local svc.cluster.local cluster.local\noptions ndots:5\n"},
		{"ClusterFirst with dnsConfig", pod(corev1.DNSClusterFirst, &corev1.PodDNSConfig{
			Nameservers: []string{"1.1.1.1"}, Searches: []string{"corp.example"},
			Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &two}, {Name: "edns0"}}}),
			"nameserver 10.96.0.10\nnameserver 1.1.1.1\nsearch team.svc.cluster.local svc.cluster.local cluster.local corp.example\noptions ndots:2 edns0\n"},
		{"Default keeps the guest file", pod(corev1.DNSDefault, nil), ""},
		{"Default with dnsConfig", pod(corev1.DNSDefault, &corev1.PodDNSConfig{Searches: []string{"corp.example"}}),
			"nameserver 10.87.0.1\nsearch corp.example\n"},
		{"None", pod(corev1.DNSNone, &corev1.PodDNSConfig{Nameservers: []string{"9.9.9.9"}}),
			"nameserver 9.9.9.9\n"},
	} {
		if got := ResolvConf(tc.pod, d); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
