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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// JoinBundle mints the credentials a second node agent needs: a node
// kubeconfig, a kubelet serving certificate and the CA. The cluster must
// already run on this Mac. The joining agent gets its own state dir and
// hull store; on the same host it shares this cluster's gateway.
func JoinBundle(name, nodeIP string, port int, serverIP, out string, w io.Writer) error {
	if net.ParseIP(nodeIP) == nil {
		return fmt.Errorf("--ip %q is not an IP address", nodeIP)
	}
	p, err := NewPaths()
	if err != nil {
		return err
	}
	if out == "" {
		out = filepath.Join(p.Root, "join", name)
	}
	if serverIP == "" {
		if ips := hostIPv4s(); len(ips) > 0 {
			serverIP = ips[0].String()
		} else {
			serverIP = "127.0.0.1"
		}
	}
	ca, err := ensureCA(p.PKI)
	if err != nil {
		return fmt.Errorf("cluster CA: %w (is the cluster up?)", err)
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	specs := []CertSpec{
		{Name: "node-" + name, CN: "system:node:" + name, Orgs: []string{"system:nodes"}, Client: true},
		{
			Name: "kubelet-serving-" + name, CN: "system:node:" + name, Orgs: []string{"system:nodes"}, Server: true,
			DNS: []string{name, name + ".local", "localhost"},
			IPs: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(nodeIP)},
		},
	}
	for _, s := range specs {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		der, err := ca.Sign(s, &key.PublicKey)
		if err != nil {
			return err
		}
		if err := writePair(filepath.Join(out, s.Name+".crt"), filepath.Join(out, s.Name+".key"), der, key); err != nil {
			return err
		}
	}
	caPEM, err := os.ReadFile(filepath.Join(p.PKI, "ca.crt"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "ca.crt"), caPEM, 0o644); err != nil {
		return err
	}
	server := "https://" + net.JoinHostPort(serverIP, strconv.Itoa(APIServerPort))
	kubeconfig := filepath.Join(out, "node.kubeconfig")
	if err := WriteKubeconfigTo(kubeconfig, out, "node-"+name, "node", server); err != nil {
		return err
	}
	fmt.Fprintf(w, `join bundle for node %s in %s

on the joining Mac, with this repo built and hull installed:

  export BRIGLET_STATE_DIR=~/.briglet-%s
  mkdir -p "$BRIGLET_STATE_DIR/guest-bin"
  cp <briglet-guest> "$BRIGLET_STATE_DIR/guest-bin/"
  bin/briglet node \
    --kubeconfig %s \
    --client-ca-file %s \
    --tls-cert-file %s \
    --tls-private-key-file %s \
    --name %s --node-ip %s --port %d

on the same host, add --gateway-sock %s so the node shares this
cluster's gateway and pod network. The controller manager allocates the
node's pod CIDR half; the agent adopts it on startup.
`, name, out, name, kubeconfig, filepath.Join(out, "ca.crt"),
		filepath.Join(out, "kubelet-serving-"+name+".crt"),
		filepath.Join(out, "kubelet-serving-"+name+".key"),
		name, nodeIP, port, p.GatewaySock)
	return nil
}
