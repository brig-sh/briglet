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
	"fmt"
	"net"
	"os"
)

// LANAddr returns the address other nodes reach this Mac at. It is
// $BRIGLET_NODE_IP when set, then the address of the interface that holds
// the default route, then the first routable IPv4, and loopback on a Mac
// without a network.
func LANAddr() (string, error) {
	if v := os.Getenv("BRIGLET_NODE_IP"); v != "" {
		if ip := net.ParseIP(v); ip == nil || ip.To4() == nil {
			return "", fmt.Errorf("BRIGLET_NODE_IP %q is not an IPv4 address", v)
		}
		return v, nil
	}
	if ip := pickNodeIP(defaultRouteIPv4(), hostIPv4s()); ip != nil {
		return ip.String(), nil
	}
	return NodeIP, nil
}

// pickNodeIP prefers the default-route address when it is one of this
// Mac's routable addresses. A second interface, such as a direct cable to
// another Mac, can list before the LAN one and is not reachable from the
// rest of the network.
func pickNodeIP(def net.IP, candidates []net.IP) net.IP {
	for _, ip := range candidates {
		if def != nil && ip.Equal(def) {
			return ip
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return nil
}

// defaultRouteIPv4 returns the local address of the default route, or nil.
// Connecting a UDP socket sends nothing; the kernel only picks the route
// and the source address. 192.0.2.1 is a documentation address, so the
// default route is the one that matches it.
func defaultRouteIPv4() net.IP {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return nil
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.To4()
	}
	return nil
}
