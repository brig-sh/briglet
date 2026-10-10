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
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Pod addresses come from the gateway subnet. The subnet's first address
// is the gateway and the next nine are kept free; the broadcast address
// is never handed out. The range is a /24, or a /25 half of one when two
// agents share a gateway.
const firstPodHost = 10

// IPAM hands out pod addresses and persists them in a JSON file, guarded by
// an flock on a sibling lock file so two agents never share an address.
type IPAM struct {
	path   string
	prefix netip.Prefix
}

// Allocation is one address and the pod holding it.
type Allocation struct {
	UID  string `json:"uid"`
	Pod  string `json:"pod"` // namespace/name, for people reading the file
	Addr string `json:"addr"`
}

// NewIPAM returns an allocator over cidr persisted at path. The cidr is a
// /24 or a /25 half of the gateway's /24.
func NewIPAM(path, cidr string) (*IPAM, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, err
	}
	if p.Bits() != 24 && p.Bits() != 25 || !p.Addr().Is4() {
		return nil, fmt.Errorf("pod CIDR %s: only an IPv4 /24 or /25 is supported", cidr)
	}
	return &IPAM{path: path, prefix: p.Masked()}, nil
}

// Bits is the interface mask for --gateway-cidr. The gateway's pod
// network is one L2 /24 when nodes share a gateway; a standalone
// gateway on a /25 half uses its own mask, so the guest's on-link route
// and derived gateway address match the netstack that actually serves it.
func (m *IPAM) Bits() int { return m.prefix.Bits() }

func (m *IPAM) locked(f func(map[string]Allocation) (bool, error)) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(m.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lf.Fd()), unix.LOCK_UN)
	byUID := map[string]Allocation{}
	b, err := os.ReadFile(m.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		var list []Allocation
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("%s: %w", m.path, err)
		}
		for _, a := range list {
			byUID[a.UID] = a
		}
	}
	changed, err := f(byUID)
	if err != nil || !changed {
		return err
	}
	list := make([]Allocation, 0, len(byUID))
	for _, a := range byUID {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool {
		return netip.MustParseAddr(list[i].Addr).Less(netip.MustParseAddr(list[j].Addr))
	})
	out, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// Allocate returns the pod's address. The first time, it assigns the next
// free address after the last one it handed out, wrapping at the end of the
// range, as the CNI host-local plugin does. The gateway keeps a released
// address's old MAC until its neighbor entry expires, and a booting guest
// sends no gratuitous ARP, so an address reused at once is unreachable for
// a while.
func (m *IPAM) Allocate(uid, pod string) (string, error) {
	var addr string
	err := m.locked(func(byUID map[string]Allocation) (bool, error) {
		if a, ok := byUID[uid]; ok {
			addr = a.Addr
			return false, nil
		}
		used := map[string]bool{}
		for _, a := range byUID {
			used[a.Addr] = true
		}
		base := m.prefix.Addr().As4()
		// The range spans from firstPodHost above the network address to
		// one below the broadcast, all inside the last octet.
		size := 1 << (32 - m.prefix.Bits())
		lo := int(base[3]) + firstPodHost
		n := size - firstPodHost - 1
		start := 0
		if b, err := os.ReadFile(m.lastPath()); err == nil {
			if prev, err := netip.ParseAddr(strings.TrimSpace(string(b))); err == nil && m.prefix.Contains(prev) {
				if h := int(prev.As4()[3]) - lo; h >= 0 && h < n {
					start = h + 1
				}
			}
		}
		for i := 0; i < n; i++ {
			base[3] = byte(lo + (start+i)%n)
			cand := netip.AddrFrom4(base).String()
			if used[cand] {
				continue
			}
			if err := os.WriteFile(m.lastPath(), []byte(cand+"\n"), 0o600); err != nil {
				return false, err
			}
			addr = cand
			byUID[uid] = Allocation{UID: uid, Pod: pod, Addr: cand}
			return true, nil
		}
		return false, fmt.Errorf("pod CIDR %s is exhausted", m.prefix)
	})
	return addr, err
}

// lastPath holds the last address Allocate handed out.
func (m *IPAM) lastPath() string { return m.path + ".last" }

// Lookup returns the pod's address if it has one.
func (m *IPAM) Lookup(uid string) (string, bool) {
	var addr string
	_ = m.locked(func(byUID map[string]Allocation) (bool, error) {
		addr = byUID[uid].Addr
		return false, nil
	})
	return addr, addr != ""
}

// Release frees the pod's address.
func (m *IPAM) Release(uid string) error {
	return m.locked(func(byUID map[string]Allocation) (bool, error) {
		if _, ok := byUID[uid]; !ok {
			return false, nil
		}
		delete(byUID, uid)
		return true, nil
	})
}

// Retain frees every address whose pod is not in keep and returns the
// released allocations.
func (m *IPAM) Retain(keep map[string]bool) ([]Allocation, error) {
	var gone []Allocation
	err := m.locked(func(byUID map[string]Allocation) (bool, error) {
		for uid, a := range byUID {
			if !keep[uid] {
				gone = append(gone, a)
				delete(byUID, uid)
			}
		}
		return len(gone) > 0, nil
	})
	return gone, err
}
