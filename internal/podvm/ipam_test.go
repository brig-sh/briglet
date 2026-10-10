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
	"net/netip"
	"testing"
)

func TestIPAMHalves(t *testing.T) {
	for _, tc := range []struct {
		cidr  string
		first string
	}{
		{"10.87.0.0/25", "10.87.0.10"},
		{"10.87.0.128/25", "10.87.0.138"},
		{"10.87.0.0/24", "10.87.0.10"},
	} {
		t.Run(tc.cidr, func(t *testing.T) {
			m, err := NewIPAM(t.TempDir()+"/ipam.json", tc.cidr)
			if err != nil {
				t.Fatal(err)
			}
			p := netip.MustParsePrefix(tc.cidr)
			if m.Bits() != p.Bits() {
				t.Errorf("Bits() = %d, want the pod CIDR's %d", m.Bits(), p.Bits())
			}
			seen := map[string]bool{}
			for i := 0; i < 30; i++ {
				uid := fmt.Sprintf("pod-%d", i)
				addr, err := m.Allocate(uid, uid)
				if err != nil {
					t.Fatal(err)
				}
				ip := netip.MustParseAddr(addr)
				if !p.Contains(ip) {
					t.Fatalf("allocated %s outside %s", addr, tc.cidr)
				}
				if seen[addr] {
					t.Fatalf("allocated %s twice", addr)
				}
				seen[addr] = true
				if i == 0 && addr != tc.first {
					t.Errorf("first allocation = %s, want %s", addr, tc.first)
				}
			}
		})
	}
}

func TestIPAMRejectsOtherPrefixes(t *testing.T) {
	for _, cidr := range []string{"10.87.0.0/23", "10.87.0.0/26", "fd00::/64"} {
		if _, err := NewIPAM(t.TempDir()+"/ipam.json", cidr); err == nil {
			t.Errorf("NewIPAM(%s) succeeded, want rejected", cidr)
		}
	}
}
