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
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ServiceEnv computes the Service link variables the kubelet injects: the
// `kubernetes` Service of the default namespace always, and with
// enableServiceLinks every Service of the pod's namespace that has a cluster
// IP. services is every Service the node knows of.
func ServiceEnv(pod *corev1.Pod, services []*corev1.Service) []EnvVar {
	links := pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks
	byName := map[string]*corev1.Service{}
	for _, s := range services {
		if s.Spec.ClusterIP == "" || s.Spec.ClusterIP == corev1.ClusterIPNone || len(s.Spec.Ports) == 0 {
			continue
		}
		switch {
		case s.Namespace == "default" && s.Name == "kubernetes":
			if _, ok := byName[s.Name]; !ok {
				byName[s.Name] = s
			}
		case s.Namespace == pod.Namespace && links:
			byName[s.Name] = s
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []EnvVar
	for _, n := range names {
		out = append(out, serviceVars(byName[n])...)
	}
	return out
}

func envName(s string) string { return strings.ToUpper(strings.ReplaceAll(s, "-", "_")) }

// serviceVars is FromServices and makeLinkVariables of the kubelet's
// envvars package for one Service.
func serviceVars(s *corev1.Service) []EnvVar {
	prefix := envName(s.Name)
	ip := s.Spec.ClusterIP
	out := []EnvVar{
		{Name: prefix + "_SERVICE_HOST", Value: ip},
		{Name: prefix + "_SERVICE_PORT", Value: strconv.Itoa(int(s.Spec.Ports[0].Port))},
	}
	for _, p := range s.Spec.Ports {
		if p.Name != "" {
			out = append(out, EnvVar{Name: prefix + "_SERVICE_PORT_" + envName(p.Name), Value: strconv.Itoa(int(p.Port))})
		}
	}
	for i, p := range s.Spec.Ports {
		proto := string(corev1.ProtocolTCP)
		if p.Protocol != "" {
			proto = string(p.Protocol)
		}
		port := strconv.Itoa(int(p.Port))
		url := strings.ToLower(proto) + "://" + net.JoinHostPort(ip, port)
		if i == 0 {
			out = append(out, EnvVar{Name: prefix + "_PORT", Value: url})
		}
		pp := fmt.Sprintf("%s_PORT_%d_%s", prefix, p.Port, strings.ToUpper(proto))
		out = append(out,
			EnvVar{Name: pp, Value: url},
			EnvVar{Name: pp + "_PROTO", Value: strings.ToLower(proto)},
			EnvVar{Name: pp + "_PORT", Value: port},
			EnvVar{Name: pp + "_ADDR", Value: ip},
		)
	}
	return out
}
