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
	"net"
	"testing"
)

func TestPickNodeIP(t *testing.T) {
	cable := net.ParseIP("10.10.0.1").To4()
	lan := net.ParseIP("192.168.1.20").To4()
	tailnet := net.ParseIP("100.64.0.7").To4()
	for _, tc := range []struct {
		name       string
		def        net.IP
		candidates []net.IP
		want       net.IP
	}{
		// A direct cable to another Mac lists before Wi-Fi.
		{"default route wins over interface order", lan, []net.IP{cable, lan, tailnet}, lan},
		{"no default route", nil, []net.IP{cable, lan}, cable},
		{"default route not a candidate", net.ParseIP("172.16.0.5").To4(), []net.IP{lan}, lan},
		{"no addresses", lan, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickNodeIP(tc.def, tc.candidates)
			if !got.Equal(tc.want) {
				t.Errorf("pickNodeIP = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLANAddrOverride(t *testing.T) {
	t.Setenv("BRIGLET_NODE_IP", "192.0.2.10")
	got, err := LANAddr()
	if err != nil || got != "192.0.2.10" {
		t.Fatalf("LANAddr = %q, %v; want 192.0.2.10", got, err)
	}
	for _, bad := range []string{"mac.local", "fe80::1", "192.0.2"} {
		t.Setenv("BRIGLET_NODE_IP", bad)
		if _, err := LANAddr(); err == nil {
			t.Errorf("BRIGLET_NODE_IP=%q: no error", bad)
		}
	}
}
