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

package node

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func podWithPort(port int32, proto corev1.Protocol, hostIP string) *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Ports: []corev1.ContainerPort{{ContainerPort: 80, HostPort: port, Protocol: proto, HostIP: hostIP}},
	}}}}
}

func TestHostPortsConflict(t *testing.T) {
	tests := []struct {
		name string
		a, b *corev1.Pod
		want bool
	}{
		{"same port", podWithPort(21017, "", ""), podWithPort(21017, corev1.ProtocolTCP, ""), true},
		{"other port", podWithPort(21017, "", ""), podWithPort(21018, "", ""), false},
		{"other protocol", podWithPort(21017, corev1.ProtocolTCP, ""), podWithPort(21017, corev1.ProtocolUDP, ""), false},
		{"other host IPs", podWithPort(21017, "", "127.0.0.1"), podWithPort(21017, "", "127.0.0.2"), false},
		{"specific and wildcard", podWithPort(21017, "", "127.0.0.1"), podWithPort(21017, "", "0.0.0.0"), true},
		{"no host port", podWithPort(0, "", ""), podWithPort(0, "", ""), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostPortsConflict(tc.a, tc.b); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
