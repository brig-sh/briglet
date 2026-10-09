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

// Package services turns Services and EndpointSlices into the hull
// gateway's service table and NodePort forwards.
package services

import (
	"net"
	"net/netip"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/brig-sh/briglet/internal/hull"
)

// The apiserver's default --service-node-port-range.
const (
	NodePortMin = 30000
	NodePortMax = 32767
)

// Params are the network facts the table is built against.
type Params struct {
	// GatewayIP is the address the apiserver publishes itself at. An
	// endpoint there is the host, reached at 127.0.0.1.
	GatewayIP string
	// PodCIDR holds the guest endpoints. Other addresses are skipped: the
	// gateway refuses a table with a guest endpoint off its subnet.
	PodCIDR netip.Prefix
	// ServiceCIDR holds the cluster IPs.
	ServiceCIDR netip.Prefix
}

func protocolOf(p corev1.Protocol) string {
	switch p {
	case "", corev1.ProtocolTCP:
		return "tcp"
	case corev1.ProtocolUDP:
		return "udp"
	}
	return ""
}

// routed reports whether the Service gets table entries: it has an IPv4
// cluster IP. Headless and ExternalName Services are DNS only.
func routed(s *corev1.Service, p Params) (netip.Addr, bool) {
	if s.Spec.Type == corev1.ServiceTypeExternalName {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(s.Spec.ClusterIP)
	if err != nil || !ip.Is4() || !p.ServiceCIDR.Contains(ip) {
		return netip.Addr{}, false
	}
	return ip, true
}

// Build computes the full service table: one entry per cluster IP, port
// and protocol, with the ready endpoints of the EndpointSlices whose port
// matches by name and protocol. skipped lists endpoints left out because
// the gateway cannot reach them.
func Build(svcs []*corev1.Service, slices []*discoveryv1.EndpointSlice, p Params) (table []hull.Service, skipped []string) {
	bySvc := map[string][]*discoveryv1.EndpointSlice{}
	for _, sl := range slices {
		name := sl.Labels[discoveryv1.LabelServiceName]
		if name == "" || sl.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		bySvc[sl.Namespace+"/"+name] = append(bySvc[sl.Namespace+"/"+name], sl)
	}
	gwIP, _ := netip.ParseAddr(p.GatewayIP)
	table = []hull.Service{}
	for _, s := range svcs {
		vip, ok := routed(s, p)
		if !ok {
			continue
		}
		for _, sp := range s.Spec.Ports {
			proto := protocolOf(sp.Protocol)
			if proto == "" {
				continue // SCTP
			}
			seen := map[hull.ServiceEndpoint]bool{}
			eps := []hull.ServiceEndpoint{}
			for _, sl := range bySvc[s.Namespace+"/"+s.Name] {
				port, ok := slicePort(sl, sp)
				if !ok {
					continue
				}
				for _, ep := range sl.Endpoints {
					if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
						continue
					}
					for _, a := range ep.Addresses {
						ip, err := netip.ParseAddr(a)
						var e hull.ServiceEndpoint
						switch {
						case err != nil:
							continue
						case ip == gwIP:
							e = hull.ServiceEndpoint{IP: "127.0.0.1", Port: port, Host: true}
						case p.PodCIDR.Contains(ip):
							e = hull.ServiceEndpoint{IP: a, Port: port}
						default:
							skipped = append(skipped, s.Namespace+"/"+s.Name+" "+net.JoinHostPort(a, strconv.Itoa(int(port))))
							continue
						}
						if !seen[e] {
							seen[e] = true
							eps = append(eps, e)
						}
					}
				}
			}
			sort.Slice(eps, func(i, j int) bool {
				if eps[i].IP != eps[j].IP {
					return addrLess(eps[i].IP, eps[j].IP)
				}
				return eps[i].Port < eps[j].Port
			})
			table = append(table, hull.Service{VIP: vip.String(), Port: sp.Port, Protocol: proto, Endpoints: eps})
		}
	}
	sort.Slice(table, func(i, j int) bool {
		a, b := table[i], table[j]
		if a.VIP != b.VIP {
			return addrLess(a.VIP, b.VIP)
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Protocol < b.Protocol
	})
	return table, skipped
}

func addrLess(a, b string) bool {
	x, err1 := netip.ParseAddr(a)
	y, err2 := netip.ParseAddr(b)
	if err1 != nil || err2 != nil {
		return a < b
	}
	return x.Less(y)
}

// slicePort finds the EndpointSlice port that serves the Service port: the
// one with the same name and protocol.
func slicePort(sl *discoveryv1.EndpointSlice, sp corev1.ServicePort) (int32, bool) {
	for _, ep := range sl.Ports {
		name, proto := "", corev1.ProtocolTCP
		if ep.Name != nil {
			name = *ep.Name
		}
		if ep.Protocol != nil {
			proto = *ep.Protocol
		}
		spProto := sp.Protocol
		if spProto == "" {
			spProto = corev1.ProtocolTCP
		}
		if name == sp.Name && proto == spProto && ep.Port != nil {
			return *ep.Port, true
		}
	}
	return 0, false
}

// NodePort is a node port the gateway publishes on the host, carried to
// the Service's cluster IP and port.
type NodePort struct {
	Protocol string
	Port     int32
	Remote   string
}

// NodePorts lists the node ports of NodePort and LoadBalancer Services.
func NodePorts(svcs []*corev1.Service, p Params) []NodePort {
	var out []NodePort
	for _, s := range svcs {
		if s.Spec.Type != corev1.ServiceTypeNodePort && s.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		vip, ok := routed(s, p)
		if !ok {
			continue
		}
		for _, sp := range s.Spec.Ports {
			proto := protocolOf(sp.Protocol)
			if proto == "" || sp.NodePort == 0 {
				continue
			}
			out = append(out, NodePort{Protocol: proto, Port: sp.NodePort,
				Remote: net.JoinHostPort(vip.String(), strconv.Itoa(int(sp.Port)))})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out
}
