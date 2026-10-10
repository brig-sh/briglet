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
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/brig-sh/briglet/internal/hull"
)

// egressProxy serves the apiserver's cluster egress selector: an HTTP
// CONNECT proxy. A destination in the service or pod CIDR gets a
// temporary gateway forward on an ephemeral loopback port, so the dial
// enters the pod network, where the service table routes it, the way a
// NodePort forward does. A loopback destination, such as the node
// agent's kubelet server, is dialed directly.
type egressProxy struct {
	gw      *hull.Gateway
	svcCIDR netip.Prefix
	podCIDR netip.Prefix
	// nodeIP is this node's InternalIP, dialed directly like loopback.
	nodeIP netip.Addr
}

// serveEgressProxy answers CONNECT until ctx ends.
func serveEgressProxy(ctx context.Context, sockPath string, gw *hull.Gateway, svcCIDR, podCIDR netip.Prefix, nodeIP string) error {
	ip, _ := netip.ParseAddr(nodeIP)
	p := &egressProxy{gw: gw, svcCIDR: svcCIDR, podCIDR: podCIDR, nodeIP: ip}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		_ = ln.Close()
	}()
	log.Printf("egress proxy: listening on %s for the service CIDR %s and pod CIDR %s", sockPath, svcCIDR, podCIDR)
	return srv.Serve(ln)
}

func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	host, _, err := net.SplitHostPort(r.URL.Host)
	if err != nil {
		http.Error(w, "bad destination", http.StatusBadRequest)
		return
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		http.Error(w, "bad destination", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	var dst net.Conn
	var fwd hull.Forward
	switch {
	case ip.IsLoopback() || ip == p.nodeIP:
		// The apiserver's own dials, such as the kubelet server, pass
		// through the cluster selection in 1.37 and stay on the host.
		dst, err = net.DialTimeout("tcp", r.URL.Host, 5*time.Second)
	case p.svcCIDR.Contains(ip) || p.podCIDR.Contains(ip):
		dst, fwd, err = p.forward(r.URL.Host)
	default:
		// Anything else, such as another node's kubelet, is a host
		// address: dial it directly.
		dst, err = net.DialTimeout("tcp", r.URL.Host, 5*time.Second)
	}
	if err != nil || dst == nil {
		log.Printf("egress proxy: %s: %v", r.URL.Host, err)
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() {
		dst.Close()
		if fwd.Protocol != "" {
			_ = p.gw.RemoveForward(context.Background(), fwd.Protocol, fwd.Local)
		}
	}()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(dst, client); done <- struct{}{} }()
	go func() { io.Copy(client, dst); done <- struct{}{} }()
	<-done
}

// forward answers the destination through a temporary gateway forward
// on an ephemeral loopback port. The port is reserved with the kernel
// first, so two concurrent connections never pick the same one.
func (p *egressProxy) forward(remote string) (net.Conn, hull.Forward, error) {
	cx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for range 10 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			lastErr = err
			break
		}
		local := l.Addr().String()
		l.Close()
		fwd := hull.Forward{Protocol: "tcp", Local: local, Remote: remote}
		if err := p.gw.AddForward(cx, fwd); err != nil {
			lastErr = fmt.Errorf("add forward %s: %w", local, err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		dst, err := net.DialTimeout("tcp", local, 5*time.Second)
		if err != nil {
			lastErr = fmt.Errorf("dial %s: %w", local, err)
			_ = p.gw.RemoveForward(context.Background(), fwd.Protocol, fwd.Local)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return dst, fwd, nil
	}
	return nil, hull.Forward{}, fmt.Errorf("no forward to %s: %w", remote, lastErr)
}
