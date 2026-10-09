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

package services

import (
	"encoding/json"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/hull"
)

var params = Params{
	GatewayIP:   "10.87.0.1",
	PodCIDR:     netip.MustParsePrefix("10.87.0.0/24"),
	ServiceCIDR: netip.MustParsePrefix("10.96.0.0/12"),
}

func svc(ns, name, ip string, typ corev1.ServiceType, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: typ, ClusterIP: ip, Ports: ports},
	}
}

func slice(ns, svcName string, ports []discoveryv1.EndpointPort, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svcName + "-x",
			Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       ports,
		Endpoints:   eps,
	}
}

func ep(ready *bool, addrs ...string) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{Addresses: addrs, Conditions: discoveryv1.EndpointConditions{Ready: ready}}
}

func port(name string, proto corev1.Protocol, p int32) discoveryv1.EndpointPort {
	return discoveryv1.EndpointPort{Name: ptr.To(name), Protocol: ptr.To(proto), Port: ptr.To(p)}
}

func TestBuild(t *testing.T) {
	svcs := []*corev1.Service{
		svc("default", "kubernetes", "10.96.0.1", corev1.ServiceTypeClusterIP,
			corev1.ServicePort{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP}),
		svc("kube-system", "kube-dns", "10.96.0.10", corev1.ServiceTypeClusterIP,
			corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP},
			corev1.ServicePort{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP}),
		svc("default", "web", "10.96.5.5", corev1.ServiceTypeNodePort,
			corev1.ServicePort{Port: 80, NodePort: 30080}),
		svc("default", "headless", corev1.ClusterIPNone, corev1.ServiceTypeClusterIP,
			corev1.ServicePort{Port: 80}),
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ext"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"}},
	}
	slices := []*discoveryv1.EndpointSlice{
		slice("default", "kubernetes", []discoveryv1.EndpointPort{port("https", corev1.ProtocolTCP, 6443)},
			ep(nil, "10.87.0.1")),
		slice("kube-system", "kube-dns",
			[]discoveryv1.EndpointPort{port("dns", corev1.ProtocolUDP, 53), port("dns-tcp", corev1.ProtocolTCP, 53)},
			ep(ptr.To(true), "10.87.0.12")),
		slice("default", "web", []discoveryv1.EndpointPort{port("", corev1.ProtocolTCP, 8080)},
			ep(ptr.To(true), "10.87.0.14"), ep(ptr.To(false), "10.87.0.15"), ep(ptr.To(true), "10.87.0.11"),
			ep(ptr.To(true), "192.168.1.9")),
		slice("default", "headless", []discoveryv1.EndpointPort{port("", corev1.ProtocolTCP, 80)},
			ep(nil, "10.87.0.20")),
	}
	table, skipped := Build(svcs, slices, params)
	got, _ := json.Marshal(table)
	want := `[{"vip":"10.96.0.1","port":443,"protocol":"tcp","endpoints":[{"ip":"127.0.0.1","port":6443,"host":true}]},` +
		`{"vip":"10.96.0.10","port":53,"protocol":"tcp","endpoints":[{"ip":"10.87.0.12","port":53}]},` +
		`{"vip":"10.96.0.10","port":53,"protocol":"udp","endpoints":[{"ip":"10.87.0.12","port":53}]},` +
		`{"vip":"10.96.5.5","port":80,"protocol":"tcp","endpoints":[{"ip":"10.87.0.11","port":8080},{"ip":"10.87.0.14","port":8080}]}]`
	if string(got) != want {
		t.Fatalf("table\n got %s\nwant %s", got, want)
	}
	if len(skipped) != 1 || skipped[0] != "default/web 192.168.1.9:8080" {
		t.Fatalf("skipped = %q", skipped)
	}

	nps := NodePorts(svcs, params)
	if len(nps) != 1 || nps[0] != (NodePort{Protocol: "tcp", Port: 30080, Remote: "10.96.5.5:80"}) {
		t.Fatalf("node ports = %+v", nps)
	}
}

func TestBuildNoEndpoints(t *testing.T) {
	table, _ := Build([]*corev1.Service{svc("default", "web", "10.96.5.5", "", corev1.ServicePort{Port: 80})}, nil, params)
	got, _ := json.Marshal(table)
	if want := `[{"vip":"10.96.5.5","port":80,"protocol":"tcp","endpoints":[]}]`; string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
	table, _ = Build(nil, nil, params)
	if got, _ := json.Marshal(table); string(got) != "[]" {
		t.Fatalf("empty table = %s", got)
	}
}

func TestOwnedForward(t *testing.T) {
	s := &Syncer{p: params}
	for f, want := range map[[2]string]bool{
		{"0.0.0.0:30080", "10.96.5.5:80"}:    true,
		{"127.0.0.1:30080", "10.96.5.5:80"}:  true,
		{"127.0.0.1:52011", "10.87.0.12:80"}: false,
		{"127.0.0.1:30080", "10.87.0.12:80"}: false,
		{"0.0.0.0:8080", "10.96.5.5:80"}:     false,
	} {
		_, ok := s.ownedForward(hullForward(f[0], f[1]))
		if ok != want {
			t.Errorf("%v owned = %v, want %v", f, ok, want)
		}
	}
}

func hullForward(local, remote string) hull.Forward {
	return hull.Forward{Protocol: "tcp", Local: local, Remote: remote}
}
